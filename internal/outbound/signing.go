package outbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// SignatureHeader carries the proof that a delivery came from here.
const SignatureHeader = "X-Charon-Signature"

// The layout of that header. Charon signs one way and only one way: a format
// a receiver has already written code against cannot be improved without
// breaking every receiver, so there is nothing to configure.
const (
	timestampKey = "t"
	signatureKey = "v1"
)

// Signature is what SignatureHeader holds: the moment of this attempt, then
// one digest per secret the destination is signing with.
//
// The timestamp is inside what is signed, so a captured delivery cannot be
// replayed forever. It belongs to the attempt rather than to the delivery,
// because a retry an hour later carrying the original moment is a delivery the
// receiver is right to refuse.
//
// Rotating means both the old secret and the new one appear here at once. The
// receiver accepts whichever it already knows, so neither side has to change
// at the same instant as the other.
func Signature(secrets [][]byte, unix int64, body []byte) string {
	if len(secrets) == 0 {
		return ""
	}

	moment := strconv.FormatInt(unix, 10)
	signed := append([]byte(moment+"."), body...)

	parts := make([]string, 0, len(secrets)+1)
	parts = append(parts, timestampKey+"="+moment)
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(signed)
		parts = append(parts, signatureKey+"="+hex.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(parts, ",")
}
