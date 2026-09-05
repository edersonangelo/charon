package provider_test

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the code under test supports it, so the test has to produce it
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/provider"
)

const secret = "whsec_do_not_tell"

var body = []byte(`{"id":"evt_1","type":"charge.succeeded"}`)

func digest(t *testing.T, algorithm, encoding string, key string, payload []byte) string {
	t.Helper()

	var newHash func() hash.Hash
	switch algorithm {
	case provider.SHA1:
		newHash = sha1.New
	case provider.SHA512:
		newHash = sha512.New
	default:
		newHash = sha256.New
	}

	mac := hmac.New(newHash, []byte(key))
	mac.Write(payload)
	sum := mac.Sum(nil)

	if encoding == provider.Base64 {
		return base64.StdEncoding.EncodeToString(sum)
	}
	return hex.EncodeToString(sum)
}

func headers(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func verifierFor(t *testing.T, settings provider.Settings) provider.Verifier {
	t.Helper()

	verifier, err := provider.Default().Verifier(settings)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	return verifier
}

func check(t *testing.T, verifier provider.Verifier, h http.Header, payload []byte, now time.Time) (provider.State, error) {
	t.Helper()
	return verifier.Verify(provider.Request{Headers: h, Body: payload, Now: now})
}

func TestNothingConfiguredLeavesTheRequestUnchecked(t *testing.T) {
	t.Parallel()

	state, err := check(t, verifierFor(t, provider.Settings{}), headers(), body, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != provider.Unchecked {
		t.Errorf("state = %q, want %q", state, provider.Unchecked)
	}
}

func TestSettingsThatCannotVerifyRefuseInsteadOfPassing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings provider.Settings
		wantErr  error
	}{
		{
			name:     "a named secret that is not set",
			settings: provider.Settings{Verifier: provider.HMAC},
			wantErr:  provider.ErrNoSecret,
		},
		{
			name:     "a kind nobody registered",
			settings: provider.Settings{Verifier: "magic", Secret: secret},
			wantErr:  provider.ErrUnknownKind,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			verifier, err := provider.Default().Verifier(tt.settings)
			if err != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}

			state, _ := check(t, verifier, headers("X-Signature", "anything"), body, time.Now())
			if state == provider.Valid || state == provider.Unchecked {
				t.Errorf("state = %q, want a refusal: broken settings must not let a request through", state)
			}
		})
	}
}

func TestUnusableParametersAreRejectedWhenBuilding(t *testing.T) {
	t.Parallel()

	tests := []provider.Settings{
		{Verifier: provider.HMAC, Secret: secret, Algorithm: "md5"},
		{Verifier: provider.HMAC, Secret: secret, Encoding: "rot13"},
		{Verifier: provider.HMAC, Secret: secret, Scheme: "telepathy"},
	}

	for _, settings := range tests {
		t.Run(settings.Algorithm+settings.Encoding+settings.Scheme, func(t *testing.T) {
			t.Parallel()

			if _, err := provider.Default().Verifier(settings); !errors.Is(err, provider.ErrUnknownOption) {
				t.Errorf("error = %v, want ErrUnknownOption", err)
			}
		})
	}
}

// One HMAC engine, four axes: algorithm, encoding, header layout and header
// name. A vendor is a point in this space, not a branch.
func TestHMACAcrossEveryCombination(t *testing.T) {
	t.Parallel()

	for _, algorithm := range provider.Algorithms() {
		for _, encoding := range provider.Encodings() {
			name := algorithm + "/" + encoding

			t.Run("simple/"+name, func(t *testing.T) {
				t.Parallel()

				settings := provider.Settings{
					Verifier: provider.HMAC, Secret: secret, Scheme: provider.Simple,
					Algorithm: algorithm, Encoding: encoding, Header: "X-Proof",
				}
				verifier := verifierFor(t, settings)

				state, err := check(t, verifier,
					headers("X-Proof", digest(t, algorithm, encoding, secret, body)), body, time.Now())
				if err != nil || state != provider.Valid {
					t.Fatalf("state = %q, err = %v, want valid", state, err)
				}

				state, _ = check(t, verifier,
					headers("X-Proof", digest(t, algorithm, encoding, "wrong", body)), body, time.Now())
				if state != provider.Invalid {
					t.Errorf("state = %q with another secret, want invalid", state)
				}
			})

			t.Run("advanced/"+name, func(t *testing.T) {
				t.Parallel()

				now := time.Unix(1757000000, 0)
				settings := provider.Settings{
					Verifier: provider.HMAC, Secret: secret, Scheme: provider.Advanced,
					Algorithm: algorithm, Encoding: encoding, Header: "X-Proof",
					TimestampKey: "t", SignatureKey: "v1", Tolerance: 5 * time.Minute,
				}
				verifier := verifierFor(t, settings)

				signed := func(at time.Time, key string) string {
					stamp := fmt.Sprint(at.Unix())
					return fmt.Sprintf("t=%s,v1=%s", stamp,
						digest(t, algorithm, encoding, key, append([]byte(stamp+"."), body...)))
				}

				state, err := check(t, verifier, headers("X-Proof", signed(now, secret)), body, now)
				if err != nil || state != provider.Valid {
					t.Fatalf("state = %q, err = %v, want valid", state, err)
				}

				state, replayErr := check(t, verifier,
					headers("X-Proof", signed(now.Add(-time.Hour), secret)), body, now)
				if state != provider.Invalid || !errors.Is(replayErr, provider.ErrStale) {
					t.Errorf("state = %q, err = %v, want a stale refusal for a replay", state, replayErr)
				}
			})
		}
	}
}

func TestHMACRefusalsAndAbsences(t *testing.T) {
	t.Parallel()

	simple := verifierFor(t, provider.Settings{
		Verifier: provider.HMAC, Secret: secret, Header: "X-Proof",
	})
	advanced := verifierFor(t, provider.Settings{
		Verifier: provider.HMAC, Secret: secret, Header: "X-Proof",
		Scheme: provider.Advanced, Tolerance: time.Minute,
	})

	tests := []struct {
		name     string
		verifier provider.Verifier
		headers  http.Header
		body     []byte
		want     provider.State
		wantErr  error
	}{
		{
			name: "no header at all", verifier: simple, headers: headers(), body: body,
			want: provider.Missing, wantErr: provider.ErrNoProof,
		},
		{
			name: "the body changed after signing", verifier: simple,
			headers: headers("X-Proof", digest(t, provider.SHA256, provider.Hex, secret, body)),
			body:    append(body, ' '),
			want:    provider.Invalid, wantErr: provider.ErrMismatch,
		},
		{
			name: "a prefixed digest is accepted", verifier: simple,
			headers: headers("X-Proof", "sha256="+digest(t, provider.SHA256, provider.Hex, secret, body)),
			body:    body, want: provider.Valid,
		},
		{
			name: "a digest that is not in the declared encoding", verifier: simple,
			headers: headers("X-Proof", "not-a-digest"), body: body,
			want: provider.Invalid, wantErr: provider.ErrMismatch,
		},
		{
			name: "an advanced header with no timestamp", verifier: advanced,
			headers: headers("X-Proof", "v1="+digest(t, provider.SHA256, provider.Hex, secret, body)),
			body:    body, want: provider.Invalid, wantErr: provider.ErrMalformed,
		},
		{
			name: "an advanced header with no digest", verifier: advanced,
			headers: headers("X-Proof", "t=1757000000"), body: body,
			want: provider.Invalid, wantErr: provider.ErrMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, err := check(t, tt.verifier, tt.headers, tt.body, time.Unix(1757000000, 0))
			if state != tt.want {
				t.Errorf("state = %q, want %q", state, tt.want)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A provider that offers no signature leaves only a shared value to compare.
// It proves the caller knows the secret and nothing about the body.
func TestSharedToken(t *testing.T) {
	t.Parallel()

	verifier := verifierFor(t, provider.Settings{
		Verifier: provider.SharedToken, Secret: "token-123", Header: "X-Api-Key",
	})

	tests := []struct {
		name    string
		headers http.Header
		want    provider.State
	}{
		{"the configured token", headers("X-Api-Key", "token-123"), provider.Valid},
		{"another token", headers("X-Api-Key", "token-456"), provider.Invalid},
		{"no token", headers(), provider.Missing},
		{"a different header", headers("X-Other", "token-123"), provider.Missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, _ := check(t, verifier, tt.headers, body, time.Now())
			if state != tt.want {
				t.Errorf("state = %q, want %q", state, tt.want)
			}
		})
	}
}

func TestABearerPrefixIsTheSameCaseAsABareToken(t *testing.T) {
	t.Parallel()

	verifier := verifierFor(t, provider.Settings{
		Verifier: provider.SharedToken, Secret: "token-123",
	})

	state, _ := check(t, verifier, headers("Authorization", "Bearer token-123"), body, time.Now())
	if state != provider.Valid {
		t.Errorf("state = %q, want %q", state, provider.Valid)
	}
}

func TestBasicAuth(t *testing.T) {
	t.Parallel()

	verifier := verifierFor(t, provider.Settings{
		Verifier: provider.BasicAuth, Secret: "hooks:s3cr3t",
	})

	encode := func(pair string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair))
	}

	tests := []struct {
		name    string
		headers http.Header
		want    provider.State
	}{
		{"the configured pair", headers("Authorization", encode("hooks:s3cr3t")), provider.Valid},
		{"another password", headers("Authorization", encode("hooks:wrong")), provider.Invalid},
		{"not basic", headers("Authorization", "Bearer s3cr3t"), provider.Invalid},
		{"not base64", headers("Authorization", "Basic !!!"), provider.Invalid},
		{"no credentials", headers(), provider.Missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, _ := check(t, verifier, tt.headers, body, time.Now())
			if state != tt.want {
				t.Errorf("state = %q, want %q", state, tt.want)
			}
		})
	}
}

// A preset must be nothing but parameters: whatever it sets has to work when
// typed by hand, and the engine must never read its name.
func TestEveryPresetIsOnlyParameters(t *testing.T) {
	t.Parallel()

	registry := provider.Default()

	for _, preset := range provider.Presets() {
		t.Run(preset.Name, func(t *testing.T) {
			t.Parallel()

			if !registry.Knows(preset.Settings.Verifier) {
				t.Fatalf("preset %q names verifier %q, which is not registered",
					preset.Name, preset.Settings.Verifier)
			}

			settings := preset.Settings
			settings.Secret = secret
			if _, err := registry.Verifier(settings); err != nil {
				t.Errorf("preset %q does not build: %v", preset.Name, err)
			}
		})
	}
}

// The preset for a well known vendor has to accept what that vendor actually
// sends. This is the check that the parameters, not the name, do the work.
func TestThePresetsMatchWhatTheVendorsSend(t *testing.T) {
	t.Parallel()

	now := time.Unix(1757000000, 0)
	stamp := fmt.Sprint(now.Unix())

	tests := []struct {
		preset  string
		headers http.Header
	}{
		{
			preset: "github",
			headers: headers("X-Hub-Signature-256",
				"sha256="+digest(t, provider.SHA256, provider.Hex, secret, body)),
		},
		{
			preset: "stripe",
			headers: headers("Stripe-Signature", fmt.Sprintf("t=%s,v1=%s", stamp,
				digest(t, provider.SHA256, provider.Hex, secret, append([]byte(stamp+"."), body...)))),
		},
		{
			preset: "shopify",
			headers: headers("X-Shopify-Hmac-Sha256",
				digest(t, provider.SHA256, provider.Base64, secret, body)),
		},
		{
			preset:  "bearer-token",
			headers: headers("Authorization", "Bearer "+secret),
		},
		{
			preset: "basic-auth",
			headers: headers("Authorization",
				"Basic "+base64.StdEncoding.EncodeToString([]byte(secret))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.preset, func(t *testing.T) {
			t.Parallel()

			preset, found := provider.PresetByName(tt.preset)
			if !found {
				t.Fatalf("no preset named %q", tt.preset)
			}

			settings := preset.Settings
			settings.Secret = secret

			state, err := check(t, verifierFor(t, settings), tt.headers, body, now)
			if err != nil || state != provider.Valid {
				t.Errorf("state = %q, err = %v, want valid", state, err)
			}
		})
	}
}
