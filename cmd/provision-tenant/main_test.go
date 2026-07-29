// Smoke tests for the real compiled cmd/provision-tenant binary.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "provision-tenant")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func run(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return testexec.Run(t, binPath, env, args...)
}

// freshControlDB returns an *unmigrated* fresh database plus its DSN —
// provision-tenant applies control-plane migrations itself on startup,
// same as cmd/universal-core does, so the test must not do it first
// (that would leave the binary's own db.ApplyControl call untested).
func freshControlDB(t *testing.T) (dsn string) {
	t.Helper()
	return testexec.FreshDatabase(t, "uc_test_provision_control")
}

func openRouter(t *testing.T, controlDSN string) *tenantdb.Router {
	t.Helper()
	control := testexec.Open(t, controlDSN)
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl (verification connection): %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	return router
}

func TestProvisionTenant_MissingDatabaseURL_FailsFast(t *testing.T) {
	_, stderr, code := run(t, []string{}, "-name=x", "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(stderr, "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got stderr: %q", stderr)
	}
}

func TestProvisionTenant_MissingActorID_FailsFast(t *testing.T) {
	dsn := freshControlDB(t)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=x")
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

func TestProvisionTenant_MissingNameAndTenantID_FailsFast(t *testing.T) {
	dsn := freshControlDB(t)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with neither -name nor -tenant-id set")
	}
	if !strings.Contains(stderr, "-name is required") {
		t.Fatalf("expected -name error, got stderr: %q", stderr)
	}
}

func TestProvisionTenant_UnknownModule_FailsFast(t *testing.T) {
	dsn := freshControlDB(t)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=x", "-actor-id=a", "-modules=nonsense")
	if code == 0 {
		t.Fatal("expected non-zero exit with an unknown module")
	}
	if !strings.Contains(stderr, `unknown module "nonsense"`) {
		t.Fatalf("expected unknown-module error, got stderr: %q", stderr)
	}
}

func TestProvisionTenant_NewTenant_PublishesFoundationOnly(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Foundation Only", "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatal("expected the new tenant id printed to stdout")
	}
	testexec.DropTenantDatabase(t, testexec.Open(t, dsn), id)

	router := openRouter(t, dsn)
	tenantDB, err := router.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve provisioned tenant database: %v", err)
	}
	assertPublished(t, tenantDB, "entity_definitions", "PurchaseOrder", false)
	assertPublished(t, tenantDB, "entity_definitions", "Party", true)
	assertPublished(t, tenantDB, "form_definitions", "Party", true)
}

func TestProvisionTenant_ReuseTenantID_AddsRequestedModule(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Reuse Me", "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr: %s", code, stderr)
	}
	id := strings.TrimSpace(stdout)
	testexec.DropTenantDatabase(t, testexec.Open(t, dsn), id)

	// Reusing -tenant-id (no -name) with -modules purchasing must not
	// create a second tenant and must bring purchasing online for the
	// existing one — exactly the "pick up a newly added module" use
	// case this binary's own doc comment describes.
	stdout, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-tenant-id="+id, "-actor-id=smoke-test", "-modules=purchasing")
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr: %s", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != id {
		t.Fatalf("expected the same tenant id %q echoed back, got %q", id, got)
	}

	router := openRouter(t, dsn)
	tenantDB, err := router.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve tenant database: %v", err)
	}
	assertPublished(t, tenantDB, "entity_definitions", "PurchaseOrder", true)
	assertPublished(t, tenantDB, "form_definitions", "PurchaseOrder", true)

	var tenantCount int
	control := testexec.Open(t, dsn)
	if err := control.QueryRowContext(context.Background(), `SELECT count(*) FROM tenants`).Scan(&tenantCount); err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	if tenantCount != 1 {
		t.Fatalf("expected exactly 1 tenant row, got %d", tenantCount)
	}
}

func TestProvisionTenant_RerunSameModule_IsIdempotent(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Idempotent", "-actor-id=smoke-test", "-modules=purchasing,sales")
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr: %s", code, stderr)
	}
	id := strings.TrimSpace(stdout)
	testexec.DropTenantDatabase(t, testexec.Open(t, dsn), id)

	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-tenant-id="+id, "-actor-id=smoke-test", "-modules=purchasing,sales")
	if code != 0 {
		t.Fatalf("second run should be a no-op, got exit %d, stderr: %s", code, stderr)
	}

	router := openRouter(t, dsn)
	tenantDB, err := router.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve tenant database: %v", err)
	}
	var count int
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM entity_definitions WHERE entity_type = 'PurchaseOrder' AND status = 'published'`,
	).Scan(&count); err != nil {
		t.Fatalf("count published PurchaseOrder definitions: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 published PurchaseOrder definition after re-running provision-tenant, got %d", count)
	}
}

// TestProvisionTenant_FinanceModule_PublishesWithoutStatuses is finance's
// own regression test for the modulePublishers map's nilable
// publishStatuses field — finance is the first module with no
// StatusType-managed entity (finance.go's own doc comment explains why),
// so this is the first real exercise of the `if p.publishStatuses !=
// nil` branch in main() via a real subprocess, not just purchasing/
// sales always taking the non-nil path.
func TestProvisionTenant_FinanceModule_PublishesWithoutStatuses(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Finance Only", "-actor-id=smoke-test", "-modules=finance")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	id := strings.TrimSpace(stdout)
	testexec.DropTenantDatabase(t, testexec.Open(t, dsn), id)

	router := openRouter(t, dsn)
	tenantDB, err := router.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve tenant database: %v", err)
	}
	assertPublished(t, tenantDB, "entity_definitions", "Account", true)
	assertPublished(t, tenantDB, "form_definitions", "Account", true)
}

func assertPublished(t *testing.T, tenantDB *sql.DB, table, entityType string, wantPublished bool) {
	t.Helper()
	var exists bool
	err := tenantDB.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE entity_type = $1 AND status = 'published')`, entityType,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check %s.%s published: %v", table, entityType, err)
	}
	if exists != wantPublished {
		t.Fatalf("%s.%s published = %v, want %v", table, entityType, exists, wantPublished)
	}
}
