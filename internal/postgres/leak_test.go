package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func recordFor(ctx context.Context, t *testing.T, store *postgres.Store, provider string) {
	t.Helper()

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   provider,
		Path:       "/webhooks/" + provider,
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording for %s: %v", provider, err)
	}
}

// The tenant is carried on the connection, and connections are reused. If the
// value outlived the query that set it, whoever picked that connection up next
// would be answered as the tenant before them — which is the one thing row
// level security is here to stop.
func TestATenantDoesNotOutliveTheQueryThatSetIt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	mine, err := store.CreateTenant(ctx, "leak-mine", "Mine")
	if err != nil {
		t.Fatalf("creating a tenant: %v", err)
	}
	yours, err := store.CreateTenant(ctx, "leak-yours", "Yours")
	if err != nil {
		t.Fatalf("creating the other tenant: %v", err)
	}

	recordFor(authz.WithTenant(ctx, mine.ID), t, store, "mine")
	recordFor(authz.WithTenant(ctx, yours.ID), t, store, "yours")

	// Enough turns that the pool hands the same connection back and forth.
	for range 20 {
		seen, err := store.SearchEvents(authz.WithTenant(ctx, mine.ID), console.Filter{PageSize: 50})
		if err != nil {
			t.Fatalf("searching as mine: %v", err)
		}
		for _, event := range seen.Events {
			if event.Provider != "mine" {
				t.Fatalf("as one tenant, saw %q", event.Provider)
			}
		}

		seen, err = store.SearchEvents(authz.WithTenant(ctx, yours.ID), console.Filter{PageSize: 50})
		if err != nil {
			t.Fatalf("searching as yours: %v", err)
		}
		for _, event := range seen.Events {
			if event.Provider != "yours" {
				t.Fatalf("as the other tenant, saw %q", event.Provider)
			}
		}
	}
}

// The same, from many goroutines at once, which is how the panel and the
// dispatcher actually use the pool.
func TestTenantsDoNotCrossOnAContendedPool(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	tenants := map[string]console.Tenant{}
	for _, slug := range []string{"busy-one", "busy-two", "busy-three"} {
		tenant, err := store.CreateTenant(ctx, slug, slug)
		if err != nil {
			t.Fatalf("creating %s: %v", slug, err)
		}
		tenants[slug] = tenant
		recordFor(authz.WithTenant(ctx, tenant.ID), t, store, slug)
	}

	var wait sync.WaitGroup
	for slug, tenant := range tenants {
		for range 8 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for range 10 {
					seen, err := store.SearchEvents(
						authz.WithTenant(ctx, tenant.ID), console.Filter{PageSize: 50})
					if err != nil {
						t.Errorf("searching as %s: %v", slug, err)
						return
					}
					for _, event := range seen.Events {
						if event.Provider != slug {
							t.Errorf("as %s, saw an event of %q", slug, event.Provider)
							return
						}
					}
				}
			}()
		}
	}
	wait.Wait()
}
