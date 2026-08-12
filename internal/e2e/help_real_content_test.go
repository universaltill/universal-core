// Real-browser proof that uc-infra#147's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144) for a real
// foundation-module tenant, and that the "?" affordance (uc-infra#143)
// actually flips from disabled to a real link now that content exists
// for these entity types. Nothing else in this package exercises the
// real embedded FS end to end: every other Help* test in help_test.go
// deliberately overrides help.GetIndex with a small in-memory fixture
// (see that file's own package doc comment on why), which is the right
// choice for proving the viewer's mechanics in isolation, but leaves the
// actual shipped content itself unverified against a real browser until
// this file.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// realContentTestServer is testServer (csv_import_test.go) plus
// foundation.PublishForms — testServer only calls foundation.Publish,
// which is enough for the entity Definitions themselves but not for
// /forms/{entityType}/new to resolve (see cmd/provision-tenant's own doc
// comment on the Publish/PublishForms gap, uc-infra#83's review record).
// Deliberately does NOT call help.SetIndexForTesting, unlike
// testHelpServer (help_test.go) — the whole point of this file is to hit
// the real embedded content.
func realContentTestServer(t *testing.T) (srvURL string, tenantID string) {
	t.Helper()
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	return srv.URL, tenantID
}

// TestHelp_RealFoundationContent_RendersInViewer_RealBrowser proves the
// real shipped Party topic (en) and Role topic (ar) — not fixture
// content — render through /help/{topicID} in a real browser, with the
// real translated Arabic prose actually present (not a silent fallback
// to en) and the page correctly right-to-left for ar.
func TestHelp_RealFoundationContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := realContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: Party topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Party?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Party?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"Party",
			"single record for anyone or anything",
			"Party Role",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Party (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("ar: Role topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srvURL+"/help/entity/Role?lang=ar"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/entity/Role?lang=ar: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for ar, got %q`, dir)
		}
		// The real Arabic title ("الدور") and a real sentence from the
		// body, not the English title standing in for a missing
		// translation (the same "no silent fallback" check
		// TestHelp_MultiLocaleRender_RealBrowser already makes for
		// fixture content — this is the same proof for real content).
		for _, want := range []string{"الدور", "tenant_admin"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real Role (ar) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/entity/Role.md
		// body ("an access-control role you define") — if the ar render
		// silently fell back to en (help.Index.resolve does fall back to
		// en when a locale's file is missing/blank), this English phrase
		// would be sitting right in the rendered body text.
		if strings.Contains(bodyText, "access-control role") {
			t.Errorf("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
		}
	})
}

// TestHelp_RealFoundationContent_AffordanceNowEnabled_RealBrowser proves
// the "?" affordance on a real foundation-entity form page — disabled by
// construction before this card (uc-infra#143's own
// TestHelpAffordance_RealBrowser asserts exactly that, deliberately
// against the still-undocumented Item entity so it stays true after this
// card lands) — is now a real, enabled link once uc-infra#147 ships
// content for that entity type, and that following it lands on the real
// topic.
func TestHelp_RealFoundationContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := realContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/forms/Party/new"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /forms/Party/new and inspect affordance: %v", err)
	}
	if disabled {
		t.Error("expected the \"?\" affordance on /forms/Party/new to be enabled now that entity/Party has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/entity/Party" {
		t.Errorf("expected the affordance href to be /help/entity/Party, got %q", href)
	}

	// Follow it for real — a genuine browser navigation, not just an
	// href-string assertion, lands on the real Party topic.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("click affordance and land on topic: %v", err)
	}
	if !strings.Contains(bodyText, "single record for anyone or anything") {
		t.Errorf("expected clicking the affordance to land on the real Party topic content, got body text:\n%s", bodyText)
	}
}
