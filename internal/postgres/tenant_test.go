package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

// Two tenants, one database, and a query that names neither: what it returns
// is the whole point of the tenant.
func TestATenantOnlySeesWhatArrivedForIt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	first, err := store.TenantBySlug(ctx, postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	second, err := store.CreateTenant(ctx, "second", "Second")
	if err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}

	ours := authz.WithTenant(ctx, first.ID)
	theirs := authz.WithTenant(ctx, second.ID)

	mine := request([]byte(`{"whose":"mine"}`))
	mine.Tenant = first.ID
	yours := request([]byte(`{"whose":"yours"}`))
	yours.Tenant = second.ID
	yours.Provider = "shopify"

	mineID, err := store.Record(ctx, mine)
	if err != nil {
		t.Fatalf("recording for the first tenant: %v", err)
	}
	yoursID, err := store.Record(ctx, yours)
	if err != nil {
		t.Fatalf("recording for the second tenant: %v", err)
	}

	found, err := store.SearchEvents(ours, console.Filter{})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(found) != 1 || found[0].ID != mineID {
		t.Fatalf("a search saw %d events, want only the one recorded for that tenant", len(found))
	}

	if _, err := store.EventDetail(ours, yoursID); !errors.Is(err, postgres.ErrNoEvent) {
		t.Errorf("reading another tenant's event by id: %v, want %v", err, postgres.ErrNoEvent)
	}
	if _, err := store.EventDetail(theirs, yoursID); err != nil {
		t.Errorf("a tenant cannot read its own event: %v", err)
	}

	providers, err := store.Providers(ours)
	if err != nil {
		t.Fatalf("listing providers: %v", err)
	}
	if len(providers) != 1 || providers[0] != "stripe" {
		t.Errorf("providers = %v, want only the one that sent to that tenant", providers)
	}
}

// A route is configuration, and configuration belongs to the tenant that made
// it: the same provider name in two tenants is two different arrangements.
func TestRoutesDoNotCrossTenants(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	first, err := store.TenantBySlug(ctx, postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	second, err := store.CreateTenant(ctx, "second", "Second")
	if err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}

	ours := authz.WithTenant(ctx, first.ID)
	theirs := authz.WithTenant(ctx, second.ID)

	if err := store.AddRoute(ours, "stripe", "ours", "https://ours.example/hooks", ""); err != nil {
		t.Fatalf("routing for the first tenant: %v", err)
	}
	// The same destination name and the same url, which is only possible
	// because they are different tenants.
	if err := store.AddRoute(theirs, "stripe", "ours", "https://ours.example/hooks", ""); err != nil {
		t.Fatalf("routing for the second tenant: %v", err)
	}

	for name, scope := range map[string]context.Context{"first": ours, "second": theirs} {
		routes, err := store.Routes(scope)
		if err != nil {
			t.Fatalf("listing the routes of the %s tenant: %v", name, err)
		}
		if len(routes) != 1 {
			t.Errorf("the %s tenant sees %d routes, want 1", name, len(routes))
		}
	}
}

// Every tenant gets the shipped roles when it is created, because a tenant
// nobody can be an operator of is of no use.
func TestANewTenantIsUsableImmediately(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	created, err := store.CreateTenant(ctx, "fresh", "Fresh")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}

	roles, err := store.Roles(authz.WithTenant(ctx, created.ID))
	if err != nil {
		t.Fatalf("reading the roles: %v", err)
	}
	if len(roles) != len(authz.BuiltIn()) {
		t.Errorf("a new tenant has %d roles, want the %d shipped", len(roles), len(authz.BuiltIn()))
	}
}

func TestAnUnknownSlugIsNotATenant(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	if _, known, err := store.Tenant(context.Background(), "nobody"); err != nil || known {
		t.Errorf("Tenant(%q) = known %v, err %v; want not known and no error", "nobody", known, err)
	}
}
