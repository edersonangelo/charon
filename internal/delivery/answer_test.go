package delivery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
)

func answering(t *testing.T, status int, kind, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if kind != "" {
				w.Header().Set("Content-Type", kind)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(server.Close)
	return server
}

// A status code is usually the whole answer and cannot be relied on to be. A
// rejection that explains itself is the reason this is kept at all.
func TestWhatTheDestinationSaysBackIsKept(t *testing.T) {
	destination := answering(t, http.StatusBadRequest, "application/json",
		`{"error":"customer_id is required"}`)

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})

	if result.Accepted {
		t.Error("a 400 was taken as delivered")
	}
	if string(result.Answer.Body) != `{"error":"customer_id is required"}` {
		t.Errorf("kept %q", result.Answer.Body)
	}
	if result.Answer.Type != "application/json" {
		t.Errorf("the type is %q", result.Answer.Type)
	}
	if result.Answer.Truncated {
		t.Error("a short answer was reported as cut")
	}
}

// It is a diagnostic, not an archive: a destination that answers with
// megabytes does not get to put them on every attempt.
func TestALongAnswerIsCutAndSaysSo(t *testing.T) {
	destination := answering(t, http.StatusOK, "text/plain", strings.Repeat("x", 5000))

	transport := delivery.DefaultTransports().Build(delivery.HTTP,
		delivery.Options{MaxResponseBytes: 100})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})

	if !result.Accepted {
		t.Fatalf("the destination answered %d", result.Status)
	}
	if len(result.Answer.Body) != 100 {
		t.Errorf("kept %d bytes, want the limit", len(result.Answer.Body))
	}
	if !result.Answer.Truncated {
		t.Error("what was cut does not say so")
	}
}

// An answer exactly at the limit is whole, not cut. Off by one here would call
// every full answer truncated.
func TestAnAnswerAtTheLimitIsWhole(t *testing.T) {
	destination := answering(t, http.StatusOK, "text/plain", strings.Repeat("x", 100))

	transport := delivery.DefaultTransports().Build(delivery.HTTP,
		delivery.Options{MaxResponseBytes: 100})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})

	if len(result.Answer.Body) != 100 || result.Answer.Truncated {
		t.Errorf("kept %d bytes, cut=%v", len(result.Answer.Body), result.Answer.Truncated)
	}
}

func TestADestinationThatSaysNothingIsNotAnError(t *testing.T) {
	destination := answering(t, http.StatusNoContent, "", "")

	transport := delivery.DefaultTransports().Build(delivery.HTTP, delivery.Options{})
	result := transport.Send(context.Background(), outbound.Delivery{
		URL: destination.URL, Body: []byte("{}"),
	})

	if !result.Accepted {
		t.Errorf("a 204 was not taken as delivered: %s", result.Detail)
	}
	if len(result.Answer.Body) != 0 {
		t.Errorf("something was kept from an empty answer: %q", result.Answer.Body)
	}
}
