package data

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Unit tests: no database needed ---

func TestOpenExtSQL_RejectsUnknownDriver(t *testing.T) {
	if _, err := OpenExtSQL("oracle", "whatever"); err == nil {
		t.Fatal("expected an error for an unsupported driver")
	}
}

func TestQuoteRelation_MSSQL(t *testing.T) {
	e := &ExtSQL{driver: ExtDriverMSSQL}
	got, err := e.quoteRelation("dbo", "CRONUS International Ltd_$Item")
	if err != nil {
		t.Fatal(err)
	}
	want := "[dbo].[CRONUS International Ltd_$Item]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestQuoteRelation_MSSQLEscapesClosingBracket(t *testing.T) {
	e := &ExtSQL{driver: ExtDriverMSSQL}
	got, err := e.quoteRelation("dbo", "weird]name")
	if err != nil {
		t.Fatal(err)
	}
	if got != "[dbo].[weird]]name]" {
		t.Fatalf("got %q — a ] inside an identifier must double, or the quoting breaks out", got)
	}
}

func TestQuoteRelation_PostgresEscapesQuote(t *testing.T) {
	e := &ExtSQL{driver: ExtDriverPostgres}
	got, err := e.quoteRelation("public", `weird"name`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `"public"."weird""name"` {
		t.Fatalf("got %q — a \" inside an identifier must double, or the quoting breaks out", got)
	}
}

func TestQuoteRelation_RejectsEmptyAndNUL(t *testing.T) {
	e := &ExtSQL{driver: ExtDriverPostgres}
	if _, err := e.quoteRelation("", "x"); err == nil {
		t.Fatal("expected an error for an empty schema name")
	}
	if _, err := e.quoteRelation("public", "a\x00b"); err == nil {
		t.Fatal("expected an error for a NUL in an identifier")
	}
}

func TestFetchRows_RequiresPositiveLimit(t *testing.T) {
	e := &ExtSQL{driver: ExtDriverPostgres}
	if _, _, err := e.FetchRows(context.Background(), "public", "t", 0); err == nil {
		t.Fatal("expected an error for a non-positive row limit")
	}
}

func TestStringifyExtValue(t *testing.T) {
	pureDate := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	withTime := time.Date(2026, 3, 15, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""}, // NULL is csvimport's blank-cell-is-absent
		{[]byte("bytes"), "bytes"},
		// pgx numerics / go-mssqldb decimals arrive as textual bytes.
		{[]byte("1234.56"), "1234.56"},
		// Genuinely binary bytes (NAV rowversion, uniqueidentifier's raw
		// 16 bytes, blobs) must not inject invalid UTF-8 — hex instead.
		{[]byte{0x00, 0x01, 0xff}, "0x0001ff"},
		{"s", "s"},
		{pureDate, "2026-03-15"}, // entity.FieldDate's expected shape
		{withTime, "2026-03-15T09:30:00Z"},
		{true, "true"},
		{int64(42), "42"},
		{float64(19.5), "19.5"},
	}
	for _, c := range cases {
		if got := stringifyExtValue(c.in); got != c.want {
			t.Errorf("stringifyExtValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- Integration tests: a real Postgres acting as the EXTERNAL source
// (ADR-0019: the postgres dialect exists partly so this suite exercises
// the real discovery/fetch path with no new CI service). The scratch
// database gets NAV-shaped relation names — `$`, spaces and quotes in
// identifiers are the correctness risk this feature carries.

func externalSourceDB(t *testing.T) (dsn string, db *sql.DB) {
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

	name := fmt.Sprintf("uc_test_extsql_%d", time.Now().UnixNano())
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
	ext, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open external test database: %v", err)
	}
	t.Cleanup(func() { ext.Close() })

	stmts := []string{
		`CREATE TABLE "CRONUS International Ltd_$Item" (
			"No_" text NOT NULL,
			"Description" text,
			"Unit Price" numeric(12,2),
			"Blocked" boolean,
			"Last Date Modified" timestamp
		)`,
		`INSERT INTO "CRONUS International Ltd_$Item" VALUES
			('1000', 'Bicycle', 4000.00, false, '2026-03-15 00:00:00'),
			('1001', 'Touring Bicycle', NULL, true, NULL)`,
		`CREATE VIEW "CRONUS International Ltd_$ItemView" AS
			SELECT "No_", "Description" FROM "CRONUS International Ltd_$Item"`,
	}
	for _, s := range stmts {
		if _, err := ext.Exec(s); err != nil {
			t.Fatalf("seed external schema: %v", err)
		}
	}
	return u.String(), ext
}

func TestExtSQL_RelationsFindsNAVShapedNamesAndKinds(t *testing.T) {
	dsn, _ := externalSourceDB(t)
	e, err := OpenExtSQL(ExtDriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	rels, err := e.Relations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ExtRelation{}
	for _, r := range rels {
		byName[r.Name] = r
		if r.Schema == "pg_catalog" || r.Schema == "information_schema" {
			t.Errorf("system schema leaked into relation list: %+v", r)
		}
	}
	item, ok := byName["CRONUS International Ltd_$Item"]
	if !ok || item.Kind != "table" {
		t.Fatalf("expected the NAV-shaped table in the list as a table, got %+v (found=%v)", item, ok)
	}
	view, ok := byName["CRONUS International Ltd_$ItemView"]
	if !ok || view.Kind != "view" {
		t.Fatalf("expected the NAV-shaped view in the list as a view, got %+v (found=%v)", view, ok)
	}
}

// TestExtSQL_FetchRowsHeadersInOrdinalOrder — headers come from the
// SELECT * result set itself (there is deliberately no separate
// column-discovery query: one less SQL surface in this file), and must
// arrive in the table's ordinal order for the mapping UI.
func TestExtSQL_FetchRowsHeadersInOrdinalOrder(t *testing.T) {
	dsn, _ := externalSourceDB(t)
	e, err := OpenExtSQL(ExtDriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	headers, _, err := e.FetchRows(context.Background(), "public", "CRONUS International Ltd_$Item", 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"No_", "Description", "Unit Price", "Blocked", "Last Date Modified"}
	if strings.Join(headers, "|") != strings.Join(want, "|") {
		t.Fatalf("got headers %v, want %v (ordinal order)", headers, want)
	}
}

func TestExtSQL_FetchRowsStringifiesLikeACSVCell(t *testing.T) {
	dsn, _ := externalSourceDB(t)
	e, err := OpenExtSQL(ExtDriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	headers, rows, err := e.FetchRows(context.Background(), "public", "CRONUS International Ltd_$Item", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 5 {
		t.Fatalf("expected 5 headers, got %v", headers)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Row 1: numeric renders plainly, bool as true/false, midnight
	// timestamp as a pure ISO date.
	if rows[0][2] != "4000.00" && rows[0][2] != "4000" {
		t.Errorf("numeric column rendered as %q", rows[0][2])
	}
	if rows[0][3] != "false" {
		t.Errorf("bool column rendered as %q, want %q", rows[0][3], "false")
	}
	if rows[0][4] != "2026-03-15" {
		t.Errorf("midnight timestamp rendered as %q, want %q", rows[0][4], "2026-03-15")
	}
	// Row 2: NULLs are empty strings (blank-cell-is-absent).
	if rows[1][2] != "" || rows[1][4] != "" {
		t.Errorf("NULLs must render as empty strings, got %q / %q", rows[1][2], rows[1][4])
	}
}

func TestExtSQL_FetchRowsHonoursLimit(t *testing.T) {
	dsn, _ := externalSourceDB(t)
	e, err := OpenExtSQL(ExtDriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	_, rows, err := e.FetchRows(context.Background(), "public", "CRONUS International Ltd_$Item", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row under limit 1, got %d", len(rows))
	}
}

func TestExtSQL_PingReportsUnreachableSource(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	e, err := OpenExtSQL(ExtDriverPostgres, "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Ping(ctx); err == nil {
		t.Fatal("expected Ping to fail against an unreachable source")
	}
}
