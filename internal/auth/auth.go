// Package auth decides who an operator is. It does not decide whether they are
// allowed in: that is a policy applied once, over whatever a method reports.
package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// Identity is what a method could establish about whoever is signing in.
type Identity struct {
	// Subject is stable at the source and is what an account is linked by.
	// A method that has no such notion leaves it empty.
	Subject string
	Email   string
	// Claims are the values the source sent about what this identity belongs
	// to. What they are called at the provider — groups, roles, scopes — is
	// the provider's business; here they are values, and a value that is the
	// name of a tenant places the identity in it.
	Claims []string
	// EmailUnverified is what the source said about the address, not what to
	// do about it. A method with no such notion leaves it false.
	EmailUnverified bool
}

// Method establishes an identity from a request. One implementation per way of
// signing in.
type Method interface {
	// Name appears in urls and in configuration.
	Name() string
	// Label appears on the sign-in page.
	Label() string
	// Identify reads the request and reports who is signing in, or refuses.
	Identify(ctx context.Context, r *http.Request) (Identity, error)
}

// Redirector is a method that begins by sending the operator somewhere else
// and finishes when they come back. Identify then reads the return leg.
type Redirector interface {
	Method
	Start(w http.ResponseWriter, r *http.Request) error
}

var (
	ErrRefused     = errors.New("the credentials were refused")
	ErrUnavailable = errors.New("the method is not reachable right now")
	ErrNoMethod    = errors.New("no such sign-in method")
)

// Registry holds the ways of signing in that a deployment offers, in the order
// they were added, which is the order the sign-in page shows them.
type Registry struct {
	order  []string
	byName map[string]Method
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]Method{}}
}

func (r *Registry) Register(method Method) {
	if _, already := r.byName[method.Name()]; !already {
		r.order = append(r.order, method.Name())
	}
	r.byName[method.Name()] = method
}

func (r *Registry) Method(name string) (Method, bool) {
	method, found := r.byName[name]
	return method, found
}

func (r *Registry) Methods() []Method {
	methods := make([]Method, 0, len(r.order))
	for _, name := range r.order {
		methods = append(methods, r.byName[name])
	}
	return methods
}

func (r *Registry) Names() []string { return slices.Clone(r.order) }

func (r *Registry) Empty() bool { return len(r.order) == 0 }

// Policy is who is let in once a method has said who they are. It is applied
// to every method, so a rule cannot be enforced for one way of signing in and
// forgotten for another.
type Policy struct {
	// RequiredClaim admits only identities carrying this value, whatever the
	// provider calls the claim it arrives in.
	RequiredClaim string

	// AcceptUnverifiedEmail admits an identity whose provider says the address
	// was never verified. It matters where addresses are self-asserted, and it
	// does not where accounts are created by an administrator, which is why
	// the deployment decides.
	AcceptUnverifiedEmail bool

	// AutoProvision creates an operator on first sign-in instead of requiring
	// one to exist already.
	AutoProvision bool
}

func (p Policy) Admits(identity Identity) error {
	if identity.Email == "" {
		return errors.New("the method established no address")
	}
	if identity.EmailUnverified && !p.AcceptUnverifiedEmail {
		return errors.New("the address is not verified at the provider")
	}
	if p.RequiredClaim != "" && !slices.Contains(identity.Claims, p.RequiredClaim) {
		return errors.New("the account is not in " + p.RequiredClaim)
	}
	return nil
}
