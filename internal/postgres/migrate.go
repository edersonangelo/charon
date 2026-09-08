package postgres

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/edersonangelo/charon/migrations"
)

const migrationLockID int64 = 4919_0001

// Pending is what this build carries that the database has not applied. A
// process that only reads and writes — the one that delivers — has to know
// this before it starts, because running new code against an old schema fails
// on every round with an error about a missing column, which reads like a
// defect rather than like a rollout still in progress.
func (s *Store) Pending(ctx context.Context) ([]string, error) {
	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	// A database nothing has ever been applied to has no record of what was,
	// which is every migration pending rather than a failure to answer. Asked
	// outright, because the driver only reports a missing table when the rows
	// are read and that is indistinguishable from a real failure.
	var recorded *string
	if err := s.pool.QueryRow(ctx,
		`select to_regclass('schema_migration')::text`).Scan(&recorded); err != nil {
		return nil, fmt.Errorf("looking for the record of applied migrations: %w", err)
	}
	if recorded == nil {
		return names, nil
	}

	rows, err := s.pool.Query(ctx, `select name from schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("collecting applied migrations: %w", err)
	}

	var waiting []string
	for _, name := range names {
		if !slices.Contains(done, name) {
			waiting = append(waiting, name)
		}
	}
	return waiting, nil
}

func (s *Store) Migrate(ctx context.Context) (applied []string, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring a connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockID); err != nil {
		return nil, fmt.Errorf("taking the migration lock: %w", err)
	}
	defer func() {
		_, unlockErr := conn.Exec(ctx, `select pg_advisory_unlock($1)`, migrationLockID)
		if unlockErr != nil && err == nil {
			err = fmt.Errorf("releasing the migration lock: %w", unlockErr)
		}
	}()

	if _, err := conn.Exec(ctx, `
		create table if not exists schema_migration (
			name       varchar(120) primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return nil, fmt.Errorf("creating schema_migration: %w", err)
	}

	rows, err := conn.Query(ctx, `select name from schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("collecting applied migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		if slices.Contains(done, name) {
			continue
		}

		body, readErr := migrations.FS.ReadFile(name)
		if readErr != nil {
			return applied, fmt.Errorf("reading migration %s: %w", name, readErr)
		}

		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return applied, fmt.Errorf("beginning migration %s: %w", name, beginErr)
		}

		if _, execErr := tx.Exec(ctx, string(body)); execErr != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("applying migration %s: %w", name, execErr)
		}

		if _, recErr := tx.Exec(ctx,
			`insert into schema_migration (name) values ($1)`, name); recErr != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("recording migration %s: %w", name, recErr)
		}

		if commitErr := tx.Commit(ctx); commitErr != nil {
			return applied, fmt.Errorf("committing migration %s: %w", name, commitErr)
		}

		applied = append(applied, name)
	}

	return applied, nil
}

func migrationNames() ([]string, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, path.Clean(entry.Name()))
	}
	slices.Sort(names)

	return names, nil
}
