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

func failingDelivery(ctx context.Context, t *testing.T, store *postgres.Store) (console.RouteRow, string) {
	t.Helper()

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/wrong", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}

	// Five attempts against an address that was never going to answer, which
	// is a backoff of a good while.
	for attempt := 1; attempt <= 5; attempt++ {
		if err := store.MarkFailed(ctx, claimed[0].Tenant, claimed[0].ID,
			time.Now().Add(time.Hour), 404, "destination answered 404", 12); err != nil {
			t.Fatalf("failing attempt %d: %v", attempt, err)
		}
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("reading the route: %v (%d)", err, len(routes))
	}
	return routes[0], claimed[0].ID.String()
}

// The address was wrong, so what failed was the address. Correcting it is the
// operator saying so, and whatever is waiting for that destination should be
// tried at once instead of serving out a backoff earned by a typo.
func TestCorrectingTheAddressTriesWhatWaitsAtOnce(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "wrong-address", "Wrong")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	route, _ := failingDelivery(ctx, t, store)

	// Nothing is due yet: the backoff has an hour to run.
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("something was due while the backoff was still running")
	}

	if err := store.SaveRoute(ctx, route.ID, "https://sink.example/right", true); err != nil {
		t.Fatalf("correcting the address: %v", err)
	}

	claimed, err = store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after the correction: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want the one waiting on the corrected address", len(claimed))
	}
	if claimed[0].URL != "https://sink.example/right" {
		t.Errorf("it is being sent to %q", claimed[0].URL)
	}
	// The attempts were spent on somewhere that never existed.
	if claimed[0].Attempts != 0 {
		t.Errorf("it carries %d attempts, want the budget back", claimed[0].Attempts)
	}
}

// Saving a route without touching the address changes nothing about what is
// waiting: a backoff earned by a destination that is genuinely down is not
// cleared by opening the page and pressing save.
func TestSavingWithoutChangingTheAddressLeavesTheBackoff(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "same-address", "Same")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	route, _ := failingDelivery(ctx, t, store)

	if err := store.SaveRoute(ctx, route.ID, route.URL, true); err != nil {
		t.Fatalf("saving: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("pressing save cleared a backoff the destination had earned")
	}
}
