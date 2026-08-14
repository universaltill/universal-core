package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestPurchasingReport_HiddenPurchaseOrderPartyItemFields_RealBrowser
// (uc-infra#233) is the real-browser proof that hiding
// PurchaseOrder.total/Party.name/Item.sku/Item.name via a FieldPermission
// removes each real value from the live DOM entirely — not merely from a
// rendered-HTML-string assertion (internal/api's own
// TestAPI_PurchasingReport_HiddenPurchaseOrderPartyItemFieldsRenderNotAvailable,
// which already pins per-field independence one field at a time). Same
// class of proof TestFieldPermission_HiddenFieldAbsentFromLiveDOM gives
// for a form field, and the same discipline this report's own
// InventoryItem.qty_on_hand/qty_available_to_promise fix will need when
// it lands (uc-infra#230, a separate, still-open card — not covered by
// this test) — ReportingRepo's raw SQL reads all four of these fields
// entirely outside the ts.crud-redacted path, so nothing short of an
// actual browser DOM scan proves the real values never reach the page.
//
// All four fields are hidden from the SAME browser actor at once here
// (unlike the internal/api test's one-field-at-a-time actors): browserCtx
// always authenticates as the one fixed e2eActorID, and this test's job
// is to prove the real DOM never leaks any of the four, together, not to
// re-litigate their independence (already covered at the HTTP level).
func TestPurchasingReport_HiddenPurchaseOrderPartyItemFields_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	// ONE role holding all four FieldPermission rows, not four
	// single-field roles granted to the same actor: authz.Resolver.
	// HiddenFields' own contract (authz.go) is a field is hidden only
	// when EVERY role the actor holds has a hidden=true row for it — four
	// single-field roles would each fail that unanimity test for the
	// OTHER three roles' fields, hiding NONE of them (caught by this
	// test's own first run: every value leaked with four separate
	// grantE2ERole/seedFieldPermission calls, exactly this gotcha).
	roleID := grantE2ERole(t, tenantDB, "e2e_233_all_redacted")
	engine := crud.NewEngine(tenantDB)
	for _, fp := range []struct{ entityType, field string }{
		{"PurchaseOrder", "total"}, {"Party", "name"}, {"Item", "sku"}, {"Item", "name"},
	} {
		if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
			"role_id": roleID, "entity_type": fp.entityType, "field_name": fp.field, "hidden": true,
		}, actor); err != nil {
			t.Fatalf("create FieldPermission %s.%s: %v", fp.entityType, fp.field, err)
		}
	}
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Leak Vendor Co", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	approvedID := publishedStatusID(t, tenantDB, "purchase_order_status", "approved")
	// total is FieldMoney (minor units, uc-infra#136): 246800 -> "2468.00"
	// — deliberately not a bare integer, so a coincidental hex-substring
	// match inside a seeded record's own random UUID can't produce a
	// false POSITIVE on the leak scan below (a bare "246800"-style digit
	// run is exactly the kind of thing a random UUID could coincidentally
	// contain; the decimal-point rendering of a real FieldMoney value
	// isn't).
	if _, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-E2E-LEAK233", "vendor_id": vendor.ID, "order_date": "2026-08-01",
		"status_id": approvedID, "total": 246800,
	}, actor); err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-LEAK233", "name": "E2E Leak Widget 233", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	facility, err := engine.Create(ctx, purchasing.Facility(), map[string]any{
		"code": "E2E-LEAK233-MAIN", "name": "E2E Leak Warehouse 233", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed Facility: %v", err)
	}
	// qty_available_to_promise -3.5 (<=0, and not a bare integer for the
	// same leak-scan reason as total above) puts the item on the
	// Stockout Risk table, so its SKU/Name cells are exercised too.
	if _, err := engine.Create(ctx, purchasing.InventoryItem(), map[string]any{
		"item_id": item.ID, "facility_id": facility.ID,
		"qty_on_hand": 12, "qty_available_to_promise": -3.5,
	}, actor); err != nil {
		t.Fatalf("seed InventoryItem: %v", err)
	}
	// A SECOND, completed PurchaseOrder for the SAME vendor — feeding the
	// Supplier Lead Times/On-Time Delivery tables, whose Vendor column
	// reads Party.name through this exact same unredacted raw-SQL path.
	// Independent review of an earlier version of this fix caught it
	// missing these tables (and the Quality table, covered separately at
	// the HTTP level by TestAPI_PurchasingReport_HiddenPurchaseOrderPartyItemFieldsRenderNotAvailable,
	// whose GoodsReceiptLine fixture doesn't need the real crud.Engine
	// hooks a browser-driven Quality fixture here would) entirely — this
	// fixture (and the leak scan below) is what proves the real DOM
	// never leaks the vendor name on the other two either, not just on
	// the vendor-spend table.
	receivedID := publishedStatusID(t, tenantDB, "purchase_order_status", "received")
	if _, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-E2E-LEAK233-LT", "vendor_id": vendor.ID, "status_id": receivedID,
		"order_date": "2026-08-01", "received_at": "2026-08-05", "promised_delivery_date": "2026-08-05",
	}, actor); err != nil {
		t.Fatalf("seed completed PurchaseOrder: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	for _, leaked := range []string{"2468.00", "E2E Leak Vendor Co", "SKU-E2E-LEAK233", "E2E Leak Widget 233"} {
		if strings.Contains(bodyText, leaked) {
			t.Errorf("redacted value %q reached the live page for an actor with it hidden:\n%s", leaked, bodyText)
		}
	}
	if !strings.Contains(bodyText, "Not available") {
		t.Errorf("expected at least one 'Not available' placeholder (status card, vendor row, stockout row):\n%s", bodyText)
	}
	// qty_on_hand/qty_available_to_promise are DIFFERENT fields, not
	// hidden by this actor's role — their real numbers, and the item's
	// presence on the Stockout Risk table they drive, must still render.
	if !strings.Contains(bodyText, "12") || !strings.Contains(bodyText, "-3.5") {
		t.Errorf("expected qty_on_hand (12) and qty_available_to_promise (-3.5) — fields this actor CAN see — to still render:\n%s", bodyText)
	}

	// Belt-and-braces, same reasoning TestFieldPermission_HiddenFieldAbsentFromLiveDOM's
	// own leak check already uses for a form field (internal/e2e/field_permission_test.go)
	// — confirm none of the four figures are sitting anywhere in the
	// document outside the visible text either. (This report's own
	// InventoryItem quantities will need the equivalent check when
	// uc-infra#230 lands — not yet, since that fix isn't in this
	// codebase.)
	for _, leaked := range []string{"2468.00", "E2E Leak Vendor Co", "SKU-E2E-LEAK233", "E2E Leak Widget 233"} {
		var leaks bool
		if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
			`document.documentElement.outerHTML.includes("`+leaked+`")`, &leaks,
		)); err != nil {
			t.Fatalf("scan document for %q: %v", leaked, err)
		}
		if leaks {
			t.Errorf("redacted value %q reached the browser somewhere in the document", leaked)
		}
	}
}

// TestPurchasingReport_LeadTimeAndReorderSections_RealBrowser (#30):
// /reports/purchasing renders the two new sections — "Supplier Lead
// Times" and "Reorder Signals" — in a real headless Chrome, with a
// reorder signal actually firing against seeded inventory + open-PO
// state and carrying the P90 expected-days context computed from two
// completed POs (9 and 11 days -> P90 10.8, exact type-7
// interpolation; see internal/kernel/forecast). The internal/api tests
// already pin the row markup; this proves the page a buyer actually
// loads carries both sections through the real server + browser stack,
// same reasoning as every other e2e in this package.
func TestPurchasingReport_LeadTimeAndReorderSections_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// testServer publishes entities/forms but not the status graph —
	// PurchaseOrder.status_id needs real Status records to point at
	// (idempotent, same call cmd/provision-tenant makes; same setup as
	// form_save_test.go's reversed-dates test).
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Report Vendor Co", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	receivedID := publishedStatusID(t, tenantDB, "purchase_order_status", "received")
	approvedID := publishedStatusID(t, tenantDB, "purchase_order_status", "approved")

	// Two completed POs: 9 days and 11 days -> P50 10 / P90 10.8.
	for _, po := range []struct{ number, ordered, received string }{
		{"PO-E2E-1", "2026-07-01", "2026-07-10"},
		{"PO-E2E-2", "2026-07-05", "2026-07-16"},
	} {
		if _, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
			"po_number": po.number, "vendor_id": vendor.ID, "order_date": po.ordered,
			"status_id": receivedID, "received_at": po.received,
		}, actor); err != nil {
			t.Fatalf("seed completed PO %s: %v", po.number, err)
		}
	}

	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-LOW", "name": "E2E Reorder Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	// InventoryItem is keyed by (item, facility) since #12, so stock
	// needs somewhere to be. One facility here on purpose: this test is
	// about the reorder signal, and the cross-facility aggregation it
	// depends on is pinned separately in internal/data's
	// TestStockReports_AggregateAcrossFacilities.
	facility, err := engine.Create(ctx, purchasing.Facility(), map[string]any{
		"code": "E2E-MAIN", "name": "E2E Main Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed Facility: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.InventoryItem(), map[string]any{
		"item_id": item.ID, "facility_id": facility.ID,
		"qty_on_hand": 5, "qty_available_to_promise": 5,
	}, actor); err != nil {
		t.Fatalf("seed InventoryItem: %v", err)
	}
	openPO, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-E2E-OPEN", "vendor_id": vendor.ID, "order_date": "2026-07-20",
		"status_id": approvedID,
	}, actor)
	if err != nil {
		t.Fatalf("seed open PO: %v", err)
	}
	// unit_price is FieldMoney now (uc-infra#136): 150 minor units = $1.50.
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": openPO.ID, "item_id": item.ID, "qty": 20, "unit_price": 150,
	}, actor); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}
	// Position 5 + 20 = 25 <= 30 -> the signal fires.
	if _, err := engine.Create(ctx, purchasing.ReorderRule(), map[string]any{
		"item_id": item.ID, "reorder_point": 30, "safety_stock": 0,
		"target_lead_time_confidence": "p90",
	}, actor); err != nil {
		t.Fatalf("seed ReorderRule: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	for _, want := range []string{
		"Supplier Lead Times",
		"Reorder Signals",
		"Report Vendor Co", // the vendor's lead-time row
		"10.8",             // P90 from the two completed POs
		"E2E Reorder Widget",
		"Order now — expect ~10.8 days",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}

	// The signal row's item links to the real Item form — clicking is the
	// buyer's next step, so the anchor must resolve, not 404.
	var href string
	var ok bool
	if err := chromedp.Run(bctx,
		chromedp.AttributeValue(`a[href="/forms/Item/`+item.ID+`"]`, "href", &href, &ok, chromedp.ByQuery),
	); err != nil || !ok {
		t.Fatalf("signal row's item link missing (err=%v ok=%v)", err, ok)
	}
}

// TestPurchasingReport_OnTimeDeliverySection_RealBrowser (#11): the
// third completed-PO-derived section — "On-Time Delivery" — renders in
// a real headless Chrome alongside the two #30 sections it sits next to
// on the same page. The internal/api tests already pin the exact row
// markup/percentage math; this proves the page a buyer actually loads
// carries the section through the real server + browser stack, same
// reasoning as TestPurchasingReport_LeadTimeAndReorderSections_RealBrowser.
func TestPurchasingReport_OnTimeDeliverySection_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Promise Co", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	receivedID := publishedStatusID(t, tenantDB, "purchase_order_status", "received")

	// Two on-time (received on/before promise), one late -> 2/3 = 66.7%.
	for _, po := range []struct{ number, ordered, promised, received string }{
		{"PO-OT-E2E-1", "2026-07-01", "2026-07-10", "2026-07-08"},
		{"PO-OT-E2E-2", "2026-07-02", "2026-07-12", "2026-07-12"},
		{"PO-OT-E2E-3", "2026-07-03", "2026-07-11", "2026-07-15"},
	} {
		if _, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
			"po_number": po.number, "vendor_id": vendor.ID, "order_date": po.ordered,
			"promised_delivery_date": po.promised, "status_id": receivedID, "received_at": po.received,
		}, actor); err != nil {
			t.Fatalf("seed completed PO %s: %v", po.number, err)
		}
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	for _, want := range []string{
		"On-Time Delivery",
		"Promise Co",
		"66.7%",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}
}

// TestPurchasingReport_QualitySection_RealBrowser (uc-infra#82): the
// fourth completed-receipt-derived section — "Quality" — renders in a
// real headless Chrome alongside the sections it sits next to on the
// same page. The internal/api tests already pin the exact row markup/
// percentage math; this proves the page a buyer actually loads carries
// the section through the real server + browser stack, AND that a real
// GoodsReceiptLine write with qty_accepted/qty_rejected set actually
// passes purchasing.validateGoodsReceiptLineQuality's write-time hook
// (crud.Engine.Create, not a raw RecordRepo seed) — the internal/api
// tests bypass that hook entirely, so this is the only layer proving the
// whole real write -> read -> render path works end to end.
func TestPurchasingReport_QualitySection_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	engine.SetHook("GoodsReceiptLine", purchasing.PostGoodsReceiptLineToLedger)

	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Inspected Co", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	draftID := publishedStatusID(t, tenantDB, "purchase_order_status", "draft")

	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-Q-E2E-1", "vendor_id": vendor.ID, "order_date": "2026-07-01", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-Q-E2E", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	// unit_price 0 deliberately: PostGoodsReceiptLineToLedger's
	// zero-value-line short-circuit (its own doc comment) means this
	// exercises validateGoodsReceiptLineQuality without also needing a
	// seeded chart of accounts (1300/2100) just to post a ledger entry
	// this test isn't about.
	poLine, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": float64(10), "unit_price": float64(0),
	}, actor)
	if err != nil {
		t.Fatalf("seed POLine: %v", err)
	}
	// facility_id became Required on GoodsReceipt with uc-infra#54, which
	// landed in parallel with this test — a receipt now has to say where
	// the goods physically arrived. The receiving facility is incidental
	// to what this test asserts (the Quality report section), but it has
	// to exist for the seed to validate at all.
	qFacility, err := engine.Create(ctx, purchasing.Facility(), map[string]any{
		"code": "E2E-Q-MAIN", "name": "E2E Quality Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed Facility: %v", err)
	}
	gr, err := engine.Create(ctx, purchasing.GoodsReceipt(), map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05", "facility_id": qFacility.ID,
	}, actor)
	if err != nil {
		t.Fatalf("seed GoodsReceipt: %v", err)
	}
	// Two lines, each 9 accepted / 1 rejected -> N=2 (>= MinSamples, a
	// single line would show "Insufficient history" instead of a rate),
	// 18/20 = 90%. Written through the real crud.Engine path so
	// validateGoodsReceiptLineQuality's netting invariant (9 + 1 ==
	// qty_received 10) is actually exercised, not just asserted against
	// a raw seeded record.
	for i := 0; i < 2; i++ {
		if _, err := engine.Create(ctx, purchasing.GoodsReceiptLine(), map[string]any{
			"goods_receipt_id": gr.ID, "po_line_id": poLine.ID, "item_id": item.ID,
			"qty_received": float64(10), "qty_accepted": float64(9), "qty_rejected": float64(1),
		}, actor); err != nil {
			t.Fatalf("seed GoodsReceiptLine with quality data: %v", err)
		}
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	for _, want := range []string{
		"Quality", // the section heading — genuinely pins its presence
		// now that no seeded name ("Inspected Co"/"Widget") contains
		// "Quality" itself; an earlier draft named the vendor "Quality
		// Co", which made this assertion pass even with the section
		// deleted (independent review of uc-infra#82).
		"Inspected Co",
		"90%",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}
}

// e2eSeedPurchasingLeakFixture seeds one Item + InventoryItem + ReorderRule
// tuned so a single fixture exercises all three uc-infra#230 leak sites at
// once (Stock Summary card, Stockout Risk table, Reorder Signals table):
// qty_on_hand/qty_available_to_promise are fractional (not bare integers)
// specifically so the "does the real number leak anywhere in the raw DOM"
// scans below can't produce a false negative by coincidentally matching a
// hex run inside one of the seeded records' own random UUIDs — a bare
// integer like "42" is a plausible hex substring, but "42.5" never is
// (UUIDs are hex digits and hyphens only, no '.'), the same reason
// TestProjectBudgetReport_PlannedRedacted_RealBrowser's own leak scan uses
// "500.00" rather than "500". qty_available_to_promise -7.5 (<=0) puts the
// item on the Stockout Risk table; reorder_point 100 (well above any
// realistic position) guarantees the reorder signal fires regardless of
// which field is hidden from the browser's own actor.
func e2eSeedPurchasingLeakFixture(t *testing.T, ctx context.Context, engine *crud.Engine, actor audit.Actor) (itemID string) {
	t.Helper()
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-LEAK", "name": "E2E Leak Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	facility, err := engine.Create(ctx, purchasing.Facility(), map[string]any{
		"code": "E2E-LEAK-MAIN", "name": "E2E Leak Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed Facility: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.InventoryItem(), map[string]any{
		"item_id": item.ID, "facility_id": facility.ID,
		"qty_on_hand": 42.5, "qty_available_to_promise": -7.5,
	}, actor); err != nil {
		t.Fatalf("seed InventoryItem: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.ReorderRule(), map[string]any{
		"item_id": item.ID, "reorder_point": 100, "safety_stock": 0,
		"target_lead_time_confidence": "p90",
	}, actor); err != nil {
		t.Fatalf("seed ReorderRule: %v", err)
	}
	return item.ID
}

// TestPurchasingReport_OnHandRedacted_RealBrowser (uc-infra#230) is the
// real-browser proof that hiding InventoryItem.qty_on_hand via a
// FieldPermission removes the real quantity from the live DOM entirely —
// not merely from a rendered-HTML-string assertion (internal/api's own
// HTTP-level TestAPI_PurchasingReport_HiddenInventoryQuantitiesRenderNotAvailable) —
// same class of proof TestProjectBudgetReport_PlannedRedacted_RealBrowser
// already gives for a different entity/field. ReportingRepo's raw SQL
// reads qty_on_hand entirely outside the ts.crud-redacted path, so
// nothing short of an actual browser DOM scan proves the real number
// never reaches the page.
func TestPurchasingReport_OnHandRedacted_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	// Hide InventoryItem.qty_on_hand from the browser's own actor BEFORE
	// seeding data, same ordering the project-budget redaction e2e tests
	// use.
	seedFieldPermission(t, tenantDB, "e2e_onhand_redacted", "InventoryItem", "qty_on_hand")

	engine := crud.NewEngine(tenantDB)
	e2eSeedPurchasingLeakFixture(t, ctx, engine, actor)

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`.uc-report-cards`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	if strings.Contains(bodyText, "42.5") {
		t.Errorf("the real qty_on_hand (42.5) reached the live page for an actor with it redacted:\n%s", bodyText)
	}
	// qty_available_to_promise is a DIFFERENT field, not hidden by this
	// actor's role — its real number, and the Stockout Risk row/count it
	// drives, must still reach the page.
	if !strings.Contains(bodyText, "-7.5") {
		t.Errorf("qty_available_to_promise (a field this actor CAN see) should still render its real number:\n%s", bodyText)
	}
	if !strings.Contains(bodyText, "Not available") {
		t.Errorf("expected at least one 'Not available' placeholder (Stock Summary On Hand card, Stockout Risk On Hand column, Reorder Signals On Hand/Position columns):\n%s", bodyText)
	}
	if !strings.Contains(bodyText, "E2E Leak Widget") {
		t.Errorf("expected the reorder signal to still fire (position math is unaffected by display redaction):\n%s", bodyText)
	}

	// Belt-and-braces, same reasoning as the project-budget redaction
	// e2e tests' own leak check: confirm the figure isn't sitting
	// anywhere in the document outside the visible text either.
	var leaks bool
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.outerHTML.includes("42.5")`, &leaks,
	)); err != nil {
		t.Fatalf("scan document for the redacted on-hand quantity: %v", err)
	}
	if leaks {
		t.Fatal("redacted qty_on_hand reached the browser somewhere in the document")
	}
}

// TestPurchasingReport_AvailableToPromiseRedacted_RealBrowser
// (uc-infra#230) is the qty_available_to_promise counterpart to
// TestPurchasingReport_OnHandRedacted_RealBrowser above — and the one
// that actually proves the harder fix: StockoutRiskItems' row membership
// and StockSummary's StockoutCount are both computed server-side from the
// real, un-redacted qty_available_to_promise, so this test also confirms
// the Stockout Risk section renders as a whole "not available" state —
// no rows, no count — rather than merely blanking the ATP cell of a row
// whose very presence would otherwise still disclose "this item's real
// ATP is <= 0" (the gap an earlier version of this fix left open, caught
// by independent review before this test existed).
func TestPurchasingReport_AvailableToPromiseRedacted_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	seedFieldPermission(t, tenantDB, "e2e_atp_redacted", "InventoryItem", "qty_available_to_promise")

	engine := crud.NewEngine(tenantDB)
	e2eSeedPurchasingLeakFixture(t, ctx, engine, actor)

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/purchasing"),
		chromedp.WaitVisible(`.uc-report-cards`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/purchasing: %v", err)
	}

	if strings.Contains(bodyText, "-7.5") {
		t.Errorf("the real qty_available_to_promise (-7.5) reached the live page for an actor with it redacted:\n%s", bodyText)
	}
	if strings.Contains(bodyText, "SKU-E2E-LEAK") {
		t.Errorf("the Stockout Risk row must not render at all when qty_available_to_promise is hidden (its membership itself is derived from the hidden field), but the SKU appears:\n%s", bodyText)
	}
	if strings.Contains(bodyText, "Stockout Risk (") {
		t.Errorf("the Stockout Risk heading must not show a count derived from the hidden field:\n%s", bodyText)
	}
	// qty_on_hand is a DIFFERENT field, not hidden by this actor's role —
	// its real number must still render, including in the Reorder
	// Signals row (unaffected by ATP being hidden).
	if !strings.Contains(bodyText, "42.5") {
		t.Errorf("qty_on_hand (a field this actor CAN see) should still render its real number:\n%s", bodyText)
	}

	var leaks bool
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.outerHTML.includes("-7.5")`, &leaks,
	)); err != nil {
		t.Fatalf("scan document for the redacted available-to-promise quantity: %v", err)
	}
	if leaks {
		t.Fatal("redacted qty_available_to_promise reached the browser somewhere in the document")
	}
}
