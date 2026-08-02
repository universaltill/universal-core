package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestRFQCompareQuotesLink_RealBrowser is the real-browser layer for
// uc-infra#69 — the independent review of #9 found the shipped RFQ
// vendor comparison report (GET /reports/rfq/{id}) had no way to
// actually reach it from the application. internal/api's own tests
// already pin the rendered markup (a rendered-HTML-string test proves
// structure, never that a real browser actually offers a clickable,
// working link — CLAUDE.md's own e2e rule); this drives a real headless
// Chrome to an existing RequestForQuotation's record form, finds the
// real anchor element in the real DOM, clicks it, and confirms the
// browser actually lands on the report page — proving the affordance and
// the report it reaches both work together, same pattern as
// TestUBLDownloadLink_RealBrowser (uc-infra#66).
func TestRFQCompareQuotesLink_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	draftID := publishedStatusID(t, tenantDB, "rfq_status", "draft")

	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-E2E-LINK-1", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed RequestForQuotation: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	// 1. The record page renders a real, visible anchor pointing at the
	// report — real computed styles, not a markup string.
	wantHref := "/reports/rfq/" + rfq.ID
	selector := `a[href="` + wantHref + `"]`
	var linkText string
	var style struct {
		Visible bool    `json:"visible"`
		Width   float64 `json:"width"`
	}
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/RequestForQuotation/"+rfq.ID),
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Text(selector, &linkText, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`(function(){
				var a = document.querySelector(`+"`"+selector+"`"+`);
				var r = a.getBoundingClientRect();
				return { visible: r.width > 0 && r.height > 0, width: r.width };
			})()`,
			&style),
	); err != nil {
		t.Fatalf("RequestForQuotation form page: %v", err)
	}
	if strings.TrimSpace(linkText) != "Compare Quotes" {
		t.Fatalf("expected the anchor's real text content to read \"Compare Quotes\", got %q", linkText)
	}
	if !style.Visible || style.Width <= 0 {
		t.Fatalf("Compare Quotes link has no rendered size — not actually visible/clickable: %+v", style)
	}

	// 2. Clicking it actually navigates the same browser session to the
	// real report page — proving the affordance and the report it reaches
	// work together end to end, not just each in isolation.
	var landedURL, bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitVisible(`body`, chromedp.ByQuery),
		chromedp.Location(&landedURL),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click Compare Quotes link: %v", err)
	}
	if !strings.HasSuffix(landedURL, wantHref) {
		t.Fatalf("expected the browser to land on %q, got %q", wantHref, landedURL)
	}
	if !strings.Contains(bodyText, "RFQ-E2E-LINK-1") {
		t.Errorf("landed report page missing the RFQ number; body text:\n%s", bodyText)
	}
}
