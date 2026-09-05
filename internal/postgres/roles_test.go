package postgres_test

import (
	"context"
	"slices"
	"testing"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres"
)

// The shipped roles are written twice: in Go, so the code can describe them,
// and in a migration, so a fresh database has them before anything runs. Two
// representations of one thing drift, and this is what notices.
func TestTheSeededRolesMatchTheOnesTheCodeShips(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	stored, err := store.Roles(context.Background())
	if err != nil {
		t.Fatalf("reading the seeded roles: %v", err)
	}

	byName := map[string]authz.Role{}
	for _, role := range stored {
		byName[role.Name] = role
	}

	for _, shipped := range authz.BuiltIn() {
		seeded, present := byName[shipped.Name]
		if !present {
			t.Errorf("role %q is shipped in code and missing from the migration", shipped.Name)
			continue
		}

		wanted := slices.Clone(shipped.Grants)
		got := slices.Clone(seeded.Grants)
		slices.Sort(wanted)
		slices.Sort(got)

		if !slices.Equal(wanted, got) {
			t.Errorf("role %q grants %v in the database and %v in the code",
				shipped.Name, got, wanted)
		}
		if seeded.Description != shipped.Description {
			t.Errorf("role %q is described differently in the database and in the code",
				shipped.Name)
		}
		if !seeded.BuiltIn {
			t.Errorf("role %q is not marked built in", shipped.Name)
		}
	}

	if len(stored) != len(authz.BuiltIn()) {
		t.Errorf("the database has %d roles and the code ships %d",
			len(stored), len(authz.BuiltIn()))
	}
}

// Every permission a seeded role grants has to be one the code enforces.
// The permissions are written twice as well: as a constant set in Go, because
// the code checks them, and as rows, because a grant points at one. This is
// what notices when the two stop agreeing.
func TestThePermissionsInTheDatabaseAreTheOnesTheCodeEnforces(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	stored, err := store.Permissions(context.Background())
	if err != nil {
		t.Fatalf("reading the permissions: %v", err)
	}

	shipped := authz.Described()
	if len(stored) != len(shipped) {
		t.Errorf("the database holds %d permissions, the code enforces %d", len(stored), len(shipped))
	}

	for permission, description := range shipped {
		held, present := stored[permission]
		if !present {
			t.Errorf("permission %q is enforced in code and missing from the migration", permission)
			continue
		}
		if held != description {
			t.Errorf("permission %q is described as %q in the database and %q in code",
				permission, held, description)
		}
	}
	for permission := range stored {
		if _, enforced := shipped[permission]; !enforced {
			t.Errorf("the database holds permission %q that nothing enforces", permission)
		}
	}
}

func TestNoSeededRoleGrantsAPermissionNothingEnforces(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	stored, err := store.Roles(context.Background())
	if err != nil {
		t.Fatalf("reading the seeded roles: %v", err)
	}

	for _, role := range stored {
		if err := role.Validate(); err != nil {
			t.Errorf("seeded role %q: %v", role.Name, err)
		}
	}
}

// A transport, a verifier or a way of signing in is registered in Go, and a
// key in the database points at what exists. The rows come from the process
// that has them, so adding one is writing Go and starting, not writing a
// migration.
func TestWhatABuildKnowsIsRecordedByStarting(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	if err := store.AddRoute(ctx, "stripe", "pigeons", "https://roof.example", "carrier-pigeon"); err == nil {
		t.Fatal("a destination was created on a transport nothing registered")
	}

	if err := store.Register(ctx, postgres.Kinds{Transports: []string{"carrier-pigeon"}}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if err := store.AddRoute(ctx, "stripe", "pigeons", "https://roof.example", "carrier-pigeon"); err != nil {
		t.Fatalf("routing over a registered transport: %v", err)
	}

	// Starting again says the same thing, and says it once.
	if err := store.Register(ctx, postgres.Kinds{Transports: []string{"carrier-pigeon"}}); err != nil {
		t.Fatalf("registering a second time: %v", err)
	}
}
