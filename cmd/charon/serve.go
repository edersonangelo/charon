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

	"github.com/edersonangelo/charon/internal/health"
	"github.com/edersonangelo/charon/internal/ingest"
	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/web"
)

type serveConfig struct {
	addr         string
	databaseURL  string
	maxBodyBytes int64
	logLevel     string
	autoMigrate  bool
	secureCookie bool
	sessionTTL   time.Duration
	oidc         web.OIDC
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

	mux := http.NewServeMux()
	ingest.New(store, ingest.Config{
		MaxBodyBytes: cfg.maxBodyBytes,
		Logger:       logger,
	}).Register(mux)
	health.New(store).Register(mux)
	web.New(store, web.Config{
		SecureCookie: cfg.secureCookie,
		SessionTTL:   cfg.sessionTTL,
		OIDC:         cfg.oidc,
		Logger:       logger,
	}).Register(mux)

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

	fs.StringVar(&cfg.oidc.Issuer, "oidc-issuer", envOr("CHARON_OIDC_ISSUER", ""),
		"OpenID Connect issuer url, for example https://keycloak/realms/main (env CHARON_OIDC_ISSUER)")
	fs.StringVar(&cfg.oidc.ClientID, "oidc-client-id", envOr("CHARON_OIDC_CLIENT_ID", ""),
		"OpenID Connect client id (env CHARON_OIDC_CLIENT_ID)")
	fs.StringVar(&cfg.oidc.ClientSecret, "oidc-client-secret", envOr("CHARON_OIDC_CLIENT_SECRET", ""),
		"OpenID Connect client secret (env CHARON_OIDC_CLIENT_SECRET)")
	fs.StringVar(&cfg.oidc.RedirectURL, "oidc-redirect-url", envOr("CHARON_OIDC_REDIRECT_URL", ""),
		"where the provider sends the operator back, ending in /auth/sso/callback (env CHARON_OIDC_REDIRECT_URL)")
	scopes := fs.String("oidc-scopes", envOr("CHARON_OIDC_SCOPES", "openid,profile,email"),
		"comma separated scopes to request (env CHARON_OIDC_SCOPES)")
	fs.BoolVar(&cfg.oidc.AutoProvision, "oidc-auto-provision",
		envOr("CHARON_OIDC_AUTO_PROVISION", "") != "",
		"create an operator on first single sign-on instead of requiring one to exist (env CHARON_OIDC_AUTO_PROVISION)")
	fs.StringVar(&cfg.oidc.RequiredGroup, "oidc-required-group", envOr("CHARON_OIDC_REQUIRED_GROUP", ""),
		"only accept accounts carrying this value in the groups claim (env CHARON_OIDC_REQUIRED_GROUP)")

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
