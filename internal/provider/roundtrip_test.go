package provider_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/provider"
)

// One Charon delivering to another is the only case where both sides of a
// signature are in this repository, so it is the only place the format can be
// proven rather than described. If these two ever disagree, every receiver
// written from the documentation is wrong too.
func verifierForCharon(t *testing.T, secret string) provider.Verifier {
	t.Helper()

	preset, found := provider.PresetByName("charon")
	if !found {
		t.Fatal("no charon preset")
	}
	settings := preset.Settings
	settings.Secret = secret

	verifier, err := provider.Default().Verifier(settings)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	return verifier
}

func signed(secrets []string, now time.Time, body []byte) provider.Request {
	raw := make([][]byte, 0, len(secrets))
	for _, secret := range secrets {
		raw = append(raw, []byte(secret))
	}

	headers := http.Header{}
	headers.Set(outbound.SignatureHeader, outbound.Signature(raw, now.Unix(), body))
	return provider.Request{Headers: headers, Body: body, Now: now}
}

func TestCharonVerifiesWhatCharonSigns(t *testing.T) {
	now := time.Now()
	body := []byte(`{"id":"evt_1","amount":100}`)

	state, err := verifierForCharon(t, "s3cret").
		Verify(signed([]string{"s3cret"}, now, body))

	if err != nil || state != provider.Valid {
		t.Fatalf("got %s (%v), want valid", state, err)
	}
}

func TestEitherSecretPassesWhileRotating(t *testing.T) {
	now := time.Now()
	body := []byte(`{"id":"evt_1"}`)
	request := signed([]string{"old", "new"}, now, body)

	for _, secret := range []string{"old", "new"} {
		state, err := verifierForCharon(t, secret).Verify(request)
		if err != nil || state != provider.Valid {
			t.Errorf("a receiver still holding %q got %s (%v), want valid",
				secret, state, err)
		}
	}
}

func TestAnotherSecretDoesNotPass(t *testing.T) {
	now := time.Now()
	body := []byte(`{"id":"evt_1"}`)

	state, _ := verifierForCharon(t, "someone else's").
		Verify(signed([]string{"s3cret"}, now, body))

	if state == provider.Valid {
		t.Error("a signature made with another secret was accepted")
	}
}

func TestATamperedBodyDoesNotPass(t *testing.T) {
	now := time.Now()
	request := signed([]string{"s3cret"}, now, []byte(`{"amount":100}`))
	request.Body = []byte(`{"amount":900}`)

	state, _ := verifierForCharon(t, "s3cret").Verify(request)
	if state == provider.Valid {
		t.Error("a body changed after signing was accepted")
	}
}

// The reason the moment belongs to the attempt and not to the delivery: a
// retry carrying the moment of the first attempt is refused by the receiver
// exactly when the delivery most needs to get through.
func TestAStaleMomentDoesNotPass(t *testing.T) {
	signedAt := time.Now().Add(-time.Hour)
	request := signed([]string{"s3cret"}, signedAt, []byte(`{"id":"evt_1"}`))
	request.Now = time.Now()

	state, err := verifierForCharon(t, "s3cret").Verify(request)
	if state == provider.Valid {
		t.Error("an hour-old signature was accepted")
	}
	if err == nil {
		t.Error("refusing a stale signature should say why")
	}
}
