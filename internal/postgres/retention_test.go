package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func aged(ctx context.Context, t *testing.T, store *postgres.Store, age time.Duration) {
	t.Helper()

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC().Add(-age).Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"id":"evt"}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
}

func TestNothingIsDiscardedUntilATenantAsks(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "keeps-everything", "Keeps")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	aged(ctx, t, store, 400*24*time.Hour)

	discarded, err := store.Purge(context.Background())
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if discarded != 0 {
		t.Errorf("discarded %d events from a tenant that never asked", discarded)
	}
}

func TestWhatIsPastRetentionGoes(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "keeps-thirty", "Keeps thirty")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	aged(ctx, t, store, 40*24*time.Hour)
	aged(ctx, t, store, 2*24*time.Hour)

	if err := store.SetRetention(context.Background(), "keeps-thirty", 30); err != nil {
		t.Fatalf("setting retention: %v", err)
	}

	waiting, err := store.Purgeable(context.Background())
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if waiting != 1 {
		t.Errorf("says %d would go, want 1", waiting)
	}

	discarded, err := store.Purge(context.Background())
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if discarded != 1 {
		t.Fatalf("discarded %d, want 1", discarded)
	}

	events, err := store.SearchEvents(ctx, console.Filter{PageSize: 10})
	if err != nil {
		t.Fatalf("reading what is left: %v", err)
	}
	if len(events.Events) != 1 {
		t.Errorf("%d events left, want the recent one only", len(events.Events))
	}
}

// A delivery still being attempted is live work. Its age says the destination
// has been unreachable for a long time, which is a reason to look at it rather
// than to erase the evidence.
func TestAnEventStillBeingDeliveredStays(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "keeps-pending", "Keeps pending")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "stripe", "billing",
		"https://billing.internal/hook", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	aged(ctx, t, store, 40*24*time.Hour)
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	if err := store.SetRetention(context.Background(), "keeps-pending", 30); err != nil {
		t.Fatalf("setting retention: %v", err)
	}

	discarded, err := store.Purge(context.Background())
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if discarded != 0 {
		t.Error("an event with a delivery still pending was discarded")
	}
}

func TestRetentionHasToBeASpan(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	if _, err := store.CreateTenant(context.Background(), "keeps-badly", "Keeps badly"); err != nil {
		t.Fatalf("creating: %v", err)
	}

	for _, days := range []int{-1, 4000} {
		if err := store.SetRetention(context.Background(), "keeps-badly", days); !errors.Is(err, postgres.ErrRetentionSpan) {
			t.Errorf("%d days gave %v, want ErrRetentionSpan", days, err)
		}
	}
}

func TestRetentionOfATenantThatIsNotThere(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	if err := store.SetRetention(context.Background(), "no-such-tenant", 30); !errors.Is(err, postgres.ErrNoTenant) {
		t.Errorf("got %v, want ErrNoTenant", err)
	}
}
