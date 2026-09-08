package provider_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/provider"
)

var (
	oneTenant     = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	anotherTenant = uuid.MustParse("00000000-0000-0000-0000-000000000002")
)

type fakeSource struct {
	settings map[uuid.UUID]map[string]provider.Settings
	err      error
}

func (f fakeSource) AllVerification(
	context.Context,
) (map[uuid.UUID]map[string]provider.Settings, error) {
	return f.settings, f.err
}

// A closed channel makes Watch load once and return, which is how the load
// this package keeps unexported is driven from a test.
func (f fakeSource) ProviderChanges(context.Context) <-chan struct{} {
	changes := make(chan struct{})
	close(changes)
	return changes
}

func loaded(t *testing.T, source fakeSource) *provider.Cache {
	t.Helper()

	cache := provider.NewCache(source, provider.Default(), time.Minute, nil)
	cache.Watch(t.Context())
	return cache
}

func TestTheCacheHandsOverTheVerifyToken(t *testing.T) {
	t.Parallel()

	cache := loaded(t, fakeSource{settings: map[uuid.UUID]map[string]provider.Settings{
		oneTenant: {
			"whatsapp": {
				Verifier: provider.HMAC, Scheme: provider.Simple,
				Algorithm: provider.SHA256, Encoding: provider.Hex,
				Header: "X-Hub-Signature-256", Secret: secret,
				VerifyToken: "meta-token",
			},
			"stripe": {Verifier: provider.HMAC, Secret: secret, Header: "Stripe-Signature"},
		},
		anotherTenant: {
			"whatsapp": {Verifier: provider.HMAC, Secret: secret, VerifyToken: "another-token"},
		},
	}})

	tests := []struct {
		name   string
		tenant uuid.UUID
		what   string
		want   string
	}{
		{"a provider that confirms its address", oneTenant, "whatsapp", "meta-token"},
		{"the same provider under another tenant", anotherTenant, "whatsapp", "another-token"},
		{"a provider that confirms nothing", oneTenant, "stripe", ""},
		{"a provider nobody configured", oneTenant, "shopify", ""},
		{"a tenant nobody configured", uuid.Nil, "whatsapp", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := cache.VerifyToken(tt.tenant, tt.what); got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
}

// The verifiers and the tokens are loaded together and swapped together, so
// carrying one must not have disturbed the other.
func TestTheVerifiersSurviveCarryingTheTokens(t *testing.T) {
	t.Parallel()

	cache := loaded(t, fakeSource{settings: map[uuid.UUID]map[string]provider.Settings{
		oneTenant: {"whatsapp": {
			Verifier: provider.HMAC, Scheme: provider.Simple,
			Algorithm: provider.SHA256, Encoding: provider.Hex,
			Header: "X-Hub-Signature-256", Secret: secret, VerifyToken: "meta-token",
		}},
	}})

	state, err := cache.Verifier(oneTenant, "whatsapp").Verify(provider.Request{
		Headers: headers("X-Hub-Signature-256",
			"sha256="+digest(t, provider.SHA256, provider.Hex, secret, body)),
		Body: body,
		Now:  time.Now(),
	})
	if err != nil || state != provider.Valid {
		t.Fatalf("state = %q (%v), want %q", state, err, provider.Valid)
	}

	unconfigured, _ := cache.Verifier(oneTenant, "nobody").Verify(provider.Request{Body: body})
	if unconfigured != provider.Unchecked {
		t.Errorf("state = %q, want %q", unconfigured, provider.Unchecked)
	}
}
