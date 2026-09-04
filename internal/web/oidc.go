package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const stateCookie = "charon_oidc"

type OIDC struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	AutoProvision bool
	RequiredGroup string
}

func (c OIDC) Configured() bool {
	return c.Issuer != "" && c.ClientID != "" && c.RedirectURL != ""
}

// Discovery happens on first use, not at start. The identity provider being
// unreachable must never keep the inbound port from serving.
type provider struct {
	cfg OIDC

	once     sync.Mutex
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

func (p *provider) resolve(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	p.once.Lock()
	defer p.once.Unlock()

	if p.oauth != nil {
		return p.oauth, p.verifier, nil
	}

	discovered, err := oidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("discovering %s: %w", p.cfg.Issuer, err)
	}

	scopes := p.cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}

	p.oauth = &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		RedirectURL:  p.cfg.RedirectURL,
		Endpoint:     discovered.Endpoint(),
		Scopes:       scopes,
	}
	p.verifier = discovered.Verifier(&oidc.Config{ClientID: p.cfg.ClientID})

	return p.oauth, p.verifier, nil
}

type handshake struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
}

func (h *Handler) startSSO(w http.ResponseWriter, r *http.Request) {
	oauthCfg, _, err := h.oidc.resolve(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "single sign-on is unavailable", "error", err)
		http.Redirect(w, r, "/login?sso=unavailable", http.StatusSeeOther)
		return
	}

	state, err := randomString()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	nonce, err := randomString()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	verifier := oauth2.GenerateVerifier()

	payload, err := json.Marshal(handshake{State: state, Nonce: nonce, Verifier: verifier})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	//nolint:gosec // Secure is configurable: the panel also runs over plain HTTP locally
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    encode(payload),
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, oauthCfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
}

func (h *Handler) callbackSSO(w http.ResponseWriter, r *http.Request) {
	oauthCfg, verifier, err := h.oidc.resolve(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "single sign-on is unavailable", "error", err)
		http.Redirect(w, r, "/login?sso=unavailable", http.StatusSeeOther)
		return
	}

	started, err := h.readHandshake(r)
	if err != nil {
		h.rejectSSO(w, r, "no handshake to match", err)
		return
	}
	h.clearStateCookie(w)

	if r.URL.Query().Get("state") != started.State {
		h.rejectSSO(w, r, "state did not match", nil)
		return
	}

	token, err := oauthCfg.Exchange(r.Context(),
		r.URL.Query().Get("code"), oauth2.VerifierOption(started.Verifier))
	if err != nil {
		h.rejectSSO(w, r, "exchanging the code", err)
		return
	}

	raw, ok := token.Extra("id_token").(string)
	if !ok {
		h.rejectSSO(w, r, "the response carried no id token", nil)
		return
	}

	identity, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		h.rejectSSO(w, r, "verifying the id token", err)
		return
	}
	if identity.Nonce != started.Nonce {
		h.rejectSSO(w, r, "nonce did not match", nil)
		return
	}

	var claims struct {
		Email    string   `json:"email"`
		Verified *bool    `json:"email_verified"`
		Groups   []string `json:"groups"`
	}
	if err := identity.Claims(&claims); err != nil {
		h.rejectSSO(w, r, "reading the claims", err)
		return
	}
	if claims.Email == "" {
		h.rejectSSO(w, r, "the id token carried no email", nil)
		return
	}
	if claims.Verified != nil && !*claims.Verified {
		h.rejectSSO(w, r, "the address is not verified at the provider", nil)
		return
	}
	if group := h.oidc.cfg.RequiredGroup; group != "" && !slices.Contains(claims.Groups, group) {
		h.rejectSSO(w, r, "the account is not in "+group, nil)
		return
	}

	operator, err := h.store.UserBySubject(
		r.Context(), identity.Subject, claims.Email, h.oidc.cfg.AutoProvision)
	if err != nil {
		h.rejectSSO(w, r, "no operator for "+claims.Email, err)
		return
	}

	if err := h.grantSession(w, r, operator.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.logger.InfoContext(r.Context(), "signed in through single sign-on",
		"email", operator.Email, "subject", identity.Subject)
	http.Redirect(w, r, "/events", http.StatusSeeOther)
}

func (h *Handler) readHandshake(r *http.Request) (handshake, error) {
	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		return handshake{}, fmt.Errorf("reading the handshake cookie: %w", err)
	}

	payload, err := decode(cookie.Value)
	if err != nil {
		return handshake{}, err
	}

	var started handshake
	if err := json.Unmarshal(payload, &started); err != nil {
		return handshake{}, fmt.Errorf("decoding the handshake: %w", err)
	}
	if started.State == "" || started.Nonce == "" || started.Verifier == "" {
		return handshake{}, errors.New("the handshake is incomplete")
	}
	return started, nil
}

func (h *Handler) rejectSSO(w http.ResponseWriter, r *http.Request, why string, err error) {
	h.clearStateCookie(w)
	h.logger.WarnContext(r.Context(), "rejected a single sign-on attempt",
		"reason", why, "error", err)
	http.Redirect(w, r, "/login?sso=rejected", http.StatusSeeOther)
}

func (h *Handler) clearStateCookie(w http.ResponseWriter) {
	//nolint:gosec // same as above
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}
