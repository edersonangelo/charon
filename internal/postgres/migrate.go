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
			name       text        primary key,
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
