package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func waiting(ctx context.Context, t *testing.T, store *postgres.Store, provider string) {
	t.Helper()

	if err := store.AddRoute(ctx, provider, provider+"-sink",
		"https://sink.example/"+provider, "http"); err != nil {
		t.Fatalf("routing %s: %v", provider, err)
	}
	if _, err := store.Record(ctx, inbound.Request{
		Provider:   provider,
		Path:       "/webhooks/" + provider,
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording for %s: %v", provider, err)
	}
}

// The process that delivers works for every tenant at once, and the database
// answers for one at a time. A sweep that names no tenant is answered for
// whichever one the connection happens to be pinned to and finds nothing for
// the rest — quietly, which is how a deployment ends up with events recorded,
// routes configured, and nothing ever delivered.
func TestTheDispatcherReachesEveryTenant(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	first, err := store.TenantBySlug(ctx, postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	second, err := store.CreateTenant(ctx, "somewhere-else", "Elsewhere")
	if err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}

	waiting(authz.WithTenant(ctx, first.ID), t, store, "here")
	waiting(authz.WithTenant(ctx, second.ID), t, store, "elsewhere")

	// Nothing names a tenant, exactly as the dispatcher runs.
	planned, err := store.Plan(ctx, 50)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if planned != 2 {
		t.Errorf("planned %d deliveries, want one for each tenant", planned)
	}

	claimed, err := store.Claim(ctx, 50, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d deliveries, want one for each tenant", len(claimed))
	}

	seen := map[string]bool{}
	for _, delivery := range claimed {
		seen[delivery.Provider] = true
		if delivery.Tenant == (first.ID) && delivery.Provider != "here" {
			t.Errorf("a delivery carries the wrong tenant")
		}
	}
	for _, provider := range []string{"here", "elsewhere"} {
		if !seen[provider] {
			t.Errorf("nothing was claimed for %q", provider)
		}
	}

	// And the loop has to be told there is something due, or it sleeps out the
	// safety interval while another tenant waits.
	at, found, err := store.NextWorkAt(ctx)
	if err != nil {
		t.Fatalf("reading the next work time: %v", err)
	}
	if !found {
		t.Error("nothing is due, with two deliveries claimed and pending")
	}
	if at.After(time.Now().Add(time.Minute)) {
		t.Errorf("the next work is said to be at %s", at)
	}
}

// What is done about a delivery afterwards is confined to its tenant too, so
// the delivery carries which one it is.
func TestADeliveryIsMarkedInItsOwnTenant(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := context.Background()

	tenant, err := store.CreateTenant(ctx, "marked-elsewhere", "Marked")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	waiting(authz.WithTenant(ctx, tenant.ID), t, store, "marked")

	if _, err := store.Plan(ctx, 50); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 50, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}

	item := claimed[0]
	if item.Tenant != tenant.ID {
		t.Fatalf("the delivery says it belongs to %s, want %s", item.Tenant, tenant.ID)
	}

	if err := store.RecordAttempt(ctx, item.Tenant, item.ID, 1, 200,
		[]string{}, "", time.Second); err != nil {
		t.Fatalf("recording the attempt: %v", err)
	}
	if err := store.MarkDelivered(ctx, item.Tenant, item.ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	states, err := store.DeliveryStates(authz.WithTenant(ctx, tenant.ID))
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if states["delivered"] != 1 {
		t.Errorf("states are %v, want one delivered", states)
	}
}
