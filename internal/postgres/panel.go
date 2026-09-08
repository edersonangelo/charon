package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var (
	ErrNoUser     = errors.New("no user with that email")
	ErrNoEvent    = errors.New("no event with that identifier")
	ErrNoRoute    = errors.New("no route with that identifier")
	ErrNoProvider = errors.New("no verification configured")
)

// CreateUser makes the person and puts them in a tenant in one go, because an
// account that belongs nowhere can reach nothing.
func (s *Store) CreateUser(
	ctx context.Context, email, passwordHash string, place console.Placement,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the operator transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	id, err := q.CreatePanelUser(ctx, db.CreatePanelUserParams{
		Email: email, PasswordHash: text(passwordHash),
	})
	if err != nil {
		return fmt.Errorf("creating user %q: %w", email, err)
	}
	if err := s.join(ctx, q, id, place); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing user %q: %w", email, err)
	}
	return nil
}

// join records that somebody belongs to a tenant, as a role or as none, which
// is the least a member holds.
func (s *Store) join(
	ctx context.Context, q *db.Queries, user uuid.UUID, place console.Placement,
) error {
	var role uuid.UUID
	if place.Role != "" {
		found, err := s.roleIDIn(ctx, place)
		if err != nil {
			return err
		}
		role = found
	}

	if err := q.JoinTenant(ctx, db.JoinTenantParams{
		UserID: user, TenantID: place.Tenant, RoleID: maybe(role),
	}); err != nil {
		return fmt.Errorf("adding %s to a tenant: %w", user, err)
	}
	return nil
}

// Join puts somebody who already exists into a tenant, or changes the role
// they hold there.
func (s *Store) Join(ctx context.Context, user uuid.UUID, place console.Placement) error {
	return s.join(ctx, s.q, user, place)
}

// Settle makes somebody belong to exactly what the identity provider named,
// and to nothing else. A value that names only a tenant leaves the role to
// whoever decides it here; one that names the role too makes the token the
// truth about that as well.
func (s *Store) Settle(ctx context.Context, user uuid.UUID, named []console.Placement) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the membership transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	tenants := make([]uuid.UUID, 0, len(named))
	for _, place := range named {
		tenants = append(tenants, place.Tenant)

		if place.Role == "" {
			if err := q.JoinFromProvider(ctx, db.JoinFromProviderParams{
				UserID: user, TenantID: place.Tenant,
			}); err != nil {
				return fmt.Errorf("adding %s to a tenant the provider names: %w", user, err)
			}
			continue
		}

		// A role the provider names that nothing here knows leaves the person
		// at the least rather than shutting them out over a name.
		role, err := s.roleIDIn(ctx, place)
		if err != nil {
			if !errors.Is(err, ErrNoRole) {
				return err
			}
			role = uuid.Nil
		}
		if err := q.JoinFromProviderAs(ctx, db.JoinFromProviderAsParams{
			UserID: user, TenantID: place.Tenant, RoleID: maybe(role),
		}); err != nil {
			return fmt.Errorf("adding %s to a tenant as the role named: %w", user, err)
		}
	}

	if err := q.LeaveTenantsNoLongerNamed(ctx, db.LeaveTenantsNoLongerNamedParams{
		UserID: user, Column2: tenants,
	}); err != nil {
		return fmt.Errorf("withdrawing what the provider no longer names: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the memberships of %s: %w", user, err)
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
	return s.user(ctx, row)
}

// Finds the operator behind an identity token: by subject when the account was
// already linked, then by email so an existing local operator keeps one
// account. Provisioning a brand new operator is only done when the deployment
// asks for it.
func (s *Store) UserBySubject(
	ctx context.Context, subject, email string, place console.Placement, provision bool,
) (console.User, error) {
	if row, err := s.q.PanelUserBySubject(ctx, text(subject)); err == nil {
		return s.user(ctx, row)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return console.User{}, fmt.Errorf("reading the user of subject %q: %w", subject, err)
	}

	row, err := s.q.LinkSubjectToUser(ctx, db.LinkSubjectToUserParams{
		Email: email, OidcSubject: text(subject),
	})
	if err == nil {
		return s.user(ctx, row)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return console.User{}, fmt.Errorf("linking subject %q to %q: %w", subject, email, err)
	}

	if !provision {
		return console.User{}, ErrNoUser
	}

	created, err := s.q.CreateSSOUser(ctx, db.CreateSSOUserParams{
		Email: email, OidcSubject: text(subject),
	})
	if err != nil {
		return console.User{}, fmt.Errorf("provisioning %q: %w", email, err)
	}
	if err := s.join(ctx, s.q, created.ID, place); err != nil {
		return console.User{}, err
	}
	return s.user(ctx, created)
}

func (s *Store) user(_ context.Context, row db.PanelUser) (console.User, error) {
	return console.User{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash.String,
		Subject:      row.OidcSubject.String,
		SystemAdmin:  row.SystemAdmin,
	}, nil
}

// Memberships is every tenant somebody belongs to, which is what the panel
// offers them to move between.
func (s *Store) Memberships(ctx context.Context, user uuid.UUID) ([]console.Membership, error) {
	rows, err := s.q.Memberships(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("reading the tenants of %s: %w", user, err)
	}

	held := make([]console.Membership, 0, len(rows))
	for _, row := range rows {
		held = append(held, console.Membership{
			Tenant: row.TenantID, Slug: row.Slug, Name: row.TenantName,
			Role: row.Role, Since: row.CreatedAt,
		})
	}
	return held, nil
}

// RoleIn is the role somebody holds in one tenant. Belonging without a role
// stated is belonging as the least, so an empty name means exactly that.
func (s *Store) RoleIn(ctx context.Context, user, tenant uuid.UUID) (string, bool, error) {
	role, err := s.q.MembershipIn(ctx, db.MembershipInParams{UserID: user, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the role of %s: %w", user, err)
	}
	return role, true, nil
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
	return console.User{
		ID:          row.ID,
		Email:       row.Email,
		SystemAdmin: row.SystemAdmin,
	}, nil
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
	ctx context.Context, tenant, deliveryID uuid.UUID, attempt, status int,
	signedWith []string, answer outbound.Answer, reason string, took time.Duration,
) error {
	ctx = authz.WithTenant(ctx, tenant)
	lastStatus := pgtype.Int4{}
	if status > 0 {
		lastStatus = pgtype.Int4{Int32: int32(status), Valid: true} //nolint:gosec // http status
	}

	if err := s.q.RecordDeliveryAttempt(ctx, db.RecordDeliveryAttemptParams{
		DeliveryID: deliveryID,
		Attempt:    int32(attempt), //nolint:gosec // bounded by max attempts
		Status:     lastStatus,
		Error:      pgtype.Text{String: reason, Valid: reason != ""},
		DurationMs: int32(took.Milliseconds()), //nolint:gosec // bounded by request timeout
		SignedWith: strings.Join(signedWith, ", "),
		Response:   answer.Body,
		// A type longer than the column is a destination misbehaving, not a
		// reason to lose the attempt.
		ResponseType:      trimTo(answer.Type, 120),
		ResponseTruncated: answer.Truncated,
	}); err != nil {
		return fmt.Errorf("recording attempt %d of delivery %s: %w", attempt, deliveryID, err)
	}
	return nil
}

func trimTo(value string, most int) string {
	if len(value) <= most {
		return value
	}
	return value[:most]
}

func (s *Store) SearchEvents(ctx context.Context, filter console.Filter) ([]console.EventSummary, error) {
	rows, err := s.q.SearchEvents(ctx, db.SearchEventsParams{
		TenantID:   s.tenantOf(ctx),
		Provider:   text(filter.Provider),
		State:      text(filter.State),
		Signature:  text(filter.Signature),
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
			Signature:  row.Signature,
			Planned:    row.Planned,
			Forced:     row.Forced,
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
	providers, err := s.q.DistinctProviders(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("listing providers: %w", err)
	}
	return providers, nil
}

func (s *Store) EventDetail(ctx context.Context, id uuid.UUID) (console.EventDetail, error) {
	row, err := s.q.EventDetail(ctx, db.EventDetailParams{ID: id, TenantID: s.tenantOf(ctx)})
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
		Signature:  row.Signature,
		Planned:    row.PlannedAt.Valid,
		Headers:    headers,
		Body:       row.Body,
		Overruled:  s.overruleOn(ctx, id),
	}, nil
}

func (s *Store) EventDeliveries(ctx context.Context, eventID uuid.UUID) ([]console.Delivery, error) {
	rows, err := s.q.EventDeliveries(ctx, db.EventDeliveriesParams{EventID: eventID, TenantID: s.tenantOf(ctx)})
	if err != nil {
		return nil, fmt.Errorf("reading the deliveries of %s: %w", eventID, err)
	}

	deliveries := make([]console.Delivery, 0, len(rows))
	for _, row := range rows {
		history, attemptErr := s.q.DeliveryAttempts(ctx, db.DeliveryAttemptsParams{DeliveryID: row.ID, TenantID: s.tenantOf(ctx)})
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
				SignedWith:  item.SignedWith,
				Forced:      item.Forced,
				Answer:      item.Response,
				AnswerType:  item.ResponseType,

				AnswerTruncated: item.ResponseTruncated,
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
		return q.ReplayDelivery(ctx, db.ReplayDeliveryParams{ID: id, TenantID: s.tenantOf(ctx)})
	})
}

// Replays every delivery of an event, and unplans the event so a route added
// since is picked up as well.
func (s *Store) ReplayEvent(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return s.replay(ctx, func(q *db.Queries) (int64, error) {
		affected, err := q.ReplayEvent(ctx, db.ReplayEventParams{EventID: eventID, TenantID: s.tenantOf(ctx)})
		if err != nil {
			return 0, err
		}
		if err := q.UnplanEvent(ctx, db.UnplanEventParams{ID: eventID, TenantID: s.tenantOf(ctx)}); err != nil {
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

// A role can be absent — a membership without one is a membership as the
// least — so it crosses the query layer as something that may not be there.
func maybe(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil}
}

func text(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func stamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: !value.IsZero()}
}

func (s *Store) UnroutedProviders(ctx context.Context) ([]console.UnroutedProvider, error) {
	rows, err := s.q.UnroutedProviders(ctx, s.tenantOf(ctx))
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
	rows, err := s.q.DetailedRoutes(ctx, s.tenantOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}

	routes := make([]console.RouteRow, 0, len(rows))
	for _, row := range rows {
		signing, signErr := s.SigningFor(ctx, row.DestinationID)
		if signErr != nil {
			return nil, signErr
		}
		routes = append(routes, console.RouteRow{
			ID:            row.ID,
			Provider:      row.Provider,
			DestinationID: row.DestinationID,
			Destination:   row.Name,
			Transport:     row.Transport,
			URL:           row.Url,
			Enabled:       row.Enabled,
			Deliveries:    row.Deliveries,
			Signing:       signing,
		})
	}
	return routes, nil
}

func (s *Store) SaveRoute(ctx context.Context, routeID uuid.UUID, url string, enabled bool) error {
	route, err := s.q.RouteByID(ctx, db.RouteByIDParams{ID: routeID, TenantID: s.tenantOf(ctx)})
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
		ID: route.DestinationID, TenantID: s.tenantOf(ctx), Url: url, Enabled: enabled,
	}); err != nil {
		return fmt.Errorf("updating destination %s: %w", route.Name, err)
	}

	// A destination coming back on has the same gap a new route has: events
	// planned while it was off carry no delivery for it.
	if enabled && !route.Enabled {
		if _, err := q.UnplanProvider(ctx, db.UnplanProviderParams{Provider: route.Provider, TenantID: s.tenantOf(ctx)}); err != nil {
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
	if err := s.q.DeleteRoute(ctx, db.DeleteRouteParams{ID: routeID, TenantID: s.tenantOf(ctx)}); err != nil {
		return fmt.Errorf("deleting route %s: %w", routeID, err)
	}
	s.announceToPanel(ctx)
	return nil
}

// Replays everything already sent to one destination. Changing where a route
// points does not resend what was delivered to the old address; this is how an
// operator asks for that on purpose.
func (s *Store) ReplayDestination(ctx context.Context, routeID uuid.UUID) (int64, error) {
	route, err := s.q.RouteByID(ctx, db.RouteByIDParams{ID: routeID, TenantID: s.tenantOf(ctx)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNoRoute
		}
		return 0, fmt.Errorf("reading route %s: %w", routeID, err)
	}

	return s.replay(ctx, func(q *db.Queries) (int64, error) {
		return q.ReplayDestination(ctx, db.ReplayDestinationParams{DestinationID: route.DestinationID, TenantID: s.tenantOf(ctx)})
	})
}

// VerifyPassword is the credentials port the local sign-in needs. It reports
// the stored address so that a case difference in what was typed does not end
// up on the session.
func (s *Store) VerifyPassword(ctx context.Context, email, password string) (string, error) {
	user, err := s.UserByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	if err := console.CheckPassword(user.PasswordHash, password); err != nil {
		return "", err
	}
	return user.Email, nil
}
