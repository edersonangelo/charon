package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres"
)

// The panel refused these and the command line took them, which is two rules
// for one decision. A destination stored this way is routed to and fails at
// delivery, spending the attempt budget on something that could never work.
func TestAnAddressNothingCanBeDeliveredToIsRefused(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "bad-address", "Bad")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	for name, address := range map[string]string{
		"nothing at all":        "",
		"only spaces":           "   ",
		"not an address":        "banana",
		"a path on its own":     "/relative",
		"a scheme we cannot":    "ftp://host/x",
		"a scheme with no host": "http://",
	} {
		t.Run(name, func(t *testing.T) {
			err := store.AddRoute(ctx, "erp", "sink-"+name, address, "http")
			if !errors.Is(err, postgres.ErrBadDestination) {
				t.Errorf("%q was accepted (%v)", address, err)
			}
		})
	}
}

// What it must not refuse. A trailing slash is a real endpoint, and what comes
// after the host is the receiver's business, not something to second-guess.
func TestAnAddressThatIsFineIsTaken(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "good-address", "Good")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	for index, address := range []string{
		"https://host/webhooks/erp",
		"https://host/webhooks/",
		"https://host",
		"http://host:8080/in",
		"https://host/in?tenant=acme",
	} {
		if err := store.AddRoute(ctx, "erp",
			"sink-"+string(rune('a'+index)), address, "http"); err != nil {
			t.Errorf("%q was refused: %v", address, err)
		}
	}
}

// A transport that is not http addresses its destination its own way, and this
// rule has nothing to say about it.
func TestAnotherTransportIsNotHeldToAnHttpAddress(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "other-transport", "Other")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.Register(ctx, postgres.Kinds{Transports: []string{"carrier-pigeon"}}); err != nil {
		t.Fatalf("registering the transport: %v", err)
	}
	if err := store.AddRoute(ctx, "erp", "roof", "the one with the white tail",
		"carrier-pigeon"); err != nil {
		t.Errorf("a destination of another transport was held to an http address: %v", err)
	}
}
