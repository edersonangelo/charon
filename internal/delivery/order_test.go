package delivery_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
)

func TestARouteDeliversInTheOrderItsEventsArrivedThroughARetry(t *testing.T) {
	t.Parallel()

	store := migrated(t)
	ctx := context.Background()

	var (
		mu       sync.Mutex
		accepted []string
		refused  bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if string(payload) == `{"n":1}` && !refused {
			refused = true
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accepted = append(accepted, string(payload))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if err := store.AddRoute(ctx, "stripe", "ordered", server.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	want := []string{`{"n":1}`, `{"n":2}`, `{"n":3}`}
	for n, payload := range want {
		recordAt(t, store, "stripe", start.Add(time.Duration(n)*time.Second), payload)
	}

	dispatcher := delivery.New(store, delivery.Config{
		BackoffBase: time.Millisecond,
		BackoffCap:  2 * time.Millisecond,
	})
	deadline := time.Now().Add(5 * time.Second)
	for states(t, store)["delivered"] < int64(len(want)) && time.Now().Before(deadline) {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(accepted, want) {
		t.Errorf("the destination accepted %v, want %v: the first was refused once and the rest waited for it", accepted, want)
	}
}
