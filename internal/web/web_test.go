package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/inbound"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
	"github.com/edersonangelo/charon/internal/web"
)

const (
	email    = "operator@example.com"
	password = "correct horse battery"
)

var body = []byte(`{"id":"evt_1","type":"charge.succeeded"}`)

func setup(t *testing.T) (*postgres.Store, *httptest.Server, *http.Client, uuid.UUID) {
	t.Helper()

	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	// Serving does this on every start, and what it records is what decides
	// whether an operator may do anything at all.
	if err := store.Register(ctx, postgres.Kinds{}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing the password: %v", err)
	}
	if err := store.CreateUser(ctx, email, hash, owner(t, store)); err != nil {
		t.Fatalf("creating the user: %v", err)
	}

	eventID, err := store.Record(ctx, inbound.Request{
		Provider:   "stripe",
		Path:       "/webhooks/stripe",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Stripe-Signature": {"t=1,v1=deadbeef"}},
		Body:       body,
	})
	if err != nil {
		t.Fatalf("recording the event: %v", err)
	}

	methods := auth.NewRegistry()
	methods.Register(auth.NewPassword(credentials{store}))

	mux := http.NewServeMux()
	web.New(store, web.Config{Auth: methods}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}

	return store, server, &http.Client{Jar: jar}, eventID
}

// owner places an operator in the tenant every deployment has, as the role
// that can reach everything.
func owner(t *testing.T, store *postgres.Store) console.Placement {
	t.Helper()

	tenant, err := store.TenantBySlug(context.Background(), postgres.DefaultSlug)
	if err != nil {
		t.Fatalf("reading the default tenant: %v", err)
	}
	return console.Placement{Tenant: tenant.ID, Role: "owner"}
}

// tenantNamed is the tenant a value from the provider names.
func tenantNamed(t *testing.T, store *postgres.Store, slug string) uuid.UUID {
	t.Helper()

	found, err := store.TenantBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("reading the tenant %q: %v", slug, err)
	}
	return found.ID
}

// roleOf is the role somebody holds in a tenant, which is what a membership
// says rather than anything on the account.
func roleOf(t *testing.T, store *postgres.Store, user, tenant uuid.UUID) string {
	t.Helper()

	name, member, err := store.RoleIn(context.Background(), user, tenant)
	if err != nil {
		t.Fatalf("reading the role: %v", err)
	}
	if !member {
		return "(not a member)"
	}
	if name == "" {
		return authz.Least
	}
	return name
}

func mustMemberships(t *testing.T, store *postgres.Store, user uuid.UUID) []console.Membership {
	t.Helper()

	held, err := store.Memberships(context.Background(), user)
	if err != nil {
		t.Fatalf("reading the memberships: %v", err)
	}
	return held
}

// jar is a cookie jar for a second client, so two operators can be signed in
// at once inside one test.
func jar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	made, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}
	return made
}

// scoped is a context confined to the tenant the tests operate in.
func scoped(t *testing.T, store *postgres.Store) context.Context {
	t.Helper()
	return authz.WithTenant(context.Background(), owner(t, store).Tenant)
}

// Returns a copy rather than the *http.Response, so callers cannot leak a body
// that this helper already closed.
type response struct {
	status   int
	location string
	finalURL string
	body     string
}

func do(t *testing.T, client *http.Client, method, target string, form url.Values) response {
	t.Helper()

	var payload io.Reader
	if form != nil {
		payload = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(t.Context(), method, target, payload)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, target, err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", target, err)
	}

	return response{
		status:   resp.StatusCode,
		location: resp.Header.Get("Location"),
		finalURL: resp.Request.URL.String(),
		body:     string(read),
	}
}

func signIn(t *testing.T, server *httptest.Server, client *http.Client, pass string) response {
	t.Helper()
	return do(t, client, http.MethodPost, server.URL+"/auth/"+auth.Password, url.Values{
		"email":    {email},
		"password": {pass},
	})
}

func get(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	got := do(t, client, http.MethodGet, target, nil)
	return got.status, got.body
}

func TestThePanelRefusesAnyoneWithoutASession(t *testing.T) {
	t.Parallel()

	_, server, _, _ := setup(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, path := range []string{"/", "/events", "/routes"} {
		got := do(t, client, http.MethodGet, server.URL+path, nil)

		if got.status != http.StatusSeeOther || got.location != "/login" {
			t.Errorf("GET %s = %d to %q, want 303 to /login", path, got.status, got.location)
		}
	}
}

func TestSigningInWithTheWrongPasswordDoesNotGrantASession(t *testing.T) {
	t.Parallel()

	_, server, client, _ := setup(t)

	got := signIn(t, server, client, "not the password")
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", got.finalURL)
	}

	status, page := get(t, client, server.URL+"/events")
	if status != http.StatusOK || !strings.Contains(page, "sign in") {
		t.Errorf("after a failed sign in the panel is reachable: status %d", status)
	}
}

func TestAnOperatorFindsAnEventAndItsBody(t *testing.T) {
	t.Parallel()

	_, server, client, eventID := setup(t)
	signIn(t, server, client, password)

	status, page := get(t, client, server.URL+"/events")
	if status != http.StatusOK {
		t.Fatalf("GET /events = %d", status)
	}
	if !strings.Contains(page, "stripe") {
		t.Error("the event list does not mention the provider")
	}
	if !strings.Contains(page, "awaiting a route") {
		t.Error("the event list does not show that the event has no route")
	}

	status, detail := get(t, client, server.URL+"/events/"+eventID.String())
	if status != http.StatusOK {
		t.Fatalf("GET the event = %d", status)
	}
	if !strings.Contains(detail, "charge.succeeded") {
		t.Error("the detail page does not show the raw body")
	}
	if !strings.Contains(detail, "Stripe-Signature") {
		t.Error("the detail page does not show the headers")
	}
}

func TestSearchNarrowsByProvider(t *testing.T) {
	t.Parallel()

	store, server, client, stripeEvent := setup(t)
	signIn(t, server, client, password)

	githubEvent, err := store.Record(context.Background(), inbound.Request{
		Provider:   "github",
		Path:       "/webhooks/github",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{},
		Body:       []byte(`{"ref":"refs/heads/main"}`),
	})
	if err != nil {
		t.Fatalf("recording a second event: %v", err)
	}

	_, all := get(t, client, server.URL+"/events")
	if !strings.Contains(all, stripeEvent.String()) || !strings.Contains(all, githubEvent.String()) {
		t.Fatal("the unfiltered list is missing one of the events")
	}

	_, filtered := get(t, client, server.URL+"/events?provider=github")
	if !strings.Contains(filtered, githubEvent.String()) {
		t.Error("filtering by provider hid the matching event")
	}
	if strings.Contains(filtered, stripeEvent.String()) {
		t.Error("filtering by provider kept a non-matching event")
	}

	_, byText := get(t, client, server.URL+"/events?q=refs%2Fheads%2Fmain")
	if !strings.Contains(byText, githubEvent.String()) {
		t.Error("searching the body text hid the matching event")
	}
	if strings.Contains(byText, stripeEvent.String()) {
		t.Error("searching the body text kept a non-matching event")
	}
}

func TestReplayReturnsADeadDeliveryToPendingAndKeepsItsHistory(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)

	ctx := context.Background()

	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(refuse.Close)

	if err := store.AddRoute(ctx, "stripe", "refusing", refuse.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{
		MaxAttempts: 1,
		BackoffBase: time.Millisecond,
		BackoffCap:  2 * time.Millisecond,
	})
	for range 3 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	if got := mustStates(t, store)["dead"]; got != 1 {
		t.Fatalf("dead deliveries = %d, want 1 before the replay", got)
	}

	replayed := do(t, client, http.MethodPost, server.URL+"/events/"+eventID.String()+"/replay", url.Values{})
	if replayed.status != http.StatusOK {
		t.Fatalf("replay = %d, want 200", replayed.status)
	}

	states := mustStates(t, store)
	if states["pending"] != 1 || states["dead"] != 0 {
		t.Fatalf("states = %v, want the delivery back to pending", states)
	}

	deliveries, err := store.EventDeliveries(ctx, eventID)
	if err != nil {
		t.Fatalf("reading deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(deliveries))
	}
	if deliveries[0].ReplayCount != 1 {
		t.Errorf("replay count = %d, want 1", deliveries[0].ReplayCount)
	}
	if len(deliveries[0].Rounds) == 0 {
		t.Error("the replay erased the attempt history")
	}
}

func mustStates(t *testing.T, store *postgres.Store) map[string]int64 {
	t.Helper()
	states, err := store.DeliveryStates(context.Background())
	if err != nil {
		t.Fatalf("reading delivery states: %v", err)
	}
	return states
}

func TestTheEventListOpensTheDetailInAModal(t *testing.T) {
	t.Parallel()

	_, server, client, eventID := setup(t)
	signIn(t, server, client, password)

	_, list := get(t, client, server.URL+"/events")
	if !strings.Contains(list, `hx-get="/events/`+eventID.String()+`/fragment"`) {
		t.Error("the row does not load the detail fragment")
	}
	if !strings.Contains(list, "data-open") {
		t.Error("the row is not marked as clickable")
	}
	if !strings.Contains(list, `<dialog id="detail">`) {
		t.Error("the page has no dialog to open")
	}
	if !strings.Contains(list, `onclick="event.stopPropagation()"`) {
		t.Error("the replay cell would also open the modal")
	}

	status, fragment := get(t, client, server.URL+"/events/"+eventID.String()+"/fragment")
	if status != http.StatusOK {
		t.Fatalf("GET the fragment = %d", status)
	}
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, "<dialog") {
		t.Error("the fragment carries the whole page instead of just the detail")
	}
	if !strings.Contains(fragment, "charge.succeeded") {
		t.Error("the fragment does not show the body")
	}
	if !strings.Contains(fragment, "open full page") {
		t.Error("the fragment has no link out to the full page")
	}
}

func TestTheRoutesPageHighlightsProvidersWithNoRouteAndCreatesOne(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/routes")
	if !strings.Contains(page, "Received, but going nowhere") {
		t.Fatal("the page does not call out providers without a route")
	}
	if !strings.Contains(page, "section class=\"attention\"") {
		t.Error("providers without a route are not highlighted")
	}
	if !strings.Contains(page, "stripe") {
		t.Error("the recorded provider is not listed as unrouted")
	}

	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)

	created := do(t, client, http.MethodPost, server.URL+"/routes", url.Values{
		"provider": {"stripe"},
		"url":      {destination.URL},
	})
	if created.status != http.StatusOK {
		t.Fatalf("creating the route = %d", created.status)
	}
	if strings.Contains(created.body, "Received, but going nowhere") {
		t.Error("the provider is still listed as unrouted after being given a route")
	}

	routes, err := store.DetailedRoutes(context.Background())
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	if len(routes) != 1 || routes[0].Provider != "stripe" || !routes[0].Enabled {
		t.Fatalf("routes = %+v, want one enabled stripe route", routes)
	}

	saved := do(t, client, http.MethodPost,
		server.URL+"/routes/"+routes[0].ID.String()+"/save", url.Values{
			"url": {destination.URL + "/changed"},
		})
	if saved.status != http.StatusOK {
		t.Fatalf("saving the route = %d", saved.status)
	}

	routes, err = store.DetailedRoutes(context.Background())
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	if routes[0].URL != destination.URL+"/changed" {
		t.Errorf("url = %q, want it changed", routes[0].URL)
	}
	if routes[0].Enabled {
		t.Error("leaving the checkbox off did not disable the route")
	}

	removed := do(t, client, http.MethodPost,
		server.URL+"/routes/"+routes[0].ID.String()+"/delete", url.Values{})
	if removed.status != http.StatusOK {
		t.Fatalf("removing the route = %d", removed.status)
	}

	routes, err = store.DetailedRoutes(context.Background())
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	if len(routes) != 0 {
		t.Errorf("routes = %+v, want none after removal", routes)
	}
}

func TestARouteNeedsAnHttpDestination(t *testing.T) {
	t.Parallel()

	_, server, client, _ := setup(t)
	signIn(t, server, client, password)

	for _, bad := range []string{"", "not a url", "ftp://host/x", "/relative"} {
		got := do(t, client, http.MethodPost, server.URL+"/routes", url.Values{
			"provider": {"stripe"},
			"url":      {bad},
		})
		if got.status != http.StatusBadRequest {
			t.Errorf("creating a route with url %q = %d, want 400", bad, got.status)
		}
	}
}

func TestTheDetailIndentsJsonAndKeepsTheOriginalOneClickAway(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	minified := []byte(`{"evento":"alterado","id":26281,"pessoa":{"id":40104,"razao_social":"A"}}`)
	jsonEvent, err := store.Record(context.Background(), inbound.Request{
		Provider:   "indented",
		Path:       "/webhooks/indented",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       minified,
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	_, page := get(t, client, server.URL+"/events/"+jsonEvent.String())
	if !strings.Contains(page, `&#34;evento&#34;: &#34;alterado&#34;`) {
		t.Error("the body was not indented")
	}
	if !strings.Contains(page, "raw, exactly as received") {
		t.Error("the original body is not offered")
	}

	plainEvent, err := store.Record(context.Background(), inbound.Request{
		Provider:   "not-json",
		Path:       "/webhooks/not-json",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{},
		Body:       []byte("status=paid&ref=xyz"),
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	_, plain := get(t, client, server.URL+"/events/"+plainEvent.String())
	if !strings.Contains(plain, "status=paid&amp;ref=xyz") {
		t.Error("a body that is not json is not shown")
	}
	if strings.Contains(plain, "raw, exactly as received") {
		t.Error("a body that is not json should not offer a raw toggle")
	}
}

func TestTheDetailOffersToCopyTheRawBody(t *testing.T) {
	t.Parallel()

	_, server, client, eventID := setup(t)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/events/"+eventID.String())

	if !strings.Contains(page, `onclick="copyBody(this)"`) {
		t.Error("there is no copy button")
	}
	if !strings.Contains(page, `class="raw-body"`) {
		t.Error("the copy button has no raw body to read")
	}
	if strings.Count(page, `class="raw-body"`) != 1 {
		t.Errorf(`found %d raw bodies, want exactly one for the button to find`,
			strings.Count(page, `class="raw-body"`))
	}
}

func TestChangingARouteDoesNotResendUntilTheOperatorAsks(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	var first, second atomic.Int32
	oldTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(oldTarget.Close)
	newTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(newTarget.Close)

	if err := store.AddRoute(ctx, "stripe", "moving", oldTarget.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{})
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("old target saw %d, new target saw %d, want 1 and 0", first.Load(), second.Load())
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}

	moved := do(t, client, http.MethodPost,
		server.URL+"/routes/"+routes[0].ID.String()+"/save", url.Values{
			"url":     {newTarget.URL},
			"enabled": {"1"},
		})
	if moved.status != http.StatusOK {
		t.Fatalf("saving the route = %d", moved.status)
	}
	if !strings.Contains(moved.body, "resend</button>") {
		t.Error("the page does not offer resend, which is the only way to send it again")
	}

	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick after the change: %v", err)
		}
	}
	if second.Load() != 0 {
		t.Fatalf("the new target saw %d deliveries, want 0 without an explicit resend", second.Load())
	}

	resent := do(t, client, http.MethodPost,
		server.URL+"/routes/"+routes[0].ID.String()+"/resend", url.Values{})
	if resent.status != http.StatusOK {
		t.Fatalf("resending = %d", resent.status)
	}

	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick after the resend: %v", err)
		}
	}
	if second.Load() != 1 {
		t.Errorf("the new target saw %d deliveries after the resend, want 1", second.Load())
	}

	deliveries, err := store.EventDeliveries(ctx, eventID)
	if err != nil {
		t.Fatalf("reading deliveries: %v", err)
	}
	if len(deliveries[0].Rounds) != 2 {
		t.Errorf("got %d rounds, want the original and the resend kept apart", len(deliveries[0].Rounds))
	}
}

func TestASecondRouteReachesEventsThatWereAlreadyPlanned(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	var first, second atomic.Int32
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(one.Close)
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(two.Close)

	dispatcher := delivery.New(store, delivery.Config{})

	if err := store.AddRoute(ctx, "stripe", "one", one.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the first route: %v", err)
	}
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if first.Load() != 1 {
		t.Fatalf("the first destination saw %d deliveries, want 1", first.Load())
	}

	// The event is planned by now. A second destination must still reach it.
	if err := store.AddRoute(ctx, "stripe", "two", two.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the second route: %v", err)
	}
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	if second.Load() != 1 {
		t.Errorf("the second destination saw %d deliveries, want 1", second.Load())
	}
	if first.Load() != 1 {
		t.Errorf("the first destination saw %d deliveries, want the original one only", first.Load())
	}
}

func TestSwitchingADestinationBackOnReachesEventsPlannedWhileItWasOff(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	var seen atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	if err := store.AddRoute(ctx, "stripe", "off-then-on", target.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	if err := store.SaveRoute(ctx, routes[0].ID, target.URL, false); err != nil {
		t.Fatalf("disabling: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{})
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick while disabled: %v", err)
		}
	}
	if seen.Load() != 0 {
		t.Fatalf("the destination saw %d deliveries while disabled, want 0", seen.Load())
	}

	if err := store.SaveRoute(ctx, routes[0].ID, target.URL, true); err != nil {
		t.Fatalf("enabling: %v", err)
	}
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick after enabling: %v", err)
		}
	}
	if seen.Load() != 1 {
		t.Errorf("the destination saw %d deliveries after coming back on, want 1", seen.Load())
	}
}

// A duplicated id made every live refresh nest the list inside itself, so the
// same events appeared again and again.
func TestTheEventListHasExactlyOneSwapTarget(t *testing.T) {
	t.Parallel()

	_, server, client, _ := setup(t)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/events")
	if got := strings.Count(page, `id="events"`); got != 1 {
		t.Fatalf(`found %d elements with id="events", want exactly 1`, got)
	}
	if got := strings.Count(page, `id="detail"`); got != 1 {
		t.Errorf(`found %d elements with id="detail", want exactly 1`, got)
	}
	if got := strings.Count(page, `id="detail-body"`); got != 1 {
		t.Errorf(`found %d elements with id="detail-body", want exactly 1`, got)
	}
	if got := strings.Count(page, `id="summary"`); got != 1 {
		t.Errorf(`found %d elements with id="summary", want exactly 1`, got)
	}
}

// Every swap target the live stream refreshes must survive being replaced by
// the same fragment, or the page grows a copy of itself each time.
func TestRefreshingTheListDoesNotNestIt(t *testing.T) {
	t.Parallel()

	_, server, client, _ := setup(t)
	signIn(t, server, client, password)

	for range 3 {
		_, page := get(t, client, server.URL+"/events")
		if got := strings.Count(page, `id="events"`); got != 1 {
			t.Fatalf(`found %d elements with id="events" after a refresh, want 1`, got)
		}
	}
}

// Styling the dialog with display:flex unconditionally overrides the browser's
// display:none for a closed dialog, which leaves an empty box sitting in the
// page. The flex layout has to be scoped to the open state.
func TestTheDialogIsOnlyLaidOutWhenOpen(t *testing.T) {
	t.Parallel()

	_, server, client, _ := setup(t)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/events")

	if !strings.Contains(page, "dialog[open] { display: flex") {
		t.Error("the dialog's flex layout is not scoped to [open]")
	}
	if strings.Contains(page, "dialog { width") && strings.Contains(page, "display: flex; flex-direction: column; overflow: hidden") {
		t.Error("the unscoped dialog rule is back, which shows the modal when closed")
	}
}

// Every button that swaps a region has to be answered with a response that
// actually contains that region. Getting this wrong replaces the region with
// nothing, which is how the event list disappeared after a replay.
func TestEveryReplayAnswersWithTheRegionItSwaps(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	if err := store.AddRoute(ctx, "stripe", "somewhere", target.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route: %v", err)
	}
	if _, err := delivery.New(store, delivery.Config{}).Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	deliveries, err := store.EventDeliveries(ctx, eventID)
	if err != nil || len(deliveries) == 0 {
		t.Fatalf("reading deliveries: %v", err)
	}
	event := eventID.String()
	item := deliveries[0].ID.String()

	tests := []struct {
		name string
		path string
		want string
		full bool
	}{
		{"replay from the list", "/events/" + event + "/replay?view=list", `id="events"`, true},
		{"replay from the modal", "/events/" + event + "/replay?view=fragment", "Deliveries", false},
		{"replay from the page", "/events/" + event + "/replay", `id="detail-body"`, true},
		{"one delivery from the modal",
			"/events/" + event + "/deliveries/" + item + "/replay?view=fragment", "Deliveries", false},
		{"one delivery from the page",
			"/events/" + event + "/deliveries/" + item + "/replay", `id="detail-body"`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := do(t, client, http.MethodPost, server.URL+tt.path, url.Values{})
			if got.status != http.StatusOK {
				t.Fatalf("POST %s = %d", tt.path, got.status)
			}
			if !strings.Contains(got.body, tt.want) {
				t.Errorf("the response does not contain %q, so htmx would swap in nothing", tt.want)
			}
			if isFull := strings.Contains(got.body, "<html"); isFull != tt.full {
				t.Errorf("full page = %v, want %v", isFull, tt.full)
			}
		})
	}
}

// Replaying from the list must keep the search the operator was looking at.
func TestReplayingFromTheListKeepsTheFilters(t *testing.T) {
	t.Parallel()

	store, server, client, stripeEvent := setup(t)
	signIn(t, server, client, password)

	other, err := store.Record(context.Background(), inbound.Request{
		Provider:   "github",
		Path:       "/webhooks/github",
		ReceivedAt: time.Now().UTC(),
		Headers:    map[string][]string{},
		Body:       body,
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	got := do(t, client, http.MethodPost,
		server.URL+"/events/"+stripeEvent.String()+"/replay?view=list",
		url.Values{"provider": {"stripe"}})
	if got.status != http.StatusOK {
		t.Fatalf("replay = %d", got.status)
	}
	if !strings.Contains(got.body, stripeEvent.String()) {
		t.Error("the filtered event is missing from the response")
	}
	if strings.Contains(got.body, other.String()) {
		t.Error("the filter was dropped: an event from another provider came back")
	}
}

// The routes page is the truth about where events go. A delivery whose route
// was removed stays as history and must never be sent again, or a replay hits
// an address the operator already took off the list.
func TestReplayDoesNotReachADestinationThatIsNoLongerRouted(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	var kept, dropped atomic.Int32
	keptTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		kept.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(keptTarget.Close)
	droppedTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dropped.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(droppedTarget.Close)

	if err := store.AddRoute(ctx, "stripe", "kept", keptTarget.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route to keep: %v", err)
	}
	if err := store.AddRoute(ctx, "stripe", "dropped", droppedTarget.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route to drop: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{})
	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if kept.Load() != 1 || dropped.Load() != 1 {
		t.Fatalf("first pass delivered %d and %d, want 1 each", kept.Load(), dropped.Load())
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	for _, route := range routes {
		if route.Destination == "dropped" {
			if err := store.DeleteRoute(ctx, route.ID); err != nil {
				t.Fatalf("removing the route: %v", err)
			}
		}
	}

	replayed := do(t, client, http.MethodPost,
		server.URL+"/events/"+eventID.String()+"/replay?view=fragment", url.Values{})
	if replayed.status != http.StatusOK {
		t.Fatalf("replay = %d", replayed.status)
	}
	if !strings.Contains(replayed.body, "no longer routed") {
		t.Error("the detail does not say the orphaned delivery is no longer routed")
	}

	for range 2 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick after the replay: %v", err)
		}
	}

	if kept.Load() != 2 {
		t.Errorf("the routed destination saw %d deliveries, want 2", kept.Load())
	}
	if dropped.Load() != 1 {
		t.Errorf("the destination whose route was removed saw %d deliveries, want the original one only",
			dropped.Load())
	}
}

func TestAProviderCannotBeRoutedTwiceToTheSameUrl(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	target := "http://service.internal/webhooks/stripe"

	first := do(t, client, http.MethodPost, server.URL+"/routes", url.Values{
		"provider": {"stripe"}, "url": {target}, "destination": {"one"},
	})
	if first.status != http.StatusOK {
		t.Fatalf("first route = %d", first.status)
	}

	second := do(t, client, http.MethodPost, server.URL+"/routes", url.Values{
		"provider": {"stripe"}, "url": {target}, "destination": {"two"},
	})
	if second.status != http.StatusConflict {
		t.Errorf("second route to the same url = %d, want 409", second.status)
	}

	routes, err := store.DetailedRoutes(context.Background())
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	if len(routes) != 1 {
		t.Errorf("got %d routes, want 1: the same url twice would double every delivery", len(routes))
	}
}

// The list is the triage screen: it has to reconcile with the routes page and
// show what is struggling without being opened.
func TestTheListSeparatesRoutedFromOrphanedAndShowsAttempts(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(refusing.Close)
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dropped.Close)

	if err := store.AddRoute(ctx, "stripe", "refusing", refusing.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the refusing route: %v", err)
	}
	if err := store.AddRoute(ctx, "stripe", "dropped", dropped.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the route to drop: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{
		MaxAttempts: 20,
		BackoffBase: time.Millisecond,
		BackoffCap:  2 * time.Millisecond,
	})
	for range 4 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	routes, err := store.DetailedRoutes(ctx)
	if err != nil {
		t.Fatalf("reading routes: %v", err)
	}
	for _, route := range routes {
		if route.Destination == "dropped" {
			if err := store.DeleteRoute(ctx, route.ID); err != nil {
				t.Fatalf("removing the route: %v", err)
			}
		}
	}

	events, err := store.SearchEvents(ctx, console.Filter{})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}

	var found console.EventSummary
	for _, event := range events {
		if event.ID == eventID {
			found = event
		}
	}

	if found.Deliveries != 1 {
		t.Errorf("deliveries = %d, want 1: only the destination still routed counts",
			found.Deliveries)
	}
	if found.Unrouted != 1 {
		t.Errorf("unrouted = %d, want 1 reported apart", found.Unrouted)
	}
	if found.Attempts < 2 {
		t.Errorf("attempts = %d, want the retries to show", found.Attempts)
	}

	_, page := get(t, client, server.URL+"/events")
	if !strings.Contains(page, "1 no longer routed") {
		t.Error("the list does not report the orphaned delivery apart")
	}
	if !strings.Contains(page, "attempts") {
		t.Error("the list does not show how many attempts a delivery has taken")
	}
}

// The modal has to answer "where did this go" before "what were the tries". An
// open attempt history buries every destination after the first.
func TestTheDetailListsEveryDestinationBeforeTheAttemptHistory(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	for _, name := range []string{"one", "two", "three"} {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(target.Close)
		if err := store.AddRoute(ctx, "stripe", name, target.URL, outbound.HTTP); err != nil {
			t.Fatalf("adding route %s: %v", name, err)
		}
	}

	dispatcher := delivery.New(store, delivery.Config{
		MaxAttempts: 20,
		BackoffBase: time.Millisecond,
		BackoffCap:  2 * time.Millisecond,
	})
	for range 4 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, fragment := get(t, client, server.URL+"/events/"+eventID.String()+"/fragment")

	for _, name := range []string{"one", "two", "three"} {
		if !strings.Contains(fragment, "<th>"+name+"</th>") {
			t.Errorf("destination %q is missing from the detail", name)
		}
	}
	if got := strings.Count(fragment, "attempt history"); got != 3 {
		t.Errorf("found %d collapsed histories, want one per destination", got)
	}
	if !strings.Contains(fragment, "<details>") {
		t.Error("the attempt history is not collapsed")
	}

	// No attempt table may sit outside a collapsed block: an expanded one
	// pushes every destination after it off the screen.
	if first, table := strings.Index(fragment, "attempt history"), strings.Index(fragment, "<thead>"); table < first {
		t.Error("an attempt table is rendered outside a collapsed history")
	}
}

func TestFilteringByDeliveryState(t *testing.T) {
	t.Parallel()

	store, server, client, deliveredEvent := setup(t)
	signIn(t, server, client, password)
	ctx := context.Background()

	accepts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(accepts.Close)
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(refuses.Close)

	recorded := map[string]uuid.UUID{}
	for _, provider := range []string{"github", "nobody"} {
		id, err := store.Record(ctx, inbound.Request{
			Provider: provider, Path: "/webhooks/" + provider,
			ReceivedAt: time.Now().UTC(), Headers: map[string][]string{}, Body: body,
		})
		if err != nil {
			t.Fatalf("recording %s: %v", provider, err)
		}
		recorded[provider] = id
	}
	deadEvent, waitingEvent := recorded["github"], recorded["nobody"]

	if err := store.AddRoute(ctx, "stripe", "accepts", accepts.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the accepting route: %v", err)
	}
	if err := store.AddRoute(ctx, "github", "refuses", refuses.URL, outbound.HTTP); err != nil {
		t.Fatalf("adding the refusing route: %v", err)
	}

	dispatcher := delivery.New(store, delivery.Config{
		MaxAttempts: 1, BackoffBase: time.Millisecond, BackoffCap: 2 * time.Millisecond,
	})
	for range 4 {
		if _, err := dispatcher.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	tests := []struct {
		state  string
		want   uuid.UUID
		absent []uuid.UUID
	}{
		{"delivered", deliveredEvent, []uuid.UUID{deadEvent, waitingEvent}},
		{"dead", deadEvent, []uuid.UUID{deliveredEvent, waitingEvent}},
		{"unrouted", waitingEvent, []uuid.UUID{deliveredEvent, deadEvent}},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			_, page := get(t, client, server.URL+"/events?state="+tt.state)
			if !strings.Contains(page, tt.want.String()) {
				t.Errorf("state=%s hid the event that matches", tt.state)
			}
			for _, other := range tt.absent {
				if strings.Contains(page, other.String()) {
					t.Errorf("state=%s kept an event that does not match", tt.state)
				}
			}
		})
	}

	_, page := get(t, client, server.URL+"/events")
	if !strings.Contains(page, `<option value="unrouted">awaiting a route</option>`) {
		t.Error("the filter does not offer awaiting a route")
	}
}

// credentials adapts the store to the port the password method declares.
type credentials struct{ store *postgres.Store }

func (c credentials) Verify(ctx context.Context, email, password string) (string, error) {
	return c.store.VerifyPassword(ctx, email, password)
}
