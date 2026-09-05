package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var ErrNoOperator = errors.New("no operator with that identifier in this tenant")

// A role is named by an operator and pointed at by an identifier, so this is
// where the one becomes the other.
func (s *Store) roleID(ctx context.Context, name string) (uuid.UUID, error) {
	return s.roleIDIn(ctx, console.Placement{Tenant: s.tenantOf(ctx), Role: name})
}

func (s *Store) roleIDIn(ctx context.Context, place console.Placement) (uuid.UUID, error) {
	id, err := s.q.RoleIDByName(ctx, db.RoleIDByNameParams{
		TenantID: maybe(place.Tenant), Name: place.Role,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("%q: %w", place.Role, ErrNoRole)
		}
		return uuid.Nil, fmt.Errorf("reading role %q: %w", place.Role, err)
	}
	return id, nil
}

func (s *Store) Operators(ctx context.Context) ([]console.Operator, error) {
	rows, err := s.q.PanelUsers(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("listing operators: %w", err)
	}

	operators := make([]console.Operator, 0, len(rows))
	for _, row := range rows {
		operators = append(operators, console.Operator{
			ID:        row.ID,
			Email:     row.Email,
			Role:      row.Role.String,
			Subject:   row.OidcSubject.String,
			CreatedAt: row.CreatedAt,
		})
	}
	return operators, nil
}

func (s *Store) SetOperatorRole(ctx context.Context, id uuid.UUID, role string) error {
	wanted, err := s.roleID(ctx, role)
	if err != nil {
		return err
	}

	changed, err := s.q.UpdatePanelUserRole(ctx, db.UpdatePanelUserRoleParams{
		UserID: id, TenantID: s.tenantOf(ctx), RoleID: maybe(wanted),
	})
	if err != nil {
		return fmt.Errorf("changing the role of operator %s: %w", id, err)
	}
	if changed == 0 {
		return ErrNoOperator
	}
	return nil
}

// LeaveTenant takes somebody out of a tenant. The account stays: it may belong
// to others, and belonging is a relationship rather than a property.
func (s *Store) LeaveTenant(ctx context.Context, user, tenant uuid.UUID) error {
	if _, err := s.q.DeletePanelUser(ctx, db.DeletePanelUserParams{
		UserID: user, TenantID: tenant,
	}); err != nil {
		return fmt.Errorf("taking %s out of a tenant: %w", user, err)
	}
	return nil
}

func (s *Store) DeleteOperator(ctx context.Context, id uuid.UUID) error {
	removed, err := s.q.DeletePanelUser(ctx, db.DeletePanelUserParams{
		UserID: id, TenantID: s.tenantOf(ctx),
	})
	if err != nil {
		return fmt.Errorf("removing operator %s: %w", id, err)
	}
	if removed == 0 {
		return ErrNoOperator
	}
	return nil
}

// SetSystemAdmin makes an account one that is not confined to a tenant, or
// takes that away. Who may call this is decided above: only somebody who
// already is one.
// SystemAdmins are the accounts that belong to no tenant, which is why they
// are listed apart from any tenant's operators.
func (s *Store) SystemAdmins(ctx context.Context) ([]console.Operator, error) {
	rows, err := s.q.SystemAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing system administrators: %w", err)
	}

	admins := make([]console.Operator, 0, len(rows))
	for _, row := range rows {
		admins = append(admins, console.Operator{
			ID:          row.ID,
			Email:       row.Email,
			Subject:     row.OidcSubject.String,
			CreatedAt:   row.CreatedAt,
			SystemAdmin: true,
		})
	}
	return admins, nil
}

// SetSystemAdmin takes the account out of its tenant, because the standing is
// exactly not belonging to one.
func (s *Store) SetSystemAdmin(ctx context.Context, id uuid.UUID) error {
	// The standing is not belonging to a tenant, so it leaves the ones it had:
	// an account cannot both be of the system and be a member somewhere.
	if err := s.q.LeaveEveryTenant(ctx, id); err != nil {
		return fmt.Errorf("taking operator %s out of its tenants: %w", id, err)
	}

	changed, err := s.q.SetSystemAdmin(ctx, id)
	if err != nil {
		return fmt.Errorf("making operator %s a system administrator: %w", id, err)
	}
	if changed == 0 {
		return ErrNoOperator
	}
	return nil
}

// ClearSystemAdmin puts the account back into a tenant, which it has to be
// given because it no longer has one.
func (s *Store) ClearSystemAdmin(ctx context.Context, id uuid.UUID) error {
	changed, err := s.q.ClearSystemAdmin(ctx, id)
	if err != nil {
		return fmt.Errorf("returning operator %s to a tenant: %w", id, err)
	}
	if changed == 0 {
		return ErrNoOperator
	}
	return nil
}

func (s *Store) SetSystemAdminByEmail(ctx context.Context, email string) error {
	if err := s.q.LeaveEveryTenantByEmail(ctx, email); err != nil {
		return fmt.Errorf("taking %q out of its tenants: %w", email, err)
	}

	changed, err := s.q.SetSystemAdminByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("making %q a system administrator: %w", email, err)
	}
	if changed == 0 {
		return ErrNoUser
	}
	return nil
}

func (s *Store) ClearSystemAdminByEmail(ctx context.Context, email string) error {
	changed, err := s.q.ClearSystemAdminByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("returning %q to a tenant: %w", email, err)
	}
	if changed == 0 {
		return ErrNoUser
	}
	return nil
}

func (s *Store) CountSystemAdmins(ctx context.Context) (int64, error) {
	total, err := s.q.CountSystemAdmins(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting system administrators: %w", err)
	}
	return total, nil
}
