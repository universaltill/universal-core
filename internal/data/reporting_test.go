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
