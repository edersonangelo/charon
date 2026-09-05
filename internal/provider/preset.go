package provider

import "time"

// A preset is a name for a set of parameters. It adds no behaviour: everything
// it fills in could be typed by hand, and nothing reads the name at runtime.
type Preset struct {
	Name        string
	Description string
	Settings    Settings
}

func Presets() []Preset {
	return []Preset{
		{
			Name:        "stripe",
			Description: "Stripe-Signature with t and v1, signed over the timestamp and the body",
			Settings: Settings{
				Verifier: HMAC, Scheme: Advanced, Algorithm: SHA256, Encoding: Hex,
				Header: "Stripe-Signature", TimestampKey: "t", SignatureKey: "v1",
				Tolerance: 5 * time.Minute,
			},
		},
		{
			Name:        "github",
			Description: "X-Hub-Signature-256 in hex, signed over the body",
			Settings: Settings{
				Verifier: HMAC, Scheme: Simple, Algorithm: SHA256, Encoding: Hex,
				Header: "X-Hub-Signature-256",
			},
		},
		{
			Name:        "shopify",
			Description: "X-Shopify-Hmac-Sha256 in base64, signed over the body",
			Settings: Settings{
				Verifier: HMAC, Scheme: Simple, Algorithm: SHA256, Encoding: Base64,
				Header: "X-Shopify-Hmac-Sha256",
			},
		},
		{
			Name:        "bearer-token",
			Description: "a fixed token in Authorization, for a provider that does not sign",
			Settings:    Settings{Verifier: SharedToken, Header: "Authorization"},
		},
		{
			Name:        "basic-auth",
			Description: "HTTP basic credentials, for a provider that does not sign",
			Settings:    Settings{Verifier: BasicAuth},
		},
	}
}

func PresetByName(name string) (Preset, bool) {
	for _, preset := range Presets() {
		if preset.Name == name {
			return preset, true
		}
	}
	return Preset{}, false
}

func PresetNames() []string {
	names := make([]string, 0, len(Presets()))
	for _, preset := range Presets() {
		names = append(names, preset.Name)
	}
	return names
}
