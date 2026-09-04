package testsupport

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// IdentityProvider is an OpenID Connect provider small enough to run inside a
// test: it discovers, authorises, exchanges and signs, so the whole sign-on
// flow is exercised for real rather than stubbed at the edges.
type IdentityProvider struct {
	Subject string
	Email   string
	Groups  []string
	Verify  bool

	server *httptest.Server
	key    *rsa.PrivateKey

	mu     sync.Mutex
	nonces map[string]string
}

func NewIdentityProvider(tb testing.TB) *IdentityProvider {
	tb.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatalf("generating a signing key: %v", err)
	}

	idp := &IdentityProvider{
		Subject: "sub-1",
		Email:   "operator@example.com",
		Verify:  true,
		key:     key,
		nonces:  map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("GET /authorize", idp.authorize)
	mux.HandleFunc("POST /token", idp.token)

	idp.server = httptest.NewServer(mux)
	tb.Cleanup(idp.server.Close)

	return idp
}

func (i *IdentityProvider) Issuer() string { return i.server.URL }

func (i *IdentityProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                i.server.URL,
		"authorization_endpoint":                i.server.URL + "/authorize",
		"token_endpoint":                        i.server.URL + "/token",
		"jwks_uri":                              i.server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (i *IdentityProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": "test",
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.key.E)).Bytes()),
	}}})
}

func (i *IdentityProvider) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	code, err := randomCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	i.mu.Lock()
	i.nonces[code] = query.Get("nonce")
	i.mu.Unlock()

	back, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	returned := back.Query()
	returned.Set("code", code)
	returned.Set("state", query.Get("state"))
	back.RawQuery = returned.Encode()

	// An authorisation endpoint redirecting to the client's redirect_uri is
	// what the protocol says to do; this is a test provider, not a target.
	//nolint:gosec // G710
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (i *IdentityProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	i.mu.Lock()
	nonce, known := i.nonces[r.PostForm.Get("code")]
	i.mu.Unlock()
	if !known {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}

	claims := map[string]any{
		"iss":            i.server.URL,
		"aud":            r.PostForm.Get("client_id"),
		"sub":            i.Subject,
		"email":          i.Email,
		"email_verified": i.Verify,
		"nonce":          nonce,
		"iat":            time.Now().Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
	if r.PostForm.Get("client_id") == "" {
		user, _, _ := r.BasicAuth()
		claims["aud"] = user
	}
	if len(i.Groups) > 0 {
		claims["groups"] = i.Groups
	}

	signed, err := i.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"access_token": "not-used",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     signed,
	})
}

func (i *IdentityProvider) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	if err != nil {
		return "", fmt.Errorf("encoding the header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encoding the claims: %w", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)

	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing: %w", err)
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func randomCode() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
