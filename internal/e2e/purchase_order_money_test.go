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

// TestPurchaseOrderForm_RollUpTotalRendersAsMoneyDecimal_RealBrowser
// (uc-infra#136) is this migration's own real-browser regression pin,
// mirroring rfq_report_test.go's own "assert against real DOM text, not
// a rendered-HTML-string match" discipline (CLAUDE.md): PurchaseOrder.
// total/POLine.unit_price/line_total are FieldMoney now (minor units
// stored, e.g. 2000 == $20.00), and PurchaseOrderForm's Lines section is
// the FIRST FieldMoney field in this kernel to be a form RollUpTarget —
// formrender/rollup.go's computeRollUp and render.go's RollUpTotal
// formatting were both, until this migration, only ever exercised
// against a plain FieldNumber roll-up. Without the money-aware sum and
// display fix, the roll-up paragraph would show the raw summed minor-
// units integer ("3000") instead of the major-unit decimal a human
// reads as money ("30.00").
func TestPurchaseOrderForm_RollUpTotalRendersAsMoneyDecimal_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Money RollUp Vendor", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-MONEY-ROLLUP", "name": "Money RollUp Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	statusID := publishedStatusID(t, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-MONEY-ROLLUP-1", "vendor_id": vendor.ID, "order_date": "2026-08-01", "status_id": statusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	// Two lines summing to 3000 minor units ($30.00) — a value that would
	// print differently as a raw integer ("3000") than as the correct
	// major-unit decimal ("30.00"), so this genuinely distinguishes the
	// two behaviors rather than coincidentally matching either way.
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 2, "unit_price": 1000, "line_total": 2000,
	}, actor); err != nil {
		t.Fatalf("seed POLine 1: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 1, "unit_price": 1000, "line_total": 1000,
	}, actor); err != nil {
		t.Fatalf("seed POLine 2: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var rollUpText, lineTotalText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/"+po.ID),
		chromedp.WaitVisible(`table.uc-master-detail`, chromedp.ByQuery),
		chromedp.Text(`p.uc-rollup[data-field="total"]`, &rollUpText, chromedp.ByQuery),
		chromedp.Text(`table.uc-master-detail tbody tr:first-child td[data-field="line_total"]`, &lineTotalText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /forms/PurchaseOrder/%s: %v", po.ID, err)
	}

	if !strings.Contains(rollUpText, "30.00") {
		t.Fatalf("expected the roll-up total to render as the major-unit decimal 30.00, got %q", rollUpText)
	}
	if strings.Contains(rollUpText, "3000") {
		t.Fatalf("roll-up total rendered the raw minor-units integer instead of a money decimal: %q", rollUpText)
	}
	// The child row's own line_total cell (childCellValue's FieldMoney
	// case, uc-infra#68) must agree with what it's summed into — both
	// read "20.00", not "2000".
	if !strings.Contains(lineTotalText, "20.00") {
		t.Fatalf("expected the first line's line_total cell to render as 20.00, got %q", lineTotalText)
	}

	// The header's own visible "total" field (a plain ComponentFields
	// input, fed by the same roll-up computation) must show the same
	// major-unit decimal in its value attribute.
	var totalInputValue string
	if err := chromedp.Run(bctx, chromedp.Value(`input#total`, &totalInputValue, chromedp.ByQuery)); err != nil {
		t.Fatalf("read total input value: %v", err)
	}
	if totalInputValue != "30.00" {
		t.Fatalf("expected the header total input to show 30.00, got %q", totalInputValue)
	}
}

// TestPurchaseOrderForm_MoneyChildCellAndRollUpAreRegionallyFormatted_RealBrowser
// (uc-infra#166 independent review, finding 2) is the real-browser layer
// this card's fix was otherwise missing entirely: a FieldMoney
// master-detail child cell (childCellValue's own FieldMoney case) and its
// money-typed roll-up total (both fixed by the same commit — the roll-up
// fold-in was formerly filed separately as uc-infra#189) must actually
// render regionally grouped in a real Chromium DOM under a Turkish
// region, not just in a rendered-HTML-string test. Uses a whole-dollar
// amount over 999.99 (uc-infra#166 independent review, finding 1): a
// wrong "natural precision" (-1) FormatNumber call would print the
// shorter "1.234.567" with no decimals, distinguishable here from the
// correct fixed-precision "1.234.567,00".
func TestPurchaseOrderForm_MoneyChildCellAndRollUpAreRegionallyFormatted_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Regional Money RollUp Vendor", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-MONEY-ROLLUP-REGIONAL", "name": "Regional Money RollUp Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	statusID := publishedStatusID(t, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-MONEY-ROLLUP-REGIONAL-1", "vendor_id": vendor.ID, "order_date": "2026-08-01", "status_id": statusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	// One line, 123456700 minor units == $1,234,567.00 exactly — large
	// enough to require Turkish thousands grouping, and a whole-dollar
	// amount so a wrong -1 precision (no decimals) is distinguishable
	// from the correct fixed money.Decimals (always ".00").
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 1, "unit_price": 123456700, "line_total": 123456700,
	}, actor); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var rollUpText, lineTotalText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/"+po.ID+"?lang=tr&region=tr-TR"),
		chromedp.WaitVisible(`table.uc-master-detail`, chromedp.ByQuery),
		chromedp.Text(`p.uc-rollup[data-field="total"]`, &rollUpText, chromedp.ByQuery),
		chromedp.Text(`table.uc-master-detail tbody tr:first-child td[data-field="line_total"]`, &lineTotalText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /forms/PurchaseOrder/%s?lang=tr&region=tr-TR: %v", po.ID, err)
	}

	// Turkish groups thousands with a dot, decimals with a comma:
	// 1234567.00 -> "1.234.567,00".
	if !strings.Contains(lineTotalText, "1.234.567,00") {
		t.Fatalf("expected the child cell to render the Turkish-grouped amount 1.234.567,00, got %q", lineTotalText)
	}
	if strings.Contains(lineTotalText, "1.234.567<") || strings.TrimSpace(lineTotalText) == "1.234.567" {
		t.Fatalf("child cell rendered the natural-precision amount (missing ,00) instead of the fixed-precision one, got %q", lineTotalText)
	}
	if !strings.Contains(rollUpText, "1.234.567,00") {
		t.Fatalf("expected the roll-up total to render the SAME Turkish-grouped amount as its own child cell (1.234.567,00), got %q", rollUpText)
	}
	if strings.TrimSpace(rollUpText) == "Toplam: 1.234.567" {
		t.Fatalf("roll-up total rendered the natural-precision amount (missing ,00) instead of the fixed-precision one, got %q", rollUpText)
	}
}
