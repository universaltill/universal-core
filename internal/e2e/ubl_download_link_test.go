package e2e

import (
	"context"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// TestUBLDownloadLink_RealBrowser is the real-browser layer for
// uc-infra#66 — the independent review of #27 found the shipped UBL
// export (GET /export/{entityType}/{id}/ubl) had no way to actually
// reach it from the application. internal/api's own tests already pin
// the rendered markup (a rendered-HTML-string test proves structure,
// never that a real browser actually offers a clickable, working link —
// CLAUDE.md's own e2e rule); this drives a real headless Chrome to an
// existing PurchaseOrder's record form, finds the real anchor element in
// the real DOM, and has the same browser session actually fetch what
// that link points at — proving the affordance and the export it
// reaches both work together, not just each in isolation.
func TestUBLDownloadLink_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// testServer publishes foundation + purchasing's entities/forms but
	// no status graph (purchasing_report_test.go's own PublishStatuses
	// call notes the same gap) and no sales at all — the
	// SalesOrder/CustomerInvoice half of this card needs both published
	// (idempotent against the fresh tenant, same calls
	// cmd/provision-tenant makes for a real tenant with both modules).
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	if err := sales.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Demo Organization Supplier", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	customer, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Demo Organization Customer", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": customer.ID, "role_type": "customer",
	}, actor); err != nil {
		t.Fatalf("seed customer PartyRole: %v", err)
	}
	currency, err := engine.Create(ctx, foundation.Currency(), map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": 2.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed currency: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-E2E-UBL", "name": "E2E UBL Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}

	poDraft := publishedStatusID(t, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-E2E-UBL-1", "vendor_id": vendor.ID, "order_date": "2026-08-01",
		"currency_id": currency.ID, "status_id": poDraft, "total": 100.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 10.0, "unit_price": 10.0, "line_total": 100.0,
	}, actor); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	soDraft := publishedStatusID(t, tenantDB, "sales_order_status", "draft")
	so, err := engine.Create(ctx, sales.SalesOrder(), map[string]any{
		"so_number": "SO-E2E-UBL-1", "customer_id": customer.ID, "order_date": "2026-08-01",
		"currency_id": currency.ID, "status_id": soDraft, "total": 200.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed SalesOrder: %v", err)
	}
	if _, err := engine.Create(ctx, sales.SOLine(), map[string]any{
		"sales_order_id": so.ID, "item_id": item.ID, "qty": 20.0, "unit_price": 10.0, "line_total": 200.0,
	}, actor); err != nil {
		t.Fatalf("seed SOLine: %v", err)
	}

	invoiceDraft := publishedStatusID(t, tenantDB, "customer_invoice_status", "draft")
	invoice, err := engine.Create(ctx, sales.CustomerInvoice(), map[string]any{
		"invoice_number": "INV-E2E-UBL-1", "sales_order_id": so.ID, "customer_id": customer.ID,
		"invoice_date": "2026-08-01", "currency_id": currency.ID, "status_id": invoiceDraft, "total": 200.0,
	}, actor)
	if err != nil {
		t.Fatalf("seed CustomerInvoice: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	// 1. Each of the three record pages renders a real, visible anchor
	// pointing at its own export — real computed styles, not a markup
	// string, same trap CLAUDE.md's e2e rule (and the SAF-T export's own
	// e2e test) guards against.
	for _, tc := range []struct {
		name       string
		entityType string
		id         string
	}{
		{"PurchaseOrder", "PurchaseOrder", po.ID},
		{"SalesOrder", "SalesOrder", so.ID},
		{"CustomerInvoice", "CustomerInvoice", invoice.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantHref := "/export/" + tc.entityType + "/" + tc.id + "/ubl"
			selector := `a[href="` + wantHref + `"]`
			var linkText string
			var style struct {
				Visible bool    `json:"visible"`
				Cursor  string  `json:"cursor"`
				Width   float64 `json:"width"`
			}
			if err := chromedp.Run(bctx,
				chromedp.Navigate(srv.URL+"/forms/"+tc.entityType+"/"+tc.id),
				chromedp.WaitVisible(selector, chromedp.ByQuery),
				chromedp.Text(selector, &linkText, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(
					`(function(){
						var a = document.querySelector(`+"`"+selector+"`"+`);
						var r = a.getBoundingClientRect();
						var cs = getComputedStyle(a);
						return { visible: r.width > 0 && r.height > 0, cursor: cs.cursor, width: r.width };
					})()`,
					&style),
			); err != nil {
				t.Fatalf("%s form page: %v", tc.entityType, err)
			}
			if strings.TrimSpace(linkText) != "Download UBL file" {
				t.Fatalf("expected the anchor's real text content to read \"Download UBL file\", got %q", linkText)
			}
			if !style.Visible || style.Width <= 0 {
				t.Fatalf("download link has no rendered size — not actually visible/clickable: %+v", style)
			}
		})
	}

	// 2. The link on the PurchaseOrder page actually fetches a real UBL
	// document — the same browser session, the same request a click
	// would issue, proving the affordance and the export it reaches work
	// together end to end (not just each independently unit-tested).
	var result struct {
		Status      int    `json:"status"`
		ContentType string `json:"contentType"`
		Body        string `json:"body"`
	}
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/"+po.ID),
		chromedp.Evaluate(`
			fetch('/export/PurchaseOrder/`+po.ID+`/ubl')
				.then(function(r){ return r.text().then(function(t){ return {status: r.status, contentType: r.headers.get('content-type'), body: t}; }); })
		`, &result, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
	); err != nil {
		t.Fatalf("fetch UBL document from the browser: %v", err)
	}
	if result.Status != 200 {
		t.Fatalf("expected 200 from the browser's own fetch of the download link's href, got %d: %.300s", result.Status, result.Body)
	}
	if !strings.Contains(result.ContentType, "application/xml") {
		t.Fatalf("expected an XML content type, got %q", result.ContentType)
	}
	for _, want := range []string{"PO-E2E-UBL-1", "Demo Organization Supplier", "SKU-E2E-UBL"} {
		if !strings.Contains(result.Body, want) {
			t.Fatalf("downloaded UBL document missing %q:\n%.500s", want, result.Body)
		}
	}
}
