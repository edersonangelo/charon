package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/edersonangelo/charon/internal/postgres"
)

var errRetentionUsage = errors.New(
	"usage: charon retention set -tenant <slug> -days <n>\n" +
		"       charon retention set -tenant <slug> -forever\n" +
		"       charon retention list")

func retention(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errRetentionUsage
	}

	switch args[0] {
	case "set":
		return retentionSet(ctx, args[1:], stdout, stderr)
	case "list":
		return retentionList(ctx, args[1:], stdout, stderr)
	default:
		return errRetentionUsage
	}
}

func retentionSet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon retention set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	days := fs.Int("days", 0, "how many days of events to keep")
	forever := fs.Bool("forever", false, "keep events for ever, which is the default")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if (*days == 0) == !*forever {
		return errRetentionUsage
	}
	if *forever {
		*days = 0
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.SetRetention(ctx, *slug, *days); err != nil {
		return err
	}

	if *days == 0 {
		_, err = fmt.Fprintf(stdout, "%s keeps its events for ever\n", *slug)
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s keeps %d days of events\n", *slug, *days)
	return err
}

func retentionList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	store, err := openFromFlags(ctx, "charon retention list", args, stderr)
	if err != nil {
		return err
	}
	defer store.Close()

	spans, err := store.Retentions(ctx)
	if err != nil {
		return err
	}
	for _, span := range spans {
		kept := "for ever"
		if span.Days > 0 {
			kept = fmt.Sprintf("%d days", span.Days)
		}
		if _, err := fmt.Fprintf(stdout, "%-24s %s\n", span.Slug, kept); err != nil {
			return err
		}
	}
	return nil
}

func purge(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon purge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	dryRun := fs.Bool("dry-run", false, "say what would go without discarding it")

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

	if *dryRun {
		count, err := store.Purgeable(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s would be discarded\n", counted(count, "event"))
		return err
	}

	discarded, err := store.Purge(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "discarded %s\n", counted(discarded, "event"))
	return err
}

func counted(n int64, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
