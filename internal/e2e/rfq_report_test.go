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
	// Y, rendered as a blank cell, not a fabricated zero. unit_price is
	// FieldMoney now (minor units, uc-infra#68): 950 == $9.50.
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorX.ID, "unit_price": 950,
	}, actor); err != nil {
		t.Fatalf("seed quote lineA/vendorX: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorY.ID, "unit_price": 1025,
	}, actor); err != nil {
		t.Fatalf("seed quote lineA/vendorY: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineB.ID, "vendor_id": vendorX.ID, "unit_price": 400,
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
		"9.50", "10.25", "4.00",
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

	// The real DOM/CSS proof: the cheapest cell on lineA (vendor X's 9.50)
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

// TestRFQComparisonReport_HiddenItemPartyFields_RealBrowser (uc-infra#234)
// is the real-browser proof that hiding Item.name/Party.name via a
// FieldPermission removes each real value from the live DOM entirely —
// not merely from a rendered-HTML-string assertion (internal/api's own
// TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable, which
// already pins per-field independence one field at a time). Same class
// of proof purchasing_report_test.go's
// TestPurchasingReport_HiddenPurchaseOrderPartyItemFields_RealBrowser
// gives for the equivalent purchasing-report fix — data.ReportingRepo.
// RFQComparison's raw SQL reads both fields entirely outside the
// ts.crud-redacted path, so nothing short of an actual browser DOM scan
// proves the real values never reach the page.
//
// Both fields are hidden from the SAME browser actor at once here
// (browserCtx always authenticates as the one fixed e2eActorID): this
// test's job is to prove the real DOM never leaks either, together, not
// to re-litigate their independence (already covered at the HTTP level).
func TestRFQComparisonReport_HiddenItemPartyFields_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	// ONE role holding both FieldPermission rows, not two single-field
	// roles granted to the same actor — authz.Resolver.HiddenFields' own
	// contract (a field is hidden only when EVERY role the actor holds
	// has a hidden=true row for it) would hide NEITHER field with two
	// single-field roles, the exact gotcha
	// TestPurchasingReport_HiddenPurchaseOrderPartyItemFields_RealBrowser's
	// own comment documents hitting on its first run.
	roleID := grantE2ERole(t, tenantDB, "e2e_234_all_redacted")
	engine := crud.NewEngine(tenantDB)
	for _, fp := range []struct{ entityType, field string }{
		{"Item", "name"}, {"Party", "name"},
	} {
		if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
			"role_id": roleID, "entity_type": fp.entityType, "field_name": fp.field, "hidden": true,
		}, actor); err != nil {
			t.Fatalf("create FieldPermission %s.%s: %v", fp.entityType, fp.field, err)
		}
	}

	draftID := publishedStatusID(t, tenantDB, "rfq_status", "draft")
	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-E2E-LEAK234", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed RequestForQuotation: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-LEAK234", "name": "E2E Leak Widget 234", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	line, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": item.ID, "qty": 10.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Leak Vendor Co 234", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendor.ID,
	}, actor); err != nil {
		t.Fatalf("invite vendor: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": line.ID, "vendor_id": vendor.ID, "unit_price": 950,
	}, actor); err != nil {
		t.Fatalf("seed quote line: %v", err)
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

	for _, leaked := range []string{"E2E Leak Widget 234", "E2E Leak Vendor Co 234"} {
		if strings.Contains(bodyText, leaked) {
			t.Errorf("redacted value %q reached the live page for an actor with it hidden:\n%s", leaked, bodyText)
		}
	}
	if !strings.Contains(bodyText, "Not available") {
		t.Errorf("expected at least one 'Not available' placeholder (row label, vendor column header):\n%s", bodyText)
	}
	// The real quoted price (a different field, not hidden by this
	// actor's role) must still render — redacting the labels must not
	// also blank the price grid itself.
	if !strings.Contains(bodyText, "9.50") {
		t.Errorf("expected the real quoted price (9.50) — a field this actor CAN see — to still render:\n%s", bodyText)
	}

	// Belt-and-braces, same reasoning
	// TestPurchasingReport_HiddenPurchaseOrderPartyItemFields_RealBrowser's
	// own leak check already uses — confirm neither value is sitting
	// anywhere in the document outside the visible text either.
	for _, leaked := range []string{"E2E Leak Widget 234", "E2E Leak Vendor Co 234"} {
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

// TestRFQComparisonReport_HiddenUnitPrice_RealBrowser (uc-infra#235) is
// the real-browser proof for the field #234's own review found and left
// open: RequestForQuotationQuoteLine.unit_price. Unlike Item.name/
// Party.name, unit_price also drives a real computed style
// (.uc-rfq-lowest) — proving the redaction removes the VALUE from the
// live DOM is not enough; this also has to prove the lowest-price mark
// itself never renders, since that mark is derived information about the
// hidden price (which cell is cheapest), the same class of proof
// TestRFQComparisonReport_RealBrowser already gives for the mark existing
// and resolving a real background color when unit_price is NOT hidden.
func TestRFQComparisonReport_HiddenUnitPrice_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	roleID := grantE2ERole(t, tenantDB, "e2e_235_price_redacted")
	engine := crud.NewEngine(tenantDB)
	if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
		"role_id": roleID, "entity_type": "RequestForQuotationQuoteLine", "field_name": "unit_price", "hidden": true,
	}, actor); err != nil {
		t.Fatalf("create FieldPermission RequestForQuotationQuoteLine.unit_price: %v", err)
	}

	draftID := publishedStatusID(t, tenantDB, "rfq_status", "draft")
	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-E2E-LEAK235", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed RequestForQuotation: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-LEAK235", "name": "E2E Leak Widget 235", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	line, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": item.ID, "qty": 10.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed line: %v", err)
	}
	vendorX, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Leak Vendor X 235", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor X: %v", err)
	}
	vendorY, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Leak Vendor Y 235", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor Y: %v", err)
	}
	for _, v := range []string{vendorX.ID, vendorY.ID} {
		if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
			"request_for_quotation_id": rfq.ID, "vendor_id": v,
		}, actor); err != nil {
			t.Fatalf("invite vendor %s: %v", v, err)
		}
	}
	// Both vendors quote — vendor X cheaper, so an UNredacted actor would
	// see it marked lowest. This actor must see neither price nor mark.
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": line.ID, "vendor_id": vendorX.ID, "unit_price": 950,
	}, actor); err != nil {
		t.Fatalf("seed quote line X: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": line.ID, "vendor_id": vendorY.ID, "unit_price": 1200,
	}, actor); err != nil {
		t.Fatalf("seed quote line Y: %v", err)
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

	for _, leaked := range []string{"9.50", "12.00"} {
		if strings.Contains(bodyText, leaked) {
			t.Errorf("redacted unit_price %q reached the live page for an actor with it hidden:\n%s", leaked, bodyText)
		}
	}
	if !strings.Contains(bodyText, "Not available") {
		t.Errorf("expected at least one 'Not available' placeholder for the hidden quoted prices:\n%s", bodyText)
	}
	// A different field (Item.name/Party.name) this actor CAN see — the
	// redaction must not blank fields unrelated to unit_price.
	for _, want := range []string{"E2E Leak Widget 235", "E2E Leak Vendor X 235", "E2E Leak Vendor Y 235"} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("expected %q (a different field) to still render:\n%s", want, bodyText)
		}
	}

	// The real DOM/CSS proof: the lowest-price mark is derived from the
	// hidden price, so it must not render AT ALL for this actor — not
	// just have its cell text blanked. querySelectorAll returning 0 is
	// the only way to prove the mark itself never made it into the DOM,
	// the same "class name in the markup" gap the unredacted browser test
	// closes for the positive case.
	var lowestCellCount int
	if err := chromedp.Run(bctx,
		chromedp.Evaluate(`document.querySelectorAll('.uc-rfq-lowest').length`, &lowestCellCount),
	); err != nil {
		t.Fatalf("inspect lowest-price cell count: %v", err)
	}
	if lowestCellCount != 0 {
		t.Errorf("expected 0 .uc-rfq-lowest cells when unit_price is redacted, got %d", lowestCellCount)
	}

	// Belt-and-braces outerHTML scan, same reasoning
	// TestRFQComparisonReport_HiddenItemPartyFields_RealBrowser's own leak
	// check already uses.
	for _, leaked := range []string{"9.50", "12.00"} {
		var leaks bool
		if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
			`document.documentElement.outerHTML.includes("`+leaked+`")`, &leaks,
		)); err != nil {
			t.Fatalf("scan document for %q: %v", leaked, err)
		}
		if leaks {
			t.Errorf("redacted unit_price %q reached the browser somewhere in the document", leaked)
		}
	}
}
