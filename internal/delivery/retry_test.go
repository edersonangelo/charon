package delivery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func answered(t *testing.T, status int, header map[string]string) outbound.Result {
	t.Helper()

	destination := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			for name, value := range header {
				w.Header().Set(name, value)
			}
			w.WriteHeader(status)
		}))
	t.Cleanup(destination.Close)

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	return transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})
}

// The request never changes between attempts, so an answer about the request
// being wrong is the same answer every time. Retrying it twelve times only
// moves the dead letter hours later and fills the log with what looks like a
// network problem.
func TestARejectionOfTheRequestIsNotRetried(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusUnsupportedMediaType,
	} {
		result := answered(t, status, nil)
		if result.Accepted {
			t.Errorf("%d was taken as delivered", status)
		}
		if result.Retryable {
			t.Errorf("%d is retried, and the same bytes will get the same answer", status)
		}
	}
}

// Three say the request was fine and the moment was not.
func TestWhatTimeCanFixIsRetried(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if result := answered(t, status, nil); !result.Retryable {
			t.Errorf("%d is not retried, and time might well fix it", status)
		}
	}
}

func TestADestinationThatSaysHowLongIsBelieved(t *testing.T) {
	t.Parallel()

	result := answered(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "120"})
	if result.RetryAfter != 2*time.Minute {
		t.Errorf("waits %s, want the two minutes it asked for", result.RetryAfter)
	}
}

func TestADateInRetryAfterIsUnderstood(t *testing.T) {
	t.Parallel()

	when := time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	result := answered(t, http.StatusServiceUnavailable, map[string]string{"Retry-After": when})

	// The header has one-second resolution, so this is about the minute.
	if result.RetryAfter < 80*time.Second || result.RetryAfter > 95*time.Second {
		t.Errorf("waits %s, want about ninety seconds", result.RetryAfter)
	}
}

// Nonsense, a moment already past, or nothing at all leaves the backoff to
// decide rather than producing a wait of zero or a negative one.
func TestAnUnusableRetryAfterIsIgnored(t *testing.T) {
	t.Parallel()

	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	for name, value := range map[string]string{
		"nothing":      "",
		"not a number": "soon",
		"zero":         "0",
		"negative":     "-30",
		"already past": past,
	} {
		t.Run(name, func(t *testing.T) {
			result := answered(t, http.StatusServiceUnavailable,
				map[string]string{"Retry-After": value})
			if result.RetryAfter != 0 {
				t.Errorf("asks to wait %s", result.RetryAfter)
			}
		})
	}
}

// A rejection of the request is settled on the first attempt rather than
// twelve times over the hours the backoff would take to give up.
func TestARejectedRequestIsDeadOnTheFirstAttempt(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, dsn, _ := setup(t, delivery.Config{MaxAttempts: 12})
	target.status.Store(http.StatusUnprocessableEntity)

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := states(t, store); got["dead"] != 1 || got["pending"] != 0 {
		t.Errorf("states = %v, want it dead already", got)
	}
	if got := attempts(t, dsn); got != 1 {
		t.Errorf("attempts = %d, want the one that settled it", got)
	}
}

// And the wait a destination asks for is obeyed only up to the cap the
// deployment set: a receiver answering with a week would otherwise hold a
// delivery for a week.
func TestAnAbsurdRetryAfterIsCappedByTheDeployment(t *testing.T) {
	t.Parallel()

	store, dispatcher, target, _, _ := setup(t, delivery.Config{
		MaxAttempts: 12, BackoffBase: time.Second, BackoffCap: 10 * time.Minute,
	})
	target.status.Store(http.StatusTooManyRequests)
	target.retryAfter.Store(604800) // a week

	before := time.Now().UTC()
	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	next := nextAttemptAt(t, store)
	if next.IsZero() {
		t.Fatal("nothing is waiting to be tried again")
	}
	if wait := next.Sub(before); wait > 11*time.Minute {
		t.Errorf("the next attempt is %s away, past the cap", wait)
	} else if wait < 9*time.Minute {
		t.Errorf("the next attempt is %s away, which ignores what was asked", wait)
	}
}

func nextAttemptAt(t *testing.T, store *postgres.Store) time.Time {
	t.Helper()

	found, err := store.SearchEvents(context.Background(), console.Filter{PageSize: 5})
	if err != nil || len(found) == 0 {
		t.Fatalf("reading the events: %v", err)
	}
	deliveries, err := store.EventDeliveries(context.Background(), found[0].ID)
	if err != nil || len(deliveries) == 0 {
		t.Fatalf("reading the deliveries: %v", err)
	}
	return deliveries[0].NextAttemptAt.UTC()
}
