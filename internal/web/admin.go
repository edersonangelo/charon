package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
)

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, "Settings", settingsPage())
}

func (h *Handler) operators(w http.ResponseWriter, r *http.Request) {
	h.renderOperators(w, r)
}

func (h *Handler) renderOperators(w http.ResponseWriter, r *http.Request) {
	operators, err := h.store.Operators(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	roles, err := h.store.Roles(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	admins, err := h.store.SystemAdmins(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, "Operators", operatorsPage(operators, admins, roles))
}

func (h *Handler) roles(w http.ResponseWriter, r *http.Request) {
	h.renderRoles(w, r)
}

func (h *Handler) renderRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.store.Roles(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pointed, err := h.store.PointedHere(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, "Roles", rolesPage(roles, pointed, h.auth.Names()))
}

func (h *Handler) createOperator(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	email := strings.TrimSpace(r.PostForm.Get("email"))
	role := strings.TrimSpace(r.PostForm.Get("role"))
	password := r.PostForm.Get("password")
	if email == "" || role == "" {
		http.Error(w, "an email and a role are required", http.StatusBadRequest)
		return
	}

	// How strong a password is belongs to whoever chooses it. What the schema
	// requires is that an account has a way in at all: a password here, or a
	// linked subject, which single sign-on adds when the operator arrives.
	if password == "" {
		http.Error(w, "a password is required", http.StatusBadRequest)
		return
	}
	hash, err := console.HashPassword(password)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// The tenant being administered, not the one the operator came from: a
	// system administrator adds people to the tenant they are inside.
	place := console.Placement{Tenant: inTenant(r), Role: role}
	if err := h.store.CreateUser(r.Context(), email, hash, place); err != nil {
		http.Error(w, "could not create that operator: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderOperators(w, r)
}

func (h *Handler) setOperatorRole(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	// Taking your own last permission away would lock the tenant out of its
	// own administration, and only another operator could undo it.
	if id == currentUser(r).ID {
		http.Error(w, "an operator cannot change their own role", http.StatusConflict)
		return
	}

	if err := h.store.SetOperatorRole(r.Context(), id, r.PostForm.Get("role")); err != nil {
		http.Error(w, "could not change that role: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderOperators(w, r)
}

func (h *Handler) deleteOperator(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if id == currentUser(r).ID {
		http.Error(w, "an operator cannot remove their own account", http.StatusConflict)
		return
	}

	if err := h.store.DeleteOperator(r.Context(), id); err != nil {
		http.Error(w, "could not remove that operator: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderOperators(w, r)
}

func (h *Handler) saveRole(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		http.Error(w, "a name is required", http.StatusBadRequest)
		return
	}

	granted := make([]authz.Permission, 0, len(r.PostForm["grant"]))
	for _, one := range r.PostForm["grant"] {
		granted = append(granted, authz.Permission(one))
	}

	if err := h.store.SetRole(r.Context(), authz.Role{
		Name:        name,
		Description: strings.TrimSpace(r.PostForm.Get("description")),
		Grants:      granted,
	}); err != nil {
		http.Error(w, "could not save that role: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.renderRoles(w, r)
}

// pointValue is the exception to the convention: a value the provider sends
// that names no tenant of ours is said to mean this one.
func (h *Handler) pointValue(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	method := strings.TrimSpace(r.PostForm.Get("method"))
	value := strings.TrimSpace(r.PostForm.Get("value"))
	if method == "" || value == "" {
		http.Error(w, "a value and a way of signing in are required", http.StatusBadRequest)
		return
	}
	if !slices.Contains(h.auth.Names(), method) {
		http.Error(w, "no way of signing in goes by that name", http.StatusBadRequest)
		return
	}

	if err := h.store.PointValueAt(r.Context(), method, value, console.Placement{
		Tenant: inTenant(r), Role: strings.TrimSpace(r.PostForm.Get("role")),
	}); err != nil {
		http.Error(w, "could not point that value: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderRoles(w, r)
}

func (h *Handler) stopPointingValue(w http.ResponseWriter, r *http.Request) {
	if err := h.store.StopPointingValue(r.Context(),
		r.PathValue("method"), r.PathValue("value")); err != nil {
		h.fail(w, r, err)
		return
	}
	h.renderRoles(w, r)
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		http.Error(w, "could not remove that role: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderRoles(w, r)
}

func (h *Handler) tenants(w http.ResponseWriter, r *http.Request) {
	h.renderTenants(w, r)
}

func (h *Handler) renderTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.store.Tenants(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	isolation, err := h.store.Isolation(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, "Tenants", tenantsPage(tenants, inTenant(r), isolation))
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	slug := strings.TrimSpace(r.PostForm.Get("slug"))
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if slug == "" {
		http.Error(w, "a slug is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = slug
	}

	if _, err := h.store.CreateTenant(r.Context(), slug, name); err != nil {
		http.Error(w, "could not create that tenant: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderTenants(w, r)
}

func (h *Handler) deleteTenant(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteTenant(r.Context(), r.PathValue("slug")); err != nil {
		http.Error(w, "could not remove that tenant: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderTenants(w, r)
}

// switchTenant is how somebody who belongs to more than one tenant moves
// between them. Nothing reads across tenants: the request simply runs in
// another one of theirs.
func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	slug := strings.TrimSpace(r.PostForm.Get("tenant"))
	reachable := h.reachable(r, currentUser(r))
	if !slices.ContainsFunc(reachable, func(m console.Membership) bool { return m.Slug == slug }) {
		http.Error(w, "you do not belong to a tenant by that name", http.StatusForbidden)
		return
	}

	//nolint:gosec // Secure is configurable: the panel also runs over plain HTTP locally
	http.SetCookie(w, &http.Cookie{
		Name:     tenantCookie,
		Value:    slug,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// back is the page the switch was made from, reduced to a path of this panel.
// Whatever the header carries, nothing outside here is ever redirected to.
func back(r *http.Request) string {
	came, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || !strings.HasPrefix(came.EscapedPath(), "/") {
		return "/events"
	}
	return came.EscapedPath()
}

// makeSystemAdmin takes an operator of this tenant out of it. The form names
// the account, because the account is about to stop being listed here.
func (h *Handler) makeSystemAdmin(w http.ResponseWriter, r *http.Request) {
	if !currentUser(r).SystemAdmin {
		http.Error(w, "only a system administrator makes another", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	id, err := uuid.Parse(r.PostForm.Get("id"))
	if err != nil {
		http.Error(w, "no operator was named", http.StatusBadRequest)
		return
	}
	if err := h.store.SetSystemAdmin(r.Context(), id); err != nil {
		http.Error(w, "could not change that: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderOperators(w, r)
}

// Only somebody who already is one may make another, so the standing cannot be
// granted from inside a tenant by whoever happens to administer it.
func (h *Handler) setSystemAdmin(w http.ResponseWriter, r *http.Request) {
	if !currentUser(r).SystemAdmin {
		http.Error(w, "only a system administrator makes another", http.StatusForbidden)
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if id == currentUser(r).ID {
		http.Error(w, "an operator cannot change their own standing", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	if r.PostForm.Get("system") == "on" {
		if err := h.store.SetSystemAdmin(r.Context(), id); err != nil {
			http.Error(w, "could not change that: "+err.Error(), http.StatusConflict)
			return
		}
		h.renderOperators(w, r)
		return
	}

	// Giving the standing up leaves the account where its memberships put it,
	// which may be nowhere: so it joins the tenant being administered, as the
	// role that was chosen.
	if err := h.store.ClearSystemAdmin(r.Context(), id); err != nil {
		http.Error(w, "could not change that: "+err.Error(), http.StatusConflict)
		return
	}
	if err := h.store.Join(r.Context(), id, console.Placement{
		Tenant: inTenant(r), Role: strings.TrimSpace(r.PostForm.Get("role")),
	}); err != nil {
		http.Error(w, "could not change that: "+err.Error(), http.StatusConflict)
		return
	}
	h.renderOperators(w, r)
}
