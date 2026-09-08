package web_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/testsupport"
	"github.com/edersonangelo/charon/internal/web"
)

func withSSO(t *testing.T, policy auth.Policy) (*postgres.Store, *httptest.Server, *testsupport.IdentityProvider, *http.Client) {
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

	idp := testsupport.NewIdentityProvider(t)

	mux := http.NewServeMux()
	server := httptest.NewUnstartedServer(mux)
	server.Start()
	t.Cleanup(server.Close)

	methods := auth.NewRegistry()
	methods.Register(auth.NewPassword(credentials{store}))
	methods.Register(auth.NewOIDC(auth.OIDCSettings{
		Issuer:       idp.Issuer(),
		ClientID:     "charon",
		ClientSecret: "shh",
		RedirectURL:  server.URL + "/auth/" + auth.OIDC + "/callback",
	}))

	web.New(store, web.Config{Auth: methods, Policy: policy}).Register(mux)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}

	return store, server, idp, &http.Client{Jar: jar}
}

// place makes the tenant a value the provider sends names. The name is the
// mapping: nothing else is registered.
func place(t *testing.T, store *postgres.Store, named string) {
	t.Helper()

	if _, err := store.CreateTenant(context.Background(), named, named); err != nil {
		t.Fatalf("creating the tenant named %q: %v", named, err)
	}
}

func TestSingleSignOnAdmitsAnExistingOperator(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{})
	ctx := context.Background()

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if err := store.CreateUser(ctx, idp.Email, hash, owner(t, store)); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}
	idp.Claims = []string{postgres.DefaultSlug}

	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
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

	store, server, idp, client := withSSO(t, auth.Policy{})

	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", got.finalURL)
	}

	if _, err := store.UserByEmail(context.Background(), idp.Email); err == nil {
		t.Error("an operator was created even though provisioning is off")
	}
}

func TestSingleSignOnProvisionsWhenAskedTo(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})

	idp.Claims = []string{"charon-operators"}
	place(t, store, "charon-operators")

	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
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
	if held := roleOf(t, store, created.ID, tenantNamed(t, store, "charon-operators")); held != authz.Least {
		t.Errorf("role = %q, want the least, which is what a name alone gives", held)
	}
}

// A group nothing maps says placement was meant to be decided and has not
// been, so the arrival is refused rather than put wherever there is room. This
// holds even when there is exactly one tenant it could have gone to.
func TestSingleSignOnDoesNotProvisionAnUnmappedIdentity(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})

	idp.Claims = []string{"acme"}
	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", got.finalURL)
	}

	if _, err := store.UserByEmail(context.Background(), idp.Email); err == nil {
		t.Error("an operator arrived with no mapping to say where")
	}
}

func TestSingleSignOnEnforcesTheRequiredClaim(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{
		AutoProvision: true,
		RequiredClaim: "charon-operators",
	})

	idp.Claims = []string{"some-other-team"}
	refused := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.Contains(refused.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", refused.finalURL)
	}

	idp.Claims = []string{"charon-operators"}
	place(t, store, "charon-operators")
	admitted := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.HasSuffix(admitted.finalURL, "/events") {
		t.Errorf("landed on %s, want /events once the group matches", admitted.finalURL)
	}
}

func TestSingleSignOnRefusesACallbackWithoutAHandshake(t *testing.T) {
	t.Parallel()

	_, server, _, client := withSSO(t, auth.Policy{AutoProvision: true})

	got := do(t, client, http.MethodGet,
		server.URL+"/auth/"+auth.OIDC+"/callback?code=made-up&state=made-up", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Errorf("landed on %s, want the login page", got.finalURL)
	}
}

func TestAMethodThatWasNotRegisteredIsNotRouted(t *testing.T) {
	t.Parallel()

	_, server, _, _ := setup(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if got.status != http.StatusNotFound {
		t.Errorf("start = %d, want 404 for a method nobody registered", got.status)
	}

	_, page := get(t, client, server.URL+"/login")
	if strings.Contains(page, "single sign-on") {
		t.Error("the sign-in page offers a method that is not registered")
	}
}

// An identity provider that does not verify addresses is not a broken one: it
// is one where an administrator creates the accounts. Whether that is
// acceptable is the deployment's call, so it is a setting and not a refusal.
func TestAnUnverifiedAddressIsRefusedUnlessTheDeploymentAcceptsIt(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{})
	idp.Verify = false
	idp.Claims = []string{postgres.DefaultSlug}

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if err := store.CreateUser(context.Background(), idp.Email, hash, owner(t, store)); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}

	refused := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.Contains(refused.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", refused.finalURL)
	}

	accepting, acceptingServer, acceptingIDP, acceptingClient := withSSO(t, auth.Policy{
		AcceptUnverifiedEmail: true,
	})
	acceptingIDP.Verify = false
	acceptingIDP.Claims = []string{postgres.DefaultSlug}
	if err := accepting.CreateUser(context.Background(), acceptingIDP.Email, hash,
		owner(t, accepting)); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}

	admitted := do(t, acceptingClient, http.MethodGet,
		acceptingServer.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.HasSuffix(admitted.finalURL, "/events") {
		t.Errorf("landed on %s, want /events once unverified addresses are accepted",
			admitted.finalURL)
	}
}

// Carrying no group looks exactly like a provider that sends none, or sends
// them somewhere this deployment is not looking. None of those say where the
// person belongs, so no account is made from them — not even when there is a
// single tenant it could only have been.
func TestAnArrivalWithNothingMappedIsNotProvisioned(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})
	idp.Claims = nil

	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.Contains(got.finalURL, "/login") {
		t.Fatalf("landed on %s, want the login page", got.finalURL)
	}

	if _, err := store.UserByEmail(context.Background(), idp.Email); err == nil {
		t.Error("an account was made for an identity that said nothing about where it belongs")
	}
}

// Groups are not part of OpenID Connect: providers differ on what they call
// them, and one that namespaces the claim must work as well as one that does
// not.
func TestTheTenantClaimIsWhateverTheProviderCallsIt(t *testing.T) {
	t.Parallel()

	const claim = "https://charon.example/groups"

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

	idp := testsupport.NewIdentityProvider(t)
	idp.TenantClaim = claim
	idp.Claims = []string{"gente-do-plantao"}

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	methods := auth.NewRegistry()
	methods.Register(auth.NewOIDC(auth.OIDCSettings{
		Issuer:       idp.Issuer(),
		ClientID:     "charon",
		ClientSecret: "shh",
		RedirectURL:  server.URL + "/auth/" + auth.OIDC + "/callback",
		TenantClaim:  claim,
	}))
	web.New(store, web.Config{
		Auth:   methods,
		Policy: auth.Policy{AutoProvision: true, RequiredClaim: "gente-do-plantao"},
	}).Register(mux)

	if _, err := store.CreateTenant(ctx, "gente-do-plantao", "Plantao"); err != nil {
		t.Fatalf("creating the tenant the group names: %v", err)
	}

	client := &http.Client{Jar: jar(t)}
	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	created, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if held := roleOf(t, store, created.ID, tenantNamed(t, store, "gente-do-plantao")); held != authz.Least {
		t.Errorf("role = %q, want the least, which is what a name alone gives", held)
	}
}

// Providers nest. Keycloak puts its realm roles under realm_access.roles, and
// a claim is named the way it reads, by its path.
func TestATenantClaimCanBeNested(t *testing.T) {
	t.Parallel()

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

	idp := testsupport.NewIdentityProvider(t)
	idp.TenantClaim = "realm_access.roles"
	idp.Claims = []string{"charon-acme"}

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	methods := auth.NewRegistry()
	methods.Register(auth.NewOIDC(auth.OIDCSettings{
		Issuer:       idp.Issuer(),
		ClientID:     "charon",
		ClientSecret: "shh",
		RedirectURL:  server.URL + "/auth/" + auth.OIDC + "/callback",
		TenantClaim:  "realm_access.roles",
	}))
	if err := store.Register(ctx, postgres.Kinds{Methods: methods.Names()}); err != nil {
		t.Fatalf("registering the ways of signing in: %v", err)
	}
	web.New(store, web.Config{Auth: methods, Policy: auth.Policy{AutoProvision: true}}).Register(mux)

	if _, err := store.CreateTenant(ctx, "charon-acme", "Acme"); err != nil {
		t.Fatalf("creating the tenant the claim names: %v", err)
	}

	client := &http.Client{Jar: jar(t)}
	got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	created, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if held := roleOf(t, store, created.ID, tenantNamed(t, store, "charon-acme")); held != authz.Least {
		t.Errorf("role = %q, want the least, which is what a name alone gives", held)
	}
}

// The token says where somebody belongs, so it is applied whole at every
// sign-in: a value that stops arriving takes the tenant with it. What it says
// nothing about — the role held there — is decided here and survives.
func TestWhatTheProviderStopsSayingIsWithdrawn(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})
	ctx := context.Background()

	place(t, store, "first-tenant")
	place(t, store, "second-tenant")
	idp.Claims = []string{"first-tenant", "second-tenant"}

	if got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	arrived, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if held := mustMemberships(t, store, arrived.ID); len(held) != 2 {
		t.Fatalf("belongs to %d tenants, want the 2 the provider named", len(held))
	}

	// The role held in a tenant is decided here, and the token says nothing
	// about roles, so it survives.
	if err := store.Join(ctx, arrived.ID, console.Placement{
		Tenant: tenantNamed(t, store, "first-tenant"), Role: "operator",
	}); err != nil {
		t.Fatalf("raising the role by hand: %v", err)
	}

	idp.Claims = []string{"first-tenant"}
	do(t, client, http.MethodPost, server.URL+"/logout", nil)
	if got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	held := mustMemberships(t, store, arrived.ID)
	slugs := make([]string, 0, len(held))
	for _, one := range held {
		slugs = append(slugs, one.Slug)
	}
	sort.Strings(slugs)

	if len(slugs) != 1 || slugs[0] != "first-tenant" {
		t.Errorf("belongs to %v, want only the one the token still names", slugs)
	}
	if role := roleOf(t, store, arrived.ID, tenantNamed(t, store, "first-tenant")); role != "operator" {
		t.Errorf("role = %q, want the one given here, which the token says nothing about", role)
	}
}

// A value is a path: the tenant, and the role held there when the provider
// says one. Keycloak writes nested groups that way, and so can any provider
// that lets a group be named.
func TestAPathNamesTheTenantAndTheRole(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})
	ctx := context.Background()

	place(t, store, "acme")
	place(t, store, "globex")
	idp.Claims = []string{"/acme/operator", "/globex"}

	if got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	arrived, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}

	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "acme")); held != "operator" {
		t.Errorf("in acme the role is %q, want the one the path names", held)
	}
	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "globex")); held != authz.Least {
		t.Errorf("in globex the role is %q, want the least, since the path names none", held)
	}

	// The provider says the role now, so changing it there changes it here.
	idp.Claims = []string{"/acme/viewer", "/globex"}
	do(t, client, http.MethodPost, server.URL+"/logout", nil)
	do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil)

	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "acme")); held != "viewer" {
		t.Errorf("in acme the role is %q after the provider changed it, want viewer", held)
	}
}

// A role the provider names that nothing here knows leaves somebody at the
// least rather than shutting them out over a name.
func TestAnUnknownRoleInAPathLeavesTheLeast(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})

	place(t, store, "acme")
	idp.Claims = []string{"/acme/wizard"}

	if got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	arrived, err := store.UserByEmail(context.Background(), idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "acme")); held != authz.Least {
		t.Errorf("role = %q, want the least when the provider names one nothing knows", held)
	}
}

// The convention covers a deployment whose groups are named after its tenants.
// A directory that names them its own way points the value at a tenant, and
// what is pointed wins over what the name would have said.
func TestAValuePointedAtATenantWinsOverItsName(t *testing.T) {
	t.Parallel()

	store, server, idp, client := withSSO(t, auth.Policy{AutoProvision: true})
	ctx := context.Background()

	place(t, store, "acme")
	place(t, store, "globex")

	// Named their way, meaning nothing here.
	if err := store.PointValueAt(ctx, auth.OIDC, "SEC-CHARON-PROD-ADMINS",
		console.Placement{Tenant: tenantNamed(t, store, "acme"), Role: "admin"}); err != nil {
		t.Fatalf("pointing the value: %v", err)
	}

	idp.Claims = []string{"SEC-CHARON-PROD-ADMINS", "/globex/operator"}

	if got := do(t, client, http.MethodGet, server.URL+"/auth/"+auth.OIDC+"/start", nil); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	arrived, err := store.UserByEmail(ctx, idp.Email)
	if err != nil {
		t.Fatalf("the operator was not provisioned: %v", err)
	}
	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "acme")); held != "admin" {
		t.Errorf("the pointed value put them in acme as %q, want admin", held)
	}
	if held := roleOf(t, store, arrived.ID, tenantNamed(t, store, "globex")); held != "operator" {
		t.Errorf("the path put them in globex as %q, want operator", held)
	}
}
