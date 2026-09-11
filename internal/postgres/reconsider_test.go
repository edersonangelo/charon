package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

func arrived(ctx context.Context, t *testing.T, store *postgres.Store,
	name string, headers map[string][]string, body []byte,
) console.EventSummary {
	t.Helper()

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   name,
		Path:       "/webhooks/" + name,
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    headers,
		Body:       body,
	}); err != nil {
		t.Fatalf("recording for %s: %v", name, err)
	}

	found, err := store.SearchEvents(ctx, console.Filter{Provider: name, PageSize: 1})
	if err != nil || len(found.Events) == 0 {
		t.Fatalf("reading back what arrived for %s: %v", name, err)
	}
	return found.Events[0]
}

func signatureOf(ctx context.Context, t *testing.T, store *postgres.Store, id string) string {
	t.Helper()

	found, err := store.SearchEvents(ctx, console.Filter{Search: id, PageSize: 10})
	if err != nil {
		t.Fatalf("reading the event back: %v", err)
	}
	for _, event := range found.Events {
		if event.ID.String() == id {
			return event.Signature
		}
	}
	t.Fatalf("event %s is gone", id)
	return ""
}

func withToken(token string) map[string][]string {
	return map[string][]string{
		"Content-Type":  {"application/json"},
		"Authorization": {token},
	}
}

// The case a real deployment hits: requests arrive, verification is configured
// afterwards, and everything already recorded is unchecked. Unchecked is the
// absence of a verdict, not one, so configuring the settings is what makes the
// question askable — and nobody should have to ask again by hand.
func TestWhatArrivedBeforeTheSettingsIsCheckedWhenTheyArrive(t *testing.T) {
	// Not parallel: the secret is named by an environment variable, and this
	// test is what sets it.
	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "arrived-before", "Before")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	right := arrived(ctx, t, store, "erp", withToken("Bearer s3cret"), []byte(`{"n":1}`))
	wrong := arrived(ctx, t, store, "erp", withToken("Bearer nope"), []byte(`{"n":2}`))
	none := arrived(ctx, t, store, "erp",
		map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"n":3}`))

	for _, event := range []console.EventSummary{right, wrong, none} {
		if event.Signature != "unchecked" {
			t.Fatalf("before any settings, %s is %q, want unchecked", event.ID, event.Signature)
		}
	}

	// The secret is the bare token: "Bearer " is stripped from what arrives.
	t.Setenv("CHARON_TEST_BEARER", "s3cret")
	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name: "erp", SecretEnv: "CHARON_TEST_BEARER",
		Settings: provider.Settings{Verifier: provider.SharedToken, Header: "Authorization"},
	}); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	recovered, err := store.Reconsider(ctx, 100)
	if err != nil {
		t.Fatalf("reconsidering: %v", err)
	}
	if recovered != 1 {
		t.Errorf("reopened %d requests, want the one that verifies", recovered)
	}

	for id, want := range map[string]string{
		right.ID.String(): "valid",
		wrong.ID.String(): "invalid",
		none.ID.String():  "missing",
	} {
		if got := signatureOf(ctx, t, store, id); got != want {
			t.Errorf("event %s is %q, want %q", id, got, want)
		}
	}
}

// Something already handed over keeps the answer it went out with. Saying now
// that it was never signed would claim it had been held back, and it was not.
func TestWhatWasAlreadyDeliveredKeepsItsAnswer(t *testing.T) {
	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "already-gone", "Gone")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}

	gone := arrived(ctx, t, store, "erp", withToken("Bearer nope"), []byte(`{"n":1}`))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}
	if err := store.MarkDelivered(ctx, claimed[0].Tenant, claimed[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	t.Setenv("CHARON_TEST_BEARER_GONE", "s3cret")
	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name: "erp", SecretEnv: "CHARON_TEST_BEARER_GONE",
		Settings: provider.Settings{Verifier: provider.SharedToken, Header: "Authorization"},
	}); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}

	if _, err := store.Reconsider(ctx, 100); err != nil {
		t.Fatalf("reconsidering: %v", err)
	}

	if got := signatureOf(ctx, t, store, gone.ID.String()); got != "unchecked" {
		t.Errorf("a delivered request is now %q, want it left as unchecked", got)
	}
}
