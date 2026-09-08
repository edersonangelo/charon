package web_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
	"github.com/edersonangelo/charon/internal/web"
)

// A way of signing in that the panel has never heard of, written entirely in
// this test. Registering it must be the whole change.
type headerMethod struct {
	header string
	admits map[string]auth.Identity
}

func (headerMethod) Name() string  { return "trusted-header" }
func (headerMethod) Label() string { return "the reverse proxy" }

func (m headerMethod) Identify(_ context.Context, r *http.Request) (auth.Identity, error) {
	identity, admitted := m.admits[r.Header.Get(m.header)]
	if !admitted {
		return auth.Identity{}, auth.ErrRefused
	}
	return identity, nil
}

func withMethods(t *testing.T, policy auth.Policy, extra ...auth.Method) (*postgres.Store, *httptest.Server, *http.Client) {
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

	methods := auth.NewRegistry()
	methods.Register(auth.NewPassword(credentials{store}))
	for _, method := range extra {
		methods.Register(method)
	}
	if err := store.Register(ctx, postgres.Kinds{Methods: methods.Names()}); err != nil {
		t.Fatalf("registering the ways of signing in: %v", err)
	}

	mux := http.NewServeMux()
	web.New(store, web.Config{Auth: methods, Policy: policy}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}
	return store, server, &http.Client{Jar: jar}
}

func TestAMethodCanBeAddedWithoutChangingThePanel(t *testing.T) {
	t.Parallel()

	method := headerMethod{
		header: "X-Forwarded-Operator",
		admits: map[string]auth.Identity{
			"someone@example.com": {
				Subject: "proxy|1", Email: "someone@example.com", Claims: []string{"operators"},
			},
		},
	}

	store, server, client := withMethods(t, auth.Policy{AutoProvision: true}, method)

	if _, err := store.CreateTenant(context.Background(), "operators", "Operators"); err != nil {
		t.Fatalf("creating the tenant the group names: %v", err)
	}

	_, page := get(t, client, server.URL+"/login")
	if !strings.Contains(page, "the reverse proxy") {
		t.Error("the sign-in page does not offer the registered method")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+"/auth/trusted-header", strings.NewReader(""))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-Forwarded-Operator", "someone@example.com")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("signing in: %v", err)
	}
	_ = resp.Body.Close()

	status, events := get(t, client, server.URL+"/events")
	if status != http.StatusOK || !strings.Contains(events, "someone@example.com") {
		t.Fatalf("the panel is not reachable after signing in: status %d", status)
	}

	provisioned, err := store.UserByEmail(context.Background(), "someone@example.com")
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if provisioned.Subject != "proxy|1" {
		t.Errorf("subject = %q, want the identity linked", provisioned.Subject)
	}
}

// The policy is applied over whatever a method reports, so a rule cannot hold
// for one way of signing in and be forgotten for another.
func TestThePolicyAppliesToEveryMethod(t *testing.T) {
	t.Parallel()

	method := headerMethod{
		header: "X-Forwarded-Operator",
		admits: map[string]auth.Identity{
			"outsider@example.com": {Subject: "proxy|2", Email: "outsider@example.com"},
		},
	}

	_, server, client := withMethods(t,
		auth.Policy{AutoProvision: true, RequiredClaim: "operators"}, method)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+"/auth/trusted-header", strings.NewReader(""))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-Forwarded-Operator", "outsider@example.com")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("signing in: %v", err)
	}
	_ = resp.Body.Close()

	if !strings.Contains(resp.Request.URL.String(), "/login") {
		t.Errorf("landed on %s, want the sign-in page: the group was not required",
			resp.Request.URL)
	}
}

// A password never provisions: the operator has to exist first.
func TestPasswordNeverProvisions(t *testing.T) {
	t.Parallel()

	store, server, client := withMethods(t, auth.Policy{AutoProvision: true})

	got := do(t, client, http.MethodPost, server.URL+"/auth/"+auth.Password, url.Values{
		"email":    {"nobody@example.com"},
		"password": {"whatever-it-is"},
	})
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the sign-in page", got.finalURL)
	}

	if _, err := store.UserByEmail(context.Background(), "nobody@example.com"); err == nil {
		t.Error("a password sign-in created an operator")
	}
}
