package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/edersonangelo/charon/internal/postgres"
)

var errForceUsage = errors.New(
	"usage: charon events force -by <email> -reason <why> -id <uuid> [-id <uuid>...]")

// ids collects a flag given more than once, because one reason covers a batch.
type ids []uuid.UUID

func (i *ids) String() string { return fmt.Sprint(*i) }

func (i *ids) Set(value string) error {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%q is not an event identifier: %w", value, err)
	}
	*i = append(*i, parsed)
	return nil
}

func events(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "force" {
		return errForceUsage
	}

	fs := flag.NewFlagSet("charon events force", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	by := fs.String("by", "", "the operator taking this decision, by the address they sign in with")
	reason := fs.String("reason", "", "why these are being delivered although they did not verify")
	var chosen ids
	fs.Var(&chosen, "id", "an event to deliver anyway; give it once per event")

	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *by == "" || *reason == "" || len(chosen) == 0 {
		return errForceUsage
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

	operator, err := store.UserByEmail(ctx, strings.TrimSpace(*by))
	if err != nil {
		return fmt.Errorf("no operator signs in as %q", *by)
	}

	forced, err := store.Force(ctx, operator.ID, *reason, chosen)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout,
		"%d event(s) will be delivered although they did not verify, on %s's decision\n",
		forced, operator.Email)
	return err
}
