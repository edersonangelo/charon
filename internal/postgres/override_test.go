package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

func refusedEvents(ctx context.Context, t *testing.T, store *postgres.Store) []console.EventSummary {
	t.Helper()

	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name: "erp", SecretEnv: "CHARON_TEST_FORCED",
		Settings: provider.Settings{Verifier: provider.SharedToken, Header: "Authorization"},
	}); err != nil {
		t.Fatalf("configuring verification: %v", err)
	}
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}

	for _, body := range []string{`{"n":1}`, `{"n":2}`} {
		if _, err := store.Record(ctx, inbound.Request{
			Provider:   "erp",
			Path:       "/webhooks/erp",
			ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
			Headers:    map[string][]string{"Authorization": {"Bearer wrong"}},
			Body:       []byte(body),
		}); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	if _, err := store.Reconsider(ctx, 100); err != nil {
		t.Fatalf("reconsidering: %v", err)
	}

	found, err := store.SearchEvents(ctx, console.Filter{Provider: "erp", PageSize: 10})
	if err != nil {
		t.Fatalf("reading them back: %v", err)
	}
	return found.Events
}

func operatorID(ctx context.Context, t *testing.T, store *postgres.Store, email string) uuid.UUID {
	t.Helper()

	tenant, err := store.TenantBySlug(ctx, postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	if err := store.CreateUser(ctx, email, "x",
		console.Placement{Tenant: tenant.ID, Role: authz.Admin}); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}
	user, err := store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("reading the operator: %v", err)
	}
	return user.ID
}

// The whole point: something that failed can be delivered, but only by
// somebody who says why, and the verdict is not laundered in the process.
func TestForcingRecordsWhoDecidedAndWhy(t *testing.T) {
	t.Setenv("CHARON_TEST_FORCED", "s3cret")

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "forced-here", "Forced")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)
	who := operatorID(context.Background(), t, store, "decider@example.com")

	refused := refusedEvents(ctx, t, store)
	if len(refused) != 2 {
		t.Fatalf("got %d refused events, want 2", len(refused))
	}
	for _, event := range refused {
		if event.Signature != "invalid" {
			t.Fatalf("event is %q, want invalid", event.Signature)
		}
		if event.Forced {
			t.Fatal("an event is already marked forced")
		}
	}

	// One reason covers the batch, which is how an operator actually works.
	forced, err := store.Force(ctx, who, "the sender rotated its token without telling us",
		[]uuid.UUID{refused[0].ID, refused[1].ID})
	if err != nil {
		t.Fatalf("forcing: %v", err)
	}
	if forced != 2 {
		t.Errorf("forced %d, want 2", forced)
	}

	for _, want := range refused {
		detail, err := store.EventDetail(ctx, want.ID)
		if err != nil {
			t.Fatalf("reading %s: %v", want.ID, err)
		}
		// The verdict is never rewritten. That is the property.
		if detail.Signature != "invalid" {
			t.Errorf("signature is now %q, want it left as invalid", detail.Signature)
		}
		if detail.Overruled == nil {
			t.Fatalf("%s does not say who let it through", want.ID)
		}
		if !strings.Contains(detail.Overruled.Reason, "rotated its token") {
			t.Errorf("the reason is %q", detail.Overruled.Reason)
		}
		if detail.Overruled.DecidedBy != "decider@example.com" {
			t.Errorf("decided by %q", detail.Overruled.DecidedBy)
		}
	}
}

func TestSomethingForcedIsThenDelivered(t *testing.T) {
	t.Setenv("CHARON_TEST_FORCED", "s3cret")

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "forced-out", "Out")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)
	who := operatorID(context.Background(), t, store, "sender@example.com")

	refused := refusedEvents(ctx, t, store)

	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("a refused event was queued for delivery before anybody said to")
	}

	if _, err := store.Force(ctx, who, "checked with the sender, these are genuine",
		[]uuid.UUID{refused[0].ID}); err != nil {
		t.Fatalf("forcing: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning again: %v", err)
	}

	claimed, err = store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming again: %v", err)
	}
	if len(claimed) != 1 || claimed[0].EventID != refused[0].ID {
		t.Errorf("claimed %d deliveries, want the one that was forced", len(claimed))
	}
}

func TestAReasonIsRequiredAndHasToBeOne(t *testing.T) {
	t.Setenv("CHARON_TEST_FORCED", "s3cret")

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "forced-badly", "Badly")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)
	who := operatorID(context.Background(), t, store, "lazy@example.com")

	refused := refusedEvents(ctx, t, store)

	for _, reason := range []string{"", "   ", "ok", "porque sim"[:5]} {
		if _, err := store.Force(ctx, who, reason, []uuid.UUID{refused[0].ID}); !errors.Is(err, postgres.ErrNoReason) {
			t.Errorf("reason %q gave %v, want ErrNoReason", reason, err)
		}
	}

	detail, err := store.EventDetail(ctx, refused[0].ID)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if detail.Overruled != nil {
		t.Error("an event was let through without a reason")
	}
}

// Nothing that passed, and nothing that was never checked, is on offer: there
// is no refusal to overrule.
func TestOnlyARefusalCanBeOverruled(t *testing.T) {
	t.Setenv("CHARON_TEST_FORCED", "s3cret")

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "forced-nothing", "Nothing")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)
	who := operatorID(context.Background(), t, store, "eager@example.com")

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "nothing-configured",
		Path:       "/webhooks/nothing-configured",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	found, err := store.SearchEvents(ctx, console.Filter{Provider: "nothing-configured", PageSize: 1})
	if err != nil || len(found.Events) != 1 {
		t.Fatalf("reading it back: %v", err)
	}

	_, err = store.Force(ctx, who, "I would like this one to go anyway", []uuid.UUID{found.Events[0].ID})
	if !errors.Is(err, postgres.ErrNothingToForce) {
		t.Errorf("got %v, want ErrNothingToForce", err)
	}
}
