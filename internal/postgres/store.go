package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres/db"
	"github.com/edersonangelo/charon/internal/provider"
)

var ErrBodyTooLarge = errors.New("body too large to record")

type Store struct {
	dsn  string
	pool *pgxpool.Pool
	q    *db.Queries

	tenantMu     sync.RWMutex
	tenantBySlug map[string]uuid.UUID
	pinned       sync.Map

	// Which tenant the next round of planning or claiming starts at, so a busy
	// one does not take the whole batch every time.
	turns atomic.Uint64
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}

	store := &Store{dsn: dsn, tenantBySlug: map[string]uuid.UUID{}}

	// Row level security reads the tenant from the session, so every
	// connection carries the tenant of whoever asked for it before a query
	// runs on it.
	cfg.PrepareConn = store.pin
	cfg.BeforeClose = func(conn *pgx.Conn) { store.pinned.Delete(conn) }

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reaching the database: %w", err)
	}

	store.pool = pool
	store.q = db.New(pool)
	return store, nil
}

// pin sets the tenant of the acquiring context on the connection, skipping the
// round trip when the connection already carries it.
func (s *Store) pin(ctx context.Context, conn *pgx.Conn) (bool, error) {
	wanted := ""
	if tenant, scoped := authz.TenantFrom(ctx); scoped {
		wanted = tenant.String()
	}

	if current, seen := s.pinned.Load(conn); seen && current == wanted {
		return true, nil
	}
	if _, err := conn.Exec(ctx, "select set_config('charon.tenant', $1, false)", wanted); err != nil {
		s.pinned.Delete(conn)
		return false, fmt.Errorf("setting the tenant of a connection: %w", err)
	}
	s.pinned.Store(conn, wanted)
	return true, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging the database: %w", err)
	}
	return nil
}

// Returns the identifier the database gave the event, and only after the
// transaction has committed. Everything the caller tells the provider
// afterwards depends on that.
func (s *Store) Record(ctx context.Context, req inbound.Request) (uuid.UUID, error) {
	if len(req.Body) > math.MaxInt32 {
		return uuid.Nil, ErrBodyTooLarge
	}

	headers, err := json.Marshal(req.Headers)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encoding the request headers: %w", err)
	}

	// The request says which tenant it arrived for, so the connection is
	// scoped to it here: this is the one place where the tenant comes from
	// what is being written rather than from who is asking.
	tenant := req.Tenant
	if tenant == uuid.Nil {
		tenant = s.tenantOf(ctx)
	}
	ctx = authz.WithTenant(ctx, tenant)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("beginning the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	id, err := q.CreateInboundEvent(ctx, db.CreateInboundEventParams{
		TenantID:   tenant,
		Provider:   req.Provider,
		Path:       req.Path,
		ReceivedAt: req.ReceivedAt,
		BodySize:   int32(len(req.Body)), //nolint:gosec // bounded above
		Signature:  signature(req.Signature),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("recording the inbound event: %w", err)
	}

	if err := q.CreateInboundRequest(ctx, db.CreateInboundRequestParams{
		EventID:  id,
		TenantID: tenant,
		Headers:  headers,
		Body:     req.Body,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("recording the raw request: %w", err)
	}

	if err := q.NotifyWork(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("announcing the inbound record: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("announcing the inbound record to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("committing the inbound record: %w", err)
	}
	return id, nil
}

// An empty state means nothing checked the request, which is what a provider
// with no verifier configured produces.
func signature(state string) string {
	if state == "" {
		return string(provider.Unchecked)
	}
	return state
}

func (s *Store) Event(ctx context.Context, id uuid.UUID) (db.InboundEvent, error) {
	event, err := s.q.GetInboundEvent(ctx, db.GetInboundEventParams{ID: id, TenantID: s.tenantOf(ctx)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboundEvent{}, fmt.Errorf("event %s: %w", id, err)
		}
		return db.InboundEvent{}, fmt.Errorf("reading event %s: %w", id, err)
	}
	return event, nil
}

func (s *Store) RawRequest(ctx context.Context, eventID uuid.UUID) (db.InboundRequest, error) {
	raw, err := s.q.GetInboundRequest(ctx, db.GetInboundRequestParams{EventID: eventID, TenantID: s.tenantOf(ctx)})
	if err != nil {
		return db.InboundRequest{}, fmt.Errorf("reading the raw request of %s: %w", eventID, err)
	}
	return raw, nil
}

func (s *Store) CountEvents(ctx context.Context) (int64, error) {
	n, err := s.q.CountInboundEvents(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting events: %w", err)
	}
	return n, nil
}
