package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
)

// TestSlugify pins the derivation rules the 0002/006 backfills mirror in
// SQL: lowercase, non-alphanumeric runs collapsed to '-', trimmed, empty
// falls back to "tenant", digits survive.
func TestSlugify(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain two words", "Demo Organization", "demo-organization"},
		{"unicode and punctuation collapse", "  Ünïcode & Co.  ", "n-code-co"},
		{"digits kept", "Tenant 42", "tenant-42"},
		{"only digits kept", "42", "42"},
		{"trailing punctuation trimmed", "ACME!!!", "acme"},
		{"pre-hyphenated collapses and trims", "--Already--Sluggy--", "already-sluggy"},
		{"empty falls back", "", "tenant"},
		{"whitespace-only falls back", "   ", "tenant"},
		{"punctuation-only falls back", "!!!", "tenant"},
		{"mixed case", "AcMe TeXtIlEs", "acme-textiles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slugify(tc.in); got != tc.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlugify_ReservedSegments: every reserved top-level path segment is
// suffixed "-tenant" so a tenant named after a real route can never
// shadow it under /t/'s sibling paths — and a name that merely CONTAINS
// a reserved word is left alone.
func TestSlugify_ReservedSegments(t *testing.T) {
	reserved := []string{
		"api", "ui", "t", "static", "settings", "healthz", "records",
		"forms", "modules", "import", "export", "reports",
		"issue-report", "workflow-jobs",
	}
	for _, word := range reserved {
		t.Run(word, func(t *testing.T) {
			want := word + "-tenant"
			if got := Slugify(word); got != want {
				t.Fatalf("Slugify(%q) = %q, want %q", word, got, want)
			}
		})
	}
	// Reserved words are matched against the WHOLE derived slug, not as
	// substrings — and case-insensitively, since Slugify lowercases first.
	if got := Slugify("API"); got != "api-tenant" {
		t.Fatalf("Slugify(\"API\") = %q, want \"api-tenant\"", got)
	}
	if got := Slugify("Issue-Report"); got != "issue-report-tenant" {
		t.Fatalf("Slugify(\"Issue-Report\") = %q, want \"issue-report-tenant\"", got)
	}
	if got := Slugify("api gateway"); got != "api-gateway" {
		t.Fatalf("Slugify(\"api gateway\") = %q, want \"api-gateway\"", got)
	}
}

// freshSlugDB mirrors freshTenantDB (records_test.go) for the two tenants
// schemas the slug column lives in: apply is db.Apply for the legacy
// shared schema (Create's only valid target) or db.ApplyControl for the
// control-plane schema (CreateWithDatabase's). Skips without
// TEST_DATABASE_URL, same as every other integration test here.
func freshSlugDB(t *testing.T, prefix string, apply func(context.Context, *sql.DB) error) *sql.DB {
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

	name := fmt.Sprintf("uc_test_%s_%d", prefix, time.Now().UnixNano())
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
	testDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open database %s: %v", name, err)
	}
	t.Cleanup(func() { testDB.Close() })
	if err := testDB.Ping(); err != nil {
		t.Fatalf("ping database %s: %v", name, err)
	}
	if _, err := testDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := apply(context.Background(), testDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return testDB
}

// TestTenantRepo_Slug_LegacyCreate: Create (legacy shared schema, 006's
// slug column) derives the slug from the name at INSERT time.
func TestTenantRepo_Slug_LegacyCreate(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_legacy", db.Apply)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id, err := repo.Create(ctx, "Demo Organization", "eu-west")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	slug, err := repo.SlugByID(ctx, id)
	if err != nil {
		t.Fatalf("SlugByID: %v", err)
	}
	if slug != "demo-organization" {
		t.Fatalf("expected slug \"demo-organization\", got %q", slug)
	}
}

// TestTenantRepo_Slug_CreateWithDatabase: the control-plane insert
// (ADR-0003) gets the same slug derivation, alongside its generated
// db_name.
func TestTenantRepo_Slug_CreateWithDatabase(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_control", db.ApplyControl)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id, dbName, err := repo.CreateWithDatabase(ctx, "Demo Organization", "eu-west")
	if err != nil {
		t.Fatalf("CreateWithDatabase: %v", err)
	}
	if dbName == "" {
		t.Fatal("expected a generated db_name")
	}
	slug, err := repo.SlugByID(ctx, id)
	if err != nil {
		t.Fatalf("SlugByID: %v", err)
	}
	if slug != "demo-organization" {
		t.Fatalf("expected slug \"demo-organization\", got %q", slug)
	}
}

// TestTenantRepo_Slug_DuplicateNameGetsNumericSuffix: two tenants with
// the SAME name — the unique index (tenants_slug_key) rejects the first
// slug and withSlugRetry lands the second on "-2".
func TestTenantRepo_Slug_DuplicateNameGetsNumericSuffix(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_dup", db.ApplyControl)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id1, _, err := repo.CreateWithDatabase(ctx, "Acme Corp", "eu-west")
	if err != nil {
		t.Fatalf("first CreateWithDatabase: %v", err)
	}
	id2, _, err := repo.CreateWithDatabase(ctx, "Acme Corp", "eu-west")
	if err != nil {
		t.Fatalf("second CreateWithDatabase: %v", err)
	}

	slug1, err := repo.SlugByID(ctx, id1)
	if err != nil {
		t.Fatalf("SlugByID first: %v", err)
	}
	slug2, err := repo.SlugByID(ctx, id2)
	if err != nil {
		t.Fatalf("SlugByID second: %v", err)
	}
	if slug1 != "acme-corp" {
		t.Fatalf("expected first slug \"acme-corp\", got %q", slug1)
	}
	if slug2 != "acme-corp-2" {
		t.Fatalf("expected second slug \"acme-corp-2\", got %q", slug2)
	}
}

// TestTenantRepo_Slug_GetBySlugRoundtrip: slug -> id resolves what was
// just created; an unknown slug is ErrNotFound (the 404 tenantPrefixed
// leans on).
func TestTenantRepo_Slug_GetBySlugRoundtrip(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_getbyslug", db.ApplyControl)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id, _, err := repo.CreateWithDatabase(ctx, "Roundtrip Tenant", "eu-west")
	if err != nil {
		t.Fatalf("CreateWithDatabase: %v", err)
	}
	got, err := repo.GetBySlug(ctx, "roundtrip-tenant")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got != id {
		t.Fatalf("GetBySlug returned %q, want %q", got, id)
	}

	if _, err := repo.GetBySlug(ctx, "no-such-slug"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown slug, got %v", err)
	}
}

// TestTenantRepo_Slug_SlugByIDRoundtrip: id -> slug (the canonicalizing
// redirect's lookup) and ErrNotFound for an id with no row.
func TestTenantRepo_Slug_SlugByIDRoundtrip(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_slugbyid", db.ApplyControl)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id, _, err := repo.CreateWithDatabase(ctx, "Slug By Id Tenant", "eu-west")
	if err != nil {
		t.Fatalf("CreateWithDatabase: %v", err)
	}
	slug, err := repo.SlugByID(ctx, id)
	if err != nil {
		t.Fatalf("SlugByID: %v", err)
	}
	if slug != "slug-by-id-tenant" {
		t.Fatalf("expected slug \"slug-by-id-tenant\", got %q", slug)
	}

	if _, err := repo.SlugByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a missing tenant id, got %v", err)
	}
}

// TestTenantRepo_Slug_NamesByIDs: display names resolve for existing
// ids; an id with no row is simply absent from the map, not an error
// (the tenant picker's "deleted since the session was sealed" case).
func TestTenantRepo_Slug_NamesByIDs(t *testing.T) {
	tdb := freshSlugDB(t, "data_slug_names", db.ApplyControl)
	ctx := context.Background()
	repo := NewTenantRepo(tdb)

	id1, _, err := repo.CreateWithDatabase(ctx, "First Tenant", "eu-west")
	if err != nil {
		t.Fatalf("first CreateWithDatabase: %v", err)
	}
	id2, _, err := repo.CreateWithDatabase(ctx, "Second Tenant", "eu-west")
	if err != nil {
		t.Fatalf("second CreateWithDatabase: %v", err)
	}
	missing := "00000000-0000-0000-0000-000000000000"

	names, err := repo.NamesByIDs(ctx, []string{id1, id2, missing})
	if err != nil {
		t.Fatalf("NamesByIDs: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 resolved names, got %d: %v", len(names), names)
	}
	if names[id1] != "First Tenant" {
		t.Fatalf("expected %q -> \"First Tenant\", got %q", id1, names[id1])
	}
	if names[id2] != "Second Tenant" {
		t.Fatalf("expected %q -> \"Second Tenant\", got %q", id2, names[id2])
	}
	if _, ok := names[missing]; ok {
		t.Fatalf("expected missing id to be absent from the map, got %v", names)
	}
}
