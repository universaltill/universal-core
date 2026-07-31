package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
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
	// #30's no-data states: no completed POs -> lead-time empty state; no
	// ReorderRule Definition even published for this tenant (purchasing
	// module absent) -> the reorder section degrades to its empty state
	// rather than erroring the whole report.
	if !strings.Contains(body, "No completed purchase orders yet.") {
		t.Errorf("expected the lead-time empty state:\n%s", body)
	}
	if !strings.Contains(body, "No reorder signals right now.") {
		t.Errorf("expected the reorder-signal empty state:\n%s", body)
	}
}

// TestAPI_PurchasingReport_LeadTimeAndReorderSections (#30) drives both
// new sections end to end against a hand-computed fixture: two
// completed POs for one vendor (9 and 11 days -> per-vendor P50 10,
// P90 10.8 by exact type-7 interpolation), one of them proving the
// GoodsReceipt-fallback receipt time, a second vendor with a single
// 20-day completed PO (insufficient on its own; overall becomes
// [9, 11, 20] -> P50 11, P90 18.2), an open PO whose lines feed the
// on-order quantities, and three ReorderRules — one firing with its
// vendor's own lead-time context, one whose vendor has insufficient
// history and must show the DISTINCT "(all suppliers)" overall string
// (never the plain per-vendor one), and one that must NOT fire despite
// low on-hand because a big open PO holds its position up (BA
// acceptance criterion 4, the position-math regression case).
func TestAPI_PurchasingReport_LeadTimeAndReorderSections(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	// buildReorderSignals reads ReorderRule/InventoryItem/Item through
	// the guarded engine, which needs their published Definitions — the
	// real purchasing module publish, same as provision-tenant runs.
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	records := data.NewRecordRepo(db)

	mustCreate := func(entityType string, fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, entityType, fields)
		if err != nil {
			t.Fatalf("create %s: %v", entityType, err)
		}
		return rec.ID
	}

	approved := mustCreate("Status", map[string]any{"code": "approved", "name": "Approved"})
	received := mustCreate("Status", map[string]any{"code": "received", "name": "Received"})
	vendor := mustCreate("Party", map[string]any{"name": "Acme LT", "party_type": "organization"})
	soloVendor := mustCreate("Party", map[string]any{"name": "Solo Vendor", "party_type": "organization"})

	widgetA := mustCreate("Item", map[string]any{"sku": "SKU-A", "name": "Widget A", "item_type": "stock"})
	widgetB := mustCreate("Item", map[string]any{"sku": "SKU-B", "name": "Widget B", "item_type": "stock"})
	widgetC := mustCreate("Item", map[string]any{"sku": "SKU-C", "name": "Widget C", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": widgetA, "qty_on_hand": 5, "qty_available_to_promise": 5})
	mustCreate("InventoryItem", map[string]any{"item_id": widgetB, "qty_on_hand": 5, "qty_available_to_promise": 5})
	mustCreate("InventoryItem", map[string]any{"item_id": widgetC, "qty_on_hand": 2, "qty_available_to_promise": 2})

	// Completed PO #1: 9 days via its own received_at stage.
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-LT-1", "vendor_id": vendor, "status_id": received,
		"order_date": "2026-07-01", "received_at": "2026-07-10",
	})
	// Completed PO #2: 11 days via the earliest GoodsReceipt fallback —
	// no received_at on the PO itself.
	poLT2 := mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-LT-2", "vendor_id": vendor, "status_id": received,
		"order_date": "2026-07-05",
	})
	mustCreate("GoodsReceipt", map[string]any{"purchase_order_id": poLT2, "received_date": "2026-07-16"})

	// Solo Vendor's single completed PO (20 days) — insufficient history
	// for a per-vendor quantile, but real evidence in the overall
	// distribution; its POLine also makes Solo Vendor Widget C's
	// most-recent-PO vendor.
	poSolo := mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-SOLO-1", "vendor_id": soloVendor, "status_id": received,
		"order_date": "2026-07-02", "received_at": "2026-07-22",
	})
	mustCreate("POLine", map[string]any{"purchase_order_id": poSolo, "item_id": widgetC, "qty": 10, "unit_price": 2.0})

	// Open (approved) PO: 20 of Widget A and 100 of Widget B on order.
	// Also the most recent PO touching both items -> their signal vendor.
	openPO := mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OPEN-1", "vendor_id": vendor, "status_id": approved,
		"order_date": "2026-07-20", "total": 0,
	})
	mustCreate("POLine", map[string]any{"purchase_order_id": openPO, "item_id": widgetA, "qty": 20, "unit_price": 1.0})
	mustCreate("POLine", map[string]any{"purchase_order_id": openPO, "item_id": widgetB, "qty": 100, "unit_price": 1.0})

	// Widget A: position 5 + 20 = 25 <= 30 + 0 -> fires, P90 context.
	mustCreate("ReorderRule", map[string]any{
		"item_id": widgetA, "reorder_point": 30, "safety_stock": 0, "target_lead_time_confidence": "p90",
	})
	// Widget B: on-hand 5 is way below the same reorder point, but
	// position 5 + 100 = 105 > 30 -> must NOT fire (BA acceptance #4).
	mustCreate("ReorderRule", map[string]any{
		"item_id": widgetB, "reorder_point": 30, "safety_stock": 0, "target_lead_time_confidence": "p90",
	})
	// Widget C: position 2 + 0 = 2 <= 10 -> fires; its vendor (Solo, n=1)
	// is insufficient, so the context must be the disclosed overall
	// string, never the plain per-vendor one.
	mustCreate("ReorderRule", map[string]any{
		"item_id": widgetC, "reorder_point": 10, "safety_stock": 0, "target_lead_time_confidence": "p90",
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Supplier Lead Times") || !strings.Contains(body, "Reorder Signals") {
		t.Fatalf("expected both #30 section headings:\n%s", body)
	}
	// Exact vendor row: n=2, P50 = 10, P90 = 9 + 0.9*(11-9) = 10.8.
	if !strings.Contains(body, "<tr><td>Acme LT</td><td>2</td><td>10</td><td>10.8</td></tr>") {
		t.Errorf("expected the Acme LT lead-time row with P50 10 / P90 10.8:\n%s", body)
	}
	// Solo Vendor's single sample: n=1, insufficient in both quantile
	// cells — never a fabricated number.
	if !strings.Contains(body, "<tr><td>Solo Vendor</td><td>1</td><td>Insufficient history</td><td>Insufficient history</td></tr>") {
		t.Errorf("expected the Solo Vendor row to say insufficient at n=1:\n%s", body)
	}
	// Overall row across all three samples [9, 11, 20]: P50 11,
	// P90 = 11 + 0.8*(20-11) = 18.2.
	if !strings.Contains(body, "<tr><td>All vendors</td><td>3</td><td>11</td><td>18.2</td></tr>") {
		t.Errorf("expected the all-vendors summary row with P50 11 / P90 18.2:\n%s", body)
	}
	// Exact firing signal row for Widget A: on-hand 5, on-order 20,
	// position 25, reorder point 30, the PER-VENDOR P90 context (Acme LT
	// has sufficient history of its own — no "(all suppliers)" suffix).
	wantSignal := "<tr><td><a href=\"/forms/Item/" + widgetA + "\">Widget A</a></td><td>5</td><td>20</td><td>25</td><td>30</td><td>Order now — expect ~10.8 days</td></tr>"
	if !strings.Contains(body, wantSignal) {
		t.Errorf("expected the Widget A reorder signal row %q:\n%s", wantSignal, body)
	}
	if strings.Contains(body, "Widget B") {
		t.Errorf("Widget B must not fire — position (105) is held up by its open PO even though on-hand (5) is below the reorder point:\n%s", body)
	}
	// Widget C's vendor has insufficient history -> overall stats via the
	// DISTINCT disclosed string (overall P90 18.2), never the plain
	// per-vendor wording.
	wantOverallSignal := "<tr><td><a href=\"/forms/Item/" + widgetC + "\">Widget C</a></td><td>2</td><td>0</td><td>2</td><td>10</td><td>Order now — expect ~18.2 days (all suppliers)</td></tr>"
	if !strings.Contains(body, wantOverallSignal) {
		t.Errorf("expected the Widget C signal row with the disclosed all-suppliers context %q:\n%s", wantOverallSignal, body)
	}
	if strings.Contains(body, "expect ~18.2 days</td>") {
		t.Errorf("overall-derived context must always carry the (all suppliers) disclosure:\n%s", body)
	}

	// Localized render (the en assertions above would pass even if the
	// catalog were bypassed, since Go fallbacks match the en strings —
	// same tautology the #29 review flagged): the Turkish page must carry
	// the tr headings and signal context.
	recTR := getAs(t, mux, "/reports/purchasing?lang=tr", tenantID, "farshid")
	if recTR.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?lang=tr, got %d: %s", recTR.Code, recTR.Body.String())
	}
	bodyTR := recTR.Body.String()
	for _, want := range []string{
		"Tedarikçi Tedarik Süreleri",
		"Yeniden Sipariş Sinyalleri",
		"Şimdi sipariş verin — yaklaşık 10.8 gün içinde gelir",
		"Şimdi sipariş verin — yaklaşık 18.2 gün içinde gelir (tüm tedarikçiler)",
	} {
		if !strings.Contains(bodyTR, want) {
			t.Errorf("expected localized string %q on the tr report:\n%s", want, bodyTR)
		}
	}
}

// TestAPI_PurchasingReport_InsufficientHistoryStates (#30, BA R1's
// minimum-sample rule at the surface): with a single completed PO the
// vendor and overall rows must say "insufficient" rather than fabricate
// a quantile from one observation, and a firing reorder signal must
// carry the insufficient-lead-time text instead of an expected-days
// number.
func TestAPI_PurchasingReport_InsufficientHistoryStates(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	records := data.NewRecordRepo(db)

	mustCreate := func(entityType string, fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, entityType, fields)
		if err != nil {
			t.Fatalf("create %s: %v", entityType, err)
		}
		return rec.ID
	}

	vendor := mustCreate("Party", map[string]any{"name": "Lone Order Vendor", "party_type": "organization"})
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-LONE-1", "vendor_id": vendor,
		"order_date": "2026-07-01", "received_at": "2026-07-09",
	})
	item := mustCreate("Item", map[string]any{"sku": "SKU-LOW", "name": "Low Widget", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": item, "qty_on_hand": 2, "qty_available_to_promise": 2})
	mustCreate("ReorderRule", map[string]any{
		"item_id": item, "reorder_point": 10, "safety_stock": 5, "target_lead_time_confidence": "p90",
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// One sample: the vendor row and overall row both show N=1 and the
	// insufficient text in both quantile cells.
	if !strings.Contains(body, "<tr><td>Lone Order Vendor</td><td>1</td><td>Insufficient history</td><td>Insufficient history</td></tr>") {
		t.Errorf("expected the single-sample vendor row to say insufficient, not a fabricated quantile:\n%s", body)
	}
	// The signal fires (position 2 <= 10+5) but must show the
	// insufficient-lead-time context.
	if !strings.Contains(body, "Low Widget") {
		t.Errorf("expected the Low Widget signal to fire:\n%s", body)
	}
	if !strings.Contains(body, "Insufficient lead-time history") {
		t.Errorf("expected the insufficient-lead-time context on the signal row:\n%s", body)
	}
	if strings.Contains(body, "expect ~") {
		t.Errorf("no expected-days figure may be shown with one lead-time sample:\n%s", body)
	}
}

// TestAPI_PurchasingReport_EntityTypeListMatchesTheActualQueries pins
// purchasingReportEntityTypes to a hardcoded, independently-derived set
// so a change to the var — an addition OR an omission — fails loudly
// here instead of silently. This is NOT redundant with the table-driven
// test below: that test only proves each LISTED type is enforced: it
// iterates the var itself, so an unlisted type (independent review,
// 2026-07-30: "Status" was missing from the first draft, joined by
// PurchaseOrderStatusBreakdown but never checked) produces no subtest at
// all and would pass silently forever. This test is what actually
// catches that class of drift.
func TestAPI_PurchasingReport_EntityTypeListMatchesTheActualQueries(t *testing.T) {
	// Independently re-derived from internal/data/reporting.go's SQL plus
	// the guarded-engine reads in reporting.go's buildReorderSignals, not
	// copied from purchasingReportEntityTypes: PurchaseOrderStatusBreakdown
	// reads PurchaseOrder and joins Status; TopVendorsBySpend reads
	// PurchaseOrder and joins Party; StockSummary reads InventoryItem;
	// StockoutRiskItems reads InventoryItem and joins Item;
	// CompletedPOLeadTimes (#30) reads PurchaseOrder and joins Party and
	// GoodsReceipt; OnOrderQtyByItem reads POLine joined to PurchaseOrder
	// and Status; OnHandQtyByItem reads InventoryItem;
	// LatestPOVendorByItem reads POLine joined to PurchaseOrder;
	// buildReorderSignals reads ReorderRule and Item through the guarded
	// engine.
	want := map[string]bool{
		"PurchaseOrder": true, "Status": true, "Party": true, "InventoryItem": true, "Item": true,
		"POLine": true, "GoodsReceipt": true, "ReorderRule": true,
	}
	got := make(map[string]bool, len(purchasingReportEntityTypes))
	for _, et := range purchasingReportEntityTypes {
		got[et] = true
	}
	if len(got) != len(purchasingReportEntityTypes) {
		t.Fatalf("purchasingReportEntityTypes has a duplicate entry: %v", purchasingReportEntityTypes)
	}
	for et := range want {
		if !got[et] {
			t.Errorf("purchasingReportEntityTypes is missing %q, which the SQL reads from", et)
		}
	}
	for et := range got {
		if !want[et] {
			t.Errorf("purchasingReportEntityTypes names %q, which no query in internal/data/reporting.go reads from — either the SQL changed and this test's `want` set needs updating, or the entry is stale", et)
		}
	}
}

// TestAPI_PurchasingReport_DeniedWhenAnyUnderlyingTypeIsRestricted is the
// ADR-0006 addendum's regression test: a role denied read on any ONE of
// the entity types the report aggregates must be refused the whole page,
// not served a partial or full report. Table-driven over
// purchasingReportEntityTypes, so it only proves each LISTED type is
// enforced — see TestAPI_PurchasingReport_EntityTypeListMatchesTheActual
// Queries above for the check that catches an unlisted type.
func TestAPI_PurchasingReport_DeniedWhenAnyUnderlyingTypeIsRestricted(t *testing.T) {
	for _, restricted := range purchasingReportEntityTypes {
		t.Run(restricted, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)

			seedRBAC(t, db,
				map[string][]string{"outsider": {"user-outsider"}},
				[]map[string]any{{"role": "outsider", "entity_type": restricted, "can_read": false, "can_write": false}},
			)

			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)
			rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-outsider")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("role denied read on %s: expected 403, got %d: %s", restricted, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Access denied") {
				t.Fatalf("not the rendered denial page:\n%s", rec.Body.String())
			}
		})
	}
}

// TestAPI_PurchasingReport_DeniedOnUnrelatedTypeStillSeesReport is the
// negative control the table-driven denial test above can't provide by
// itself: it proves the gate is scoped to purchasingReportEntityTypes,
// not "any Permission row anywhere makes this report 403." A role denied
// read on an entity type the report never touches (Employee — no query
// in internal/data/reporting.go reads it) must still see the real
// report. Without this, an over-broad gate (deny-on-anything-denies-the-
// report) would pass every other test in this file.
func TestAPI_PurchasingReport_DeniedOnUnrelatedTypeStillSeesReport(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	records := data.NewRecordRepo(db)

	st, err := records.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Demo Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-UNRELATED", "vendor_id": vendor.ID, "status_id": st.ID, "total": 314.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"outsider": {"user-outsider"}},
		[]map[string]any{{"role": "outsider", "entity_type": "Employee", "can_read": false, "can_write": false}},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-outsider")
	if rec.Code != http.StatusOK {
		t.Fatalf("role denied on an unrelated type (Employee): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "314") {
		t.Fatalf("expected the seeded PurchaseOrder total in the report:\n%s", rec.Body.String())
	}
}

// TestAPI_PurchasingReport_AdminAndUnrestrictedRolesStillSeeIt covers the
// two ways this gate must NOT trigger: tenant_admin always passes
// (ADR-0006's lockout-prevention convention), and a role granted read on
// every underlying type sees the real report, not just a 403 avoided.
func TestAPI_PurchasingReport_AdminAndUnrestrictedRolesStillSeeIt(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()

	statusRow, err := data.NewRecordRepo(db).Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := data.NewRecordRepo(db).Create(ctx, "Party", map[string]any{"name": "Acme", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := data.NewRecordRepo(db).Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "status_id": statusRow.ID, "total": 42.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	perms := make([]map[string]any, len(purchasingReportEntityTypes))
	for i, et := range purchasingReportEntityTypes {
		perms[i] = map[string]any{"role": "reader", "entity_type": et, "can_read": true, "can_write": false}
	}
	seedRBAC(t, db,
		map[string][]string{"reader": {"user-reader"}, authz.AdminRoleCode: {"user-admin"}},
		perms,
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	for _, actor := range []string{"user-reader", "user-admin"} {
		rec := getAs(t, mux, "/reports/purchasing", tenantID, actor)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", actor, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "42") {
			t.Fatalf("%s: expected the seeded PurchaseOrder total in the report:\n%s", actor, rec.Body.String())
		}
	}
}

// TestAPI_PurchasingReport_StatusDeniedButOthersGrantedStillBlocksReport
// is the exact regression independent review found live: a role granted
// read on PurchaseOrder/Party/InventoryItem/Item but explicitly denied
// on Status must still be refused the whole report, not served the
// status breakdown with everything else intact.
// PurchaseOrderStatusBreakdown (internal/data/reporting.go) joins
// PurchaseOrder to Status to read the human-readable status code — a
// role that cannot read Status cannot legitimately see that join's
// output, even though Status is not the type the URL/report is "about."
// Distinct from the table-driven denial test above (which denies one
// type at a time, proving the OTHER types don't accidentally deny too)
// — this proves the specific leak: granting every type the report is
// nominally about while denying the join target it also silently reads.
func TestAPI_PurchasingReport_StatusDeniedButOthersGrantedStillBlocksReport(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	records := data.NewRecordRepo(db)

	statusRow, err := records.Create(ctx, "Status", map[string]any{"code": "approved", "name": "Approved"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Demo Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-STATUS-LEAK", "vendor_id": vendor.ID, "status_id": statusRow.ID, "total": 7777.0,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"probe": {"user-probe"}},
		[]map[string]any{
			{"role": "probe", "entity_type": "Status", "can_read": false, "can_write": false},
			{"role": "probe", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "Party", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "InventoryItem", "can_read": true, "can_write": false},
			{"role": "probe", "entity_type": "Item", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-probe")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("role denied Status read (everything else granted): expected 403, got %d — the report leaked status-joined data despite the denial:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Access denied") {
		t.Fatalf("not the rendered denial page:\n%s", rec.Body.String())
	}
}

// purchaseOrderModuleEntityDef/FormDef exist only so accessibleModules
// has something to group under the "purchasing" module key — the report
// link itself needs no Definition (ReportingRepo reads raw records), but
// reaching /modules/purchasing at all requires at least one published
// entity+form declaring Module "purchasing" (see accessibleModules' own
// doc comment). Deliberately minimal; not the real purchasing.PurchaseOrder
// Definition, which this package's tests never import directly.
func purchaseOrderModuleEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Module:     "purchasing",
		Fields:     []entity.Field{{Name: "po_number", Type: entity.FieldString, Required: true}},
	}
}

func purchaseOrderModuleFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields:    []form.FormField{{Name: "po_number", Label: "PO Number"}},
		}},
	}
}

// TestAPI_PurchasingReport_ModuleMenuHidesReportLinkWhenDenied is the
// regression test for the gap independent review found: /modules/
// purchasing offered the report link unconditionally, so a user denied
// read on any purchasingReportEntityTypes entry saw a link that led
// straight to a guaranteed 403 — precisely the "tile that leads to a
// 403" outcome dashboard.go's own CanRead filtering exists to prevent
// for entity nodes, just not previously applied to report links.
func TestAPI_PurchasingReport_ModuleMenuHidesReportLinkWhenDenied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, purchaseOrderModuleEntityDef(), purchaseOrderModuleFormDef())

	var grantedPerms, deniedPerms []map[string]any
	for _, et := range purchasingReportEntityTypes {
		grantedPerms = append(grantedPerms, map[string]any{"role": "reader", "entity_type": et, "can_read": true, "can_write": false})
		// Every type granted EXCEPT "Item" — deliberately NOT PurchaseOrder,
		// which is the only type published under the "purchasing" module
		// here: denying it would also hide the module's own hub node
		// (accessibleModules' CanRead filtering, dashboard.go), collapsing
		// the module to a 404 and proving nothing about the report link
		// specifically. Denying an entity type the report needs but the
		// module page doesn't isolate the exact thing this test targets.
		deniedPerms = append(deniedPerms, map[string]any{"role": "denied", "entity_type": et, "can_read": et != "Item", "can_write": false})
	}
	seedRBAC(t, db,
		map[string][]string{"reader": {"user-reader"}, "denied": {"user-denied"}},
		append(grantedPerms, deniedPerms...),
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	granted := getAs(t, mux, "/modules/purchasing", tenantID, "user-reader")
	if granted.Code != http.StatusOK {
		t.Fatalf("user-reader module menu: expected 200, got %d: %s", granted.Code, granted.Body.String())
	}
	if !strings.Contains(granted.Body.String(), "/reports/purchasing") {
		t.Fatalf("user-reader (full read access) lost the report link:\n%s", granted.Body.String())
	}

	denied := getAs(t, mux, "/modules/purchasing", tenantID, "user-denied")
	if denied.Code != http.StatusOK {
		t.Fatalf("user-denied module menu: expected 200 (PurchaseOrder itself is still granted, so the module's own hub node stays reachable), got %d: %s", denied.Code, denied.Body.String())
	}
	if !strings.Contains(denied.Body.String(), "/records/PurchaseOrder") {
		t.Fatalf("user-denied lost the PurchaseOrder hub node too — this test no longer isolates the report link specifically:\n%s", denied.Body.String())
	}
	if strings.Contains(denied.Body.String(), "/reports/purchasing") {
		t.Fatalf("user-denied (Item read denied, a report dependency) still saw the report link, which leads to a guaranteed 403:\n%s", denied.Body.String())
	}
}
