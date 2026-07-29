// Smoke tests for the real compiled cmd/seed-demo-data binary.
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
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "seed-demo-data")
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

type moduleSeed struct {
	publish, publishForms, publishStatuses func(context.Context, *sql.DB, audit.Actor) error
}

var moduleSeeds = map[string]moduleSeed{
	"purchasing": {purchasing.Publish, purchasing.PublishForms, purchasing.PublishStatuses},
	"sales":      {sales.Publish, sales.PublishForms, sales.PublishStatuses},
}

// provisionedTenant creates a fresh control database plus a new tenant
// with foundation always published, and each requested module fully
// published (entities, forms, and statuses) — the real
// cmd/provision-tenant path, called directly rather than shelled out to
// since this package already needs these internal imports to verify
// what the seed binary under test actually wrote. Returns the control
// DSN and the new tenant id.
func provisionedTenant(t *testing.T, modules ...string) (controlDSN, tenantID string) {
	t.Helper()
	controlDSN = testexec.FreshDatabase(t, "uc_test_seed_control")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	tenantID, err = router.Create(ctx, "Seed Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, control, tenantID)
	tenantDB, err := router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	for _, m := range modules {
		seed, ok := moduleSeeds[m]
		if !ok {
			t.Fatalf("provisionedTenant: unknown module %q", m)
		}
		if err := seed.publish(ctx, tenantDB, actor); err != nil {
			t.Fatalf("%s Publish: %v", m, err)
		}
		if err := seed.publishForms(ctx, tenantDB, actor); err != nil {
			t.Fatalf("%s PublishForms: %v", m, err)
		}
		if err := seed.publishStatuses(ctx, tenantDB, actor); err != nil {
			t.Fatalf("%s PublishStatuses: %v", m, err)
		}
	}
	return controlDSN, tenantID
}

func TestSeedDemoData_MissingDatabaseURL_FailsFast(t *testing.T) {
	_, stderr, code := run(t, []string{}, "-tenant-id=x", "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(stderr, "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_MissingTenantID_FailsFast(t *testing.T) {
	controlDSN, _ := provisionedTenant(t, "purchasing", "sales")
	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with -tenant-id unset")
	}
	if !strings.Contains(stderr, "-tenant-id is required") {
		t.Fatalf("expected -tenant-id error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_MissingActorID_FailsFast(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales")
	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id)
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_UnprovisionedModule_FailsCleanly(t *testing.T) {
	// Foundation-only tenant: seed-demo-data unconditionally seeds
	// PurchaseOrders/SalesOrders, so it must fail loudly (not panic, not
	// silently skip) when purchasing/sales were never published.
	controlDSN, id := provisionedTenant(t) // no modules
	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit against a tenant with no modules published")
	}
	if !strings.Contains(stderr, "has this module been provisioned") {
		t.Fatalf("expected a clear provisioning error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_SeedsSampleRecordsAndIsIdempotent(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr: %s", code, stderr)
	}

	control := testexec.Open(t, controlDSN)
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	tenantDB, err := router.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	counts := map[string]int{}
	for _, entityType := range []string{"Party", "Item", "PurchaseOrder", "SalesOrder", "CustomerInvoice"} {
		counts[entityType] = countRecords(t, tenantDB, entityType)
		if counts[entityType] == 0 {
			t.Fatalf("expected at least one %s record after seeding, got 0", entityType)
		}
	}

	// Re-run: must be idempotent (getOrCreate, per this binary's own
	// doc comment) — record counts must not double.
	_, stderr, code = run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run should be a no-op, got exit %d, stderr: %s", code, stderr)
	}
	for entityType, want := range counts {
		if got := countRecords(t, tenantDB, entityType); got != want {
			t.Fatalf("%s count changed after re-running seed-demo-data: had %d, now %d (not idempotent)", entityType, want, got)
		}
	}
}

func countRecords(t *testing.T, tenantDB *sql.DB, entityType string) int {
	t.Helper()
	var n int
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM records WHERE entity_type = $1 AND deleted_at IS NULL`, entityType,
	).Scan(&n); err != nil {
		t.Fatalf("count %s records: %v", entityType, err)
	}
	return n
}
