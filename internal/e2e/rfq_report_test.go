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

// TestRFQComparisonReport_RealBrowser (#9) mirrors
// purchasing_report_test.go's own structure: seed a real RFQ with lines,
// invited vendors, and quote lines through the real server, drive
// chromedp to the real /reports/rfq/{id} URL, and assert the rendered
// table's actual text content AND a real computed style via the DOM —
// not a string match on the handler's own return value, per CLAUDE.md's
// explicit rule that a rendered-HTML-string test never substitutes for
// a real browser test on a rendered page (the cheapest-price marking is
// exactly the kind of "does the CSS actually apply" question a
// string-match test cannot answer).
func TestRFQComparisonReport_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// testServer publishes purchasing entities/forms but not the status
	// graph — RequestForQuotation.status_id needs a real Status record to
	// point at (idempotent, same call cmd/provision-tenant makes).
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	draftID := publishedStatusID(t, tenantDB, "rfq_status", "draft")

	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-E2E-1", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed RequestForQuotation: %v", err)
	}

	itemA, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-RFQ-A", "name": "E2E Widget A", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item A: %v", err)
	}
	itemB, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-RFQ-B", "name": "E2E Widget B", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item B: %v", err)
	}
	lineA, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemA.ID, "qty": 10.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed line A: %v", err)
	}
	lineB, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemB.ID, "qty": 5.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed line B: %v", err)
	}

	vendorX, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Vendor X", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor X: %v", err)
	}
	vendorY, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Vendor Y", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor Y: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorX.ID,
	}, actor); err != nil {
		t.Fatalf("invite vendor X: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorY.ID,
	}, actor); err != nil {
		t.Fatalf("invite vendor Y: %v", err)
	}

	// lineA: vendor X quotes cheaper (9.50 < 10.25) — the cheapest-price
	// mark this test asserts against as a real computed style. lineB:
	// only vendor X quoted — the deliberate missing-quote gap for vendor
	// Y, rendered as a blank cell, not a fabricated zero.
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorX.ID, "unit_price": 9.5,
	}, actor); err != nil {
		t.Fatalf("seed quote lineA/vendorX: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorY.ID, "unit_price": 10.25,
	}, actor); err != nil {
		t.Fatalf("seed quote lineA/vendorY: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineB.ID, "vendor_id": vendorX.ID, "unit_price": 4.0,
	}, actor); err != nil {
		t.Fatalf("seed quote lineB/vendorX: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/rfq/"+rfq.ID),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/rfq/%s: %v", rfq.ID, err)
	}

	for _, want := range []string{
		"E2E Widget A", "E2E Widget B", "E2E Vendor X", "E2E Vendor Y",
		"9.5", "10.25", "4",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}
	// The genuinely missing quote (vendor Y never quoted lineB) must
	// render as the localized placeholder, visible text a real reader
	// would see — never a fabricated "0".
	if !strings.Contains(bodyText, "—") {
		t.Errorf("report page missing the missing-quote placeholder; body text:\n%s", bodyText)
	}

	// The real DOM/CSS proof: the cheapest cell on lineA (vendor X's 9.5)
	// must carry the .uc-rfq-lowest class AND that class must actually
	// resolve to a real, non-default computed background — proving the
	// stylesheet is really loaded and applied, not just that the class
	// name landed in the markup (exactly the gap a rendered-HTML-string
	// test can't close, per CLAUDE.md).
	var lowestCellCount int
	var lowestBg, plainBg string
	if err := chromedp.Run(bctx,
		chromedp.Evaluate(`document.querySelectorAll('.uc-rfq-lowest').length`, &lowestCellCount),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('.uc-rfq-lowest')).backgroundColor`, &lowestBg),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('table.uc-table thead th')).backgroundColor`, &plainBg),
	); err != nil {
		t.Fatalf("inspect lowest-price cell styling: %v", err)
	}
	// lineA's one cheapest cell + lineB's sole (therefore also "lowest")
	// quote = 2 marked cells.
	if lowestCellCount != 2 {
		t.Errorf("expected 2 .uc-rfq-lowest cells, got %d", lowestCellCount)
	}
	if lowestBg == "" || lowestBg == plainBg {
		t.Errorf("expected .uc-rfq-lowest to resolve a real, distinct background color, got %q (header background %q)", lowestBg, plainBg)
	}

	// The vendor-name column headers must NOT inherit table.uc-table th's
	// blanket text-transform: uppercase — a vendor name is a proper noun
	// (often a registered brand), and defacing it is a real, visible
	// defect a rendered-HTML-string test cannot catch at all: uppercasing
	// happens purely in the computed style, textContent is identical
	// either way (independent review, 2026-07-31 — the .uc-rfq-vendor-col
	// opt-out in app.css shipped untested). Asserted against the plain
	// fixed-label header on the same table, which must still uppercase.
	var vendorThTransform, plainThTransform string
	if err := chromedp.Run(bctx,
		chromedp.Evaluate(`getComputedStyle(document.querySelector('th.uc-rfq-vendor-col')).textTransform`, &vendorThTransform),
		chromedp.Evaluate(`getComputedStyle(document.querySelectorAll('table.uc-table thead th')[0]).textTransform`, &plainThTransform),
	); err != nil {
		t.Fatalf("inspect vendor column header casing: %v", err)
	}
	if vendorThTransform != "none" {
		t.Errorf("vendor column <th> text-transform = %q, want \"none\" (a real vendor name must not be uppercased)", vendorThTransform)
	}
	if plainThTransform != "uppercase" {
		t.Errorf("plain fixed-label <th> text-transform = %q, want \"uppercase\" — the opt-out must be scoped to vendor columns only, not disable the global rule", plainThTransform)
	}

	// The header's own rfq_number/due_date render too — a buyer reading
	// this report needs to know which RFQ they're looking at.
	if !strings.Contains(bodyText, "RFQ-E2E-1") {
		t.Errorf("report page missing the RFQ number; body text:\n%s", bodyText)
	}
}
