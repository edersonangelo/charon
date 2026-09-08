package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres"
)

// What a build knows, a build writes. A migration that seeded something once
// is not a guarantee it is still there, and every one of these rows is a
// locked door when it is missing: no permission is grantable, no role grants
// anything, and an event cannot even be recorded because its state has nowhere
// to point.
func TestAStartPutsBackWhatTheBuildKnows(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, emptied := range []string{
		"delete from role_grant",
		"delete from role where built_in",
		"delete from permission",
		"delete from delivery_state",
		"delete from signature_state",
	} {
		if _, err := conn.Exec(ctx, emptied); err != nil {
			t.Fatalf("%s: %v", emptied, err)
		}
	}

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	for table, want := range map[string]int{
		"permission":      len(authz.All()),
		"delivery_state":  3,
		"signature_state": 4,
	} {
		var got int
		if err := conn.QueryRow(ctx, "select count(*) from "+table).Scan(&got); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s has %d rows, want %d", table, got, want)
		}
	}

	for _, want := range authz.BuiltIn() {
		role, err := store.Role(ctx, want.Name)
		if err != nil {
			t.Fatalf("reading %s: %v", want.Name, err)
		}
		if len(role.Grants) != len(want.Grants) {
			t.Errorf("%s grants %v, want %v", want.Name, role.Grants, want.Grants)
		}
	}
}

// Registering twice changes nothing, because every start does it.
func TestRegisteringAgainChangesNothing(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	count := func() int {
		var n int
		if err := conn.QueryRow(ctx,
			"select count(*) from role_grant").Scan(&n); err != nil {
			t.Fatalf("counting: %v", err)
		}
		return n
	}

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	first := count()
	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering again: %v", err)
	}
	if again := count(); again != first {
		t.Errorf("a second start changed the grants from %d to %d", first, again)
	}
}
