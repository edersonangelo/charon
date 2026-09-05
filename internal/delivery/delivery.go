package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/edersonangelo/charon/internal/outbound"
)

type Queue interface {
	Plan(ctx context.Context, batch int) (int, error)
	Claim(ctx context.Context, batch int, lease time.Duration) ([]outbound.Delivery, error)
	MarkDelivered(ctx context.Context, id uuid.UUID, status int) error
	MarkFailed(ctx context.Context, id uuid.UUID, nextAttempt time.Time,
		status int, reason string, maxAttempts int) error
	RecordAttempt(ctx context.Context, deliveryID uuid.UUID, attempt, status int,
		reason string, took time.Duration) error
	NextWorkAt(ctx context.Context) (time.Time, bool, error)
	Notifications(ctx context.Context) <-chan struct{}
}

type Config struct {
	Transports     *Transports
	Workers        int
	BatchSize      int
	SafetyInterval time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	BackoffBase    time.Duration
	BackoffCap     time.Duration
	Logger         *slog.Logger
}

type Dispatcher struct {
	queue  Queue
	cfg    Config
	logger *slog.Logger

	mu     sync.RWMutex
	byKind map[string]outbound.Transport
}

func New(queue Queue, cfg Config) *Dispatcher {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.SafetyInterval <= 0 {
		cfg.SafetyInterval = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 12
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 5 * time.Second
	}
	if cfg.BackoffCap <= 0 {
		cfg.BackoffCap = time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	if cfg.Transports == nil {
		cfg.Transports = DefaultTransports()
	}

	return &Dispatcher{
		queue:  queue,
		cfg:    cfg,
		logger: cfg.Logger,
		byKind: map[string]outbound.Transport{},
	}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	woken := d.queue.Notifications(ctx)

	d.logger.InfoContext(ctx, "dispatching",
		"workers", d.cfg.Workers, "batch", d.cfg.BatchSize,
		"max_attempts", d.cfg.MaxAttempts, "safety_interval", d.cfg.SafetyInterval)

	for ctx.Err() == nil {
		if d.round(ctx) > 0 {
			continue
		}

		timer := time.NewTimer(d.wait(ctx))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-woken:
			timer.Stop()
		case <-timer.C:
		}
	}

	return nil
}

// How long a claim is held. Derived rather than configured: it only has to
// outlast an attempt, so that a worker that dies leaves its rows to be picked
// up again instead of stuck.
func (d *Dispatcher) lease() time.Duration { return d.cfg.RequestTimeout * 4 }

// A failed round is logged and picked up again on the next wake-up: a
// transient database error must not take the dispatcher down.
func (d *Dispatcher) round(ctx context.Context) int {
	worked, err := d.Tick(ctx)
	if err != nil && ctx.Err() == nil {
		d.logger.ErrorContext(ctx, "dispatch round failed", "error", err)
	}
	return worked
}

// Sleeps until the next delivery is actually due rather than asking on a fixed
// interval, waking early when an inbound record announces itself. The safety
// interval bounds the sleep, because a row can become due by a route that
// announced nothing: a replay, a manual change, or an expired lease.
func (d *Dispatcher) wait(ctx context.Context) time.Duration {
	wait := d.cfg.SafetyInterval

	at, found, err := d.queue.NextWorkAt(ctx)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			d.logger.WarnContext(ctx, "could not read the next work time", "error", err)
		}
	case found:
		if until := time.Until(at); until < wait {
			wait = until
		}
	}

	if wait < minimumWait {
		wait = minimumWait
	}
	return wait
}

func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	planned, err := d.queue.Plan(ctx, d.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("planning deliveries: %w", err)
	}

	deliveries, err := d.queue.Claim(ctx, d.cfg.BatchSize, d.lease())
	if err != nil {
		return planned, fmt.Errorf("claiming deliveries: %w", err)
	}
	if len(deliveries) == 0 {
		return planned, nil
	}

	var group errgroup.Group
	group.SetLimit(d.cfg.Workers)

	for _, item := range deliveries {
		group.Go(func() error {
			d.attempt(ctx, item)
			return nil
		})
	}
	_ = group.Wait()

	return planned + len(deliveries), nil
}

// A delivery is closed as delivered only when the transport reports that the
// destination accepted it.
func (d *Dispatcher) attempt(ctx context.Context, item outbound.Delivery) {
	attempts := int(item.Attempts) + 1

	started := time.Now()
	result := d.transport(item).Send(ctx, item)
	took := time.Since(started)

	detail := ""
	if !result.Accepted {
		detail = result.Detail
	}

	if recErr := d.queue.RecordAttempt(ctx, item.ID, attempts, result.Status, detail, took); recErr != nil {
		d.logger.ErrorContext(ctx, "could not record a delivery attempt",
			"delivery_id", item.ID, "error", recErr)
	}

	if result.Accepted {
		if markErr := d.queue.MarkDelivered(ctx, item.ID, result.Status); markErr != nil {
			d.logger.ErrorContext(ctx, "could not record a delivery",
				"delivery_id", item.ID, "error", markErr)
		}
		d.logger.DebugContext(ctx, "delivered",
			"delivery_id", item.ID, "event_id", item.EventID, "transport", item.Transport,
			"status", result.Status, "took_ms", took.Milliseconds())
		return
	}

	// A transport that says another attempt cannot help is taken at its word:
	// retrying a rejection that will never change only delays the dead letter.
	limit := d.cfg.MaxAttempts
	if !result.Retryable {
		limit = attempts
	}

	next := time.Now().UTC().Add(backoff(attempts, d.cfg.BackoffBase, d.cfg.BackoffCap))

	if markErr := d.queue.MarkFailed(ctx, item.ID, next, result.Status, detail, limit); markErr != nil {
		d.logger.ErrorContext(ctx, "could not record a failed delivery",
			"delivery_id", item.ID, "error", markErr)
		return
	}

	level := slog.LevelWarn
	if attempts >= limit {
		level = slog.LevelError
	}
	d.logger.Log(ctx, level, "delivery attempt failed",
		"delivery_id", item.ID, "event_id", item.EventID, "provider", item.Provider,
		"transport", item.Transport, "attempt", attempts, "of", limit,
		"status", result.Status, "reason", detail)
}

// The transport for one delivery, built once per kind and kept.
func (d *Dispatcher) transport(item outbound.Delivery) outbound.Transport {
	kind := item.Transport
	if kind == "" {
		kind = HTTP
	}

	d.mu.RLock()
	built, ready := d.byKind[kind]
	d.mu.RUnlock()
	if ready {
		return built
	}

	built = d.cfg.Transports.Build(kind, Options{RequestTimeout: d.cfg.RequestTimeout})

	d.mu.Lock()
	d.byKind[kind] = built
	d.mu.Unlock()

	return built
}

// Exponential with half jitter, so a destination coming back up is not hit by
// every pending delivery at the same instant.
func backoff(attempt int, base, limit time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	window := base
	for i := 1; i < attempt && window < limit; i++ {
		window *= 2
	}
	if window > limit || window <= 0 {
		window = limit
	}

	half := window / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not a secret
}

// Floor on the sleep between rounds, so a delivery due in a microsecond does
// not spin the loop.
const minimumWait = 50 * time.Millisecond
