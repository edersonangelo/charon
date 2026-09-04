// Package ingest accepts inbound webhooks.
//
// Rule: nothing about a payload's content may cause a rejection. A malformed
// body, an unknown provider and an unmapped event type are all recorded.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/inbound"
)

type Recorder interface {
	Record(ctx context.Context, req inbound.Request) error
}

// A resource limit, not content validation: a body that cannot be held cannot
// be recorded, and 413 is the only honest answer.
const DefaultMaxBodyBytes int64 = 1 << 20

type Config struct {
	MaxBodyBytes int64
	Logger       *slog.Logger
}

type Handler struct {
	recorder Recorder
	maxBody  int64
	logger   *slog.Logger
}

func New(recorder Recorder, cfg Config) *Handler {
	h := &Handler{
		recorder: recorder,
		maxBody:  cfg.MaxBodyBytes,
		logger:   cfg.Logger,
	}
	if h.maxBody <= 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	return h
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/{provider}", h.Receive)
}

type acceptedResponse struct {
	ID string `json:"id"`
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := r.PathValue("provider")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.logger.WarnContext(ctx, "inbound request exceeded the body limit",
				"provider", provider, "limit_bytes", h.maxBody)
			http.Error(w, "request body exceeds the configured limit",
				http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.WarnContext(ctx, "inbound request body could not be read",
			"provider", provider, "error", err)
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		h.logger.ErrorContext(ctx, "could not generate an event id", "error", err)
		http.Error(w, "could not record the request", http.StatusServiceUnavailable)
		return
	}

	req := inbound.Request{
		ID:         id,
		Provider:   provider,
		Path:       r.URL.Path,
		ReceivedAt: time.Now().UTC(),
		Headers:    r.Header.Clone(),
		Body:       body,
	}

	// 202 states that the request is committed. It is emitted below this call
	// and never before it.
	if err := h.recorder.Record(ctx, req); err != nil {
		h.logger.ErrorContext(ctx, "could not record an inbound request",
			"provider", provider, "event_id", id, "error", err)
		http.Error(w, "could not record the request", http.StatusServiceUnavailable)
		return
	}

	h.logger.DebugContext(ctx, "recorded an inbound request",
		"provider", provider, "event_id", id, "bytes", len(body))

	response, err := json.Marshal(acceptedResponse{ID: id.String()})
	if err != nil {
		h.logger.ErrorContext(ctx, "could not encode the response",
			"event_id", id, "error", err)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write(response); err != nil {
		h.logger.WarnContext(ctx, "could not write the response body",
			"event_id", id, "error", err)
	}
}
