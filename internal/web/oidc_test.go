package web_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
	"github.com/edersonangelo/charon/internal/web"
)

func withSSO(t *testing.T, cfg web.OIDC) (*postgres.Store, *httptest.Server, *testsupport.IdentityProvider, *http.Client) {
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

	idp := testsupport.NewIdentityProvider(t)

	mux := http.NewServeMux()
	server := httptest.NewUnstartedServer(mux)
	server.Start()
	t.Cleanup(server.Close)

	cfg.Issuer = idp.Issuer()
	cfg.ClientID = "charon"
	cfg.ClientSecret = "shh"
	cfg.RedirectURL = server.URL + "/auth/sso/callback"

	web.New(store, web.Config{OIDC: cfg}).Register(mux)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}

	return store, server, idp, &http.Client{Jar: jar}
}

func TestSingleSignOnAdmitsAnExistingOperator(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, web.OIDC{})
	ctx := context.Background()

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if err := store.CreateUser(ctx, idp.Email, hash); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}

	got := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	status, page := get(t, client, server.URL+"/events")
	if status != http.StatusOK || !strings.Contains(page, idp.Email) {
		t.Errorf("the panel does not show the signed in operator: status %d", status)
	}

	linked, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("reading the operator: %v", err)
	}
	if linked.Subject != idp.Subject {
		t.Errorf("subject = %q, want it linked to %q", linked.Subject, idp.Subject)
	}
}

func TestSingleSignOnRefusesAnUnknownOperatorByDefault(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, web.OIDC{})

	got := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", got.finalURL)
	}

	if _, err := store.UserByEmail(context.Background(), idp.Email); err == nil {
		t.Error("an operator was created even though provisioning is off")
	}
}

func TestSingleSignOnProvisionsWhenAskedTo(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, web.OIDC{AutoProvision: true})

	got := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	created, err := store.UserByEmail(context.Background(), idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if created.CanSignInWithPassword() {
		t.Error("a provisioned operator must not have a password")
	}
}

func TestSingleSignOnEnforcesTheRequiredGroup(t *testing.T) {
	t.Parallel()

	_, server, idp, client := withSSO(t, web.OIDC{
		AutoProvision: true,
		RequiredGroup: "charon-operators",
	})

	idp.Groups = []string{"some-other-team"}
	refused := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if !strings.Contains(refused.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", refused.finalURL)
	}

	idp.Groups = []string{"charon-operators"}
	admitted := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if !strings.HasSuffix(admitted.finalURL, "/events") {
		t.Errorf("landed on %s, want /events once the group matches", admitted.finalURL)
	}
}

func TestSingleSignOnRefusesACallbackWithoutAHandshake(t *testing.T) {
	t.Parallel()

	_, server, _, client := withSSO(t, web.OIDC{AutoProvision: true})

	got := do(t, client, http.MethodGet,
		server.URL+"/auth/sso/callback?code=made-up&state=made-up", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Errorf("landed on %s, want the login page", got.finalURL)
	}
}

func TestSingleSignOnRoutesAreAbsentWhenNotConfigured(t *testing.T) {
	t.Parallel()

	_, server, _, _ := setup(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	got := do(t, client, http.MethodGet, server.URL+"/auth/sso", nil)
	if got.status != http.StatusNotFound {
		t.Errorf("GET /auth/sso = %d, want 404 when single sign-on is not configured", got.status)
	}

	_, page := get(t, client, server.URL+"/login")
	if strings.Contains(page, "single sign-on") {
		t.Error("the login page offers single sign-on that is not configured")
	}
}
