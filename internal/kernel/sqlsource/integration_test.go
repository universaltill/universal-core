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
