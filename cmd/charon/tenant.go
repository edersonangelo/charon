package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/edersonangelo/charon/internal/authz"
	"github.com/edersonangelo/charon/internal/postgres"
)

var errTenantUsage = errors.New(
	"usage: charon tenant add -slug <slug> [-name <name>]\n" +
		"       charon tenant list\n" +
		"       charon tenant remove -slug <slug>")

// tenantFlag is on every command that touches recorded data, because the
// tenant decides what that command can even see.
func tenantFlag(fs *flag.FlagSet) *string {
	return fs.String("tenant", envOr("CHARON_TENANT", postgres.DefaultSlug),
		"slug of the tenant to act for (env CHARON_TENANT)")
}

// scoped confines everything done with the returned context to one tenant.
func scoped(ctx context.Context, store *postgres.Store, slug string) (context.Context, error) {
	tenant, err := store.TenantBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return authz.WithTenant(ctx, tenant.ID), nil
}

func tenant(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errTenantUsage
	}

	switch args[0] {
	case "add":
		return tenantAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return tenantList(ctx, args[1:], stdout, stderr)
	case "remove":
		return tenantRemove(ctx, args[1:], stdout, stderr)
	default:
		return errTenantUsage
	}
}

func tenantAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon tenant add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := fs.String("slug", "", "short name used in webhook addresses")
	name := fs.String("name", "", "display name, defaults to the slug")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if strings.TrimSpace(*slug) == "" {
		return errTenantUsage
	}
	if *name == "" {
		*name = *slug
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	created, err := store.CreateTenant(ctx, *slug, *name)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "created %s (%s), webhooks at /webhooks/%s/{provider}\n",
		created.Slug, created.ID, created.Slug)
	return err
}

func tenantList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	store, err := openFromFlags(ctx, "charon tenant list", args, stderr)
	if err != nil {
		return err
	}
	defer store.Close()

	tenants, err := store.Tenants(ctx)
	if err != nil {
		return err
	}
	for _, item := range tenants {
		if _, err := fmt.Fprintf(stdout, "%-24s %-36s %s\n",
			item.Slug, item.ID, item.Name); err != nil {
			return err
		}
	}
	return nil
}

func tenantRemove(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon tenant remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := fs.String("slug", "", "the tenant to remove, with everything recorded for it")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *slug == "" {
		return errTenantUsage
	}

	store, err := postgres.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.DeleteTenant(ctx, *slug); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "removed %s\n", *slug)
	return err
}

var errRoleUsage = errors.New(
	"usage: charon role list\n" +
		"       charon role set -name <name> [-description <text>] -grant <permission,...>\n" +
		"       charon role remove -name <name>\n" +
		"       charon role permissions")

func role(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errRoleUsage
	}

	switch args[0] {
	case "list":
		return roleList(ctx, args[1:], stdout, stderr)
	case "set":
		return roleSet(ctx, args[1:], stdout, stderr)
	case "remove":
		return roleRemove(ctx, args[1:], stdout, stderr)
	case "permissions":
		return rolePermissions(stdout)
	default:
		return errRoleUsage
	}
}

func rolePermissions(stdout io.Writer) error {
	for _, permission := range authz.All() {
		if _, err := fmt.Fprintf(stdout, "%-20s %s\n",
			permission, authz.Described()[permission]); err != nil {
			return err
		}
	}
	return nil
}

func roleList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon role list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)

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

	ctx, err = scoped(ctx, store, *slug)
	if err != nil {
		return err
	}

	roles, err := store.Roles(ctx)
	if err != nil {
		return err
	}
	for _, item := range roles {
		grants := make([]string, 0, len(item.Grants))
		for _, granted := range item.Grants {
			grants = append(grants, string(granted))
		}
		if _, err := fmt.Fprintf(stdout, "%-12s %s\n", item.Name, strings.Join(grants, " ")); err != nil {
			return err
		}
	}
	return nil
}

func roleSet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon role set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	name := fs.String("name", "", "the role to define")
	description := fs.String("description", "", "what the role is for")
	grant := fs.String("grant", "", "comma separated permissions, see charon role permissions")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *name == "" || *grant == "" {
		return errRoleUsage
	}

	granted := make([]authz.Permission, 0)
	for _, one := range strings.Split(*grant, ",") {
		if trimmed := strings.TrimSpace(one); trimmed != "" {
			granted = append(granted, authz.Permission(trimmed))
		}
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

	if err := store.SetRole(ctx, authz.Role{
		Name: *name, Description: *description, Grants: granted,
	}); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s grants %d permission(s) in %s\n", *name, len(granted), *slug)
	return err
}

func roleRemove(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon role remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	databaseURL := fs.String("database-url", envOr("CHARON_DATABASE_URL", ""),
		"PostgreSQL connection string (env CHARON_DATABASE_URL)")
	slug := tenantFlag(fs)
	name := fs.String("name", "", "the role to remove")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *databaseURL == "" {
		return errMissingDatabaseURL
	}
	if *name == "" {
		return errRoleUsage
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

	if err := store.DeleteRole(ctx, *name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "removed %s from %s\n", *name, *slug)
	return err
}
