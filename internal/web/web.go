package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

//go:embed static
var assets embed.FS

const sessionCookie = "charon_session"

type Store interface {
	UserByEmail(ctx context.Context, email string) (console.User, error)
	CreateSession(ctx context.Context, digest []byte, userID uuid.UUID, expires time.Time) error
	SessionUser(ctx context.Context, digest []byte) (console.User, error)
	DeleteSession(ctx context.Context, digest []byte) error
	UserBySubject(ctx context.Context, subject, email string, provision bool) (console.User, error)
	SearchEvents(ctx context.Context, filter console.Filter) ([]console.EventSummary, error)
	Providers(ctx context.Context) ([]string, error)
	EventDetail(ctx context.Context, id uuid.UUID) (console.EventDetail, error)
	EventDeliveries(ctx context.Context, id uuid.UUID) ([]console.Delivery, error)
	ReplayEvent(ctx context.Context, id uuid.UUID) (int64, error)
	ReplayDelivery(ctx context.Context, id uuid.UUID) (int64, error)
	UnroutedProviders(ctx context.Context) ([]console.UnroutedProvider, error)
	DetailedRoutes(ctx context.Context) ([]console.RouteRow, error)
	AddRoute(ctx context.Context, provider, destination, url string) error
	SaveRoute(ctx context.Context, routeID uuid.UUID, url string, enabled bool) error
	DeleteRoute(ctx context.Context, routeID uuid.UUID) error
	ReplayDestination(ctx context.Context, routeID uuid.UUID) (int64, error)
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
	SessionTTL   time.Duration
	SecureCookie bool
	OIDC         OIDC
	Logger       *slog.Logger
}

type Handler struct {
	store  Store
	cfg    Config
	oidc   *provider
	logger *slog.Logger
}

func New(store Store, cfg Config) *Handler {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{store: store, cfg: cfg, oidc: &provider{cfg: cfg.OIDC}, logger: cfg.Logger}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /login", h.loginForm)
	mux.HandleFunc("POST /login", h.login)
	mux.HandleFunc("POST /logout", h.logout)

	if h.cfg.OIDC.Configured() {
		mux.HandleFunc("GET /auth/sso", h.startSSO)
		mux.HandleFunc("GET /auth/sso/callback", h.callbackSSO)
	}

	mux.Handle("GET /{$}", h.authenticated(h.index))
	mux.Handle("GET /events", h.authenticated(h.events))
	mux.Handle("GET /events/{id}", h.authenticated(h.event))
	mux.Handle("GET /events/{id}/fragment", h.authenticated(h.eventFragment))
	mux.Handle("POST /events/{id}/replay", h.authenticated(h.replayEvent))
	mux.Handle("POST /events/{eventID}/deliveries/{id}/replay", h.authenticated(h.replayDelivery))
	mux.Handle("GET /routes", h.authenticated(h.routes))
	mux.Handle("POST /routes", h.authenticated(h.createRoute))
	mux.Handle("POST /routes/{id}/save", h.authenticated(h.saveRoute))
	mux.Handle("POST /routes/{id}/delete", h.authenticated(h.deleteRoute))
	mux.Handle("POST /routes/{id}/resend", h.authenticated(h.resendRoute))
	mux.Handle("GET /stream", h.authenticated(h.stream))
}

type userKey struct{}

func (h *Handler) authenticated(next func(http.ResponseWriter, *http.Request)) http.Handler {
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

		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

func currentUser(r *http.Request) console.User {
	user, _ := r.Context().Value(userKey{}).(console.User)
	return user
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	notice := ""
	switch r.URL.Query().Get("sso") {
	case "rejected":
		notice = "single sign-on refused that account"
	case "unavailable":
		notice = "the identity provider could not be reached"
	}
	h.render(w, r, loginPage(
		r.URL.Query().Get("failed") != "", h.cfg.OIDC.Configured(), notice))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?failed=1", http.StatusSeeOther)
		return
	}

	email := strings.TrimSpace(r.PostForm.Get("email"))
	user, err := h.store.UserByEmail(r.Context(), email)
	if err == nil {
		err = console.CheckPassword(user.PasswordHash, r.PostForm.Get("password"))
	}
	if err != nil {
		h.logger.WarnContext(r.Context(), "failed sign in", "email", email)
		http.Redirect(w, r, "/login?failed=1", http.StatusSeeOther)
		return
	}

	if err := h.grantSession(w, r, user.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/events", http.StatusSeeOther)
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

func randomString() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func encode(payload []byte) string { return base64.RawURLEncoding.EncodeToString(payload) }

func decode(value string) ([]byte, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	return payload, nil
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
	h.page(w, r, "Routes", routesPage(routes, unrouted))
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

	if err := h.store.AddRoute(r.Context(), provider, destination, target); err != nil {
		if errors.Is(err, postgres.ErrAlreadyRouted) {
			http.Error(w, provider+" already delivers to that url", http.StatusConflict)
			return
		}
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "created a route",
		"provider", provider, "destination", destination, "url", target, "by", currentUser(r).Email)

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
		Provider: strings.TrimSpace(query.Get("provider")),
		State:    strings.TrimSpace(query.Get("state")),
		Search:   strings.TrimSpace(query.Get("q")),
		Since:    parseTime(query.Get("since")),
		Until:    parseTime(query.Get("until")),
		PageSize: 50,
	}
	if page, err := strconv.Atoi(query.Get("page")); err == nil && page > 0 {
		filter.Page = page
	} else {
		filter.Page = 1
	}
	if !validState(filter.State) {
		filter.State = ""
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
