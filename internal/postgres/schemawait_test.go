package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The process that delivers does not migrate, so on a rollout it can come up
// against a schema the running build is ahead of. It has to be able to ask,
// rather than find out by failing on a column that does not exist yet.
func TestADatabaseThatIsBehindSaysWhatItIsMissing(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	pending, err := store.Pending(ctx)
	if err != nil {
		t.Fatalf("asking what is pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a migrated database says %d are pending", len(pending))
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// A database one version behind the build.
	if _, err := conn.Exec(ctx,
		`delete from schema_migration where name = (
			select name from schema_migration order by name desc limit 1)`); err != nil {
		t.Fatalf("putting the schema behind: %v", err)
	}

	pending, err = store.Pending(ctx)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("says %d are pending, want the one that was removed", len(pending))
	}
}

// A database nothing has ever been applied to is every migration pending, not
// a failure to answer: that is a first start, and the answer is "wait".
func TestADatabaseWithNoSchemaAtAllIsEntirelyPending(t *testing.T) {
	t.Parallel()

	store, dsn := open(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "drop table schema_migration"); err != nil {
		t.Fatalf("removing the record of what was applied: %v", err)
	}

	pending, err := store.Pending(context.Background())
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if len(pending) == 0 {
		t.Error("a database with no schema says nothing is pending")
	}
}
