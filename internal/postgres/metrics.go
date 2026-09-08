package postgres

import (
	"context"
	"fmt"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/metrics"
)

// Snapshot reads every number a scrape reports, one tenant at a time, so it
// reports the same whether or not the role it connects as is confined by row
// level security.
func (s *Store) Snapshot(ctx context.Context) (metrics.Snapshot, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return metrics.Snapshot{}, err
	}

	snapshot := metrics.Snapshot{Tenants: float64(len(tenants))}

	operators, err := s.q.CountOperators(ctx)
	if err != nil {
		return metrics.Snapshot{}, fmt.Errorf("counting the operators: %w", err)
	}
	snapshot.Operators = float64(operators)

	for _, tenant := range tenants {
		scope := authz.WithTenant(ctx, tenant.ID)

		events, err := s.q.EventTotals(scope, tenant.ID)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("counting the events of %s: %w", tenant.Slug, err)
		}
		for _, row := range events {
			snapshot.Events = append(snapshot.Events, metrics.Measure{
				Tenant: row.Slug, Kind: row.Signature, Value: float64(row.Total),
			})
		}

		deliveries, err := s.q.DeliveryTotals(scope, tenant.ID)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("counting the deliveries of %s: %w", tenant.Slug, err)
		}
		for _, row := range deliveries {
			snapshot.Deliveries = append(snapshot.Deliveries, metrics.Measure{
				Tenant: row.Slug, Kind: row.State, Value: float64(row.Total),
			})
		}

		attempts, err := s.q.AttemptTotals(scope, tenant.ID)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("counting the attempts of %s: %w", tenant.Slug, err)
		}
		for _, row := range attempts {
			snapshot.Attempts = append(snapshot.Attempts, metrics.Measure{
				Tenant: row.Slug, Value: float64(row.Total),
			})
		}

		waiting, err := s.q.OldestPending(scope, tenant.ID)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("timing the queue of %s: %w", tenant.Slug, err)
		}
		for _, row := range waiting {
			snapshot.OldestPending = append(snapshot.OldestPending, metrics.Measure{
				Tenant: row.Slug, Value: row.Seconds,
			})
		}
	}
	return snapshot, nil
}
