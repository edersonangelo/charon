package postgres_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

func claimedInAnyOrder(ctx context.Context, t *testing.T, store *postgres.Store) []string {
	t.Helper()

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	got := bodies(claimed)
	slices.Sort(got)
	return got
}

func plan(ctx context.Context, t *testing.T, store *postgres.Store) {
	t.Helper()

	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
}

func TestARouteWithNothingToCatchUpOnDeliversSideBySide(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "nothing-behind")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`, `{"n":2}`}) {
		t.Errorf("claimed %v, want both at once: nothing was waiting when the route was made", got)
	}
}

func TestARouteThatSkipsWhatWasRecordedDeliversSideBySide(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "skips-past")

	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":0}`, start)
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.SkipAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start.Add(time.Second))
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(2*time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`, `{"n":2}`}) {
		t.Errorf("claimed %v, want both at once: a route that skips the past has nothing to catch up on", got)
	}
}

func TestWhatArrivesWhileARouteCatchesUpWaitsBehindIt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "waits-behind")

	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`}) {
		t.Errorf("claimed %v, want only what was recorded before the route, with what came after waiting behind it", got)
	}
}

func TestARouteThatCaughtUpDeliversSideBySide(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "caught-up")

	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	plan(ctx, t, store)
	past, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(past) != 1 {
		t.Fatalf("claiming the past: %v (%v)", err, bodies(past))
	}
	if err := store.MarkDelivered(ctx, past[0].Tenant, past[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	arrivedAt(ctx, t, store, "erp", `{"n":3}`, start.Add(2*time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":2}`, `{"n":3}`}) {
		t.Errorf("claimed %v, want both at once once the past was delivered", got)
	}
}

func TestADestinationSwitchedBackOnCatchesUpInOrder(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "back-on")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.SkipAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	route := theRoute(ctx, t, store)
	if err := store.SaveRoute(ctx, route.ID, route.URL, false); err != nil {
		t.Fatalf("switching it off: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	plan(ctx, t, store)

	if err := store.SaveRoute(ctx, route.ID, route.URL, true); err != nil {
		t.Fatalf("switching it back on: %v", err)
	}
	arrivedAt(ctx, t, store, "erp", `{"n":3}`, start.Add(2*time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`}) {
		t.Errorf("claimed %v, want what waited while it was off delivered in order, first one first", got)
	}
}

func TestACorrectedAddressCatchesUpInOrder(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "readdressed")

	if err := store.AddRoute(ctx, "erp", "sink", "https://wrong.example/h", "http", console.SkipAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	plan(ctx, t, store)

	failing, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(failing) != 2 {
		t.Fatalf("claiming side by side: %v (%v)", err, bodies(failing))
	}
	for _, delivery := range failing {
		if err := store.MarkFailed(ctx, delivery.Tenant, delivery.ID,
			time.Now().Add(time.Hour), 404, "destination answered 404", 12); err != nil {
			t.Fatalf("failing: %v", err)
		}
	}

	route := theRoute(ctx, t, store)
	if err := store.SaveRoute(ctx, route.ID, "https://right.example/h", true); err != nil {
		t.Fatalf("correcting the address: %v", err)
	}

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`}) {
		t.Errorf("claimed %v, want what failed against the wrong address delivered in order, first one first", got)
	}
}

// The route caught up when its past was delivered, and nothing has claimed
// since to notice. A replay of that past must not put the route back in order:
// the newer event goes alongside it.
func TestAnOldEventReplayedOnceCaughtUpGoesSideBySide(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "replayed-after")

	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	plan(ctx, t, store)
	past, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(past) != 1 {
		t.Fatalf("claiming the past: %v (%v)", err, bodies(past))
	}
	if err := store.MarkDelivered(ctx, past[0].Tenant, past[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	if replayed, err := store.ReplayDelivery(ctx, past[0].ID); err != nil || replayed != 1 {
		t.Fatalf("replaying: %v (%d)", err, replayed)
	}
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":1}`, `{"n":2}`}) {
		t.Errorf("claimed %v, want the replay and the newer event side by side", got)
	}
}

// Everything the route was given has been delivered, but an event from before
// it caught up is still waiting to be planned. The route has not caught up: once
// that event is planned it goes first, and what arrived after waits behind it.
func TestARouteHasNotCaughtUpWhileItsPastWaitsToBePlanned(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "unplanned-past")

	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if _, err := store.Plan(ctx, 1); err != nil {
		t.Fatalf("planning only the first: %v", err)
	}
	first, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claiming the first: %v (%v)", err, bodies(first))
	}
	if err := store.MarkDelivered(ctx, first[0].Tenant, first[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}
	if nothing := claimedInAnyOrder(ctx, t, store); len(nothing) != 0 {
		t.Fatalf("claimed %v with the second still unplanned", nothing)
	}

	arrivedAt(ctx, t, store, "erp", `{"n":3}`, start.Add(2*time.Second))
	plan(ctx, t, store)

	if got := claimedInAnyOrder(ctx, t, store); !slices.Equal(got, []string{`{"n":2}`}) {
		t.Errorf("claimed %v, want the second alone: the route was still catching up on it", got)
	}
}
