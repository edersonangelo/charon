package delivery_test

import (
	"context"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
)

// Registering a transport must not require touching the delivery loop.
func TestATransportCanBeAddedWithoutChangingTheLoop(t *testing.T) {
	t.Parallel()

	handed := make(chan outbound.Delivery, 1)
	transports := delivery.DefaultTransports()
	transports.Register("carrier-pigeon", func(delivery.Options) (outbound.Transport, error) {
		return pigeon{handed: handed}, nil
	})

	if !transports.Knows("carrier-pigeon") {
		t.Fatal("the transport was not registered")
	}

	store, dispatcher, _, _, _ := setupOn(t, delivery.Config{
		Transports: transports,
	}, "carrier-pigeon")

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	select {
	case carried := <-handed:
		if carried.Transport != "carrier-pigeon" {
			t.Errorf("transport = %q, want the one on the destination", carried.Transport)
		}
		if string(carried.Body) != string(body) {
			t.Errorf("body = %q, want the recorded bytes", carried.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the transport was never asked to send anything")
	}

	if got := states(t, store)["delivered"]; got != 1 {
		t.Errorf("delivered = %d, want 1", got)
	}
}

// A destination pointing at a transport nobody registered must fail loudly and
// not be retried: another attempt cannot register it.
func TestAnUnknownTransportFailsWithoutRetrying(t *testing.T) {
	t.Parallel()

	store, dispatcher, _, dsn, _ := setupOn(t, delivery.Config{
		MaxAttempts: 12,
	}, "smoke-signals")

	if _, err := dispatcher.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got := states(t, store)
	if got["dead"] != 1 {
		t.Fatalf("states = %v, want it dead at once", got)
	}
	if attempts(t, dsn) != 1 {
		t.Errorf("attempts = %d, want 1: retrying cannot register a transport", attempts(t, dsn))
	}
}

type pigeon struct{ handed chan outbound.Delivery }

func (p pigeon) Send(_ context.Context, item outbound.Delivery) outbound.Result {
	p.handed <- item
	return outbound.Result{Accepted: true, Detail: "carried", Retryable: true}
}
