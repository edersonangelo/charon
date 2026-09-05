package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	OIDC = "oidc"

	// DefaultTenantClaim is what most providers that send groups at all call
	// them. It is not part of OpenID Connect, which is why it can be changed.
	DefaultTenantClaim = "groups"

	stateCookie = "charon_oidc"
)

// OIDCSettings is what a deployment gives the OpenID Connect method.
type OIDCSettings struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// TenantClaim is the claim carrying the groups an identity belongs to.
	// It is not part of OpenID Connect: providers differ on the name, and some
	// send none at all, so the deployment names it.
	TenantClaim string
	// SecureCookie marks the handshake cookie Secure. It follows the panel's
	// own setting, because both cookies travel the same way.
	SecureCookie bool
	Logger       *slog.Logger
}

func (s OIDCSettings) Configured() bool {
	return s.Issuer != "" && s.ClientID != "" && s.RedirectURL != ""
}

// Discovery happens on first use, not at start. The identity provider being
// unreachable must never keep the inbound port from serving.
type openIDConnect struct {
	settings OIDCSettings
	logger   *slog.Logger

	once     sync.Mutex
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	provider *oidc.Provider
}

// NewOIDC builds the OpenID Connect sign-in. It is a Redirector: it sends the
// operator to the provider and reads the return leg.
func NewOIDC(settings OIDCSettings) Redirector {
	logger := settings.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &openIDConnect{settings: settings, logger: logger}
}

func (*openIDConnect) Name() string  { return OIDC }
func (*openIDConnect) Label() string { return "single sign-on" }

func (o *openIDConnect) resolve(
	ctx context.Context,
) (*oauth2.Config, *oidc.IDTokenVerifier, *oidc.Provider, error) {
	o.once.Lock()
	defer o.once.Unlock()

	if o.oauth != nil {
		return o.oauth, o.verifier, o.provider, nil
	}

	discovered, err := oidc.NewProvider(ctx, o.settings.Issuer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: discovering %s: %w",
			ErrUnavailable, o.settings.Issuer, err)
	}

	scopes := o.settings.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}

	o.oauth = &oauth2.Config{
		ClientID:     o.settings.ClientID,
		ClientSecret: o.settings.ClientSecret,
		RedirectURL:  o.settings.RedirectURL,
		Endpoint:     discovered.Endpoint(),
		Scopes:       scopes,
	}
	o.verifier = discovered.Verifier(&oidc.Config{ClientID: o.settings.ClientID})
	o.provider = discovered

	return o.oauth, o.verifier, o.provider, nil
}

type handshake struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
}

func (o *openIDConnect) Start(w http.ResponseWriter, r *http.Request) error {
	oauthCfg, _, _, err := o.resolve(r.Context())
	if err != nil {
		return err
	}

	state, err := randomString()
	if err != nil {
		return err
	}
	nonce, err := randomString()
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()

	payload, err := json.Marshal(handshake{State: state, Nonce: nonce, Verifier: verifier})
	if err != nil {
		return fmt.Errorf("encoding the handshake: %w", err)
	}

	//nolint:gosec // Secure follows the panel, which also runs over plain HTTP locally
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   o.settings.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, oauthCfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
	return nil
}

func (o *openIDConnect) Identify(ctx context.Context, r *http.Request) (Identity, error) {
	oauthCfg, verifier, provider, err := o.resolve(ctx)
	if err != nil {
		return Identity{}, err
	}

	started, err := readHandshake(r)
	if err != nil {
		return Identity{}, errors.Join(ErrRefused, err)
	}
	if r.URL.Query().Get("state") != started.State {
		return Identity{}, errors.Join(ErrRefused, errors.New("the state did not match"))
	}

	token, err := oauthCfg.Exchange(ctx,
		r.URL.Query().Get("code"), oauth2.VerifierOption(started.Verifier))
	if err != nil {
		return Identity{}, errors.Join(ErrRefused, fmt.Errorf("exchanging the code: %w", err))
	}

	raw, carried := token.Extra("id_token").(string)
	if !carried {
		return Identity{}, errors.Join(ErrRefused, errors.New("the response carried no id token"))
	}

	identity, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, errors.Join(ErrRefused, fmt.Errorf("verifying the id token: %w", err))
	}
	if identity.Nonce != started.Nonce {
		return Identity{}, errors.Join(ErrRefused, errors.New("the nonce did not match"))
	}

	var claims map[string]any
	if err := identity.Claims(&claims); err != nil {
		return Identity{}, errors.Join(ErrRefused, fmt.Errorf("reading the claims: %w", err))
	}

	email, _ := claims["email"].(string)
	verified, said := claims["email_verified"].(bool)

	values := valuesIn(claims, o.settings.TenantClaim)

	// The id token is not the only place a provider puts claims. Where groups
	// live is a per provider choice: userinfo is the other place OpenID Connect
	// defines, and the access token is where Keycloak puts the roles it hands
	// out. Both are asked before giving up on them.
	if len(values) == 0 {
		values = o.valuesFromUserInfo(ctx, provider, token)
	}
	if len(values) == 0 {
		values = o.valuesFromAccessToken(ctx, provider, token)
	}
	if len(values) == 0 {
		// Where these values live is not settled between providers, so when
		// the claim that was named holds nothing anywhere, say which claims did
		// arrive and which of them carry a list. That is the difference between
		// guessing at a configuration and reading one.
		o.logger.InfoContext(ctx, "the identity carried nothing under the claim asked for",
			"claim", o.settings.tenantClaim(),
			"looked_in", "id token, userinfo, access token",
			"claims_present", names(claims),
			"claims_that_carry_a_list_of_names", listsOfStringsIn(claims))
	}
	return Identity{
		Subject: identity.Subject,
		Email:   email,
		Claims:  values,
		// Said out loud rather than acted on here: whether an unverified
		// address is acceptable depends on the provider, and that is the
		// deployment's call, not this method's.
		EmailUnverified: said && !verified,
	}, nil
}

// groupsIn reads the groups out of whatever the provider chose to send: a list
// of names, a single name, or nothing at all. Values that are not strings are
// left out rather than rendered, because a group is something an operator maps
// by name and has to be able to type.
// valuesFromUserInfo asks the provider directly. It is only reached when the
// id token carried none, and a failure is not a refusal: an identity without
// groups is an identity without groups, whatever the reason.
func (o *openIDConnect) valuesFromUserInfo(
	ctx context.Context, provider *oidc.Provider, token *oauth2.Token,
) []string {
	info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		o.logger.InfoContext(ctx, "could not read the provider's userinfo", "error", err)
		return nil
	}

	var claims map[string]any
	if err := info.Claims(&claims); err != nil {
		o.logger.InfoContext(ctx, "could not read the userinfo claims", "error", err)
		return nil
	}

	return valuesIn(claims, o.settings.TenantClaim)
}

// valuesFromAccessToken reads the other token the exchange returned. Its
// claims are only trusted after the provider's own keys have verified it:
// deciding anything from a token nobody checked is deciding from nothing.
func (o *openIDConnect) valuesFromAccessToken(
	ctx context.Context, provider *oidc.Provider, token *oauth2.Token,
) []string {
	// The audience of an access token is a resource, not this client, so the
	// check that belongs to an id token is not the one to make here.
	checked, err := provider.Verifier(&oidc.Config{
		SkipClientIDCheck: true,
	}).Verify(ctx, token.AccessToken)
	if err != nil {
		o.logger.DebugContext(ctx, "the access token is not one this provider signed",
			"error", err)
		return nil
	}

	var claims map[string]any
	if err := checked.Claims(&claims); err != nil {
		return nil
	}

	return valuesIn(claims, o.settings.TenantClaim)
}

func (s OIDCSettings) tenantClaim() string {
	if s.TenantClaim == "" {
		return DefaultTenantClaim
	}
	return s.TenantClaim
}

// names is what the token carried, without the values: enough to point the
// configuration at the right claim and nothing more than that. Claims nest, so
// the deeper ones are named by their path.
func names(claims map[string]any) []string {
	held := []string{}
	walk(claims, "", func(path string, _ any) { held = append(held, path) })
	slices.Sort(held)
	return held
}

// listsOfStringsIn narrows that down to the claims shaped like groups, which
// is what an operator is actually looking for. A provider is as likely to put
// them under something like realm_access.roles as at the top.
func listsOfStringsIn(claims map[string]any) []string {
	found := []string{}
	walk(claims, "", func(path string, value any) {
		if list, ok := value.([]any); ok && len(list) > 0 {
			if _, text := list[0].(string); text {
				found = append(found, path)
			}
		}
	})
	slices.Sort(found)
	return found
}

func walk(claims map[string]any, prefix string, visit func(path string, value any)) {
	for name, value := range claims {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if nested, ok := value.(map[string]any); ok {
			walk(nested, path, visit)
			continue
		}
		visit(path, value)
	}
}

// at follows a claim by its path, so a claim nested under another is named the
// way it reads: realm_access.roles.
func at(claims map[string]any, path string) any {
	segments := strings.Split(path, ".")
	var held any = claims
	for _, segment := range segments {
		nested, ok := held.(map[string]any)
		if !ok {
			return nil
		}
		held = nested[segment]
	}
	return held
}

func valuesIn(claims map[string]any, name string) []string {
	if name == "" {
		name = DefaultTenantClaim
	}

	switch held := at(claims, name).(type) {
	case []any:
		groups := make([]string, 0, len(held))
		for _, one := range held {
			if group, ok := one.(string); ok && group != "" {
				groups = append(groups, group)
			}
		}
		return groups
	case string:
		if held == "" {
			return nil
		}
		return []string{held}
	default:
		return nil
	}
}

// Done is called once the panel has finished with the return leg, so the
// handshake cookie does not outlive it.
func (o *openIDConnect) Done(w http.ResponseWriter) {
	//nolint:gosec // same as above
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: o.settings.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func readHandshake(r *http.Request) (handshake, error) {
	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		return handshake{}, fmt.Errorf("reading the handshake cookie: %w", err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return handshake{}, fmt.Errorf("decoding the handshake: %w", err)
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

func randomString() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
