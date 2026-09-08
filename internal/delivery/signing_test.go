package delivery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/provider"
)

func received(t *testing.T, sent http.Header, body []byte, secret string) provider.State {
	t.Helper()

	preset, _ := provider.PresetByName("charon")
	settings := preset.Settings
	settings.Secret = secret

	verifier, err := provider.Default().Verifier(settings)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}

	state, _ := verifier.Verify(provider.Request{
		Headers: sent, Body: body, Now: time.Now(),
	})
	return state
}

func TestADeliveryIsSignedWithWhatTheDestinationSignsWith(t *testing.T) {
	t.Setenv("CHARON_TEST_SIGNING", "s3cret")

	body := []byte(`{"id":"evt_1"}`)

	var got http.Header
	destination := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
	defer destination.Close()

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL:     destination.URL,
		Body:    body,
		Signing: []string{"env:CHARON_TEST_SIGNING"},
	})

	if !result.Accepted {
		t.Fatalf("the destination answered %d: %s", result.Status, result.Detail)
	}
	if state := received(t, got, body, "s3cret"); state != provider.Valid {
		t.Errorf("the destination could not verify what it received: %s", state)
	}
}

func TestADestinationThatSignsWithNothingGetsNoHeader(t *testing.T) {
	var got http.Header
	destination := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
	defer destination.Close()

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})

	if signature := got.Get(outbound.SignatureHeader); signature != "" {
		t.Errorf("an unsigned destination received %s: %q",
			outbound.SignatureHeader, signature)
	}
}

// A secret that cannot be read is a misconfiguration, and delivering unsigned
// to a receiver that has started checking is the outage signing exists to
// prevent. Nothing reaches the destination.
func TestASecretThatCannotBeReadStopsTheDelivery(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
	defer destination.Close()

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL:     destination.URL,
		Body:    []byte("{}"),
		Signing: []string{"env:CHARON_TEST_ABSENT"},
	})

	if reached {
		t.Error("the delivery went out unsigned")
	}
	if result.Accepted {
		t.Error("the attempt was counted as delivered")
	}
	// Retryable, because a deploy is what fixes it and the delivery should
	// still be waiting when that happens.
	if !result.Retryable {
		t.Error("the delivery was given up on instead of waiting for the secret")
	}
	if !strings.Contains(result.Detail, "CHARON_TEST_ABSENT") {
		t.Errorf("the attempt should record what was missing, got %q", result.Detail)
	}
}
