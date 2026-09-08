package testsupport

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	containerMu  sync.Mutex
	containerDSN string
)

// Name of the variable that points every package at one PostgreSQL instance.
const testDatabaseURL = "CHARON_TEST_DATABASE_URL"

// One container per test binary when none is provided, kept for the life of the
// process and removed by the testcontainers reaper afterwards.
//
// A failed start is retried rather than remembered: caching the error would
// fail every remaining test in the package over one transient hiccup.
func sharedContainer(tb testing.TB) string {
	tb.Helper()

	containerMu.Lock()
	defer containerMu.Unlock()

	if containerDSN != "" {
		return containerDSN
	}

	// One server for the whole run when the caller provides it. Four packages
	// each starting a container saturates Docker and makes unrelated tests
	// fail; `make test` points every package at the same instance.
	if given := os.Getenv(testDatabaseURL); given != "" {
		containerDSN = given
		return containerDSN
	}

	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		dsn, err := startPostgres()
		if err == nil {
			containerDSN = dsn
			return containerDSN
		}
		last = err
		tb.Logf("starting postgres, attempt %d of 3: %v", attempt, err)
	}

	tb.Fatalf("could not start postgres: %v", last)
	return ""
}

func startPostgres() (string, error) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:18-alpine",
		postgres.WithDatabase("charon"),
		postgres.WithUsername("charon"),
		postgres.WithPassword("charon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return "", fmt.Errorf("starting postgres: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("building the connection string: %w", err)
	}
	return dsn, nil
}

// PostgresDSN returns a connection string to a database of this test's own, on
// a PostgreSQL instance shared with the rest of the package.
func PostgresDSN(tb testing.TB) string {
	tb.Helper()

	if testing.Short() {
		tb.Skip("skipping: needs a database and -short was given")
	}

	adminDSN := sharedContainer(tb)

	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		tb.Fatalf("connecting to create %s: %v", name, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		tb.Fatalf("creating database %s: %v", name, err)
	}

	// The database is owned by an ordinary role, because every deployment has
	// one: a managed Postgres does not hand out superuser. It matters here
	// because a superuser is not subject to row level security at all, so a
	// statement that reads nothing under a policy reads everything under a
	// test, and a whole class of mistake becomes invisible.
	role := "role_" + strings.TrimPrefix(name, "test_")
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		// createrole, because a test that proves row level security confines
		// somebody has to be able to make that somebody. It is not superuser
		// and it does not bypass row level security, which is the whole point.
		"create role %s login createrole password %s",
		pgx.Identifier{role}.Sanitize(), quoted(role))); err != nil {
		tb.Fatalf("creating role %s: %v", role, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("alter database %s owner to %s",
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{role}.Sanitize())); err != nil {
		tb.Fatalf("giving %s to %s: %v", name, role, err)
	}

	adminHere, err := withDatabase(adminDSN, name)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	inside, err := pgx.Connect(ctx, adminHere)
	if err != nil {
		tb.Fatalf("connecting to %s: %v", name, err)
	}
	defer func() { _ = inside.Close(ctx) }()

	// Owning the database is not owning what is in it, and since PostgreSQL 15
	// the public schema is not writable by anyone else.
	if _, err := inside.Exec(ctx, fmt.Sprintf("alter schema public owner to %s",
		pgx.Identifier{role}.Sanitize())); err != nil {
		tb.Fatalf("giving the schema of %s to %s: %v", name, role, err)
	}

	tb.Cleanup(func() {
		clean, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = clean.Close(context.Background()) }()
		_, _ = clean.Exec(context.Background(),
			"drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)")
		_, _ = clean.Exec(context.Background(),
			"drop role if exists "+pgx.Identifier{role}.Sanitize())
	})

	return asRole(adminHere, role)
}

// quoted is a string literal for a statement that cannot take a parameter,
// which create role is.
func quoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func asRole(dsn, role string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	parsed.User = url.UserPassword(role, role)
	return parsed.String()
}

func withDatabase(dsn, database string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing the connection string: %w", err)
	}
	parsed.Path = "/" + database
	return parsed.String(), nil
}
