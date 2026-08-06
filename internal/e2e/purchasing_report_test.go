package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

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
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": openPO.ID, "item_id": item.ID, "qty": 20, "unit_price": 1.5,
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
		"name": "Quality Co", "party_type": "organization", "status": "active",
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
		"sku": "SKU-Q-E2E", "name": "Quality Widget", "item_type": "stock",
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
	gr, err := engine.Create(ctx, purchasing.GoodsReceipt(), map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
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
		"Quality",
		"Quality Co",
		"90%",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}
}
