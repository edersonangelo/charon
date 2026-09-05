package provider

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Proof that carries no signature: the caller only shows it knows the secret.
// It says nothing about the body, and is what a provider that does not sign
// leaves you with.
const (
	SharedToken = "shared-token"
	BasicAuth   = "basic-auth"
)

type sharedTokenVerifier struct {
	header string
	secret []byte
}

func buildSharedToken(settings Settings) (Verifier, error) {
	header := settings.Header
	if header == "" {
		header = "Authorization"
	}
	return sharedTokenVerifier{header: header, secret: []byte(settings.Secret)}, nil
}

func (v sharedTokenVerifier) Verify(req Request) (State, error) {
	given := req.Headers.Get(v.header)
	if given == "" {
		return Missing, ErrNoProof
	}

	// "Bearer <token>" is the same case as a bare token.
	if scheme, rest, found := strings.Cut(given, " "); found && strings.EqualFold(scheme, "bearer") {
		given = rest
	}

	if subtle.ConstantTimeCompare([]byte(given), v.secret) != 1 {
		return Invalid, ErrMismatch
	}
	return Valid, nil
}

// The secret is the whole "user:password" pair, so a provider that sets only a
// password works with the user left empty.
type basicAuthVerifier struct{ secret []byte }

func buildBasicAuth(settings Settings) (Verifier, error) {
	return basicAuthVerifier{secret: []byte(settings.Secret)}, nil
}

func (v basicAuthVerifier) Verify(req Request) (State, error) {
	given := req.Headers.Get("Authorization")
	if given == "" {
		return Missing, ErrNoProof
	}

	encoded, found := strings.CutPrefix(given, "Basic ")
	if !found {
		return Invalid, ErrMalformed
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return Invalid, ErrMalformed
	}

	if subtle.ConstantTimeCompare(decoded, v.secret) != 1 {
		return Invalid, ErrMismatch
	}
	return Valid, nil
}
