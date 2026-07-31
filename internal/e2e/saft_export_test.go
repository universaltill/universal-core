package e2e

import (
	"context"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/finance"
)

// TestSAFTExport_FinanceMenuToDownload is the real-browser layer for the
// SAF-T export (universaltill/uc-infra#28): the Finance module menu
// actually shows the export link, the linked form page renders real,
// interactable date inputs (prefilled — not just the right HTML strings,
// which internal/api's own test already covers), and the form's target
// actually hands a real browser a well-formed SAF-T XML document for the
// selected range.
func TestSAFTExport_FinanceMenuToDownload(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()

	// testServer publishes foundation + purchasing; SAF-T lives on the
	// finance module menu, so publish finance too, plus a minimal ledger
	// so the downloaded file carries a real journal entry.
	actor := humanActor()
	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}
	if err := finance.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.PublishForms: %v", err)
	}
	accounts := data.NewGLAccountRepo(tenantDB)
	bankID, err := accounts.UpsertByCode(ctx, "1100", "Bank", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert bank: %v", err)
	}
	revID, err := accounts.UpsertByCode(ctx, "3000", "Revenue", "income", "USD", true)
	if err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}
	tx, err := tenantDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := data.NewJournalEntryRepo(tenantDB).CreateTx(ctx, tx, "2026-03-15", "E2E entry", "", "",
		[]string{bankID, revID}, []int64{99_00, 0}, []int64{0, 99_00}); err != nil {
		t.Fatalf("create journal entry: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	// 1. The finance module menu shows the SAF-T export link.
	var linkHref string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/modules/finance"),
		chromedp.WaitVisible(`.uc-module-reports a`, chromedp.ByQuery),
		chromedp.AttributeValue(`.uc-module-reports a`, "href", &linkHref, nil, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("finance module menu: %v", err)
	}
	if linkHref != "/export/saft/form" {
		t.Fatalf("expected the menu's report link to target /export/saft/form, got %q", linkHref)
	}

	// 2. The form page renders real, prefilled date inputs — values as
	// the browser's own DOM holds them, not string-matched markup.
	var fromValue, toValue string
	var buttonStyle struct {
		Visible      bool   `json:"visible"`
		BorderRadius string `json:"borderRadius"`
		Cursor       string `json:"cursor"`
		FormBorder   string `json:"formBorder"`
	}
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/export/saft/form"),
		chromedp.WaitVisible(`.uc-saft-form button`, chromedp.ByQuery),
		chromedp.Value(`.uc-saft-form input[name="from"]`, &fromValue, chromedp.ByQuery),
		chromedp.Value(`.uc-saft-form input[name="to"]`, &toValue, chromedp.ByQuery),
		// Real computed styles, not markup strings: the app stylesheet's
		// uc-form rules (app.css: 4px button radius, pointer cursor,
		// bordered form card) must actually have applied — a
		// browser-default unstyled <button> passes a bare
		// bounding-box check, which is exactly the trap CLAUDE.md's
		// e2e rule warns about (and this test originally fell into,
		// per #28's independent review).
		chromedp.EvaluateAsDevTools(
			`(function(){
				var b = document.querySelector('.uc-saft-form button');
				var f = document.querySelector('.uc-saft-form');
				var r = b.getBoundingClientRect();
				var bs = getComputedStyle(b), fs = getComputedStyle(f);
				return {
					visible: r.width > 0 && r.height > 0,
					borderRadius: bs.borderRadius,
					cursor: bs.cursor,
					formBorder: fs.borderStyle
				};
			})()`,
			&buttonStyle),
	); err != nil {
		t.Fatalf("SAF-T form page: %v", err)
	}
	if len(fromValue) != 10 || len(toValue) != 10 || !strings.HasSuffix(fromValue, "-01-01") {
		t.Fatalf("expected prefilled ISO dates (year start / today), got from=%q to=%q", fromValue, toValue)
	}
	if !buttonStyle.Visible {
		t.Fatal("download button has no rendered size — not actually visible")
	}
	if buttonStyle.BorderRadius != "4px" || buttonStyle.Cursor != "pointer" || buttonStyle.FormBorder != "solid" {
		t.Fatalf("app.css uc-form styling not actually applied (browser defaults rendered instead): %+v", buttonStyle)
	}

	// 3. Submitting the form's target really yields the XML document —
	// fetched by the same browser session (dev-auth headers included via
	// network.SetExtraHTTPHeaders), the exact request the form submit
	// performs.
	var result struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := chromedp.Run(bctx, chromedp.Evaluate(`
		fetch('/export/saft?from=2026-01-01&to=2026-12-31')
			.then(function(r){ return r.text().then(function(t){ return {status: r.status, body: t}; }); })
	`, &result, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})); err != nil {
		t.Fatalf("fetch SAF-T file from the browser: %v", err)
	}
	status, bodyText := result.Status, result.Body
	if status != 200 {
		t.Fatalf("expected 200 from the browser's own export fetch, got %d: %.300s", status, bodyText)
	}
	for _, want := range []string{"<AuditFile", "<TotalDebit>99.00</TotalDebit>", "E2E entry", "1100"} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("downloaded SAF-T file missing %q:\n%.500s", want, bodyText)
		}
	}
}
