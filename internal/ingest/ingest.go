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
	"github.com/edersonangelo/charon/internal/provider"
)

// Store is what accepting a request needs: somewhere to record it and a way
// to resolve the tenant it was addressed to.
type Store interface {
	// Record returns the identifier the event was recorded under, which the
	// database makes: nothing can know it before the row exists.
	Record(ctx context.Context, req inbound.Request) (uuid.UUID, error)
	// Tenant reports the tenant of a slug, and whether there is one. An
	// address for a tenant that does not exist and a database that cannot
	// answer are different failures and get different answers.
	Tenant(ctx context.Context, slug string) (uuid.UUID, bool, error)
}

// Verification hands over the verifier for a tenant's provider, already
// built, so that checking a request costs no query and no construction.
type Verification interface {
	Verifier(tenant uuid.UUID, provider string) provider.Verifier
}

// A resource limit, not content validation: a body that cannot be held cannot
// be recorded, and 413 is the only honest answer.
const DefaultMaxBodyBytes int64 = 1 << 20

type Config struct {
	MaxBodyBytes int64
	Verification Verification
	Logger       *slog.Logger
}

type Handler struct {
	store   Store
	verify  Verification
	maxBody int64
	logger  *slog.Logger
}

func New(store Store, cfg Config) *Handler {
	h := &Handler{
		store:   store,
		verify:  cfg.Verification,
		maxBody: cfg.MaxBodyBytes,
		logger:  cfg.Logger,
	}
	if h.verify == nil {
		h.verify = unchecked{}
	}
	if h.maxBody <= 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	return h
}

// DefaultTenant is the tenant an address without one belongs to, so a single
// tenant deployment needs no slug in its webhook URLs.
const DefaultTenant = "default"

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/{provider}", h.Receive)
	mux.HandleFunc("POST /webhooks/{tenant}/{provider}", h.Receive)
}

type acceptedResponse struct {
	ID string `json:"id"`
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("provider")

	slug := r.PathValue("tenant")
	if slug == "" {
		slug = DefaultTenant
	}

	// The only rejection that is not about content: an address naming a tenant
	// that does not exist has nowhere to be recorded, and recording it under
	// another tenant would put one tenant's traffic inside another.
	tenant, known, err := h.store.Tenant(ctx, slug)
	if err != nil {
		h.logger.ErrorContext(ctx, "could not resolve the tenant of an inbound request",
			"tenant", slug, "provider", name, "error", err)
		http.Error(w, "could not record the request", http.StatusServiceUnavailable)
		return
	}
	if !known {
		h.logger.WarnContext(ctx, "inbound request named an unknown tenant",
			"tenant", slug, "provider", name)
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.logger.WarnContext(ctx, "inbound request exceeded the body limit",
				"provider", name, "limit_bytes", h.maxBody)
			http.Error(w, "request body exceeds the configured limit",
				http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.WarnContext(ctx, "inbound request body could not be read",
			"provider", name, "error", err)
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	// Checking a signature never rejects: the outcome is recorded on the
	// event, and a request that fails is kept so it can be looked at.
	state, verifyErr := h.verify.Verifier(tenant, name).Verify(provider.Request{
		Headers: r.Header,
		Body:    body,
		Now:     time.Now(),
	})
	if verifyErr != nil {
		h.logger.WarnContext(ctx, "inbound signature did not verify",
			"provider", name, "state", state, "reason", verifyErr)
	}

	req := inbound.Request{
		Tenant:     tenant,
		Provider:   name,
		Path:       r.URL.Path,
		ReceivedAt: time.Now().UTC(),
		Headers:    r.Header.Clone(),
		Signature:  string(state),
		Body:       body,
	}

	// 202 states that the request is committed. It is emitted below this call
	// and never before it.
	id, err := h.store.Record(ctx, req)
	if err != nil {
		h.logger.ErrorContext(ctx, "could not record an inbound request",
			"provider", name, "error", err)
		http.Error(w, "could not record the request", http.StatusServiceUnavailable)
		return
	}

	h.logger.DebugContext(ctx, "recorded an inbound request",
		"provider", name, "event_id", id, "bytes", len(body))

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

// unchecked stands in when no verification is wired, so the port has one path
// rather than a nil test on every request.
type unchecked struct{}

func (unchecked) Verifier(uuid.UUID, string) provider.Verifier { return provider.Unverified{} }
