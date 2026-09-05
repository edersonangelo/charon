package postgres_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
)

func open(t *testing.T) (*postgres.Store, string) {
	t.Helper()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return store, dsn
}

func request(body []byte) inbound.Request {
	return inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Stripe-Signature": {"t=1,v1=deadbeef"}},
		Body:       body,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	applied, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second migrate applied %v, want nothing", applied)
	}
}

func TestRecordStoresTheRequestVerbatim(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	body := []byte("{\n  \"z\": 1,\t\"a\": [2,3]  }\n\x00\xff")
	req := request(body)

	id, err := store.Record(ctx, req)
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	event, err := store.Event(ctx, id)
	if err != nil {
		t.Fatalf("reading the event: %v", err)
	}
	if event.Provider != req.Provider {
		t.Errorf("provider = %q, want %q", event.Provider, req.Provider)
	}
	if int(event.BodySize) != len(body) {
		t.Errorf("body size = %d, want %d", event.BodySize, len(body))
	}
	raw, err := store.RawRequest(ctx, id)
	if err != nil {
		t.Fatalf("reading the raw request: %v", err)
	}
	if !bytes.Equal(raw.Body, body) {
		t.Errorf("stored body = %q, want %q", raw.Body, body)
	}
	if !bytes.Contains(raw.Headers, []byte("Stripe-Signature")) {
		t.Errorf("stored headers = %s, want them to carry Stripe-Signature", raw.Headers)
	}
}

func TestRecordIsAtomic(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting directly: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, `drop table inbound_request`); err != nil {
		t.Fatalf("dropping inbound_request: %v", err)
	}

	if _, err := store.Record(ctx, request([]byte(`{"a":1}`))); err == nil {
		t.Fatal("Record() succeeded with no inbound_request table, want an error")
	}

	var events int
	if err := conn.QueryRow(ctx, `select count(*) from inbound_event`).Scan(&events); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 0 {
		t.Errorf("inbound_event has %d rows, want 0 after the rollback", events)
	}
}

func TestRecordFailsWhenTheDatabaseIsGone(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	store.Close()

	if _, err := store.Record(context.Background(), request([]byte(`{}`))); err == nil {
		t.Fatal("Record() succeeded against a closed pool, want an error")
	}
}

func TestPingReportsAClosedPool(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() on an open pool: %v", err)
	}

	store.Close()
	if err := store.Ping(context.Background()); err == nil {
		t.Error("Ping() succeeded against a closed pool, want an error")
	}
}
