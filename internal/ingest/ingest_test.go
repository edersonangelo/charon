package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/ingest"
)

type fakeRecorder struct {
	mu   sync.Mutex
	got  []inbound.Request
	fail error
}

func (f *fakeRecorder) Record(_ context.Context, req inbound.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, req)
	return nil
}

func (f *fakeRecorder) recorded() []inbound.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

func serve(t *testing.T, r ingest.Recorder, cfg ingest.Config) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	ingest.New(r, cfg).Register(mux)
	return mux
}

func post(t *testing.T, mux *http.ServeMux, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestReceiveAcceptsAnyContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		body string
	}{
		{"well formed json", "/webhooks/stripe", `{"id":"evt_1","type":"charge.succeeded"}`},
		{"malformed json", "/webhooks/stripe", `{"id":"evt_1",`},
		{"not json at all", "/webhooks/stripe", `<xml><nope/></xml>`},
		{"empty body", "/webhooks/stripe", ``},
		{"binary", "/webhooks/stripe", "\x00\x01\x02\xff"},
		{"unknown provider", "/webhooks/never-heard-of-it", `{"a":1}`},
		{"unmapped event", "/webhooks/stripe", `{"type":"something.we.do.not.handle"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeRecorder{}
			rec := post(t, serve(t, fake, ingest.Config{}), tt.path, []byte(tt.body))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d (body %q)",
					rec.Code, http.StatusAccepted, rec.Body.String())
			}
			if got := len(fake.recorded()); got != 1 {
				t.Fatalf("recorded %d requests, want 1", got)
			}
		})
	}
}

func TestReceiveStoresTheBodyVerbatim(t *testing.T) {
	t.Parallel()

	body := []byte("{\n  \"z\": 1,\n  \"a\": [2,3]  }\t\n")

	fake := &fakeRecorder{}
	rec := post(t, serve(t, fake, ingest.Config{}), "/webhooks/asaas", body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	recorded := fake.recorded()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(recorded))
	}
	if !bytes.Equal(recorded[0].Body, body) {
		t.Errorf("stored body = %q, want %q", recorded[0].Body, body)
	}
	if recorded[0].Provider != "asaas" {
		t.Errorf("provider = %q, want %q", recorded[0].Provider, "asaas")
	}
	if recorded[0].Path != "/webhooks/asaas" {
		t.Errorf("request path = %q, want %q", recorded[0].Path, "/webhooks/asaas")
	}
}

func TestReceiveDoesNotAcknowledgeWhenTheStoreFails(t *testing.T) {
	t.Parallel()

	fake := &fakeRecorder{fail: errors.New("database is on fire")}
	rec := post(t, serve(t, fake, ingest.Config{}), "/webhooks/stripe", []byte(`{"a":1}`))

	if rec.Code/100 == 2 {
		t.Fatalf("status = %d, want a non-2xx when nothing was stored", rec.Code)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := len(fake.recorded()); got != 0 {
		t.Errorf("recorded %d requests, want 0", got)
	}
}

func TestReceiveRefusesABodyOverTheLimit(t *testing.T) {
	t.Parallel()

	fake := &fakeRecorder{}
	mux := serve(t, fake, ingest.Config{MaxBodyBytes: 16})

	rec := post(t, mux, "/webhooks/stripe", []byte(strings.Repeat("x", 17)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if got := len(fake.recorded()); got != 0 {
		t.Errorf("recorded %d requests, want 0", got)
	}
}

func TestReceiveReturnsTheEventIdentifier(t *testing.T) {
	t.Parallel()

	fake := &fakeRecorder{}
	rec := post(t, serve(t, fake, ingest.Config{}), "/webhooks/stripe", []byte(`{}`))

	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (body %q)", err, rec.Body.String())
	}

	recorded := fake.recorded()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(recorded))
	}
	if body.ID != recorded[0].ID.String() {
		t.Errorf("response id = %q, want the stored id %q", body.ID, recorded[0].ID)
	}
}

func TestReceiveIsPostOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeRecorder{}
	mux := serve(t, fake, ingest.Config{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/webhooks/stripe", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
