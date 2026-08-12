// Real-browser proof that uc-infra#149's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144) for a real
// sales+finance tenant, and that the "?" affordance (uc-infra#143)
// actually flips from disabled to a real link now that content exists
// for these entity types. Mirrors help_real_content_test.go's own
// foundation-module proof (uc-infra#147) exactly, one locale pair
// (en/ar) across one representative entity per module rather than every
// one of the eight this card shipped — the coverage_test.go allowlist
// shrink is what actually proves every entity/locale pair exists; this
// file's job is only proving the real content renders through a real
// browser for real, not re-deriving that count a second time.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/sales"
)

// salesFinanceContentTestServer is testServer (csv_import_test.go) plus
// sales.Publish/PublishForms/PublishStatuses and finance.Publish/
// PublishForms — testServer only calls foundation.Publish and
// purchasing.Publish/PublishForms, neither of which is enough for
// /forms/SalesOrder/new or /help/entity/Account to resolve (same
// Publish/PublishForms gap realContentTestServer's own doc comment
// already covers for the foundation module). PublishStatuses is needed
// for sales specifically (SalesOrder/CustomerInvoice's StatusTypeCode
// fields), matching TestUBLDownloadLink_RealBrowser's identical call
// sequence; finance has no PublishStatuses counterpart (finance.go's own
// doc comment: neither of its entities opted into the foundation
// Status/StatusType pattern yet).
func salesFinanceContentTestServer(t *testing.T) (srvURL string, tenantID string) {
	t.Helper()
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()
	if err := sales.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}
	if err := finance.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.PublishForms: %v", err)
	}
	return srv.URL, tenantID
}

// TestHelp_RealSalesFinanceContent_RendersInViewer_RealBrowser proves the
// real shipped SalesOrder topic (en) and Account topic (ar) — not
// fixture content — render through /help/{topicID} in a real browser,
// with the real translated Arabic prose actually present (not a silent
// fallback to en) and the page correctly right-to-left for ar.
func TestHelp_RealSalesFinanceContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := salesFinanceContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: SalesOrder topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/SalesOrder?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/SalesOrder?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"Sales Order",
			"committed order from a customer",
			"Line Total is not calculated for you",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real SalesOrder (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("ar: Account topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Account?lang=ar"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Account?lang=ar: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for ar, got %q`, dir)
		}
		// The real Arabic title ("الحساب") and a real Arabic-only phrase
		// from the body ("شجرة حساباتك", "your chart of accounts") —
		// not "4100", which also appears verbatim in the English body
		// and so would pass even on a silent en fallback. Same "no
		// silent fallback" proof help_real_content_test.go's own
		// Role(ar) case already makes.
		for _, want := range []string{"الحساب", "شجرة حساباتك"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Account (ar) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/entity/Account.md
		// body — if the ar render silently fell back to en
		// (help.Index.resolve does fall back to en when a locale's file
		// is missing/blank), this English phrase would be sitting right
		// in the rendered body text.
		if strings.Contains(bodyText, "one line in your chart of accounts") {
			t.Errorf("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
		}
	})
}

// TestHelp_RealSalesFinanceContent_AffordanceNowEnabled_RealBrowser proves
// the "?" affordance on a real sales-entity form page — disabled by
// construction before this card (mirrors uc-infra#143's own
// TestHelpAffordance_RealBrowser proof, deliberately against a still-
// undocumented entity so it stays true after this card lands) — is now
// a real, enabled link once uc-infra#149 ships content for that entity
// type, and that following it lands on the real topic.
func TestHelp_RealSalesFinanceContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := salesFinanceContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/forms/SalesOrder/new"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /forms/SalesOrder/new and inspect affordance: %v", err)
	}
	if disabled {
		t.Error("expected the \"?\" affordance on /forms/SalesOrder/new to be enabled now that entity/SalesOrder has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/entity/SalesOrder" {
		t.Errorf("expected the affordance href to be /help/entity/SalesOrder, got %q", href)
	}

	// Follow it for real — a genuine browser navigation, not just an
	// href-string assertion, lands on the real SalesOrder topic.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("click affordance and land on topic: %v", err)
	}
	if !strings.Contains(bodyText, "committed order from a customer") {
		t.Errorf("expected clicking the affordance to land on the real SalesOrder topic content, got body text:\n%s", bodyText)
	}
}
