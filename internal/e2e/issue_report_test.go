package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// TestIssueReportPage_FormIsStyled is the real-browser regression test
// for a bug Farshid found by actually looking at the rendered page: the
// capture form's <form> tag had no class="uc-form" attribute, so none of
// app.css's `.uc-form label/input/select/textarea` rules applied at all
// — only its buttons happened to be styled, via the broader
// `.uc-container button` selector, which is exactly why the screenshot
// showed a blue "Record voice note" button next to a completely
// unstyled, browser-default Title input. Every prior test for this page
// (issuereport_test.go) only ever checked the DOM's raw HTML string for
// the right ids/attributes — none of them render a real page or apply
// real CSS, so a missing class attribute like this was invisible to
// them. This test actually asks a real browser what an input's computed
// style is, the only way to catch this class of bug at all.
func TestIssueReportPage_FormIsStyled(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-title`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue report page: %v", err)
	}

	var formHasClass bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-report-form").classList.contains("uc-form")`,
		&formHasClass,
	)); err != nil {
		t.Fatalf("check the form's class: %v", err)
	}
	if !formHasClass {
		t.Fatal("expected the capture form to carry class=\"uc-form\" so app.css's form styling actually applies")
	}

	// The real proof, not just the class attribute existing: app.css's
	// `.uc-form input` rule sets a real 1px border — the browser default
	// for a plain <input> is "none" (rendered via the OS's native
	// control appearance instead), so this distinguishes "styled" from
	// "unstyled" the same way the screenshot did, just by asking the
	// browser's own computed style instead of eyeballing a screenshot.
	var titleBorderStyle string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`getComputedStyle(document.getElementById("uc-issue-title")).borderStyle`,
		&titleBorderStyle,
	)); err != nil {
		t.Fatalf("read the title input's computed border style: %v", err)
	}
	if titleBorderStyle != "solid" {
		t.Fatalf(`expected the Title input's computed border-style to be "solid" (app.css's .uc-form input rule), got %q — the form's CSS isn't actually applying`, titleBorderStyle)
	}

	var labelDisplay string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`getComputedStyle(document.querySelector('label[for="uc-issue-title"]')).display`,
		&labelDisplay,
	)); err != nil {
		t.Fatalf("read the Title label's computed display: %v", err)
	}
	if labelDisplay != "block" {
		t.Fatalf(`expected the Title label's computed display to be "block" (app.css's .uc-form label rule — this is what puts each field on its own line), got %q`, labelDisplay)
	}
}

// TestIssueReportPage_VoiceRecordShowsCleanErrorNotRawJSON is the real-
// browser regression test for the second bug in the same screenshot: a
// failed /issue-report/transcribe call (e.g. a deployment with no
// WHISPER_URL configured, speechassist.Client disabled, a real 503) used
// to dump the server's raw {"data":null,"error":"..."} envelope text
// verbatim into the status line next to the button — the exact text
// visible in Farshid's screenshot. Fixed by extractErrorMessage
// (issuereport.go) parsing that envelope and showing only its .error
// field. A headless browser has no real microphone, so this fakes
// getUserMedia/MediaRecorder via a script injected before the page loads
// (page.AddScriptToEvaluateOnNewDocument) — the fake still drives the
// real click handler, the real fetch() call, and the real
// extractErrorMessage parsing logic; only the actual audio capture
// itself is stubbed out, which is the one part no CI/dev machine can be
// relied on to have a working microphone for regardless.
func TestIssueReportPage_VoiceRecordShowsCleanErrorNotRawJSON(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t) // no speech client wired — transcribe always 503s
	ctx := browserCtx(t, tenantID)

	const fakeMediaScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.start = function() {};
    this.stop = function() {
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeMediaScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media devices script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // start "recording"
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // stop -> triggers the real transcribe fetch
	); err != nil {
		t.Fatalf("click record then stop: %v", err)
	}

	// Polls past the transient "Transcribing…" status text (set the
	// instant Stop is clicked, before the fetch to the real, disabled
	// speech endpoint has even resolved) to the final error state.
	// Hardcodes the English catalog's exact strings ("Transcribing…"
	// here, "voice transcription is not configured" below) — testServer
	// always loads i18n.Load("en"), so this holds today, but would need
	// updating if that default ever changed; a wrong assumption here
	// fails loudly (the poll times out, or the final assertion mismatches
	// on real text), never silently false-passes.
	var statusText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.length > 0 && t.indexOf("Transcribing") === -1 ? t : null;
		}`,
		&statusText,
	)); err != nil {
		t.Fatalf("wait for the status line to show an error: %v", err)
	}

	if strings.Contains(statusText, `"data"`) || strings.Contains(statusText, `{`) {
		t.Fatalf("expected a clean human error message, got the raw JSON envelope: %q", statusText)
	}
	if !strings.Contains(statusText, "voice transcription is not configured") {
		t.Fatalf("expected the real server error message to reach the status line, got %q", statusText)
	}
}

// TestIssueReportPage_ConsoleLogCapturedFromEarlierPageAndPrefilled is the
// real-browser proof for universaltill/uc-infra#46's log-capture slice:
// internal/api/layout.go's shellTmpl installs a console/error listener on
// EVERY page (persisted to sessionStorage), specifically so a problem
// noticed on one page can still be reported with its own console output
// once the person navigates to the issue-report page — a rendered-HTML-
// string test can't prove any of this, since it's entirely a real
// browser's console object, sessionStorage, and page-to-page navigation
// working together, not markup.
func TestIssueReportPage_ConsoleLogCapturedFromEarlierPageAndPrefilled(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	// Land on an ordinary page first — anything that goes through the
	// shared shell — and make it misbehave, the same way a real user
	// would stumble into a real error before ever opening the issue
	// reporter.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-title`, chromedp.ByQuery),
		chromedp.Evaluate(`console.error("Save failed", "TypeError: cannot read properties of undefined"); void 0;`, nil),
	); err != nil {
		t.Fatalf("trigger a console.error on the first page load: %v", err)
	}

	// A fresh navigation to the same capture page (simulating the user
	// leaving and coming back to file the report) must still see the
	// entry sessionStorage already holds from before.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-console-log`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue report page: %v", err)
	}

	var consoleLogValue string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-console-log`, &consoleLogValue, chromedp.ByQuery)); err != nil {
		t.Fatalf("read the console-log textarea's value: %v", err)
	}
	if !strings.Contains(consoleLogValue, "Save failed") || !strings.Contains(consoleLogValue, "TypeError") {
		t.Fatalf("expected the earlier console.error to be pre-filled into the console-log field, got %q", consoleLogValue)
	}

	// And it must be genuinely submitted, not just displayed: same
	// end-to-end proof TestIssueReport_Submit_StoresConsoleLog already
	// gives at the HTTP layer, but here driven by a real click through the
	// real DOM. Submitting this <form> is a plain (non-htmx) POST, so the
	// browser navigates to a whole new document — issueReportSubmit's own
	// "result" template output, identified by its class (not the
	// original page's #uc-issue-report-result placeholder div, which this
	// navigation replaces entirely rather than swapping into).
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`#uc-issue-title`, "Save button throws", chromedp.ByQuery),
		chromedp.SetValue(`#uc-issue-description`, "Clicking save throws a JS error.", chromedp.ByQuery),
		chromedp.Click(`#uc-issue-report-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-issue-report-result`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit the report: %v", err)
	}

	// The vacuous version of this check would still pass with console_log
	// silently dropped from the submit (the result page looks identical
	// either way, since the field is optional) — so this reads the actual
	// stored record back through the generic API, same server session,
	// rather than trusting the result page's mere existence.
	var apiResult struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		fetch('/api/records/IssueReport')
			.then(function(r){ return r.text().then(function(t){ return {status: r.status, body: t}; }); })
	`, &apiResult, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})); err != nil {
		t.Fatalf("fetch the stored IssueReport records from the browser: %v", err)
	}
	if apiResult.Status != 200 {
		t.Fatalf("expected 200 listing IssueReport records, got %d: %.300s", apiResult.Status, apiResult.Body)
	}
	if !strings.Contains(apiResult.Body, "Save failed") || !strings.Contains(apiResult.Body, "TypeError") {
		t.Fatalf("expected the pre-filled console log to have actually been submitted and stored, got:\n%.500s", apiResult.Body)
	}
}
