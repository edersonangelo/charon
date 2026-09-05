package postgres

import (
	"context"
	"fmt"
)

// Isolation reports whether the database is enforcing the separation as well
// as Charon does. Charon keeps tenants apart on its own: every row carries the
// tenant it belongs to and every query is scoped to one. Row level security is
// the same rule again, one layer down, for a deployment that wants the database
// to refuse rather than trust the application. It applies only to a role that
// is neither a superuser nor carries BYPASSRLS.
type Isolation struct {
	// Enforced is false when the connected role is a superuser or carries
	// BYPASSRLS. Postgres exempts both, and a policy that does not apply is
	// decoration.
	Enforced bool
	Tenants  int64
	Role     string
}

func (s *Store) Isolation(ctx context.Context) (Isolation, error) {
	// Straight to pgx: this reads a catalog table, which is not part of the
	// schema the query layer is generated from.
	var role string
	var bypassed bool
	if err := s.pool.QueryRow(ctx, `
		select current_user, coalesce(bool_or(rolsuper or rolbypassrls), false)
		from pg_roles where rolname = current_user
	`).Scan(&role, &bypassed); err != nil {
		return Isolation{}, fmt.Errorf("reading the role's privileges: %w", err)
	}

	tenants, err := s.q.CountTenants(ctx)
	if err != nil {
		return Isolation{}, fmt.Errorf("counting tenants: %w", err)
	}

	return Isolation{Enforced: !bypassed, Tenants: tenants, Role: role}, nil
}
