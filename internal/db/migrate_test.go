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

	"github.com/jackc/pgx/v5/pgconn"
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

// TestApplyControl_AfterApply_FailsWithGuidance is the regression test
// for universaltill/uc-infra#84: running cmd/migrate's default
// (-target=legacy, i.e. Apply) against a fresh database and then
// cmd/provision-tenant (which always calls ApplyControl internally)
// against that same database used to fail with a bare "relation
// \"tenants\" already exists (SQLSTATE 42P07)" — Apply's 001_init.sql
// and ApplyControl's 0001_init.sql are two different migration files
// with two different shapes of a same-named tenants table, so
// schema_migrations' filename-keyed dedup can't recognize this as
// "already done." The fix doesn't make this a no-op (the two schemas
// are genuinely different, not duplicates) — it turns the raw Postgres
// error into a clear diagnosis instead of something that reads like
// corruption.
func TestApplyControl_AfterApply_FailsWithGuidance(t *testing.T) {
	db := freshTestDB(t, "uc_test_legacy_then_control")
	ctx := context.Background()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply (legacy): %v", err)
	}

	err := ApplyControl(ctx, db)
	if err == nil {
		t.Fatal("expected ApplyControl to fail against a database already migrated with a different target, got nil")
	}
	if !strings.Contains(err.Error(), "already has a different migration set applied") {
		t.Fatalf("expected a clear diagnosis naming the likely cause, got: %v", err)
	}
	// The original Postgres detail must still be reachable (wrapped, not
	// swallowed) — this is a clearer error, not a replacement one.
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected the underlying Postgres error to still be present (wrapped): %v", err)
	}
}

// TestApply_AfterApplyControl_FailsWithGuidance is the symmetric case:
// ApplyControl first, then Apply (legacy) against the same database.
func TestApply_AfterApplyControl_FailsWithGuidance(t *testing.T) {
	db := freshTestDB(t, "uc_test_control_then_legacy")
	ctx := context.Background()

	if err := ApplyControl(ctx, db); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}

	err := Apply(ctx, db)
	if err == nil {
		t.Fatal("expected Apply to fail against a database already migrated with a different target, got nil")
	}
	if !strings.Contains(err.Error(), "already has a different migration set applied") {
		t.Fatalf("expected a clear diagnosis naming the likely cause, got: %v", err)
	}
}

// TestIsAlreadyExistsError is a unit-level test of the classification
// logic on its own, independent of a real database round-trip —
// TestApplyControl_AfterApply_FailsWithGuidance above only exercises the
// "true, duplicate_table" case that actually occurs in practice; this
// covers the negative paths (a plain non-Postgres error, and a
// PgError whose code isn't in the "already exists" set) directly.
func TestIsAlreadyExistsError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"plain non-Postgres error", errors.New("boom"), false},
		{"PgError with an unrelated code", &pgconn.PgError{Code: "23505"}, false}, // unique_violation
		{"PgError: duplicate_table", &pgconn.PgError{Code: "42P07"}, true},
		{"PgError: duplicate_column", &pgconn.PgError{Code: "42701"}, true},
		{"PgError: duplicate_object", &pgconn.PgError{Code: "42710"}, true},
		{"PgError: duplicate_schema", &pgconn.PgError{Code: "42P06"}, true},
		{"PgError: duplicate_function", &pgconn.PgError{Code: "42723"}, true},
		// duplicate_database (42P04) is deliberately NOT in the allowlist —
		// see alreadyExistsSQLStates' doc comment: migration statements
		// always run inside a transaction, and CREATE DATABASE can't run
		// in one at all, so this code can never actually reach here.
		{"PgError: duplicate_database is NOT classified as already-exists here", &pgconn.PgError{Code: "42P04"}, false},
		{"wrapped PgError", fmt.Errorf("apply migration x: %w", &pgconn.PgError{Code: "42P07"}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAlreadyExistsError(c.err); got != c.want {
				t.Errorf("isAlreadyExistsError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
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

// TestApplyTenant_RecordUniqueKeysTableAndIndexExist confirms
// 0006_record_unique_keys.sql actually lands (universaltill/uc-infra#81):
// the composite UNIQUE constraint here is the real correctness guarantee
// entity.Definition.Unique's enforcement depends on (ADR-0018 section 3) —
// a table that merely EXISTS without the constraint would let
// crud.Engine's writeUniqueConstraintKeys silently stop enforcing
// anything, so this pins the constraint's exact column set, not just the
// table's presence.
func TestApplyTenant_RecordUniqueKeysTableAndIndexExist(t *testing.T) {
	db := freshTestDB(t, "uc_test_tenant")
	ctx := context.Background()

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}

	var cols []string
	rows, err := db.QueryContext(ctx, `
		SELECT a.attname
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conname = 'record_unique_keys_key_uq'
		ORDER BY k.ord`)
	if err != nil {
		t.Fatalf("query record_unique_keys_key_uq columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{"entity_type", "constraint_name", "key_value"}
	if len(cols) != len(want) {
		t.Fatalf("expected record_unique_keys_key_uq to cover %v, got %v", want, cols)
	}
	for i, c := range cols {
		if c != want[i] {
			t.Fatalf("expected record_unique_keys_key_uq column %d to be %q, got %q (full: %v)", i, want[i], c, cols)
		}
	}

	var indexdef string
	if err := db.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'record_unique_keys' AND indexname = $1`,
		"record_unique_keys_record_id_idx",
	).Scan(&indexdef); err != nil {
		t.Fatalf("expected record_unique_keys_record_id_idx to exist after ApplyTenant: %v", err)
	}
}

// TestApplyTenant_GLAccountsSourceRecordIDBackfillLinksPreExistingRows
// confirms 0009_gl_accounts_source_record_id.sql's backfill UPDATE
// (uc-infra#205) actually links a gl_accounts row that predates the
// column to its still-live Account record, by code correlation — not
// just that the column/constraint exist
// (TestApplyTenant_CreatesEverySchemaObjectWithoutTenantID already
// covers presence, not the backfill's own correlation logic). Simulates
// "before this migration ran" by reverting its effect on an
// already-fully-migrated database, inserting rows in the same
// pre-migration shape, then letting ApplyTenant re-run the (now
// unapplied) migration for real — the actual code path
// cmd/migrate -target tenant takes against an existing tenant, not a
// hand-copied duplicate of the migration's own SQL.
func TestApplyTenant_GLAccountsSourceRecordIDBackfillLinksPreExistingRows(t *testing.T) {
	db := freshTestDB(t, "uc_test_tenant")
	ctx := context.Background()

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("ApplyTenant (first pass): %v", err)
	}

	// Revert 0009's effect to reproduce the pre-migration shape.
	if _, err := db.ExecContext(ctx, `ALTER TABLE gl_accounts DROP CONSTRAINT gl_accounts_source_record_id_key`); err != nil {
		t.Fatalf("drop constraint to simulate pre-migration state: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE gl_accounts DROP COLUMN source_record_id`); err != nil {
		t.Fatalf("drop column to simulate pre-migration state: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename = $1`,
		"0009_gl_accounts_source_record_id.sql"); err != nil {
		t.Fatalf("unrecord migration to simulate pre-migration state: %v", err)
	}

	// A pre-existing Account record and its matching, pre-migration
	// gl_accounts row (created the old UpsertByCode-only way — no link).
	var accountID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO records (entity_type, data) VALUES ('Account', $1) RETURNING id`,
		`{"code":"1000","name":"Assets","type":"asset","is_active":true}`,
	).Scan(&accountID); err != nil {
		t.Fatalf("insert pre-existing Account record: %v", err)
	}
	var glAccountID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO gl_accounts (code, name, account_type, currency, is_active) VALUES ('1000', 'Assets', 'asset', 'USD', true) RETURNING id`,
	).Scan(&glAccountID); err != nil {
		t.Fatalf("insert pre-existing gl_accounts row: %v", err)
	}

	// A genuinely orphaned row (no live matching Account) must be left
	// unlinked — the backfill has nothing to correlate it to.
	var orphanID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO gl_accounts (code, name, account_type, currency, is_active) VALUES ('9999', 'Dead Code', 'asset', 'USD', false) RETURNING id`,
	).Scan(&orphanID); err != nil {
		t.Fatalf("insert orphaned gl_accounts row: %v", err)
	}

	// A SOFT-DELETED Account record must not be treated as a live match
	// — linking to it would leave a live Account that legitimately
	// reuses that code permanently unable to save (uc-infra#205 review
	// finding 6). deleted_at IS NULL is load-bearing; this pins it.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO records (entity_type, data, deleted_at) VALUES ('Account', $1, now())`,
		`{"code":"8888","name":"Deleted Account","type":"asset","is_active":false}`,
	); err != nil {
		t.Fatalf("insert soft-deleted Account record: %v", err)
	}
	var softDeletedGLID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO gl_accounts (code, name, account_type, currency, is_active) VALUES ('8888', 'Deleted Account', 'asset', 'USD', false) RETURNING id`,
	).Scan(&softDeletedGLID); err != nil {
		t.Fatalf("insert gl_accounts row matching the soft-deleted Account's code: %v", err)
	}

	// Two LIVE Account records ambiguously sharing a code (a real,
	// pre-uc-infra#204 state this codebase already tolerates —
	// crud/unique_constraints.go's BackfillUniqueConstraintKeys skips
	// rather than repairs pre-existing duplicates). The backfill must
	// leave this unlinked rather than guessing which one is right
	// (uc-infra#205 review finding 5).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO records (entity_type, data) VALUES ('Account', $1), ('Account', $2)`,
		`{"code":"7777","name":"Ambiguous A","type":"asset","is_active":true}`,
		`{"code":"7777","name":"Ambiguous B","type":"asset","is_active":true}`,
	); err != nil {
		t.Fatalf("insert two Account records sharing a code: %v", err)
	}
	var ambiguousGLID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO gl_accounts (code, name, account_type, currency, is_active) VALUES ('7777', 'Ambiguous', 'asset', 'USD', true) RETURNING id`,
	).Scan(&ambiguousGLID); err != nil {
		t.Fatalf("insert gl_accounts row matching the ambiguous code: %v", err)
	}

	if err := ApplyTenant(ctx, db); err != nil {
		t.Fatalf("ApplyTenant (second pass, re-applying 0009): %v", err)
	}

	var linkedID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT source_record_id FROM gl_accounts WHERE id = $1`, glAccountID).
		Scan(&linkedID); err != nil {
		t.Fatalf("read back backfilled source_record_id: %v", err)
	}
	if !linkedID.Valid || linkedID.String != accountID {
		t.Fatalf("expected the pre-existing row to be backfilled with source_record_id=%q, got %+v", accountID, linkedID)
	}

	var orphanLinked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT source_record_id FROM gl_accounts WHERE id = $1`, orphanID).
		Scan(&orphanLinked); err != nil {
		t.Fatalf("read back orphan's source_record_id: %v", err)
	}
	if orphanLinked.Valid {
		t.Fatalf("expected the genuinely orphaned row (no matching live Account) to stay unlinked, got %q", orphanLinked.String)
	}

	var softDeletedLinked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT source_record_id FROM gl_accounts WHERE id = $1`, softDeletedGLID).
		Scan(&softDeletedLinked); err != nil {
		t.Fatalf("read back soft-deleted-match row: %v", err)
	}
	if softDeletedLinked.Valid {
		t.Fatalf("expected the row matching only a SOFT-DELETED Account to stay unlinked, got %q", softDeletedLinked.String)
	}

	var ambiguousLinked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT source_record_id FROM gl_accounts WHERE id = $1`, ambiguousGLID).
		Scan(&ambiguousLinked); err != nil {
		t.Fatalf("read back ambiguous-match row: %v", err)
	}
	if ambiguousLinked.Valid {
		t.Fatalf("expected the row matching TWO live Accounts (ambiguous) to stay unlinked rather than guess, got %q", ambiguousLinked.String)
	}
}
