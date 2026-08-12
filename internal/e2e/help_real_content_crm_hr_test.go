// Real-browser proof that uc-infra#150's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144) for a real
// crm+hr tenant, and that the "?" affordance (uc-infra#143) actually
// flips from disabled to a real link now that content exists for these
// entity types. Mirrors help_real_content_sales_finance_test.go's own
// dual-module proof (uc-infra#149) exactly, one locale pair (en/ar)
// across one representative entity per module rather than every one of
// the seven this card shipped — the coverage_test.go allowlist shrink is
// what actually proves every entity/locale pair exists; this file's job
// is only proving the real content renders through a real browser for
// real, not re-deriving that count a second time.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/hr"
)

// crmHRContentTestServer is testServer (csv_import_test.go) plus
// crm.Publish/PublishForms/PublishStatuses and hr.Publish/PublishForms/
// PublishStatuses — testServer only calls foundation.Publish and
// purchasing.Publish/PublishForms, neither of which is enough for
// /forms/Lead/new or /help/entity/Employee to resolve (same Publish/
// PublishForms gap realContentTestServer's own doc comment already
// covers for the foundation module). PublishStatuses is needed for both
// modules here: every one of crm's four entities and both of hr's
// status-typed entities (Employee, LeaveRequest) opted into the
// StatusType/Status/StatusTransition graph from day one, unlike finance
// (salesFinanceContentTestServer's own doc comment notes finance has no
// PublishStatuses counterpart).
func crmHRContentTestServer(t *testing.T) (srvURL string, tenantID string) {
	t.Helper()
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()
	if err := crm.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("crm.Publish: %v", err)
	}
	if err := crm.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("crm.PublishForms: %v", err)
	}
	if err := crm.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("crm.PublishStatuses: %v", err)
	}
	if err := hr.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("hr.Publish: %v", err)
	}
	if err := hr.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("hr.PublishForms: %v", err)
	}
	if err := hr.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("hr.PublishStatuses: %v", err)
	}
	return srv.URL, tenantID
}

// TestHelp_RealCRMHRContent_RendersInViewer_RealBrowser proves the real
// shipped Lead topic (en) and Employee topic (ar) — not fixture content
// — render through /help/{topicID} in a real browser, with the real
// translated Arabic prose actually present (not a silent fallback to en)
// and the page correctly right-to-left for ar.
func TestHelp_RealCRMHRContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := crmHRContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: Lead topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Lead?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Lead?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"Lead",
			"unqualified prospect",
			"Converted To",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Lead (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("ar: Employee topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Employee?lang=ar"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Employee?lang=ar: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for ar, got %q`, dir)
		}
		// The real Arabic title ("موظف") and a real Arabic-only phrase
		// from the body — not anything that also appears verbatim in the
		// English body, so this wouldn't pass on a silent en fallback.
		// Same "no silent fallback" proof the foundation/sales-finance
		// files' own ar cases already make.
		for _, want := range []string{"موظف", "سجل الإجازات"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Employee (ar) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/entity/Employee.md
		// body — if the ar render silently fell back to en (help.Index.
		// resolve does fall back to en when a locale's file is missing/
		// blank), this English phrase would be sitting right in the
		// rendered body text.
		if strings.Contains(bodyText, "one employment — not a person") {
			t.Errorf("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
		}
	})
}

// TestHelp_RealCRMHRContent_AffordanceNowEnabled_RealBrowser proves the
// "?" affordance on a real crm-entity form page — disabled by
// construction before this card (mirrors uc-infra#143's own
// TestHelpAffordance_RealBrowser proof, deliberately against a still-
// undocumented entity so it stays true after this card lands) — is now a
// real, enabled link once uc-infra#150 ships content for that entity
// type, and that following it lands on the real topic.
func TestHelp_RealCRMHRContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := crmHRContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/forms/Lead/new"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /forms/Lead/new and inspect affordance: %v", err)
	}
	if disabled {
		t.Error("expected the \"?\" affordance on /forms/Lead/new to be enabled now that entity/Lead has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/entity/Lead" {
		t.Errorf("expected the affordance href to be /help/entity/Lead, got %q", href)
	}

	// Follow it for real — a genuine browser navigation, not just an
	// href-string assertion, lands on the real Lead topic.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("click affordance and land on topic: %v", err)
	}
	if !strings.Contains(bodyText, "unqualified prospect") {
		t.Errorf("expected clicking the affordance to land on the real Lead topic content, got body text:\n%s", bodyText)
	}
}
