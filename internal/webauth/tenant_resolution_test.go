package webauth

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
)

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

// TestOrgToTenantResolution is the real, DB-backed proof of this
// package's one genuinely new piece versus ut-cloud's own webauth
// (which never needs a database at all): a Zitadel org id extracted
// from an id_token's claims (orgIDFromClaims) resolving, through
// data.TenantRepo, to the correct Universal Core tenant — end to end,
// against real Postgres, not each half tested in isolation and merely
// assumed to compose correctly.
func TestOrgToTenantResolution(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenants := data.NewTenantRepo(db)

	tenantID, err := tenants.Create(ctx, "Acme Textiles", "eu-west")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	// zitadel_org_id is UNIQUE — derived from the tenant's own generated
	// id (not a fixed literal) so repeated runs against a persistent
	// database never collide, the same way every other DB-backed test in
	// this repo relies on gen_random_uuid()'d identity instead of a
	// fixed test string.
	orgIDValue := "zitadel-org-" + tenantID
	if err := tenants.SetZitadelOrgID(ctx, tenantID, orgIDValue); err != nil {
		t.Fatalf("link zitadel org: %v", err)
	}

	// The exact claim shape a real id_token carries (project_role_assertion
	// on Universal Core's Zitadel project — see uc-infra's zitadel.tf).
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{orgIDValue: "acme.id.universaltill.com"},
		},
	}
	orgID, ok := orgIDFromClaims(claims)
	if !ok {
		t.Fatal("expected orgIDFromClaims to extract an org id")
	}

	resolved, err := tenants.GetByZitadelOrgID(ctx, orgID)
	if err != nil {
		t.Fatalf("GetByZitadelOrgID: %v", err)
	}
	if resolved != tenantID {
		t.Fatalf("resolved tenant %q, want %q", resolved, tenantID)
	}
}

// TestOrgToTenantResolution_UnlinkedOrgIsNotFound confirms a real
// Zitadel org with no tenants row linked to it (the "notLinked" case in
// handleCallback) surfaces as data.ErrNotFound specifically — not a
// generic error handleCallback couldn't distinguish from a genuine DB
// failure.
func TestOrgToTenantResolution_UnlinkedOrgIsNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenants := data.NewTenantRepo(db)

	_, err := tenants.GetByZitadelOrgID(ctx, "some-org-nobody-linked")
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected data.ErrNotFound, got %v", err)
	}
}

// TestSetZitadelOrgID_UnknownTenantIsNotFound confirms linking a
// nonexistent tenant id fails loud rather than silently no-op-ing (an
// UPDATE matching zero rows).
func TestSetZitadelOrgID_UnknownTenantIsNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenants := data.NewTenantRepo(db)

	err := tenants.SetZitadelOrgID(ctx, "99999999-9999-9999-9999-999999999999", "some-org")
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected data.ErrNotFound, got %v", err)
	}
}
