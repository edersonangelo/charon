package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

// insideTenant opens a connection of its own, scoped to the tenant of ctx, for
// a test that has to hold something open while the store works around it.
func insideTenant(ctx context.Context, t *testing.T, dsn string, tenant uuid.UUID) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := conn.Exec(ctx, "select set_config('charon.tenant', $1, false)", tenant.String()); err != nil {
		t.Fatalf("scoping the connection: %v", err)
	}
	return conn
}

func inTenantWith(tenant uuid.UUID) context.Context {
	return authz.WithTenant(context.Background(), tenant)
}

func waitingOnLocks(ctx context.Context, t *testing.T, conn *pgx.Conn) int {
	t.Helper()

	var waiting int
	if err := conn.QueryRow(ctx,
		"select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'",
	).Scan(&waiting); err != nil {
		t.Fatalf("reading who waits: %v", err)
	}
	return waiting
}

// Holds planning in the middle of planning an event: with the row of one of its
// destinations locked, the planner's insert of a delivery to it waits, after it
// has read which destinations the event goes to and before it commits.
func planningHeldMidway(
	ctx context.Context, t *testing.T, store *postgres.Store, dsn string,
	tenant, destination uuid.UUID,
) (pgx.Tx, <-chan error) {
	t.Helper()

	holder := insideTenant(ctx, t, dsn, tenant)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning: %v", err)
	}
	if _, err := tx.Exec(ctx, "select 1 from destination where id = $1 for update", destination); err != nil {
		t.Fatalf("holding the destination: %v", err)
	}

	planned := make(chan error, 1)
	go func() {
		_, err := store.Plan(ctx, 10)
		planned <- err
	}()

	watcher := insideTenant(ctx, t, dsn, tenant)
	untilWaiting(ctx, t, watcher, 1, nil)
	return tx, planned
}

// Waits until want sessions wait on a lock, or until finished says the work
// that might have waited is already done.
func untilWaiting(ctx context.Context, t *testing.T, watcher *pgx.Conn, want int, finished <-chan struct{}) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-finished:
			return
		default:
		}
		if waitingOnLocks(ctx, t, watcher) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nobody came to wait on a lock")
}

func destinationOf(ctx context.Context, t *testing.T, store *postgres.Store, name string) console.RouteRow {
	t.Helper()

	routes, err := store.DetailedRoutes(ctx)
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	for _, route := range routes {
		if route.Destination == name {
			return route
		}
	}
	t.Fatalf("no route to %s", name)
	return console.RouteRow{}
}

func claimedURLs(ctx context.Context, t *testing.T, store *postgres.Store) map[string]bool {
	t.Helper()

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	urls := map[string]bool{}
	for _, delivery := range claimed {
		urls[delivery.URL] = true
	}
	return urls
}

func TestARouteAddedWhileAnEventIsBeingPlannedStillReachesIt(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	tenant, err := store.CreateTenant(context.Background(), "added-midway", "Midway")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := inTenantWith(tenant.ID)

	if err := store.AddRoute(ctx, "erp", "first", "https://first.example/h", "http",
		console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing to the first: %v", err)
	}
	recordFor(ctx, t, store, "erp")
	first := destinationOf(ctx, t, store, "first")

	held, planned := planningHeldMidway(ctx, t, store, dsn, tenant.ID, first.DestinationID)

	var addErr error
	added := make(chan struct{})
	go func() {
		defer close(added)
		addErr = store.AddRoute(ctx, "erp", "second", "https://second.example/h", "http",
			console.DeliverAlreadyRecorded)
	}()
	untilWaiting(ctx, t, insideTenant(ctx, t, dsn, tenant.ID), 2, added)

	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("letting planning go: %v", err)
	}
	if err := <-planned; err != nil {
		t.Fatalf("planning: %v", err)
	}
	select {
	case <-added:
		if addErr != nil {
			t.Fatalf("adding the second route: %v", addErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("adding the second route never finished")
	}

	if urls := claimedURLs(ctx, t, store); !urls["https://second.example/h"] {
		t.Errorf("claimed %v: the route added while the event was being planned never got it", urls)
	}
}

func TestADestinationSwitchedOnWhileAnEventIsBeingPlannedStillReachesIt(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	tenant, err := store.CreateTenant(context.Background(), "back-midway", "Midway")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := inTenantWith(tenant.ID)

	for _, name := range []string{"other", "sink"} {
		if err := store.AddRoute(ctx, "erp", name, "https://"+name+".example/h", "http",
			console.DeliverAlreadyRecorded); err != nil {
			t.Fatalf("routing to %s: %v", name, err)
		}
	}
	sink := destinationOf(ctx, t, store, "sink")
	if err := store.SaveRoute(ctx, sink.ID, sink.URL, false); err != nil {
		t.Fatalf("switching the sink off: %v", err)
	}
	recordFor(ctx, t, store, "erp")
	other := destinationOf(ctx, t, store, "other")

	held, planned := planningHeldMidway(ctx, t, store, dsn, tenant.ID, other.DestinationID)

	var saveErr error
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		saveErr = store.SaveRoute(ctx, sink.ID, sink.URL, true)
	}()
	untilWaiting(ctx, t, insideTenant(ctx, t, dsn, tenant.ID), 2, saved)

	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("letting planning go: %v", err)
	}
	if err := <-planned; err != nil {
		t.Fatalf("planning: %v", err)
	}
	select {
	case <-saved:
		if saveErr != nil {
			t.Fatalf("switching the sink on: %v", saveErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("switching the sink on never finished")
	}

	if urls := claimedURLs(ctx, t, store); !urls["https://sink.example/h"] {
		t.Errorf("claimed %v: the destination switched on while the event was being planned never got it", urls)
	}
}

// An identifier is handed out before its event commits, so without something
// keeping them in step an event could become visible after a later one of the
// same provider had already been seen, and be delivered behind it.
func TestEventsOfAProviderBecomeVisibleInTheOrderOfTheirIdentifiers(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	tenant, err := store.CreateTenant(context.Background(), "in-step", "Step")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := inTenantWith(tenant.ID)
	reader := insideTenant(ctx, t, dsn, tenant.ID)

	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			for range 40 {
				if _, err := store.Record(ctx, inbound.Request{
					Provider:   "erp",
					Path:       "/webhooks/erp",
					ReceivedAt: time.Now().UTC(),
					Headers:    map[string][]string{"Content-Type": {"application/json"}},
					Body:       []byte(`{}`),
				}); err != nil {
					t.Errorf("recording: %v", err)
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()

	seen := map[uuid.UUID]bool{}
	var highest uuid.UUID
	var overtaken string
	for finished := false; !finished && overtaken == ""; {
		select {
		case <-done:
			finished = true
		default:
		}
		rows, err := reader.Query(ctx, "select id from inbound_event where provider = 'erp' order by id")
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			if string(id[:]) < string(highest[:]) {
				overtaken = fmt.Sprintf("event %s became visible after %s had already been seen", id, highest)
			}
			seen[id] = true
		}
		if len(ids) > 0 {
			highest = ids[len(ids)-1]
		}
	}
	<-done
	if overtaken != "" {
		t.Error(overtaken)
	}
}
