// Real-browser proof for uc-infra#243: the "help" family screenshot
// (captured/budgeted/staleness-gated by the other files in this
// package) is now actually embedded in a real, shipped topic — the
// manual documenting itself — and the manual's own "?" affordance
// (next to its own <h1>) is a real, enabled link to it. Same shape as
// help_real_content_test.go's own proofs for entity/Party and
// entity/Role, applied to this card's new route/help topic instead.
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestHelp_RealRouteHelpContent_RendersInViewer_RealBrowser proves the
// real shipped route/help topic — not fixture content — renders through
// /help/route/help in a real browser for both en (LTR) and ar (RTL),
// with real translated Arabic prose (not a silent fallback to en), the
// correct per-locale image src, and the embedded screenshot actually
// LOADING (naturalWidth > 0) — a broken/404'ing src still produces an
// <img> element with the right src/alt attributes, so a markup-only
// assertion can't catch that (CLAUDE.md's own warning against
// exactly this gap; same proof shape as
// TestHelp_TopicImageRendersWithAltText_RealBrowser, applied to this
// card's real content instead of that test's fixture topic).
func TestHelp_RealRouteHelpContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := realContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	cases := []struct {
		locale, wantSrc, wantBody, dir string
	}{
		{"en", "/help/assets/help/en.jpg", "search box", "ltr"},
		{"ar", "/help/assets/help/ar.jpg", "مربع البحث", "rtl"},
	}
	for _, c := range cases {
		t.Run(c.locale, func(t *testing.T) {
			const imgSel = `#uc-help-detail img.uc-help-image`
			var bodyText, dir, imgSrc string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(srvURL+"/help/route/help?lang="+c.locale),
				chromedp.WaitVisible(imgSel, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
				chromedp.AttributeValue(`html`, `dir`, &dir, nil),
				chromedp.AttributeValue(imgSel, `src`, &imgSrc, nil),
			); err != nil {
				t.Fatalf("%s: navigate /help/route/help and inspect: %v", c.locale, err)
			}
			if dir != c.dir {
				t.Errorf("%s: dir = %q, want %q", c.locale, dir, c.dir)
			}
			if !strings.Contains(bodyText, c.wantBody) {
				t.Errorf("%s: expected real route/help content %q in rendered page, got body text:\n%s", c.locale, c.wantBody, bodyText)
			}
			if imgSrc != c.wantSrc {
				t.Errorf("%s: img src = %q, want %q", c.locale, imgSrc, c.wantSrc)
			}

			// The image load completing is async relative to the DOM
			// insertion WaitVisible already confirmed — poll
			// naturalWidth rather than reading it immediately, same
			// reasoning TestHelp_TopicImageRendersWithAltText_RealBrowser
			// already documents.
			if err := chromedp.Run(ctx, chromedp.Poll(
				`document.querySelector('`+imgSel+`').naturalWidth > 0`,
				nil, chromedp.WithPollingTimeout(5*time.Second),
			)); err != nil {
				t.Fatalf("%s: image never finished loading (naturalWidth stayed 0): %v", c.locale, err)
			}
		})
	}

	// ar must not have silently fallen back to the en body — a real
	// word-for-word substring of the real en/route/help.md body ("You
	// can also open it directly") sitting in the ar render would mean
	// help.Index.resolve's en-fallback kicked in instead of the real
	// Arabic translation, same check
	// TestHelp_RealFoundationContent_RendersInViewer_RealBrowser already
	// makes for entity/Role.
	var arBodyText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/help/route/help?lang=ar"),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &arBodyText),
	); err != nil {
		t.Fatalf("navigate /help/route/help?lang=ar: %v", err)
	}
	if strings.Contains(arBodyText, "You can also open it directly") {
		t.Error("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
	}
}

// TestHelp_OwnAffordance_EnabledAndNavigable_RealBrowser proves the
// manual's own "?" affordance (previously nonexistent — the index page
// had no <h1> affordance of its own before this card) is present,
// enabled, and actually lands on the real route/help topic when
// clicked — a genuine browser navigation, not just an href-string
// assertion (same proof shape as
// TestHelp_RealFoundationContent_AffordanceNowEnabled_RealBrowser for
// entity/Party's own affordance on /forms/Party/new).
//
// The click-through check asserts on #uc-help-detail's own text, not
// document.body.innerText (independent review) — route/help's title is
// ALSO rendered in the left-pane topic list on every /help page
// (buildHelpTopicListData, never authz-filtered for a route/ id), so a
// body-wide substring check for the topic's title would already be
// satisfied by the pre-click page and could never fail even if the
// click navigated nowhere. Checking the detail pane specifically for
// body prose that exists only there ("search box", from the topic's
// own "Finding a topic" section) actually proves the navigation
// happened.
func TestHelp_OwnAffordance_EnabledAndNavigable_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srvURL, tenantID := realContentTestServer(t)
	ctx := browserCtx(t, tenantID)

	var disabled bool
	var href string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srvURL+"/help"),
		chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
		),
		chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
	); err != nil {
		t.Fatalf("navigate /help and inspect its own affordance: %v", err)
	}
	if disabled {
		t.Error("expected the manual's own \"?\" affordance to be enabled now that route/help has real content, got aria-disabled=\"true\"")
	}
	if href != "/help/route/help" {
		t.Errorf("expected the affordance href to be /help/route/help, got %q", href)
	}

	var detailText string
	if err := chromedp.Run(ctx,
		chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').innerText`, &detailText),
	); err != nil {
		t.Fatalf("click the manual's own affordance and land on its topic: %v", err)
	}
	if !strings.Contains(detailText, "search box") {
		t.Errorf("expected clicking the affordance to land on the real route/help topic content (detail-pane-only prose), got detail pane text:\n%s", detailText)
	}
}
