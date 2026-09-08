package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

// The bug this guards: the process that receives webhooks resolves a slug once
// and holds it, while the one that removes and makes tenants is the command
// line or the panel. Held across that, a slug made again points at the tenant
// that is gone, and every webhook sent to that address is refused with a
// foreign key violation for as long as the process lives.
func TestASlugMadeAgainIsTheNewTenant(t *testing.T) {
	t.Parallel()

	receiving, dsn := open(t)
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()

	// A second store on the same database is the other process: the command
	// line, or the panel, neither of which runs where webhooks are received.
	administering, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the administering store: %v", err)
	}
	defer administering.Close()

	changes := receiving.TenantChanges(ctx)
	go func() {
		for range changes {
			receiving.ForgetTenants()
		}
	}()
	time.Sleep(500 * time.Millisecond)

	first, err := administering.CreateTenant(ctx, "reused", "First")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	held, found, err := receiving.Tenant(ctx, "reused")
	if err != nil || !found {
		t.Fatalf("resolving: %v", err)
	}
	if held != first.ID {
		t.Fatalf("resolved to %s, want %s", held, first.ID)
	}

	if err := administering.DeleteTenant(ctx, "reused"); err != nil {
		t.Fatalf("removing: %v", err)
	}
	second, err := administering.CreateTenant(ctx, "reused", "Second")
	if err != nil {
		t.Fatalf("creating it again: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("the same identifier came back, so this proves nothing")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		again, found, resolveErr := receiving.Tenant(ctx, "reused")
		if resolveErr != nil {
			t.Fatalf("resolving again: %v", resolveErr)
		}
		if found && again == second.ID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the address still points at the removed tenant: got %s, want %s",
				again, second.ID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Removing a tenant in one process has to reach the one receiving webhooks,
// which is what the announcement is for.
func TestATenantChangeIsAnnounced(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()

	changes := store.TenantChanges(ctx)
	// The listener connects in the background; without this the notification
	// is sent before anything is subscribed to hear it.
	time.Sleep(500 * time.Millisecond)

	if _, err := store.CreateTenant(ctx, "announced", "Announced"); err != nil {
		t.Fatalf("creating: %v", err)
	}

	select {
	case _, open := <-changes:
		if !open {
			t.Fatal("the channel closed instead of reporting the change")
		}
	case <-ctx.Done():
		t.Fatal("creating a tenant was not announced")
	}
}
