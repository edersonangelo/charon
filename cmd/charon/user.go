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

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/console"
	"github.com/edersonangelo/charon/internal/postgres"
)

var errUserUsage = errors.New(
	"usage: charon user add -email <email> [-tenant <slug>] [-role <name>] [-superadmin]\n" +
		"       charon user superadmin -email <email> [-revoke]")

func user(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errUserUsage
	}
	if args[0] == "superadmin" {
		return userSuperadmin(ctx, args[1:], stdout, stderr)
	}
	if args[0] != "add" {
		return errUserUsage
	}

	fs := flag.NewFlagSet("charon user add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	email := fs.String("email", "", "the address that signs in")
	password := fs.String("password", envOr("CHARON_PASSWORD", ""),
		"password; prompted for when absent (env CHARON_PASSWORD)")
	slug := tenantFlag(fs)
	role := fs.String("role", authz.Least, "the role this operator has in that tenant")
	superadmin := fs.Bool("superadmin", false,
		"make a system administrator, which is not confined to a tenant")

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
	// How strong a password is belongs to whoever chooses it. The only thing
	// refused here is none at all, because an account with no way in cannot be
	// reached.
	if *password == "" {
		return errors.New("a password is required")
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

	tenant, err := store.TenantBySlug(ctx, *slug)
	if err != nil {
		return err
	}

	if err := store.CreateUser(ctx, strings.TrimSpace(*email), hash, console.Placement{
		Tenant: tenant.ID, Role: *role,
	}); err != nil {
		return err
	}

	if *superadmin {
		if err := store.SetSystemAdminByEmail(ctx, strings.TrimSpace(*email)); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "created %s as a system administrator\n", *email)
		return err
	}

	_, err = fmt.Fprintf(stdout, "created %s as %s of %s\n", *email, *role, *slug)
	return err
}

// The first system administrator is made here, because a deployment with none
// has nobody who could make one from the panel.
func userSuperadmin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon user superadmin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	email := fs.String("email", "", "the operator who becomes one")
	revoke := fs.Bool("revoke", false, "take the standing away instead of granting it")
	slug := tenantFlag(fs)
	role := fs.String("role", authz.Least, "when revoking, the tenant role to return to")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if strings.TrimSpace(*email) == "" {
		return errUserUsage
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	if *revoke {
		if err := store.ClearSystemAdminByEmail(ctx, strings.TrimSpace(*email)); err != nil {
			return err
		}

		// The standing was not a place, so giving it up leaves the account
		// wherever its memberships put it. A tenant is named here so it is not
		// left reaching nothing.
		tenant, err := store.TenantBySlug(ctx, *slug)
		if err != nil {
			return err
		}
		operator, err := store.UserByEmail(ctx, strings.TrimSpace(*email))
		if err != nil {
			return err
		}
		if err := store.Join(ctx, operator.ID,
			console.Placement{Tenant: tenant.ID, Role: *role}); err != nil {
			return err
		}
	} else if err := store.SetSystemAdminByEmail(ctx, strings.TrimSpace(*email)); err != nil {
		return err
	}

	if *revoke {
		_, err = fmt.Fprintf(stdout, "%s is no longer a system administrator, and is %s of %s\n",
			*email, *role, *slug)
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s is a system administrator and reaches every tenant\n", *email)
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
