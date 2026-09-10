package handshake_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/handshake"
	"github.com/edersonangelo/charon/internal/ingest"
)

var (
	firstTenant  = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	secondTenant = uuid.MustParse("00000000-0000-0000-0000-000000000002")
)

type fakeTenants struct{ fail error }

func (f fakeTenants) Tenant(_ context.Context, slug string) (uuid.UUID, bool, error) {
	if f.fail != nil {
		return uuid.Nil, false, f.fail
	}
	switch slug {
	case ingest.DefaultTenant:
		return firstTenant, true, nil
	case "second":
		return secondTenant, true, nil
	default:
		return uuid.Nil, false, nil
	}
}

// tokens is the port the handler needs: a token per tenant and provider, and
// nothing for anybody absent. A provider present with an empty token is one
// that named a variable this process cannot read.
type tokens map[uuid.UUID]map[string]string

func (t tokens) VerifyToken(tenant uuid.UUID, name string) (string, bool) {
	token, configured := t[tenant][name]
	return token, configured
}

func serve(t *testing.T, cfg handshake.Config) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	handshake.New(fakeTenants{}, cfg).Register(mux)
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

const (
	challenge = "1158201444"
	token     = "a-token-the-operator-chose"
)

func configured() handshake.Config {
	return handshake.Config{Tokens: tokens{firstTenant: {"whatsapp": token}}}
}

func TestTheChallengeIsEchoedExactly(t *testing.T) {
	t.Parallel()

	rec := get(t, serve(t, configured()),
		"/webhooks/whatsapp?hub.mode=subscribe&hub.challenge="+challenge+"&hub.verify_token="+token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	// By equality, not by containment: the provider compares the body byte for
	// byte, so a trailing newline is a failed handshake.
	if got := rec.Body.String(); got != challenge {
		t.Errorf("body = %q, want %q", got, challenge)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q, want text/plain", got)
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{"a different token", "&hub.verify_token=not-the-token"},
		{"an empty token", "&hub.verify_token="},
		{"no token at all", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := get(t, serve(t, configured()),
				"/webhooks/whatsapp?hub.mode=subscribe&hub.challenge="+challenge+tt.query)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			if body := rec.Body.String(); strings.Contains(body, challenge) {
				t.Errorf("body %q echoed the challenge to a caller that proved nothing", body)
			}
			if body := rec.Body.String(); strings.Contains(body, token) {
				t.Errorf("body %q gave away the configured token", body)
			}
		})
	}
}

// A token that was asked for and cannot be read here is a deploy that has not
// landed. Answering 405 would say this address never confirmed anything, and a
// provider that gives up on it stops trying; 503 is true and it will retry.
func TestATokenThatCannotBeReadHereIsNotARefusal(t *testing.T) {
	t.Parallel()

	cfg := handshake.Config{Tokens: tokens{firstTenant: {"whatsapp": ""}}}
	rec := get(t, serve(t, cfg),
		"/webhooks/whatsapp?hub.challenge="+challenge+"&hub.verify_token="+token)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body %q)",
			rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, challenge) {
		t.Errorf("body %q echoed the challenge with no token to compare", body)
	}
	if got := rec.Header().Get("Allow"); got != "" {
		t.Errorf("allow = %q, want it unset: the method is not the complaint", got)
	}
}

// ConstantTimeCompare of two empty slices holds, so a provider whose token
// cannot be read here must not confirm its address to a request that offered
// nothing either.
func TestAnEmptyTokenNeverMatches(t *testing.T) {
	t.Parallel()

	cfg := handshake.Config{Tokens: tokens{firstTenant: {"whatsapp": ""}}}
	rec := get(t, serve(t, cfg), "/webhooks/whatsapp?hub.challenge="+challenge)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want anything but %d (body %q)",
			rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, challenge) {
		t.Errorf("body %q echoed the challenge to a caller that proved nothing", body)
	}
}

func TestAProviderWithNoHandshakeIsStillPostOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  handshake.Config
	}{
		{"nothing configured for this provider", configured()},
		{"nothing wired at all", handshake.Config{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := get(t, serve(t, tt.cfg), "/webhooks/stripe")

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("allow = %q, want %q", got, http.MethodPost)
			}
		})
	}
}

func TestTheAddressDecidesWhoseTokenIsExpected(t *testing.T) {
	t.Parallel()

	cfg := handshake.Config{Tokens: tokens{
		firstTenant:  {"whatsapp": token},
		secondTenant: {"whatsapp": "another-tenants-token"},
	}}
	mux := serve(t, cfg)

	confirmed := get(t, mux,
		"/webhooks/second/whatsapp?hub.challenge="+challenge+"&hub.verify_token=another-tenants-token")
	if confirmed.Code != http.StatusOK {
		t.Fatalf("the second tenant's own token = %d, want %d", confirmed.Code, http.StatusOK)
	}

	crossed := get(t, mux,
		"/webhooks/second/whatsapp?hub.challenge="+challenge+"&hub.verify_token="+token)
	if crossed.Code != http.StatusForbidden {
		t.Errorf("one tenant's token on another's address = %d, want %d",
			crossed.Code, http.StatusForbidden)
	}
}

func TestAHandshakeForAnUnknownTenantIsNotFound(t *testing.T) {
	t.Parallel()

	rec := get(t, serve(t, configured()),
		"/webhooks/nobody/whatsapp?hub.challenge="+challenge+"&hub.verify_token="+token)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAnAbsentChallengeIsEchoedAsNothing(t *testing.T) {
	t.Parallel()

	rec := get(t, serve(t, configured()), "/webhooks/whatsapp?hub.verify_token="+token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("body = %q, want it empty", body)
	}
}

func TestAChallengeTooLongToEchoIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		bytes int
		want  int
	}{
		{"at the ceiling", 1024, http.StatusOK},
		{"over the ceiling", 1025, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			long := strings.Repeat("9", tt.bytes)
			rec := get(t, serve(t, configured()),
				"/webhooks/whatsapp?hub.challenge="+long+"&hub.verify_token="+token)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusOK && rec.Body.String() != long {
				t.Errorf("body is %d bytes, want the challenge's %d",
					rec.Body.Len(), len(long))
			}
			if tt.want != http.StatusOK && strings.Contains(rec.Body.String(), long) {
				t.Error("the refusal echoed the challenge it refused")
			}
		})
	}
}

// The ceiling is checked after the token, so an oversized challenge cannot be
// used to tell a configured address apart from one that serves only POST.
func TestAnUnknownCallerIsNotToldItsChallengeIsTooLong(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("9", 1025)
	rec := get(t, serve(t, configured()),
		"/webhooks/whatsapp?hub.challenge="+long+"&hub.verify_token=not-the-token")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// A database that cannot answer is not a refusal: answering 403 would tell a
// provider its token is wrong when nothing was ever compared.
func TestADatabaseThatCannotAnswerIsNotARefusal(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handshake.New(fakeTenants{fail: errors.New("the database is gone")}, configured()).Register(mux)

	rec := get(t, mux, "/webhooks/whatsapp?hub.challenge="+challenge+"&hub.verify_token="+token)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
