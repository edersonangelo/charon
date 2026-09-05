package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var ErrNoTenant = errors.New("no tenant with that slug")

// DefaultSlug is the tenant every deployment has.
const DefaultSlug = "default"

func (s *Store) Tenants(ctx context.Context) ([]console.Tenant, error) {
	rows, err := s.q.Tenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}

	tenants := make([]console.Tenant, 0, len(rows))
	for _, row := range rows {
		tenants = append(tenants, console.Tenant{
			ID: row.ID, Slug: row.Slug, Name: row.Name, CreatedAt: row.CreatedAt,
		})
	}
	return tenants, nil
}

func (s *Store) TenantBySlug(ctx context.Context, slug string) (console.Tenant, error) {
	row, err := s.q.TenantBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return console.Tenant{}, fmt.Errorf("%q: %w", slug, ErrNoTenant)
		}
		return console.Tenant{}, fmt.Errorf("reading tenant %q: %w", slug, err)
	}
	return console.Tenant{
		ID: row.ID, Slug: row.Slug, Name: row.Name, CreatedAt: row.CreatedAt,
	}, nil
}

// CreateTenant makes a tenant. The roles Charon ships belong to no tenant and
// are held in every one, so there is nothing to seed alongside it.
func (s *Store) CreateTenant(ctx context.Context, slug, name string) (console.Tenant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return console.Tenant{}, fmt.Errorf("beginning the tenant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	row, err := q.CreateTenant(ctx, db.CreateTenantParams{Slug: slug, Name: name})
	if err != nil {
		return console.Tenant{}, fmt.Errorf("creating tenant %q: %w", slug, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return console.Tenant{}, fmt.Errorf("committing tenant %q: %w", slug, err)
	}
	return console.Tenant{
		ID: row.ID, Slug: row.Slug, Name: row.Name, CreatedAt: row.CreatedAt,
	}, nil
}

func (s *Store) DeleteTenant(ctx context.Context, slug string) error {
	if slug == DefaultSlug {
		return errors.New("the default tenant cannot be removed")
	}
	if err := s.q.DeleteTenant(ctx, slug); err != nil {
		return fmt.Errorf("deleting tenant %q: %w", slug, err)
	}
	s.forgetTenant(slug)
	return nil
}

// Tenant resolves a slug to its id. The mapping never changes for a living
// tenant, so it is held to keep an address lookup off every inbound request.
func (s *Store) Tenant(ctx context.Context, slug string) (uuid.UUID, bool, error) {
	s.tenantMu.RLock()
	id, held := s.tenantBySlug[slug]
	s.tenantMu.RUnlock()
	if held {
		return id, true, nil
	}

	tenant, err := s.TenantBySlug(ctx, slug)
	switch {
	case errors.Is(err, ErrNoTenant):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, err
	}

	s.tenantMu.Lock()
	s.tenantBySlug[slug] = tenant.ID
	s.tenantMu.Unlock()

	return tenant.ID, true, nil
}

func (s *Store) forgetTenant(slug string) {
	s.tenantMu.Lock()
	delete(s.tenantBySlug, slug)
	s.tenantMu.Unlock()
}

// tenantOf is the tenant a query belongs to. A context that names none acts
// for the tenant every deployment has, found by its slug because its
// identifier is the database's to choose. A tenant that cannot be resolved
// scopes the query to nothing rather than to everything.
func (s *Store) tenantOf(ctx context.Context) uuid.UUID {
	if tenant, scoped := authz.TenantFrom(ctx); scoped {
		return tenant
	}
	id, known, err := s.Tenant(ctx, DefaultSlug)
	if err != nil || !known {
		return uuid.Nil
	}
	return id
}
