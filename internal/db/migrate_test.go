package db

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// embeddedMigrationCount reads the count directly off migrationsFS rather
// than hardcoding a number here — a hardcoded count goes stale the
// moment a new migration file lands (it already did once, silently,
// until 004_tenant_zitadel_org.sql's own test run caught it).
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	return len(entries)
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// TestApply_CreatesEverySchemaObject applies against a fresh (unmigrated)
// database and confirms tables from every migration file exist —
// verifying the embedded multi-statement .sql files actually ran end to
// end via database/sql + pgx, not just that Apply returned nil.
func TestApply_CreatesEverySchemaObject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, table := range []string{
		"tenants", "entity_definitions", "form_definitions", "records",
		"audit_log", "gl_accounts", "journal_entries", "journal_lines",
		"workflow_jobs", "workflow_definitions", "schema_migrations",
	} {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %q to exist after Apply", table)
		}
	}
}

// TestApply_IsIdempotent confirms a second call (simulating a process
// restart against an already-migrated database) is a safe no-op, not a
// "relation already exists" error.
func TestApply_IsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("second Apply should be a no-op, got: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if want := embeddedMigrationCount(t); count != want {
		t.Fatalf("expected exactly %d recorded migrations, got %d", want, count)
	}
}

// TestApply_FormDefinitionsHasActorTrackingColumns is a narrow regression
// check that 003_definition_registry.sql's ALTER TABLE actually landed —
// the migration that brought form_definitions up to parity with
// entity_definitions (see docs/code-reviews/2026-07-19-definition-
// registry.md in uc-infra).
func TestApply_FormDefinitionsHasActorTrackingColumns(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, col := range []string{"created_by_type", "created_by", "approved_by"} {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = 'form_definitions' AND column_name = $1)`, col,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check column %s: %v", col, err)
		}
		if !exists {
			t.Fatalf("expected form_definitions.%s to exist after Apply", col)
		}
	}
}

// TestApply_ConcurrentCallersDoNotFail is the regression test for the
// code-review finding that Apply's "CREATE TABLE IF NOT EXISTS
// schema_migrations" plus per-migration "SELECT EXISTS ... INSERT" isn't
// itself a safe compare-and-set under concurrent execution: several
// replicas booting simultaneously against a fresh (unmigrated) database
// used to crash-loop every replica but one on a duplicate-key error.
// pg_advisory_lock (migrationLockKey) now serializes concurrent callers.
func TestApply_ConcurrentCallersDoNotFail(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = Apply(ctx, db)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Apply caller %d failed: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if want := embeddedMigrationCount(t); count != want {
		t.Fatalf("expected exactly %d recorded migrations after %d concurrent Apply calls, got %d", want, callers, count)
	}
}
