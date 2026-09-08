package postgres_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func signingTenant(t *testing.T, store *postgres.Store, slug string) context.Context {
	t.Helper()

	tenant, err := store.CreateTenant(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "stripe", "billing",
		"https://billing.internal/hook", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	return ctx
}

func TestADestinationSignsWithEverySecretAddedToIt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-with-both")

	for _, reference := range []string{"env:FIRST", "file:/run/secrets/second"} {
		if err := store.AddSigningSecret(ctx, "billing", reference); err != nil {
			t.Fatalf("adding %s: %v", reference, err)
		}
	}

	secrets, err := store.SigningSecrets(ctx)
	if err != nil {
		t.Fatalf("reading them back: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("got %d secrets, want 2", len(secrets))
	}
	for _, secret := range secrets {
		if secret.Destination != "billing" {
			t.Errorf("got a secret for %q", secret.Destination)
		}
		if secret.Added.IsZero() {
			t.Error("when a secret was added is what says it is safe to drop the old one")
		}
	}
}

func TestAddingTheSameSecretTwiceChangesNothing(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-once")

	for range 2 {
		if err := store.AddSigningSecret(ctx, "billing", "env:ONCE"); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}

	secrets, err := store.SigningSecrets(ctx)
	if err != nil {
		t.Fatalf("reading them back: %v", err)
	}
	if len(secrets) != 1 {
		t.Errorf("got %d secrets, want 1", len(secrets))
	}
}

func TestRemovingASecretItDoesNotSignWithSaysSo(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-with-nothing")

	err := store.RemoveSigningSecret(ctx, "billing", "env:NEVER_ADDED")
	if !errors.Is(err, postgres.ErrNoSigningSecret) {
		t.Errorf("got %v, want ErrNoSigningSecret", err)
	}
}

func TestSigningASignlessDestinationSaysSo(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-nothing-there")

	err := store.AddSigningSecret(ctx, "not-a-destination", "env:SOMETHING")
	if !errors.Is(err, postgres.ErrNoDestination) {
		t.Errorf("got %v, want ErrNoDestination", err)
	}
}

// A reference that names no location would deliver unsigned or fail every
// attempt, and neither is something to discover in production.
func TestASecretHasToSayWhereItIs(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-from-nowhere")

	for _, reference := range []string{"CHARON_SIGNING", "vault:secret/billing", "file:relative"} {
		if err := store.AddSigningSecret(ctx, "billing", reference); err == nil {
			t.Errorf("%q was accepted", reference)
		}
	}
}

func TestSigningSecretsDoNotCrossTenants(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ours := signingTenant(t, store, "signs-ours")
	theirs := signingTenant(t, store, "signs-theirs")

	if err := store.AddSigningSecret(ours, "billing", "env:OURS"); err != nil {
		t.Fatalf("adding: %v", err)
	}

	secrets, err := store.SigningSecrets(theirs)
	if err != nil {
		t.Fatalf("reading the other tenant's: %v", err)
	}
	if len(secrets) != 0 {
		t.Errorf("another tenant sees %d of our signing secrets", len(secrets))
	}
}

// Where the references have to arrive for any of this to sign anything: on the
// delivery the dispatcher is about to hand over.
func TestAClaimedDeliveryKnowsHowItIsSigned(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-on-claim")

	for _, reference := range []string{"env:FIRST", "env:SECOND"} {
		if err := store.AddSigningSecret(ctx, "billing", reference); err != nil {
			t.Fatalf("adding %s: %v", reference, err)
		}
	}

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"id":"evt_1"}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d deliveries, want 1", len(claimed))
	}

	signing := claimed[0].Signing
	slices.Sort(signing)
	if !slices.Equal(signing, []string{"env:FIRST", "env:SECOND"}) {
		t.Errorf("the delivery signs with %v, want both secrets", signing)
	}
}

func TestThePanelIsToldWhatTheDispatcherFound(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-and-reports")

	if err := store.AddSigningSecret(ctx, "billing", "env:REPORTED"); err != nil {
		t.Fatalf("adding: %v", err)
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("reading the route: %v (%d routes)", err, len(routes))
	}
	destination := routes[0].DestinationID

	before, err := store.SigningFor(ctx, destination)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("got %d secrets, want 1", len(before))
	}
	// Nothing has looked yet, and the panel must not claim either way.
	if before[0].Checked {
		t.Error("a secret nobody has read yet was reported on")
	}

	references, err := store.EveryReference(ctx)
	if err != nil {
		t.Fatalf("reading every reference: %v", err)
	}
	var ours postgres.AReference
	for _, reference := range references {
		if reference.Reference == "env:REPORTED" {
			ours = reference
		}
	}
	if ours.Reference == "" {
		t.Fatal("the reference is not visible to the process that delivers")
	}

	if err := store.RecordReading(ctx, ours, errors.New("REPORTED is not set in this process")); err != nil {
		t.Fatalf("recording: %v", err)
	}

	after, err := store.SigningFor(ctx, destination)
	if err != nil {
		t.Fatalf("reading again: %v", err)
	}
	if !after[0].Checked || after[0].Readable {
		t.Errorf("got checked=%v readable=%v, want checked and not readable",
			after[0].Checked, after[0].Readable)
	}
	if after[0].Detail == "" {
		t.Error("an unreadable secret should say why")
	}
}

func TestADestinationThatSignsWithNothingIsReported(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-nothing-at-all")

	unsigned, err := store.Unsigned(ctx)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(unsigned) != 1 || unsigned[0].Name != "billing" {
		t.Fatalf("got %+v, want billing reported as unsigned", unsigned)
	}

	if err := store.AddSigningSecret(ctx, "billing", "env:NOW_SIGNED"); err != nil {
		t.Fatalf("adding: %v", err)
	}

	unsigned, err = store.Unsigned(ctx)
	if err != nil {
		t.Fatalf("reading again: %v", err)
	}
	if len(unsigned) != 0 {
		t.Errorf("a destination that signs is still reported: %+v", unsigned)
	}
}

// A test has to go out the way a real delivery does, signature and all, which
// is why the panel records one instead of sending it.
func TestATestGoesThroughTheDeliveryPath(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := signingTenant(t, store, "signs-a-test")

	if err := store.AddSigningSecret(ctx, "billing", "env:TESTED"); err != nil {
		t.Fatalf("adding: %v", err)
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("reading the route: %v (%d routes)", err, len(routes))
	}
	if len(routes[0].Signing) != 1 {
		t.Errorf("the panel does not show the route as signed: %+v", routes[0].Signing)
	}

	event, err := store.SendTest(ctx, routes[0].ID)
	if err != nil {
		t.Fatalf("sending a test: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}

	var found bool
	for _, delivery := range claimed {
		if delivery.EventID == event {
			found = true
			if len(delivery.Signing) != 1 || delivery.Signing[0] != "env:TESTED" {
				t.Errorf("the test delivery signs with %v, want env:TESTED", delivery.Signing)
			}
		}
	}
	if !found {
		t.Error("the test event produced no delivery for the dispatcher to take")
	}
}
