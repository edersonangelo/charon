package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var (
	ErrNoUser  = errors.New("no user with that email")
	ErrNoEvent = errors.New("no event with that identifier")
	ErrNoRoute = errors.New("no route with that identifier")
)

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating a user id: %w", err)
	}
	if err := s.q.CreatePanelUser(ctx, db.CreatePanelUserParams{
		ID: id, Email: email, PasswordHash: text(passwordHash),
	}); err != nil {
		return fmt.Errorf("creating user %q: %w", email, err)
	}
	return nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (console.User, error) {
	row, err := s.q.PanelUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return console.User{}, ErrNoUser
		}
		return console.User{}, fmt.Errorf("reading user %q: %w", email, err)
	}
	return user(row), nil
}

// Finds the operator behind an identity token: by subject when the account was
// already linked, then by email so an existing local operator keeps one
// account. Provisioning a brand new operator is only done when the deployment
// asks for it.
func (s *Store) UserBySubject(
	ctx context.Context, subject, email string, provision bool,
) (console.User, error) {
	if row, err := s.q.PanelUserBySubject(ctx, text(subject)); err == nil {
		return user(row), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return console.User{}, fmt.Errorf("reading the user of subject %q: %w", subject, err)
	}

	row, err := s.q.LinkSubjectToUser(ctx, db.LinkSubjectToUserParams{
		Email: email, OidcSubject: text(subject),
	})
	if err == nil {
		return user(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return console.User{}, fmt.Errorf("linking subject %q to %q: %w", subject, email, err)
	}

	if !provision {
		return console.User{}, ErrNoUser
	}

	id, err := uuid.NewV7()
	if err != nil {
		return console.User{}, fmt.Errorf("generating a user id: %w", err)
	}
	created, err := s.q.CreateSSOUser(ctx, db.CreateSSOUserParams{
		ID: id, Email: email, OidcSubject: text(subject),
	})
	if err != nil {
		return console.User{}, fmt.Errorf("provisioning %q: %w", email, err)
	}
	return user(created), nil
}

func user(row db.PanelUser) console.User {
	return console.User{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash.String,
		Subject:      row.OidcSubject.String,
	}
}

func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	n, err := s.q.CountPanelUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return n, nil
}

func (s *Store) CreateSession(ctx context.Context, digest []byte, userID uuid.UUID, expires time.Time) error {
	if err := s.q.CreatePanelSession(ctx, db.CreatePanelSessionParams{
		Token: digest, UserID: userID, ExpiresAt: expires,
	}); err != nil {
		return fmt.Errorf("creating a session: %w", err)
	}
	return nil
}

func (s *Store) SessionUser(ctx context.Context, digest []byte) (console.User, error) {
	row, err := s.q.PanelSessionUser(ctx, digest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return console.User{}, ErrNoUser
		}
		return console.User{}, fmt.Errorf("reading the session: %w", err)
	}
	return console.User{ID: row.ID, Email: row.Email}, nil
}

func (s *Store) DeleteSession(ctx context.Context, digest []byte) error {
	if err := s.q.DeletePanelSession(ctx, digest); err != nil {
		return fmt.Errorf("deleting the session: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	if err := s.q.DeleteExpiredPanelSessions(ctx); err != nil {
		return fmt.Errorf("deleting expired sessions: %w", err)
	}
	return nil
}

func (s *Store) RecordAttempt(
	ctx context.Context, deliveryID uuid.UUID, attempt, status int, reason string, took time.Duration,
) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating an attempt id: %w", err)
	}

	lastStatus := pgtype.Int4{}
	if status > 0 {
		lastStatus = pgtype.Int4{Int32: int32(status), Valid: true} //nolint:gosec // http status
	}

	if err := s.q.RecordDeliveryAttempt(ctx, db.RecordDeliveryAttemptParams{
		ID:         id,
		DeliveryID: deliveryID,
		Attempt:    int32(attempt), //nolint:gosec // bounded by max attempts
		Status:     lastStatus,
		Error:      pgtype.Text{String: reason, Valid: reason != ""},
		DurationMs: int32(took.Milliseconds()), //nolint:gosec // bounded by request timeout
	}); err != nil {
		return fmt.Errorf("recording attempt %d of delivery %s: %w", attempt, deliveryID, err)
	}
	return nil
}

func (s *Store) SearchEvents(ctx context.Context, filter console.Filter) ([]console.EventSummary, error) {
	rows, err := s.q.SearchEvents(ctx, db.SearchEventsParams{
		Provider:   text(filter.Provider),
		State:      text(filter.State),
		Search:     text(filter.Search),
		Since:      stamp(filter.Since),
		Until:      stamp(filter.Until),
		PageSize:   int32(filter.Size()),   //nolint:gosec // capped at 200
		PageOffset: int32(filter.Offset()), //nolint:gosec // derived from page size
	})
	if err != nil {
		return nil, fmt.Errorf("searching events: %w", err)
	}

	events := make([]console.EventSummary, 0, len(rows))
	for _, row := range rows {
		events = append(events, console.EventSummary{
			ID:         row.ID,
			Provider:   row.Provider,
			Path:       row.Path,
			ReceivedAt: row.ReceivedAt,
			BodySize:   row.BodySize,
			Planned:    row.Planned,
			Deliveries: row.Deliveries,
			Delivered:  row.Delivered,
			Dead:       row.Dead,
			Pending:    row.Pending,
			Unrouted:   row.Unrouted,
			Attempts:   row.Attempts,
		})
	}
	return events, nil
}

func (s *Store) Providers(ctx context.Context) ([]string, error) {
	providers, err := s.q.DistinctProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing providers: %w", err)
	}
	return providers, nil
}

func (s *Store) EventDetail(ctx context.Context, id uuid.UUID) (console.EventDetail, error) {
	row, err := s.q.EventDetail(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return console.EventDetail{}, ErrNoEvent
		}
		return console.EventDetail{}, fmt.Errorf("reading event %s: %w", id, err)
	}

	var headers map[string][]string
	if err := json.Unmarshal(row.Headers, &headers); err != nil {
		return console.EventDetail{}, fmt.Errorf("decoding the headers of %s: %w", id, err)
	}

	return console.EventDetail{
		ID:         row.ID,
		Provider:   row.Provider,
		Path:       row.Path,
		ReceivedAt: row.ReceivedAt,
		BodySize:   row.BodySize,
		Planned:    row.PlannedAt.Valid,
		Headers:    headers,
		Body:       row.Body,
	}, nil
}

func (s *Store) EventDeliveries(ctx context.Context, eventID uuid.UUID) ([]console.Delivery, error) {
	rows, err := s.q.EventDeliveries(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("reading the deliveries of %s: %w", eventID, err)
	}

	deliveries := make([]console.Delivery, 0, len(rows))
	for _, row := range rows {
		history, attemptErr := s.q.DeliveryAttempts(ctx, row.ID)
		if attemptErr != nil {
			return nil, fmt.Errorf("reading the attempts of %s: %w", row.ID, attemptErr)
		}

		var rounds []console.Round
		for _, item := range history {
			attempt := console.Attempt{
				Round:       item.Round,
				Attempt:     item.Attempt,
				AttemptedAt: item.AttemptedAt,
				Status:      item.Status.Int32,
				Error:       item.Error.String,
				DurationMs:  item.DurationMs,
			}
			if len(rounds) == 0 || rounds[len(rounds)-1].Number != item.Round {
				rounds = append(rounds, console.Round{Number: item.Round})
			}
			last := &rounds[len(rounds)-1]
			last.Attempts = append(last.Attempts, attempt)
		}

		deliveries = append(deliveries, console.Delivery{
			ID:            row.ID,
			Destination:   row.Destination,
			URL:           row.Url,
			State:         row.State,
			Attempts:      row.Attempts,
			NextAttemptAt: row.NextAttemptAt,
			LastStatus:    row.LastStatus.Int32,
			LastError:     row.LastError.String,
			DeliveredAt:   row.DeliveredAt.Time,
			ReplayCount:   row.ReplayCount,
			ReplayedAt:    row.ReplayedAt.Time,
			Routed:        row.Routed,
			Rounds:        rounds,
		})
	}
	return deliveries, nil
}

// Replay resets a delivery to pending and announces the work. Attempt history
// survives in delivery_attempt, so an operator can still see what was tried.
func (s *Store) ReplayDelivery(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.replay(ctx, func(q *db.Queries) (int64, error) {
		return q.ReplayDelivery(ctx, id)
	})
}

// Replays every delivery of an event, and unplans the event so a route added
// since is picked up as well.
func (s *Store) ReplayEvent(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return s.replay(ctx, func(q *db.Queries) (int64, error) {
		affected, err := q.ReplayEvent(ctx, eventID)
		if err != nil {
			return 0, err
		}
		if err := q.UnplanEvent(ctx, eventID); err != nil {
			return 0, err
		}
		return affected, nil
	})
}

func (s *Store) replay(ctx context.Context, apply func(*db.Queries) (int64, error)) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning the replay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	affected, err := apply(q)
	if err != nil {
		return 0, fmt.Errorf("replaying: %w", err)
	}

	if err := q.NotifyWork(ctx); err != nil {
		return 0, fmt.Errorf("announcing the replay: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return 0, fmt.Errorf("announcing the replay to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the replay: %w", err)
	}
	return affected, nil
}

func text(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func stamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: !value.IsZero()}
}

func (s *Store) UnroutedProviders(ctx context.Context) ([]console.UnroutedProvider, error) {
	rows, err := s.q.UnroutedProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing providers without a route: %w", err)
	}

	providers := make([]console.UnroutedProvider, 0, len(rows))
	for _, row := range rows {
		providers = append(providers, console.UnroutedProvider{
			Provider:     row.Provider,
			Events:       row.Events,
			LastReceived: row.LastReceived,
		})
	}
	return providers, nil
}

func (s *Store) DetailedRoutes(ctx context.Context) ([]console.RouteRow, error) {
	rows, err := s.q.DetailedRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}

	routes := make([]console.RouteRow, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, console.RouteRow{
			ID:            row.ID,
			Provider:      row.Provider,
			DestinationID: row.DestinationID,
			Destination:   row.Name,
			URL:           row.Url,
			Enabled:       row.Enabled,
			Deliveries:    row.Deliveries,
		})
	}
	return routes, nil
}

func (s *Store) SaveRoute(ctx context.Context, routeID uuid.UUID, url string, enabled bool) error {
	route, err := s.q.RouteByID(ctx, routeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRoute
		}
		return fmt.Errorf("reading route %s: %w", routeID, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the route transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	if err := q.UpdateDestination(ctx, db.UpdateDestinationParams{
		ID: route.DestinationID, Url: url, Enabled: enabled,
	}); err != nil {
		return fmt.Errorf("updating destination %s: %w", route.Name, err)
	}

	// A destination coming back on has the same gap a new route has: events
	// planned while it was off carry no delivery for it.
	if enabled && !route.Enabled {
		if _, err := q.UnplanProvider(ctx, route.Provider); err != nil {
			return fmt.Errorf("reopening planning for %q: %w", route.Provider, err)
		}
	}

	if err := q.NotifyPanel(ctx); err != nil {
		return fmt.Errorf("announcing the change to the panel: %w", err)
	}
	if enabled {
		if err := q.NotifyWork(ctx); err != nil {
			return fmt.Errorf("announcing the change: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the route change: %w", err)
	}
	return nil
}

// Removes the route so nothing new is planned for it. The destination and
// every delivery already recorded against it are left alone: dropping them
// would erase history an operator may still need.
func (s *Store) DeleteRoute(ctx context.Context, routeID uuid.UUID) error {
	if err := s.q.DeleteRoute(ctx, routeID); err != nil {
		return fmt.Errorf("deleting route %s: %w", routeID, err)
	}
	s.announceToPanel(ctx)
	return nil
}

// Replays everything already sent to one destination. Changing where a route
// points does not resend what was delivered to the old address; this is how an
// operator asks for that on purpose.
func (s *Store) ReplayDestination(ctx context.Context, routeID uuid.UUID) (int64, error) {
	route, err := s.q.RouteByID(ctx, routeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNoRoute
		}
		return 0, fmt.Errorf("reading route %s: %w", routeID, err)
	}

	return s.replay(ctx, func(q *db.Queries) (int64, error) {
		return q.ReplayDestination(ctx, route.DestinationID)
	})
}
