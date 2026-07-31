package data_test

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
)

// This package's own tests (unlike every other repo, which is only ever
// exercised indirectly through internal/kernel/crud or a module's own
// seed tests) talk to RecordRepo/ReportingRepo directly — the aggregate
// queries in reporting.go are entity-specific by design (see that file's
// own doc comment on why that's fine here, unlike the generic engines),
// so there's no kernel package positioned to test them instead.

// freshTenantDB returns a connection to a brand-new, uniquely-named
// database with the tenant migration set applied (ADR-0003) — the
// per-tenant database every test in this file now runs against, in
// place of the old single shared database + tenant_id column. Two calls
// in the same test give two physically separate databases, which is how
// the former "within tenant"/"ignores other tenants" tests below now
// prove isolation — a stronger proof than a WHERE clause, since there is
// no shared table for a bug to accidentally query across.
func freshTenantDB(t *testing.T) *sql.DB {
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

	name := fmt.Sprintf("uc_test_reporting_%d", time.Now().UnixNano())
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
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func TestPurchaseOrderStatusBreakdown_GroupsByStatusAndIsolatedPerTenantDatabase(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	recordsA := data.NewRecordRepo(dbA)
	recordsB := data.NewRecordRepo(dbB)
	reportingA := data.NewReportingRepo(dbA)

	mustCreateStatus := func(records *data.RecordRepo, code string) string {
		t.Helper()
		rec, err := records.Create(ctx, "Status", map[string]any{"code": code, "name": code})
		if err != nil {
			t.Fatalf("create Status: %v", err)
		}
		return rec.ID
	}
	draftA := mustCreateStatus(recordsA, "draft")
	approvedA := mustCreateStatus(recordsA, "approved")
	draftB := mustCreateStatus(recordsB, "draft")

	mustCreate := func(records *data.RecordRepo, fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, "PurchaseOrder", fields); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreate(recordsA, map[string]any{"po_number": "PO-A1", "status_id": draftA, "total": 100.0})
	mustCreate(recordsA, map[string]any{"po_number": "PO-A2", "status_id": draftA, "total": 50.0})
	mustCreate(recordsA, map[string]any{"po_number": "PO-A3", "status_id": approvedA, "total": 200.0})
	// A different tenant's order, in a genuinely different database, must
	// never contaminate tenantA's totals — even though "draft" resolves to
	// a different Status row per tenant and this order's own row id space
	// otherwise overlaps freely with tenantA's.
	mustCreate(recordsB, map[string]any{"po_number": "PO-B1", "status_id": draftB, "total": 999.0})
	// A status_id that isn't even a well-formed UUID (e.g. a bad CSV
	// import mapping) must be excluded, not abort the whole query — see
	// reporting.go's uuidPattern doc comment.
	mustCreate(recordsA, map[string]any{"po_number": "PO-A4", "status_id": "not-a-uuid", "total": 1234.0})

	rows, err := reportingA.PurchaseOrderStatusBreakdown(ctx)
	if err != nil {
		t.Fatalf("PurchaseOrderStatusBreakdown: %v", err)
	}
	byStatus := map[string]data.PurchaseOrderStatusCount{}
	for _, r := range rows {
		byStatus[r.Status] = r
	}
	if got := byStatus["draft"]; got.Count != 2 || got.Value != 150.0 {
		t.Errorf("draft = %+v, want Count=2 Value=150", got)
	}
	if got := byStatus["approved"]; got.Count != 1 || got.Value != 200.0 {
		t.Errorf("approved = %+v, want Count=1 Value=200", got)
	}
	if _, ok := byStatus["submitted"]; ok {
		t.Error("submitted should not appear — no orders in that status")
	}
}

func TestTopVendorsBySpend_RanksDescendingAndExcludesMalformedRefs(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	records := data.NewRecordRepo(dbA)
	reporting := data.NewReportingRepo(dbA)

	bigVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Big Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	smallVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Small Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	mustCreatePO := func(vendorID string, total float64) {
		t.Helper()
		if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": "PO-" + vendorID, "vendor_id": vendorID, "total": total,
		}); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreatePO(bigVendor.ID, 1000.0)
	mustCreatePO(bigVendor.ID, 500.0)
	mustCreatePO(smallVendor.ID, 10.0)
	// A vendor_id that isn't even a well-formed UUID (e.g. a bad CSV
	// import mapping) must be excluded, not abort the whole query — see
	// reporting.go's uuidPattern doc comment.
	mustCreatePO("not-a-uuid", 50000.0)
	// A well-formed but dangling vendor_id (no matching Party in this
	// database — deleted, or, under the old shared-DB design, belonging
	// to a different tenant) must not resolve either.
	mustCreatePO("00000000-0000-0000-0000-000000000000", 999.0)

	got, err := reporting.TopVendorsBySpend(ctx, 10)
	if err != nil {
		t.Fatalf("TopVendorsBySpend: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vendors, want 2 (malformed/dangling refs excluded): %+v", len(got), got)
	}
	if got[0].VendorName != "Big Vendor" || got[0].Total != 1500.0 || got[0].OrderCount != 2 {
		t.Errorf("rank 0 = %+v, want Big Vendor/1500/2", got[0])
	}
	if got[1].VendorName != "Small Vendor" || got[1].Total != 10.0 {
		t.Errorf("rank 1 = %+v, want Small Vendor/10", got[1])
	}
}

// TestCompletedPOLeadTimes_ReceivedAtWithGoodsReceiptFallback covers
// #30's query-time receipt derivation: a PO's own received_at wins when
// present, the EARLIEST GoodsReceipt.received_date fills in when it
// isn't, and a PO with neither (in-flight/draft) is excluded entirely.
// A PO whose vendor_id is malformed or dangling is still RETURNED —
// with an empty VendorID — because its lead time is real evidence for
// the overall distribution even when it can't be attributed to a
// vendor (see CompletedPOLeadTimes' own doc comment).
func TestCompletedPOLeadTimes_ReceivedAtWithGoodsReceiptFallback(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Lead Time Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	mustCreatePO := func(fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, "PurchaseOrder", fields)
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		return rec.ID
	}
	mustCreateGR := func(poID, receivedDate string) {
		t.Helper()
		if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
			"purchase_order_id": poID, "received_date": receivedDate,
		}); err != nil {
			t.Fatalf("create GoodsReceipt: %v", err)
		}
	}

	// PO with its own received_at AND a later GoodsReceipt — the stored
	// stage must win over the fallback.
	stamped := mustCreatePO(map[string]any{
		"po_number": "PO-STAMPED", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "sourced_at": "2026-07-03", "received_at": "2026-07-10",
	})
	mustCreateGR(stamped, "2026-07-12")

	// PO with NO received_at but two GoodsReceipts (partial deliveries) —
	// the EARLIEST received_date is the fallback receipt time.
	fallback := mustCreatePO(map[string]any{
		"po_number": "PO-FALLBACK", "vendor_id": vendor.ID, "order_date": "2026-07-05",
	})
	mustCreateGR(fallback, "2026-07-20")
	mustCreateGR(fallback, "2026-07-17")

	// In-flight PO: neither received_at nor any GoodsReceipt — excluded.
	mustCreatePO(map[string]any{
		"po_number": "PO-OPEN", "vendor_id": vendor.ID,
		"order_date": "2026-07-08", "sourced_at": "2026-07-09",
	})
	// Malformed vendor_id — still a completed PO, returned with an empty
	// VendorID (counts toward overall stats), not a query-aborting cast
	// error and not silently dropped.
	badVendor := mustCreatePO(map[string]any{
		"po_number": "PO-BADVENDOR", "vendor_id": "not-a-uuid",
		"order_date": "2026-07-02", "received_at": "2026-07-09",
	})
	// A GoodsReceipt whose purchase_order_id is malformed must neither
	// error the query nor attach anywhere.
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": "not-a-uuid", "received_date": "2026-07-01",
	}); err != nil {
		t.Fatalf("create malformed GoodsReceipt: %v", err)
	}

	got, err := reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		t.Fatalf("CompletedPOLeadTimes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d completed POs, want 3 (incl. the vendorless one): %+v", len(got), got)
	}
	// Ordered by order_date: PO-STAMPED (07-01), PO-BADVENDOR (07-02),
	// PO-FALLBACK (07-05).
	if got[0].POID != stamped || got[0].ReceivedDate != "2026-07-10" {
		t.Errorf("row 0 = %+v, want PO-STAMPED with its own received_at 2026-07-10", got[0])
	}
	if got[0].VendorID != vendor.ID || got[0].VendorName != "Lead Time Vendor" {
		t.Errorf("row 0 vendor = %s/%s, want the seeded vendor", got[0].VendorID, got[0].VendorName)
	}
	if got[0].OrderDate != "2026-07-01" || got[0].SourcedAt != "2026-07-03" {
		t.Errorf("row 0 dates = order %q sourced %q, want 2026-07-01/2026-07-03", got[0].OrderDate, got[0].SourcedAt)
	}
	if got[0].ShippedAt != "" {
		t.Errorf("row 0 shipped_at = %q, want empty for an unrecorded stage", got[0].ShippedAt)
	}
	if got[1].POID != badVendor || got[1].VendorID != "" || got[1].VendorName != "" {
		t.Errorf("row 1 = %+v, want PO-BADVENDOR with empty vendor id/name", got[1])
	}
	if got[1].ReceivedDate != "2026-07-09" {
		t.Errorf("row 1 received = %q, want 2026-07-09", got[1].ReceivedDate)
	}
	if got[2].POID != fallback || got[2].ReceivedDate != "2026-07-17" {
		t.Errorf("row 2 = %+v, want PO-FALLBACK with earliest GoodsReceipt date 2026-07-17", got[2])
	}
}

// TestOnOrderQtyByItem_SumsOpenStatusesOnly pins R3's on-order
// definition: submitted + approved POs count, drafts and terminal
// statuses (received, cancelled) don't, and quantities sum across lines
// and orders per item.
func TestOnOrderQtyByItem_SumsOpenStatusesOnly(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	statusIDs := map[string]string{}
	for _, code := range []string{"draft", "submitted", "approved", "received", "cancelled"} {
		rec, err := records.Create(ctx, "Status", map[string]any{"code": code, "name": code})
		if err != nil {
			t.Fatalf("create Status: %v", err)
		}
		statusIDs[code] = rec.ID
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OO", "name": "On Order Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	otherItem, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-NONE", "name": "Never Ordered", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreatePOWithLine := func(poNumber, status, itemID string, qty float64) {
		t.Helper()
		po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": poNumber, "order_date": "2026-07-01", "status_id": statusIDs[status],
		})
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		if _, err := records.Create(ctx, "POLine", map[string]any{
			"purchase_order_id": po.ID, "item_id": itemID, "qty": qty, "unit_price": 1.0,
		}); err != nil {
			t.Fatalf("create POLine: %v", err)
		}
	}
	mustCreatePOWithLine("PO-SUB", "submitted", item.ID, 100)
	mustCreatePOWithLine("PO-APP", "approved", item.ID, 40)
	mustCreatePOWithLine("PO-DRAFT", "draft", item.ID, 999)
	mustCreatePOWithLine("PO-RCVD", "received", item.ID, 999)
	mustCreatePOWithLine("PO-CANC", "cancelled", item.ID, 999)
	// Malformed refs are excluded, not fatal.
	if _, err := records.Create(ctx, "POLine", map[string]any{
		"purchase_order_id": "not-a-uuid", "item_id": item.ID, "qty": 999.0, "unit_price": 1.0,
	}); err != nil {
		t.Fatalf("create malformed POLine: %v", err)
	}

	got, err := reporting.OnOrderQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnOrderQtyByItem: %v", err)
	}
	if qty := got[item.ID]; qty != 140 {
		t.Errorf("on-order for item = %v, want 140 (submitted 100 + approved 40 only)", qty)
	}
	if _, ok := got[otherItem.ID]; ok {
		t.Errorf("item with no open PO must have no entry, got %v", got[otherItem.ID])
	}
}

// TestOnHandQtyByItem sums qty_on_hand per item across every
// InventoryItem row, skipping rows with no item_id.
func TestOnHandQtyByItem(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	itemA, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OH-A", "name": "On Hand A", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	itemB, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OH-B", "name": "On Hand B", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	mustCreateInv := func(fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, "InventoryItem", fields); err != nil {
			t.Fatalf("create InventoryItem: %v", err)
		}
	}
	// Two rows for itemA — sums, matching StockSummary's treatment of
	// multiple rows per item.
	mustCreateInv(map[string]any{"item_id": itemA.ID, "qty_on_hand": 30, "qty_available_to_promise": 30})
	mustCreateInv(map[string]any{"item_id": itemA.ID, "qty_on_hand": 12, "qty_available_to_promise": 12})
	mustCreateInv(map[string]any{"item_id": itemB.ID, "qty_on_hand": 7, "qty_available_to_promise": 7})
	// No item_id — skipped, not an empty-string map key.
	mustCreateInv(map[string]any{"qty_on_hand": 99, "qty_available_to_promise": 99})

	got, err := reporting.OnHandQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnHandQtyByItem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(got), got)
	}
	if got[itemA.ID] != 42 {
		t.Errorf("itemA on-hand = %v, want 42 (30+12)", got[itemA.ID])
	}
	if got[itemB.ID] != 7 {
		t.Errorf("itemB on-hand = %v, want 7", got[itemB.ID])
	}
}

// TestLatestPOVendorByItem picks the vendor of the most recent PO per
// item, regardless of status — see the method's own doc comment.
func TestLatestPOVendorByItem(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	oldVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Old Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	newVendor, err := records.Create(ctx, "Party", map[string]any{"name": "New Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-VND", "name": "Vendored Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreatePOWithLine := func(poNumber, vendorID, orderDate string) {
		t.Helper()
		po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": poNumber, "vendor_id": vendorID, "order_date": orderDate,
		})
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		if _, err := records.Create(ctx, "POLine", map[string]any{
			"purchase_order_id": po.ID, "item_id": item.ID, "qty": 1.0, "unit_price": 1.0,
		}); err != nil {
			t.Fatalf("create POLine: %v", err)
		}
	}
	mustCreatePOWithLine("PO-OLD", oldVendor.ID, "2026-06-01")
	mustCreatePOWithLine("PO-NEW", newVendor.ID, "2026-07-15")

	got, err := reporting.LatestPOVendorByItem(ctx)
	if err != nil {
		t.Fatalf("LatestPOVendorByItem: %v", err)
	}
	if got[item.ID] != newVendor.ID {
		t.Errorf("latest vendor = %s, want the 2026-07-15 order's vendor %s", got[item.ID], newVendor.ID)
	}
}

func TestStockSummaryAndStockoutRiskItems(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	healthy, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OK", "name": "Healthy Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	lowStock, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-LOW", "name": "Low Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	worseStock, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-WORSE", "name": "Worse Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreateInv := func(itemID string, onHand, atp float64) {
		t.Helper()
		if _, err := records.Create(ctx, "InventoryItem", map[string]any{
			"item_id": itemID, "qty_on_hand": onHand, "qty_available_to_promise": atp,
		}); err != nil {
			t.Fatalf("create InventoryItem: %v", err)
		}
	}
	mustCreateInv(healthy.ID, 100, 100)
	mustCreateInv(lowStock.ID, 10, 0)
	mustCreateInv(worseStock.ID, 5, -20)
	// A malformed item_id must be excluded from the stockout list, not
	// error the whole query.
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": "not-a-uuid", "qty_on_hand": 0, "qty_available_to_promise": -5,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	summary, err := reporting.StockSummary(ctx)
	if err != nil {
		t.Fatalf("StockSummary: %v", err)
	}
	if summary.ItemCount != 4 {
		t.Errorf("ItemCount = %d, want 4 (includes the malformed-ref row — it's still a real InventoryItem row)", summary.ItemCount)
	}
	if summary.TotalOnHand != 115 {
		t.Errorf("TotalOnHand = %v, want 115", summary.TotalOnHand)
	}
	if summary.StockoutCount != 3 {
		t.Errorf("StockoutCount = %d, want 3 (low, worse, and the malformed-ref row all have ATP <= 0)", summary.StockoutCount)
	}

	risk, err := reporting.StockoutRiskItems(ctx, 10)
	if err != nil {
		t.Fatalf("StockoutRiskItems: %v", err)
	}
	// Only 2 rows: the malformed item_id can't join to a real Item, so
	// it's excluded here even though StockSummary still counted it.
	if len(risk) != 2 {
		t.Fatalf("got %d stockout rows, want 2: %+v", len(risk), risk)
	}
	if risk[0].SKU != "SKU-WORSE" || risk[0].QtyATP != -20 {
		t.Errorf("rank 0 = %+v, want SKU-WORSE at -20 (worst first)", risk[0])
	}
	if risk[1].SKU != "SKU-LOW" || risk[1].QtyATP != 0 {
		t.Errorf("rank 1 = %+v, want SKU-LOW at 0", risk[1])
	}
}
