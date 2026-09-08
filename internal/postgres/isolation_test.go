package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

// Filtering in the query is the application being careful; row level security
// is the database refusing regardless. This proves the second one, which is
// the only one that still holds when a query is wrong.
func TestTheDatabaseRefusesAnotherTenantEvenWhenTheQueryDoesNot(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	first, err := store.TenantBySlug(ctx, postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	second, err := store.CreateTenant(ctx, "second", "Second")
	if err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}

	mine := request([]byte(`{"whose":"mine"}`))
	mine.Tenant = first.ID
	yours := request([]byte(`{"whose":"yours"}`))
	yours.Tenant = second.ID

	mineID, err := store.Record(ctx, mine)
	if err != nil {
		t.Fatalf("recording for the first tenant: %v", err)
	}
	if _, err := store.Record(ctx, yours); err != nil {
		t.Fatalf("recording for the second tenant: %v", err)
	}

	confined := asConfinedRole(t, dsn)

	isolation, err := confined.Isolation(ctx)
	if err != nil {
		t.Fatalf("reading the isolation in force: %v", err)
	}
	if !isolation.Enforced {
		t.Fatalf("role %q still bypasses row level security", isolation.Role)
	}

	found, err := confined.SearchEvents(authz.WithTenant(ctx, first.ID), console.Filter{})
	if err != nil {
		t.Fatalf("searching as the first tenant: %v", err)
	}
	if len(found) != 1 || found[0].ID != mineID {
		t.Errorf("saw %d events, want only the one recorded for that tenant", len(found))
	}

	// Nothing said which tenant, so the database answers for none of them.
	blind, err := confined.SearchEvents(ctx, console.Filter{})
	if err != nil {
		t.Fatalf("searching with no tenant: %v", err)
	}
	if len(blind) != 0 {
		t.Errorf("a query for no tenant returned %d events, want none", len(blind))
	}
}

// asConfinedRole opens a second store as a role Postgres does not exempt from
// row level security, which is what a deployment is meant to run the panel as.
func asConfinedRole(t *testing.T, dsn string) *postgres.Store {
	t.Helper()

	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to prepare the role: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := "charon_app_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, statement := range []string{
		fmt.Sprintf("create role %s login password 'confined' nosuperuser nobypassrls", name),
		fmt.Sprintf("grant usage on schema public to %s", name),
		fmt.Sprintf("grant select, insert, update, delete on all tables in schema public to %s", name),
		fmt.Sprintf("grant execute on all functions in schema public to %s", name),
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing the confined role: %v", err)
		}
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the database url: %v", err)
	}
	parsed.User = url.UserPassword(name, "confined")

	confined, err := postgres.Open(ctx, parsed.String())
	if err != nil {
		t.Fatalf("opening the store as %s: %v", name, err)
	}
	t.Cleanup(func() {
		confined.Close()

		cleanup, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close(ctx) }()

		for _, statement := range []string{
			fmt.Sprintf("revoke all on all tables in schema public from %s", name),
			fmt.Sprintf("revoke all on all functions in schema public from %s", name),
			fmt.Sprintf("revoke all on schema public from %s", name),
			fmt.Sprintf("drop role %s", name),
		} {
			_, _ = cleanup.Exec(ctx, statement)
		}
	})

	return confined
}
