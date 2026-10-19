package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func inTenant(t *testing.T, store *postgres.Store, slug string) context.Context {
	t.Helper()

	tenant, err := store.CreateTenant(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	return authz.WithTenant(context.Background(), tenant.ID)
}

func arrivedAt(ctx context.Context, t *testing.T, store *postgres.Store, provider, body string, at time.Time) {
	t.Helper()

	if _, err := store.Record(ctx, inbound.Request{
		Provider:   provider,
		Path:       "/webhooks/" + provider,
		ReceivedAt: at.UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(body),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
}

func bodies(claimed []outbound.Delivery) []string {
	got := make([]string, 0, len(claimed))
	for _, delivery := range claimed {
		got = append(got, string(delivery.Body))
	}
	return got
}

func TestOnlyTheFirstWaitingDeliveryOfARouteIsClaimed(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "in-order")

	for _, provider := range []string{"erp", "crm"} {
		if err := store.AddRoute(ctx, provider, "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
			t.Fatalf("routing %s: %v", provider, err)
		}
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"erp":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"erp":2}`, start.Add(time.Second))
	arrivedAt(ctx, t, store, "crm", `{"crm":1}`, start.Add(2*time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	got := map[string]bool{}
	for _, body := range bodies(claimed) {
		got[body] = true
	}
	if len(claimed) != 2 || !got[`{"erp":1}`] || !got[`{"crm":1}`] {
		t.Fatalf("claimed %v, want the first of each route and nothing behind it", bodies(claimed))
	}

	again, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming while the first is in flight: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("claimed %v while the one ahead was still in flight", bodies(again))
	}

	for _, delivery := range claimed {
		if delivery.Provider == "erp" {
			if err := store.MarkDelivered(ctx, delivery.Tenant, delivery.ID, 200); err != nil {
				t.Fatalf("marking delivered: %v", err)
			}
		}
	}
	next, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after the first was delivered: %v", err)
	}
	if len(next) != 1 || string(next[0].Body) != `{"erp":2}` {
		t.Errorf("claimed %v, want the second erp event once the first was delivered", bodies(next))
	}
}

func TestOneWaitingOutARetryHoldsTheOnesBehindIt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "held")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%v)", err, bodies(claimed))
	}
	if err := store.MarkFailed(ctx, claimed[0].Tenant, claimed[0].ID,
		time.Now().Add(time.Hour), 503, "destination answered 503", 12); err != nil {
		t.Fatalf("failing: %v", err)
	}

	behind, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming during the retry: %v", err)
	}
	if len(behind) != 0 {
		t.Errorf("claimed %v while the one ahead was waiting out its retry", bodies(behind))
	}
}

func TestADeadDeliveryLetsTheNextOneGo(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "dead-ahead")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%v)", err, bodies(claimed))
	}
	if err := store.MarkFailed(ctx, claimed[0].Tenant, claimed[0].ID,
		time.Now(), 400, "destination answered 400", 1); err != nil {
		t.Fatalf("failing for good: %v", err)
	}

	next, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after the dead one: %v", err)
	}
	if len(next) != 1 || string(next[0].Body) != `{"n":2}` {
		t.Errorf("claimed %v, want the next one once the first was dead", bodies(next))
	}
}

func TestAnEarlierEventNotYetPlannedHoldsTheRoute(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "late-plan")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	refused, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Signature:  "invalid",
		Body:       []byte(`{"n":1}`),
	})
	if err != nil {
		t.Fatalf("recording the refused one: %v", err)
	}
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, time.Now())
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	// Overruling the refusal makes the first one deliverable again, and
	// unplanned until the next round plans it.
	who := operatorID(context.Background(), t, store, "late-plan@example.com")
	if _, err := store.Force(ctx, who, "checked with the sender, it is genuine", []uuid.UUID{refused}); err != nil {
		t.Fatalf("forcing: %v", err)
	}

	held, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("claimed %v ahead of an earlier event nobody had planned yet", bodies(held))
	}

	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning the earlier one: %v", err)
	}
	first, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming once both are planned: %v", err)
	}
	if len(first) != 1 || string(first[0].Body) != `{"n":1}` {
		t.Errorf("claimed %v, want the one that arrived first", bodies(first))
	}
}

func TestTwoDispatchersNeverTakeTwoFromOneRoute(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "side-by-side")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	for n := range 5 {
		arrivedAt(ctx, t, store, "erp", `{}`, start.Add(time.Duration(n)*time.Second))
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	for range 4 {
		wait.Go(func() {
			taken, err := store.Claim(ctx, 10, time.Minute)
			if err != nil {
				t.Errorf("claiming: %v", err)
				return
			}
			mu.Lock()
			claimed += len(taken)
			mu.Unlock()
		})
	}
	wait.Wait()

	if claimed != 1 {
		t.Errorf("claimed %d at once from one route, want 1", claimed)
	}
}

func TestTheDispatcherSleepsWhileTheFirstOfARouteWaitsOutItsRetry(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "sleeps")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%v)", err, bodies(claimed))
	}
	retryAt := time.Now().Add(time.Hour)
	if err := store.MarkFailed(ctx, claimed[0].Tenant, claimed[0].ID,
		retryAt, 503, "destination answered 503", 12); err != nil {
		t.Fatalf("failing: %v", err)
	}

	at, found, err := store.NextWorkAt(ctx)
	if err != nil {
		t.Fatalf("reading the next work time: %v", err)
	}
	if !found || at.Before(retryAt.Add(-time.Second)) {
		t.Errorf("next work at %s (found %v), want the retry at %s: nothing behind it can go before",
			at, found, retryAt)
	}
}

func TestANewRouteDoesNotHoldUpTheProvidersOtherRoutes(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "side-routes")

	if err := store.AddRoute(ctx, "erp", "first", "https://first.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing to the first: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	delivered, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(delivered) != 1 {
		t.Fatalf("claiming: %v (%v)", err, bodies(delivered))
	}
	if err := store.MarkDelivered(ctx, delivered[0].Tenant, delivered[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	if err := store.AddRoute(ctx, "erp", "second", "https://second.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing to the second: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming after the second route: %v", err)
	}
	got := map[string]string{}
	for _, delivery := range claimed {
		got[delivery.URL] = string(delivery.Body)
	}
	if got["https://first.example/h"] != `{"n":2}` {
		t.Errorf("the first route claimed %q, want its next event without waiting on the second route's past",
			got["https://first.example/h"])
	}
	if got["https://second.example/h"] != `{"n":1}` {
		t.Errorf("the second route claimed %q, want the first event it was given", got["https://second.example/h"])
	}
}

func TestRoutingAProviderToADestinationItAlreadyReachesIsRefused(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "routed-twice")

	arrivedAt(ctx, t, store, "erp", `{"n":1}`, time.Now())
	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.SkipAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/other", "http",
		console.DeliverAlreadyRecorded); err == nil {
		t.Error("routing erp to sink a second time succeeded, and its choice was ignored")
	}

	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %v: the route that skips the past delivered it", bodies(claimed))
	}
}

func TestAReplayWaitsForWhatIsAlreadyInFlightOnItsRoute(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := inTenant(t, store, "replay-waits")

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http", console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("routing: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	arrivedAt(ctx, t, store, "erp", `{"n":1}`, start)
	arrivedAt(ctx, t, store, "erp", `{"n":2}`, start.Add(time.Second))
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	first, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claiming the first: %v (%v)", err, bodies(first))
	}
	if err := store.MarkDelivered(ctx, first[0].Tenant, first[0].ID, 200); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}
	inFlight, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(inFlight) != 1 {
		t.Fatalf("claiming the second: %v (%v)", err, bodies(inFlight))
	}

	if replayed, err := store.ReplayDelivery(ctx, first[0].ID); err != nil || replayed != 1 {
		t.Fatalf("replaying the first: %v (%d)", err, replayed)
	}

	alongside, err := store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claiming during the flight: %v", err)
	}
	if len(alongside) != 0 {
		t.Errorf("claimed %v while the second was still in flight on the same route", bodies(alongside))
	}
}
