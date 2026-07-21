package data_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
)

// This package's own tests (unlike every other repo, which is only ever
// exercised indirectly through internal/kernel/crud or a module's own
// seed tests) talk to RecordRepo/ReportingRepo directly — the aggregate
// queries in reporting.go are entity-specific by design (see that file's
// own doc comment on why that's fine here, unlike the generic engines),
// so there's no kernel package positioned to test them instead.

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

func seedTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
		"Test Tenant", "eu-west",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func TestPurchaseOrderStatusBreakdown_GroupsByStatusWithinTenant(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	reporting := data.NewReportingRepo(db)

	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)

	mustCreate := func(tenantID string, fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, tenantID, "PurchaseOrder", fields); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreate(tenantA, map[string]any{"po_number": "PO-A1", "status": "draft", "total": 100.0})
	mustCreate(tenantA, map[string]any{"po_number": "PO-A2", "status": "draft", "total": 50.0})
	mustCreate(tenantA, map[string]any{"po_number": "PO-A3", "status": "approved", "total": 200.0})
	// A different tenant's order must never contaminate tenantA's totals.
	mustCreate(tenantB, map[string]any{"po_number": "PO-B1", "status": "draft", "total": 999.0})

	rows, err := reporting.PurchaseOrderStatusBreakdown(ctx, tenantA)
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

func TestTopVendorsBySpend_RanksDescendingAndIgnoresOtherTenantsAndMalformedRefs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	reporting := data.NewReportingRepo(db)

	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)

	bigVendor, err := records.Create(ctx, tenantA, "Party", map[string]any{"name": "Big Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	smallVendor, err := records.Create(ctx, tenantA, "Party", map[string]any{"name": "Small Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	otherTenantVendor, err := records.Create(ctx, tenantB, "Party", map[string]any{"name": "Other Tenant Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	mustCreatePO := func(tenantID, vendorID string, total float64) {
		t.Helper()
		if _, err := records.Create(ctx, tenantID, "PurchaseOrder", map[string]any{
			"po_number": "PO-" + vendorID, "vendor_id": vendorID, "total": total,
		}); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreatePO(tenantA, bigVendor.ID, 1000.0)
	mustCreatePO(tenantA, bigVendor.ID, 500.0)
	mustCreatePO(tenantA, smallVendor.ID, 10.0)
	// A vendor_id that isn't even a well-formed UUID (e.g. a bad CSV
	// import mapping) must be excluded, not abort the whole query — see
	// reporting.go's uuidPattern doc comment.
	mustCreatePO(tenantA, "not-a-uuid", 50000.0)
	// A vendor_id pointing at a real Party row, but one belonging to a
	// different tenant, must not resolve either (the join is tenant-
	// scoped on both sides).
	mustCreatePO(tenantA, otherTenantVendor.ID, 999.0)

	got, err := reporting.TopVendorsBySpend(ctx, tenantA, 10)
	if err != nil {
		t.Fatalf("TopVendorsBySpend: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vendors, want 2 (malformed/cross-tenant refs excluded): %+v", len(got), got)
	}
	if got[0].VendorName != "Big Vendor" || got[0].Total != 1500.0 || got[0].OrderCount != 2 {
		t.Errorf("rank 0 = %+v, want Big Vendor/1500/2", got[0])
	}
	if got[1].VendorName != "Small Vendor" || got[1].Total != 10.0 {
		t.Errorf("rank 1 = %+v, want Small Vendor/10", got[1])
	}
}

func TestStockSummaryAndStockoutRiskItems(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	reporting := data.NewReportingRepo(db)

	tenantA := seedTenant(t, db)

	healthy, err := records.Create(ctx, tenantA, "Item", map[string]any{"sku": "SKU-OK", "name": "Healthy Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	lowStock, err := records.Create(ctx, tenantA, "Item", map[string]any{"sku": "SKU-LOW", "name": "Low Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	worseStock, err := records.Create(ctx, tenantA, "Item", map[string]any{"sku": "SKU-WORSE", "name": "Worse Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreateInv := func(itemID string, onHand, atp float64) {
		t.Helper()
		if _, err := records.Create(ctx, tenantA, "InventoryItem", map[string]any{
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
	if _, err := records.Create(ctx, tenantA, "InventoryItem", map[string]any{
		"item_id": "not-a-uuid", "qty_on_hand": 0, "qty_available_to_promise": -5,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	summary, err := reporting.StockSummary(ctx, tenantA)
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

	risk, err := reporting.StockoutRiskItems(ctx, tenantA, 10)
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
