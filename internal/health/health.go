package health

import (
	"context"
	"net/http"
	"time"
)

type Pinger interface {
	Ping(ctx context.Context) error
}

// Ceiling on a readiness probe, so a slow database cannot hold one open.
const probeTimeout = 2 * time.Second

type Handler struct {
	database Pinger
}

func New(database Pinger) *Handler {
	return &Handler{database: database}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.live)
	mux.HandleFunc("GET /readyz", h.ready)
}

func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	writePlain(w, http.StatusOK, "ok")
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	if err := h.database.Ping(ctx); err != nil {
		writePlain(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writePlain(w, http.StatusOK, "ok")
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
