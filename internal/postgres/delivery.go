package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres/db"
	"github.com/edersonangelo/charon/internal/provider"
)

var (
	ErrNoDestination = errors.New("no destination with that name")
	ErrAlreadyRouted = errors.New("this provider already delivers to that url")
)

// Creates the delivery rows a recorded event is owed, one per enabled route
// for its provider. Runs after ingestion so that the inbound port never has to
// know about routing.
// turns is how many rounds of planning or claiming have happened, so each one
// starts at a different tenant.
//
// Without it the budget is spent in whatever order the tenants come back in,
// and a busy tenant early in that order takes the whole batch every round
// while the ones after it are never reached. Starting one further along each
// time means every tenant leads eventually, however busy its neighbours are.
func (s *Store) inTurn(tenants []console.Tenant) []console.Tenant {
	if len(tenants) < 2 {
		return tenants
	}
	at := int((s.turns.Add(1) - 1) % uint64(len(tenants))) //nolint:gosec // bounded by the length
	return append(append([]console.Tenant{}, tenants[at:]...), tenants[:at]...)
}

// Plan turns what arrived into deliveries, for every tenant.
//
// One tenant at a time, because the process that plans works for all of them
// while row level security answers one at a time: a single sweep that names no
// tenant is answered for one of them and finds nothing for the rest, quietly.
// The batch is a budget spread across tenants rather than granted to each, so
// the work one round does stays bounded however many there are.
func (s *Store) Plan(ctx context.Context, batch int) (int, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, err
	}

	planned, left := 0, batch
	for _, tenant := range s.inTurn(tenants) {
		if left <= 0 {
			break
		}
		count, taken, err := s.planFor(authz.WithTenant(ctx, tenant.ID), left)
		if err != nil {
			return planned, err
		}
		planned += count
		left -= taken
	}
	return planned, nil
}

// planFor plans one tenant's arrivals and says how many events it looked at,
// which is what the budget is spent on.
func (s *Store) planFor(ctx context.Context, batch int) (planned, taken int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("beginning the planning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	events, err := q.ClaimUnplannedEvents(ctx, int32(batch)) //nolint:gosec // batch is small
	if err != nil {
		return 0, 0, fmt.Errorf("claiming unplanned events: %w", err)
	}

	for _, event := range events {
		// A signature that was checked and failed is settled here: recorded,
		// visible, and never delivered anywhere.
		if !event.Overridden &&
			(event.Signature == string(provider.Invalid) || event.Signature == string(provider.Missing)) {
			if markErr := q.MarkEventPlanned(ctx, event.ID); markErr != nil {
				return 0, 0, fmt.Errorf("settling event %s: %w", event.ID, markErr)
			}
			continue
		}

		destinations, destErr := q.EnabledDestinationsForProvider(ctx,
			db.EnabledDestinationsForProviderParams{
				TenantID: event.TenantID, Provider: event.Provider,
			})
		if destErr != nil {
			return 0, 0, fmt.Errorf("reading destinations for %q: %w", event.Provider, destErr)
		}

		// An event whose provider has no enabled route is left unplanned, so
		// that adding the route later still reaches it. Marking it planned
		// would leave it recorded and never delivered, with no way back.
		if len(destinations) == 0 {
			continue
		}

		for _, destinationID := range destinations {
			if createErr := q.CreateDelivery(ctx, db.CreateDeliveryParams{
				TenantID:      event.TenantID,
				EventID:       event.ID,
				DestinationID: destinationID,
			}); createErr != nil {
				return 0, 0, fmt.Errorf("creating a delivery for event %s: %w", event.ID, createErr)
			}
			planned++
		}

		if markErr := q.MarkEventPlanned(ctx, event.ID); markErr != nil {
			return 0, 0, fmt.Errorf("marking event %s planned: %w", event.ID, markErr)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("committing the planned deliveries: %w", err)
	}
	return planned, len(events), nil
}

// Claim takes a batch of due deliveries under a lease, so a worker that dies
// leaves its rows to be picked up again once the lease expires.
//
// One tenant at a time, for the same reason planning is: this process works
// for all of them and the database answers for one. The batch is spread across
// them rather than granted to each.
func (s *Store) Claim(
	ctx context.Context, batch int, lease time.Duration,
) ([]outbound.Delivery, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return nil, err
	}

	all := make([]outbound.Delivery, 0, batch)
	for _, tenant := range s.inTurn(tenants) {
		left := batch - len(all)
		if left <= 0 {
			break
		}
		taken, err := s.claimFor(authz.WithTenant(ctx, tenant.ID), tenant.ID, left, lease)
		if err != nil {
			return all, err
		}
		all = append(all, taken...)
	}
	return all, nil
}

func (s *Store) claimFor(
	ctx context.Context, tenant uuid.UUID, batch int, lease time.Duration,
) ([]outbound.Delivery, error) {
	claimed, err := s.q.ClaimDeliveries(ctx, db.ClaimDeliveriesParams{
		LeaseSeconds: lease.Seconds(),
		BatchSize:    int32(batch), //nolint:gosec // batch is small
	})
	if err != nil {
		return nil, fmt.Errorf("claiming deliveries: %w", err)
	}

	deliveries := make([]outbound.Delivery, 0, len(claimed))
	for _, row := range claimed {
		target, targetErr := s.q.DeliveryTarget(ctx, row.ID)
		if targetErr != nil {
			return nil, fmt.Errorf("reading the target of delivery %s: %w", row.ID, targetErr)
		}

		var headers map[string][]string
		if err := json.Unmarshal(target.Headers, &headers); err != nil {
			return nil, fmt.Errorf("decoding the headers of delivery %s: %w", row.ID, err)
		}

		signing, signErr := s.q.SigningSecretsFor(ctx, row.DestinationID)
		if signErr != nil {
			return nil, fmt.Errorf("reading how delivery %s is signed: %w", row.ID, signErr)
		}

		deliveries = append(deliveries, outbound.Delivery{
			ID:        row.ID,
			Tenant:    tenant,
			EventID:   row.EventID,
			Attempts:  row.Attempts,
			Replays:   row.ReplayCount,
			Transport: target.Transport,
			URL:       target.Url,
			Provider:  target.Provider,
			Headers:   headers,
			Body:      target.Body,
			Signing:   signing,
		})
	}
	return deliveries, nil
}

func (s *Store) MarkDelivered(ctx context.Context, tenant, id uuid.UUID, status int) error {
	ctx = authz.WithTenant(ctx, tenant)
	if err := s.q.MarkDelivered(ctx, db.MarkDeliveredParams{
		ID:         id,
		LastStatus: pgtype.Int4{Int32: int32(status), Valid: true}, //nolint:gosec // http status
	}); err != nil {
		return fmt.Errorf("marking delivery %s delivered: %w", id, err)
	}
	s.announceToPanel(ctx)
	return nil
}

// The panel is told about every change so an open page does not have to ask.
// A failure here is not worth surfacing: the worst case is a page that updates
// on its next interaction.
func (s *Store) announceToPanel(ctx context.Context) {
	_ = s.q.NotifyPanel(ctx)
}

// Wake-ups for the panel: a recorded event, a delivery that changed state, a
// replay. Closed when ctx is done.
func (s *Store) PanelChanges(ctx context.Context) <-chan struct{} {
	return s.listenOn(ctx, "charon_panel")
}

func (s *Store) MarkFailed(
	ctx context.Context,
	tenant, id uuid.UUID,
	nextAttempt time.Time,
	status int,
	reason string,
	maxAttempts int,
) error {
	ctx = authz.WithTenant(ctx, tenant)
	lastStatus := pgtype.Int4{}
	if status > 0 {
		lastStatus = pgtype.Int4{Int32: int32(status), Valid: true} //nolint:gosec // http status
	}

	if err := s.q.MarkFailed(ctx, db.MarkFailedParams{
		ID:            id,
		NextAttemptAt: nextAttempt,
		LastStatus:    lastStatus,
		LastError:     pgtype.Text{String: reason, Valid: reason != ""},
		MaxAttempts:   int32(maxAttempts), //nolint:gosec // small
	}); err != nil {
		return fmt.Errorf("marking delivery %s failed: %w", id, err)
	}
	s.announceToPanel(ctx)
	return nil
}

// ErrBadDestination is an address nothing could ever be delivered to.
var ErrBadDestination = errors.New(
	"a destination needs an http or https address with a host")

// DeliverableURL is what an address has to be before anything is routed to it.
// It lives here rather than in a form, because the panel and the command line
// are two ways to the same decision and only one of them was checking.
//
// It says nothing about the path. A trailing slash is a real endpoint, and
// what belongs after the host is the receiver's business.
func DeliverableURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)

	parsed, err := neturl.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadDestination, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: no host in %q", ErrBadDestination, trimmed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: %q is not http or https", ErrBadDestination, trimmed)
	}
	return trimmed, nil
}

func (s *Store) AddRoute(
	ctx context.Context, provider, destination, url, transport string,
) error {
	if transport == "" {
		transport = outbound.HTTP
	}

	// Only an address something is delivered over has to look like one. A
	// transport that is not http addresses its destination its own way.
	if transport == outbound.HTTP {
		checked, err := DeliverableURL(url)
		if err != nil {
			return err
		}
		url = checked
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the route transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	existing, err := q.DestinationByName(ctx, db.DestinationByNameParams{
		TenantID: s.tenantOf(ctx), Name: destination,
	})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		existing, err = q.CreateDestination(ctx, db.CreateDestinationParams{
			TenantID: s.tenantOf(ctx), Name: destination, Url: url, Transport: transport,
		})
		if err != nil {
			return fmt.Errorf("creating destination %q: %w", destination, err)
		}
	default:
		return fmt.Errorf("reading destination %q: %w", destination, err)
	}

	taken, err := q.ProviderAlreadyRoutedTo(ctx, db.ProviderAlreadyRoutedToParams{
		TenantID: s.tenantOf(ctx), Provider: provider, Url: url,
	})
	if err != nil {
		return fmt.Errorf("checking existing routes for %q: %w", provider, err)
	}
	if taken {
		return ErrAlreadyRouted
	}

	if err := q.CreateRoute(ctx, db.CreateRouteParams{
		TenantID: s.tenantOf(ctx), Provider: provider, DestinationID: existing.ID,
	}); err != nil {
		return fmt.Errorf("creating the route: %w", err)
	}

	// Routes are evaluated once per event. Without unplanning, a route added
	// after an event was planned would never reach it, and there would be no
	// delivery row to resend either.
	if _, err := q.UnplanProvider(ctx, db.UnplanProviderParams{Provider: provider, TenantID: s.tenantOf(ctx)}); err != nil {
		return fmt.Errorf("reopening planning for %q: %w", provider, err)
	}

	// A new route can make events that were waiting for one deliverable, and
	// nothing else would announce that.
	if err := q.NotifyWork(ctx); err != nil {
		return fmt.Errorf("announcing the route: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return fmt.Errorf("announcing the route to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the route: %w", err)
	}
	return nil
}

func (s *Store) Routes(ctx context.Context) ([]outbound.Route, error) {
	rows, err := s.q.ListRoutes(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}
	routes := make([]outbound.Route, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, outbound.Route{
			Provider:    row.Provider,
			Destination: row.Name,
			Transport:   row.Transport,
			URL:         row.Url,
			Enabled:     row.Enabled,
			Signed:      int(row.Signed),
		})
	}
	return routes, nil
}

func (s *Store) DeliveryStates(ctx context.Context) (map[string]int64, error) {
	rows, err := s.q.DeliveryStateTotals(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("counting deliveries: %w", err)
	}
	states := make(map[string]int64, len(rows))
	for _, row := range rows {
		states[row.State] = row.Total
	}
	return states, nil
}

func (s *Store) OldestPendingSeconds(ctx context.Context) (float64, error) {
	seconds, err := s.q.OldestPendingAge(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the oldest pending age: %w", err)
	}
	return seconds, nil
}

// The earliest moment there is anything to do: an event still to plan, or a
// delivery whose retry window has come. Reports false when the queue is idle.
// NextWorkAt is the earliest moment any tenant has something due, so the loop
// sleeps until then instead of polling. Asked one tenant at a time, because a
// question that names none is answered for one of them, and sleeping on that
// answer leaves every other tenant waiting out the safety interval.
func (s *Store) NextWorkAt(ctx context.Context) (time.Time, bool, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return time.Time{}, false, err
	}

	var earliest time.Time
	found := false
	for _, tenant := range tenants {
		row, err := s.q.NextWorkAt(authz.WithTenant(ctx, tenant.ID))
		if err != nil {
			return time.Time{}, false, fmt.Errorf("reading the next work time: %w", err)
		}
		if !row.Found {
			continue
		}
		if !found || row.At.Before(earliest) {
			earliest, found = row.At, true
		}
	}
	return earliest, found, nil
}

// Wake-ups announced by Record when it commits. The channel is closed when ctx
// is done. A lost notification is not an error here: the dispatcher's safety
// interval catches whatever a notification failed to announce.
func (s *Store) Notifications(ctx context.Context) <-chan struct{} {
	return s.listenOn(ctx, "charon_work")
}

func (s *Store) listenOn(ctx context.Context, channel string) <-chan struct{} {
	woken := make(chan struct{}, 1)

	go func() {
		defer close(woken)

		for ctx.Err() == nil {
			if err := s.listen(ctx, channel, woken); err != nil && ctx.Err() == nil {
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
	}()

	return woken
}

func (s *Store) listen(ctx context.Context, channel string, woken chan<- struct{}) error {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return fmt.Errorf("connecting to listen: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, "listen "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listening on %s: %w", channel, err)
	}

	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("waiting for a notification: %w", err)
		}
		select {
		case woken <- struct{}{}:
		default:
		}
	}
}

// Events recorded for a provider that has no enabled route. They stay
// unplanned on purpose and are delivered as soon as a route appears.
func (s *Store) EventsAwaitingRoute(ctx context.Context) (int64, error) {
	n, err := s.q.CountEventsAwaitingRoute(ctx, s.tenantOf(ctx))
	if err != nil {
		return 0, fmt.Errorf("counting events awaiting a route: %w", err)
	}
	return n, nil
}
