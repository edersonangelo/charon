package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func operatorNamed(t *testing.T, store *postgres.Store, address string) uuid.UUID {
	t.Helper()

	operators, err := store.Operators(context.Background())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	for _, operator := range operators {
		if operator.Email == address {
			return operator.ID
		}
	}
	t.Fatalf("no operator is %s", address)
	return uuid.Nil
}

// A zone fourteen hours ahead with no daylight saving, so the reading it
// produces cannot be confused with the recorded instant by accident.
const farAhead = "Pacific/Kiritimati"

// arrived is a fixed instant, so what the panel renders is a fact and not the
// time the test happened to run.
var arrived = time.Date(2026, 3, 1, 23, 30, 0, 0, time.UTC)

func recordAt(t *testing.T, store *postgres.Store, provider string, at time.Time) {
	t.Helper()

	if _, err := store.Record(context.Background(), inbound.Request{
		Provider:   provider,
		Path:       "/webhooks/" + provider,
		ReceivedAt: at,
		Body:       body,
	}); err != nil {
		t.Fatalf("recording an event for %s: %v", provider, err)
	}
}

func setZone(t *testing.T, server *httptest.Server, client *http.Client, name string) response {
	t.Helper()
	return do(t, client, http.MethodPost, server.URL+"/profile", url.Values{"time_zone": {name}})
}

func TestAnInstantIsShownInTheZoneTheReaderChose(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordAt(t, store, "acme", arrived)
	signIn(t, server, client, password)

	_, before := get(t, client, server.URL+"/events")
	if !strings.Contains(before, "2026-03-01 23:30:00") {
		t.Fatalf("without a zone the panel does not show the instant in UTC:\n%s", before)
	}

	if got := setZone(t, server, client, farAhead); got.status != http.StatusOK {
		t.Fatalf("saving the zone = %d", got.status)
	}

	_, after := get(t, client, server.URL+"/events")
	if !strings.Contains(after, "2026-03-02 13:30:00") {
		t.Errorf("the instant is not read in %s:\n%s", farAhead, after)
	}
	if strings.Contains(after, "2026-03-01 23:30:00") {
		t.Errorf("the instant is still read in UTC after choosing %s", farAhead)
	}
	if !strings.Contains(after, farAhead) {
		t.Errorf("the page does not say which zone it is showing")
	}
}

// The recorded moment does not move, whoever is reading it.
func TestChoosingAZoneChangesNothingThatWasRecorded(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordAt(t, store, "acme", arrived)
	signIn(t, server, client, password)
	setZone(t, server, client, farAhead)

	events, err := store.SearchEvents(context.Background(), console.Filter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}

	for _, event := range events {
		if event.Provider == "acme" && !event.ReceivedAt.Equal(arrived) {
			t.Errorf("received at %s, want %s", event.ReceivedAt, arrived)
		}
	}
}

// A datetime-local field submits a wall clock and no zone. It means the clock
// on the wall of whoever typed it.
func TestTheDateFilterIsReadOnTheReadersClock(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordAt(t, store, "acme", arrived)
	signIn(t, server, client, password)
	setZone(t, server, client, farAhead)

	// 13:00 in Kiritimati is 23:00 UTC, half an hour before the event. Read as
	// UTC the same text would be fourteen hours after it, and match nothing.
	_, page := get(t, client, server.URL+"/events?since=2026-03-02T13:00")
	if !strings.Contains(page, "2026-03-02 13:30:00") {
		t.Errorf("a filter typed on the reader's clock hid an event that is after it:\n%s", page)
	}

	_, later := get(t, client, server.URL+"/events?since=2026-03-02T14:00")
	if strings.Contains(later, "2026-03-02 13:30:00") {
		t.Errorf("a filter after the event still matched it:\n%s", later)
	}
}

func TestAZoneNobodyNamesIsRefusedAndNothingIsSaved(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordAt(t, store, "acme", arrived)
	signIn(t, server, client, password)

	got := setZone(t, server, client, "Mars/Olympus_Mons")
	if got.status != http.StatusBadRequest {
		t.Errorf("saving a zone nobody names = %d, want 400", got.status)
	}

	_, page := get(t, client, server.URL+"/events")
	if !strings.Contains(page, "2026-03-01 23:30:00") {
		t.Errorf("a refused zone changed how instants are read:\n%s", page)
	}
}

// Reading a clock is not something a role grants or withholds: it is the
// person's own account.
func TestAnyoneSignedInReachesTheirProfile(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	viewer, err := store.Role(context.Background(), authz.Least)
	if err != nil {
		t.Fatalf("reading the least role: %v", err)
	}
	viewer.Grants = nil
	if err := store.SetRole(context.Background(), viewer); err != nil {
		t.Fatalf("emptying the least role: %v", err)
	}
	if err := store.SetOperatorRole(context.Background(),
		operatorNamed(t, store, email), authz.Least); err != nil {
		t.Fatalf("demoting the operator: %v", err)
	}

	status, page := get(t, client, server.URL+"/profile")
	if status != http.StatusOK {
		t.Fatalf("GET /profile as a role that grants nothing = %d", status)
	}
	if !strings.Contains(page, "time zone") {
		t.Errorf("the profile does not offer a time zone:\n%s", page)
	}

	if refused, _ := get(t, client, server.URL+"/events"); refused != http.StatusForbidden {
		t.Errorf("GET /events as a role that grants nothing = %d, want 403", refused)
	}
}
