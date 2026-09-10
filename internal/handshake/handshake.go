// Package handshake answers the GET some providers send before they will
// accept a callback address.
//
// It shares an address with the inbound port and inverts its rule: a webhook
// is never rejected for what it says, and a handshake exists to be rejected.
// An address a stranger can confirm is an address anyone can point at their
// own provider, so a wrong token is refused and nothing is recorded either
// way.
package handshake

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/ingest"
)

// Declared here, by the side that consumes them, so this package depends on
// no storage.
type (
	Tenants interface {
		Tenant(ctx context.Context, slug string) (uuid.UUID, bool, error)
	}

	Tokens interface {
		// VerifyToken is the token a provider must offer, and whether one was
		// ever asked for. A provider that asked and whose token cannot be read
		// here answers ("", true).
		VerifyToken(tenant uuid.UUID, provider string) (token string, configured bool)
	}
)

// The names Meta sends. Constants rather than settings because one provider is
// not a pattern: the second one that asks for this under different names is
// when they become parameters.
const (
	challengeParam   = "hub.challenge"
	verifyTokenParam = "hub.verify_token"
)

// A ceiling, not a defence: the url bounds this already, and Meta's challenge
// is ten digits. A value a hundred times longer is not a challenge, and
// echoing it would be answering something else.
const maxChallengeBytes = 1024

type Config struct {
	Tokens Tokens
	Logger *slog.Logger
}

type Handler struct {
	tenants Tenants
	tokens  Tokens
	logger  *slog.Logger
}

func New(tenants Tenants, cfg Config) *Handler {
	h := &Handler{tenants: tenants, tokens: cfg.Tokens, logger: cfg.Logger}
	if h.tokens == nil {
		h.tokens = nothingConfigured{}
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	return h
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /webhooks/{provider}", h.Answer)
	mux.HandleFunc("GET /webhooks/{tenant}/{provider}", h.Answer)
}

func (h *Handler) Answer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("provider")

	slug := r.PathValue("tenant")
	if slug == "" {
		slug = ingest.DefaultTenant
	}

	// The tenant comes first because the expected token is a fact about one:
	// there is nothing to compare against until it is known.
	tenant, known, err := h.tenants.Tenant(ctx, slug)
	if err != nil {
		h.logger.ErrorContext(ctx, "could not resolve the tenant of a handshake",
			"tenant", slug, "provider", name, "error", err)
		http.Error(w, "could not answer the handshake", http.StatusServiceUnavailable)
		return
	}
	if !known {
		h.logger.WarnContext(ctx, "a handshake named an unknown tenant",
			"tenant", slug, "provider", name)
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}

	// Both empty cases are refused ahead of the comparison below, because
	// ConstantTimeCompare of two empty slices holds: without this, a provider
	// with no token would confirm its address to a request offering none.
	expected, configured := h.tokens.VerifyToken(tenant, name)
	if !configured {
		h.logger.DebugContext(ctx, "a handshake arrived for a provider that confirms nothing",
			"tenant", slug, "provider", name)
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "only POST is served for this provider", http.StatusMethodNotAllowed)
		return
	}
	// A token that was asked for and cannot be read here is a deploy that has
	// not landed, so it is ours and not the caller's: 405 would say this
	// address never confirmed anything, and it did. The inbound port answers
	// the same misconfiguration by recording the request as invalid, which is
	// open to it because a POST carries a payload that has to be kept
	// whatever we conclude. A handshake carries nothing to keep, so the status
	// is the whole answer and has to carry the difference itself.
	if expected == "" {
		h.logger.ErrorContext(ctx,
			"a handshake arrived for a provider whose verify token is not set in this process",
			"tenant", slug, "provider", name)
		http.Error(w, "the verify token is not readable here", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(query.Get(verifyTokenParam)), []byte(expected)) != 1 {
		// The offered token is never logged, nor anything measured from it.
		h.logger.WarnContext(ctx, "a handshake offered the wrong verify token",
			"tenant", slug, "provider", name)
		http.Error(w, "the verify token does not match", http.StatusForbidden)
		return
	}

	// After the token and not before it: a 400 handed to a caller who proved
	// nothing is an answer they could tell apart from 405, which would let a
	// stranger learn that this address confirms one. The comparison above is
	// over a different parameter and costs nothing more for an oversized one.
	challenge := query.Get(challengeParam)
	if len(challenge) > maxChallengeBytes {
		// The length only. The value is not a credential, but it is not ours.
		h.logger.WarnContext(ctx, "a handshake offered a challenge too long to echo",
			"tenant", slug, "provider", name, "bytes", len(challenge))
		http.Error(w, "the challenge is too long to echo", http.StatusBadRequest)
		return
	}

	h.logger.InfoContext(ctx, "confirmed a webhook address", "tenant", slug, "provider", name)

	// The body is the challenge and nothing else: the provider compares it
	// byte for byte, so a trailing newline fails the handshake. Echoing what
	// arrived is the whole point, and the two headers below are what keeps it
	// from being read as markup by anything that follows a link here — as is
	// the token, since nothing is echoed to a caller that could not offer it.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // G705: the echo is the contract, and text/plain with nosniff is the mitigation
	if _, err := w.Write([]byte(challenge)); err != nil {
		h.logger.WarnContext(ctx, "could not write the handshake answer",
			"tenant", slug, "provider", name, "error", err)
	}
}

// nothingConfigured stands in when no tokens are wired, so the port has one
// path rather than a nil test on every request.
type nothingConfigured struct{}

func (nothingConfigured) VerifyToken(uuid.UUID, string) (string, bool) { return "", false }
