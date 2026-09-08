package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

// The answer has to survive the round trip, or the panel is back to showing a
// status code and nothing else, which is the state this was written to end.
func TestWhatTheDestinationSaidIsReadBack(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "answered-here", "Answered")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}

	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}
	item := claimed[0]

	said := []byte(`{"error":"customer_id is required"}`)
	if err := store.RecordAttempt(ctx, item.Tenant, item.ID, 1, 400, nil,
		outbound.Answer{Body: said, Type: "application/json", Truncated: true},
		"destination answered 400", 120*time.Millisecond); err != nil {
		t.Fatalf("recording the attempt: %v", err)
	}

	attempt := onlyAttempt(ctx, t, store, item.EventID)
	if string(attempt.Answer) != string(said) {
		t.Errorf("read back %q", attempt.Answer)
	}
	if attempt.AnswerType != "application/json" {
		t.Errorf("the type read back as %q", attempt.AnswerType)
	}
	if !attempt.AnswerTruncated {
		t.Error("what was cut does not say so once read back")
	}
}

// A destination that answers with nothing is the ordinary case, and must not
// leave anything behind that the page then has to explain.
func TestAnAttemptWithNoAnswerKeepsNone(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "answered-nothing", "Nothing")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}

	if err := store.RecordAttempt(ctx, claimed[0].Tenant, claimed[0].ID, 1, 204, nil,
		outbound.Answer{}, "", 10*time.Millisecond); err != nil {
		t.Fatalf("recording: %v", err)
	}

	attempt := onlyAttempt(ctx, t, store, claimed[0].EventID)
	if len(attempt.Answer) != 0 || attempt.AnswerType != "" || attempt.AnswerTruncated {
		t.Errorf("an empty answer read back as %q/%q/%v",
			attempt.Answer, attempt.AnswerType, attempt.AnswerTruncated)
	}
}

// A destination is not trusted to keep its content type short.
func TestAnAbsurdContentTypeDoesNotLoseTheAttempt(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	tenant, err := store.CreateTenant(context.Background(), "answered-loudly", "Loud")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	ctx := authz.WithTenant(context.Background(), tenant.ID)

	if err := store.AddRoute(ctx, "erp", "sink", "https://sink.example/h", "http"); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if _, err := store.Record(ctx, inbound.Request{
		Provider:   "erp",
		Path:       "/webhooks/erp",
		ReceivedAt: time.Now().UTC().Truncate(time.Microsecond),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := store.Plan(ctx, 10); err != nil {
		t.Fatalf("planning: %v", err)
	}
	claimed, err := store.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claiming: %v (%d)", err, len(claimed))
	}

	if err := store.RecordAttempt(ctx, claimed[0].Tenant, claimed[0].ID, 1, 200, nil,
		outbound.Answer{Body: []byte("ok"), Type: strings.Repeat("x", 400)},
		"", time.Millisecond); err != nil {
		t.Fatalf("an oversized content type lost the attempt: %v", err)
	}

	if got := len(onlyAttempt(ctx, t, store, claimed[0].EventID).AnswerType); got != 120 {
		t.Errorf("the type was kept at %d characters", got)
	}
}

// onlyAttempt reads back the one attempt these tests record.
func onlyAttempt(
	ctx context.Context, t *testing.T, store *postgres.Store, event uuid.UUID,
) console.Attempt {
	t.Helper()

	deliveries, err := store.EventDeliveries(ctx, event)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("reading the deliveries: %v (%d)", err, len(deliveries))
	}
	if len(deliveries[0].Rounds) == 0 || len(deliveries[0].Rounds[0].Attempts) == 0 {
		t.Fatal("no attempt was recorded")
	}
	return deliveries[0].Rounds[0].Attempts[0]
}
