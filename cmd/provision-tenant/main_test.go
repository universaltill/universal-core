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
	"github.com/universaltill/universal-core/internal/kernel/modulebundle"
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

// TestProvisionTenant_AssetsModule is the smoke layer for the Assets
// module (universaltill/uc-infra#19): the real compiled binary
// publishes its entities, forms AND status graph into a real tenant
// database — the third module to exercise the non-nil publishStatuses
// path, and the first whose status graph models an asset's life rather
// than a document's approval.
func TestProvisionTenant_AssetsModule(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Assets Smoke Test", "-actor-id=smoke-test", "-modules=assets")
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
	ctx := context.Background()

	for _, et := range []string{"FixedAsset", "DepreciationSchedule", "MaintenanceOrder"} {
		var count int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM entity_definitions WHERE entity_type = $1 AND status = 'published'`, et,
		).Scan(&count); err != nil {
			t.Fatalf("count published %s: %v", et, err)
		}
		if count != 1 {
			t.Errorf("expected 1 published %s definition, got %d", et, count)
		}
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM form_definitions WHERE entity_type = $1 AND status = 'published'`, et,
		).Scan(&count); err != nil {
			t.Fatalf("count published %s form: %v", et, err)
		}
		if count != 1 {
			t.Errorf("expected 1 published %s form, got %d", et, count)
		}
	}

	// The status graph is real tenant DATA, not registry metadata — so
	// it has to be counted in the records table, and its absence is
	// exactly what would make a FixedAsset uncreatable.
	var statuses int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records r
		 WHERE r.entity_type = 'Status' AND r.deleted_at IS NULL
		   AND r.data->>'status_type_id' IN (
		     SELECT id::text FROM records
		     WHERE entity_type = 'StatusType' AND data->>'code' = 'fixed_asset_status' AND deleted_at IS NULL)`,
	).Scan(&statuses); err != nil {
		t.Fatalf("count seeded statuses: %v", err)
	}
	if statuses != 5 {
		t.Errorf("expected 5 seeded fixed_asset_status rows, got %d", statuses)
	}

	// The module seeds TWO status graphs (asset lifecycle + maintenance
	// order); a second Seed call in the same PublishStatuses is exactly
	// where a code-collision bug would surface, so count it separately.
	var maintenance int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records r
		 WHERE r.entity_type = 'Status' AND r.deleted_at IS NULL
		   AND r.data->>'status_type_id' IN (
		     SELECT id::text FROM records
		     WHERE entity_type = 'StatusType' AND data->>'code' = 'maintenance_order_status' AND deleted_at IS NULL)`,
	).Scan(&maintenance); err != nil {
		t.Fatalf("count maintenance statuses: %v", err)
	}
	if maintenance != 4 {
		t.Errorf("expected 4 seeded maintenance_order_status rows, got %d", maintenance)
	}
}

// TestProvisionTenant_ProjectsModule is the smoke layer for the
// Projects module (universaltill/uc-infra#18): the real compiled binary
// publishes its three entities, three forms and BOTH status graphs.
func TestProvisionTenant_ProjectsModule(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=Projects Smoke Test", "-actor-id=smoke-test", "-modules=projects")
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
	ctx := context.Background()

	for _, et := range []string{"Project", "Task", "TimeEntry"} {
		var count int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM entity_definitions WHERE entity_type = $1 AND status = 'published'`, et,
		).Scan(&count); err != nil {
			t.Fatalf("count published %s: %v", et, err)
		}
		if count != 1 {
			t.Errorf("expected 1 published %s definition, got %d", et, count)
		}
	}

	// Both graphs, counted separately: two Seed calls in one
	// PublishStatuses, and they share a "cancelled" code — exactly where
	// the 2026-07-29 collision bug would resurface.
	for _, g := range []struct {
		code string
		want int
	}{{"project_status", 5}, {"task_status", 5}} {
		var n int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM records r
			 WHERE r.entity_type = 'Status' AND r.deleted_at IS NULL
			   AND r.data->>'status_type_id' IN (
			     SELECT id::text FROM records
			     WHERE entity_type = 'StatusType' AND data->>'code' = $1 AND deleted_at IS NULL)`, g.code,
		).Scan(&n); err != nil {
			t.Fatalf("count %s statuses: %v", g.code, err)
		}
		if n != g.want {
			t.Errorf("expected %d %s rows, got %d", g.want, g.code, n)
		}
	}
}

// TestProvisionTenant_HRModule is the smoke layer for the HR module
// (universaltill/uc-infra#16): the real compiled binary publishes
// Employee and LeaveRequest with both status graphs.
func TestProvisionTenant_HRModule(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=HR Smoke Test", "-actor-id=smoke-test", "-modules=hr")
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
	ctx := context.Background()

	for _, et := range []string{"Employee", "LeaveRequest", "AttendanceRecord"} {
		var count int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM entity_definitions WHERE entity_type = $1 AND status = 'published'`, et,
		).Scan(&count); err != nil {
			t.Fatalf("count published %s: %v", et, err)
		}
		if count != 1 {
			t.Errorf("expected 1 published %s definition, got %d", et, count)
		}
	}
	for _, g := range []struct {
		code string
		want int
	}{{"employee_status", 4}, {"leave_request_status", 5}} {
		var n int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM records r
			 WHERE r.entity_type = 'Status' AND r.deleted_at IS NULL
			   AND r.data->>'status_type_id' IN (
			     SELECT id::text FROM records
			     WHERE entity_type = 'StatusType' AND data->>'code' = $1 AND deleted_at IS NULL)`, g.code,
		).Scan(&n); err != nil {
			t.Fatalf("count %s statuses: %v", g.code, err)
		}
		if n != g.want {
			t.Errorf("expected %d %s rows, got %d", g.want, g.code, n)
		}
	}
}

// TestModulePublishers_MatchReservedModules is the parity test
// modulebundle.ReservedModules' own comment claimed existed and did
// not. Without it the two lists drifted by three modules, and the
// independent review showed the consequence: a bundle declaring an
// unlisted built-in key installs its own Definition, after which the
// real module's Publish silently no-ops and reports success.
func TestModulePublishers_MatchReservedModules(t *testing.T) {
	// foundation is always published and has no entry in
	// modulePublishers, so it is the one legitimate difference.
	want := map[string]bool{"foundation": true}
	for key := range modulePublishers {
		want[key] = true
	}
	for key := range want {
		if !modulebundle.ReservedModules[key] {
			t.Errorf("built-in module %q is missing from modulebundle.ReservedModules — a bundle could claim that key and silently pre-empt the real module", key)
		}
	}
	for key := range modulebundle.ReservedModules {
		if !want[key] {
			t.Errorf("modulebundle.ReservedModules has %q, which is not a built-in module — bundles are needlessly barred from that key", key)
		}
	}
}
