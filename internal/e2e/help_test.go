// The real-browser layer for the in-product help manual (ADR-0023 in
// uc-infra, uc-infra#144 — slice 2 of uc-infra#141, on top of slice 1's
// uc-infra#143/help_affordance_test.go). internal/api/help_test.go and
// internal/help's own unit tests already prove the rendered markup and
// the Index's search/authz/locale-fallback logic in Go; what only a real
// browser can prove is: the CSS-driven two-pane geometry actually
// mirrors under RTL, a real keyup-driven htmx round trip actually
// narrows the visible list, and clicking a topic link is a real browser
// navigation whose URL/back-forward behavior is native, not simulated.
//
// This file's own topics are never the real shipped content — every
// fixture topic here is small, genuine-looking placeholder content
// authored for this test, wired in via help.SetIndexForTesting
// (internal/help/index.go), which swaps out the real embedded
// internal/help/content/ index entirely for the duration of each test.
// That stays true regardless of how much real content has since shipped
// under internal/help/content/ itself (uc-infra#147-152's module
// slices, now all landed) — this file deliberately never depends on
// what's really there; see help_real_content_test.go and its per-module
// siblings for the tests that do.
package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/help"
)

// helpFixtureTopic is one topic's cross-locale content — enough to
// exercise the four shipped locales (en/tr/fa/ar), a real diacritic
// (Turkish dotted İ, folded by help.foldForSearch's NFKD strip), an
// authz-gated entity/ topic, a never-gated route/ topic, and an
// admin-audience topic, per uc-infra#144's own acceptance criteria.
type helpFixtureTopic struct {
	id       string
	module   string
	audience string
	order    int
	// title/body per locale; every locale gets a real (if short)
	// translated sentence, not a copy-pasted English string, so the
	// multi-locale render test can tell a genuine translation apart from
	// a silent fallback to en.
	title map[string]string
	body  map[string]string
}

// helpFixtureTopics is this file's whole fixture manual: one entity-typed
// topic (tied to the real "Item" entity type testServer's foundation +
// purchasing publish already seeds, so it's a real entity read access can
// be granted/denied on), two route-typed topics never authz-filtered (one
// of them admin-audience, one carrying the diacritic case), plus a second
// diacritic case in the entity topic's own fa translation.
var helpFixtureTopics = []helpFixtureTopic{
	{
		id:       "entity/Item",
		module:   "inventory",
		audience: "user",
		order:    1,
		title: map[string]string{
			"en": "Item Help",
			"tr": "Ürün Yardımı",
			"fa": "راهنمای کالا",
			"ar": "مساعدة الصنف",
		},
		body: map[string]string{
			"en": "How to manage warehouse items and track stock levels.",
			"tr": "Depo ürünlerini ve stok seviyelerini nasıl yöneteceğiniz.",
			"fa": "نحوهٔ مدیریت اقلام انبار و سطح موجودی.",
			"ar": "كيفية إدارة أصناف المستودع ومستويات المخزون.",
		},
	},
	{
		id:       "route/getting-started",
		module:   "general",
		audience: "user",
		order:    1,
		title: map[string]string{
			"en": "Getting Started",
			"tr": "Başlangıç",
			"fa": "شروع کار",
			"ar": "البدء",
		},
		// The body's second paragraph (uc-infra#145) is a real
		// ![alt](url) image reference to one of the 8 real captured
		// screenshots (internal/e2e's own TestCaptureHelpScreenshots,
		// checked in under internal/help/content/assets/) — the src is
		// intentionally the SAME file across every locale below (there is
		// only one "list" family en capture — see the issue's own
		// locale-policy trade-off), but each locale's ALT text is its own
		// real, translated sentence, not a copy-pasted English string,
		// matching this file's existing per-locale translation pattern.
		// TestHelp_TopicImageRendersWithAltText_RealBrowser is what
		// exercises this.
		body: map[string]string{
			"en": "Welcome to Universal Core. This guide walks new users through their first login.\n\n![Screenshot of the Item list page](/help/assets/list/en.jpg)",
			"tr": "Universal Core'a hoş geldiniz. Bu kılavuz yeni kullanıcılara ilk girişte yol gösterir.\n\n![Ürün liste sayfasının ekran görüntüsü](/help/assets/list/en.jpg)",
			"fa": "به یونیورسال کور خوش آمدید. این راهنما کاربران جدید را در اولین ورود راهنمایی می‌کند.\n\n![تصویری از صفحهٔ فهرست اقلام](/help/assets/list/en.jpg)",
			"ar": "مرحبًا بك في يونيفرسال كور. يوجّه هذا الدليل المستخدمين الجدد خلال أول تسجيل دخول.\n\n![لقطة شاشة لصفحة قائمة الأصناف](/help/assets/list/en.jpg)",
		},
	},
	{
		id:       "route/admin-guide",
		module:   "admin",
		audience: "admin",
		order:    1,
		title: map[string]string{
			"en": "Admin Configuration Guide",
			"tr": "Yönetici Yapılandırma Kılavuzu",
			"fa": "راهنمای پیکربندی مدیر",
			"ar": "دليل تهيئة المسؤول",
		},
		body: map[string]string{
			"en": "Configure tenant-wide settings as an administrator.",
			"tr": "Yönetici olarak kiracı çapında ayarları yapılandırın.",
			"fa": "تنظیمات سراسری مستأجر را به عنوان مدیر پیکربندی کنید.",
			"ar": "قم بتهيئة إعدادات المستأجر كمسؤول.",
		},
	},
	{
		// The diacritic fixture: en's own "Café" (a precomposed é) and
		// tr's own "İşlem" (U+0130, NFKD-decomposes into "I" + a
		// combining dot above) both fold to their plain-ASCII form under
		// help.foldForSearch — TestHelp_SearchNarrowing_RealBrowser types
		// the undiacritized query for real and expects a real match.
		id:       "route/cafe-guide",
		module:   "general",
		audience: "user",
		order:    2,
		title: map[string]string{
			"en": "Café Setup Guide",
			"tr": "İşlem Rehberi",
			"fa": "راهنمای کافه",
			"ar": "دليل إعداد المقهى",
		},
		body: map[string]string{
			"en": "How to configure your café point-of-sale workflow. Café settings live under Settings.",
			"tr": "Kafe veya restoran satış noktası iş akışınızı nasıl yapılandıracağınız. İşlem ayarları Ayarlar altında bulunur.",
			"fa": "نحوهٔ پیکربندی گردش‌کار نقطهٔ فروش کافهٔ یا رستوران شما. تنظیمات کافه در بخش تنظیمات قرار دارد.",
			"ar": "كيفية تهيئة سير عمل نقطة بيع المقهى أو المطعم الخاص بك. تُحفظ إعدادات المقهى ضمن الإعدادات.",
		},
	},
	{
		// The list-continuation fixture (uc-infra#207): an ordered list
		// whose second item wraps across two indented source lines, the
		// exact authoring convention used throughout the real shipped
		// manual (internal/help/content/en/entity/AIProviderConnection.md's
		// own numbered steps among them) that parseBlocks used to
		// mis-handle — independent review confirmed this fix changes the
		// rendered output of 157 of the 161 shipped content files (a
		// differential render of every file, not an indentation-heuristic
		// grep, which undercounts). See internal/help/markdown_test.go's
		// own unit coverage for the parser-level regression test. This
		// fixture's job is the one only a real browser can do: prove the
		// DOM a real client renders is one complete <ol> with the
		// wrapped item's full text in a single <li>, not markup-string
		// matching (already covered at the unit layer) and not two
		// lists split by a stray paragraph. Real translations in every
		// locale, same as every fixture above — TestHelp_MultiLocaleRender_RealBrowser asserts
		// each fixture topic's own translated title renders (and never
		// silently falls back to en) unconditionally across all four
		// locales; only this topic's en body is what
		// TestHelp_ListItemWrappedAcrossSourceLines_RealBrowser below
		// actually drives, since list-item line-folding is locale-
		// invariant logic and the other fixtures already cover per-locale
		// body translation/fallback behavior on their own.
		id:       "route/list-continuation-check",
		module:   "general",
		audience: "user",
		order:    3,
		title: map[string]string{
			"en": "List Continuation Check",
			"tr": "Liste Devamı Kontrolü",
			"fa": "بررسی ادامهٔ فهرست",
			"ar": "فحص متابعة القائمة",
		},
		body: map[string]string{
			"en": "1. Choose **Report an Issue** from the navigation bar.\n" +
				"2. Type a short title and a description of the problem. You can also\n" +
				"   record a voice note, which is transcribed automatically and appended\n" +
				"   to your description.\n" +
				"3. Submit the report.\n",
			"tr": "Bu konu, uzun bir liste öğesinin birden çok kaynak satırına sarılmasını test eder.",
			"fa": "این موضوع، پیچیده‌شدن یک مورد فهرست بلند را در چند خط منبع آزمایش می‌کند.",
			"ar": "يختبر هذا الموضوع التفاف عنصر قائمة طويل عبر عدة أسطر مصدر.",
		},
	},
}

// helpE2EFixtureIndex builds the real help.Index the tests in this file
// drive a real browser against, from an in-memory fstest.MapFS — content
// is a compile-time go:embed in production (internal/help/index.go's own
// doc comment on GetIndex), so a real running server can only be pointed
// at fixture content via help.SetIndexForTesting, never by writing files
// under internal/help/content/ itself.
func helpE2EFixtureIndex(t *testing.T) *help.Index {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, topic := range helpFixtureTopics {
		for _, locale := range []string{"en", "tr", "fa", "ar"} {
			title, ok := topic.title[locale]
			if !ok {
				continue
			}
			body := topic.body[locale]
			raw := "---\ntitle: " + title + "\naudience: " + topic.audience +
				"\nmodule: " + topic.module + "\norder: " + strconv.Itoa(topic.order) +
				"\n---\n# " + title + "\n\n" + body + "\n"
			fsys["content/"+locale+"/"+topic.id+".md"] = &fstest.MapFile{Data: []byte(raw)}
		}
	}
	idx, err := help.BuildIndexFrom(fsys)
	if err != nil {
		t.Fatalf("help.BuildIndexFrom fixture: %v", err)
	}
	return idx
}

// testHelpServer is testServer (csv_import_test.go) plus this file's
// fixture Index wired in via help.SetIndexForTesting, restored on
// cleanup so the override never leaks into a later test — the same
// helper shape internal/api/help_test.go's own withHelpFixtureIndex
// already establishes, reused here rather than duplicated per test
// function.
func testHelpServer(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	restore := help.SetIndexForTesting(helpE2EFixtureIndex(t))
	t.Cleanup(restore)
	return testServer(t)
}

// navigateBackAndWait drives one browser-back gesture at the CDP level —
// the same page.NavigateToHistoryEntry call chromedp.NavigateBack itself
// issues (see that function's own source in the chromedp module) — then
// polls wantExpr against the resulting page, instead of going through
// chromedp.NavigateBack/responseAction's network-lifecycle "page load"
// detection, which this file's own TestHelp_TopicSelectionAndBackForward_
// RealBrowser found does not reliably fire for a back navigation this
// browser serves from its back-forward cache (reproduced both via
// chromedp.NavigateBack directly and via an equivalent history.back() JS
// eval — both hung to the test's full context deadline on a repeat run).
// Retries a few times on "Cannot find context with specified id", the one
// transient error seen immediately after triggering the navigation, while
// the old page's JS execution context is still being torn down.
func navigateBackAndWait(ctx context.Context, wantExpr string) error {
	trigger := chromedp.ActionFunc(func(ctx context.Context) error {
		cur, entries, err := page.GetNavigationHistory().Do(ctx)
		if err != nil {
			return err
		}
		if cur <= 0 || cur > int64(len(entries)-1) {
			return fmt.Errorf("no previous navigation history entry (cur=%d, %d entries)", cur, len(entries))
		}
		return page.NavigateToHistoryEntry(entries[cur-1].ID).Do(ctx)
	})
	if err := chromedp.Run(ctx, trigger); err != nil {
		return fmt.Errorf("trigger back navigation: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = chromedp.Run(ctx, chromedp.Poll(wantExpr, nil, chromedp.WithPollingTimeout(2*time.Second)))
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// waitForHelpTopicCount polls #uc-help-topic-results for its current
// number of rendered .uc-help-topic rows — used to wait out the search
// box's hx-trigger="keyup changed delay:300ms" debounce deterministically
// instead of a fixed sleep.
func waitForHelpTopicCount(t *testing.T, ctx context.Context, want int) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Poll(
		`document.querySelectorAll('#uc-help-topic-results .uc-help-topic').length === `+strconv.Itoa(want),
		nil, chromedp.WithPollingTimeout(5*time.Second),
	)); err != nil {
		var got int
		_ = chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
			`document.querySelectorAll('#uc-help-topic-results .uc-help-topic').length`, &got,
		))
		t.Fatalf("waitForHelpTopicCount(%d): timed out (currently %d): %v", want, got, err)
	}
}

// TestHelp_MultiLocaleRender_RealBrowser (uc-infra#144 acceptance
// criterion 1): the manual renders with a topic tree, search box, and
// detail panel in all four shipped locales, with each locale showing its
// own translated topic titles — not a silent fallback to English.
func TestHelp_MultiLocaleRender_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	for _, locale := range []string{"en", "tr", "fa", "ar"} {
		t.Run(locale, func(t *testing.T) {
			var topicsPresent, searchPresent, detailPresent bool
			var bodyText string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(srv.URL+"/help?lang="+locale),
				chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-topics') !== null`, &topicsPresent),
				chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-search') !== null`, &searchPresent),
				chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail') !== null`, &detailPresent),
				chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			); err != nil {
				t.Fatalf("%s: navigate /help and inspect layout: %v", locale, err)
			}
			if !topicsPresent || !searchPresent || !detailPresent {
				t.Fatalf("%s: expected #uc-help-topics/#uc-help-search/#uc-help-detail all present, got topics=%v search=%v detail=%v", locale, topicsPresent, searchPresent, detailPresent)
			}
			for _, topic := range helpFixtureTopics {
				want := topic.title[locale]
				if !strings.Contains(bodyText, want) {
					t.Errorf("%s: expected the %s topic's own translated title %q rendered, got body text:\n%s", locale, topic.id, want, bodyText)
				}
			}
			// Not a silent fallback to en: the en title text (when
			// distinct from this locale's own) must not appear standing
			// in for the real translation.
			if locale != "en" {
				for _, topic := range helpFixtureTopics {
					enTitle := topic.title["en"]
					localTitle := topic.title[locale]
					if enTitle == localTitle {
						continue
					}
					if strings.Contains(bodyText, enTitle) {
						t.Errorf("%s: found the en title %q for %s — expected the %s translation %q instead (a silent fallback to en)", locale, enTitle, topic.id, locale, localTitle)
					}
				}
			}
		})
	}
}

// TestHelp_SearchNarrowing_RealBrowser (uc-infra#144 acceptance criteria
// 3+4): typing into #uc-help-search drives a real keyup-triggered htmx
// round trip that narrows #uc-help-topic-results to matching topics only,
// and clearing the box restores the full list — via chromedp.SendKeys
// (real keydown/keyup events), not chromedp.SetValue (which would not
// fire the "keyup" trigger help.go's hx-trigger listens on).
func TestHelp_SearchNarrowing_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help"),
		chromedp.WaitVisible(`#uc-help-search`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate /help: %v", err)
	}

	// Full list to start: 5 fixture topics, nothing filtered yet.
	waitForHelpTopicCount(t, ctx, len(helpFixtureTopics))

	// "warehouse" only appears in entity/Item's en body — narrows to 1.
	if err := chromedp.Run(ctx, chromedp.SendKeys(`#uc-help-search`, "warehouse", chromedp.ByQuery)); err != nil {
		t.Fatalf("type into #uc-help-search: %v", err)
	}
	waitForHelpTopicCount(t, ctx, 1)
	var narrowedText string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-topic-results').innerText`, &narrowedText)); err != nil {
		t.Fatalf("read narrowed results: %v", err)
	}
	if !strings.Contains(narrowedText, "Item Help") {
		t.Errorf("expected the narrowed results to contain the matching Item Help topic, got:\n%s", narrowedText)
	}
	for _, unwanted := range []string{"Getting Started", "Admin Configuration Guide", "Café Setup Guide"} {
		if strings.Contains(narrowedText, unwanted) {
			t.Errorf("expected the narrowed results to exclude %q, got:\n%s", unwanted, narrowedText)
		}
	}

	// Clearing the box (real Backspace keys, one per typed character —
	// each is its own keyup) restores the full list.
	if err := chromedp.Run(ctx, chromedp.SendKeys(`#uc-help-search`, strings.Repeat("\b", len("warehouse")), chromedp.ByQuery)); err != nil {
		t.Fatalf("clear #uc-help-search: %v", err)
	}
	waitForHelpTopicCount(t, ctx, len(helpFixtureTopics))

	// A query matching nothing narrows to the "no results" state, not an
	// error or a stale list.
	if err := chromedp.Run(ctx, chromedp.SendKeys(`#uc-help-search`, "zzz-nothing-matches", chromedp.ByQuery)); err != nil {
		t.Fatalf("type a non-matching query: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Poll(
		`document.querySelector('#uc-help-topic-results .uc-help-empty') !== null`,
		nil, chromedp.WithPollingTimeout(5*time.Second),
	)); err != nil {
		t.Fatalf("wait for the no-results state: %v", err)
	}
	waitForHelpTopicCount(t, ctx, 0)
}

// TestHelp_SearchNarrowing_DiacriticFold_RealBrowser drives the same real
// keyup round trip as above, but with an undiacritized query typed
// against locales whose own fixture title carries a real diacritic —
// en's "Café" and tr's "İşlem" — proving help.foldForSearch's NFKD fold
// (internal/help/index.go) actually reaches a real browser's live search,
// not just the Go-level unit tests already covering foldForSearch itself
// (internal/help/index_test.go).
func TestHelp_SearchNarrowing_DiacriticFold_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	cases := []struct {
		locale, query, wantTitle string
	}{
		{"en", "cafe", "Café Setup Guide"}, // "cafe" (plain) must still find "Café"
		{"tr", "islem", "İşlem Rehberi"},   // "islem" (plain) must still find "İşlem"
	}
	for _, c := range cases {
		t.Run(c.locale, func(t *testing.T) {
			if err := chromedp.Run(ctx,
				chromedp.Navigate(srv.URL+"/help?lang="+c.locale),
				chromedp.WaitVisible(`#uc-help-search`, chromedp.ByQuery),
				chromedp.SendKeys(`#uc-help-search`, c.query, chromedp.ByQuery),
			); err != nil {
				t.Fatalf("%s: navigate and type %q: %v", c.locale, c.query, err)
			}
			waitForHelpTopicCount(t, ctx, 1)
			var text string
			if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-topic-results').innerText`, &text)); err != nil {
				t.Fatalf("%s: read narrowed results: %v", c.locale, err)
			}
			if !strings.Contains(text, c.wantTitle) {
				t.Errorf("%s: query %q (undiacritized) expected to fold-match %q, got:\n%s", c.locale, c.query, c.wantTitle, text)
			}
		})
	}
}

// TestHelp_TopicSelectionAndBackForward_RealBrowser (uc-infra#144
// acceptance criterion 4): a topic link is a plain <a href> — real
// browser navigation, not htmx — so clicking one must swap
// #uc-help-detail's content, update the real browser URL
// (chromedp.Location), and support real back/forward.
func TestHelp_TopicSelectionAndBackForward_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help"),
		chromedp.WaitVisible(`a[data-help-topic="entity/Item"]`, chromedp.ByQuery),
		chromedp.Click(`a[data-help-topic="entity/Item"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail h2`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click the entity/Item topic: %v", err)
	}
	var loc, detail string
	if err := chromedp.Run(ctx,
		chromedp.Location(&loc),
		chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').innerText`, &detail),
	); err != nil {
		t.Fatalf("read location/detail after selecting entity/Item: %v", err)
	}
	if !strings.HasSuffix(loc, "/help/entity/Item") {
		t.Errorf("expected the URL to end in /help/entity/Item, got %q", loc)
	}
	if !strings.Contains(detail, "Item Help") || !strings.Contains(detail, "How to manage warehouse items") {
		t.Errorf("expected the Item topic's own content in #uc-help-detail, got:\n%s", detail)
	}

	// Select a second topic.
	if err := chromedp.Run(ctx,
		chromedp.Click(`a[data-help-topic="route/getting-started"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail h2`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click the route/getting-started topic: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Location(&loc),
		chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').innerText`, &detail),
	); err != nil {
		t.Fatalf("read location/detail after selecting route/getting-started: %v", err)
	}
	if !strings.HasSuffix(loc, "/help/route/getting-started") {
		t.Errorf("expected the URL to end in /help/route/getting-started, got %q", loc)
	}
	if !strings.Contains(detail, "Getting Started") {
		t.Errorf("expected the Getting Started topic's own content in #uc-help-detail, got:\n%s", detail)
	}

	// Real browser back restores the first topic — native history, since
	// selection is a plain <a href>, not a custom pushState/htmx swap.
	// chromedp.NavigateBack's own load-detection (chromedp.go's
	// responseAction, keyed off a network loaderID/"init" lifecycle
	// event) never resolves when Chrome serves the previous page straight
	// out of the back-forward cache rather than issuing a new network
	// request — verified by reproducing it, not assumed: with
	// chromedp.NavigateBack this test hangs to its 30s context deadline
	// every run against this package's real headless Chrome, and a
	// history.back() JS-eval equivalent was flaky the same way across
	// repeated runs. navigateBackAndWait below drives the identical
	// browser-level gesture (Page.navigateToHistoryEntry, the same CDP
	// call NavigateBack itself issues) directly, then polls the
	// resulting DOM/URL instead of waiting on that same internal
	// network-lifecycle detection.
	if err := navigateBackAndWait(ctx, `location.pathname.endsWith('/help/entity/Item') && document.querySelector('#uc-help-detail h2') !== null`); err != nil {
		t.Fatalf("navigate back to /help/entity/Item: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Location(&loc),
		chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').innerText`, &detail),
	); err != nil {
		t.Fatalf("read location/detail after navigating back: %v", err)
	}
	if !strings.HasSuffix(loc, "/help/entity/Item") {
		t.Errorf("expected back navigation to restore /help/entity/Item, got %q", loc)
	}
	if !strings.Contains(detail, "Item Help") {
		t.Errorf("expected back navigation to restore the Item topic's content, got:\n%s", detail)
	}
}

// TestHelp_RTLGeometry_RealBrowser (uc-infra#144 acceptance criterion 4):
// CLAUDE.md's own warning against markup-only proof — dir="rtl" being
// present on <html> proves nothing about whether the two panes actually
// swapped sides on screen. This compares each pane's real
// getBoundingClientRect() between an en (LTR) and an ar (RTL) render of
// the identical page.
func TestHelp_RTLGeometry_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	rects := func(locale string) (dir string, topicsLeft, detailLeft float64) {
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help?lang="+locale),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.documentElement.getAttribute('dir')`, &dir),
			chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-topics').getBoundingClientRect().left`, &topicsLeft),
			chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').getBoundingClientRect().left`, &detailLeft),
		); err != nil {
			t.Fatalf("%s: read layout geometry: %v", locale, err)
		}
		return dir, topicsLeft, detailLeft
	}

	enDir, enTopicsLeft, enDetailLeft := rects("en")
	if enDir != "ltr" {
		t.Fatalf("expected dir=\"ltr\" for en, got %q", enDir)
	}
	if !(enTopicsLeft < enDetailLeft) {
		t.Fatalf("en (LTR): expected the topics pane left of the detail pane (topicsLeft=%v < detailLeft=%v)", enTopicsLeft, enDetailLeft)
	}

	arDir, arTopicsLeft, arDetailLeft := rects("ar")
	if arDir != "rtl" {
		t.Fatalf("expected dir=\"rtl\" for ar, got %q", arDir)
	}
	// The real, load-bearing assertion: in a mirrored RTL render, the
	// topics pane (first in DOM order) must sit VISUALLY to the RIGHT of
	// the detail pane — the opposite geometric relationship from en —
	// not merely carry a dir="rtl" string with unchanged pixel positions.
	if !(arTopicsLeft > arDetailLeft) {
		t.Fatalf("ar (RTL): expected the topics pane visually to the RIGHT of the detail pane (topicsLeft=%v, detailLeft=%v) — the layout did not actually mirror under dir=\"rtl\"", arTopicsLeft, arDetailLeft)
	}
}

// TestHelp_Authz_DeniedTopicHiddenAndNotFound_RealBrowser (uc-infra#144
// acceptance criterion 2): a viewer denied read on Item must see neither
// its topic in the tree nor its content by direct URL — 404, not 403, so
// the response doesn't even confirm the topic exists (internal/api/
// help.go's own writeHelpNotFound doc comment). The real HTTP status is
// asserted via a plain, non-browser request carrying the same dev-auth
// headers browserCtx sets (internal/api/help_test.go's own
// TestAPI_HelpTopic_DeniedTopicIs404NotVisible already proves this at the
// handler level; the value a browser adds here is confirming the actual
// rendered not-found PAGE a person driven to that URL would see).
func TestHelp_Authz_DeniedTopicHiddenAndNotFound_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testHelpServer(t)

	seedEntityPermission(t, tenantDB, "e2e_help_denied", "Item", false, false)

	ctx := browserCtx(t, tenantID)

	// Hidden from the rendered tree.
	var bodyText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help"),
		chromedp.WaitVisible(`#uc-help-topics`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
	); err != nil {
		t.Fatalf("navigate /help as the denied actor: %v", err)
	}
	if strings.Contains(bodyText, "Item Help") {
		t.Errorf("expected the denied Item topic hidden from the rendered tree, got:\n%s", bodyText)
	}
	if !strings.Contains(bodyText, "Getting Started") {
		t.Errorf("expected the never-gated route/getting-started topic still visible, got:\n%s", bodyText)
	}

	// Real HTTP status: a plain request (same dev-auth headers browserCtx
	// sets on the browser) against the real running httptest server.
	req, err := http.NewRequest("GET", srv.URL+"/help/entity/Item", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", e2eActorID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /help/entity/Item as the denied actor: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /help/entity/Item (denied actor) = %d, want 404", resp.StatusCode)
	}

	// The real rendered not-found PAGE, not a generic error — a browser
	// navigated straight to the URL sees the styled 404 state inside the
	// normal page shell.
	var notFoundText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help/entity/Item"),
		chromedp.WaitVisible(`.uc-help-not-found`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &notFoundText),
	); err != nil {
		t.Fatalf("navigate directly to the denied topic's URL: %v", err)
	}
	if strings.Contains(notFoundText, "How to manage warehouse items") {
		t.Errorf("expected the denied topic's content never rendered, got:\n%s", notFoundText)
	}
	var navCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`document.querySelectorAll("nav.uc-nav").length`, &navCount)); err != nil {
		t.Fatalf("count nav elements on the not-found page: %v", err)
	}
	if navCount == 0 {
		t.Error("expected the not-found page to still render inside the normal shell (nav present), not a dead end")
	}
}

// TestHelp_TopicImageRendersWithAltText_RealBrowser (uc-infra#145) proves
// ![alt](url) markdown in a topic body reaches a real, LOADED <img> in
// the browser — not just that the markup contains an <img> tag.
// CLAUDE.md's own rule against markup-only proof for anything CSS/
// DOM-behavior-dependent applies directly here: a broken/404'ing src
// still produces an <img> element with the right alt attribute, so this
// asserts naturalWidth > 0 (via a real DOM read, chromedp.EvaluateAsDevTools)
// as the actual "it loaded" proof, for both an LTR (en) and RTL (ar)
// locale, with each locale's own real translated alt text (not a
// copy-pasted English fallback).
//
// Also asserts real layout geometry (independent review, uc-infra#145):
// this test's first version only checked src/alt/naturalWidth and missed
// that .uc-help-image had no CSS rule at all — every committed capture is
// 1280px wide, wider than #uc-help-detail's own content column, so the
// image rendered at its native size, overflowed the pane, and forced the
// whole document to scroll horizontally. A markup-only assertion can't
// catch that (CLAUDE.md's own warning against exactly this gap); a real
// getBoundingClientRect/scrollWidth read can and does.
func TestHelp_TopicImageRendersWithAltText_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	cases := []struct {
		locale, wantAlt string
	}{
		{"en", "Screenshot of the Item list page"},
		{"ar", "لقطة شاشة لصفحة قائمة الأصناف"},
	}
	for _, c := range cases {
		t.Run(c.locale, func(t *testing.T) {
			const imgSel = `#uc-help-detail img.uc-help-image`
			var imgCount int
			var src, alt string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(srv.URL+"/help/route/getting-started?lang="+c.locale),
				chromedp.WaitVisible(imgSel, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(`document.querySelectorAll('`+imgSel+`').length`, &imgCount),
				chromedp.EvaluateAsDevTools(`document.querySelector('`+imgSel+`').getAttribute('src')`, &src),
				chromedp.EvaluateAsDevTools(`document.querySelector('`+imgSel+`').getAttribute('alt')`, &alt),
			); err != nil {
				t.Fatalf("%s: navigate and inspect the topic image: %v", c.locale, err)
			}
			if imgCount == 0 {
				t.Fatalf("%s: expected an <img class=%q> in #uc-help-detail, found none", c.locale, "uc-help-image")
			}
			if src != "/help/assets/list/en.jpg" {
				t.Errorf("%s: img src = %q, want %q", c.locale, src, "/help/assets/list/en.jpg")
			}
			if strings.TrimSpace(alt) == "" {
				t.Fatalf("%s: img alt is empty", c.locale)
			}
			if alt != c.wantAlt {
				t.Errorf("%s: img alt = %q, want the real translated text %q (not a silent fallback to en)", c.locale, alt, c.wantAlt)
			}

			// The image load completing is async relative to the DOM
			// insertion WaitVisible already confirmed — poll naturalWidth
			// rather than reading it immediately, so a real (if brief)
			// network fetch of /help/assets/list/en.jpg has time to
			// finish before this asserts on it.
			if err := chromedp.Run(ctx, chromedp.Poll(
				`document.querySelector('`+imgSel+`').naturalWidth > 0`,
				nil, chromedp.WithPollingTimeout(5*time.Second),
			)); err != nil {
				t.Fatalf("%s: image never finished loading (naturalWidth stayed 0 — a broken src still produces an <img> tag, which is exactly why this check exists): %v", c.locale, err)
			}
			var naturalWidth float64
			if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
				`document.querySelector('`+imgSel+`').naturalWidth`, &naturalWidth,
			)); err != nil {
				t.Fatalf("%s: read naturalWidth: %v", c.locale, err)
			}
			if naturalWidth <= 0 {
				t.Errorf("%s: naturalWidth = %v, want > 0 (the image failed to actually load)", c.locale, naturalWidth)
			}

			// Geometry: the image must fit inside its pane, and the page
			// must never gain a horizontal scrollbar because of it — the
			// concrete symptom independent review measured before
			// app.css's .uc-help-image rule existed (imgRight 1701 vs
			// detailRight 1192, documentElement.scrollWidth 1701 > 1280).
			var imgWidth, detailWidth, imgRight, detailRight, scrollWidth, clientWidth float64
			if err := chromedp.Run(ctx,
				chromedp.EvaluateAsDevTools(`document.querySelector('`+imgSel+`').getBoundingClientRect().width`, &imgWidth),
				chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').getBoundingClientRect().width`, &detailWidth),
				chromedp.EvaluateAsDevTools(`document.querySelector('`+imgSel+`').getBoundingClientRect().right`, &imgRight),
				chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').getBoundingClientRect().right`, &detailRight),
				chromedp.EvaluateAsDevTools(`document.documentElement.scrollWidth`, &scrollWidth),
				chromedp.EvaluateAsDevTools(`document.documentElement.clientWidth`, &clientWidth),
			); err != nil {
				t.Fatalf("%s: read layout geometry: %v", c.locale, err)
			}
			if imgWidth > detailWidth+0.5 {
				t.Errorf("%s: image rendered width %v exceeds its pane's own width %v — .uc-help-image must not overflow #uc-help-detail", c.locale, imgWidth, detailWidth)
			}
			if imgRight > detailRight+0.5 {
				t.Errorf("%s: image right edge %v is past its pane's right edge %v — the image escapes the pane", c.locale, imgRight, detailRight)
			}
			if scrollWidth > clientWidth+0.5 {
				t.Errorf("%s: document.scrollWidth %v exceeds clientWidth %v — the image forced the whole page to scroll horizontally", c.locale, scrollWidth, clientWidth)
			}
		})
	}
}

// TestHelp_AdminBadge_VisibleNotHidden_RealBrowser (uc-infra#144
// acceptance criterion 5 / ADR-0023: "one manual, admin topics marked
// visually, not access-restricted"): a non-admin viewer (no role/RBAC
// configured for this tenant at all — every topic is visible per
// authz.Resolver's own "no rules configured yet" default, same as
// internal/api/help_test.go's TestAPI_HelpIndex_ListsVisibleTopics) still
// sees the admin-audience topic, badged, and can select it like any
// other.
func TestHelp_AdminBadge_VisibleNotHidden_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	var badgeCount int
	var badgeText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help"),
		chromedp.WaitVisible(`a[data-help-topic="route/admin-guide"]`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.querySelectorAll('a[data-help-topic="route/admin-guide"] .uc-help-topic-admin').length`, &badgeCount),
		chromedp.EvaluateAsDevTools(`(document.querySelector('a[data-help-topic="route/admin-guide"] .uc-help-topic-admin')||{}).textContent`, &badgeText),
	); err != nil {
		t.Fatalf("navigate /help and inspect the admin-audience topic: %v", err)
	}
	if badgeCount == 0 {
		t.Fatal("expected the admin-audience topic to carry an .uc-help-topic-admin badge, found none")
	}
	if strings.TrimSpace(badgeText) == "" {
		t.Error("expected the admin badge to have real translated label text, got empty")
	}

	// Still fully clickable/visible to this non-admin viewer.
	var detail string
	if err := chromedp.Run(ctx,
		chromedp.Click(`a[data-help-topic="route/admin-guide"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-help-detail h2`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.querySelector('#uc-help-detail').innerText`, &detail),
	); err != nil {
		t.Fatalf("click the admin-audience topic as a non-admin viewer: %v", err)
	}
	if !strings.Contains(detail, "Admin Configuration Guide") {
		t.Errorf("expected the admin-audience topic's content to render for a non-admin viewer (marked, never restricted), got:\n%s", detail)
	}
}

// TestHelp_ListItemWrappedAcrossSourceLines_RealBrowser is the real-
// browser regression test for uc-infra#207: a list item whose source
// text wraps across multiple indented lines (route/list-continuation-
// check's fixture body above) must render, in an actual browser DOM, as
// one complete <ol> with each item's full text intact — not two lists
// split by a stray paragraph with the wrapped item truncated mid-
// sentence, which is what internal/help/markdown.go's parseBlocks used
// to produce before the fix. internal/help/markdown_test.go already
// proves the generated HTML string is correct; this proves a real
// browser parses that HTML into one <ol> DOM node with the right number
// of list-item children, which a string-contains assertion cannot.
func TestHelp_ListItemWrappedAcrossSourceLines_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testHelpServer(t)
	ctx := browserCtx(t, tenantID)

	const detailSel = `#uc-help-detail`
	const liSel = detailSel + ` ol > li`
	var olCount, liCount int
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/help/route/list-continuation-check"),
		chromedp.WaitVisible(detailSel+" ol", chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(`document.querySelectorAll('`+detailSel+` ol').length`, &olCount),
		chromedp.EvaluateAsDevTools(`document.querySelectorAll('`+liSel+`').length`, &liCount),
	); err != nil {
		t.Fatalf("navigate and inspect the wrapped list item: %v", err)
	}
	if olCount != 1 {
		t.Errorf("ol count = %d, want exactly 1 — a wrapped continuation line must not close the list early and start a second <ol>", olCount)
	}
	// Fatal, not Error: the pre-fix bug produces 3 total <li> too (2 in a
	// first <ol> plus 1 in a second one, per the olCount check above), so
	// this count alone isn't what catches the regression — the item text
	// below is. But reading li[1] below assumes at least 2 exist; stop
	// here with a clear message instead of letting a wrong count surface
	// as an opaque JS "Cannot read properties of undefined" exception
	// (independent review, uc-infra#207).
	if liCount != 3 {
		t.Fatalf("li count = %d, want exactly 3 (one per source list item, the wrapped one folded into a single <li>)", liCount)
	}

	var secondItemText string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('`+liSel+`')[1]?.textContent ?? ''`, &secondItemText,
	)); err != nil {
		t.Fatalf("read the wrapped <li>'s textContent: %v", err)
	}
	wantSecondItem := "Type a short title and a description of the problem. You can also " +
		"record a voice note, which is transcribed automatically and appended " +
		"to your description."
	if secondItemText != wantSecondItem {
		t.Errorf("second <li> textContent = %q, want the full wrapped sentence intact:\n%q", secondItemText, wantSecondItem)
	}
}
