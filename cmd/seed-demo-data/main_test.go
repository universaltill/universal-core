// Smoke tests for the real compiled cmd/seed-demo-data binary.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
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
	"finance":    {finance.Publish, finance.PublishForms, nil},
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
		if seed.publishStatuses != nil {
			if err := seed.publishStatuses(ctx, tenantDB, actor); err != nil {
				t.Fatalf("%s PublishStatuses: %v", m, err)
			}
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
	controlDSN, _ := provisionedTenant(t, "purchasing", "sales", "finance")
	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with -tenant-id unset")
	}
	if !strings.Contains(stderr, "-tenant-id is required") {
		t.Fatalf("expected -tenant-id error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_MissingActorID_FailsFast(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance")
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
	// Accounts/PurchaseOrders/SalesOrders, so it must fail loudly (not
	// panic, not silently skip) when finance/purchasing/sales were never
	// published.
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
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance")

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
	for _, entityType := range []string{
		"Party", "Item", "PurchaseOrder", "SalesOrder", "CustomerInvoice",
		"Account", "FiscalYear", "Period", "TaxCode", "CostCenter",
	} {
		counts[entityType] = countRecords(t, tenantDB, entityType)
		if counts[entityType] == 0 {
			t.Fatalf("expected at least one %s record after seeding, got 0", entityType)
		}
	}

	// gl_accounts (the ledger core's own typed chart, ADR-0004) is a
	// separate table, not a generic `records` row — confirmed the
	// finance.SyncGLAccounts call wired into seed-demo-data actually ran,
	// not just that Account records exist.
	glAccountCount := countGLAccounts(t, tenantDB)
	if glAccountCount != counts["Account"] {
		t.Fatalf("expected gl_accounts to be synced 1:1 with Account records (%d), got %d", counts["Account"], glAccountCount)
	}

	// The GoodsReceiptLine-create and CustomerInvoice-issue hooks
	// (internal/kernel/purchasing/sales's ledger.go) are wired into this
	// binary's crud.Engine same as production — confirms sample data
	// actually posted, not just that the sample records exist.
	journalEntryCount := countJournalEntries(t, tenantDB)
	if journalEntryCount == 0 {
		t.Fatal("expected at least one journal entry posted by the GoodsReceiptLine/CustomerInvoice hooks")
	}

	// #29: a fresh seed gives PO-2026-0004 (a fully received PO) the
	// complete six-stage lead-time chain, values exactly as declared in
	// seedPurchaseOrders' table — this is #30's forecast demo data, so
	// the actual dates matter, not just non-emptiness.
	wantStages := map[string]string{
		"sourced_at":          "2026-07-19",
		"production_start_at": "2026-07-20",
		"production_ready_at": "2026-07-23",
		"shipped_at":          "2026-07-24",
		"customs_cleared_at":  "2026-07-26",
		"received_at":         "2026-07-27",
	}
	_, poData, poVersion := purchaseOrderByNumber(t, tenantDB, "PO-2026-0004")
	for name, want := range wantStages {
		if got, _ := poData[name].(string); got != want {
			t.Errorf("PO-2026-0004 stage %q: expected %q, got %q", name, want, got)
		}
	}
	// And a draft PO carries none — drafts/cancelled are the deliberate
	// no-stage rows in the seed table.
	_, draftData, _ := purchaseOrderByNumber(t, tenantDB, "PO-2026-0003")
	if v, ok := draftData["sourced_at"]; ok && v != "" {
		t.Errorf("PO-2026-0003 (draft) should have no stages, got sourced_at=%v", v)
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
	if got := countGLAccounts(t, tenantDB); got != glAccountCount {
		t.Fatalf("gl_accounts count changed after re-running seed-demo-data: had %d, now %d (not idempotent)", glAccountCount, got)
	}
	if got := countJournalEntries(t, tenantDB); got != journalEntryCount {
		t.Fatalf("journal entry count changed after re-running seed-demo-data: had %d, now %d (not idempotent — a hook may have double-posted)", journalEntryCount, got)
	}
	// #29's backfill must also be idempotent: a PO that already has
	// sourced_at is left alone entirely (no Update, so no version bump).
	_, poDataAfter, poVersionAfter := purchaseOrderByNumber(t, tenantDB, "PO-2026-0004")
	if poVersionAfter != poVersion {
		t.Fatalf("PO-2026-0004 version changed after re-run: had %d, now %d (stage backfill re-ran on a PO that already had stages)", poVersion, poVersionAfter)
	}
	for name, want := range wantStages {
		if got, _ := poDataAfter[name].(string); got != want {
			t.Errorf("PO-2026-0004 stage %q changed after re-run: expected %q, got %q", name, want, got)
		}
	}
}

// TestSeedDemoData_BackfillsStagesOnPreexistingPurchaseOrder covers
// seedPurchaseOrders' other #29 branch: a PO seeded before the stage
// fields existed (simulated by pre-creating PO-2026-0001 with no stage
// timestamps through the same crud engine the binary uses) keeps its
// identity — same record id, no duplicate row — and gains exactly the
// stage prefix the seed table declares for it, with its other data
// untouched. This is the "re-run the seeder against the live Demo
// Organization tenant" convention working on a real pre-#29 row.
func TestSeedDemoData_BackfillsStagesOnPreexistingPurchaseOrder(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance")
	control := testexec.Open(t, controlDSN)
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	ctx := context.Background()
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, def("Party"), map[string]any{
		"party_type": "organization", "name": "Pre-Stage Vendor Co", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "purchase_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list purchase_order_status StatusType: %v (n=%d)", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var draftID string
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			draftID = s.ID
		}
	}
	if draftID == "" {
		t.Fatal("no draft Status seeded for purchase_order_status")
	}
	pre, err := engine.Create(ctx, def("PurchaseOrder"), map[string]any{
		"po_number": "PO-2026-0001", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("pre-create PurchaseOrder: %v", err)
	}

	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("seed run: exit %d, stderr: %s", code, stderr)
	}

	gotID, gotData, _ := purchaseOrderByNumber(t, tenantDB, "PO-2026-0001")
	if gotID != pre.ID {
		t.Fatalf("expected the pre-existing PO-2026-0001 to be backfilled in place, got a different record (had %s, now %s)", pre.ID, gotID)
	}
	// PO-2026-0001 is an in-flight PO in the seed table: exactly the
	// first four stages, no customs_cleared_at/received_at.
	wantStages := map[string]string{
		"sourced_at":          "2026-07-04",
		"production_start_at": "2026-07-08",
		"production_ready_at": "2026-07-18",
		"shipped_at":          "2026-07-22",
	}
	for name, want := range wantStages {
		if got, _ := gotData[name].(string); got != want {
			t.Errorf("backfilled stage %q: expected %q, got %q", name, want, got)
		}
	}
	for _, absent := range []string{"customs_cleared_at", "received_at"} {
		if v, ok := gotData[absent]; ok && v != "" {
			t.Errorf("stage %q must not be backfilled onto an in-flight PO, got %v", absent, v)
		}
	}
	// The rest of the record rides along unchanged — the backfill copies
	// the stored data and only adds stages, it doesn't reseed the row.
	if got, _ := gotData["vendor_id"].(string); got != vendor.ID {
		t.Errorf("vendor_id changed during backfill: had %s, got %s", vendor.ID, got)
	}
	if got, _ := gotData["status_id"].(string); got != draftID {
		t.Errorf("status_id changed during backfill: had %s, got %s", draftID, got)
	}
}

// purchaseOrderByNumber fetches the single live PurchaseOrder row with
// the given po_number straight off the records table — and fails if
// there's more than one, which doubles as the "no duplicate was created"
// assertion everywhere it's called.
func purchaseOrderByNumber(t *testing.T, tenantDB *sql.DB, poNumber string) (recordID string, recordData map[string]any, version int) {
	t.Helper()
	rows, err := tenantDB.QueryContext(context.Background(),
		`SELECT id, data, version FROM records WHERE entity_type = 'PurchaseOrder' AND data->>'po_number' = $1 AND deleted_at IS NULL`, poNumber)
	if err != nil {
		t.Fatalf("query PurchaseOrder %s: %v", poNumber, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var raw []byte
		if err := rows.Scan(&recordID, &raw, &version); err != nil {
			t.Fatalf("scan PurchaseOrder %s: %v", poNumber, err)
		}
		if err := json.Unmarshal(raw, &recordData); err != nil {
			t.Fatalf("unmarshal PurchaseOrder %s data: %v", poNumber, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate PurchaseOrder %s: %v", poNumber, err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one PurchaseOrder with po_number %s, got %d", poNumber, n)
	}
	return recordID, recordData, version
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

func countGLAccounts(t *testing.T, tenantDB *sql.DB) int {
	t.Helper()
	var n int
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT count(*) FROM gl_accounts`).Scan(&n); err != nil {
		t.Fatalf("count gl_accounts: %v", err)
	}
	return n
}

func countJournalEntries(t *testing.T, tenantDB *sql.DB) int {
	t.Helper()
	var n int
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT count(*) FROM journal_entries`).Scan(&n); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	return n
}
