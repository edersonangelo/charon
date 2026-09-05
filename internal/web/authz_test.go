package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
)

const viewerEmail = "viewer@example.com"

// A role is only worth having if the panel obeys it, on the page and at the
// route: hiding a button that the handler would still accept is decoration.
func TestARoleDecidesWhatThePanelOffersAndAccepts(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	ctx := context.Background()

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	place := owner(t, store)
	place.Role = "viewer"
	if err := store.CreateUser(ctx, viewerEmail, hash, place); err != nil {
		t.Fatalf("creating the viewer: %v", err)
	}

	got := do(t, client, http.MethodPost, server.URL+"/auth/"+auth.Password, url.Values{
		"email":    {viewerEmail},
		"password": {password},
	})
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	status, page := get(t, client, server.URL+"/events")
	if status != http.StatusOK {
		t.Fatalf("the events page is not reachable: status %d", status)
	}
	if strings.Contains(page, "/replay") {
		t.Error("the page offers a replay to a role that may not replay")
	}

	refused := do(t, client, http.MethodPost,
		server.URL+"/events/"+eventID.String()+"/replay", nil)
	if refused.status != http.StatusForbidden {
		t.Errorf("replay = %d, want %d", refused.status, http.StatusForbidden)
	}

	// A viewer reads the configuration pages and changes nothing on them.
	status, verification := get(t, client, server.URL+"/verification")
	if status != http.StatusOK {
		t.Fatalf("the verification page = %d, want %d", status, http.StatusOK)
	}
	if strings.Contains(verification, "Verify a provider") {
		t.Error("the page offers a form the role may not submit")
	}

	changed := do(t, client, http.MethodPost, server.URL+"/verification", url.Values{
		"provider":   {"stripe"},
		"preset":     {"stripe"},
		"secret-env": {"CHARON_SECRET_STRIPE"},
	})
	if changed.status != http.StatusForbidden {
		t.Errorf("configuring verification = %d, want %d", changed.status, http.StatusForbidden)
	}
}

// The permissions a role grants are configuration, so a deployment can invent
// a role the code has never heard of and the panel obeys it at once.
func TestARoleDefinedByTheDeploymentIsObeyed(t *testing.T) {
	t.Parallel()

	store, server, client, eventID := setup(t)
	ctx := context.Background()

	scope := authz.WithTenant(ctx, owner(t, store).Tenant)
	if err := store.SetRole(scope, authz.Role{
		Name:        "resender",
		Description: "sends events again and looks at nothing else",
		Grants:      []authz.Permission{authz.EventsRead, authz.EventsReplay},
	}); err != nil {
		t.Fatalf("defining the role: %v", err)
	}

	hash, err := console.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	place := owner(t, store)
	place.Role = "resender"
	if err := store.CreateUser(ctx, viewerEmail, hash, place); err != nil {
		t.Fatalf("creating the operator: %v", err)
	}

	got := do(t, client, http.MethodPost, server.URL+"/auth/"+auth.Password, url.Values{
		"email":    {viewerEmail},
		"password": {password},
	})
	if !strings.HasSuffix(got.finalURL, "/events") {
		t.Fatalf("landed on %s, want /events", got.finalURL)
	}

	replayed := do(t, client, http.MethodPost,
		server.URL+"/events/"+eventID.String()+"/replay", nil)
	if replayed.status/100 != 2 && replayed.status/100 != 3 {
		t.Errorf("replay = %d, want it accepted", replayed.status)
	}

	if status, _ := get(t, client, server.URL+"/routes"); status != http.StatusForbidden {
		t.Errorf("the routes page = %d, want %d", status, http.StatusForbidden)
	}
}
