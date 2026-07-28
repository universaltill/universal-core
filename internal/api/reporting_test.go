package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
)

// The purchasing report reads straight off the records table
// (internal/data/reporting.go) — unlike every other page in this
// package, it never looks anything up in the Definition registry, so
// these tests seed raw records directly rather than going through
// publishEntityAndForm/the CRUD API first.

func TestAPI_PurchasingReport_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/reports/purchasing", nil) // no X-Tenant-ID/X-Actor-ID
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_PurchasingReport_TenantScopedAndEscapesRecordData is the one
// highest-stakes check for a page whose entire content is aggregated
// business data: tenant B's purchase order must never influence tenant
// A's report, and a vendor name containing raw HTML/script content
// (plausible — Party.name is free-text, reachable via CSV import) must
// render escaped, not executable.
func TestAPI_PurchasingReport_TenantScopedAndEscapesRecordData(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	_, dbB := newTestTenant(t, router)
	recordsA := data.NewRecordRepo(dbA)
	recordsB := data.NewRecordRepo(dbB)
	ctx := context.Background()

	statusA, err := recordsA.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status for tenant A: %v", err)
	}
	statusB, err := recordsB.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status for tenant B: %v", err)
	}

	vendorA, err := recordsA.Create(ctx, "Party", map[string]any{
		"name": `Acme" onmouseover="alert(1)<script>alert(2)</script>`, "party_type": "organization",
	})
	if err != nil {
		t.Fatalf("create Party for tenant A: %v", err)
	}
	if _, err := recordsA.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-A1", "vendor_id": vendorA.ID, "status_id": statusA.ID, "total": 1234.5,
	}); err != nil {
		t.Fatalf("create PurchaseOrder for tenant A: %v", err)
	}

	vendorB, err := recordsB.Create(ctx, "Party", map[string]any{"name": "Tenant B Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party for tenant B: %v", err)
	}
	if _, err := recordsB.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-B1", "vendor_id": vendorB.ID, "status_id": statusB.ID, "total": 999999.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder for tenant B: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/reports/purchasing", tenantA, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, "999999") || strings.Contains(body, "Tenant B Vendor") {
		t.Errorf("tenant B's data leaked into tenant A's report:\n%s", body)
	}
	if !strings.Contains(body, "1234.5") {
		t.Errorf("expected tenant A's own PurchaseOrder total (1234.5) in the report:\n%s", body)
	}
	if strings.Contains(body, "<script>alert(2)</script>") {
		t.Errorf("vendor name rendered as raw, unescaped HTML — XSS: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Errorf("expected the vendor name's script tag HTML-escaped in the output:\n%s", body)
	}
}

// TestAPI_PurchasingReport_StockoutRiskAndEmptyStates covers the stock
// side (a join through InventoryItem -> Item, unlike the vendor table's
// join through PurchaseOrder -> Party) and confirms both empty states
// (no vendors, no stockouts) render their own message instead of an
// empty table.
func TestAPI_PurchasingReport_StockoutRiskAndEmptyStates(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	records := data.NewRecordRepo(db)
	ctx := context.Background()

	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-EMPTY", "name": "Out of Stock Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": item.ID, "qty_on_hand": 0, "qty_available_to_promise": 0,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/reports/purchasing", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "SKU-EMPTY") || !strings.Contains(body, "Out of Stock Widget") {
		t.Errorf("expected the stockout-risk item in the report:\n%s", body)
	}
	if !strings.Contains(body, "No purchase orders yet.") {
		t.Errorf("expected the vendor-empty-state message (no POs seeded for this tenant):\n%s", body)
	}
}
