package delivery_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
	"github.com/edersonangelo/charon/internal/testsupport"
)

// The point of checking a signature is that a forged request never reaches the
// service behind Charon.
func TestOnlyASignatureThatPassedIsDelivered(t *testing.T) {
	t.Parallel()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	if err := store.AddRoute(ctx, "stripe", "service", target.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	states := map[string]string{
		"valid":     "valid",
		"unchecked": "unchecked",
		"invalid":   "invalid",
		"missing":   "missing",
	}
	for name, state := range states {
		if _, err := store.Record(ctx, inbound.Request{
			Provider:   "stripe",
			Path:       "/webhooks/stripe",
			ReceivedAt: time.Now().UTC(),
			Headers:    map[string][]string{},
			Signature:  state,
			Body:       []byte(`{"case":"` + name + `"}`),
		}); err != nil {
			t.Fatalf("recording the %s case: %v", name, err)
		}
	}

	dispatcher := delivery.New(store, delivery.Config{})
	for range 3 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	if got := received.Load(); got != 2 {
		t.Errorf("the service received %d requests, want 2: only valid and unchecked", got)
	}

	counted, err := store.DeliveryStates(ctx)
	if err != nil {
		t.Fatalf("reading delivery states: %v", err)
	}
	if counted["delivered"] != 2 {
		t.Errorf("delivered = %d, want 2", counted["delivered"])
	}

	signatures, err := store.SignatureTotals(ctx)
	if err != nil {
		t.Fatalf("reading signature totals: %v", err)
	}
	for state, want := range map[string]int64{"valid": 1, "unchecked": 1, "invalid": 1, "missing": 1} {
		if signatures[state] != want {
			t.Errorf("%s events = %d, want %d: a refused request is kept", state, signatures[state], want)
		}
	}
}

// A secret configured wrong marks legitimate traffic as invalid. Recheck is
// the way back, and it must not need the events to arrive again.
func TestRecheckRecoversWhatASecretHadRefused(t *testing.T) {
	// t.Setenv rules out t.Parallel, and the secret has to come from the
	// environment because that is where the real one comes from.
	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	t.Setenv("CHARON_TEST_SECRET", "the-right-secret")
	preset, _ := provider.PresetByName("github")
	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name:      "github",
		SecretEnv: "CHARON_TEST_SECRET",
		Settings:  preset.Settings,
	}); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	payload := []byte(`{"ref":"refs/heads/main"}`)
	_, err = store.Record(ctx, inbound.Request{
		Provider:   "github",
		Path:       "/webhooks/github",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"X-Hub-Signature-256": {"sha256=" + githubDigest(payload, "the-right-secret")}},
		Signature:  "invalid",
		Body:       payload,
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	recovered, err := store.Recheck(ctx, "github", 100)
	if err != nil {
		t.Fatalf("rechecking: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered %d, want 1", recovered)
	}

	signatures, err := store.SignatureTotals(ctx)
	if err != nil {
		t.Fatalf("reading signature totals: %v", err)
	}
	if signatures["valid"] != 1 || signatures["invalid"] != 0 {
		t.Errorf("signatures = %v, want the request marked valid", signatures)
	}
}

func githubDigest(payload []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
