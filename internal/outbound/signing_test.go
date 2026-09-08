package outbound_test

import (
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/outbound"
)

// The vectors published in docs/signing.md. A receiver in any language should
// reproduce these exactly, so changing what this test expects breaks every
// receiver already written against them.
func TestSignatureVectors(t *testing.T) {
	const (
		secret = "chsec_test"
		unix   = int64(1700000000)
		body   = `{"id":"evt_1"}`
	)

	got := outbound.Signature([][]byte{[]byte(secret)}, unix, []byte(body))
	want := "t=1700000000," +
		"v1=633c7e4aab70fda7cbb8bec33866b5d0708dc9b7019607c7ab8de4835adf588e"

	if got != want {
		t.Errorf("signature\n got %s\nwant %s", got, want)
	}
}

func TestSignatureSignsWithEverySecret(t *testing.T) {
	got := outbound.Signature(
		[][]byte{[]byte("old"), []byte("new")}, 1700000000, []byte("{}"))

	if count := strings.Count(got, "v1="); count != 2 {
		t.Errorf("rotating should offer both digests, got %d in %q", count, got)
	}
	if !strings.HasPrefix(got, "t=1700000000,") {
		t.Errorf("the moment comes first, got %q", got)
	}
}

func TestSignatureIsEmptyWithoutSecrets(t *testing.T) {
	if got := outbound.Signature(nil, 1700000000, []byte("{}")); got != "" {
		t.Errorf("a destination that signs with nothing sends no header, got %q", got)
	}
}

func TestSignatureCoversTheMoment(t *testing.T) {
	first := outbound.Signature([][]byte{[]byte("s")}, 1700000000, []byte("{}"))
	later := outbound.Signature([][]byte{[]byte("s")}, 1700000060, []byte("{}"))

	if first == later {
		t.Error("the same body at a different moment must not sign the same")
	}
}
