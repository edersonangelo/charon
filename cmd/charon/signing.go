package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/edersonangelo/charon/internal/postgres"
)

var errSignUsage = errors.New(
	"usage: charon sign add -destination <name> -secret <env:NAME|file:/path>\n" +
		"       charon sign remove -destination <name> -secret <env:NAME|file:/path>\n" +
		"       charon sign list\n" +
		"       charon sign generate")

func sign(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errSignUsage
	}

	switch args[0] {
	case "add":
		return signAdd(ctx, args[1:], stdout, stderr)
	case "remove":
		return signRemove(ctx, args[1:], stdout, stderr)
	case "list":
		return signList(ctx, args[1:], stdout, stderr)
	case "generate":
		return signGenerate(stdout)
	default:
		return errSignUsage
	}
}

func signAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon sign add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	destination := fs.String("destination", "", "the destination whose deliveries are signed")
	secret := fs.String("secret", "",
		"where the secret is kept: env:NAME or file:/path, never the secret itself")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *destination == "" || *secret == "" {
		return errSignUsage
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

	if err := store.AddSigningSecret(ctx, *destination, *secret); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s now signs with %s\n", *destination, *secret)
	return err
}

func signRemove(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon sign remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	destination := fs.String("destination", "", "the destination to stop signing with it")
	secret := fs.String("secret", "", "the reference to remove")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *destination == "" || *secret == "" {
		return errSignUsage
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

	if err := store.RemoveSigningSecret(ctx, *destination, *secret); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s no longer signs with %s\n", *destination, *secret)
	return err
}

func signList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	ctx, store, err := openScoped(ctx, "charon sign list", args, stderr)
	if err != nil {
		return err
	}
	defer store.Close()

	secrets, err := store.SigningSecrets(ctx)
	if err != nil {
		return err
	}
	if len(secrets) == 0 {
		_, err = fmt.Fprintln(stdout, "nothing is signed")
		return err
	}

	for _, item := range secrets {
		if _, err := fmt.Fprintf(stdout, "%-20s %-40s added %s\n",
			item.Destination, item.Reference,
			item.Added.Format("2006-01-02")); err != nil {
			return err
		}
	}
	return nil
}

// A secret of the right shape is the part of this an operator should not have
// to invent, and printing one costs nothing. It is never stored: what to do
// with it is printed beside it.
func signGenerate(stdout io.Writer) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating a secret: %w", err)
	}
	secret := "chsec_" + base64.RawURLEncoding.EncodeToString(raw)

	_, err := fmt.Fprint(stdout, strings.Join([]string{
		secret,
		"",
		"Put it where the process that delivers can read it, then say where:",
		"",
		"  export CHARON_SIGNING_BILLING='" + secret + "'",
		"  charon sign add -destination billing -secret env:CHARON_SIGNING_BILLING",
		"",
		"or, to rotate it later without restarting anything:",
		"",
		"  printf %s '" + secret + "' > /run/secrets/billing",
		"  charon sign add -destination billing -secret file:/run/secrets/billing",
		"",
	}, "\n"))
	return err
}
