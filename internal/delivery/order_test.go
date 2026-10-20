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

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
)

func TestARouteCatchingUpDeliversInTheOrderItsEventsArrivedThroughARetry(t *testing.T) {
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

	start := time.Now().Add(-time.Minute)
	want := []string{`{"n":1}`, `{"n":2}`, `{"n":3}`}
	recordAt(t, store, "stripe", start, want[0])
	recordAt(t, store, "stripe", start.Add(time.Second), want[1])
	if err := store.AddRoute(ctx, "stripe", "ordered", server.URL, outbound.HTTP, console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("adding the route: %v", err)
	}
	recordAt(t, store, "stripe", start.Add(2*time.Second), want[2])

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
		t.Errorf("the destination accepted %v, want %v: the first was refused once and the rest, "+
			"recorded before the route and after it, waited for it", accepted, want)
	}
}

func TestARouteWithNothingToCatchUpOnDoesNotHoldWhatIsBehindARetry(t *testing.T) {
	t.Parallel()

	store := migrated(t)
	ctx := context.Background()

	var (
		mu       sync.Mutex
		accepted []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		if string(payload) == `{"n":1}` {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		accepted = append(accepted, string(payload))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if err := store.AddRoute(ctx, "stripe", "side-by-side", server.URL, outbound.HTTP, console.DeliverAlreadyRecorded); err != nil {
		t.Fatalf("adding the route: %v", err)
	}
	start := time.Now().Add(-time.Minute)
	recordAt(t, store, "stripe", start, `{"n":1}`)
	recordAt(t, store, "stripe", start.Add(time.Second), `{"n":2}`)

	dispatcher := delivery.New(store, delivery.Config{BackoffBase: time.Hour, BackoffCap: time.Hour})
	deadline := time.Now().Add(5 * time.Second)
	for states(t, store)["delivered"] < 1 && time.Now().Before(deadline) {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(accepted, []string{`{"n":2}`}) {
		t.Errorf("the destination accepted %v, want the second delivered while the first waits an hour for its retry", accepted)
	}
}
