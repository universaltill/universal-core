// Package db embeds and applies this kernel's SQL migrations
// (internal/db/migrations/*.sql — CLAUDE.md: raw SQL lives only here and
// in internal/data). Embedding means the compiled binary carries its own
// migrations; a deployment never needs the source tree or a separate
// migration-runner image alongside it.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey is an arbitrary constant used with pg_advisory_lock to
// serialize concurrent Apply calls — e.g. several replicas booting
// simultaneously against a fresh database. Neither `CREATE TABLE IF NOT
// EXISTS schema_migrations` nor the per-migration `SELECT EXISTS ...
// INSERT` check-then-write is itself a safe compare-and-set under
// concurrent execution (confirmed empirically: 4 of 5 concurrent Apply
// calls against a fresh database failed outright on a duplicate-key
// error from the race in CREATE TABLE IF NOT EXISTS, crash-looping every
// replica but one). The lock makes only one caller run migrations at a
// time; the rest block on pg_advisory_lock until it's done, then find
// everything already applied and return immediately.
const migrationLockKey = 727271

// Apply runs every embedded migration not yet recorded in
// schema_migrations, in filename order (001_, 002_, ... — the append-only
// numbering CLAUDE.md's Process section requires), each in its own
// transaction. Safe to call on every process start: an already-applied
// migration is a no-op, so this is how cmd/universal-core brings a fresh
// database up to date without a separate migrate step. Also safe to call
// concurrently from multiple processes (see migrationLockKey).
func Apply(ctx context.Context, sqlDB *sql.DB) error {
	// Advisory locks are scoped to the SESSION (the specific connection)
	// that acquired them, so the lock must be held on one pinned Conn for
	// Apply's whole duration — the pool's sql.DB is still used for the
	// actual migration statements below, since the lock's only job is to
	// stop a second concurrent caller from proceeding, not to keep every
	// individual statement on the same connection.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// A background context: ctx may already be canceled/expired by
		// the time Apply returns (e.g. the caller's own deadline), but
		// the lock must still be released — an unlock that never runs
		// wedges every future Apply call against this database forever,
		// not just this one.
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := applyOne(ctx, sqlDB, name); err != nil {
			return err
		}
	}
	return nil
}

func applyOne(ctx context.Context, sqlDB *sql.DB, name string) error {
	var applied bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, name,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", name, err)
	}
	if applied {
		return nil
	}

	stmt, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for migration %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	if _, err := tx.ExecContext(ctx, string(stmt)); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (filename) VALUES ($1)`, name,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	return tx.Commit()
}
