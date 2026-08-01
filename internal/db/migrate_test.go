package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// quoteIdentifier double-quotes a Postgres identifier, escaping any
// embedded double-quote by doubling it — the standard SQL-identifier
// quoting rule. freshTestDB's generated names are always
// prefix_<unixnano> (fixed ASCII alphanumeric/underscore), so this is
// defense in depth here, not a real injection surface.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

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

// freshTestDB opens a connection to a brand-new database on the same
// server as TEST_DATABASE_URL, named uniquely per call — ApplyControl/
// ApplyTenant need a genuinely fresh, empty database each (unlike Apply's
// tests above, which all happily share one already-migrated database
// across the whole file), since a real deployment only ever runs each
// one once per tenant/control database. Skips (like testDB) if
// TEST_DATABASE_URL isn't set.
func freshTestDB(t *testing.T, namePrefix string) *sql.DB {
	t.Helper()
	base := testDB(t)

	name := fmt.Sprintf("%s_%d", namePrefix, time.Now().UnixNano())
	if _, err := base.Exec(`CREATE DATABASE ` + quoteIdentifier(name)); err != nil {
		t.Fatalf("create fresh database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// Terminate any lingering backends first — DROP DATABASE fails
		// outright if this test's own db connection (closed via the
		// db.Close() below) hasn't fully disconnected yet.
		_, _ = base.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = base.Exec(`DROP DATABASE IF EXISTS ` + quoteIdentifier(name))
	})

	dsn := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name

	fresh, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open fresh database %s: %v", name, err)
	}
	t.Cleanup(func() { fresh.Close() })
	if err := fresh.Ping(); err != nil {
		t.Fatalf("ping fresh database %s: %v", name, err)
	}
	if _, err := fresh.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	return fresh
}

// TestApplyControl_CreatesTenantsTable confirms the control-plane
// migration set (ADR-0003) creates exactly the tenants registry, against
// a genuinely fresh database — not the shared one Apply's own tests use.
func TestApplyControl_CreatesTenantsTable(t *testing.T) {
	db := freshTestDB(t, "uc_test_control")
	ctx := context.Background()

	if err := ApplyControl(ctx, db); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}

	for _, col := range []string{"id", "name", "region", "db_name", "zitadel_org_id", "created_at"} {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = $1)`, col,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check tenants.%s: %v", col, err)
		}
		if !exists {
			t.Fatalf("expected tenants.%s to exist after ApplyControl", col)
		}
	}

	// The control database must never carry any of the per-tenant tables
	// — that would defeat the whole point of the split.
	for _, table := range []string{"records", "audit_log", "workflow_jobs", "entity_definitions"} {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if exists {
			t.Fatalf("did not expect tenant table %q in the control-plane database", table)
		}
	}
}

// TestApplyControl_IsIdempotent mirrors TestApply_IsIdempotent for the
// control-plane migration set.
func TestApplyControl_IsIdempotent(t *testing.T) {
	db := freshTestDB(t, "uc_test_control")
	ctx := context.Background()

	if err := ApplyControl(ctx, db); err != nil {
		t.Fatalf("first ApplyControl: %v", err)
	}
	if err := ApplyControl(ctx, db); err != nil {
		t.Fatalf("second ApplyControl should be a no-op, got: %v", err)
	}
}

// TestApplyTenant_CreatesEverySchemaObjectWithoutTenantID confirms the
// per-tenant migration set (ADR-0003) creates every tenant-owned table,
// and — the whole point of this migration — none of them carry a
// tenant_id column, against a genuinely fresh database.
func TestApplyTenant_CreatesEverySchemaObjectWithoutTenantID(t *testing.T) {
	db := freshTestDB(t, "uc_test_tenant")
	ctx := context.Background()

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}

	tables := []string{
		"entity_definitions", "form_definitions", "workflow_definitions",
		"records", "audit_log", "gl_accounts", "journal_entries",
		"journal_lines", "workflow_jobs", "schema_migrations",
	}
	for _, table := range tables {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %q to exist after ApplyTenant", table)
		}
	}

	// No table in a tenant database may carry a tenant_id column —
	// that's the entire point of database-per-tenant (ADR-0003 §4): a
	// tenant_id column left over anywhere would be silent dead weight at
	// best, and a sign the migration split missed something at worst.
	var leftover string
	err := db.QueryRowContext(ctx,
		`SELECT table_name FROM information_schema.columns WHERE column_name = 'tenant_id' LIMIT 1`,
	).Scan(&leftover)
	if err == nil {
		t.Fatalf("expected no tenant_id column anywhere in a tenant database, found one on %q", leftover)
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("check for leftover tenant_id columns: %v", err)
	}

	// tenants itself must never appear in a tenant database — it's the
	// control-plane's table only.
	var tenantsExists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'tenants')`,
	).Scan(&tenantsExists); err != nil {
		t.Fatalf("check tenants table: %v", err)
	}
	if tenantsExists {
		t.Fatal("did not expect the control-plane tenants table in a tenant database")
	}
}

// TestApplyTenant_RecordsListSortIndexExists confirms
// 0005_records_list_sort_index.sql's index actually lands (universaltill/
// uc-infra#50): every list page's default (unsorted) path filters on
// entity_type and orders by created_at, so that's exactly the shape this
// index needs to cover for the planner to use it instead of a seq scan
// + in-memory sort.
func TestApplyTenant_RecordsListSortIndexExists(t *testing.T) {
	db := freshTestDB(t, "uc_test_tenant")
	ctx := context.Background()

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}

	var indexdef string
	err := db.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'records' AND indexname = $1`,
		"idx_records_type_created",
	).Scan(&indexdef)
	if err != nil {
		t.Fatalf("expected index idx_records_type_created on records to exist after ApplyTenant: %v", err)
	}
	// Column ORDER matters here, not just presence: entity_type must lead
	// (it's the equality predicate) with created_at second (the sort
	// key) — reversed, the planner falls back to the pre-fix seq-scan
	// + sort plan for anything but the most common entity type. Asserting
	// the literal btree clause (rather than two independent Contains
	// checks) is what actually distinguishes "the fix" from "an index
	// that happens to mention the same two column names."
	const wantClause = "USING btree (entity_type, created_at) WHERE (deleted_at IS NULL)"
	if !strings.Contains(indexdef, wantClause) {
		t.Fatalf("expected idx_records_type_created to be %q, got definition: %s", wantClause, indexdef)
	}
}

// TestApplyTenant_IsIdempotent mirrors TestApply_IsIdempotent for the
// per-tenant migration set.
func TestApplyTenant_IsIdempotent(t *testing.T) {
	db := freshTestDB(t, "uc_test_tenant")
	ctx := context.Background()

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("first ApplyTenant: %v", err)
	}
	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("second ApplyTenant should be a no-op, got: %v", err)
	}
}

// TestApplyControl_And_ApplyTenant_LockKeysDoNotCollide is the regression
// test for the reason migrationLockKeyControl/migrationLockKeyTenant
// exist as distinct constants from migrationLockKey: pg_advisory_lock's
// key space is shared across the whole Postgres server, not scoped to
// the database a session is connected to, so running all three
// concurrently against three different fresh databases must not
// serialize or deadlock against each other.
func TestApplyControl_And_ApplyTenant_LockKeysDoNotCollide(t *testing.T) {
	controlDB := freshTestDB(t, "uc_test_control")
	tenantDB := freshTestDB(t, "uc_test_tenant")
	legacyDB := freshTestDB(t, "uc_test_legacy")
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); errs[0] = ApplyControl(ctx, controlDB) }()
	go func() { defer wg.Done(); errs[1] = ApplyTenant(ctx, tenantDB) }()
	go func() { defer wg.Done(); errs[2] = Apply(ctx, legacyDB) }()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent caller %d failed: %v", i, err)
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
