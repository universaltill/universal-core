// Real-browser proof that uc-infra#152's shipped content — the actual
// go:embed'd Markdown under internal/help/content/, not the synthetic
// fixture help_test.go's other tests wire in via help.SetIndexForTesting
// — genuinely renders through the real viewer (uc-infra#144), and that
// the "?" affordance (uc-infra#143) actually flips from disabled to a
// real, correctly-targeted link on each of the six pages this card wired
// (internal/api/import.go, extsqlimport.go, saftexport.go, reporting.go,
// rfq_report.go, workflow.go). Mirrors help_real_content_purchasing_test.go's
// own two-test shape exactly: one test proves real content renders
// through /help/{topicID} for a real tenant (including a non-English
// locale actually rendering its own translation, not silently falling
// back to English — the same guard that file's own ar case uses), the
// other proves the affordance on the real generated page itself is
// enabled and points at the right topic id. The seventh topic this card
// shipped (route/export_ubl) has no "?" wiring by design (ublexport.go's
// own doc comment: no standalone page exists to attach one to) — nothing
// here exercises it, matching the Architect note's own non-goal.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestHelp_ImportExportReportingWorkflowContent_RendersInViewer_RealBrowser
// proves the real shipped content for four of the seven uc-infra#152
// topics — not fixture content — renders through /help/{topicID} in a
// real browser: English for route/import (a quick sanity check the
// topic exists and resolves at all), then one non-English locale each
// for three of the other wired topics, mixing tr/fa/ar rather than
// repeating the same locale (route/import in fa, route/workflow_jobs in
// ar, route/reports_purchasing in tr) — with the real translated prose
// actually present (not a silent fallback to en) and the page correctly
// right-to-left for the two RTL locales.
func TestHelp_ImportExportReportingWorkflowContent_RendersInViewer_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	t.Run("en: route/import topic", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/route/import?lang=en"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/route/import?lang=en: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for en, got %q`, dir)
		}
		for _, want := range []string{
			"Import Records",
			"Nothing is written during Preview",
			"cannot be a mapping target",
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real route/import (en) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
	})

	t.Run("fa: route/import topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/route/import?lang=fa"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/route/import?lang=fa: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for fa, got %q`, dir)
		}
		// The real Persian title and a real Persian-only phrase from the
		// body — not a phrase that also happens to appear in the English
		// body — so this fails on a silent en fallback the same way
		// help_real_content_purchasing_test.go's own ar case guards
		// against.
		for _, want := range []string{"ورود رکوردها", "نمی‌تواند هدف نگاشت باشد"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real route/import (fa) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/route/import.md
		// body — if the fa render silently fell back to en (help.Index.
		// resolve does fall back to en when a locale's file is missing/
		// blank), this English phrase would be sitting right in the
		// rendered body text.
		if strings.Contains(bodyText, "cannot be a mapping target") {
			t.Errorf("fa render appears to have fallen back to the English body text instead of the real Persian translation")
		}
	})

	t.Run("ar: route/workflow_jobs topic, real translation and RTL", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/route/workflow_jobs?lang=ar"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/route/workflow_jobs?lang=ar: %v", err)
		}
		if dir != "rtl" {
			t.Errorf(`expected dir="rtl" for ar, got %q`, dir)
		}
		for _, want := range []string{"الموافقات", "يمكن للتفويض أن يتيح لك الاعتماد نيابةً عن شخص آخر"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real route/workflow_jobs (ar) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/route/workflow_jobs.md
		// body, guarding against a silent en fallback the same way the fa
		// case above does.
		if strings.Contains(bodyText, "A Delegation can let you approve on someone else's behalf") {
			t.Errorf("ar render appears to have fallen back to the English body text instead of the real Arabic translation")
		}
	})

	t.Run("tr: route/reports_purchasing topic, real translation", func(t *testing.T) {
		var bodyText, dir string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/help/route/reports_purchasing?lang=tr"),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
			chromedp.AttributeValue(`html`, `dir`, &dir, nil),
		); err != nil {
			t.Fatalf("navigate /help/route/reports_purchasing?lang=tr: %v", err)
		}
		if dir != "ltr" {
			t.Errorf(`expected dir="ltr" for tr, got %q`, dir)
		}
		for _, want := range []string{"Satın Alma Raporu", "Yetersiz geçmiş"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("expected real route/reports_purchasing (tr) content %q in rendered page, got body text:\n%s", want, bodyText)
			}
		}
		// A real word-for-word substring of the real en/route/
		// reports_purchasing.md body, guarding against a silent en
		// fallback the same way the fa/ar cases above do.
		if strings.Contains(bodyText, "Insufficient history") {
			t.Errorf("tr render appears to have fallen back to the English body text instead of the real Turkish translation")
		}
	})
}

// TestHelp_ImportExportReportingWorkflowContent_AffordanceNowEnabled_RealBrowser
// proves the "?" affordance on each of the six real pages this card
// wired — disabled by construction before this card (help.HasContent
// returned false for every route topic) — is now a real, enabled link
// pointing at the right topic id, driven through a real headless
// browser against a real compiled server. Setup mirrors the closest
// existing precedent per page (purchasing_report_test.go,
// rfq_report_test.go, saft_export_test.go, workflow_inbox_test.go): a
// fresh tenant with foundation+purchasing (testServer's own default) and
// finance published, plus one RequestForQuotation record for the RFQ
// report's own {id}-scoped route.
func TestHelp_ImportExportReportingWorkflowContent_AffordanceNowEnabled_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}
	if err := finance.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.PublishForms: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	draftID := publishedStatusID(t, tenantDB, "rfq_status", "draft")
	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-HELP-E2E-1", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("seed RequestForQuotation: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	cases := []struct {
		name    string
		path    string
		topicID string
	}{
		{"import upload page", "/import/Item", "route/import"},
		{"import from SQL source page", "/import/Item/sql", "route/import_external_sql"},
		{"SAF-T export form page", "/export/saft/form", "route/export_saft"},
		{"purchasing report", "/reports/purchasing", "route/reports_purchasing"},
		{"RFQ vendor comparison report", "/reports/rfq/" + rfq.ID, "route/reports_rfq"},
		{"workflow inbox (Approvals)", "/workflow-jobs", "route/workflow_jobs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var disabled bool
			var href, topicAttr string
			if err := chromedp.Run(bctx,
				chromedp.Navigate(srv.URL+tc.path),
				chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(
					`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
				),
				chromedp.AttributeValue(`.uc-help-affordance`, `href`, &href, nil),
				chromedp.AttributeValue(`.uc-help-affordance`, `data-help-topic`, &topicAttr, nil),
			); err != nil {
				t.Fatalf("navigate %s and inspect affordance: %v", tc.path, err)
			}
			if disabled {
				t.Errorf("expected the \"?\" affordance on %s to be enabled now that %s has real content, got aria-disabled=\"true\"", tc.path, tc.topicID)
			}
			wantHref := "/help/" + tc.topicID
			if href != wantHref {
				t.Errorf("expected the affordance href on %s to be %q, got %q", tc.path, wantHref, href)
			}
			if topicAttr != tc.topicID {
				t.Errorf("expected the affordance data-help-topic on %s to be %q, got %q", tc.path, tc.topicID, topicAttr)
			}
		})
	}

	// Follow one through for real — a genuine browser navigation, not
	// just an href-string assertion, lands on the real topic (mirrors
	// help_real_content_purchasing_test.go's own follow-through proof).
	t.Run("clicking the affordance on the workflow inbox lands on the real topic", func(t *testing.T) {
		var bodyText string
		if err := chromedp.Run(bctx,
			chromedp.Navigate(srv.URL+"/workflow-jobs"),
			chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
			chromedp.Click(`.uc-help-affordance`, chromedp.ByQuery),
			chromedp.WaitVisible(`#uc-help-detail`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(`document.body.innerText`, &bodyText),
		); err != nil {
			t.Fatalf("click affordance and land on topic: %v", err)
		}
		if !strings.Contains(bodyText, "This is your approvals inbox") {
			t.Errorf("expected clicking the affordance to land on the real route/workflow_jobs topic content, got body text:\n%s", bodyText)
		}
	})
}
