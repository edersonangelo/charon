package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres/db"
)

var ErrBodyTooLarge = errors.New("body too large to record")

type Store struct {
	dsn  string
	pool *pgxpool.Pool
	q    *db.Queries
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reaching the database: %w", err)
	}

	return &Store{dsn: dsn, pool: pool, q: db.New(pool)}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging the database: %w", err)
	}
	return nil
}

// Returns only after the transaction has committed. Everything the caller
// tells the provider afterwards depends on that.
func (s *Store) Record(ctx context.Context, req inbound.Request) error {
	if len(req.Body) > math.MaxInt32 {
		return ErrBodyTooLarge
	}

	headers, err := json.Marshal(req.Headers)
	if err != nil {
		return fmt.Errorf("encoding the request headers: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	if err := q.CreateInboundEvent(ctx, db.CreateInboundEventParams{
		ID:         req.ID,
		Provider:   req.Provider,
		Path:       req.Path,
		ReceivedAt: req.ReceivedAt,
		BodySize:   int32(len(req.Body)), //nolint:gosec // bounded above
	}); err != nil {
		return fmt.Errorf("recording the inbound event: %w", err)
	}

	if err := q.CreateInboundRequest(ctx, db.CreateInboundRequestParams{
		EventID: req.ID,
		Headers: headers,
		Body:    req.Body,
	}); err != nil {
		return fmt.Errorf("recording the raw request: %w", err)
	}

	if err := q.NotifyWork(ctx); err != nil {
		return fmt.Errorf("announcing the inbound record: %w", err)
	}
	if err := q.NotifyPanel(ctx); err != nil {
		return fmt.Errorf("announcing the inbound record to the panel: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the inbound record: %w", err)
	}
	return nil
}

func (s *Store) Event(ctx context.Context, id uuid.UUID) (db.InboundEvent, error) {
	event, err := s.q.GetInboundEvent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboundEvent{}, fmt.Errorf("event %s: %w", id, err)
		}
		return db.InboundEvent{}, fmt.Errorf("reading event %s: %w", id, err)
	}
	return event, nil
}

func (s *Store) RawRequest(ctx context.Context, eventID uuid.UUID) (db.InboundRequest, error) {
	raw, err := s.q.GetInboundRequest(ctx, eventID)
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
