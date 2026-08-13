// Real-browser proof that uc-infra#148's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144) for a real
// purchasing tenant, and that the "?" affordance (uc-infra#143) actually
// flips from disabled to a real link now that content exists for these
// entity types. Mirrors help_real_content_projects_assets_test.go's own
// proof exactly, one locale pair (en/ar) across one representative
// entity rather than every one of the fourteen this card shipped — the
// coverage_test.go allowlist shrink (now empty — this was the last of
// uc-infra#147-152's module slices) is what actually proves every
// entity/locale pair exists; this file's job is only proving the real
// content renders through a real browser for real, not re-deriving that
// count a second time.
package e2e

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestHelp_RealPurchasingContent_RendersInViewer_RealBrowser proves the
// real shipped PurchaseOrder topic (en) and VendorInvoice topic (ar) —
// not fixture content — render through /help/{topicID} in a real
// browser, with the real translated Arabic prose actually present (not
// a silent fallback to en) and the page correctly right-to-left for ar.
//
// testServer (csv_import_test.go) already publishes purchasing entities
// and forms by default — unlike the projects/assets and sales/finance
// content tests, no extra Publish/PublishForms call is needed here, and
// PublishStatuses isn't needed either: the help viewer and the "?"
// affordance only need the entity/form Definition to resolve, not a
// seeded status graph.
func TestHelp_RealPurchasingContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: PurchaseOrder topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/entity/PurchaseOrder?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/PurchaseOrder?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"buy-side counterpart to a Sales Order",
			"Line Total is not calculated for you",
			"Marking a Purchase Order Received here is just a status change",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real PurchaseOrder (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("ar: VendorInvoice topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/entity/VendorInvoice?lang=ar"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/VendorInvoice?lang=ar: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for ar, got %q`, dir)
		}
		// The real Arabic title and a real Arabic-only phrase from the
		// body — not a phrase that also happens to appear in the English
		// body — so this fails on a silent en fallback the same way
		// help_real_content_test.go's own Role(ar) case guards against.
		for _, want := range []string{"فاتورة المورد", "لا يوجد مسار مباشر"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real VendorInvoice (ar) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/entity/
		// VendorInvoice.md body — if the ar render silently fell back to
		// en (help.Index.resolve does fall back to en when a locale's
		// file is missing/blank), this English phrase would be sitting
		// right in the rendered body text.
		if strings.Contains(bodyText, "There is no direct path from Match Exception to Paid") {
			t.Errorf("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
		}
	})
}

// TestHelp_RealPurchasingContent_AffordanceNowEnabled_RealBrowser proves
// the "?" affordance on a real purchasing-entity form page — disabled by
// construction before this card — is now a real, enabled link once
// uc-infra#148 ships content for that entity type, and that following it
// lands on the real topic.
func TestHelp_RealPurchasingContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/new"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /forms/PurchaseOrder/new and inspect affordance: %v", err)
	}
	if disabled {
		t.Error("expected the \"?\" affordance on /forms/PurchaseOrder/new to be enabled now that entity/PurchaseOrder has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/entity/PurchaseOrder" {
		t.Errorf("expected the affordance href to be /help/entity/PurchaseOrder, got %q", href)
	}

	// Follow it for real — a genuine browser navigation, not just an
	// href-string assertion, lands on the real PurchaseOrder topic.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("click affordance and land on topic: %v", err)
	}
	if !strings.Contains(bodyText, "Line Total is not calculated for you") {
		t.Errorf("expected clicking the affordance to land on the real PurchaseOrder topic content, got body text:\n%s", bodyText)
	}
}
