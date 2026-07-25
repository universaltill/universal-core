package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
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
	db := testDB(t)
	withDevAuthEnabled(t)
	srv, tenantID := testServer(t, db)
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
	db := testDB(t)
	withDevAuthEnabled(t)
	srv, tenantID := testServer(t, db) // no speech client wired — transcribe always 503s
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
