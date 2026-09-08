package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var (
	ErrNoRole = errors.New("no role with that name")
	// A shipped role is what the panel falls back on, so removing one is
	// refused rather than silently ignored.
	ErrRoleKept = errors.New("that role is shipped with Charon and is not removed")
)

func permissions(granted []string) []authz.Permission {
	out := make([]authz.Permission, 0, len(granted))
	for _, one := range granted {
		out = append(out, authz.Permission(one))
	}
	return out
}

// Permissions is the closed set as the database holds it, which is what a
// grant points at. It exists as rows so that granting something nothing
// enforces fails on a foreign key and not only on a check in Go.
func (s *Store) Permissions(ctx context.Context) (map[authz.Permission]string, error) {
	rows, err := s.q.Permissions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the permissions: %w", err)
	}

	described := make(map[authz.Permission]string, len(rows))
	for _, row := range rows {
		described[authz.Permission(row.Name)] = row.Description
	}
	return described, nil
}

// Role is one role with everything it grants, which is what deciding a request
// needs.
func (s *Store) Role(ctx context.Context, name string) (authz.Role, error) {
	row, err := s.q.Role(ctx, db.RoleParams{TenantID: maybe(s.tenantOf(ctx)), Name: name})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.Role{}, fmt.Errorf("%q: %w", name, ErrNoRole)
		}
		return authz.Role{}, fmt.Errorf("reading role %q: %w", name, err)
	}
	return authz.Role{
		Name:        row.Name,
		Description: row.Description,
		BuiltIn:     row.BuiltIn,
		Grants:      permissions(row.Grants),
	}, nil
}

// SetRole writes a role and exactly the permissions given, so configuring one
// is one statement rather than a diff the caller has to work out.
func (s *Store) SetRole(ctx context.Context, role authz.Role) error {
	if err := role.Validate(); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the role transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	tenant := s.tenantOf(ctx)

	id, err := q.SetRole(ctx, db.SetRoleParams{
		TenantID: maybe(tenant), Name: role.Name, Description: role.Description,
	})
	if err != nil {
		return fmt.Errorf("writing role %q: %w", role.Name, err)
	}
	if err := q.ClearRoleGrants(ctx, id); err != nil {
		return fmt.Errorf("clearing what %q grants: %w", role.Name, err)
	}
	for _, granted := range role.Grants {
		if err := q.GrantPermission(ctx, db.GrantPermissionParams{
			RoleID: id, Name: string(granted),
		}); err != nil {
			return fmt.Errorf("granting %s to %q: %w", granted, role.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing role %q: %w", role.Name, err)
	}
	return nil
}

// DeleteRole leaves the shipped roles alone: they are what the panel falls
// back on, and a deployment that wants them gone can stop mapping anyone to
// them.
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	removed, err := s.q.DeleteRole(ctx, db.DeleteRoleParams{
		TenantID: maybe(s.tenantOf(ctx)), Name: name,
	})
	if err != nil {
		return fmt.Errorf("deleting role %q: %w", name, err)
	}
	if removed == 0 {
		return fmt.Errorf("%q: %w", name, ErrRoleKept)
	}
	return nil
}

func (s *Store) Roles(ctx context.Context) ([]authz.Role, error) {
	rows, err := s.q.Roles(ctx, maybe(s.tenantOf(ctx)))
	if err != nil {
		return nil, fmt.Errorf("reading the roles of %s: %w", s.tenantOf(ctx), err)
	}

	roles := make([]authz.Role, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, authz.Role{
			Name:        row.Name,
			Description: row.Description,
			BuiltIn:     row.BuiltIn,
			Grants:      permissions(row.Grants),
		})
	}
	return roles, nil
}

// Placement resolves where an identity belongs from the claims it presented.
// The first claim that is mapped wins, so the order the identity provider
// sends its groups in decides precedence.
// Placement is where the values an identity carries put it. A value is a path:
// its first part names a tenant, and a second part, when there is one, names
// the role held there. The name is the mapping, so a deployment whose groups
// are named after its tenants registers nothing. A value pointed at a tenant
// explicitly goes there instead, whatever its name would have said.
func (s *Store) Placement(
	ctx context.Context, method string, claims []string,
) ([]console.Placement, error) {
	if len(claims) == 0 {
		return nil, nil
	}

	// Pointed at a tenant explicitly, which is what a directory that names its
	// groups its own way needs. Anything not pointed falls to the convention.
	pointed, err := s.q.PlacementsPointedAt(ctx, db.PlacementsPointedAtParams{
		Method: method, Column2: claims,
	})
	if err != nil {
		return nil, fmt.Errorf("reading where values are pointed: %w", err)
	}

	found := make([]console.Placement, 0, len(claims))
	spoken := map[string]bool{}
	for _, row := range pointed {
		spoken[row.Value] = true
		found = append(found, console.Placement{Tenant: row.TenantID, Role: row.Role})
	}

	wanted := map[string]string{}
	slugs := make([]string, 0, len(claims))
	for _, claim := range claims {
		if spoken[claim] {
			continue
		}

		slug, role := splitPath(claim)
		if slug == "" {
			continue
		}
		// A value naming a role wins over one naming only the tenant, so the
		// order the provider happened to list them in decides nothing.
		if role != "" || wanted[slug] == "" {
			if _, seen := wanted[slug]; !seen || role != "" {
				wanted[slug] = role
			}
		}
		slugs = append(slugs, slug)
	}
	if len(slugs) == 0 {
		return found, nil
	}

	rows, err := s.q.TenantsNamed(ctx, slugs)
	if err != nil {
		return nil, fmt.Errorf("reading the tenants named by an identity: %w", err)
	}
	for _, row := range rows {
		found = append(found, console.Placement{Tenant: row.ID, Role: wanted[row.Slug]})
	}
	return found, nil
}

// splitPath reads a value as the path it is: a tenant, and the role held there
// when the provider says one. Keycloak writes them with a leading separator
// and so may others, which carries no meaning.
func splitPath(value string) (tenant, role string) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], parts[1]
	}
}

// PointValueAt is the exception to the convention: a value that names no
// tenant of ours is said to mean one. Pointing it again moves it, and a role
// left empty leaves whoever arrives at the least.
func (s *Store) PointValueAt(
	ctx context.Context, method, value string, place console.Placement,
) error {
	var role uuid.UUID
	if place.Role != "" {
		found, err := s.roleIDIn(ctx, place)
		if err != nil {
			return err
		}
		role = found
	}

	if err := s.q.PointValueAt(ctx, db.PointValueAtParams{
		Method: method, Value: value, TenantID: place.Tenant, RoleID: maybe(role),
	}); err != nil {
		return fmt.Errorf("pointing %s value %q: %w", method, value, err)
	}
	return nil
}

func (s *Store) StopPointingValue(ctx context.Context, method, value string) error {
	if err := s.q.StopPointingValue(ctx, db.StopPointingValueParams{
		Method: method, Value: value, TenantID: s.tenantOf(ctx),
	}); err != nil {
		return fmt.Errorf("unpointing %s value %q: %w", method, value, err)
	}
	return nil
}

// PointedHere is what this tenant claimed for itself, which is what its panel
// shows and edits.
func (s *Store) PointedHere(ctx context.Context) ([]console.Mapping, error) {
	rows, err := s.q.ValuesPointedAtTenant(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading the values pointed here: %w", err)
	}

	pointed := make([]console.Mapping, 0, len(rows))
	for _, row := range rows {
		pointed = append(pointed, console.Mapping{
			Method: row.Method, Claim: row.Value, Role: row.Role,
		})
	}
	return pointed, nil
}
