package main

import "testing"

// Saving a provider rewrites every column of its row, so what this decides is
// the difference between rotating a secret and switching a handshake off. The
// command that reaches the row is covered by the panel's round trip and by the
// store's own tests; this is the truth table those cannot state.
func TestChosenTokenEnvKeepsWhatNobodyAskedAbout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stored string
		given  string
		clear  bool
		want   string
	}{
		{
			name:   "a command that says nothing keeps what is there",
			stored: "CHARON_VERIFY_WHATSAPP",
			want:   "CHARON_VERIFY_WHATSAPP",
		},
		{
			name:  "a command that says nothing, with nothing there",
			given: "",
			want:  "",
		},
		{
			name:   "a variable given replaces the one there",
			stored: "CHARON_VERIFY_WHATSAPP",
			given:  "CHARON_VERIFY_ROTATED",
			want:   "CHARON_VERIFY_ROTATED",
		},
		{
			name:   "clearing forgets the one there",
			stored: "CHARON_VERIFY_WHATSAPP",
			clear:  true,
			want:   "",
		},
		{
			name:  "clearing what was never there",
			clear: true,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := chosenTokenEnv(tt.stored, tt.given, tt.clear); got != tt.want {
				t.Errorf("variable = %q, want %q", got, tt.want)
			}
		})
	}
}
