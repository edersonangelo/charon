package postgres_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres"
)

func grantsOf(ctx context.Context, t *testing.T, store *postgres.Store, name string) []authz.Permission {
	t.Helper()

	role, err := store.Role(ctx, name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	got := slices.Clone(role.Grants)
	slices.Sort(got)
	return got
}

// What happened for real: a version added a permission, the shipped roles were
// left alone because they already granted something, and the permission reached
// nobody who was already running. The feature it belonged to was dead on every
// upgraded deployment.
func TestAPermissionThisBuildAddsReachesAnUntouchedRole(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	// A deployment that started before events.force existed: the role is there
	// and grants everything except the newest thing.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `
		delete from role_grant g
		using role r, permission p
		where g.role_id = r.id and g.permission_id = p.id
		  and r.name = 'admin' and p.name = 'events.force'`); err != nil {
		t.Fatalf("taking the permission away: %v", err)
	}

	if slices.Contains(grantsOf(ctx, t, store, authz.Admin), authz.EventsForce) {
		t.Fatal("the setup did not take the permission away")
	}

	// Starting again is the upgrade.
	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering again: %v", err)
	}

	if !slices.Contains(grantsOf(ctx, t, store, authz.Admin), authz.EventsForce) {
		t.Error("a permission this build ships did not reach a role nobody had changed")
	}
}

// The other half: a role somebody narrowed on purpose stays narrowed, however
// many times the thing restarts.
func TestARoleSomebodyChangedIsLeftAlone(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "changed-here", "Changed")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	if err := store.SetRole(ctx, authz.Role{
		Name:        authz.Admin,
		Description: "narrowed on purpose",
		Grants:      []authz.Permission{authz.EventsRead},
	}); err != nil {
		t.Fatalf("narrowing: %v", err)
	}

	for range 3 {
		if err := store.Register(ctx, postgres.Kinds{}); err != nil {
			t.Fatalf("registering again: %v", err)
		}
	}

	got := grantsOf(ctx, t, store, authz.Admin)
	if len(got) != 1 || got[0] != authz.EventsRead {
		t.Errorf("the narrowed role grants %v, want only what was configured", got)
	}
}

// A role nobody has changed grants exactly what the build says, so a permission
// the build stops shipping stops being granted rather than lingering.
func TestAnUntouchedRoleGrantsExactlyWhatTheBuildSays(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// Something a past version granted and this one does not.
	if _, err := conn.Exec(ctx, `
		insert into role_grant (role_id, permission_id)
		select r.id, p.id from role r, permission p
		where r.name = 'viewer' and p.name = 'tenants.write'`); err != nil {
		t.Fatalf("granting something extra: %v", err)
	}

	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering again: %v", err)
	}

	if slices.Contains(grantsOf(ctx, t, store, authz.Viewer), authz.TenantsWrite) {
		t.Error("a viewer still grants something this build does not ship")
	}
}
