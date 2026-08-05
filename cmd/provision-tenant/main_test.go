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

// An ai_agent provisioning run must carry a model version — ADR-0001
// §14 makes AI-actor identity first-class, and audit.Actor.Validate
// enforces it. Hard-coding ActorHuman (the pre-fix shape) would have
// written a falsified actor_type for every unattended pipeline
// provisioning run (uc-infra#72, same test shape as cmd/install-module's
// TestActorTypeValidation).
//
// Every rejection here also asserts zero rows in `tenants` — not just a
// non-zero exit — because the validation runs before ANY database work
// (main.go's own comment on this ordering) and `TestMissingActorID`-
// style substring assertions alone don't pin that: `strings.Contains(
// stderr, "model_version")` alone would still pass if the early check
// were deleted, since audit.New's own downstream ErrMissingModelVersion
// contains the same substring — by which point router.Create has
// already provisioned a real tenant database (uc-infra#72 independent
// review, finding 2). Counting `tenants` catches that regression even
// though this test has no tenant of its own to explicitly drop.
func TestProvisionTenant_ActorTypeValidation(t *testing.T) {
	dsn := freshControlDB(t)
	assertNoTenantCreated := func(t *testing.T) {
		t.Helper()
		// A rejected run never reaches db.ApplyControl (validation is
		// before any database work), so `tenants` may not even exist
		// yet — apply control migrations on our own verification
		// connection first, the same way openRouter does for the
		// success-path tests, rather than assume the schema is there.
		control := testexec.Open(t, dsn)
		if err := db.ApplyControl(context.Background(), control); err != nil {
			t.Fatalf("ApplyControl (verification connection): %v", err)
		}
		var count int
		if err := control.QueryRowContext(context.Background(), `SELECT count(*) FROM tenants`).Scan(&count); err != nil {
			t.Fatalf("count tenants: %v", err)
		}
		if count != 0 {
			t.Errorf("expected no tenant to have been created before actor validation failed, got %d", count)
		}
	}

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=x", "-actor-id=pipeline", "-actor-type=ai_agent")
	if code == 0 || !strings.Contains(stderr, "model_version") {
		t.Errorf("expected ai_agent to require -model-version, got code %d: %s", code, stderr)
	}
	assertNoTenantCreated(t)

	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-name=x", "-actor-id=pipeline", "-actor-type=wizard")
	if code == 0 || !strings.Contains(stderr, "invalid actor") {
		t.Errorf("expected an unknown actor type to be rejected, got code %d: %s", code, stderr)
	}
	assertNoTenantCreated(t)

	// The other half of the same falsification, the other direction —
	// a human row carrying a model version (uc-infra#72 independent
	// review, finding 4): Validate() alone only rejects an EMPTY
	// ModelVersion on an agent, never a populated one on a human.
	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-name=x", "-actor-id=pipeline", "-model-version=claude-x")
	if code == 0 || !strings.Contains(stderr, "-model-version is only meaningful") {
		t.Errorf("expected a human actor with -model-version set to be rejected, got code %d: %s", code, stderr)
	}
	assertNoTenantCreated(t)
}

// TestProvisionTenant_ActorTypeAI_WritesRealAuditRows is the positive
// half TestProvisionTenant_ActorTypeValidation deliberately doesn't
// cover: every rejection test above only proves the guardrail exists,
// never that a SUCCESSFUL ai_agent run actually reaches the audit log
// with the right values (uc-infra#72 independent review, finding 1) — a
// wiring mistake that constructs a validated actor but passes a
// different one downstream (exactly the shape of the pre-fix bug in
// cmd/seed-demo-data, which built its Actor separately from
// cmd/install-module's validated one) would leave every rejection test
// green. This runs the real compiled binary end to end and reads the
// provisioned tenant's own audit_log directly.
func TestProvisionTenant_ActorTypeAI_WritesRealAuditRows(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn},
		"-name=AI Actor Smoke Test", "-actor-id=pipeline", "-actor-type=ai_agent", "-model-version=claude-test-1")
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

	var total, wrongActorType, wrongModelVersion int
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM audit_log`).Scan(&total); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if total == 0 {
		t.Fatal("expected foundation provisioning to write at least one audit_log row")
	}
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM audit_log WHERE actor_type != 'ai_agent' OR actor_id != 'pipeline'`,
	).Scan(&wrongActorType); err != nil {
		t.Fatalf("count wrong-actor audit_log rows: %v", err)
	}
	if wrongActorType != 0 {
		t.Errorf("expected every audit_log row to carry actor_type='ai_agent', actor_id='pipeline', got %d that don't", wrongActorType)
	}
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM audit_log WHERE model_version IS DISTINCT FROM 'claude-test-1'`,
	).Scan(&wrongModelVersion); err != nil {
		t.Fatalf("count wrong-model-version audit_log rows: %v", err)
	}
	if wrongModelVersion != 0 {
		t.Errorf("expected every audit_log row to carry model_version='claude-test-1', got %d that don't", wrongModelVersion)
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

// TestProvisionTenant_CRMModule is the smoke layer for the CRM module
// (universaltill/uc-infra#15).
func TestProvisionTenant_CRMModule(t *testing.T) {
	dsn := freshControlDB(t)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-name=CRM Smoke Test", "-actor-id=smoke-test", "-modules=crm")
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

	var count int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM entity_definitions WHERE entity_type = 'Case' AND status = 'published'`,
	).Scan(&count); err != nil {
		t.Fatalf("count published Case: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 published Case definition, got %d", count)
	}
	// CRM references Item and SalesOrder, which this tenant does NOT
	// have — publishing must still work (the module's own claim).
	var foreign int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM entity_definitions WHERE entity_type IN ('Item','SalesOrder')`,
	).Scan(&foreign); err != nil {
		t.Fatalf("count foreign definitions: %v", err)
	}
	if foreign != 0 {
		t.Errorf("expected no purchasing/sales definitions in a crm-only tenant, got %d", foreign)
	}

	var statuses int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records r
		 WHERE r.entity_type = 'Status' AND r.deleted_at IS NULL
		   AND r.data->>'status_type_id' IN (
		     SELECT id::text FROM records
		     WHERE entity_type = 'StatusType' AND data->>'code' = 'case_status' AND deleted_at IS NULL)`,
	).Scan(&statuses); err != nil {
		t.Fatalf("count case statuses: %v", err)
	}
	if statuses != 6 {
		t.Errorf("expected 6 seeded case_status rows, got %d", statuses)
	}
}
