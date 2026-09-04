package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

var errUserUsage = errors.New("usage: charon user add -email <email>")

func user(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "add" {
		return errUserUsage
	}

	fs := flag.NewFlagSet("charon user add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	email := fs.String("email", "", "the address that signs in")
	password := fs.String("password", envOr("CHARON_PASSWORD", ""),
		"password; prompted for when absent (env CHARON_PASSWORD)")

	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if strings.TrimSpace(*email) == "" {
		return errUserUsage
	}

	if *password == "" {
		read, err := readPassword(stdout)
		if err != nil {
			return err
		}
		*password = read
	}
	if len(*password) < 8 {
		return errors.New("the password must be at least 8 characters")
	}

	hash, err := console.HashPassword(*password)
	if err != nil {
		return err
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	if err := store.CreateUser(ctx, strings.TrimSpace(*email), hash); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "created %s\n", *email)
	return err
}

func readPassword(stdout io.Writer) (string, error) {
	if _, err := fmt.Fprint(stdout, "password: "); err != nil {
		return "", err
	}

	fd := syscall.Stdin
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		if err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			return "", err
		}
		return string(raw), nil
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the password: %w", err)
	}
	return strings.TrimSpace(line), nil
}
