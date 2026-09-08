package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var (
	ErrNoReason       = errors.New("say why this is being delivered anyway, in a sentence")
	ErrNothingToForce = errors.New(
		"none of those events are waiting on a failed signature")
)

// MinimumReason is the shortest thing that counts as an explanation. It is
// enforced by the database as well, because a reason nobody can read is the
// same as no reason.
const MinimumReason = 10

// Force delivers events whose signature failed, under one reason that covers
// all of them, and returns how many it applied to.
//
// The verdict is not rewritten. An event delivered this way is still invalid
// or missing and says so for ever; what is added is who let it through. An
// event already overruled is left with the first decision, because that is the
// one that let it out.
func (s *Store) Force(
	ctx context.Context, operator uuid.UUID, reason string, events []uuid.UUID,
) (int64, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < MinimumReason {
		return 0, ErrNoReason
	}
	if len(events) == 0 {
		return 0, ErrNothingToForce
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning the override transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	override, err := q.RecordOverride(ctx, db.RecordOverrideParams{
		TenantID: s.tenantOf(ctx), Reason: reason, DecidedBy: operator,
	})
	if err != nil {
		return 0, fmt.Errorf("recording the reason: %w", err)
	}

	forced, err := q.OverrideEvents(ctx, db.OverrideEventsParams{
		OverrideID: maybe(override), TenantID: s.tenantOf(ctx), Ids: events,
	})
	if err != nil {
		return 0, fmt.Errorf("overruling the refusal: %w", err)
	}
	if forced == 0 {
		return 0, ErrNothingToForce
	}

	// The dispatcher sleeps until the next delivery is due, so it is told
	// rather than left to find these on its next round.
	if err := q.NotifyWork(ctx); err != nil {
		return 0, fmt.Errorf("announcing the events to deliver: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return 0, fmt.Errorf("announcing the override to the panel: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the override: %w", err)
	}
	return forced, nil
}

// overruleOn is what let an event through, for the page that has to explain a
// delivery of something that did not verify. Absence is the ordinary case, not
// an error.
func (s *Store) overruleOn(ctx context.Context, id uuid.UUID) *console.Overruled {
	row, err := s.q.OverruleOnEvent(ctx, db.OverruleOnEventParams{
		ID: id, TenantID: s.tenantOf(ctx),
	})
	if err != nil {
		return nil
	}
	return &console.Overruled{
		Reason: row.Reason, DecidedBy: row.DecidedBy, DecidedAt: row.DecidedAt,
	}
}
