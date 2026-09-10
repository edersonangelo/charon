// Package provider decides whether an inbound request really came from the
// provider it claims to be.
//
// Nothing here knows a vendor's name. A vendor is a set of parameters, and a
// preset is a name for such a set. Adding support for one is registration or
// configuration, never a new branch.
package provider

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

type State string

const (
	// Unchecked: nothing is configured for the provider.
	Unchecked State = "unchecked"
	// Valid: the proof held.
	Valid State = "valid"
	// Invalid: a proof was offered and did not hold.
	Invalid State = "invalid"
	// Missing: a proof was required and none was offered.
	Missing State = "missing"
)

// Settings are the parameters as an operator stores them.
type Settings struct {
	Verifier string
	Secret   string
	// VerifyToken is what a provider that confirms its callback address before
	// sending anything offers when it does. Resolved, like Secret, so a preset
	// must never set either: a preset is parameters, and these are credentials.
	VerifyToken string
	// VerifyTokenNamed is whether a variable was named for that token, which
	// is a fact about the row and not its location: it names nothing and
	// resolves nothing. A resolved token cannot tell a provider that asked for
	// no confirmation from one whose variable is not set in this process, and
	// those two are owed different answers.
	VerifyTokenNamed bool
	Header           string
	Scheme           string
	Algorithm        string
	Encoding         string
	TimestampKey     string
	SignatureKey     string
	Tolerance        time.Duration
}

// Request is what a verifier is allowed to look at. The body is the exact
// bytes received, because that is what was signed.
type Request struct {
	Headers http.Header
	Body    []byte
	Now     time.Time
}

// Verifier is one way of proving a request's origin.
type Verifier interface {
	Verify(Request) (State, error)
}

// Build turns settings into a verifier, or fails if they do not describe one.
type Build func(Settings) (Verifier, error)

var (
	ErrNoProof       = errors.New("the request carried no proof of origin")
	ErrMalformed     = errors.New("the proof is malformed")
	ErrMismatch      = errors.New("the proof does not match")
	ErrStale         = errors.New("the proof is outside the tolerance window")
	ErrNoSecret      = errors.New("no secret is configured")
	ErrUnknownKind   = errors.New("unknown verifier")
	ErrUnknownOption = errors.New("unknown option")
)

// Registry maps a kind of proof to the code that builds it. Supporting a new
// kind is a call to Register, not an edit to a conditional.
type Registry struct {
	builders map[string]Build
}

func NewRegistry() *Registry {
	return &Registry{builders: map[string]Build{}}
}

func (r *Registry) Register(kind string, build Build) {
	r.builders[kind] = build
}

func (r *Registry) Kinds() []string {
	kinds := make([]string, 0, len(r.builders))
	for kind := range r.builders {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

func (r *Registry) Knows(kind string) bool {
	_, known := r.builders[kind]
	return known
}

// Verifier for these settings. Empty settings produce a verifier that checks
// nothing, so a caller never has to test for absence.
func (r *Registry) Verifier(settings Settings) (Verifier, error) {
	if settings.Verifier == "" {
		return Unverified{}, nil
	}

	// Every failure below returns a verifier that refuses. Returning one that
	// checks nothing would make a caller who ignores the error fail open, and
	// a misconfiguration is not permission to skip checking.
	build, known := r.builders[settings.Verifier]
	if !known {
		unknown := fmt.Errorf("%w: %q", ErrUnknownKind, settings.Verifier)
		return alwaysInvalid{reason: unknown}, unknown
	}
	if settings.Secret == "" {
		return alwaysInvalid{reason: ErrNoSecret}, nil
	}

	verifier, err := build(settings)
	if err != nil {
		return alwaysInvalid{reason: err}, err
	}
	return verifier, nil
}

// Unverified stands for a provider nobody configured. It is a verifier so that
// the inbound port has one path, not two.
type Unverified struct{}

func (Unverified) Verify(Request) (State, error) { return Unchecked, nil }

// alwaysInvalid answers for settings that cannot verify anything, such as a
// secret that is named but absent from the environment. It refuses rather than
// silently passing.
type alwaysInvalid struct{ reason error }

func (a alwaysInvalid) Verify(Request) (State, error) { return Invalid, a.reason }
