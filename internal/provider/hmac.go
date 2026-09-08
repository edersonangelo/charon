package provider

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // a provider that signs with sha1 still has to be read
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const HMAC = "hmac"

// How the signature header is laid out.
const (
	Simple   = "simple"
	Advanced = "advanced"
)

const (
	SHA1   = "sha1"
	SHA256 = "sha256"
	SHA512 = "sha512"
)

const (
	Hex    = "hex"
	Base64 = "base64"
)

func Schemes() []string    { return []string{Simple, Advanced} }
func Algorithms() []string { return []string{SHA1, SHA256, SHA512} }
func Encodings() []string  { return []string{Hex, Base64} }

// proof is what a layout finds in the headers: candidate digests, and the
// timestamp when the scheme signs one.
type proof struct {
	timestamp string
	digests   []string
}

// layout reads the header a provider chose to use.
type layout interface {
	extract(http.Header) (proof, error)
}

// codec undoes the text encoding a provider chose for the digest.
type codec interface {
	decode(string) ([]byte, error)
}

type hmacVerifier struct {
	secret    []byte
	newHash   func() hash.Hash
	layout    layout
	codec     codec
	tolerance time.Duration
}

func buildHMAC(settings Settings) (Verifier, error) {
	newHash, err := algorithm(settings.Algorithm)
	if err != nil {
		return nil, err
	}
	decoder, err := encoding(settings.Encoding)
	if err != nil {
		return nil, err
	}
	reader, err := scheme(settings)
	if err != nil {
		return nil, err
	}

	tolerance := settings.Tolerance
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}

	return hmacVerifier{
		secret:    []byte(settings.Secret),
		newHash:   newHash,
		layout:    reader,
		codec:     decoder,
		tolerance: tolerance,
	}, nil
}

func (v hmacVerifier) Verify(req Request) (State, error) {
	found, err := v.layout.extract(req.Headers)
	if err != nil {
		if len(found.digests) == 0 && found.timestamp == "" {
			return Missing, err
		}
		return Invalid, err
	}

	payload := req.Body
	if found.timestamp != "" {
		seconds, err := strconv.ParseInt(found.timestamp, 10, 64)
		if err != nil {
			return Invalid, ErrMalformed
		}
		if drift := req.Now.Sub(time.Unix(seconds, 0)); drift > v.tolerance || drift < -v.tolerance {
			return Invalid, ErrStale
		}
		payload = append([]byte(found.timestamp+"."), req.Body...)
	}

	mac := hmac.New(v.newHash, v.secret)
	mac.Write(payload)
	expected := mac.Sum(nil)

	for _, digest := range found.digests {
		decoded, err := v.codec.decode(strings.TrimSpace(digest))
		if err != nil {
			continue
		}
		if hmac.Equal(decoded, expected) {
			return Valid, nil
		}
	}
	return Invalid, ErrMismatch
}

// simpleLayout: the header holds one digest, sometimes behind a "name="
// prefix.
//
// Both readings are offered as candidates rather than guessed at, because
// base64 padding is also "=": cutting at the first one would mangle a padded
// digest. Only a candidate that matches the computed signature can pass, so
// offering two costs nothing.
type simpleLayout struct{ header string }

func (l simpleLayout) extract(headers http.Header) (proof, error) {
	given := headers.Get(l.header)
	if given == "" {
		return proof{}, ErrNoProof
	}

	candidates := []string{given}
	if _, after, found := strings.Cut(given, "="); found && after != "" {
		candidates = append(candidates, after)
	}
	return proof{digests: candidates}, nil
}

// advancedLayout: the header holds comma separated pairs, with a timestamp and
// one or more candidate digests. The timestamp is part of what was signed, so
// a captured request cannot be replayed indefinitely.
type advancedLayout struct {
	header       string
	timestampKey string
	signatureKey string
}

func (l advancedLayout) extract(headers http.Header) (proof, error) {
	given := headers.Get(l.header)
	if given == "" {
		return proof{}, ErrNoProof
	}

	var found proof
	for _, part := range strings.Split(given, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case l.timestampKey:
			found.timestamp = value
		case l.signatureKey:
			found.digests = append(found.digests, value)
		}
	}

	if found.timestamp == "" || len(found.digests) == 0 {
		return proof{digests: []string{given}}, ErrMalformed
	}
	return found, nil
}

type hexCodec struct{}

func (hexCodec) decode(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decoding hex: %w", err)
	}
	return decoded, nil
}

type base64Codec struct{}

func (base64Codec) decode(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decoding base64: %w", err)
	}
	return decoded, nil
}

func algorithm(name string) (func() hash.Hash, error) {
	switch name {
	case SHA1:
		return sha1.New, nil //nolint:gosec // the provider chose it
	case "", SHA256:
		return sha256.New, nil
	case SHA512:
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("%w: algorithm %q", ErrUnknownOption, name)
	}
}

func encoding(name string) (codec, error) {
	switch name {
	case "", Hex:
		return hexCodec{}, nil
	case Base64:
		return base64Codec{}, nil
	default:
		return nil, fmt.Errorf("%w: encoding %q", ErrUnknownOption, name)
	}
}

func scheme(settings Settings) (layout, error) {
	header := settings.Header
	if header == "" {
		header = "X-Signature"
	}

	switch settings.Scheme {
	case "", Simple:
		return simpleLayout{header: header}, nil
	case Advanced:
		timestampKey := settings.TimestampKey
		if timestampKey == "" {
			timestampKey = "t"
		}
		signatureKey := settings.SignatureKey
		if signatureKey == "" {
			signatureKey = "v1"
		}
		return advancedLayout{
			header:       header,
			timestampKey: timestampKey,
			signatureKey: signatureKey,
		}, nil
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrUnknownOption, settings.Scheme)
	}
}
