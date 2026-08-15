// Real-browser proof that uc-infra#151's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144) for a real
// projects+assets tenant, and that the "?" affordance (uc-infra#143)
// actually flips from disabled to a real link now that content exists
// for these entity types. Mirrors help_real_content_sales_finance_test.go's
// own proof exactly, one locale pair (en/fa) across one representative
// entity per module rather than every one of the seven this card
// shipped — the coverage_test.go allowlist shrink is what actually
// proves every entity/locale pair exists; this file's job is only
// proving the real content renders through a real browser for real, not
// re-deriving that count a second time.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/projects"
)

// projectsAssetsContentTestServer is testServer (csv_import_test.go)
// plus projects.Publish/PublishForms/PublishStatuses and
// assets.Publish/PublishForms/PublishStatuses — testServer only calls
// foundation.Publish and purchasing.Publish/PublishForms, neither of
// which is enough for /forms/Project/new or /help/entity/FixedAsset to
// resolve. PublishStatuses is needed for both modules here (Project/Task
// and FixedAsset/MaintenanceOrder all carry a StatusTypeCode field).
func projectsAssetsContentTestServer(t *testing.T) (srvURL string, tenantID string) {
	t.Helper()
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()
	if err := projects.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.PublishForms: %v", err)
	}
	if err := projects.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.PublishStatuses: %v", err)
	}
	if err := assets.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("assets.Publish: %v", err)
	}
	if err := assets.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("assets.PublishForms: %v", err)
	}
	if err := assets.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("assets.PublishStatuses: %v", err)
	}
	return srv.URL, tenantID
}

// TestHelp_RealProjectsAssetsContent_RendersInViewer_RealBrowser proves
// the real shipped Project topic (en) and FixedAsset topic (fa) — not
// fixture content — render through /help/{topicID} in a real browser,
// with the real translated Farsi prose actually present (not a silent
// fallback to en) and the page correctly right-to-left for fa.
func TestHelp_RealProjectsAssetsContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := projectsAssetsContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: Project topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Project?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Project?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"start date, an optional end date",
			"own budget",
			"Neither section rolls up into a total",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Project (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("fa: FixedAsset topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/FixedAsset?lang=fa"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/FixedAsset?lang=fa: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for fa, got %q`, dir)
		}
		// The real Farsi title ("دارایی ثابت") and a real Farsi-only
		// phrase from the body — not a phrase that also happens to
		// appear in the English body — so this fails on a silent en
		// fallback the same way help_real_content_test.go's own Role(ar)
		// case guards against.
		for _, want := range []string{"دارایی ثابت", "شما ردیف‌ها را دستی اضافه نمی‌کنید"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real FixedAsset (fa) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/entity/
		// FixedAsset.md body — if the fa render silently fell back to en
		// (help.Index.resolve does fall back to en when a locale's file
		// is missing/blank), this English phrase would be sitting right
		// in the rendered body text.
		if strings.Contains(bodyText, "Saving the asset generates this section automatically") {
			t.Errorf("fa render appears to have fallen back to the English body text instead of the real Farsi translation")
		}
	})
}

// TestHelp_RealProjectsAssetsContent_AffordanceNowEnabled_RealBrowser
// proves the "?" affordance on a real projects-entity form page —
// disabled by construction before this card — is now a real, enabled
// link once uc-infra#151 ships content for that entity type, and that
// following it lands on the real topic.
func TestHelp_RealProjectsAssetsContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := projectsAssetsContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/forms/Project/new"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /forms/Project/new and inspect affordance: %v", err)
	}
	if disabled {
		t.Error("expected the \"?\" affordance on /forms/Project/new to be enabled now that entity/Project has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/entity/Project" {
		t.Errorf("expected the affordance href to be /help/entity/Project, got %q", href)
	}

	// Follow it for real — a genuine browser navigation, not just an
	// href-string assertion, lands on the real Project topic.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("click affordance and land on topic: %v", err)
	}
	if !strings.Contains(bodyText, "own budget") {
		t.Errorf("expected clicking the affordance to land on the real Project topic content, got body text:\n%s", bodyText)
	}
}
