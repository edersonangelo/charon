package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

//go:embed static
var assets embed.FS

const (
	sessionCookie = "charon_session"
	// A system administrator acts inside one tenant at a time, and this says
	// which. Everything is still scoped to a tenant, so nothing has to read
	// across them.
	tenantCookie = "charon_tenant"
)

type Store interface {
	UserByEmail(ctx context.Context, email string) (console.User, error)
	VerifyPassword(ctx context.Context, email, password string) (string, error)
	CreateSession(ctx context.Context, digest []byte, userID uuid.UUID, expires time.Time) error
	SessionUser(ctx context.Context, digest []byte) (console.User, error)
	DeleteSession(ctx context.Context, digest []byte) error
	UserBySubject(
		ctx context.Context, subject, email string, place console.Placement, provision bool,
	) (console.User, error)
	Role(ctx context.Context, name string) (authz.Role, error)
	Roles(ctx context.Context) ([]authz.Role, error)
	SetRole(ctx context.Context, role authz.Role) error
	PointedHere(ctx context.Context) ([]console.Mapping, error)
	PointValueAt(ctx context.Context, method, value string, place console.Placement) error
	StopPointingValue(ctx context.Context, method, value string) error
	DeleteRole(ctx context.Context, name string) error
	Operators(ctx context.Context) ([]console.Operator, error)
	SystemAdmins(ctx context.Context) ([]console.Operator, error)
	SetSystemAdmin(ctx context.Context, id uuid.UUID) error
	ClearSystemAdmin(ctx context.Context, id uuid.UUID) error
	CreateUser(ctx context.Context, email, passwordHash string, place console.Placement) error
	SetOperatorRole(ctx context.Context, id uuid.UUID, role string) error
	DeleteOperator(ctx context.Context, id uuid.UUID) error
	Tenants(ctx context.Context) ([]console.Tenant, error)
	Memberships(ctx context.Context, user uuid.UUID) ([]console.Membership, error)
	RoleIn(ctx context.Context, user, tenant uuid.UUID) (string, bool, error)
	Join(ctx context.Context, user uuid.UUID, place console.Placement) error
	Settle(ctx context.Context, user uuid.UUID, named []console.Placement) error
	Isolation(ctx context.Context) (postgres.Isolation, error)
	CreateTenant(ctx context.Context, slug, name string) (console.Tenant, error)
	DeleteTenant(ctx context.Context, slug string) error
	Placement(ctx context.Context, method string, claims []string) ([]console.Placement, error)
	SearchEvents(ctx context.Context, filter console.Filter) ([]console.EventSummary, error)
	Providers(ctx context.Context) ([]string, error)
	EventDetail(ctx context.Context, id uuid.UUID) (console.EventDetail, error)
	EventDeliveries(ctx context.Context, id uuid.UUID) ([]console.Delivery, error)
	ReplayEvent(ctx context.Context, id uuid.UUID) (int64, error)
	ReplayDelivery(ctx context.Context, id uuid.UUID) (int64, error)
	UnroutedProviders(ctx context.Context) ([]console.UnroutedProvider, error)
	DetailedRoutes(ctx context.Context) ([]console.RouteRow, error)
	AddRoute(ctx context.Context, provider, destination, url, transport string) error
	SaveRoute(ctx context.Context, routeID uuid.UUID, url string, enabled bool) error
	DeleteRoute(ctx context.Context, routeID uuid.UUID) error
	ReplayDestination(ctx context.Context, routeID uuid.UUID) (int64, error)
	Verifications(ctx context.Context) ([]console.Verification, error)
	SetProvider(ctx context.Context, settings postgres.ProviderSettings) error
	DeleteProvider(ctx context.Context, name string) error
	Recheck(ctx context.Context, name string, batch int32) (int, error)
	DeliveryStates(ctx context.Context) (map[string]int64, error)
	EventsAwaitingRoute(ctx context.Context) (int64, error)
	PanelChanges(ctx context.Context) <-chan struct{}
}

type Summary struct {
	Delivered     int64
	Pending       int64
	Dead          int64
	AwaitingRoute int64
}

type Config struct {
	Transports   *delivery.Transports
	Auth         *auth.Registry
	Policy       auth.Policy
	SessionTTL   time.Duration
	SecureCookie bool
	Logger       *slog.Logger
}

type Handler struct {
	store      Store
	cfg        Config
	auth       *auth.Registry
	transports *delivery.Transports
	logger     *slog.Logger
}

func New(store Store, cfg Config) *Handler {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Transports == nil {
		cfg.Transports = delivery.DefaultTransports()
	}
	if cfg.Auth == nil {
		cfg.Auth = auth.NewRegistry()
	}
	return &Handler{
		store:      store,
		cfg:        cfg,
		auth:       cfg.Auth,
		transports: cfg.Transports,
		logger:     cfg.Logger,
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /login", h.loginForm)
	mux.HandleFunc("POST /logout", h.logout)

	// One shape of route for every method, so registering one is registration
	// and nothing else.
	mux.HandleFunc("POST /auth/{method}", h.signIn)
	mux.HandleFunc("GET /auth/{method}/start", h.startSignIn)
	mux.HandleFunc("GET /auth/{method}/callback", h.signIn)

	// Every panel route declares what it needs, so adding one without saying
	// who may reach it does not compile.
	mux.Handle("GET /{$}", h.allowed(authz.EventsRead, h.index))
	mux.Handle("GET /events", h.allowed(authz.EventsRead, h.events))
	mux.Handle("GET /events/{id}", h.allowed(authz.EventsRead, h.event))
	mux.Handle("GET /events/{id}/fragment", h.allowed(authz.EventsRead, h.eventFragment))
	mux.Handle("POST /events/{id}/replay", h.allowed(authz.EventsReplay, h.replayEvent))
	mux.Handle("POST /events/{eventID}/deliveries/{id}/replay",
		h.allowed(authz.EventsReplay, h.replayDelivery))
	mux.Handle("GET /settings", h.allowed(authz.EventsRead, h.settings))
	mux.Handle("POST /acting-tenant", h.allowed(authz.EventsRead, h.switchTenant))
	mux.Handle("POST /operators/system", h.allowed(authz.OperatorsWrite, h.makeSystemAdmin))
	mux.Handle("POST /operators/{id}/system", h.allowed(authz.OperatorsWrite, h.setSystemAdmin))
	mux.Handle("GET /routes", h.allowed(authz.RoutesRead, h.routes))
	mux.Handle("POST /routes", h.allowed(authz.RoutesWrite, h.createRoute))
	mux.Handle("POST /routes/{id}/save", h.allowed(authz.RoutesWrite, h.saveRoute))
	mux.Handle("POST /routes/{id}/delete", h.allowed(authz.RoutesWrite, h.deleteRoute))
	mux.Handle("POST /routes/{id}/resend", h.allowed(authz.EventsReplay, h.resendRoute))
	mux.Handle("GET /verification", h.allowed(authz.VerificationRead, h.verification))
	mux.Handle("POST /verification", h.allowed(authz.VerificationWrite, h.setVerification))
	mux.Handle("POST /verification/{name}/remove",
		h.allowed(authz.VerificationWrite, h.removeVerification))
	mux.Handle("POST /verification/{name}/recheck",
		h.allowed(authz.VerificationWrite, h.recheckVerification))
	mux.Handle("GET /operators", h.allowed(authz.OperatorsRead, h.operators))
	mux.Handle("POST /operators", h.allowed(authz.OperatorsWrite, h.createOperator))
	mux.Handle("POST /operators/{id}/role", h.allowed(authz.OperatorsWrite, h.setOperatorRole))
	mux.Handle("POST /operators/{id}/delete", h.allowed(authz.OperatorsWrite, h.deleteOperator))
	mux.Handle("GET /roles", h.allowed(authz.OperatorsRead, h.roles))
	mux.Handle("POST /roles", h.allowed(authz.RolesWrite, h.saveRole))
	mux.Handle("POST /roles/{name}/delete", h.allowed(authz.RolesWrite, h.deleteRole))
	mux.Handle("POST /values", h.allowed(authz.RolesWrite, h.pointValue))
	mux.Handle("POST /values/{method}/{value}/delete",
		h.allowed(authz.RolesWrite, h.stopPointingValue))
	mux.Handle("GET /tenants", h.allowed(authz.TenantsWrite, h.tenants))
	mux.Handle("POST /tenants", h.allowed(authz.TenantsWrite, h.createTenant))
	mux.Handle("POST /tenants/{slug}/delete", h.allowed(authz.TenantsWrite, h.deleteTenant))
	mux.Handle("GET /stream", h.allowed(authz.EventsRead, h.stream))
}

type (
	userKey  struct{}
	grantKey struct{}
	pathKey  struct{}
)

// allowed resolves the session, confines everything the request does to that
// operator's tenant, and refuses when their role does not grant what the route
// needs.
func (h *Handler) allowed(
	needs authz.Permission, next func(http.ResponseWriter, *http.Request),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		digest, err := console.SessionDigest(cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, err := h.store.SessionUser(r.Context(), digest)
		if err != nil {
			h.clearCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		acting, ok := h.acting(r, user)
		if !ok {
			h.logger.WarnContext(r.Context(), "the operator belongs to no tenant",
				"email", user.Email)
			http.Error(w, "you do not belong to any tenant yet", http.StatusForbidden)
			return
		}

		ctx := authz.WithTenant(r.Context(), acting.Current)

		role, err := h.roleFor(ctx, user, acting.Current)
		if err != nil {
			h.logger.ErrorContext(ctx, "could not read the role of an operator",
				"email", user.Email, "error", err)
			http.Error(w, "could not decide this request", http.StatusInternalServerError)
			return
		}

		if err := role.Authorize(needs); err != nil {
			h.logger.WarnContext(ctx, "an operator was refused",
				"email", user.Email, "role", role.Name, "needs", needs)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		ctx = context.WithValue(ctx, userKey{}, user)
		ctx = context.WithValue(ctx, grantKey{}, role)
		ctx = context.WithValue(ctx, pathKey{}, r.URL.Path)
		ctx = context.WithValue(ctx, actingKey{}, acting)
		next(w, r.WithContext(ctx))
	})
}

func currentUser(r *http.Request) console.User {
	user, _ := r.Context().Value(userKey{}).(console.User)
	return user
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, loginPage(h.auth.Methods(), r.URL.Query().Get("refused") != ""))
}

// One path for every way of signing in: a method says who this is, one policy
// says whether they are let in, and one place opens the session.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	method, found := h.auth.Method(r.PathValue("method"))
	if !found {
		http.NotFound(w, r)
		return
	}

	identity, err := method.Identify(r.Context(), r)
	h.finishSignIn(w, r, method, identity, err)
}

func (h *Handler) startSignIn(w http.ResponseWriter, r *http.Request) {
	method, found := h.auth.Method(r.PathValue("method"))
	if !found {
		http.NotFound(w, r)
		return
	}

	redirector, sendsAway := method.(auth.Redirector)
	if !sendsAway {
		http.NotFound(w, r)
		return
	}

	if err := redirector.Start(w, r); err != nil {
		h.logger.ErrorContext(r.Context(), "could not begin signing in",
			"method", method.Name(), "error", err)
		h.refuse(w, r)
	}
}

func (h *Handler) finishSignIn(
	w http.ResponseWriter, r *http.Request, method auth.Method, identity auth.Identity, err error,
) {
	if closer, has := method.(interface{ Done(http.ResponseWriter) }); has {
		closer.Done(w)
	}

	if err == nil {
		// What the provider actually said, which is the first thing anyone
		// wants when somebody lands somewhere unexpected.
		h.logger.DebugContext(r.Context(), "an identity arrived",
			"method", method.Name(), "email", identity.Email,
			"groups", identity.Claims, "address_unverified", identity.EmailUnverified)

		err = h.cfg.Policy.Admits(identity)
	}
	if err != nil {
		h.logger.WarnContext(r.Context(), "refused a sign-in",
			"method", method.Name(), "email", identity.Email, "reason", err)
		h.refuse(w, r)
		return
	}

	operator, err := h.operatorFor(r.Context(), method, identity)
	if err != nil {
		h.logger.WarnContext(r.Context(), "no operator for the identity",
			"method", method.Name(), "email", identity.Email, "reason", err)
		h.refuse(w, r)
		return
	}

	if err := h.grantSession(w, r, operator.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.logger.InfoContext(r.Context(), "signed in",
		"method", method.Name(), "email", operator.Email)
	http.Redirect(w, r, "/events", http.StatusSeeOther)
}

// An identity with a subject is linked to an account; one without, such as a
// password, only ever matches an account that already exists.
func (h *Handler) operatorFor(
	ctx context.Context, method auth.Method, identity auth.Identity,
) (console.User, error) {
	if identity.Subject == "" {
		return h.store.UserByEmail(ctx, identity.Email)
	}

	// Where somebody belongs is what the values they carry name: a value that
	// is the name of a tenant puts them in it. Nothing is registered par by
	// par, so nothing can be forgotten.
	named, err := h.store.Placement(ctx, method.Name(), identity.Claims)
	if err != nil {
		return console.User{}, err
	}

	if len(named) == 0 {
		h.logger.InfoContext(ctx, "nothing this identity carries names a tenant",
			"method", method.Name(), "email", identity.Email,
			"carried", carried(identity.Claims))
	}

	// An account is only made where something says it belongs. Carrying
	// nothing that names a tenant is not the same as belonging nowhere: a
	// provider that sends no values, or sends them somewhere this deployment
	// is not looking, looks exactly like a person with none. An account that
	// already exists is a different matter — it is linked, not created.
	// Only where, not as what: the role is applied just below, where a name
	// the provider uses and nothing here knows leaves the least instead of
	// turning somebody away.
	var first console.Placement
	if len(named) > 0 {
		first = console.Placement{Tenant: named[0].Tenant}
	}

	operator, err := h.store.UserBySubject(
		ctx, identity.Subject, identity.Email, first,
		len(named) > 0 && h.cfg.Policy.AutoProvision)
	if err != nil {
		return console.User{}, err
	}

	// The token is the truth about where somebody belongs, so it is applied
	// whole at every sign-in: the tenants it names, and no others. A system
	// account belongs nowhere by design and is left alone.
	if !operator.SystemAdmin {
		if err := h.store.Settle(ctx, operator.ID, named); err != nil {
			return console.User{}, err
		}
		if len(named) == 0 {
			return console.User{}, fmt.Errorf(
				"%w: nothing this identity carries names a tenant here (%s)",
				auth.ErrRefused, carried(identity.Claims))
		}
	}
	return operator, nil
}

// carried says what arrived, so a refusal names what the provider sent rather
// than only what it did not.
func carried(values []string) string {
	if len(values) == 0 {
		return "nothing arrived"
	}
	return strings.Join(values, ", ")
}

func (h *Handler) refuse(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?refused=1", http.StatusSeeOther)
}

func (h *Handler) grantSession(w http.ResponseWriter, r *http.Request, userID uuid.UUID) error {
	cookie, digest, err := console.NewSessionToken()
	if err != nil {
		return err
	}

	expires := time.Now().Add(h.cfg.SessionTTL)
	if err := h.store.CreateSession(r.Context(), digest, userID, expires); err != nil {
		return err
	}

	//nolint:gosec // Secure is configurable: the panel also runs over plain HTTP locally
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookie,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   h.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if digest, digestErr := console.SessionDigest(cookie.Value); digestErr == nil {
			if err := h.store.DeleteSession(r.Context(), digest); err != nil {
				h.logger.WarnContext(r.Context(), "could not delete the session", "error", err)
			}
		}
	}
	h.clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	//nolint:gosec // same as above
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/events", http.StatusSeeOther)
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	filter := parseFilter(r.Form)

	events, err := h.store.SearchEvents(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	providers, err := h.store.Providers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.page(w, r, "Events", eventsPage(events, providers, filter))
}

func (h *Handler) event(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.renderEvent(w, r, id, false)
}

// The same detail, without the page around it, for the modal the event list
// opens.
func (h *Handler) eventFragment(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.renderEvent(w, r, id, true)
}

func (h *Handler) renderEvent(w http.ResponseWriter, r *http.Request, id uuid.UUID, fragment bool) {
	detail, err := h.store.EventDetail(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deliveries, err := h.store.EventDeliveries(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if fragment {
		h.render(w, r, eventDetail(detail, deliveries, true))
		return
	}
	h.page(w, r, detail.Provider, eventPage(detail, deliveries))
}

func (h *Handler) replayEvent(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	affected, err := h.store.ReplayEvent(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "replayed an event",
		"event_id", id, "deliveries", affected, "by", currentUser(r).Email)

	h.respondAfterReplay(w, r, id)
}

// A replay is triggered from three places, and each one swaps a different part
// of the page. Answering with the wrong shape leaves htmx nothing to select,
// which blanks whatever it was told to replace. The event is passed in because
// the delivery route's own path value is the delivery, not the event.
func (h *Handler) respondAfterReplay(w http.ResponseWriter, r *http.Request, event uuid.UUID) {
	switch r.URL.Query().Get("view") {
	case "list":
		h.events(w, r)
	case "fragment":
		h.renderEvent(w, r, event, true)
	default:
		h.renderEvent(w, r, event, false)
	}
}

func (h *Handler) replayDelivery(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	event, err := uuid.Parse(r.PathValue("eventID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if _, err := h.store.ReplayDelivery(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "replayed a delivery",
		"delivery_id", id, "event_id", event, "by", currentUser(r).Email)

	h.respondAfterReplay(w, r, event)
}

func (h *Handler) routes(w http.ResponseWriter, r *http.Request) {
	routes, err := h.store.DetailedRoutes(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	unrouted, err := h.store.UnroutedProviders(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, "Routes", routesPage(routes, unrouted, h.transports.Kinds()))
}

func (h *Handler) createRoute(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	provider := strings.TrimSpace(r.PostForm.Get("provider"))
	target, err := destinationURL(r.PostForm.Get("url"))
	if err != nil || provider == "" {
		http.Error(w, "a provider and an http destination url are required", http.StatusBadRequest)
		return
	}

	destination := strings.TrimSpace(r.PostForm.Get("destination"))
	if destination == "" {
		destination = provider
	}

	transport := strings.TrimSpace(r.PostForm.Get("transport"))
	if transport == "" {
		transport = outbound.HTTP
	}
	if !h.transports.Knows(transport) {
		http.Error(w, "no transport is registered for "+transport, http.StatusBadRequest)
		return
	}

	if err := h.store.AddRoute(r.Context(), provider, destination, target, transport); err != nil {
		if errors.Is(err, postgres.ErrAlreadyRouted) {
			http.Error(w, provider+" already delivers to that url", http.StatusConflict)
			return
		}
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "created a route",
		"provider", provider, "destination", destination, "transport", transport,
		"url", target, "by", currentUser(r).Email)

	h.routes(w, r)
}

func (h *Handler) saveRoute(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	target, err := destinationURL(r.PostForm.Get("url"))
	if err != nil {
		http.Error(w, "an http destination url is required", http.StatusBadRequest)
		return
	}
	enabled := r.PostForm.Get("enabled") != ""

	if err := h.store.SaveRoute(r.Context(), id, target, enabled); err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "changed a route",
		"route_id", id, "url", target, "enabled", enabled, "by", currentUser(r).Email)

	h.routes(w, r)
}

func (h *Handler) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := h.store.DeleteRoute(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "removed a route",
		"route_id", id, "by", currentUser(r).Email)

	h.routes(w, r)
}

func (h *Handler) resendRoute(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	affected, err := h.store.ReplayDestination(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "resent everything for a route",
		"route_id", id, "deliveries", affected, "by", currentUser(r).Email)

	h.routes(w, r)
}

func (h *Handler) verification(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.Verifications(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	providers, err := h.store.Providers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, "Verification", verificationPage(items, providers))
}

func (h *Handler) setVerification(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}

	name := strings.TrimSpace(r.PostForm.Get("provider"))
	secretEnv := strings.TrimSpace(r.PostForm.Get("secret-env"))
	presetName := strings.TrimSpace(r.PostForm.Get("preset"))

	preset, found := provider.PresetByName(presetName)
	if name == "" || secretEnv == "" || !found {
		http.Error(w, "a provider, a preset and the name of a secret variable are required",
			http.StatusBadRequest)
		return
	}

	settings := preset.Settings
	if header := strings.TrimSpace(r.PostForm.Get("header")); header != "" {
		settings.Header = header
	}

	if err := h.store.SetProvider(r.Context(), postgres.ProviderSettings{
		Name:      name,
		SecretEnv: secretEnv,
		Settings:  settings,
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "configured verification",
		"provider", name, "preset", presetName, "verifier", settings.Verifier,
		"secret_env", secretEnv, "by", currentUser(r).Email)

	h.verification(w, r)
}

func (h *Handler) removeVerification(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	if err := h.store.DeleteProvider(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "stopped verifying a provider",
		"provider", name, "by", currentUser(r).Email)

	h.verification(w, r)
}

func (h *Handler) recheckVerification(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	recovered, err := h.store.Recheck(r.Context(), name, recheckBatch)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "rechecked refused requests",
		"provider", name, "recovered", recovered, "by", currentUser(r).Email)

	h.verification(w, r)
}

const recheckBatch = 500

func destinationURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing the destination url: %w", err)
	}
	if parsed.Host == "" {
		return "", errors.New("the destination url has no host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("the destination url must be http or https")
	}
	return trimmed, nil
}

// Pushes a line whenever anything an operator is looking at has changed, so an
// open page never has to ask on a timer. Bursts are coalesced: a hundred
// deliveries finishing at once become one line.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	control := http.NewResponseController(w)
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		h.logger.WarnContext(r.Context(), "could not lift the write deadline", "error", err)
	}

	changes := h.store.PanelChanges(r.Context())
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	send := func(line string) bool {
		if _, err := fmt.Fprint(w, line); err != nil {
			return false
		}
		return control.Flush() == nil
	}

	if !send(": connected\n\n") {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case <-heartbeat.C:
			if !send(": keep-alive\n\n") {
				return
			}

		case _, open := <-changes:
			if !open {
				return
			}
			if !h.settle(r.Context(), changes) {
				return
			}
			if !send("data: changed\n\n") {
				return
			}
		}
	}
}

// Waits out a burst of changes so that a page refreshes once instead of once
// per delivery. Reports whether the stream should carry on.
func (h *Handler) settle(ctx context.Context, changes <-chan struct{}) bool {
	timer := time.NewTimer(coalesceWindow)
	defer timer.Stop()

	for {
		select {
		case _, more := <-changes:
			if !more {
				return false
			}
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

const coalesceWindow = 400 * time.Millisecond

func (h *Handler) page(w http.ResponseWriter, r *http.Request, title string, body templ.Component) {
	summary := h.summary(r.Context())
	ctx := templ.WithChildren(r.Context(), body)
	h.renderWith(ctx, w, r, layout(title, currentUser(r).Email, summary))
}

func (h *Handler) summary(ctx context.Context) Summary {
	var summary Summary

	states, err := h.store.DeliveryStates(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "could not read delivery states", "error", err)
		return summary
	}
	summary.Delivered = states["delivered"]
	summary.Pending = states["pending"]
	summary.Dead = states["dead"]

	awaiting, err := h.store.EventsAwaitingRoute(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "could not count events awaiting a route", "error", err)
		return summary
	}
	summary.AwaitingRoute = awaiting

	return summary
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, component templ.Component) {
	h.renderWith(r.Context(), w, r, component)
}

func (h *Handler) renderWith(
	ctx context.Context, w http.ResponseWriter, r *http.Request, component templ.Component,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(ctx, w); err != nil {
		h.logger.ErrorContext(r.Context(), "could not render a page",
			"path", r.URL.Path, "error", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "panel request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "something went wrong", http.StatusInternalServerError)
}

func parseFilter(query url.Values) console.Filter {
	filter := console.Filter{
		Provider:  strings.TrimSpace(query.Get("provider")),
		State:     strings.TrimSpace(query.Get("state")),
		Search:    strings.TrimSpace(query.Get("q")),
		Signature: strings.TrimSpace(query.Get("signature")),
		Since:     parseTime(query.Get("since")),
		Until:     parseTime(query.Get("until")),
		PageSize:  50,
	}
	if page, err := strconv.Atoi(query.Get("page")); err == nil && page > 0 {
		filter.Page = page
	} else {
		filter.Page = 1
	}
	if !validState(filter.State) {
		filter.State = ""
	}
	if !validSignature(filter.Signature) {
		filter.Signature = ""
	}
	return filter
}

func validState(state string) bool {
	switch state {
	case "", "pending", "delivered", "dead", "unrouted":
		return true
	default:
		return false
	}
}

func validSignature(state string) bool {
	switch state {
	case "", "unchecked", "valid", "invalid", "missing":
		return true
	default:
		return false
	}
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	at, err := time.ParseInLocation("2006-01-02T15:04", value, time.Local)
	if err != nil {
		return time.Time{}
	}
	return at
}

func pageLink(filter console.Filter, page int) string {
	query := url.Values{}
	if filter.Provider != "" {
		query.Set("provider", filter.Provider)
	}
	if filter.State != "" {
		query.Set("state", filter.State)
	}
	if filter.Signature != "" {
		query.Set("signature", filter.Signature)
	}
	if filter.Search != "" {
		query.Set("q", filter.Search)
	}
	if !filter.Since.IsZero() {
		query.Set("since", localTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		query.Set("until", localTime(filter.Until))
	}
	query.Set("page", strconv.Itoa(page))
	return "/events?" + query.Encode()
}

func formatHeaders(headers map[string][]string) string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	for _, name := range names {
		for _, value := range headers[name] {
			fmt.Fprintf(&out, "%s: %s\n", name, value)
		}
	}
	return out.String()
}

// may is what the templates ask before drawing a control, so an operator is
// never shown an action their role would refuse.
func may(ctx context.Context, needs authz.Permission) bool {
	role, _ := ctx.Value(grantKey{}).(authz.Role)
	return role.Allows(needs)
}

// inTenant is the tenant this request is acting for, which is what anything
// written by it belongs to.
func inTenant(r *http.Request) uuid.UUID {
	tenant, _ := authz.TenantFrom(r.Context())
	return tenant
}

// acting is which tenant the request runs in and which ones it could run in.
// Somebody belongs to the tenants they are a member of; a system administrator
// belongs to none and reaches all of them. Either way it is one at a time, and
// the cookie only chooses among what is already theirs.
func (h *Handler) acting(r *http.Request, user console.User) (Acting, bool) {
	reachable := h.reachable(r, user)
	if len(reachable) == 0 {
		return Acting{}, false
	}

	// Listed by name for the eye, but the one to start in is where they have
	// been longest, not whichever name sorts first.
	current := slices.MinFunc(reachable, func(a, b console.Membership) int {
		return a.Since.Compare(b.Since)
	})
	if cookie, err := r.Cookie(tenantCookie); err == nil {
		for _, tenant := range reachable {
			if tenant.Slug == cookie.Value {
				current = tenant
			}
		}
	}

	return Acting{
		System:  user.SystemAdmin,
		Current: current.Tenant,
		Tenants: reachable,
	}, true
}

func (h *Handler) reachable(r *http.Request, user console.User) []console.Membership {
	if user.SystemAdmin {
		tenants, err := h.store.Tenants(r.Context())
		if err != nil {
			h.logger.WarnContext(r.Context(), "could not read the tenants", "error", err)
			return nil
		}

		all := make([]console.Membership, 0, len(tenants))
		for _, tenant := range tenants {
			all = append(all, console.Membership{
				Tenant: tenant.ID, Slug: tenant.Slug, Name: tenant.Name,
			})
		}
		return all
	}

	held, err := h.store.Memberships(r.Context(), user.ID)
	if err != nil {
		h.logger.WarnContext(r.Context(), "could not read what the operator belongs to",
			"email", user.Email, "error", err)
		return nil
	}
	return held
}

// roleFor is what somebody may do inside the tenant they are acting in. A
// system administrator may do everything the code checks; a member holds the
// role their membership states, and holding none is holding the least.
func (h *Handler) roleFor(
	ctx context.Context, user console.User, tenant uuid.UUID,
) (authz.Role, error) {
	if user.SystemAdmin {
		return authz.Everything(), nil
	}

	name, member, err := h.store.RoleIn(ctx, user.ID, tenant)
	if err != nil {
		return authz.Role{}, err
	}
	if !member {
		return authz.Role{}, nil
	}
	if name == "" {
		name = authz.Least
	}
	return h.store.Role(ctx, name)
}

// inSettings lights the settings entry from any page under it, because they
// are one place as far as the menu is concerned.
func inSettings(ctx context.Context) string {
	at, _ := ctx.Value(pathKey{}).(string)
	if at == "/settings" {
		return "at"
	}
	for _, one := range areas() {
		if at == one.Path || strings.HasPrefix(at, one.Path+"/") {
			return "at"
		}
	}
	return "muted"
}

// here marks the page being looked at, so the menu says where you are.
func here(ctx context.Context, path string) string {
	at, _ := ctx.Value(pathKey{}).(string)
	if at == path || strings.HasPrefix(at, path+"/") {
		return "at"
	}
	return "muted"
}

// Acting is what the header shows about who is looking: their role, and, for
// somebody not confined to a tenant, which one they are inside right now.
type Acting struct {
	System  bool
	Current uuid.UUID
	Tenants []console.Membership
}

type actingKey struct{}

func acting(ctx context.Context) Acting {
	state, _ := ctx.Value(actingKey{}).(Acting)
	return state
}

// tenantSlug is the tenant being looked at, named the way the provider would
// have to name it for the convention to take.
func tenantSlug(ctx context.Context) string {
	state := acting(ctx)
	for _, one := range state.Tenants {
		if one.Tenant == state.Current {
			return one.Slug
		}
	}
	return "tenant"
}

func roleName(ctx context.Context) string {
	role, _ := ctx.Value(grantKey{}).(authz.Role)
	return role.Name
}
