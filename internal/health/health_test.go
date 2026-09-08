package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edersonangelo/charon/internal/health"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		pingErr    error
		wantStatus int
	}{
		{"liveness ignores the database", "/healthz", errors.New("down"), http.StatusOK},
		{"readiness passes when the database answers", "/readyz", nil, http.StatusOK},
		{"readiness fails when the database does not", "/readyz", errors.New("down"), http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			health.New(fakePinger{err: tt.pingErr}).Register(mux)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}
