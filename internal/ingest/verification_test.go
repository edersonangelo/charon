package ingest_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/ingest"
	"github.com/edersonangelo/charon/internal/provider"
)

// verification is the port the inbound handler needs: a built verifier per
// provider, and one that checks nothing for anybody absent.
type verification map[string]provider.Verifier

func (v verification) Verifier(_ uuid.UUID, name string) provider.Verifier {
	if verifier, configured := v[name]; configured {
		return verifier
	}
	return provider.Unverified{}
}

func verifierFor(t *testing.T, settings provider.Settings) provider.Verifier {
	t.Helper()

	verifier, err := provider.Default().Verifier(settings)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	return verifier
}

func hmacHex(payload []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// A signature that does not verify is still recorded. The gateway exists so
// that a forged request can be seen, not so that it disappears.
func TestAFailedSignatureIsRecordedAndMarked(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"id":"evt_1"}`)
	configured := verification{"github": verifierFor(t, provider.Settings{
		Verifier: provider.HMAC,
		Secret:   "right",
		Header:   "X-Hub-Signature-256",
	})}

	tests := []struct {
		name      string
		signature string
		want      provider.State
	}{
		{"signed with the secret", "sha256=" + hmacHex(payload, "right"), provider.Valid},
		{"signed with another secret", "sha256=" + hmacHex(payload, "wrong"), provider.Invalid},
		{"no signature", "", provider.Missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeRecorder{}
			mux := http.NewServeMux()
			ingest.New(fake, ingest.Config{Verification: configured}).Register(mux)

			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodPost, "/webhooks/github", bytesReader(payload))
			if tt.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tt.signature)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: a bad signature is recorded, not refused",
					rec.Code, http.StatusAccepted)
			}

			recorded := fake.recorded()
			if len(recorded) != 1 {
				t.Fatalf("recorded %d requests, want 1", len(recorded))
			}
			if recorded[0].Signature != string(tt.want) {
				t.Errorf("signature = %q, want %q", recorded[0].Signature, tt.want)
			}
		})
	}
}

func TestAProviderWithNoVerifierStaysUnchecked(t *testing.T) {
	t.Parallel()

	fake := &fakeRecorder{}
	mux := http.NewServeMux()
	ingest.New(fake, ingest.Config{Verification: verification{}}).Register(mux)

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost, "/webhooks/anything", bytesReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if got := fake.recorded()[0].Signature; got != string(provider.Unchecked) {
		t.Errorf("signature = %q, want %q", got, provider.Unchecked)
	}
}
