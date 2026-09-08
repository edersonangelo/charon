package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/inbound"
)

// Metrics are the one read that is deliberately not scoped to one tenant, so
// what matters is that it still sees every tenant under row level security.
func TestASnapshotSeesEveryTenant(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	for _, slug := range []string{"measured-one", "measured-two"} {
		tenant, err := store.CreateTenant(ctx, slug, slug)
		if err != nil {
			t.Fatalf("creating %s: %v", slug, err)
		}
		scope := authz.WithTenant(ctx, tenant.ID)
		if _, err := store.Record(scope, inbound.Request{
			Provider:   "stripe",
			Path:       "/webhooks/stripe",
			ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"id":"evt"}`),
		}); err != nil {
			t.Fatalf("recording for %s: %v", slug, err)
		}
	}

	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatalf("taking a snapshot: %v", err)
	}

	seen := map[string]bool{}
	for _, measure := range snapshot.Events {
		seen[measure.Tenant] = true
	}
	for _, slug := range []string{"measured-one", "measured-two"} {
		if !seen[slug] {
			t.Errorf("the snapshot does not see %s: %+v", slug, snapshot.Events)
		}
	}
	if snapshot.Tenants < 2 {
		t.Errorf("counted %v tenants", snapshot.Tenants)
	}

	// The tenant is pinned on the connection rather than on the statement, so
	// a query that forgot to name it would report one tenant's rows under
	// every tenant's name.
	counted := map[string]int{}
	for _, measure := range snapshot.Events {
		counted[measure.Tenant+"/"+measure.Kind]++
	}
	for series, times := range counted {
		if times > 1 {
			t.Errorf("%s is counted %d times", series, times)
		}
	}
}
