package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/edersonangelo/charon/internal/postgres"
	"github.com/edersonangelo/charon/internal/provider"
)

var errVerifyUsage = errors.New(
	"usage: charon verify set -provider <name> -secret-env <VAR>\n" +
		"         -preset <" + strings.Join(provider.PresetNames(), "|") + ">\n" +
		"         [-verify-token-env <VAR>] for a provider that confirms the address first\n" +
		"         or the parameters directly:\n" +
		"         -verifier <" + strings.Join(provider.Default().Kinds(), "|") + ">" +
		" -scheme <" + strings.Join(provider.Schemes(), "|") + ">" +
		" -algorithm <" + strings.Join(provider.Algorithms(), "|") + ">" +
		" -encoding <" + strings.Join(provider.Encodings(), "|") + ">" +
		" -header <name> [-timestamp-key t] [-signature-key v1] [-tolerance 5m]\n" +
		"       charon verify presets\n" +
		"       charon verify list\n" +
		"       charon verify remove -provider <name>\n" +
		"       charon verify recheck -provider <name>")

func verify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errVerifyUsage
	}

	switch args[0] {
	case "set":
		return verifySet(ctx, args[1:], stdout, stderr)
	case "presets":
		return verifyPresets(stdout)
	case "list":
		return verifyList(ctx, args[1:], stdout, stderr)
	case "remove":
		return verifyRemove(ctx, args[1:], stdout, stderr)
	case "recheck":
		return verifyRecheck(ctx, args[1:], stdout, stderr)
	default:
		return errVerifyUsage
	}
}

func verifySet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon verify set", flag.ContinueOnError)
	fs.SetOutput(stderr)

	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	name := fs.String("provider", "", "the provider whose requests are checked")
	secretEnv := fs.String("secret-env", "",
		"name of the environment variable holding the shared secret")
	verifyTokenEnv := fs.String("verify-token-env", "",
		"name of the environment variable holding the token a provider offers "+
			"when it confirms this address")
	preset := fs.String("preset", "",
		"a named set of parameters: "+strings.Join(provider.PresetNames(), ", "))
	slug := tenantFlag(fs)

	verifier := fs.String("verifier", "", strings.Join(provider.Default().Kinds(), ", "))
	scheme := fs.String("scheme", "", strings.Join(provider.Schemes(), ", "))
	algorithm := fs.String("algorithm", "", strings.Join(provider.Algorithms(), ", "))
	encodingOf := fs.String("encoding", "", strings.Join(provider.Encodings(), ", "))
	header := fs.String("header", "", "header carrying the proof")
	timestampKey := fs.String("timestamp-key", "", "key of the timestamp inside an advanced header")
	signatureKey := fs.String("signature-key", "", "key of the digests inside an advanced header")
	tolerance := fs.Duration("tolerance", 0,
		"how far a signed timestamp may be from now, for schemes that send one")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *name == "" || *secretEnv == "" {
		return errVerifyUsage
	}

	// A preset supplies the parameters; anything given explicitly wins over it.
	var settings provider.Settings
	if *preset != "" {
		chosen, found := provider.PresetByName(*preset)
		if !found {
			return errVerifyUsage
		}
		settings = chosen.Settings
	}
	override(&settings.Verifier, *verifier)
	override(&settings.Scheme, *scheme)
	override(&settings.Algorithm, *algorithm)
	override(&settings.Encoding, *encodingOf)
	override(&settings.Header, *header)
	override(&settings.TimestampKey, *timestampKey)
	override(&settings.SignatureKey, *signatureKey)
	if *tolerance > 0 {
		settings.Tolerance = *tolerance
	}

	if !provider.Default().Knows(settings.Verifier) {
		return errVerifyUsage
	}

	// Refuse settings that cannot build, so a typo is caught here and not by
	// silently refusing every request later.
	probe := settings
	probe.Secret = "probe"
	if _, err := provider.Default().Verifier(probe); err != nil {
		return err
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

	if err := store.SetProvider(ctx, postgres.ProviderSettings{
		Name:           *name,
		SecretEnv:      *secretEnv,
		VerifyTokenEnv: *verifyTokenEnv,
		Settings:       settings,
	}); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(stdout, "%s is checked with %s, secret from %s\n",
		*name, settings.Verifier, *secretEnv); err != nil {
		return err
	}
	if *verifyTokenEnv == "" {
		return nil
	}

	_, err = fmt.Fprintf(stdout, "%s confirms its address with the token in %s\n",
		*name, *verifyTokenEnv)
	return err
}

func override(field *string, given string) {
	if given != "" {
		*field = given
	}
}

func verifyPresets(stdout io.Writer) error {
	for _, preset := range provider.Presets() {
		if _, err := fmt.Fprintf(stdout, "%-18s %s\n", preset.Name, preset.Description); err != nil {
			return err
		}
	}
	return nil
}

func verifyList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	ctx, store, err := openScoped(ctx, "charon verify list", args, stderr)
	if err != nil {
		return err
	}
	defer store.Close()

	settings, err := store.ProviderSettings(ctx)
	if err != nil {
		return err
	}
	if len(settings) == 0 {
		_, err = fmt.Fprintln(stdout, "no provider is verified")
		return err
	}

	for _, item := range settings {
		where := item.SecretEnv
		if !item.SecretPresent {
			where += " (not set in this environment)"
		}
		if item.VerifyTokenEnv != "" {
			where += ", verify token from " + item.VerifyTokenEnv
			if !item.VerifyTokenPresent {
				where += " (not set in this environment)"
			}
		}
		if _, err := fmt.Fprintf(stdout, "%-20s %-14s %-24s %s\n",
			item.Name, item.Verifier, item.Header, where); err != nil {
			return err
		}
	}
	return nil
}

func verifyRemove(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon verify remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""), "PostgreSQL connection string")
	name := fs.String("provider", "", "the provider to stop checking")
	slug := tenantFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *name == "" {
		return errVerifyUsage
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

	if err := store.DeleteProvider(ctx, *name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s is no longer checked\n", *name)
	return err
}

func verifyRecheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon verify recheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""), "PostgreSQL connection string")
	name := fs.String("provider", "", "the provider whose refused requests are checked again")
	batch := fs.Int("batch", 500, "how many requests to check")
	slug := tenantFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *name == "" {
		return errVerifyUsage
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

	recovered, err := store.Recheck(ctx, *name, int32(*batch)) //nolint:gosec // bounded by the flag
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%d request(s) now verify and were reopened for delivery\n", recovered)
	return err
}

func openFromFlags(
	ctx context.Context, name string, args []string, stderr io.Writer,
) (*postgres.Store, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return nil, errMissingDatabaseURL
	}
	return postgres.Open(ctx, *databaseURL)
}

// openScoped is the shape of every command that reads or writes what belongs
// to a tenant: connect, then confine everything to that tenant.
func openScoped(
	ctx context.Context, name string, args []string, stderr io.Writer,
) (context.Context, *postgres.Store, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	if err := fs.Parse(args); err != nil {
		return nil, nil, fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return nil, nil, errMissingDatabaseURL
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return nil, nil, err
	}

	scope, err := scoped(ctx, store, *slug)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return scope, store, nil
}
