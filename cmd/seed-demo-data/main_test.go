// Smoke tests for the real compiled cmd/seed-demo-data binary.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/forecast"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
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
	"assets":     {assets.Publish, assets.PublishForms, assets.PublishStatuses},
	"projects":   {projects.Publish, projects.PublishForms, projects.PublishStatuses},
	"hr":         {hr.Publish, hr.PublishForms, hr.PublishStatuses},
	"crm":        {crm.Publish, crm.PublishForms, crm.PublishStatuses},
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
	controlDSN, _ := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with -tenant-id unset")
	}
	if !strings.Contains(stderr, "-tenant-id is required") {
		t.Fatalf("expected -tenant-id error, got stderr: %q", stderr)
	}
}

func TestSeedDemoData_MissingActorID_FailsFast(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
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
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")

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
		"Account", "FiscalYear", "Period", "TaxCode", "CostCenter", "ReorderRule",
		"FixedAsset", "DepreciationSchedule", "MaintenanceOrder",
		"Project", "Task", "TimeEntry",
		"Employee", "LeaveRequest", "AttendanceRecord",
		"Case", "Campaign", "Lead", "Opportunity",
		"Facility", "InventoryItem", "StockTransfer",
	} {
		counts[entityType] = countRecords(t, tenantDB, entityType)
		if counts[entityType] == 0 {
			t.Fatalf("expected at least one %s record after seeding, got 0", entityType)
		}
	}

	// The declared demo stock: 400+100 + 120 + 250+50 + 80 + 45 + 600 +
	// 25 + 200 + 0 + 60 (cmd/seed-demo-data's inventoryLevels). Pinned
	// as a figure rather than a row count because #12's re-keying made
	// "one row per item" false, and an independent review measured the
	// first version of that reconcile inflating this to 2280 on the
	// documented upgrade path while every row count still looked right.
	if got := totalOnHand(t, tenantDB); got != wantTotalOnHand {
		t.Fatalf("total qty_on_hand = %v, want %v", got, wantTotalOnHand)
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

	// #30: pin the SKU-1004 reorder signal's exact numbers against the
	// seeded state, through the same repo queries + forecast math the
	// report page composes (internal/api/reporting.go): exactly the
	// three received POs feed the lead-time stats (25, 9, and 11 days ->
	// overall P50 11, P90 = 11 + 0.8*(25-11) = 22.2; Acme's own pair
	// [9, 25] -> per-vendor P50 17, P90 23.4), SKU-1004's position is
	// its on-hand 80 alone (its only PO is received, so nothing is on
	// order), and 80 <= 150+50 fires the rule seedReorderRules declares
	// for it.
	if got := countRecords(t, tenantDB, "ReorderRule"); got != 3 {
		t.Fatalf("expected 3 seeded ReorderRules, got %d", got)
	}
	reporting := data.NewReportingRepo(tenantDB)
	ctx := context.Background()
	leadTimes, err := reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		t.Fatalf("CompletedPOLeadTimes: %v", err)
	}
	if len(leadTimes) != 3 {
		t.Fatalf("expected exactly the 3 received POs as completed lead-time rows, got %d: %+v", len(leadTimes), leadTimes)
	}
	samples := make([]forecast.LeadTimeSample, 0, len(leadTimes))
	for _, row := range leadTimes {
		ordered, err := time.Parse("2006-01-02", row.OrderDate)
		if err != nil {
			t.Fatalf("parse order date %q: %v", row.OrderDate, err)
		}
		receivedAt, err := time.Parse("2006-01-02", row.ReceivedDate)
		if err != nil {
			t.Fatalf("parse received date %q: %v", row.ReceivedDate, err)
		}
		samples = append(samples, forecast.LeadTimeSample{VendorID: row.VendorID, OrderDate: ordered, ReceivedDate: receivedAt})
	}
	stats := forecast.Compute(samples)
	if stats.Overall.N != 3 || !stats.Overall.Sufficient() {
		t.Fatalf("overall lead-time stats = %+v, want N=3 sufficient", stats.Overall)
	}
	if stats.Overall.P50Days != 11 || math.Abs(stats.Overall.P90Days-22.2) > 1e-9 {
		t.Fatalf("overall P50/P90 = %v/%v, want 11/22.2 (received_at wins over the +5d GoodsReceipt fallback)", stats.Overall.P50Days, stats.Overall.P90Days)
	}
	// Acme Textiles now has two completed orders (PO-2026-0001 25d,
	// PO-2026-0004 9d) -> a real per-vendor row on the live demo report:
	// P50 17, P90 = 9 + 0.9*(25-9) = 23.4.
	acmeID := partyIDByName(t, tenantDB, "Acme Textiles")
	acme, ok := stats.ByVendor[acmeID]
	if !ok || acme.N != 2 || !acme.Sufficient() {
		t.Fatalf("Acme Textiles lead-time stats = %+v (ok=%v), want N=2 sufficient", acme, ok)
	}
	if acme.P50Days != 17 || math.Abs(acme.P90Days-23.4) > 1e-9 {
		t.Fatalf("Acme P50/P90 = %v/%v, want 17/23.4", acme.P50Days, acme.P90Days)
	}
	onOrder, err := reporting.OnOrderQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnOrderQtyByItem: %v", err)
	}
	sku1004 := itemIDBySKU(t, tenantDB, "SKU-1004")
	if qty := onOrder[sku1004]; qty != 0 {
		t.Fatalf("SKU-1004 on-order = %v, want 0 (its only PO is received)", qty)
	}
	onHand := inventoryOnHand(t, tenantDB, sku1004)
	if onHand != 80 {
		t.Fatalf("SKU-1004 on-hand = %v, want 80", onHand)
	}
	rule := reorderRuleByItem(t, tenantDB, sku1004)
	point, _ := rule["reorder_point"].(float64)
	safety, _ := rule["safety_stock"].(float64)
	if point != 150 || safety != 50 {
		t.Fatalf("SKU-1004 rule = point %v safety %v, want 150/50", point, safety)
	}
	if conf, _ := rule["target_lead_time_confidence"].(string); conf != "p90" {
		t.Fatalf("SKU-1004 confidence = %q, want p90", conf)
	}
	if position := onHand + onOrder[sku1004]; position > point+safety {
		t.Fatalf("SKU-1004 position %v must fire against threshold %v", position, point+safety)
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
	// Counts alone would not catch seedInventory drifting the
	// QUANTITIES — a reconcile that mangled a figure while keeping the
	// row count stable reads as idempotent. The total is the number the
	// stock dashboard actually shows, so pin it.
	if got := totalOnHand(t, tenantDB); got != wantTotalOnHand {
		t.Fatalf("total qty_on_hand = %v after re-run, want %v — seedInventory must converge on its declared levels", got, wantTotalOnHand)
	}
	if got := countJournalEntries(t, tenantDB); got != journalEntryCount {
		t.Fatalf("journal entry count changed after re-running seed-demo-data: had %d, now %d (not idempotent — a hook may have double-posted)", journalEntryCount, got)
	}
	// The backfill must also be idempotent: a PO missing no stage is
	// left alone entirely (no Update, so no version bump) — PO-2026-0004
	// already carries all six.
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
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
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
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := engine.Create(ctx, def("PartyRole"), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("create vendor PartyRole: %v", err)
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
	// PO-2026-0001 is a fully received PO in the seed table (#30's
	// review flipped it so Acme Textiles has two completed orders):
	// the backfill adds its complete six-stage chain.
	wantStages := map[string]string{
		"sourced_at":          "2026-07-04",
		"production_start_at": "2026-07-08",
		"production_ready_at": "2026-07-18",
		"shipped_at":          "2026-07-22",
		"customs_cleared_at":  "2026-07-24",
		"received_at":         "2026-07-26",
	}
	for name, want := range wantStages {
		if got, _ := gotData[name].(string); got != want {
			t.Errorf("backfilled stage %q: expected %q, got %q", name, want, got)
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

// partyIDByName resolves a seeded Party's record id by its name.
func partyIDByName(t *testing.T, tenantDB *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT id FROM records WHERE entity_type = 'Party' AND data->>'name' = $1 AND deleted_at IS NULL`, name,
	).Scan(&id); err != nil {
		t.Fatalf("look up Party %s: %v", name, err)
	}
	return id
}

// itemIDBySKU resolves a seeded Item's record id by its SKU natural key.
func itemIDBySKU(t *testing.T, tenantDB *sql.DB, sku string) string {
	t.Helper()
	var id string
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT id FROM records WHERE entity_type = 'Item' AND data->>'sku' = $1 AND deleted_at IS NULL`, sku,
	).Scan(&id); err != nil {
		t.Fatalf("look up Item %s: %v", sku, err)
	}
	return id
}

// inventoryOnHand reads the single seeded InventoryItem row's
// qty_on_hand for an item.
// inventoryOnHand sums qty_on_hand across every live InventoryItem row
// for itemID — SUMS, deliberately, not a single-row lookup. An item can
// legitimately have more than one row (split across facilities per
// #12/ADR-0015, or — since uc-infra#54 — one baseline row plus one per
// GoodsReceiptLine credited against it, ledger.go's own doc comment on
// why that wiring inserts rather than upserts); a bare `QueryRowContext`
// over one row silently discards the rest and reports an arbitrary
// partial total, exactly like ReportingRepo.OnHandQtyByItem would NOT
// (independent review of this card: this helper used to be a single-row
// `QueryRowContext`, which masked SKU-1004's real receipt-credited total
// until reporting.go's own summing was used here too).
func inventoryOnHand(t *testing.T, tenantDB *sql.DB, itemID string) float64 {
	t.Helper()
	var qty float64
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT coalesce(sum((data->>'qty_on_hand')::numeric), 0) FROM records
		 WHERE entity_type = 'InventoryItem' AND data->>'item_id' = $1 AND deleted_at IS NULL`, itemID,
	).Scan(&qty); err != nil {
		t.Fatalf("look up InventoryItem for %s: %v", itemID, err)
	}
	return qty
}

// reorderRuleByItem fetches the single seeded ReorderRule for an item —
// and fails on duplicates, which doubles as seedReorderRules' own
// one-rule-per-item dedup assertion.
func reorderRuleByItem(t *testing.T, tenantDB *sql.DB, itemID string) map[string]any {
	t.Helper()
	rows, err := tenantDB.QueryContext(context.Background(),
		`SELECT data FROM records WHERE entity_type = 'ReorderRule' AND data->>'item_id' = $1 AND deleted_at IS NULL`, itemID)
	if err != nil {
		t.Fatalf("query ReorderRule for %s: %v", itemID, err)
	}
	defer rows.Close()
	var out map[string]any
	n := 0
	for rows.Next() {
		n++
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan ReorderRule: %v", err)
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal ReorderRule data: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ReorderRule rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one ReorderRule for item %s, got %d", itemID, n)
	}
	return out
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

// TestSeedDemoData_ExtendsPartialStagePrefix pins the live-tenant
// scenario the original all-or-nothing backfill guard missed (#30
// DevOps): a PO backfilled by an OLDER seed version holds a four-stage
// prefix (exactly what #29's reseed left on the live tenant), and the
// newer seed table extends that same PO to a full received chain. The
// converged backfill must fill ONLY the missing stages and never touch
// ones that already hold a value.
func TestSeedDemoData_ExtendsPartialStagePrefix(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
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
		"party_type": "organization", "name": "Partial Prefix Vendor Co", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := engine.Create(ctx, def("PartyRole"), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("create vendor PartyRole: %v", err)
	}
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "purchase_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list purchase_order_status StatusType: %v (n=%d)", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var approvedID string
	for _, st := range statuses {
		if code, _ := st.Data["code"].(string); code == "approved" {
			approvedID = st.ID
		}
	}
	if approvedID == "" {
		t.Fatal("no approved Status seeded for purchase_order_status")
	}
	pre, err := engine.Create(ctx, def("PurchaseOrder"), map[string]any{
		"po_number": "PO-2026-0001", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "status_id": approvedID,
		// Deliberately OFFSET from the seed table's values (which are
		// 07-04/07-08/07-18/07-22): if the backfill blindly overwrote
		// existing stages, byte-identical dates couldn't tell — the
		// review of this fix caught exactly that vacuous assertion.
		"sourced_at": "2026-07-03", "production_start_at": "2026-07-07",
		"production_ready_at": "2026-07-17", "shipped_at": "2026-07-21",
	}, actor)
	if err != nil {
		t.Fatalf("pre-create partially-staged PurchaseOrder: %v", err)
	}

	_, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("seed run: exit %d, stderr: %s", code, stderr)
	}

	gotID, gotData, _ := purchaseOrderByNumber(t, tenantDB, "PO-2026-0001")
	if gotID != pre.ID {
		t.Fatalf("expected in-place extension, got a different record (had %s, now %s)", pre.ID, gotID)
	}
	for field, want := range map[string]string{
		"sourced_at": "2026-07-03", "production_start_at": "2026-07-07",
		"production_ready_at": "2026-07-17", "shipped_at": "2026-07-21",
		"customs_cleared_at": "2026-07-24", "received_at": "2026-07-26",
	} {
		if got, _ := gotData[field].(string); got != want {
			t.Errorf("%s = %q, want %q (existing stages must survive untouched, missing ones filled from the seed table)", field, got, want)
		}
	}
	if got, _ := gotData["status_id"].(string); got != approvedID {
		t.Errorf("status_id changed during partial backfill: had %s, got %s", approvedID, got)
	}
}

// TestSeedDemoData_RepairsPartialDepreciationSchedule: an interrupted
// first run leaves an asset with only some of its schedule rows, and
// re-running must fill in the missing ones. The earlier draft keyed
// idempotency on "does this asset have ANY rows", which stranded a
// truncated schedule permanently — the independent review demonstrated
// it, and this repo had already set the opposite bar with
// TestSeedDemoData_ExtendsPartialStagePrefix.
func TestSeedDemoData_RepairsPartialDepreciationSchedule(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
	seed := func(label string) {
		t.Helper()
		if _, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test"); code != 0 {
			t.Fatalf("%s: exit %d: %s", label, code, stderr)
		}
	}
	seed("first seed")

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
	full := countRecords(t, tenantDB, "DepreciationSchedule")
	if full == 0 {
		t.Fatal("expected seeded schedule rows")
	}

	// Truncate every asset's schedule the way an interrupted run would.
	if _, err := tenantDB.Exec(
		`DELETE FROM records WHERE entity_type = 'DepreciationSchedule' AND (data->>'sequence')::numeric > 3`,
	); err != nil {
		t.Fatalf("truncate schedules: %v", err)
	}
	if partial := countRecords(t, tenantDB, "DepreciationSchedule"); partial >= full {
		t.Fatalf("expected a truncated schedule, got %d of %d", partial, full)
	}

	seed("repair seed")
	if repaired := countRecords(t, tenantDB, "DepreciationSchedule"); repaired != full {
		t.Errorf("re-run left the schedule short: %d rows, want %d", repaired, full)
	}
}

// TestSeedDemoData_CaseWarrantyContextIsCoherent pins what an earlier
// draft got wrong: the demo case's customer, product and order were
// picked by `range … break` over three maps, which Go randomises — so
// the cited order belonged to a different customer on every run and
// the item was never a line on it. That is precisely the unenforced
// gap crm.go documents, reproduced by the demo data, and it made the
// tenant non-reproducible between seeds.
func TestSeedDemoData_CaseWarrantyContextIsCoherent(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
	if _, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test"); code != 0 {
		t.Fatalf("seed: exit %d: %s", code, stderr)
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
	ctx := context.Background()

	var caseCustomer, caseItem, caseOrder string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'customer_id', data->>'item_id', data->>'sales_order_id'
		 FROM records WHERE entity_type = 'Case' AND data->>'case_number' = 'CASE-2026-001' AND deleted_at IS NULL`,
	).Scan(&caseCustomer, &caseItem, &caseOrder); err != nil {
		t.Fatalf("read seeded case: %v", err)
	}

	// The cited order must belong to the case's own customer.
	var orderCustomer string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'customer_id' FROM records WHERE id = $1::uuid AND entity_type = 'SalesOrder'`, caseOrder,
	).Scan(&orderCustomer); err != nil {
		t.Fatalf("read cited sales order: %v", err)
	}
	if orderCustomer != caseCustomer {
		t.Errorf("the case cites an order belonging to a different customer: case=%s order=%s", caseCustomer, orderCustomer)
	}

	// And the cited item must actually be a line on that order.
	var lines int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records
		 WHERE entity_type = 'SOLine' AND deleted_at IS NULL
		   AND data->>'sales_order_id' = $1 AND data->>'item_id' = $2`, caseOrder, caseItem,
	).Scan(&lines); err != nil {
		t.Fatalf("count matching order lines: %v", err)
	}
	if lines == 0 {
		t.Error("the case cites a product that was never a line on the order it cites")
	}
}

// The pipeline demo data must model ADR-0014's Contact shape and must
// hang together as one story, not three records that merely exist.
//
// Both halves have failed in this repo before. #15's review found
// seedCases citing an order that belonged to a different customer on
// every run, because it picked records by ranging over a map. And
// ADR-0013 clause 2 originally claimed HR seeding that had not been
// written — so a demo tenant asserted to model the intended shape is
// worth a test rather than a sentence.
func TestSeedDemoData_PipelineModelsTheContactShape(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
	if _, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test"); code != 0 {
		t.Fatalf("seed: exit %d: %s", code, stderr)
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
	ctx := context.Background()

	var leadID, companyName, convertedPartyID string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT id::text, data->>'company_name', data->>'converted_party_id'
		 FROM records WHERE entity_type = 'Lead' AND data->>'name' = 'Layla Hassan' AND deleted_at IS NULL`,
	).Scan(&leadID, &companyName, &convertedPartyID); err != nil {
		t.Fatalf("read the converted lead: %v", err)
	}
	if convertedPartyID == "" {
		t.Fatal("a lead in the `converted` state must record the Party it converted to")
	}

	// It converted to a PERSON, not an organization — the distinction
	// ADR-0014 rests on.
	var partyType string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'party_type' FROM records WHERE id = $1::uuid AND entity_type = 'Party'`, convertedPartyID,
	).Scan(&partyType); err != nil {
		t.Fatalf("read the converted Party: %v", err)
	}
	if partyType != "person" {
		t.Errorf("the converted Party is a %q; a Contact is a person Party (ADR-0014)", partyType)
	}

	// It holds the `contact` role.
	var roles int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE entity_type = 'PartyRole' AND deleted_at IS NULL
		   AND data->>'party_id' = $1 AND data->>'role_type' = 'contact'`, convertedPartyID,
	).Scan(&roles); err != nil {
		t.Fatalf("count contact PartyRoles: %v", err)
	}
	if roles != 1 {
		t.Errorf("the converted Party holds %d `contact` PartyRoles, want exactly 1", roles)
	}

	// The organization the lead named must exist as a Party, and the
	// relationship must run FROM the person TO that organization. The
	// direction is the assertion that matters: the kernel cannot
	// enforce it (contact_for runs person -> organization while its
	// neighbour `employs` runs the other way), so a reversed row would
	// look perfectly valid.
	var orgID string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT id::text FROM records WHERE entity_type = 'Party' AND data->>'name' = $1 AND deleted_at IS NULL`,
		companyName,
	).Scan(&orgID); err != nil {
		t.Fatalf("the lead's company_name %q must name a real Party: %v", companyName, err)
	}
	var rels int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE entity_type = 'PartyRelationship' AND deleted_at IS NULL
		   AND data->>'relationship_type' = 'contact_for'
		   AND data->>'party_id_from' = $1 AND data->>'party_id_to' = $2`, convertedPartyID, orgID,
	).Scan(&rels); err != nil {
		t.Fatalf("count contact_for relationships: %v", err)
	}
	if rels != 1 {
		t.Errorf("want exactly 1 contact_for relationship person(%s) -> organization(%s), got %d",
			convertedPartyID, orgID, rels)
	}

	// And the deal that lead produced is against that same customer —
	// not some other account that happened to be in the map.
	var oppCustomer string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT data->>'customer_id' FROM records WHERE entity_type = 'Opportunity'
		   AND data->>'lead_id' = $1 AND deleted_at IS NULL`, leadID,
	).Scan(&oppCustomer); err != nil {
		t.Fatalf("read the opportunity the lead produced: %v", err)
	}
	if oppCustomer != orgID {
		t.Errorf("the opportunity from lead %q is against a different customer than the lead's own company: opp=%s lead-company=%s",
			leadID, oppCustomer, orgID)
	}

	// A campaign-sourced lead must actually point at the campaign, and
	// its campaign must be one the seeder created.
	var campaignName string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT c.data->>'name' FROM records l
		   JOIN records c ON c.id::text = l.data->>'campaign_id' AND c.entity_type = 'Campaign'
		 WHERE l.entity_type = 'Lead' AND l.data->>'name' = 'Nadia Karim' AND l.deleted_at IS NULL`,
	).Scan(&campaignName); err != nil {
		t.Fatalf("the event-sourced lead must resolve to a real Campaign: %v", err)
	}
	if campaignName != "Gulf Expo 2026" {
		t.Errorf("event-sourced lead points at campaign %q, want the event campaign", campaignName)
	}
}

// wantTotalOnHand is the sum of cmd/seed-demo-data's inventoryLevels
// table: 400+100 (SKU-1001) + 120 + 250+50 (SKU-1003) + 80 + 45 + 600 +
// 25 + 200 + 0 + 60 = 1930, PLUS (uc-infra#54) every unit
// seedGoodsReceipts now actually receives against the three "received"
// PurchaseOrders — real physical arrivals that purchasing.
// PostGoodsReceiptLineToLedger's InventoryItem-crediting wiring credits
// for real, the same as it would for a live tenant: PO-2026-0001 (40,
// SKU-1002) + PO-2026-0004 (60 + 30, SKU-1005/1006) + PO-2026-0005
// (3000 + 5000, SKU-1004/1009) = 8130. 1930 + 8130 = 10060. Before this
// wiring existed, receiving was a no-op on stock, so this constant only
// ever needed the hand-declared baseline; now it is the baseline plus
// what the demo data genuinely receives, same as any other tenant's
// stock would be.
const wantTotalOnHand = 10060.0

func totalOnHand(t *testing.T, tenantDB *sql.DB) float64 {
	t.Helper()
	var total float64
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT coalesce(sum((data->>'qty_on_hand')::numeric), 0)
		 FROM records WHERE entity_type = 'InventoryItem' AND deleted_at IS NULL`,
	).Scan(&total); err != nil {
		t.Fatalf("sum qty_on_hand: %v", err)
	}
	return total
}

// TestSeedDemoData_ConvergesOnTheDocumentedUpgradePath is the
// regression test for the second blocker #12's independent review
// found: the seeder must CONVERGE on its declared inventory levels, not
// merely insert what is missing.
//
// ADR-0015 prescribes re-provision → backfill → re-seed for a tenant
// that predates the (item, facility) re-keying. This reproduces the
// state that path leaves behind and asserts the seeder repairs it:
//
//   - SKU-1001 carrying its OLD pre-split total (500) at MAIN, with no
//     STORE-01 row. An insert-only seeder sees MAIN covered, skips, and
//     adds STORE-01's 100 on top of a stale 500.
//   - SKU-1008 sitting at MAIN when the table now declares it at
//     STORE-01. An insert-only seeder creates the STORE-01 row beside
//     the orphan, doubling it.
//
// Measured against the insert-only version: total on-hand 2030 instead
// of 1930. Every row count still looked right, which is why the
// idempotency test alone could not catch it.
func TestSeedDemoData_ConvergesOnTheDocumentedUpgradePath(t *testing.T) {
	controlDSN, id := provisionedTenant(t, "purchasing", "sales", "finance", "assets", "projects", "hr", "crm")
	if _, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test"); code != 0 {
		t.Fatalf("first seed: exit %d: %s", code, stderr)
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
	ctx := context.Background()

	for _, sku := range []string{"SKU-1001", "SKU-1008"} {
		if _, err := tenantDB.ExecContext(ctx,
			`DELETE FROM records WHERE entity_type='InventoryItem'
			   AND data->>'item_id' = (SELECT id::text FROM records WHERE entity_type='Item' AND data->>'sku'=$1)`, sku); err != nil {
			t.Fatalf("clear %s inventory: %v", sku, err)
		}
	}
	for _, row := range []struct {
		sku string
		qty int
	}{
		{"SKU-1001", 500}, // the pre-split total, backfilled to MAIN
		{"SKU-1008", 200}, // at MAIN, but the table now declares STORE-01
	} {
		if _, err := tenantDB.ExecContext(ctx,
			`INSERT INTO records (entity_type, data) VALUES ('InventoryItem', jsonb_build_object(
			   'item_id', (SELECT id::text FROM records WHERE entity_type='Item' AND data->>'sku'=$1),
			   'facility_id', (SELECT id::text FROM records WHERE entity_type='Facility' AND data->>'code'='MAIN'),
			   'qty_on_hand', $2::numeric, 'qty_available_to_promise', $2::numeric))`, row.sku, row.qty); err != nil {
			t.Fatalf("insert legacy %s row: %v", row.sku, err)
		}
	}

	if _, stderr, code := run(t, []string{"DATABASE_URL=" + controlDSN}, "-tenant-id="+id, "-actor-id=smoke-test"); code != 0 {
		t.Fatalf("re-seed: exit %d: %s", code, stderr)
	}
	if got := totalOnHand(t, tenantDB); got != wantTotalOnHand {
		t.Errorf("total qty_on_hand = %v after the upgrade path ADR-0015 prescribes, want %v — the seeder must converge, not only insert", got, wantTotalOnHand)
	}
	// And SKU-1008's stock ends up where the table says it is, with no
	// orphan left at MAIN.
	var mainRows int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM records inv
		 WHERE inv.entity_type='InventoryItem' AND inv.deleted_at IS NULL
		   AND inv.data->>'item_id' = (SELECT id::text FROM records WHERE entity_type='Item' AND data->>'sku'='SKU-1008')
		   AND inv.data->>'facility_id' = (SELECT id::text FROM records WHERE entity_type='Facility' AND data->>'code'='MAIN')`,
	).Scan(&mainRows); err != nil {
		t.Fatalf("count SKU-1008 rows at MAIN: %v", err)
	}
	if mainRows != 0 {
		t.Errorf("SKU-1008 still has %d row(s) at MAIN — a row at an undeclared facility must be re-pointed, not left beside a new one", mainRows)
	}
}
