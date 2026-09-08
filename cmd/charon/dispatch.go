package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edersonangelo/charon/internal/delivery"
	"github.com/edersonangelo/charon/internal/outbound"
	"github.com/edersonangelo/charon/internal/postgres"
)

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)

	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	logLevel := fs.String("log-level", envOr("CHARON_LOG_LEVEL", "info"),
		"debug, info, warn or error (env CHARON_LOG_LEVEL)")
	workers := fs.Int("workers", 8, "how many deliveries to attempt at once")
	batchSize := fs.Int("batch-size", 50, "how many deliveries to claim per round")
	safetyInterval := fs.Duration("safety-interval", 30*time.Second,
		"longest the dispatcher sleeps before scanning anyway")
	requestTimeout := fs.Duration("request-timeout", 15*time.Second, "how long a destination has to answer")
	maxAttempts := fs.Int("max-attempts", 12, "attempts before a delivery is dead lettered")
	backoffBase := fs.Duration("backoff-base", 5*time.Second, "first retry window")
	backoffCap := fs.Duration("backoff-cap", time.Hour, "largest retry window")
	maxResponse := fs.Int64("max-response-bytes", delivery.DefaultMaxResponseBytes,
		"how much of what a destination says back is kept on the attempt")
	purgeEvery := fs.Duration("purge-every", time.Hour,
		"how often to discard events past what their tenant keeps")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}

	logger := newLogger(stdout, *logLevel)

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	dispatcher := delivery.New(store, delivery.Config{
		Workers:          *workers,
		BatchSize:        *batchSize,
		SafetyInterval:   *safetyInterval,
		RequestTimeout:   *requestTimeout,
		MaxResponseBytes: *maxResponse,
		MaxAttempts:      *maxAttempts,
		BackoffBase:      *backoffBase,
		BackoffCap:       *backoffCap,
		Logger:           logger,
	})

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reportWhatCanBeSigned(ctx, store, logger)
	go purgeWhatIsNoLongerKept(ctx, store, *purgeEvery, logger)

	return dispatcher.Run(ctx)
}

// The panel cannot tell whether a signing secret is readable: it runs in
// another process, with another environment and another filesystem. So the
// process that actually signs says so, on the interval a secret can move
// underneath it — a file rewritten in place rotates without anything here
// noticing until it is read again.
const signingReport = time.Minute

func reportWhatCanBeSigned(ctx context.Context, store *postgres.Store, logger *slog.Logger) {
	report := func() {
		references, err := store.EveryReference(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.ErrorContext(ctx, "could not read the signing secrets", "error", err)
			}
			return
		}
		for _, reference := range references {
			if err := store.RecordReading(ctx, reference, delivery.CanRead(reference.Reference)); err != nil {
				if ctx.Err() == nil {
					logger.ErrorContext(ctx, "could not record a signing secret reading",
						"reference", reference.Reference, "error", err)
				}
			}
		}
	}

	report()

	changes := store.SigningChanges(ctx)
	ticker := time.NewTicker(signingReport)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-changes:
			if !open {
				return
			}
			report()
		case <-ticker.C:
			report()
		}
	}
}

// Discarding happens where delivering happens, because the two have to agree
// about what is still needed: nothing is discarded while a delivery for it is
// still pending, and this is the process that decides when one stops being.
//
// A tenant that has not asked to lose anything keeps everything, so on most
// deployments this finds nothing to do and says nothing about it.
func purgeWhatIsNoLongerKept(
	ctx context.Context, store *postgres.Store, every time.Duration, logger *slog.Logger,
) {
	if every <= 0 {
		return
	}

	discard := func() {
		discarded, err := store.Purge(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.ErrorContext(ctx, "could not discard what is no longer kept", "error", err)
		case discarded > 0:
			logger.InfoContext(ctx, "discarded events past their retention", "events", discarded)
		}
	}

	discard()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discard()
		}
	}
}

var errRouteUsage = errors.New("usage: charon route add -provider <name> -url <url> " +
	"[-destination <name>] [-tenant <slug>] | charon route list")

func route(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errRouteUsage
	}

	switch args[0] {
	case "add":
		return routeAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return routeList(ctx, args[1:], stdout, stderr)
	default:
		return errRouteUsage
	}
}

func routeAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon route add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	provider := fs.String("provider", "", "the provider whose events are routed")
	url := fs.String("url", "", "where the events are delivered")
	transport := fs.String("transport", outbound.HTTP, "kind of destination")
	destination := fs.String("destination", "", "name for the destination, defaults to the provider")
	slug := tenantFlag(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *provider == "" || *url == "" {
		return errRouteUsage
	}
	if *destination == "" {
		*destination = *provider
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, err = scoped(ctx, store, *slug)
	if err != nil {
		return err
	}

	known, err := store.KnownProviders(ctx)
	if err != nil {
		return err
	}

	if err := store.AddRoute(ctx, *provider, *destination, *url, *transport); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(stdout, "routed %s to %s (%s)\n",
		*provider, *destination, *url); err != nil {
		return err
	}
	return warnUnknownProvider(stderr, *provider, known)
}

// A provider name is never declared before it is used, so routing one nothing
// has arrived for is how every deployment starts and cannot be refused. Naming
// the ones already established is what makes a misspelling visible, instead of
// leaving a route that silently receives nothing.
func warnUnknownProvider(stderr io.Writer, provider string, known []string) error {
	if slices.Contains(known, provider) {
		return nil
	}
	if len(known) == 0 {
		_, err := fmt.Fprintf(stderr,
			"warning: nothing has arrived for %q and it has no verification configured\n",
			provider)
		return err
	}
	_, err := fmt.Fprintf(stderr,
		"warning: nothing has arrived for %q and it has no verification configured; "+
			"this tenant already knows %s\n",
		provider, strings.Join(known, ", "))
	return err
}

func routeList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	ctx, store, err := openScoped(ctx, "charon route list", args, stderr)
	if err != nil {
		return err
	}
	defer store.Close()

	routes, err := store.Routes(ctx)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		_, err = fmt.Fprintln(stdout, "no routes")
		return err
	}

	for _, r := range routes {
		state := "enabled"
		if !r.Enabled {
			state = "disabled"
		}
		signing := "unsigned"
		if r.Signed > 0 {
			signing = fmt.Sprintf("signed(%d)", r.Signed)
		}
		if _, err := fmt.Fprintf(stdout, "%-20s %-20s %-8s %-8s %-10s %s\n",
			r.Provider, r.Destination, r.Transport, state, signing, r.URL); err != nil {
			return err
		}
	}
	return nil
}
