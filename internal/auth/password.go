package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Password is the sign-in that needs nothing but this deployment: an address
// and a secret compared against a stored hash.
const Password = "password"

// Credentials is how the password method reaches the operators. Declared here,
// by the side that uses it, so this package holds no storage.
type Credentials interface {
	// Verify reports the address behind a correct pair, or refuses.
	Verify(ctx context.Context, email, password string) (string, error)
}

type password struct {
	credentials Credentials
}

// NewPassword builds the local sign-in. It never provisions: an operator has
// to exist before they can present a password.
func NewPassword(credentials Credentials) Method {
	return password{credentials: credentials}
}

func (password) Name() string  { return Password }
func (password) Label() string { return "email and password" }

func (p password) Identify(ctx context.Context, r *http.Request) (Identity, error) {
	if err := r.ParseForm(); err != nil {
		return Identity{}, fmt.Errorf("reading the form: %w", err)
	}

	email := strings.TrimSpace(r.PostForm.Get("email"))
	secret := r.PostForm.Get("password")
	if email == "" || secret == "" {
		return Identity{}, ErrRefused
	}

	verified, err := p.credentials.Verify(ctx, email, secret)
	if err != nil {
		return Identity{}, errors.Join(ErrRefused, err)
	}
	return Identity{Email: verified}, nil
}
