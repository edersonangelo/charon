package delivery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
)

var body = []byte(`{"id":"evt_1","type":"charge.succeeded"}`)

type destination struct {
	mu     sync.Mutex
	status atomic.Int32
	delay  atomic.Int64
	// retryAfter, in seconds, sent back with the answer when set.
	retryAfter atomic.Int64
	calls      atomic.Int32
	gotBody    []byte
	gotEvent   string
	gotHeaders http.Header
}

func (d *destination) headers() http.Header {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gotHeaders
}

func (d *destination) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)

		if wait := d.retryAfter.Load(); wait > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(wait, 10))
		}

		if delay := time.Duration(d.delay.Load()); delay > 0 {
			time.Sleep(delay)
		}

		received := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(received)

		d.mu.Lock()
		d.gotBody = received
		d.gotEvent = r.Header.Get("X-Charon-Event-Id")
		d.gotHeaders = r.Header.Clone()
		d.mu.Unlock()

		w.WriteHeader(int(d.status.Load()))
	}
}

func (d *destination) received() ([]byte, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gotBody, d.gotEvent
}

func setup(t *testing.T, cfg delivery.Config) (*postgres.Store, *delivery.Dispatcher, *destination, string, uuid.UUID) {
	t.Helper()
	return setupOn(t, cfg, outbound.HTTP)
}

func setupOn(
	t *testing.T, cfg delivery.Config, transport string,
) (*postgres.Store, *delivery.Dispatcher, *destination, string, uuid.UUID) {
	t.Helper()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	kinds := delivery.DefaultTransports().Kinds()
	if cfg.Transports != nil {
		kinds = cfg.Transports.Kinds()
	}
	if err := store.Register(ctx, postgres.Kinds{Transports: append(kinds, transport)}); err != nil {
		t.Fatalf("registering the transports: %v", err)
	}

	target := &destination{}
	target.status.Store(http.StatusOK)
	server := httptest.NewServer(target.handler())
	t.Cleanup(server.Close)

	if err := store.AddRoute(ctx, "stripe", "test", server.URL, transport); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	eventID, err := store.Record(ctx, inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       body,
	})
	if err != nil {
		t.Fatalf("recording the event: %v", err)
	}

	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = time.Millisecond
	}
	if cfg.BackoffCap == 0 {
		cfg.BackoffCap = 2 * time.Millisecond
	}

	return store, delivery.New(store, cfg), target, dsn, eventID
}

func states(t *testing.T, store *postgres.Store) map[string]int64 {
	t.Helper()
	got, err := store.DeliveryStates(context.Background())
	if err != nil {
		t.Fatalf("reading delivery states: %v", err)
	}
	return got
}

func attempts(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting directly: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var n int
	if err := conn.QueryRow(ctx, `select attempts from delivery limit 1`).Scan(&n); err != nil {
		t.Fatalf("reading attempts: %v", err)
	}
	return n
}

func TestDeliversWhenTheDestinationAccepts(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, eventID := setup(t, delivery.Config{})

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := states(t, store); got["delivered"] != 1 || got["pending"] != 0 {
		t.Fatalf("states = %v, want one delivered and nothing pending", got)
	}

	gotBody, gotEvent := target.received()
	if string(gotBody) != string(body) {
		t.Errorf("destination received %q, want %q", gotBody, body)
	}
	if gotEvent != eventID.String() {
		t.Errorf("X-Charon-Event-Id = %q, want %q", gotEvent, eventID)
	}
}

func TestKeepsTheDeliveryPendingWhenTheDestinationRefuses(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, dsn, _ := setup(t, delivery.Config{MaxAttempts: 5})
	target.status.Store(http.StatusInternalServerError)

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := states(t, store); got["pending"] != 1 || got["delivered"] != 0 {
		t.Fatalf("states = %v, want it still pending and nothing delivered", got)
	}
	if got := attempts(t, dsn); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestDeadLettersAfterTheAttemptLimitAndNeverCountsAsDelivered(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, _ := setup(t, delivery.Config{MaxAttempts: 2})
	target.status.Store(http.StatusInternalServerError)

	for range 6 {
		if _, err := dispatcher.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := states(t, store)
	if got["dead"] != 1 {
		t.Fatalf("states = %v, want one dead", got)
	}
	if got["delivered"] != 0 {
		t.Errorf("states = %v, a dead lettered delivery must never count as delivered", got)
	}
	if got["pending"] != 0 {
		t.Errorf("states = %v, want nothing left pending", got)
	}
}

func TestNothingIsLostWhileTheDestinationIsDown(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, _ := setup(t, delivery.Config{MaxAttempts: 20})
	target.status.Store(http.StatusServiceUnavailable)

	for range 4 {
		if _, err := dispatcher.Tick(context.Background()); err != nil {
			t.Fatalf("tick while down: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := states(t, store); got["pending"] != 1 || got["delivered"] != 0 || got["dead"] != 0 {
		t.Fatalf("states = %v, want it held pending while the destination is down", got)
	}

	target.status.Store(http.StatusOK)
	tickUntilDelivered(t, dispatcher, store)

	if got := states(t, store); got["delivered"] != 1 || got["pending"] != 0 {
		t.Fatalf("states = %v, want it delivered once the destination came back", got)
	}
	if target.calls.Load() < 5 {
		t.Errorf("destination saw %d calls, want the four refusals plus the acceptance", target.calls.Load())
	}

	gotBody, _ := target.received()
	if string(gotBody) != string(body) {
		t.Errorf("destination received %q, want the original %q", gotBody, body)
	}
}

func TestATimeoutIsAFailureNotADelivery(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, _ := setup(t, delivery.Config{
		MaxAttempts:    5,
		RequestTimeout: 50 * time.Millisecond,
	})
	target.delay.Store(int64(300 * time.Millisecond))

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := states(t, store); got["delivered"] != 0 || got["pending"] != 1 {
		t.Fatalf("states = %v, want it pending after a timeout", got)
	}
}

func TestAnEventWaitsForItsRouteAndIsDeliveredWhenItArrives(t *testing.T) {
	t.Parallel()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "arrives-first",
		Path:       "/webhooks/arrives-first",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       body,
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}

	planned, err := store.Plan(ctx, 10)
	if err != nil {
		t.Fatalf("planning without a route: %v", err)
	}
	if planned != 0 {
		t.Fatalf("planned %d deliveries, want 0 with no route", planned)
	}
	if got := states(t, store); len(got) != 0 {
		t.Fatalf("states = %v, want none", got)
	}

	awaiting, err := store.EventsAwaitingRoute(ctx)
	if err != nil {
		t.Fatalf("counting events awaiting a route: %v", err)
	}
	if awaiting != 1 {
		t.Errorf("events awaiting a route = %d, want 1", awaiting)
	}

	target := &destination{}
	target.status.Store(http.StatusOK)
	server := httptest.NewServer(target.handler())
	t.Cleanup(server.Close)

	if err := store.AddRoute(ctx, "arrives-first", "late", server.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{})
	if _, err := dispatcher.Tick(ctx); err != nil {
		t.Fatalf("tick after the route arrived: %v", err)
	}
	if _, err := dispatcher.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	if got := states(t, store); got["delivered"] != 1 {
		t.Fatalf("states = %v, want the waiting event delivered once its route arrived", got)
	}

	awaiting, err = store.EventsAwaitingRoute(ctx)
	if err != nil {
		t.Fatalf("counting events awaiting a route: %v", err)
	}
	if awaiting != 0 {
		t.Errorf("events awaiting a route = %d, want 0", awaiting)
	}
}

func TestWakesOnAnInboundRecordInsteadOfWaitingOutTheInterval(t *testing.T) {
	t.Parallel()

	store, dispatcher, _, _, _ := setup(t, delivery.Config{
		SafetyInterval: time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = dispatcher.Run(ctx)
	}()

	waitForDelivered(t, store, 1, 5*time.Second)

	if _, err := store.Record(context.Background(), inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       body,
	}); err != nil {
		t.Fatalf("recording the second event: %v", err)
	}

	// Without the notification this would sit out the one minute interval.
	waitForDelivered(t, store, 2, 5*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Run() did not return after the context was cancelled")
	}
}

func waitForDelivered(t *testing.T, store *postgres.Store, want int64, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if states(t, store)["delivered"] >= want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("delivered = %d after %v, want %d", states(t, store)["delivered"], within, want)
}

func TestARedeliveryIsNotAnnouncedAsAFirstAttempt(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, _ := setup(t, delivery.Config{})
	ctx := context.Background()

	if _, err := dispatcher.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := target.headers(); got.Get("X-Charon-Attempt") != "1" || got.Get("X-Charon-Replay") != "0" {
		t.Fatalf("first delivery announced attempt %q replay %q, want 1 and 0",
			got.Get("X-Charon-Attempt"), got.Get("X-Charon-Replay"))
	}

	events, err := store.SearchEvents(ctx, console.Filter{})
	if err != nil || len(events.Events) == 0 {
		t.Fatalf("reading the event back: %v", err)
	}
	if _, err := store.ReplayEvent(ctx, events.Events[0].ID); err != nil {
		t.Fatalf("replaying: %v", err)
	}

	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick after the replay: %v", err)
		}
	}

	got := target.headers()
	if got.Get("X-Charon-Attempt") != "1" {
		t.Errorf("attempt = %q, want 1: a replay opens a new run", got.Get("X-Charon-Attempt"))
	}
	if got.Get("X-Charon-Replay") != "1" {
		t.Errorf("replay = %q, want 1 so the destination can tell this is a redelivery",
			got.Get("X-Charon-Replay"))
	}
}

// The next attempt is scheduled a moment ahead, so a single tick right after a
// destination recovers may find nothing due yet.
func tickUntilDelivered(t *testing.T, dispatcher *delivery.Dispatcher, store *postgres.Store) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := dispatcher.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if states(t, store)["delivered"] > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing was delivered within the deadline: %v", states(t, store))
}
