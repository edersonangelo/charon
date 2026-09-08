package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edersonangelo/charon/internal/auth"
	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/health"
	"github.com/edersonangelo/charon/internal/ingest"
	"github.com/edersonangelo/charon/internal/metrics"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
	"github.com/edersonangelo/charon/internal/web"
)

type serveConfig struct {
	addr                string
	databaseURL         string
	maxBodyBytes        int64
	logLevel            string
	autoMigrate         bool
	secureCookie        bool
	sessionTTL          time.Duration
	verificationRefresh time.Duration
	metricsAddr         string
	oidc                auth.OIDCSettings
	policy              auth.Policy
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseServeFlags(args, stderr)
	if err != nil {
		return err
	}

	logger := newLogger(stdout, cfg.logLevel)

	store, err := postgres.Open(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	if cfg.autoMigrate {
		applied, migrateErr := store.Migrate(ctx)
		if migrateErr != nil {
			return migrateErr
		}
		for _, name := range applied {
			logger.InfoContext(ctx, "applied migration", "name", name)
		}
	}

	methods := signIn(cfg, store, logger)
	transports := delivery.DefaultTransports()

	// What this build knows how to do, said to the database, so a row can be
	// pointed at by a key. A deployment that registers a transport or a way of
	// signing in gets the row by starting.
	if err := store.Register(ctx, postgres.Kinds{
		Transports: transports.Kinds(),
		Verifiers:  provider.Default().Kinds(),
		Methods:    methods.Names(),
	}); err != nil {
		return err
	}

	verification := provider.NewCache(store, provider.Default(), cfg.verificationRefresh, logger)
	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go verification.Watch(watching)
	go forgetTenantsOnChange(watching, store)

	mux := http.NewServeMux()
	ingest.New(store, ingest.Config{
		MaxBodyBytes: cfg.maxBodyBytes,
		Verification: verification,
		Logger:       logger,
	}).Register(mux)
	health.New(store).Register(mux)
	web.New(store, web.Config{
		Auth:         methods,
		Transports:   transports,
		Policy:       cfg.policy,
		SecureCookie: cfg.secureCookie,
		SessionTTL:   cfg.sessionTTL,
		Logger:       logger,
	}).Register(mux)

	if cfg.metricsAddr != "" {
		go serveMetrics(watching, cfg.metricsAddr, store, logger)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return serveUntilSignal(ctx, srv, logger)
}

// Isolation between tenants is enforced by row level security, and Postgres
// Metrics get a listener of their own rather than a route on the main one.
// What they report is every tenant's counts at once, and the main listener is
// the one the internet posts webhooks to: an operational endpoint does not
// belong on a public port, and putting it behind the panel's sign-in would
// make it unscrapable.
func serveMetrics(
	ctx context.Context, addr string, store *postgres.Store, logger *slog.Logger,
) {
	mux := http.NewServeMux()
	metrics.New(store, version, logger).Register(mux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	logger.InfoContext(ctx, "serving metrics", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.ErrorContext(ctx, "the metrics listener stopped", "error", err)
	}
}

// The ways of signing in this deployment offers. Registering one is the whole
// change: the panel routes and renders whatever is here.
func signIn(cfg serveConfig, store *postgres.Store, logger *slog.Logger) *auth.Registry {
	registry := auth.NewRegistry()
	registry.Register(auth.NewPassword(storedCredentials{store}))

	if cfg.oidc.Configured() {
		cfg.oidc.SecureCookie = cfg.secureCookie
		cfg.oidc.Logger = logger
		registry.Register(auth.NewOIDC(cfg.oidc))
	}
	return registry
}

// storedCredentials adapts the store to the port the password method declares.
type storedCredentials struct{ store *postgres.Store }

func (c storedCredentials) Verify(ctx context.Context, email, password string) (string, error) {
	return c.store.VerifyPassword(ctx, email, password)
}

// Drains on shutdown so a request already inside a transaction is not cut off.
func serveUntilSignal(ctx context.Context, srv *http.Server, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("serving: %w", err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		logger.Info("draining before shutdown")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return <-errs
}

func migrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	applied, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		_, err = fmt.Fprintln(stdout, "no pending migrations")
		return err
	}
	for _, name := range applied {
		if _, err := fmt.Fprintf(stdout, "applied %s\n", name); err != nil {
			return err
		}
	}
	return nil
}

var errIncompleteOIDC = errors.New(
	"single sign-on needs -oidc-issuer, -oidc-client-id and -oidc-redirect-url together")

var errMissingDatabaseURL = errors.New("a database url is required: pass -database-url or set CHARON_DATABASE_URL")

func parseServeFlags(args []string, stderr io.Writer) (serveConfig, error) {
	fs := flag.NewFlagSet("charon serve", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var cfg serveConfig
	fs.StringVar(&cfg.addr, "addr", envOr("CHARON_ADDR", ":8080"),
		"address to listen on (env CHARON_ADDR)")
	fs.StringVar(&cfg.databaseURL, "database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	fs.Int64Var(&cfg.maxBodyBytes, "max-body-bytes", ingest.DefaultMaxBodyBytes,
		"largest request body that will be recorded")
	fs.StringVar(&cfg.logLevel, "log-level", envOr("CHARON_LOG_LEVEL", "info"),
		"debug, info, warn or error (env CHARON_LOG_LEVEL)")
	fs.BoolVar(&cfg.autoMigrate, "migrate", true,
		"apply pending migrations before serving")
	fs.BoolVar(&cfg.secureCookie, "secure-cookie", false,
		"mark the panel session cookie Secure; turn on behind HTTPS")
	fs.DurationVar(&cfg.sessionTTL, "session-ttl", 12*time.Hour,
		"how long an operator stays signed in to the panel")
	fs.StringVar(&cfg.metricsAddr, "metrics-addr", envOr("CHARON_METRICS_ADDR", ""),
		"address to serve Prometheus metrics on; off when empty (env CHARON_METRICS_ADDR)")
	fs.DurationVar(&cfg.verificationRefresh, "verification-refresh", time.Minute,
		"longest the signature settings can be stale before being reloaded anyway")

	fs.StringVar(&cfg.oidc.Issuer, "oidc-issuer", envOr("CHARON_OIDC_ISSUER", ""),
		"OpenID Connect issuer url, for example https://keycloak/realms/main (env CHARON_OIDC_ISSUER)")
	fs.StringVar(&cfg.oidc.ClientID, "oidc-client-id", envOr("CHARON_OIDC_CLIENT_ID", ""),
		"OpenID Connect client id (env CHARON_OIDC_CLIENT_ID)")
	fs.StringVar(&cfg.oidc.ClientSecret, "oidc-client-secret", envOr("CHARON_OIDC_CLIENT_SECRET", ""),
		"OpenID Connect client secret (env CHARON_OIDC_CLIENT_SECRET)")
	fs.StringVar(&cfg.oidc.RedirectURL, "oidc-redirect-url", envOr("CHARON_OIDC_REDIRECT_URL", ""),
		"where the provider sends the operator back, ending in /auth/oidc/callback (env CHARON_OIDC_REDIRECT_URL)")
	fs.StringVar(&cfg.oidc.TenantClaim, "oidc-tenant-claim",
		envOr("CHARON_OIDC_TENANT_CLAIM", auth.DefaultTenantClaim),
		"claim whose values name the tenants an identity belongs to; "+
			"providers differ on what they call it (env CHARON_OIDC_TENANT_CLAIM)")
	scopes := fs.String("oidc-scopes", envOr("CHARON_OIDC_SCOPES", "openid,profile,email"),
		"comma separated scopes to request (env CHARON_OIDC_SCOPES)")
	fs.BoolVar(&cfg.policy.AutoProvision, "auto-provision",
		envOr("CHARON_AUTO_PROVISION", "") != "",
		"create an operator on first sign-on instead of requiring one to exist (env CHARON_AUTO_PROVISION)")
	fs.BoolVar(&cfg.policy.AcceptUnverifiedEmail, "accept-unverified-email",
		envOr("CHARON_ACCEPT_UNVERIFIED_EMAIL", "") != "",
		"admit identities whose provider does not verify addresses, "+
			"for a provider where they are not self-asserted "+
			"(env CHARON_ACCEPT_UNVERIFIED_EMAIL)")
	fs.StringVar(&cfg.policy.RequiredClaim, "required-claim", envOr("CHARON_REQUIRED_CLAIM", ""),
		"only accept identities carrying this value (env CHARON_REQUIRED_CLAIM)")

	if err := fs.Parse(args); err != nil {
		return serveConfig{}, fmt.Errorf("parsing flags: %w", err)
	}
	if cfg.databaseURL == "" {
		return serveConfig{}, errMissingDatabaseURL
	}

	for _, scope := range strings.Split(*scopes, ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			cfg.oidc.Scopes = append(cfg.oidc.Scopes, trimmed)
		}
	}

	if cfg.oidc.Issuer != "" && !cfg.oidc.Configured() {
		return serveConfig{}, errIncompleteOIDC
	}

	return cfg, nil
}

// A slug is resolved to a tenant once and held, which stops being true the
// moment somebody creates or removes a tenant somewhere else — the command
// line and the panel do not run in this process. Left held, a slug removed and
// made again points at a tenant that no longer exists, and every webhook sent
// to that address is refused until this process restarts.
func forgetTenantsOnChange(ctx context.Context, store *postgres.Store) {
	changes := store.TenantChanges(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-changes:
			if !open {
				return
			}
			store.ForgetTenants()
		}
	}
}

func newLogger(out io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lvl}))
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
