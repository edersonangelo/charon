package postgres_test

import (
	"context"
	"testing"

	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

// t.Setenv rules out t.Parallel, and the token has to come from the
// environment because that is where the real one comes from.
func TestAVerifyTokenIsResolvedFromTheEnvironment(t *testing.T) {
	store, _ := open(t)
	ctx := context.Background()

	t.Setenv("CHARON_TEST_SECRET", "the-app-secret")
	t.Setenv("CHARON_TEST_VERIFY_TOKEN", "meta-token")

	preset, found := provider.PresetByName("whatsapp-bussines")
	if !found {
		t.Fatal("no whatsapp preset")
	}
	settings := postgres.ProviderSettings{
		Name:           "whatsapp",
		SecretEnv:      "CHARON_TEST_SECRET",
		VerifyTokenEnv: "CHARON_TEST_VERIFY_TOKEN",
		Settings:       preset.Settings,
	}
	if err := store.SetProvider(ctx, settings); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	resolved, err := store.Verification(ctx)
	if err != nil {
		t.Fatalf("reading verification settings: %v", err)
	}
	if got := resolved["whatsapp"].VerifyToken; got != "meta-token" {
		t.Errorf("token = %q, want %q", got, "meta-token")
	}

	shown, err := store.ProviderSettings(ctx)
	if err != nil {
		t.Fatalf("reading the settings to show: %v", err)
	}
	if len(shown) != 1 {
		t.Fatalf("read %d providers, want 1", len(shown))
	}
	if got := shown[0].VerifyTokenEnv; got != "CHARON_TEST_VERIFY_TOKEN" {
		t.Errorf("variable = %q, want %q", got, "CHARON_TEST_VERIFY_TOKEN")
	}
	if !shown[0].VerifyTokenPresent {
		t.Error("a variable that is set here was reported absent")
	}
	// The settings a panel is shown carry no credential, the way they already
	// carry no secret.
	if got := shown[0].VerifyToken; got != "" {
		t.Errorf("the settings shown carry the token %q", got)
	}
}

func TestAVerifyTokenIsClearedWhenItIsSavedAway(t *testing.T) {
	store, _ := open(t)
	ctx := context.Background()

	t.Setenv("CHARON_TEST_VERIFY_TOKEN", "meta-token")

	preset, _ := provider.PresetByName("whatsapp-bussines")
	settings := postgres.ProviderSettings{
		Name:           "whatsapp",
		SecretEnv:      "CHARON_TEST_SECRET",
		VerifyTokenEnv: "CHARON_TEST_VERIFY_TOKEN",
		Settings:       preset.Settings,
	}
	if err := store.SetProvider(ctx, settings); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	settings.VerifyTokenEnv = ""
	if err := store.SetProvider(ctx, settings); err != nil {
		t.Fatalf("saving verification again: %v", err)
	}

	resolved, err := store.Verification(ctx)
	if err != nil {
		t.Fatalf("reading verification settings: %v", err)
	}
	if got := resolved["whatsapp"].VerifyToken; got != "" {
		t.Errorf("token = %q, want it cleared", got)
	}

	shown, err := store.ProviderSettings(ctx)
	if err != nil {
		t.Fatalf("reading the settings to show: %v", err)
	}
	if shown[0].VerifyTokenPresent {
		t.Error("a provider with no variable named was reported as having one set")
	}
}

// One process answers handshakes for every tenant, so a token has to arrive
// under the tenant that configured it and under no other.
func TestEveryTenantsVerifyTokenIsLoadedUnderItsOwnTenant(t *testing.T) {
	store, _ := open(t)
	ctx := context.Background()

	t.Setenv("CHARON_TEST_VERIFY_TOKEN", "meta-token")

	preset, _ := provider.PresetByName("whatsapp-bussines")
	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name:           "whatsapp",
		SecretEnv:      "CHARON_TEST_SECRET",
		VerifyTokenEnv: "CHARON_TEST_VERIFY_TOKEN",
		Settings:       preset.Settings,
	}); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	tenants, err := store.Tenants(ctx)
	if err != nil {
		t.Fatalf("reading the tenants: %v", err)
	}
	if len(tenants) != 1 {
		t.Fatalf("read %d tenants, want 1", len(tenants))
	}

	byTenant, err := store.AllVerification(ctx)
	if err != nil {
		t.Fatalf("reading every tenant's settings: %v", err)
	}
	if got := byTenant[tenants[0].ID]["whatsapp"].VerifyToken; got != "meta-token" {
		t.Errorf("token = %q, want %q", got, "meta-token")
	}
}
