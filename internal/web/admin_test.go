package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/inbound"
)

// Administration that only exists on the command line is administration the
// panel does not have. These are the pages that answer "who can sign in, as
// what, and where does single sign-on land".
func TestAnOwnerAdministersOperatorsFromThePanel(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	if got := signIn(t, server, client, password); !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	status, page := get(t, client, server.URL+"/operators")
	if status != http.StatusOK {
		t.Fatalf("the operators page = %d, want %d", status, http.StatusOK)
	}
	if !strings.Contains(page, email) {
		t.Error("the page does not list the operator signed in")
	}

	added := do(t, client, http.MethodPost, server.URL+"/operators", url.Values{
		"email":    {"nova@example.com"},
		"role":     {"viewer"},
		"password": {"uma senha boa"},
	})
	if added.status != http.StatusOK {
		t.Fatalf("adding an operator = %d, want %d", added.status, http.StatusOK)
	}

	created, err := store.UserByEmail(ctx, "nova@example.com")
	if err != nil {
		t.Fatalf("the operator was not created: %v", err)
	}
	if held := roleOf(t, store, created.ID, owner(t, store).Tenant); held != "viewer" {
		t.Errorf("created as %q in the tenant being administered, want viewer", held)
	}

	promoted := do(t, client, http.MethodPost,
		server.URL+"/operators/"+created.ID.String()+"/role", url.Values{"role": {"operator"}})
	if promoted.status != http.StatusOK {
		t.Fatalf("changing a role = %d, want %d", promoted.status, http.StatusOK)
	}
	if after, _ := store.UserByEmail(ctx, "nova@example.com"); roleOf(t, store, after.ID, owner(t, store).Tenant) != "operator" {
		t.Errorf("role = %q, want operator", roleOf(t, store, after.ID, owner(t, store).Tenant))
	}

	// How strong a password is belongs to whoever chooses it; the panel only
	// refuses an account with no way in at all.
	short := do(t, client, http.MethodPost, server.URL+"/operators", url.Values{
		"email": {"curta@example.com"}, "role": {"viewer"}, "password": {"x"},
	})
	if short.status != http.StatusOK {
		t.Errorf("a short password = %d, want it accepted", short.status)
	}
	none := do(t, client, http.MethodPost, server.URL+"/operators", url.Values{
		"email": {"vazia@example.com"}, "role": {"viewer"}, "password": {""},
	})
	if none.status != http.StatusBadRequest {
		t.Errorf("no password at all = %d, want %d", none.status, http.StatusBadRequest)
	}

	// Removing takes somebody out of this tenant. The account is not deleted:
	// it may belong to others, and where it belongs is a relationship rather
	// than a property of the person.
	removed := do(t, client, http.MethodPost,
		server.URL+"/operators/"+created.ID.String()+"/delete", url.Values{})
	if removed.status != http.StatusOK {
		t.Fatalf("removing an operator = %d, want %d", removed.status, http.StatusOK)
	}
	if held := mustMemberships(t, store, created.ID); len(held) != 0 {
		t.Errorf("still belongs to %v after being removed from the tenant", held)
	}
	if _, err := store.UserByEmail(ctx, "nova@example.com"); err != nil {
		t.Errorf("the account was deleted, and only the membership should have been: %v", err)
	}
}

// Locking yourself out is one click, and only somebody else could undo it.
func TestAnOperatorCannotChangeOrRemoveTheirOwnAccount(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)

	signIn(t, server, client, password)
	me, err := store.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("reading the operator: %v", err)
	}

	demoted := do(t, client, http.MethodPost,
		server.URL+"/operators/"+me.ID.String()+"/role", url.Values{"role": {"viewer"}})
	if demoted.status != http.StatusConflict {
		t.Errorf("changing your own role = %d, want %d", demoted.status, http.StatusConflict)
	}

	deleted := do(t, client, http.MethodPost,
		server.URL+"/operators/"+me.ID.String()+"/delete", url.Values{})
	if deleted.status != http.StatusConflict {
		t.Errorf("removing your own account = %d, want %d", deleted.status, http.StatusConflict)
	}
}

func TestRolesAreConfiguredFromThePanel(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	defined := do(t, client, http.MethodPost, server.URL+"/roles", url.Values{
		"name":        {"resender"},
		"description": {"sends events again"},
		"grant":       {"events.read", "events.replay"},
	})
	if defined.status != http.StatusOK {
		t.Fatalf("defining a role = %d, want %d", defined.status, http.StatusOK)
	}

	scope := scoped(t, store)
	role, err := store.Role(scope, "resender")
	if err != nil {
		t.Fatalf("the role was not stored: %v", err)
	}
	if len(role.Grants) != 2 {
		t.Errorf("the role grants %v, want exactly what was ticked", role.Grants)
	}

	refused := do(t, client, http.MethodPost, server.URL+"/roles", url.Values{
		"name":  {"broken"},
		"grant": {"events.teleport"},
	})
	if refused.status != http.StatusBadRequest {
		t.Errorf("a permission nothing enforces = %d, want %d", refused.status, http.StatusBadRequest)
	}

	kept := do(t, client, http.MethodPost, server.URL+"/roles/owner/delete", url.Values{})
	if kept.status != http.StatusConflict {
		t.Errorf("removing a shipped role = %d, want %d", kept.status, http.StatusConflict)
	}
}

// A viewer may look at who can sign in and change none of it.
func TestARoleWithoutOperatorsWriteChangesNothing(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	place := owner(t, store)
	place.Role = "viewer"
	if err := store.CreateUser(ctx, "olhos@example.com", hash, place); err != nil {
		t.Fatalf("creating the viewer: %v", err)
	}

	got := do(t, client, http.MethodPost, server.URL+"/auth/password", url.Values{
		"email":    {"olhos@example.com"},
		"password": {password},
	})
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	status, page := get(t, client, server.URL+"/operators")
	if status != http.StatusOK {
		t.Fatalf("the operators page = %d, want %d", status, http.StatusOK)
	}
	if strings.Contains(page, "Add an operator") {
		t.Error("the page offers a form the role may not submit")
	}
	if _, roles := get(t, client, server.URL+"/roles"); strings.Contains(roles, "Define a role") {
		t.Error("the roles page offers a form the role may not submit")
	}
	if strings.Contains(page, `href="/tenants"`) {
		t.Error("the page links to tenants without the permission for it")
	}

	blocked := do(t, client, http.MethodPost, server.URL+"/operators", url.Values{
		"email": {"outra@example.com"}, "role": {"owner"}, "password": {"uma senha boa"},
	})
	if blocked.status != http.StatusForbidden {
		t.Errorf("adding an operator = %d, want %d", blocked.status, http.StatusForbidden)
	}
	if status, _ := get(t, client, server.URL+"/tenants"); status != http.StatusForbidden {
		t.Errorf("the tenants page = %d, want %d", status, http.StatusForbidden)
	}
}

// Configuration is one place, and it shows only the parts the role may open.
func TestSettingsListsWhatTheRoleMayConfigure(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	signIn(t, server, client, password)

	status, page := get(t, client, server.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("the settings page = %d, want %d", status, http.StatusOK)
	}
	for _, path := range []string{"/routes", "/verification", "/operators", "/roles", "/tenants"} {
		if !strings.Contains(page, `href="`+path+`"`) {
			t.Errorf("settings does not offer %s to an owner", path)
		}
	}

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	place := owner(t, store)
	place.Role = "viewer"
	if err := store.CreateUser(context.Background(), "olha@example.com", hash, place); err != nil {
		t.Fatalf("creating the viewer: %v", err)
	}

	viewer := &http.Client{Jar: jar(t)}
	do(t, viewer, http.MethodPost, server.URL+"/auth/password", url.Values{
		"email": {"olha@example.com"}, "password": {password},
	})

	_, seen := get(t, viewer, server.URL+"/settings")
	if !strings.Contains(seen, `href="/routes"`) {
		t.Error("a viewer cannot reach the routes page it may read")
	}
	if strings.Contains(seen, `href="/tenants"`) {
		t.Error("settings offers a page the role may not open")
	}
}

// The standing that reaches every tenant is only ever handed over by somebody
// who already has it: an owner administering their own tenant must not be able
// to promote themselves out of it.
func TestOnlyASystemAdministratorMakesAnother(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if err := store.CreateUser(ctx, "alvo@example.com", hash, owner(t, store)); err != nil {
		t.Fatalf("creating the other operator: %v", err)
	}
	target, err := store.UserByEmail(ctx, "alvo@example.com")
	if err != nil {
		t.Fatalf("reading the other operator: %v", err)
	}

	signIn(t, server, client, password)
	refused := do(t, client, http.MethodPost,
		server.URL+"/operators/"+target.ID.String()+"/system", url.Values{"system": {"on"}})
	if refused.status != http.StatusForbidden {
		t.Fatalf("an owner granting it = %d, want %d", refused.status, http.StatusForbidden)
	}

	if err := store.SetSystemAdminByEmail(ctx, email); err != nil {
		t.Fatalf("making the first system administrator: %v", err)
	}

	granted := do(t, client, http.MethodPost, server.URL+"/operators/system",
		url.Values{"id": {target.ID.String()}, "system": {"on"}})
	if granted.status != http.StatusOK {
		t.Fatalf("a system administrator granting it = %d, want %d", granted.status, http.StatusOK)
	}
	after, err := store.UserByEmail(ctx, "alvo@example.com")
	if err != nil || !after.SystemAdmin {
		t.Errorf("the standing was not granted: %v", err)
	}
}

// Reaching every tenant is done by acting inside one at a time, so no query
// ever reads across them.
func TestASystemAdministratorActsInsideAnotherTenant(t *testing.T) {
	t.Parallel()

	store, server, client, ours := setup(t)
	ctx := context.Background()

	second, err := store.CreateTenant(ctx, "second", "Second")
	if err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}
	theirs, err := store.Record(authz.WithTenant(ctx, second.ID), inbound.Request{
		Tenant: second.ID, Provider: "shopify", Path: "/webhooks/second/shopify",
		ReceivedAt: time.Now().UTC(), Headers: map[string][]string{}, Body: body,
	})
	if err != nil {
		t.Fatalf("recording for the second tenant: %v", err)
	}

	if err := store.SetSystemAdminByEmail(ctx, email); err != nil {
		t.Fatalf("making a system administrator: %v", err)
	}
	signIn(t, server, client, password)

	_, own := get(t, client, server.URL+"/events")
	if !strings.Contains(own, ours.String()) || strings.Contains(own, theirs.String()) {
		t.Fatal("the events page does not start in the operator's own tenant")
	}

	switched := do(t, client, http.MethodPost, server.URL+"/acting-tenant",
		url.Values{"tenant": {"second"}})
	if switched.status/100 != 2 && switched.status/100 != 3 {
		t.Fatalf("switching tenant = %d, want it accepted", switched.status)
	}

	_, other := get(t, client, server.URL+"/events")
	if !strings.Contains(other, theirs.String()) || strings.Contains(other, ours.String()) {
		t.Error("after switching, the page does not show that tenant and only it")
	}
}

// Somebody confined to a tenant cannot leave it by asking.
func TestSwitchingTenantIsRefusedToEveryoneElse(t *testing.T) {
	t.Parallel()

	store, server, client, ours := setup(t)

	if _, err := store.CreateTenant(context.Background(), "second", "Second"); err != nil {
		t.Fatalf("creating the second tenant: %v", err)
	}
	signIn(t, server, client, password)

	refused := do(t, client, http.MethodPost, server.URL+"/acting-tenant",
		url.Values{"tenant": {"second"}})
	if refused.status != http.StatusForbidden {
		t.Errorf("switching tenant = %d, want %d", refused.status, http.StatusForbidden)
	}

	if _, page := get(t, client, server.URL+"/events"); !strings.Contains(page, ours.String()) {
		t.Error("the operator was moved out of their own tenant")
	}
}

// Tenants are Charon's to keep apart, so a plain deployment creates one and
// uses it. The database can be made to enforce the same rule underneath, and
// nothing waits on that.
func TestATenantIsCreatedAndUsedOnAnOrdinaryDeployment(t *testing.T) {
	t.Parallel()

	store, server, client, ours := setup(t)
	signIn(t, server, client, password)

	created := do(t, client, http.MethodPost, server.URL+"/tenants", url.Values{
		"slug": {"acme"}, "name": {"Acme Inc"},
	})
	if created.status != http.StatusOK {
		t.Fatalf("creating a tenant = %d, want %d", created.status, http.StatusOK)
	}

	ctx := context.Background()
	acme, err := store.TenantBySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("the tenant was not created: %v", err)
	}

	roles, err := store.Roles(authz.WithTenant(ctx, acme.ID))
	if err != nil || len(roles) != len(authz.BuiltIn()) {
		t.Fatalf("the new tenant has %d roles, want the shipped ones (%v)", len(roles), err)
	}

	theirs, err := store.Record(ctx, inbound.Request{
		Tenant: acme.ID, Provider: "shopify", Path: "/webhooks/acme/shopify",
		ReceivedAt: time.Now().UTC(), Headers: map[string][]string{}, Body: body,
	})
	if err != nil {
		t.Fatalf("recording for the new tenant: %v", err)
	}

	// The operator belongs to the first tenant, so that is all they see.
	_, page := get(t, client, server.URL+"/events")
	if !strings.Contains(page, ours.String()) {
		t.Error("the operator lost sight of their own events")
	}
	if strings.Contains(page, theirs.String()) {
		t.Error("an event of another tenant is visible")
	}
}

// Somebody added on the operators page belongs to the tenant being
// administered, which for a system administrator is the one they switched to
// and not the one they came from.
func TestAnOperatorIsAddedToTheTenantBeingAdministered(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	acme, err := store.CreateTenant(ctx, "acme", "Acme Inc")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	if err := store.SetSystemAdminByEmail(ctx, email); err != nil {
		t.Fatalf("making a system administrator: %v", err)
	}
	signIn(t, server, client, password)

	do(t, client, http.MethodPost, server.URL+"/acting-tenant", url.Values{"tenant": {"acme"}})

	added := do(t, client, http.MethodPost, server.URL+"/operators", url.Values{
		"email": {"chefe@acme.example"}, "role": {"owner"}, "password": {"uma senha boa"},
	})
	if added.status != http.StatusOK {
		t.Fatalf("adding an operator = %d, want %d", added.status, http.StatusOK)
	}

	created, err := store.UserByEmail(ctx, "chefe@acme.example")
	if err != nil {
		t.Fatalf("the operator was not created: %v", err)
	}
	held := mustMemberships(t, store, created.ID)
	if len(held) != 1 || held[0].Tenant != acme.ID {
		t.Fatalf("the operator belongs to %v, want only the tenant being administered", held)
	}
	if roleOf(t, store, created.ID, acme.ID) != "owner" {
		t.Errorf("role = %q, want owner", roleOf(t, store, created.ID, acme.ID))
	}

	// And the operators page of that tenant is where they show up.
	if _, page := get(t, client, server.URL+"/operators"); !strings.Contains(page, "chefe@acme.example") {
		t.Error("the new operator is not listed in the tenant they were added to")
	}
}

// The standing is not belonging to a tenant, so an account that has it appears
// in no tenant's list of operators and carries no role.
func TestASystemAdministratorBelongsToNoTenant(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	if _, err := store.CreateTenant(ctx, "acme", "Acme Inc"); err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	if err := store.SetSystemAdminByEmail(ctx, email); err != nil {
		t.Fatalf("making a system administrator: %v", err)
	}

	me, err := store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	if held := mustMemberships(t, store, me.ID); len(held) != 0 {
		t.Errorf("the account still belongs to %d tenants, want none", len(held))
	}

	signIn(t, server, client, password)
	for _, slug := range []string{"default", "acme"} {
		do(t, client, http.MethodPost, server.URL+"/acting-tenant", url.Values{"tenant": {slug}})
		_, page := get(t, client, server.URL+"/operators")

		// Between the page title and the section that lists the accounts
		// belonging to no tenant is this tenant's own list, which is what must
		// not name them. The header names them on every page and says nothing
		// about where they belong.
		_, body, found := strings.Cut(page, "<h1>Operators</h1>")
		if !found {
			t.Fatalf("the operators page of %s did not render", slug)
		}
		operators, admins, split := strings.Cut(body, "System administrators")
		if !split {
			t.Fatalf("the accounts that belong to no tenant are not listed in %s", slug)
		}
		if strings.Contains(operators, email) {
			t.Errorf("the account is listed among the operators of %s", slug)
		}
		if !strings.Contains(admins, email) {
			t.Errorf("the account is missing from the system administrators seen in %s", slug)
		}
	}
}

// The premise of belonging: a person can hold a place in several tenants, with
// a different role in each, and sees one at a time.
func TestSomebodyCanHoldADifferentRoleInEachTenant(t *testing.T) {
	t.Parallel()

	store, server, client, ours := setup(t)
	ctx := context.Background()

	acme, err := store.CreateTenant(ctx, "acme", "Acme Inc")
	if err != nil {
		t.Fatalf("creating the tenant: %v", err)
	}
	theirs, err := store.Record(ctx, inbound.Request{
		Tenant: acme.ID, Provider: "shopify", Path: "/webhooks/acme/shopify",
		ReceivedAt: time.Now().UTC(), Headers: map[string][]string{}, Body: body,
	})
	if err != nil {
		t.Fatalf("recording for the second tenant: %v", err)
	}

	me, err := store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	// Owner where they started, and the least where they were merely added.
	if err := store.Join(ctx, me.ID, console.Placement{Tenant: acme.ID}); err != nil {
		t.Fatalf("joining the second tenant: %v", err)
	}

	signIn(t, server, client, password)

	if held := mustMemberships(t, store, me.ID); len(held) != 2 {
		t.Fatalf("belongs to %d tenants, want 2", len(held))
	}

	// In their own tenant they are an owner, so replay is offered and taken.
	replayed := do(t, client, http.MethodPost,
		server.URL+"/events/"+ours.String()+"/replay", url.Values{})
	if replayed.status/100 != 2 {
		t.Errorf("replay as owner = %d, want it accepted", replayed.status)
	}

	// In the other they hold no role, which is the least: they read and no more.
	do(t, client, http.MethodPost, server.URL+"/acting-tenant", url.Values{"tenant": {"acme"}})

	_, page := get(t, client, server.URL+"/events")
	if !strings.Contains(page, theirs.String()) || strings.Contains(page, ours.String()) {
		t.Error("after switching, the page does not show that tenant and only it")
	}
	refused := do(t, client, http.MethodPost,
		server.URL+"/events/"+theirs.String()+"/replay", url.Values{})
	if refused.status != http.StatusForbidden {
		t.Errorf("replay where they hold the least = %d, want %d",
			refused.status, http.StatusForbidden)
	}
}

// Belonging to no tenant reaches nothing, rather than reaching the first one.
func TestBelongingNowhereReachesNothing(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	ctx := context.Background()

	me, err := store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	signIn(t, server, client, password)

	if err := store.LeaveTenant(ctx, me.ID, owner(t, store).Tenant); err != nil {
		t.Fatalf("removing the membership: %v", err)
	}

	if status, _ := get(t, client, server.URL+"/events"); status != http.StatusForbidden {
		t.Errorf("the events page = %d, want %d", status, http.StatusForbidden)
	}
}
