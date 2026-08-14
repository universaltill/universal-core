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
	// total is FieldMoney now (uc-infra#136): 123450 minor units = $1234.50
	// — stored directly (this test bypasses entity.ValidateRecord via
	// data.NewRecordRepo, but internal/data/reporting.go's own
	// moneyMinorUnitsPattern guard would exclude a fractional legacy
	// value like the old "1234.5" from the sum entirely).
	if _, err := recordsA.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-A1", "vendor_id": vendorA.ID, "status_id": statusA.ID, "total": 123450,
	}); err != nil {
		t.Fatalf("create PurchaseOrder for tenant A: %v", err)
	}

	vendorB, err := recordsB.Create(ctx, "Party", map[string]any{"name": "Tenant B Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party for tenant B: %v", err)
	}
	// 999999 minor units = $9999.99 — total is FieldMoney now (uc-infra
	// #136); the leak check below must grep for the RENDERED major-unit
	// decimal ("9999.99"), not the raw stored integer, or a real leak
	// would silently stop being caught (independent review: money.Money
	// (999999).String() is "9999.99", which does not contain "999999").
	if _, err := recordsB.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-B1", "vendor_id": vendorB.ID, "status_id": statusB.ID, "total": 999999,
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

	if strings.Contains(body, "9999.99") || strings.Contains(body, "Tenant B Vendor") {
		t.Errorf("tenant B's data leaked into tenant A's report:\n%s", body)
	}
	// 123450 minor units renders as the major-unit decimal "1234.50"
	// (money.Money.String(), uc-infra#136) — not the raw stored integer.
	if !strings.Contains(body, "1234.50") {
		t.Errorf("expected tenant A's own PurchaseOrder total (1234.50) in the report:\n%s", body)
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

// TestAPI_PurchasingReport_ReorderSignalsDegradeWhenItemDefinitionUnavailable
// (uc-infra#157) covers the asymmetry independent review found in
// buildReorderSignals: the ReorderRule lookup already treats
// data.ErrNotFound as "nothing to show" (see the empty-state case in
// TestAPI_PurchasingReport_StockoutRiskAndEmptyStates), but the Item
// lookup three lines below it did not, so a tenant with real ReorderRule
// records but no published Item Definition got a full-page 500 instead
// of the reorder section degrading like every other missing-Definition
// case on this report. Reproduced here by publishing the purchasing
// module (so ReorderRule/InventoryItem/etc. are all available and a rule
// can genuinely fire), seeding a real ReorderRule against a real Item,
// then rolling back only the Item entity Definition — simulating the
// inconsistent-publish-state the independent review flagged as the live
// trigger (uc-infra#157's own originally-reported RBAC-denial scenario
// does not reproduce: the whole-page requireReportRead gate already
// covers ReorderRule/Item denial with a 403, see
// TestAPI_PurchasingReport_DeniedWhenAnyUnderlyingTypeIsRestricted).
func TestAPI_PurchasingReport_ReorderSignalsDegradeWhenItemDefinitionUnavailable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	if err := purchasing.Publish(ctx, db, actor); err != nil {
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

	// qty_available_to_promise 1 (not 0) deliberately keeps this Item off
	// the stockout-risk list below — that list reads Item/InventoryItem
	// straight off the records table by raw SQL, independent of the Item
	// Definition (internal/data/reporting.go's StockoutRiskItems), so it
	// would still render this Item's name even with its Definition
	// rolled back. Only the reorder-signal path (buildReorderSignals,
	// gated on the Definition lookup this test exercises) is under test.
	item := mustCreate("Item", map[string]any{"sku": "SKU-GONE", "name": "Vanishing Widget", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": item, "qty_on_hand": 1, "qty_available_to_promise": 1})
	mustCreate("ReorderRule", map[string]any{
		"item_id": item, "reorder_point": 10, "safety_stock": 0, "target_lead_time_confidence": "p90",
	})

	// Item's Definition version is fixed by purchasing.Item()'s own
	// declaration; rolling it back leaves the already-created
	// Item/InventoryItem/ReorderRule records untouched (they're plain
	// rows in the records table) but makes ts.entityDef(ctx, "Item")
	// return data.ErrNotFound, same as any other tenant that never
	// published Item at all.
	entDefs := data.NewEntityDefinitionRepo(db)
	if err := entDefs.Rollback(ctx, "Item", purchasing.Item().Version, actor); err != nil {
		t.Fatalf("roll back Item entity definition: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (reorder section degraded, not a page-wide failure), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No reorder signals right now.") {
		t.Errorf("expected the reorder-signal empty state once Item's Definition is unavailable, not a partial/broken render:\n%s", body)
	}
	if strings.Contains(body, "Vanishing Widget") {
		t.Errorf("no reorder signal for an unpublished Item may render:\n%s", body)
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

// TestAPI_PurchasingReport_ReorderSignalHonorsHiddenSafetyStock is
// uc-infra#229's own regression test. That issue was a secondhand flag
// from #222's independent reviewer, raised by analogy ("same shape as
// #222's ProjectBudgetLine.category leak") but never itself reproduced
// against real Postgres — this test is that reproduction attempt.
//
// safety_stock itself does NOT reproduce as a value-in-output leak:
// unlike #222's ProjectBudgetActuals (a raw-SQL data.ReportingRepo
// aggregate with no FieldPermission awareness at all), buildReorderSignals
// reads ReorderRule through ts.crud — authz.GuardedEngine, which redacts
// every hidden field out of a row's Data before this handler ever sees it
// (see GuardedEngine.List/redact). And reorderRowView (this file) has no
// SafetyStock field to begin with — the number is used only internally, to
// decide whether a rule fires, and is never rendered as text on any signal
// row. That is narrower than it might sound: this report's On Hand/
// Position columns a few lines below (qty_on_hand/qty_available_to_promise,
// read via the SAME ts.reporting.* raw-SQL path #222 fixed for a different
// entity) do NOT get this protection — confirmed, independently, as a real
// leak and filed separately as uc-infra#230. This test's "not a leak"
// finding is specific to safety_stock; it says nothing about the rest of
// this report's columns.
//
// What this test pins instead is the real, adjacent behavior a hidden
// safety_stock actually gets: because GuardedEngine.redact deletes the
// key outright, rule.Data["safety_stock"] misses its type assertion and
// falls back to the same 0 this file's own doc comment already
// documents for a genuinely-never-set safety_stock ("missing = 0, per
// the design") — a redacted actor and a "no safety stock configured"
// tenant are indistinguishable to this function. That is a real,
// separate, lower-severity question from a value leak (does a
// FieldPermission-restricted buyer sometimes silently NOT see a
// signal an unrestricted buyer sees, because their view of safety_stock
// collapsed to 0?) — filed as its own follow-up, uc-infra#231, rather
// than folded into this card: unlike #222's fix, resolving it means
// picking a business behavior (show the signal anyway? show it with a
// "some criteria hidden" note? something else?), not just widening an
// existing redaction predicate.
func TestAPI_PurchasingReport_ReorderSignalHonorsHiddenSafetyStock(t *testing.T) {
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

	// qty_available_to_promise 2 (not 0) deliberately keeps both items off
	// the stockout-risk table below (internal/data/reporting.go's
	// StockoutRiskItems filters WHERE atp <= 0) — same fixture-coupling
	// TestAPI_PurchasingReport_ReorderSignalsDegradeWhenItemDefinitionUnavailable
	// documents above. Only the reorder-signal path is under test here.
	itemSS := mustCreate("Item", map[string]any{"sku": "SKU-SS", "name": "Safety Stock Widget", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": itemSS, "qty_on_hand": 2, "qty_available_to_promise": 2})
	// position (2) > reorder_point (1) alone -> only fires if safety_stock
	// (5) is actually counted: 2 <= 1+5. Chosen deliberately so the two
	// actors below can only differ on whether THIS rule fires, not on any
	// rendered number a hidden field might otherwise leak.
	mustCreate("ReorderRule", map[string]any{
		"item_id": itemSS, "reorder_point": 1, "safety_stock": 5, "target_lead_time_confidence": "p90",
	})

	// A second item whose rule fires on reorder_point ALONE (safety_stock
	// 0, real and redacted alike) — the severity-bearing half uc-infra#231's
	// own review flagged as missing: a restricted buyer must still see
	// every signal that doesn't depend on the hidden field, with the real
	// OnHand/Position/ReorderPoint numbers on that row (reorder_point
	// itself is never subject to this actor's FieldPermission at all,
	// unlike safety_stock).
	itemRP := mustCreate("Item", map[string]any{"sku": "SKU-RP", "name": "Reorder Point Widget", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": itemRP, "qty_on_hand": 3, "qty_available_to_promise": 3})
	mustCreate("ReorderRule", map[string]any{
		"item_id": itemRP, "reorder_point": 10, "safety_stock": 0, "target_lead_time_confidence": "p90",
	})

	seedFieldRule(t, db, "restricted-buyer", "user-restricted", "ReorderRule", "safety_stock")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Unrestricted actor: real safety_stock counted, both signals fire.
	recOpen := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if recOpen.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recOpen.Code, recOpen.Body.String())
	}
	if !strings.Contains(recOpen.Body.String(), "Safety Stock Widget") {
		t.Errorf("expected the signal to fire for an actor who can see safety_stock (position 2 <= reorder_point 1 + safety_stock 5):\n%s", recOpen.Body.String())
	}
	if !strings.Contains(recOpen.Body.String(), "Reorder Point Widget") {
		t.Errorf("expected the reorder_point-only signal to fire regardless:\n%s", recOpen.Body.String())
	}

	// Restricted actor: safety_stock is redacted out of ReorderRule.Data
	// before buildReorderSignals ever reads it, so it falls back to the
	// same 0 a genuinely-unset safety_stock would use -> 2 <= 1+0 is
	// false, the Safety Stock Widget signal does not fire. This is the
	// current, intentional-looking (if debatable — see uc-infra#231)
	// behavior. It is NOT a value leak: safety_stock has no rendered
	// column on this report (reorderRowView, above) for the real number 5
	// to appear in, for either actor.
	recRestricted := getAs(t, mux, "/reports/purchasing", tenantID, "user-restricted")
	if recRestricted.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recRestricted.Code, recRestricted.Body.String())
	}
	restrictedBody := recRestricted.Body.String()
	if strings.Contains(restrictedBody, "Safety Stock Widget") {
		t.Errorf("actor with safety_stock hidden must not see a signal that only fires by counting the real safety_stock value:\n%s", restrictedBody)
	}
	// The reorder_point-only signal is NOT affected by safety_stock being
	// hidden: it must still fire, with its real OnHand/Position/
	// ReorderPoint numbers, exactly as for the unrestricted actor above —
	// a hidden safety_stock must not blank signals that never depended on
	// it (uc-infra#231's own review, finding 3d).
	wantRPSignal := "<tr><td><a href=\"/forms/Item/" + itemRP + "\">Reorder Point Widget</a></td><td>3</td><td>0</td><td>3</td><td>10</td>"
	if !strings.Contains(restrictedBody, wantRPSignal) {
		t.Errorf("expected the reorder_point-only signal row %q to survive safety_stock being hidden:\n%s", wantRPSignal, restrictedBody)
	}
}

// TestAPI_PurchasingReport_HiddenInventoryQuantitiesRenderNotAvailable is
// uc-infra#230's own regression test: the real, empirically-confirmed leak
// the independent (opus) reviewer found while investigating uc-infra#229
// (see that test's own doc comment) — qty_on_hand/qty_available_to_promise
// are read via ts.reporting's raw SQL (data.ReportingRepo), entirely
// outside the ts.crud-redacted path, in three places on this report: the
// Stock Summary card's aggregate totals, the Stockout Risk table's
// per-item columns, and the Reorder Signals table's On Hand/Position
// columns. Before this fix every one of these rendered the real number to
// an actor whose role hides the underlying InventoryItem field via a
// FieldPermission (ADR-0006), the same class of leak uc-infra#222 already
// fixed once for a different entity/field (ProjectBudgetLine.category).
//
// Deliberately covers qty_on_hand and qty_available_to_promise with TWO
// separate restricted actors (not one actor with both hidden) to prove the
// two fields are gated independently, not behind one shared "any quantity
// hidden" flag — hiding one must leave the other's real number visible.
//
// Position is checked together with On Hand for the qty_on_hand-hidden
// actor, not on its own: Position is on_hand+on_order, and On Order is
// always rendered on the same row, so a visible Position next to a visible
// On Order would let a restricted actor recover the real on-hand figure
// by simple subtraction even with the On Hand cell itself correctly
// blanked (see buildReorderSignals' own comment on why the fix gates both
// together).
//
// This fix does NOT change whether the reorder signal fires: the row
// below fires purely on real, uncomputed-for-display quantities for every
// actor (unrestricted and both restricted ones alike) — only the rendered
// cell is redacted, the fire/no-fire decision is unchanged. That is a
// deliberate, narrower scope than uc-infra#231's still-open business
// question about safety_stock: this is "redact the display," not "change
// what the report decides to show a signal for."
func TestAPI_PurchasingReport_HiddenInventoryQuantitiesRenderNotAvailable(t *testing.T) {
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

	// Deliberately distinct, unambiguous numbers (not 0, not shared with
	// any other value on this row) so a leaked figure can't hide behind a
	// coincidental match with reorder_point/on_order elsewhere in the body.
	// qty_available_to_promise -4 (<=0) also puts this item on the
	// Stockout Risk table, so this single fixture exercises all three
	// leak sites uc-infra#230 found, not just the reorder-signal one
	// uc-infra#229's own regression test already covers.
	itemID := mustCreate("Item", map[string]any{"sku": "SKU-LEAK", "name": "Leaky Widget", "item_type": "stock"})
	mustCreate("InventoryItem", map[string]any{"item_id": itemID, "qty_on_hand": 17, "qty_available_to_promise": -4})
	mustCreate("ReorderRule", map[string]any{"item_id": itemID, "reorder_point": 25, "safety_stock": 0, "target_lead_time_confidence": "p90"})

	seedFieldRule(t, db, "onhand-restricted", "user-onhand-hidden", "InventoryItem", "qty_on_hand")
	seedFieldRule(t, db, "atp-restricted", "user-atp-hidden", "InventoryItem", "qty_available_to_promise")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Unrestricted actor: every real number renders, on every table,
	// including the Stockout Risk heading's own real count.
	openBody := getAs(t, mux, "/reports/purchasing", tenantID, "farshid").Body.String()
	for _, want := range []string{
		// Stock Summary card.
		`<div class="uc-report-card-label">Qty On Hand</div>
  <div class="uc-report-card-value">17</div>`,
		`<div class="uc-report-card-label">Qty Available to Promise</div>
  <div class="uc-report-card-value">-4</div>`,
		// Stockout Risk heading count + row.
		"<h2>Stockout Risk (1)</h2>",
		`<td><a href="/forms/Item/` + itemID + `">SKU-LEAK</a></td><td>Leaky Widget</td><td>17</td><td>-4</td>`,
		// Reorder Signals row: OnHand=17, OnOrder=0, Position=17, ReorderPoint=25.
		`<tr><td><a href="/forms/Item/` + itemID + `">Leaky Widget</a></td><td>17</td><td>0</td><td>17</td><td>25</td>`,
	} {
		if !strings.Contains(openBody, want) {
			t.Errorf("unrestricted actor: expected %q in body:\n%s", want, openBody)
		}
	}

	// qty_on_hand hidden: On Hand AND Position blank on every table; ATP
	// (a different field) stays real everywhere. Not asserting "17 never
	// appears anywhere in the body" as a blanket scan: itemID is a random
	// UUID and can coincidentally contain "17"/"-4" as a hex substring,
	// so the precise, anchored per-cell checks below are the real
	// assertion — they pin exactly which cell shows what, not just
	// whether a digit sequence occurs somewhere in the page.
	onHandBody := getAs(t, mux, "/reports/purchasing", tenantID, "user-onhand-hidden").Body.String()
	for _, want := range []string{
		`<div class="uc-report-card-label">Qty On Hand</div>
  <div class="uc-report-card-value">Not available</div>`,
		`<div class="uc-report-card-label">Qty Available to Promise</div>
  <div class="uc-report-card-value">-4</div>`,
		`<td><a href="/forms/Item/` + itemID + `">SKU-LEAK</a></td><td>Leaky Widget</td><td>Not available</td><td>-4</td>`,
		`<tr><td><a href="/forms/Item/` + itemID + `">Leaky Widget</a></td><td>Not available</td><td>0</td><td>Not available</td><td>25</td>`,
	} {
		if !strings.Contains(onHandBody, want) {
			t.Errorf("qty_on_hand-restricted actor: expected %q in body:\n%s", want, onHandBody)
		}
	}

	// qty_available_to_promise hidden: the Stock Summary card's ATP total
	// blanks (a straightforward per-cell redaction, like OnHand's own
	// above). The Stockout Risk section, though, is NOT a per-cell case:
	// StockoutRiskItems' row membership and stock.StockoutCount are both
	// filtered/computed server-side from the real, un-redacted
	// qty_available_to_promise (WHERE atp <= 0), so blanking only the ATP
	// cell would still leak "this item's real ATP is <= 0" via bare
	// presence in the list and the heading's own count — the WHOLE
	// section (heading count AND table) must gate together instead (see
	// purchasingReportView.StockoutAvailable's own comment). On Hand/
	// Position stay real everywhere else — hiding ATP must not blank a
	// field it has no relationship to.
	atpBody := getAs(t, mux, "/reports/purchasing", tenantID, "user-atp-hidden").Body.String()
	for _, want := range []string{
		`<div class="uc-report-card-label">Qty On Hand</div>
  <div class="uc-report-card-value">17</div>`,
		`<div class="uc-report-card-label">Qty Available to Promise</div>
  <div class="uc-report-card-value">Not available</div>`,
		// No "(N)" count suffix on the heading, and no table — just the
		// section-level not-available notice.
		"<h2>Stockout Risk</h2>",
		`<p class="uc-empty">Not available</p>`,
		`<tr><td><a href="/forms/Item/` + itemID + `">Leaky Widget</a></td><td>17</td><td>0</td><td>17</td><td>25</td>`,
	} {
		if !strings.Contains(atpBody, want) {
			t.Errorf("qty_available_to_promise-restricted actor: expected %q in body:\n%s", want, atpBody)
		}
	}
	if strings.Contains(atpBody, "SKU-LEAK") {
		t.Errorf("qty_available_to_promise-restricted actor: the Stockout Risk row must not render at all (its membership itself is derived from the hidden field), but SKU-LEAK appears:\n%s", atpBody)
	}
	if strings.Contains(atpBody, "Stockout Risk (") {
		t.Errorf("qty_available_to_promise-restricted actor: the Stockout Risk heading must not show a count derived from the hidden field:\n%s", atpBody)
	}
}

// TestAPI_PurchasingReport_OnTimeDeliverySection (#11) exercises the
// on-time-delivery table end to end: a vendor with enough promised-date
// samples to show a real rate, a vendor with only one (insufficient),
// and a completed PO with NO promised_delivery_date at all (must be
// excluded from every count, not counted as a miss) — plus the overall
// summary row and localized (tr) rendering, same structure as
// TestAPI_PurchasingReport_LeadTimeAndReorderSections.
func TestAPI_PurchasingReport_OnTimeDeliverySection(t *testing.T) {
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

	vendor := mustCreate("Party", map[string]any{"name": "Promise Co", "party_type": "organization"})
	soloVendor := mustCreate("Party", map[string]any{"name": "Solo Promiser", "party_type": "organization"})

	// Promise Co: 2 on-time (received on/before promise), 1 late -> 2/3.
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OT-1", "vendor_id": vendor,
		"order_date": "2026-07-01", "promised_delivery_date": "2026-07-10", "received_at": "2026-07-08",
	})
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OT-2", "vendor_id": vendor,
		"order_date": "2026-07-02", "promised_delivery_date": "2026-07-12", "received_at": "2026-07-12",
	})
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OT-3", "vendor_id": vendor,
		"order_date": "2026-07-03", "promised_delivery_date": "2026-07-11", "received_at": "2026-07-15",
	})
	// Solo Promiser: one promised sample -> insufficient.
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OT-SOLO", "vendor_id": soloVendor,
		"order_date": "2026-07-04", "promised_delivery_date": "2026-07-14", "received_at": "2026-07-14",
	})
	// A completed PO with no promised_delivery_date at all — must not
	// appear in any on-time count for Promise Co (it has no promise to
	// judge against), even though it's a real completed order.
	mustCreate("PurchaseOrder", map[string]any{
		"po_number": "PO-OT-NOPROMISE", "vendor_id": vendor,
		"order_date": "2026-07-05", "received_at": "2026-07-09",
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "On-Time Delivery") {
		t.Fatalf("expected the #11 On-Time Delivery section heading:\n%s", body)
	}
	// Promise Co: N=3 (the no-promise PO excluded), 2 on time -> 66.7%.
	if !strings.Contains(body, "<tr><td>Promise Co</td><td>3</td><td>66.7%</td></tr>") {
		t.Errorf("expected the Promise Co on-time row at 3 samples / 66.7%%:\n%s", body)
	}
	// Solo Promiser: N=1, insufficient — never a fabricated rate.
	if !strings.Contains(body, "<tr><td>Solo Promiser</td><td>1</td><td>Insufficient history</td></tr>") {
		t.Errorf("expected the Solo Promiser row to say insufficient at n=1:\n%s", body)
	}
	// Overall across the 4 promised samples (Promise Co's 3 + Solo's 1):
	// 3 on-time -> 3/4 = 75%.
	if !strings.Contains(body, "<tr><td>All vendors</td><td>4</td><td>75%</td></tr>") {
		t.Errorf("expected the all-vendors on-time summary row at 4 samples / 75%%:\n%s", body)
	}

	recTR := getAs(t, mux, "/reports/purchasing?lang=tr", tenantID, "farshid")
	if recTR.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?lang=tr, got %d: %s", recTR.Code, recTR.Body.String())
	}
	if !strings.Contains(recTR.Body.String(), "Zamanında Teslimat") {
		t.Errorf("expected the localized (tr) On-Time Delivery heading:\n%s", recTR.Body.String())
	}
}

// TestAPI_PurchasingReport_OnTimeDeliveryPartialReceipt (#11,
// independent review 2026-08-01) is the end-to-end regression for the
// bug that review caught: a PO promised 2026-07-10 whose first
// GoodsReceipt arrives a day EARLY (07-09) but whose order isn't
// actually complete until a second GoodsReceipt 82 days LATE (09-30).
// Before the fix, the report used the FIRST receipt date (#30's
// lead-time semantics reused unexamined) and scored this vendor 100%
// on-time; it must now score 0% — the order was not fully satisfied
// until long after the promise.
func TestAPI_PurchasingReport_OnTimeDeliveryPartialReceipt(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	records := data.NewRecordRepo(db)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Partial Ship Co", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-PARTIAL-API", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "promised_delivery_date": "2026-07-10",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-09",
	}); err != nil {
		t.Fatalf("create early partial GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-09-30",
	}); err != nil {
		t.Fatalf("create late completing GoodsReceipt: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// N=1, 0 on time -> 0%. The single sample is below MinSamples, so the
	// vendor row itself says "Insufficient history" — but the overall row
	// (also N=1 here) proves the SAME thing: this is a per-row rendering
	// choice (MinSamples), not evidence the rate was computed correctly,
	// so assert on the underlying stats via a second promised PO for the
	// same vendor that IS on time, making N=2 sufficient and forcing the
	// rate itself (1/2 = 50%) to render.
	mustCreate := func(fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, "PurchaseOrder", fields); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreate(map[string]any{
		"po_number": "PO-PARTIAL-API-2", "vendor_id": vendor.ID,
		"order_date": "2026-07-02", "promised_delivery_date": "2026-07-12", "received_at": "2026-07-12",
	})
	rec = getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	body = rec.Body.String()
	if !strings.Contains(body, "<tr><td>Partial Ship Co</td><td>2</td><td>50%</td></tr>") {
		t.Errorf("expected Partial Ship Co at 1/2 = 50%% (the partial-receipt PO must count as LATE, not on time):\n%s", body)
	}
	if strings.Contains(body, "<tr><td>Partial Ship Co</td><td>2</td><td>100%</td></tr>") {
		t.Fatal("Partial Ship Co scored 100% on-time — the early PARTIAL receipt was used instead of the actual completion date")
	}
}

// TestAPI_PurchasingReport_OnTimeDeliveryEmptyState (#11): when no
// completed PO carries a promised_delivery_date at all — true for every
// tenant until someone starts filling it in, and true of every OTHER
// report test in this file that predates #11 — the section must render
// its own empty-state copy, not an empty table and not the lead-time
// section's unrelated one.
func TestAPI_PurchasingReport_OnTimeDeliveryEmptyState(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	records := data.NewRecordRepo(db)
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "No Promise Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-NOPROMISE-1", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "received_at": "2026-07-09",
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No completed purchase orders with a promised delivery date yet.") {
		t.Errorf("expected the on-time-delivery empty state:\n%s", body)
	}
	if !strings.Contains(body, "Supplier Lead Times") {
		t.Errorf("expected the lead-time section to still render its own (non-empty) data:\n%s", body)
	}
}

// TestAPI_PurchasingReport_QualitySection (uc-infra#82) exercises the
// quality table end to end: a vendor with enough inspected lines to
// show a real, quantity-weighted rate, a vendor with only one line
// (insufficient), and a line with NO quality data at all (must be
// excluded from every count, not counted as a defect) — plus the
// overall summary row and localized (tr) rendering, same structure as
// TestAPI_PurchasingReport_OnTimeDeliverySection.
func TestAPI_PurchasingReport_QualitySection(t *testing.T) {
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

	vendor := mustCreate("Party", map[string]any{"name": "Inspected Co", "party_type": "organization"})
	soloVendor := mustCreate("Party", map[string]any{"name": "Solo Inspected", "party_type": "organization"})

	po := mustCreate("PurchaseOrder", map[string]any{"po_number": "PO-Q-1", "vendor_id": vendor, "order_date": "2026-07-01"})
	gr := mustCreate("GoodsReceipt", map[string]any{"purchase_order_id": po, "received_date": "2026-07-05"})
	// Inspected Co: two lines, 18 accepted / 2 rejected across both -> 90%.
	mustCreate("GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr, "qty_received": 10.0, "qty_accepted": 8.0, "qty_rejected": 2.0,
	})
	mustCreate("GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr, "qty_received": 10.0, "qty_accepted": 10.0, "qty_rejected": 0.0,
	})
	// A line with no quality data at all — must not appear in any count
	// for Inspected Co, even though it's a real received line.
	mustCreate("GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr, "qty_received": 5.0,
	})

	soloPO := mustCreate("PurchaseOrder", map[string]any{"po_number": "PO-Q-SOLO", "vendor_id": soloVendor, "order_date": "2026-07-02"})
	soloGR := mustCreate("GoodsReceipt", map[string]any{"purchase_order_id": soloPO, "received_date": "2026-07-06"})
	// Solo Inspected: one inspected line -> insufficient.
	mustCreate("GoodsReceiptLine", map[string]any{
		"goods_receipt_id": soloGR, "qty_received": 4.0, "qty_accepted": 4.0, "qty_rejected": 0.0,
	})

	// A PurchaseOrder with no vendor_id at all — its quality data must
	// still count toward the OVERALL aggregate (same reasoning
	// CompletedPOLeadTimes' own vendorless samples already get) but must
	// NOT produce a broken/blank per-vendor row of its own.
	novendorPO := mustCreate("PurchaseOrder", map[string]any{"po_number": "PO-Q-NOVENDOR", "order_date": "2026-07-03"})
	novendorGR := mustCreate("GoodsReceipt", map[string]any{"purchase_order_id": novendorPO, "received_date": "2026-07-07"})
	mustCreate("GoodsReceiptLine", map[string]any{
		"goods_receipt_id": novendorGR, "qty_received": 2.0, "qty_accepted": 2.0, "qty_rejected": 0.0,
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Quality") {
		t.Fatalf("expected the uc-infra#82 Quality section heading:\n%s", body)
	}
	// Inspected Co: N=2 (the no-data line excluded), 18/20 accepted -> 90%.
	if !strings.Contains(body, "<tr><td>Inspected Co</td><td>2</td><td>90%</td></tr>") {
		t.Errorf("expected the Inspected Co quality row at 2 samples / 90%%:\n%s", body)
	}
	// Solo Inspected: N=1, insufficient — never a fabricated rate.
	if !strings.Contains(body, "<tr><td>Solo Inspected</td><td>1</td><td>Insufficient history</td></tr>") {
		t.Errorf("expected the Solo Inspected row to say insufficient at n=1:\n%s", body)
	}
	// Overall across 4 inspected lines (Inspected Co's 2 + Solo's 1 + the
	// vendorless PO's 1): 24 accepted / 26 total -> 92.3%.
	if !strings.Contains(body, "<tr><td>All vendors</td><td>4</td><td>92.3%</td></tr>") {
		t.Errorf("expected the all-vendors quality summary row at 4 samples / 92.3%%:\n%s", body)
	}
	// The vendorless PO's quality data must NOT produce its own
	// per-vendor row — no row with an empty vendor cell.
	if strings.Contains(body, "<tr><td></td><td>1</td>") {
		t.Errorf("vendorless quality data produced its own (blank-vendor) row, must only count toward the overall row:\n%s", body)
	}

	recTR := getAs(t, mux, "/reports/purchasing?lang=tr", tenantID, "farshid")
	if recTR.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?lang=tr, got %d: %s", recTR.Code, recTR.Body.String())
	}
	if !strings.Contains(recTR.Body.String(), "Kalite") {
		t.Errorf("expected the localized (tr) Quality heading:\n%s", recTR.Body.String())
	}
}

// TestAPI_PurchasingReport_QualityEmptyState (uc-infra#82): when no
// GoodsReceiptLine carries qty_accepted/qty_rejected at all — true for
// every tenant until someone starts filling it in — the section must
// render its own empty-state copy, not an empty table and not the
// on-time section's unrelated one.
func TestAPI_PurchasingReport_QualityEmptyState(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	records := data.NewRecordRepo(db)
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "No Quality Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{"po_number": "PO-NOQ-1", "vendor_id": vendor.ID, "order_date": "2026-07-01"})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{"purchase_order_id": po.ID, "received_date": "2026-07-05"})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 5.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "farshid")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No goods receipt lines with a recorded quality outcome yet.") {
		t.Errorf("expected the quality empty state:\n%s", body)
	}
	if !strings.Contains(body, "Supplier Lead Times") {
		t.Errorf("expected the lead-time section to still render its own (non-empty) data:\n%s", body)
	}
}

// TestQualitySamples_SkipsRowsWithOnlyOneQuantityParsing (uc-infra#82,
// independent review) pins qualitySamples' own defensive branch: a row
// where exactly one of QtyAccepted/QtyRejected fails to parse — reachable
// once GoodsReceiptLine's generic edit path can persist a partial/
// corrupted quality record despite the write-time hook's own
// required-together check (a direct DB edit, a pre-hook migration, or a
// hook bypassed some other way) — must be skipped entirely, not credited
// with only the half that DID parse. This is the same "don't trust one
// half of a pair without the other" discipline
// validateGoodsReceiptLineQuality enforces at write time; this is the
// read-time backstop for data that got past it anyway.
func TestQualitySamples_SkipsRowsWithOnlyOneQuantityParsing(t *testing.T) {
	rows := []data.GoodsReceiptLineQuality{
		{LineID: "l1", VendorID: "v1", QtyAccepted: "8", QtyRejected: ""},             // only one set
		{LineID: "l2", VendorID: "v1", QtyAccepted: "not-a-number", QtyRejected: "2"}, // one unparseable
		{LineID: "l3", VendorID: "v1", QtyAccepted: "", QtyRejected: ""},              // neither set — the common case
		{LineID: "l4", VendorID: "v1", QtyAccepted: "8", QtyRejected: "2"},            // the one real sample
	}
	got := qualitySamples(rows)
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1 (only the fully-parseable row): %+v", len(got), got)
	}
	if got[0].QtyAccepted != 8 || got[0].QtyRejected != 2 {
		t.Fatalf("unexpected sample survived: %+v", got[0])
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
	// and Status, and (uc-infra#54) its netting subquery joins
	// GoodsReceiptLine; OnHandQtyByItem reads InventoryItem;
	// LatestPOVendorByItem reads POLine joined to PurchaseOrder;
	// buildReorderSignals reads ReorderRule and Item through the guarded
	// engine; GoodsReceiptLineQualities (uc-infra#82) reads
	// GoodsReceiptLine joined to GoodsReceipt, PurchaseOrder, and Party.
	want := map[string]bool{
		"PurchaseOrder": true, "Status": true, "Party": true, "InventoryItem": true, "Item": true,
		"POLine": true, "GoodsReceipt": true, "GoodsReceiptLine": true, "ReorderRule": true,
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
// read on an entity type the report never touches (SalariedStaff — no query
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
	// total is FieldMoney now (uc-infra#136): 314 minor units = $3.14,
	// rendered as "3.14" (money.Money.String()), not the raw "314".
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-UNRELATED", "vendor_id": vendor.ID, "status_id": st.ID, "total": 314,
	}); err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}

	seedRBAC(t, db,
		map[string][]string{"outsider": {"user-outsider"}},
		[]map[string]any{{"role": "outsider", "entity_type": "SalariedStaff", "can_read": false, "can_write": false}},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/reports/purchasing", tenantID, "user-outsider")
	if rec.Code != http.StatusOK {
		t.Fatalf("role denied on an unrelated type (SalariedStaff): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "3.14") {
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
	// total is FieldMoney now (uc-infra#136): 42 minor units = $0.42.
	if _, err := data.NewRecordRepo(db).Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "status_id": statusRow.ID, "total": 42,
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
		if !strings.Contains(rec.Body.String(), "0.42") {
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
