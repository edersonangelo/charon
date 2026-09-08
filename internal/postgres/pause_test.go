package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func arrival(ctx context.Context, t *testing.T, store *postgres.Store, body string) {
	t.Helper()

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(body),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
}

func theRoute(ctx context.Context, t *testing.T, store *postgres.Store) console.RouteRow {
	t.Helper()

	routes, err := store.DetailedRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("reading the route: %v (%d)", err, len(routes))
	}
	return routes[0]
}

// Switching a destination off is how an operator pauses it, so what is already
// waiting has to wait too. Attempting it anyway spends the attempts against
// somewhere deliberately taken out of service, and a delivery can reach the
// limit and be dead before anybody switches it back on.
func TestSwitchingADestinationOffHoldsWhatIsWaiting(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "paused-here", "Paused")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	arrival(ctx, t, store, `{"n":1}`)
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	route := theRoute(ctx, t, store)
	if err := store.SaveRoute(ctx, route.ID, route.URL, false); err != nil {
		t.Fatalf("switching it off: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d deliveries for a destination that is switched off", len(claimed))
	}

	// And the loop is not told there is work it cannot take, which would have
	// it spinning instead of sleeping.
	if _, found, err := store.NextWorkAt(context.Background()); err != nil || found {
		t.Errorf("something is said to be due while the only destination is off (%v)", err)
	}

	if err := store.SaveRoute(ctx, route.ID, route.URL, true); err != nil {
		t.Fatalf("switching it back on: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning again: %v", err)
	}

	claimed, err = store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after it came back: %v", err)
	}
	if len(claimed) != 1 {
		t.Errorf("claimed %d after it came back, want the one that waited", len(claimed))
	}
	if len(claimed) == 1 && claimed[0].Attempts != 0 {
		t.Errorf("it comes back having spent %d attempts", claimed[0].Attempts)
	}
}

// What arrives while it is off is delivered when it comes back, which already
// worked and must keep working.
func TestWhatArrivesWhileItIsOffGoesWhenItComesBack(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "paused-meanwhile", "Meanwhile")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	route := theRoute(ctx, t, store)
	if err := store.SaveRoute(ctx, route.ID, route.URL, false); err != nil {
		t.Fatalf("switching it off: %v", err)
	}

	arrival(ctx, t, store, `{"n":1}`)
	arrival(ctx, t, store, `{"n":2}`)
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning while off: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming while off: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("%d deliveries went out while the destination was off", len(claimed))
	}

	if err := store.SaveRoute(ctx, route.ID, route.URL, true); err != nil {
		t.Fatalf("switching it back on: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning after it came back: %v", err)
	}

	claimed, err = store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after it came back: %v", err)
	}
	if len(claimed) != 2 {
		t.Errorf("claimed %d, want both that arrived while it was off", len(claimed))
	}
}
