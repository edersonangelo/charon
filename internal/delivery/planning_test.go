package delivery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
)

func migrated(t *testing.T) *postgres.Store {
	t.Helper()

	ctx := context.Background()
	store, err := postgres.Open(ctx, testsupport.PostgresDSN(t))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return store
}

func recordAt(t *testing.T, store *postgres.Store, provider string, at time.Time, payload string) uuid.UUID {
	t.Helper()

	id, err := store.Record(context.Background(), inbound.Request{
		Provider:   provider,
		Path:       "/webhooks/" + provider,
		ReceivedAt: at.UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(payload),
	})
	if err != nil {
		t.Fatalf("recording for %s: %v", provider, err)
	}
	return id
}

func TestEventsNoRouteWantsDoNotStallPlanning(t *testing.T) {
	t.Parallel()

	store := migrated(t)
	ctx := context.Background()

	target := &destination{}
	target.status.Store(http.StatusOK)
	server := httptest.NewServer(target.handler())
	t.Cleanup(server.Close)

	if err := store.AddRoute(ctx, "stripe", "test", server.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	const batch = 3
	earlier := time.Now().Add(-time.Hour)
	for i := range batch * 2 {
		recordAt(t, store, "nobody-routes-this", earlier.Add(time.Duration(i)*time.Second), `{}`)
	}
	recordAt(t, store, "stripe", time.Now(), `{}`)

	dispatcher := delivery.New(store, delivery.Config{BatchSize: batch})
	for range 3 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	if got := states(t, store); got["delivered"] != 1 {
		t.Fatalf("states = %v, want the routed event delivered past the ones nobody routes", got)
	}
}
