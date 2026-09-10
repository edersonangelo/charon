package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

// Saving a provider rewrites its whole row, so a field the form did not carry
// would be cleared. The panel therefore edits from what is stored, and this is
// the round trip that proves it: configure a handshake, change one other
// field, and the handshake survives with the parameters it had.
func TestThePanelKeepsWhatItDoesNotAskAgainFor(t *testing.T) {
	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	configured := do(t, client, http.MethodPost, server.URL+"/verification", url.Values{
		"provider":         {"whatsapp"},
		"preset":           {"whatsapp-bussines"},
		"secret-env":       {"CHARON_SECRET_WHATSAPP"},
		"verify-token-env": {"CHARON_VERIFY_WHATSAPP"},
	})
	if configured.status != http.StatusOK {
		t.Fatalf("configuring verification = %d, want %d", configured.status, http.StatusOK)
	}
	if !strings.Contains(configured.body, "CHARON_VERIFY_WHATSAPP") {
		t.Error("the page does not name the variable the token is read from")
	}

	edited := do(t, client, http.MethodGet, server.URL+"/verification?edit=whatsapp", nil)
	if edited.status != http.StatusOK {
		t.Fatalf("opening the provider to edit = %d, want %d", edited.status, http.StatusOK)
	}
	for _, want := range []string{
		`value="whatsapp"`,
		`value="CHARON_SECRET_WHATSAPP"`,
		`value="CHARON_VERIFY_WHATSAPP"`,
		"keep the current parameters",
	} {
		if !strings.Contains(edited.body, want) {
			t.Errorf("the form to edit does not carry %s", want)
		}
	}

	// What the operator posts back from that form, having changed only the
	// secret and left the preset on "keep the current parameters".
	saved := do(t, client, http.MethodPost, server.URL+"/verification", url.Values{
		"provider":         {"whatsapp"},
		"preset":           {""},
		"secret-env":       {"CHARON_SECRET_WHATSAPP_ROTATED"},
		"verify-token-env": {"CHARON_VERIFY_WHATSAPP"},
	})
	if saved.status != http.StatusOK {
		t.Fatalf("saving without a preset = %d, want %d (body %q)",
			saved.status, http.StatusOK, saved.body)
	}

	stored := providerNamed(t, store, "whatsapp")
	if stored.SecretEnv != "CHARON_SECRET_WHATSAPP_ROTATED" {
		t.Errorf("secret variable = %q, want it rotated", stored.SecretEnv)
	}
	if stored.VerifyTokenEnv != "CHARON_VERIFY_WHATSAPP" {
		t.Errorf("verify token variable = %q, want it kept", stored.VerifyTokenEnv)
	}
	if stored.Header != "X-Hub-Signature-256" {
		t.Errorf("header = %q, want the parameters kept", stored.Header)
	}
}

func TestAProviderWithNoParametersYetNeedsAPreset(t *testing.T) {
	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	got := do(t, client, http.MethodPost, server.URL+"/verification", url.Values{
		"provider":   {"whatsapp"},
		"preset":     {""},
		"secret-env": {"CHARON_SECRET_WHATSAPP"},
	})
	if got.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got.status, http.StatusBadRequest)
	}

	settings, err := store.ProviderSettings(scoped(t, store))
	if err != nil {
		t.Fatalf("reading the settings: %v", err)
	}
	if len(settings) != 0 {
		t.Errorf("recorded %d providers, want none", len(settings))
	}
}

func providerNamed(t *testing.T, store *postgres.Store, name string) postgres.ProviderSettings {
	t.Helper()

	settings, err := store.ProviderSettings(scoped(t, store))
	if err != nil {
		t.Fatalf("reading the settings: %v", err)
	}
	for _, item := range settings {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("no provider named %q is configured", name)
	return postgres.ProviderSettings{}
}

func TestOnlyAPresetThatConfirmsAnAddressAsksWhereItsTokenIs(t *testing.T) {
	_, server, client, _ := setup(t)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/verification")

	if !strings.Contains(page, `name="verify-token-env" class="confirms-address"`) {
		t.Error("the field is not tied to the presets that ask for it")
	}

	var confirming, plain []string
	for _, preset := range provider.Presets() {
		marked := strings.Contains(page,
			`value="`+preset.Name+`" title="`+templ.EscapeString(preset.Description)+`" data-confirms`)
		if preset.ConfirmsAddress && !marked {
			confirming = append(confirming, preset.Name)
		}
		if !preset.ConfirmsAddress && marked {
			plain = append(plain, preset.Name)
		}
	}
	if len(confirming) > 0 {
		t.Errorf("presets that confirm an address are not marked: %v", confirming)
	}
	if len(plain) > 0 {
		t.Errorf("presets that confirm nothing are marked as if they did: %v", plain)
	}
}
