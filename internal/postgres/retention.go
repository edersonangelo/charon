package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

// Retention is how long one tenant's events are kept. Days is zero when they
// are kept for ever, which is what a tenant nobody has configured gets.
type Retention struct {
	Slug string
	Days int
}

var ErrRetentionSpan = errors.New("retention is between 1 and 3650 days, or none to keep for ever")

// SetRetention says how long a tenant's events are kept. Days of zero puts it
// back to keeping them for ever.
func (s *Store) SetRetention(ctx context.Context, slug string, days int) error {
	if days < 0 || days > 3650 {
		return ErrRetentionSpan
	}

	span := pgtype.Int4{}
	if days > 0 {
		span = pgtype.Int4{Int32: int32(days), Valid: true} //nolint:gosec // bounded above
	}

	changed, err := s.q.SetRetention(ctx, db.SetRetentionParams{Slug: slug, RetentionDays: span})
	if err != nil {
		return fmt.Errorf("setting the retention of %q: %w", slug, err)
	}
	if changed == 0 {
		return ErrNoTenant
	}
	return nil
}

func (s *Store) Retentions(ctx context.Context) ([]Retention, error) {
	rows, err := s.q.Retentions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the retention spans: %w", err)
	}

	spans := make([]Retention, 0, len(rows))
	for _, row := range rows {
		spans = append(spans, Retention{Slug: row.Slug, Days: int(row.RetentionDays.Int32)})
	}
	return spans, nil
}

// Purge discards what every tenant has said it no longer needs, and returns
// how much went. A tenant with no retention set keeps everything, so this does
// nothing at all until somebody asks for it.
//
// It runs one tenant at a time, like everything that crosses them, so it
// discards the same rows whether or not the role it connects as is confined by
// row level security.
func (s *Store) Purge(ctx context.Context) (int64, error) {
	rows, err := s.q.Retentions(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the retention spans: %w", err)
	}

	var discarded int64
	for _, row := range rows {
		if !row.RetentionDays.Valid {
			continue
		}
		scope := authz.WithTenant(ctx, row.ID)
		went, err := s.q.PurgeEvents(scope, db.PurgeEventsParams{
			TenantID: row.ID, Days: row.RetentionDays.Int32,
		})
		if err != nil {
			return discarded, fmt.Errorf("purging %q: %w", row.Slug, err)
		}
		discarded += went
	}
	return discarded, nil
}

// Purgeable is what Purge would discard, for saying so before doing it.
func (s *Store) Purgeable(ctx context.Context) (int64, error) {
	rows, err := s.q.Retentions(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the retention spans: %w", err)
	}

	var total int64
	for _, row := range rows {
		if !row.RetentionDays.Valid {
			continue
		}
		scope := authz.WithTenant(ctx, row.ID)
		count, err := s.q.PurgeableEvents(scope, db.PurgeableEventsParams{
			TenantID: row.ID, Days: row.RetentionDays.Int32,
		})
		if err != nil {
			return 0, fmt.Errorf("counting what %q would discard: %w", row.Slug, err)
		}
		total += count
	}
	return total, nil
}
