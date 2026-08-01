package sqlsource

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

func testDatabase(t *testing.T, prefix string) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open database %s: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping database %s: %v", name, err)
	}
	return conn, u.String()
}

// TestFullPull_NAVShapedSourceIntoRealTenant drives the entire engine
// path this card shipped, end to end, against two real Postgres
// databases: one playing the tenant (real migrated schema, real crud
// engine, real audit rows), one playing the legacy NAV SQL database
// (per-company `$`-named table). Template match → constants → fetch →
// CommitRows; then the tenant database is queried directly to prove the
// records and their audit rows actually landed.
func TestFullPull_NAVShapedSourceIntoRealTenant(t *testing.T) {
	ctx := context.Background()

	// The "legacy NAV" database.
	extDB, extDSN := testDatabase(t, "uc_test_sqlsource_ext")
	stmts := []string{
		`CREATE TABLE "Demo Organization$Item" (
			"No_" text NOT NULL,
			"Description" text,
			"Unit Price" numeric(12,2)
		)`,
		`INSERT INTO "Demo Organization$Item" VALUES
			('1000', 'Bicycle', 4000.00),
			('1001', '', NULL),
			('1002', 'Touring Bicycle', 350.00)`,
	}
	for _, s := range stmts {
		if _, err := extDB.Exec(s); err != nil {
			t.Fatalf("seed legacy database: %v", err)
		}
	}

	// The tenant database, real schema.
	tenantDB, _ := testDatabase(t, "uc_test_sqlsource_tenant")
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(ctx, tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	def := purchasing.Item()

	// Template match on the NAV-shaped name.
	_, te, ok := MatchTemplate("Demo Organization$Item")
	if !ok || te.EntityType != "Item" {
		t.Fatalf("expected the nav2009 Item template to match, got ok=%v entity=%q", ok, te.EntityType)
	}

	// Fetch from the legacy database through the read-only data layer.
	ext, err := data.OpenExtSQL(data.ExtDriverPostgres, extDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	headers, rows, err := ext.FetchRows(ctx, "public", "Demo Organization$Item", MaxImportRows)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 legacy rows, got %d", len(rows))
	}

	// Constants close the required-enum gap, then commit for real.
	h, r, m, err := ApplyConstants(headers, rows, te.Mapping, te.Constants)
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: audit.ActorAgent, ID: "sqlsource-import", ModelVersion: "test"}
	results, err := csvimport.CommitRows(ctx, h, r, def, m, engine, actor)
	if err != nil {
		t.Fatal(err)
	}

	// Row 2 has an empty Description → required name is absent → skipped;
	// rows 1 and 3 land.
	if results[0].RecordID == "" || results[2].RecordID == "" {
		t.Fatalf("expected rows 1 and 3 to commit, got %+v", results)
	}
	if results[1].Err == nil || results[1].RecordID != "" {
		t.Fatalf("expected row 2 (blank name) to be skipped, got %+v", results[1])
	}

	got, err := engine.List(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 Item records in the tenant, got %d", len(got))
	}
	for _, rec := range got {
		if rec.Data["item_type"] != "stock" {
			t.Errorf("expected constant item_type=stock on record %s, got %v", rec.ID, rec.Data["item_type"])
		}
	}

	// The writes carry AI-agent audit identity, same transaction as the
	// records (CLAUDE.md's audit rule) — checked in the tenant database
	// directly, not through the engine.
	var auditCount int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'Item' AND actor_type = 'ai_agent'`,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != 2 {
		t.Fatalf("expected 2 ai_agent audit rows (one per committed record), got %d", auditCount)
	}
}

// TestUpsert_ReImportUpdatesInsteadOfDuplicating is uc-infra#101's
// stated proof, against two real Postgres databases: pull a NAV-shaped
// table into a real migrated tenant TWICE. The first run creates every
// record plus its ExternalIdentity row (carrying source_relation); the
// second run — after the legacy source changed a value AND a hand-set
// unmapped field was added to one record — updates the same records in
// place (record count constant, every row Updated=true, the changed
// value reflected, the hand-set field surviving the merge) and writes
// update audit rows. All counts are checked by direct SQL on the tenant
// database, not through the engine.
func TestUpsert_ReImportUpdatesInsteadOfDuplicating(t *testing.T) {
	ctx := context.Background()

	// The "legacy NAV" database.
	extDB, extDSN := testDatabase(t, "uc_test_upsert_ext")
	stmts := []string{
		`CREATE TABLE "Demo Organization$Item" (
			"No_" text NOT NULL,
			"Description" text
		)`,
		`INSERT INTO "Demo Organization$Item" VALUES
			('1000', 'Bicycle'),
			('1002', 'Touring Bicycle')`,
	}
	for _, s := range stmts {
		if _, err := extDB.Exec(s); err != nil {
			t.Fatalf("seed legacy database: %v", err)
		}
	}

	// The tenant database, real schema, real engine.
	tenantDB, _ := testDatabase(t, "uc_test_upsert_tenant")
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(ctx, tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	def := purchasing.Item()
	identityDef := foundation.ExternalIdentity()
	actor := audit.Actor{Type: audit.ActorAgent, ID: "sqlsource-import", ModelVersion: "test"}

	// The registered source this pull runs against — its record ID is
	// what scopes every ExternalIdentity row written below.
	srcRec, err := engine.Create(ctx, foundation.ExternalSQLSource(), map[string]any{
		"name": "Legacy NAV", "driver": "postgres",
		"host": "localhost", "database": "nav",
	}, actor)
	if err != nil {
		t.Fatalf("create ExternalSQLSource record: %v", err)
	}

	// Template match supplies mapping, constants, AND the key column.
	_, te, ok := MatchTemplate("Demo Organization$Item")
	if !ok || te.KeyColumn != "No_" {
		t.Fatalf("expected the nav2009 Item template with KeyColumn No_, got ok=%v key=%q", ok, te.KeyColumn)
	}

	// The schema-qualified relation the rows come from — part of the
	// identity scope (see CommitRowsUpserting's doc comment).
	const relation = `public.Demo Organization$Item`

	pull := func() []UpsertResult {
		t.Helper()
		ext, err := data.OpenExtSQL(data.ExtDriverPostgres, extDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer ext.Close()
		headers, rows, err := ext.FetchRows(ctx, "public", "Demo Organization$Item", MaxImportRows)
		if err != nil {
			t.Fatal(err)
		}
		h, r, m, err := ApplyConstants(headers, rows, te.Mapping, te.Constants)
		if err != nil {
			t.Fatal(err)
		}
		// The raw engine serves both roles here — a system-path import.
		// In the HTTP route the records engine is the RBAC-guarded one
		// and only the identities engine is raw (see RecordEngine's doc
		// comment); the guarded/raw split has its own coverage in
		// internal/kernel/authz.
		results, err := CommitRowsUpserting(ctx, h, r, def, m, te.KeyColumn, srcRec.ID, relation, engine, engine, identityDef, actor)
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	countByType := func(entityType string) int {
		t.Helper()
		var n int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM records WHERE entity_type = $1 AND deleted_at IS NULL`, entityType,
		).Scan(&n); err != nil {
			t.Fatalf("count %s records: %v", entityType, err)
		}
		return n
	}
	countAudit := func(action string) int {
		t.Helper()
		var n int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE entity_type = 'Item' AND action = $1`, action,
		).Scan(&n); err != nil {
			t.Fatalf("count audit_log %s rows: %v", action, err)
		}
		return n
	}

	// First run: everything is new — created records, identity rows.
	first := pull()
	if len(first) != 2 {
		t.Fatalf("expected 2 first-run results, got %d", len(first))
	}
	for i, res := range first {
		if res.Err != nil || res.RecordID == "" || res.Updated {
			t.Fatalf("first run row %d: expected a clean create, got %+v", i+1, res)
		}
	}
	if got := countByType("Item"); got != 2 {
		t.Fatalf("after first run: expected 2 Item records, got %d", got)
	}
	if got := countByType("ExternalIdentity"); got != 2 {
		t.Fatalf("after first run: expected 2 ExternalIdentity records, got %d", got)
	}
	// Every identity row carries the relation it was scoped to.
	var relCount int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE entity_type = 'ExternalIdentity' AND data->>'source_relation' = $1`, relation,
	).Scan(&relCount); err != nil {
		t.Fatalf("count identity rows by source_relation: %v", err)
	}
	if relCount != 2 {
		t.Fatalf("expected both identity rows to carry source_relation %q, got %d", relation, relCount)
	}

	// A human hand-sets a field the mapping does not carry (base_uom_id
	// has no NAV column in this template) — the merge proof: crud.Update
	// is a full replacement, so this survives a refresh only because
	// CommitRowsUpserting merges onto the stored record. Deliberately on
	// the SAME record whose Description changes below, so both halves of
	// the merge are asserted on one record.
	byS, err := engine.ListByField(ctx, def, "sku", "1000")
	if err != nil || len(byS) != 1 {
		t.Fatalf("expected exactly one record with sku 1000, got %d, %v", len(byS), err)
	}
	handEdited := byS[0]
	handEdited.Data["base_uom_id"] = "manual-uom"
	if _, err := engine.Update(ctx, def, handEdited.ID, handEdited.Data, nil, actor); err != nil {
		t.Fatalf("hand-edit record: %v", err)
	}
	updateAuditBefore := countAudit("update")

	// The legacy system changes a value between runs.
	if _, err := extDB.Exec(`UPDATE "Demo Organization$Item" SET "Description" = 'Bicycle Deluxe' WHERE "No_" = '1000'`); err != nil {
		t.Fatalf("update legacy row: %v", err)
	}

	// Second run: same source, same key column — every row must UPDATE
	// the record the first run created, never duplicate it.
	second := pull()
	firstIDs := map[string]bool{first[0].RecordID: true, first[1].RecordID: true}
	for i, res := range second {
		if res.Err != nil || !res.Updated {
			t.Fatalf("second run row %d: expected an update, got %+v", i+1, res)
		}
		if !firstIDs[res.RecordID] {
			t.Fatalf("second run row %d updated %s, which the first run never created", i+1, res.RecordID)
		}
	}
	if got := countByType("Item"); got != 2 {
		t.Fatalf("re-import duplicated: expected the Item count to stay 2, got %d", got)
	}
	if got := countByType("ExternalIdentity"); got != 2 {
		t.Fatalf("re-import duplicated identities: expected 2, got %d", got)
	}

	// The changed source value landed on the SAME record — and the
	// hand-set unmapped field survived the refresh (merge, not replace).
	var name, uom string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'name', coalesce(data->>'base_uom_id', '') FROM records WHERE entity_type = 'Item' AND data->>'sku' = '1000'`,
	).Scan(&name, &uom); err != nil {
		t.Fatalf("read updated record: %v", err)
	}
	if name != "Bicycle Deluxe" {
		t.Fatalf("expected the changed Description to be reflected, got %q", name)
	}
	if uom != "manual-uom" {
		t.Fatalf("expected the hand-set base_uom_id to survive the re-import merge, got %q", uom)
	}

	// The updates were audited (crud.Engine.Update writes the audit row
	// in the same transaction — CLAUDE.md's audit rule).
	if got := countAudit("update"); got != updateAuditBefore+2 {
		t.Fatalf("expected %d Item update audit rows after the re-import, got %d", updateAuditBefore+2, got)
	}
}
