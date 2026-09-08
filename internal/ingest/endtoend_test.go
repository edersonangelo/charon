package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/ingest"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
)

// The second store outlives closing the first, so the tables can still be read
// after the port has lost its database.
func wire(t *testing.T) (*http.ServeMux, *postgres.Store, *postgres.Store) {
	t.Helper()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	serving, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the serving store: %v", err)
	}
	t.Cleanup(serving.Close)

	if _, err := serving.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	observing, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the observing store: %v", err)
	}
	t.Cleanup(observing.Close)

	mux := http.NewServeMux()
	ingest.New(serving, ingest.Config{}).Register(mux)

	return mux, serving, observing
}

func TestAcknowledgedRequestIsRecorded(t *testing.T) {
	t.Parallel()

	mux, _, observing := wire(t)
	ctx := context.Background()

	body := []byte(`{"id":"evt_1","type":"charge.succeeded","raw":"é"}`)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body)
	}

	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	id, err := uuid.Parse(accepted.ID)
	if err != nil {
		t.Fatalf("parsing the returned id %q: %v", accepted.ID, err)
	}

	raw, err := observing.RawRequest(ctx, id)
	if err != nil {
		t.Fatalf("reading back the raw request: %v", err)
	}
	if !bytes.Equal(raw.Body, body) {
		t.Errorf("recorded body = %q, want %q", raw.Body, body)
	}
}

func TestNoAcknowledgementWhenTheDatabaseIsUnreachable(t *testing.T) {
	t.Parallel()

	mux, serving, observing := wire(t)
	ctx := context.Background()

	serving.Close()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/stripe",
		bytes.NewReader([]byte(`{"id":"evt_lost"}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code/100 == 2 {
		t.Fatalf("status = %d, want a non-2xx while the database is unreachable", rec.Code)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	events, err := observing.CountEvents(ctx)
	if err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 0 {
		t.Errorf("recorded %d events, want 0", events)
	}
}
