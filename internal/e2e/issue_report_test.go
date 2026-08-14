package e2e

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/blobstore"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/testexec"
)

// testServerWithBlobstore is testServer (csv_import_test.go) plus a real
// filesystem-backed blobstore.Store wired in — a separate helper rather
// than adding a parameter to testServer itself, so every other e2e test
// using testServer stays exactly as it was (no blobstore configured,
// matching a deployment that never called SetBlobstore), same pattern
// internal/api's own testHandlerWithSpeech establishes for speechassist.
// Needed here specifically because issueReportNewPage's AttachmentsEnabled
// gate (uc-infra#92) only renders the screen-record control at all when
// a blobstore is configured.
func testServerWithBlobstore(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	router := newTestRouter(t)
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "E2E Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := api.New(router, catalog, nil, nil, nil, nil, nil)
	store, err := blobstore.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore.NewFSStore: %v", err)
	}
	h.SetBlobstore(store)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, id, tenantDB
}

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

// TestIssueReportPage_DescriptionAndConsoleLogEnforceMaxLength is the
// real-browser regression test for uc-infra#174: title/description/
// console_log had no length bound at all before entity.Field.MaxLength
// existed. A
// string-match test on the rendered HTML (checking `maxlength="20000"`
// appears in the markup) would only prove the attribute's TEXT is
// present, not that a real browser actually parses and enforces it — the
// same "never proves it's actually applied" gap CLAUDE.md's testing
// section calls out for CSS, equally true of an HTML attribute a typo
// (e.g. a stray non-numeric character) could silently make inert. This
// asks a real browser two things: (1) the parsed DOM maxLength property
// (not the raw attribute string) is exactly the declared bound, and (2)
// inserting text past that bound — via execCommand("insertText"), which
// (unlike setting .value directly, a real browser's own paste/IME input
// path) goes through the same native length-enforcement a typed or
// pasted submission would — is actually truncated at the boundary, not
// silently accepted.
func TestIssueReportPage_DescriptionAndConsoleLogEnforceMaxLength(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-description`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue report page: %v", err)
	}

	// title's own MaxLength (uc-infra#174, independent review: the
	// original fix only covered description/console_log, leaving title —
	// reachable through the identical unbounded-FieldString gap on the
	// same request — untested here). transcript is deliberately excluded:
	// its textarea is readonly (never typed into), so execCommand
	// insertText's "real typed/pasted input" proof doesn't apply the same
	// way there — its own script-assignment clamp is covered instead by
	// TestIssueReportPage_TranscriptAndDescriptionAreClampedOnTranscribe.
	for _, tc := range []struct {
		field     string
		maxLength int
	}{
		{"uc-issue-title", issueReportFieldMaxLength(t, "title")},
		{"uc-issue-description", issueReportFieldMaxLength(t, "description")},
		{"uc-issue-console-log", issueReportFieldMaxLength(t, "console_log")},
	} {
		var domMaxLength int
		if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
			`document.getElementById("`+tc.field+`").maxLength`, &domMaxLength,
		)); err != nil {
			t.Fatalf("%s: read the parsed DOM maxLength property: %v", tc.field, err)
		}
		if domMaxLength != tc.maxLength {
			t.Fatalf("%s: DOM maxLength property = %d, want %d — the maxlength attribute isn't parsing as a real bound", tc.field, domMaxLength, tc.maxLength)
		}

		var resultLength int
		script := `(function() {
			var el = document.getElementById("` + tc.field + `");
			el.focus();
			el.value = "";
			document.execCommand("insertText", false, "a".repeat(` + strconv.Itoa(tc.maxLength+1) + `));
			return el.value.length;
		})()`
		if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(script, &resultLength)); err != nil {
			t.Fatalf("%s: insert text past the bound: %v", tc.field, err)
		}
		if resultLength != tc.maxLength {
			t.Fatalf("%s: after inserting %d characters, the field holds %d — the browser did not truncate at its declared MaxLength (%d)", tc.field, tc.maxLength+1, resultLength, tc.maxLength)
		}
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

// issueReportFieldMaxLength is this test file's own copy of the identical
// helper in internal/api/issuereport.go — reading the bound straight off
// foundation.IssueReport() rather than hardcoding it a third time (the
// Definition, the capture page's rendered attribute, and this test all
// deriving from one source, per uc-infra#174's own "one source of truth"
// fix for the constant-duplication finding).
func issueReportFieldMaxLength(t *testing.T, name string) int {
	t.Helper()
	f, ok := foundation.IssueReport().FieldByName(name)
	if !ok || f.MaxLength == nil {
		t.Fatalf("expected foundation.IssueReport() field %q to declare a MaxLength", name)
	}
	return *f.MaxLength
}

// TestIssueReportPage_ConsoleLogPrefillIsClampedToMaxLength (uc-infra#174,
// independent review) is the real-browser regression test for the fix in
// issueReportTmpl's sessionStorage-prefill script: HTML's maxlength
// attribute only constrains TYPED input, never a script assignment to
// .value — so the console-log textarea's prefill (sourced from whatever
// layout.go's shellTmpl buffered into sessionStorage during this tab's
// session) needed its own explicit clamp, independent of the attribute
// TestIssueReportPage_DescriptionAndConsoleLogEnforceMaxLength above
// already proved is enforced against typed input. Seeds sessionStorage
// with an oversized buffered log directly (the same shape
// layout.go's ucAppendLog itself writes) rather than driving real console
// activity, so this isolates the prefill script's own clamp from
// ucAppendLog's separate per-entry cap (that one has its own coverage
// need, but browser-JS-internal capture logic isn't reachable from a Go
// test without a real page navigation of its own).
func TestIssueReportPage_ConsoleLogPrefillIsClampedToMaxLength(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)
	maxLen := issueReportFieldMaxLength(t, "console_log")

	// Navigate once first so a real page (with the real
	// data-uc-tenant-bearing nav, which ucTenantKey's key derivation
	// reads) exists to seed sessionStorage against, then reload — the
	// prefill script only runs at page load.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-console-log`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("initial navigate: %v", err)
	}

	seedScript := `(function() {
		var nav = document.querySelector("[data-uc-tenant]");
		var key = "ucConsoleLog:" + (nav ? nav.getAttribute("data-uc-tenant") : "anonymous");
		window.sessionStorage.setItem(key, JSON.stringify(["a".repeat(` + strconv.Itoa(maxLen+5000) + `)]));
	})()`
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(seedScript, nil)); err != nil {
		t.Fatalf("seed an oversized buffered log into sessionStorage: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Reload(),
		chromedp.WaitVisible(`#uc-issue-console-log`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("reload to trigger the prefill script: %v", err)
	}

	var resultLength int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-console-log").value.length`, &resultLength,
	)); err != nil {
		t.Fatalf("read the prefilled console-log textarea's value length: %v", err)
	}
	if resultLength != maxLen {
		t.Fatalf("prefilled console_log value is %d characters, want exactly %d (clamped) — the sessionStorage prefill script did not clamp an oversized buffered log", resultLength, maxLen)
	}
}

// TestIssueReportPage_TranscriptAndDescriptionAreClampedOnTranscribe
// (uc-infra#174, independent review) is transcript/description's own
// version of the console-log prefill test above: the transcribe fetch's
// .then() handler assigns both transcriptEl.value and (via the
// append-into-description branch) descriptionEl.value directly, another
// script assignment HTML's maxlength attribute does nothing for. Fakes
// getUserMedia/MediaRecorder the same way
// TestIssueReportPage_VoiceRecordShowsCleanErrorNotRawJSON does (a
// headless browser has no real microphone), and additionally fakes
// window.fetch itself for the /issue-report/transcribe call specifically
// (letting every other fetch — HTMX nav, etc. — through unchanged) so
// this doesn't depend on a real speechassist backend: the real click
// handler, the real .then() clamp logic, and the real DOM assignments all
// still run untouched, only the ASR response body is substituted.
func TestIssueReportPage_TranscriptAndDescriptionAreClampedOnTranscribe(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)
	transcriptMax := issueReportFieldMaxLength(t, "transcript")
	descriptionMax := issueReportFieldMaxLength(t, "description")

	fakeScript := `
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
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return Promise.resolve(new Response("a".repeat(` + strconv.Itoa(transcriptMax+5000) + `), { status: 200 }));
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // start "recording"
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // stop -> triggers the fake-fetch transcribe response
	); err != nil {
		t.Fatalf("click record then stop: %v", err)
	}

	var transcriptLength int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var v = document.getElementById("uc-issue-transcript").value;
			return v && v.length > 0 ? v.length : null;
		}`,
		&transcriptLength,
	)); err != nil {
		t.Fatalf("wait for the transcript field to populate: %v", err)
	}
	if transcriptLength != transcriptMax {
		t.Fatalf("transcript value is %d characters, want exactly %d (clamped)", transcriptLength, transcriptMax)
	}

	var descriptionLength int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-description").value.length`, &descriptionLength,
	)); err != nil {
		t.Fatalf("read the description field's resulting value length: %v", err)
	}
	if descriptionLength != descriptionMax {
		t.Fatalf("description value is %d characters, want exactly %d (clamped) — the appended transcript was not clamped to description's own (smaller) MaxLength", descriptionLength, descriptionMax)
	}
}

// TestIssueReportPage_MicRecord_DoubleClickDoesNotStartTwoStreams is the
// real-browser regression test for uc-infra#173: found during uc-infra#92's
// independent review, which fixed the identical race in the new
// screen-record button but left the pre-existing mic button's own click
// handler untouched. Before that handler set `recording = true` only
// inside getUserMedia's `.then` callback and never disabled the button
// while the promise was pending — two clicks before the browser's
// permission prompt resolved started two independent MediaRecorders
// against one shared `mediaRecorder` variable, and Stop only ever reached
// whichever one the variable currently pointed at, leaving the other
// stream's tracks live indefinitely.
//
// The fake getUserMedia here deliberately returns a Promise that never
// resolves on its own (window.__resolveGetUserMedia stashes the resolver
// instead) so the test controls the exact moment the permission prompt
// "answers" — a Promise.resolve()-based fake (as the other tests in this
// file use, where nothing needs the pending window itself) would still
// usually only call getUserMedia once here, since a real browser
// suppresses click events on an already-disabled button; this deferred
// form additionally lets the test assert on the disabled state itself
// while the first call is still in flight, not just the eventual call
// count.
func TestIssueReportPage_MicRecord_DoubleClickDoesNotStartTwoStreams(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeDeferredMediaScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getUserMediaCallCount = 0;
  window.__resolveGetUserMedia = null;
  navigator.mediaDevices.getUserMedia = function() {
    window.__getUserMediaCallCount++;
    return new Promise(function(resolve) {
      window.__resolveGetUserMedia = resolve;
    });
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
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeDeferredMediaScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred media devices script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // first click: starts the (pending) getUserMedia call
	); err != nil {
		t.Fatalf("click the mic button once: %v", err)
	}

	// The guard's own proof, before the pending promise ever resolves:
	// the button must already be disabled, which is also what makes the
	// browser itself refuse to dispatch a second click below.
	var disabledWhilePending bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-record-btn").disabled`, &disabledWhilePending,
	)); err != nil {
		t.Fatalf("read the mic button's disabled state while getUserMedia is pending: %v", err)
	}
	if !disabledWhilePending {
		t.Fatal("expected the mic button to be disabled synchronously on click, before getUserMedia resolves (regression: uc-infra#173)")
	}

	// A real browser does not dispatch "click" on an already-disabled
	// button — this is the actual double-click scenario the bug report
	// describes, driven the same way a real impatient user would.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the mic button a second time while the first getUserMedia call is pending: %v", err)
	}

	var callCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__getUserMediaCallCount`, &callCount)); err != nil {
		t.Fatalf("read getUserMedia's call count: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected getUserMedia to have been called exactly once despite two clicks, got %d calls (regression: uc-infra#173 — an earlier stream would be left live indefinitely)", callCount)
	}

	// Resolve the pending permission prompt and confirm recording still
	// starts normally afterward — the fix must not leave the button
	// stuck disabled or the handler otherwise broken.
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`window.__resolveGetUserMedia({ getTracks: function() { return []; } }); void 0;`, nil,
	)); err != nil {
		t.Fatalf("resolve the pending getUserMedia promise: %v", err)
	}

	// Polls for the exact post-resolve text, not merely "any non-empty
	// text" (independent review): the button already reads a non-empty
	// string ("Record voice note") before the resolve, so a looser
	// predicate would be satisfied immediately and prove nothing about
	// actually waiting for the promise — it only happened to pass before
	// because this test's synchronous-resolve fake lets the microtask
	// queue drain before chromedp's next CDP round-trip, not because the
	// predicate itself waited for anything.
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-btn").textContent;
			return t === "Stop recording" ? t : null;
		}`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the mic button to reflect the started recording: %v", err)
	}
	var enabledAfterResolve bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-record-btn").disabled`, &enabledAfterResolve,
	)); err != nil {
		t.Fatalf("read the mic button's disabled state after getUserMedia resolves: %v", err)
	}
	if !enabledAfterResolve {
		t.Fatal("expected the mic button to be re-enabled once getUserMedia resolves")
	}
}

// TestIssueReportPage_MicRecord_PermissionDeniedReEnablesButtonForRetry is
// the regression test for the getUserMedia promise's own rejection path
// (independent review of uc-infra#173's fix): a denied mic permission (or
// any other getUserMedia rejection) must leave the button re-enabled, not
// permanently stuck disabled with no way to retry short of a page reload.
// Deleting the fix's `recordBtn.disabled = false;` re-enable line and
// re-running this test (done as part of that review) left the whole rest
// of the suite green — this test is what actually pins that line.
func TestIssueReportPage_MicRecord_PermissionDeniedReEnablesButtonForRetry(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeDenyThenAllowScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getUserMediaCallCount = 0;
  navigator.mediaDevices.getUserMedia = function() {
    window.__getUserMediaCallCount++;
    if (window.__getUserMediaCallCount === 1) {
      return Promise.reject(new Error("Permission denied"));
    }
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
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeDenyThenAllowScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deny-then-allow media devices script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // denied
	); err != nil {
		t.Fatalf("click the mic button: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the permission-denied status message: %v", err)
	}
	if !strings.Contains(status, "Permission denied") {
		t.Fatalf("expected the permission-denial error to reach the status line, got %q", status)
	}

	var enabledAfterDenial bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-record-btn").disabled`, &enabledAfterDenial,
	)); err != nil {
		t.Fatalf("read the mic button's disabled state after denial: %v", err)
	}
	if !enabledAfterDenial {
		t.Fatal("expected the mic button to be re-enabled after a getUserMedia rejection, so the person can retry (regression: independent review of uc-infra#173)")
	}

	// The actual retry proof: a second click (the fake now allows it)
	// must be able to reach getUserMedia again, not be permanently
	// inert from the first denial.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the mic button a second time to retry: %v", err)
	}
	var callCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__getUserMediaCallCount`, &callCount)); err != nil {
		t.Fatalf("read getUserMedia's call count: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected the retry click to reach getUserMedia a second time, got %d total calls", callCount)
	}
}

// TestIssueReportPage_MicRecord_RecorderConstructorThrowStopsStream is the
// regression test for independent review's other finding on uc-infra#173's
// fix: getUserMedia can resolve (permission granted, a real stream handed
// back) and then something between that and mediaRecorder.start() throw
// synchronously — the review reproduced this via the MediaRecorder
// constructor itself throwing, which is exactly what a browser that
// rejects an unsupported constructor argument would do. Without a
// try/catch spanning that whole span, the already-granted stream's tracks
// were never stopped on that path — reaching the literal bug this issue is
// named after ("can leave an earlier getUserMedia stream live
// indefinitely") through a different trigger than the double-click race
// #173 itself describes.
func TestIssueReportPage_MicRecord_RecorderConstructorThrowStopsStream(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeThrowingRecorderScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__trackStopCount = 0;
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({
      getTracks: function() {
        return [
          { stop: function() { window.__trackStopCount++; } },
          { stop: function() { window.__trackStopCount++; } }
        ];
      }
    });
  };
  window.MediaRecorder = function() {
    throw new Error("unsupported MediaRecorder configuration");
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeThrowingRecorderScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake throwing-MediaRecorder script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click the mic button: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the constructor-failure status message: %v", err)
	}
	if !strings.Contains(status, "unsupported MediaRecorder configuration") {
		t.Fatalf("expected the constructor's error to reach the status line, got %q", status)
	}

	// The actual proof this doesn't leak: both tracks of the
	// already-granted stream were stopped, not left live.
	var trackStopCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__trackStopCount`, &trackStopCount)); err != nil {
		t.Fatalf("read the track-stop count: %v", err)
	}
	if trackStopCount != 2 {
		t.Fatalf("expected both stream tracks to be stopped after the MediaRecorder constructor threw, got %d stopped (regression: independent review of uc-infra#173 — the stream would otherwise be left live indefinitely)", trackStopCount)
	}

	var enabledAfterThrow bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-record-btn").disabled`, &enabledAfterThrow,
	)); err != nil {
		t.Fatalf("read the mic button's disabled state after the constructor threw: %v", err)
	}
	if !enabledAfterThrow {
		t.Fatal("expected the mic button to be re-enabled after the constructor threw, so the person can retry")
	}
}

// TestIssueReportPage_MicRecord_StreamEndsOnItsOwnClickDoesNotThrow is the
// regression test for uc-infra#223, found by the independent review of
// uc-infra#220's screen-record fix while checking whether the same bug
// class survives elsewhere in this file's embedded client JS. Unlike
// uc-infra#220 (a *redundant Stop click*), this route to the identical
// InvalidStateError throw doesn't need a second click at all: the mic
// handler's Stop click branch already resets `recording` synchronously
// (unlike the screen-record handler's own Stop branch, which is exactly
// why uc-infra#220's bug needed two clicks to reach) — but nothing before
// this fix reset it when the underlying MediaStream ended on its own (mic
// permission revoked mid-recording, the input device unplugged/disabled,
// or any other browser-initiated track-ended event a real MediaRecorder
// reacts to by transitioning itself to "inactive" and firing onstop).
// Pre-fix, `recording` stayed true and the button kept reading "Stop
// recording" in that case, so the very next click on it — not a redundant
// one, the only one since the button's own last reset — read as a genuine
// Stop and called `.stop()` on an already-inactive recorder, throwing.
//
// This fake models that ordering explicitly: mediaRecorder.state flips to
// "inactive" (the spec-accurate signal a browser-driven stream end
// produces) without onstop firing yet — the same deterministic-timing
// technique TestIssueReportPage_ScreenRecord_RedundantStopClickDoesNotThrow
// already uses for its own equivalent gap, so the single click below lands
// in exactly the window pre-fix code had no guard for.
func TestIssueReportPage_MicRecord_StreamEndsOnItsOwnClickDoesNotThrow(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__uncaughtErrors = [];
  window.addEventListener("error", function(e) {
    window.__uncaughtErrors.push(String((e && e.message) || e));
  });
  window.__recorder = null;
  // Simulates the browser itself ending the recording — the stream's
  // track(s) ended on their own, which a real MediaRecorder reacts to by
  // transitioning to "inactive" synchronously, well before its onstop
  // event necessarily fires. No .stop() call involved, and onstop is
  // deliberately NOT fired here, so this pins the click handler's own
  // guard specifically, not onstop's separate resync (that half is
  // TestIssueReportPage_MicRecord_StreamEndsOnItsOwnResyncsButtonWithoutClick's
  // job, below).
  window.__endStreamOnItsOwn = function() {
    window.__recorder.state = "inactive";
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    this.stop = function() {
      if (self.state === "inactive") {
        throw new DOMException("The MediaRecorder is inactive.", "InvalidStateError");
      }
      self.state = "inactive";
    };
    this.ondataavailable = null;
    window.__recorder = self;
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake spec-accurate MediaRecorder script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue-report page: %v", err)
	}

	// Canary, before the real scenario: same reasoning as
	// TestIssueReportPage_ScreenRecord_RedundantStopClickDoesNotThrow's own
	// canary — proves window.addEventListener("error", ...) actually
	// captures an exception thrown inside a DOM click listener on this
	// page, before trusting its absence as proof of anything below.
	var canaryCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var b = document.createElement("button");
			b.id = "uc-test-canary-btn";
			b.addEventListener("click", function() { throw new Error("canary"); });
			document.body.appendChild(b);
		})();
	`, nil)); err != nil {
		t.Fatalf("inject canary button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-test-canary-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click canary button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__uncaughtErrors.length`, &canaryCount)); err != nil {
		t.Fatalf("read canary error count: %v", err)
	}
	if canaryCount != 1 {
		t.Fatalf("canary check failed: expected the error listener to capture exactly 1 uncaught exception from a throwing click handler, got %d — the rest of this test's \"no uncaught errors\" assertion would be meaningless if this hook isn't actually working", canaryCount)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		document.getElementById("uc-test-canary-btn").remove();
		window.__uncaughtErrors = [];
		void 0;
	`, nil)); err != nil {
		t.Fatalf("remove canary button and reset captured errors: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // start
		t.Fatalf("start mic recording: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for recording to start: %v", err)
	}

	// The stream ends on its own — no click, no .stop() call — leaving
	// the recorder "inactive" while the app's own recording/button state
	// hasn't caught up yet (onstop, which would normally do that, hasn't
	// fired). The one click below is the exact window pre-fix code had no
	// guard for.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__endStreamOnItsOwn(); void 0;`, nil)); err != nil {
		t.Fatalf("simulate the stream ending on its own: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the stale Stop button: %v", err)
	}

	var uncaught []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__uncaughtErrors`, &uncaught)); err != nil {
		t.Fatalf("read captured uncaught errors: %v", err)
	}
	if len(uncaught) != 0 {
		t.Fatalf("expected no uncaught exceptions from clicking Stop after the stream ended on its own, got %v (regression: uc-infra#223 — mediaRecorder.stop() must not be called again once the recorder is already \"inactive\")", uncaught)
	}

	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after the guarded click: %v", err)
	}
}

// TestIssueReportPage_MicRecord_GuardedClickThenSameTakesLateOnstopStillUploads
// composes the two halves of uc-infra#223's fix along the sequence most
// likely to happen for real (independent review): the stream ends on its
// own, the button (not yet resynced) gets one stale click — landing in the
// guarded-skip branch TestIssueReportPage_MicRecord_StreamEndsOnItsOwnClickDoesNotThrow
// pins — and only *afterward* does that same take's own onstop actually
// fire (a real MediaRecorder's onstop is a separate async task from its
// synchronous state transition, so this ordering is the realistic one, not
// a contrived corner case). Neither of the two tests above pins this
// composed ordering on its own: this proves the guarded click doesn't
// leave onstop's own resync/upload broken, and that onstop firing
// afterward doesn't un-do or duplicate what the click already did.
func TestIssueReportPage_MicRecord_GuardedClickThenSameTakesLateOnstopStillUploads(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorder = null;
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    this.stop = function() {
      if (self.state === "inactive") {
        throw new DOMException("The MediaRecorder is inactive.", "InvalidStateError");
      }
      self.state = "inactive";
    };
    this.ondataavailable = null;
    window.__recorder = self;
  };
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return Promise.resolve(new Response("late-onstop-after-guarded-click", { status: 200 }));
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake spec-accurate MediaRecorder script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // start
	); err != nil {
		t.Fatalf("start mic recording: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for recording to start: %v", err)
	}

	// The stream ends on its own, then one stale click lands in the
	// guarded-skip branch before this take's own onstop has fired.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__recorder.state = "inactive"; void 0;`, nil),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("end the stream on its own and click the stale Stop button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after the guarded click: %v", err)
	}

	// Only now does this same take's own onstop actually fire — late, same
	// recorder instance, no newer take involved.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		if (window.__recorder.ondataavailable) { window.__recorder.ondataavailable({ data: new Blob(["fake"]) }); }
		if (window.__recorder.onstop) { window.__recorder.onstop(); }
		void 0;
	`, nil)); err != nil {
		t.Fatalf("fire this take's own late onstop: %v", err)
	}

	// The upload still completes normally...
	var transcriptValue string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var v = document.getElementById("uc-issue-transcript").value; return v && v.length > 0 ? v : null; }`,
		&transcriptValue,
	)); err != nil {
		t.Fatalf("wait for the transcript field to populate: %v", err)
	}
	if transcriptValue != "late-onstop-after-guarded-click" {
		t.Fatalf("transcript value = %q, want %q", transcriptValue, "late-onstop-after-guarded-click")
	}

	// ...and the button, already correctly reset by the guarded click,
	// stays that way — onstop's own reset (mediaRecorder === thisRecorder
	// is still true here, no newer take exists) is idempotent, not a
	// second, contradictory change.
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-record-btn").textContent`, &recordBtnText,
	)); err != nil {
		t.Fatalf("read the button label after this take's late onstop: %v", err)
	}
	if recordBtnText != "Record voice note" {
		t.Fatalf("button label = %q after this take's own late onstop (following the earlier guarded click), want %q", recordBtnText, "Record voice note")
	}
}

// TestIssueReportPage_MicRecord_StreamEndsOnItsOwnResyncsButtonWithoutClick
// pins the other half of uc-infra#223's fix: onstop itself now resets
// recording/the button label, not only the click handler's own guard
// above — so a browser-initiated end of the stream resyncs the button on
// its own, without requiring anyone to click a button that (pre-fix)
// still claimed a recording was in progress. Fires onstop directly rather
// than via a synthetic .stop() call: a real MediaRecorder's onstop and
// ondataavailable are separately fireable async tasks driven by the
// browser (see the "fast Stop-then-Record-again" comment on
// issuereport.go's outer mediaRecorder var), which is what a
// browser-driven end actually looks like — not a user clicking Stop.
func TestIssueReportPage_MicRecord_StreamEndsOnItsOwnResyncsButtonWithoutClick(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorder = null;
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    this.stop = function() { self.state = "inactive"; };
    this.ondataavailable = null;
    window.__recorder = self;
  };
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return Promise.resolve(new Response("browser ended the stream", { status: 200 }));
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // start
	); err != nil {
		t.Fatalf("start mic recording: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for recording to start: %v", err)
	}

	// The stream ends on its own: the recorder transitions to "inactive"
	// and fires onstop, with no click and no explicit .stop() call
	// anywhere in this test.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		window.__recorder.state = "inactive";
		if (window.__recorder.ondataavailable) { window.__recorder.ondataavailable({ data: new Blob(["fake"]) }); }
		if (window.__recorder.onstop) { window.__recorder.onstop(); }
		void 0;
	`, nil)); err != nil {
		t.Fatalf("simulate the stream ending on its own and firing onstop: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to resync to \"Record voice note\" without a click (regression: uc-infra#223 — onstop must resync recording/the button label on a browser-initiated end, not only on an explicit Stop click): %v", err)
	}

	// The transcription path still completes normally — the resync must
	// not have interfered with onstop's own upload logic.
	var transcriptValue string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var v = document.getElementById("uc-issue-transcript").value; return v && v.length > 0 ? v : null; }`,
		&transcriptValue,
	)); err != nil {
		t.Fatalf("wait for the transcript field to populate: %v", err)
	}
	if transcriptValue != "browser ended the stream" {
		t.Fatalf("transcript value = %q, want %q", transcriptValue, "browser ended the stream")
	}
}

// TestIssueReportPage_MicRecord_LateOnstopFromEndedTakeDoesNotResetNewerTakesButton
// is the defense-in-depth counterpart to
// TestIssueReportPage_MicRecord_StreamEndsOnItsOwnResyncsButtonWithoutClick:
// uc-infra#223's onstop-side resync is guarded on `mediaRecorder ===
// thisRecorder` specifically so a *stale* take's onstop — one whose stream
// ended on its own well after a newer take has already started — can't
// stomp the newer take's own "Stop recording" button state. Same "fast
// Stop-then-Record-again" shape
// TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes already
// pins for upload content; this pins the button label specifically, which
// that test never asserted on mid-sequence.
func TestIssueReportPage_MicRecord_LateOnstopFromEndedTakeDoesNotResetNewerTakesButton(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorders = [];
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    // Deliberately does nothing on .stop() beyond flipping state — this
    // take's onstop is fired later, explicitly, by the test.
    this.stop = function() { self.state = "inactive"; };
    this.ondataavailable = null;
    window.__recorders.push(self);
  };
  window.__fireLateOnstop = function(index) {
    var self = window.__recorders[index];
    self.state = "inactive";
    if (self.onstop) { self.onstop(); }
  };
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return Promise.resolve(new Response("ok", { status: 200 }));
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred-onstop media script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
	); err != nil {
		t.Fatalf("start take #1: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #1 to start: %v", err)
	}

	// Take #1 ends on its own (device unplugged mid-recording, say) — but
	// nothing observes it yet, and no click happens: onstop for take #1
	// is fired later, explicitly, below, well after take #2 has started.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__recorders[0].state = "inactive"; void 0;`, nil)); err != nil {
		t.Fatalf("end take #1's stream on its own, without firing onstop yet: %v", err)
	}

	// This click does NOT take the ordinary .stop() path
	// TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes's
	// own "take #1: stop" click does — take #1's recorder is already
	// "inactive" (set above), so this lands in the guarded-skip branch
	// TestIssueReportPage_MicRecord_StreamEndsOnItsOwnClickDoesNotThrow
	// pins (mediaRecorder.stop() is never called), which still resets
	// recording/the button label the same way an ordinary Stop does —
	// that's what lets take #2 start normally right after.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #1: guarded-skip stop
		t.Fatalf("stop take #1: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #1: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: start
		t.Fatalf("start take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #2 to start: %v", err)
	}

	// Take #1's onstop finally fires — late, after take #2 has already
	// started and reassigned issuereport.go's shared mediaRecorder
	// variable to take #2's instance. Pre-fix (an unconditional reset
	// inside onstop) this would stomp take #2's own "Stop recording"
	// state; the mediaRecorder === thisRecorder guard must skip it.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireLateOnstop(0); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #1's late onstop: %v", err)
	}

	var afterLateOnstop string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-record-btn").textContent`, &afterLateOnstop,
	)); err != nil {
		t.Fatalf("read the button label after take #1's late onstop: %v", err)
	}
	if afterLateOnstop != "Stop recording" {
		t.Fatalf("button label = %q after take #1's late onstop, want %q — take #1's stale onstop reset take #2's still-active recording state (regression: uc-infra#223's onstop-side fix must guard on mediaRecorder === thisRecorder)", afterLateOnstop, "Stop recording")
	}

	// Take #2 itself still completes normally afterward.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: stop
		t.Fatalf("stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #2: %v", err)
	}
}

// TestIssueReportPage_MicRecord_LateOnstopFromEndedTakeDoesNotClobberStatusOrTranscript
// is the companion to LateOnstopFromEndedTakeDoesNotResetNewerTakesButton
// just above, for uc-infra#221 specifically (independent review of this
// fix's first draft): that test already builds the exact interleaving this
// one needs — take #1's stream ends on its own and its `onstop` is fired
// LATE, well after take #2 has already started — but only ever reads
// `#uc-issue-record-btn`, never the status line or transcript field, so it
// pins uc-infra#223's button-label guard without touching any of uc-infra
// #221's three guarded write sites at all. The two StaleTranscribe* tests
// below this one use a DIFFERENT (synchronous-onstop) fake MediaRecorder
// specifically so they can control fetch resolution timing independently —
// which means `mediaRecorder === thisRecorder` is trivially true at their
// own "Transcribing…" write (issuereport.go's line just after the blob is
// built), and deleting that particular guard would leave both of them, and
// every other existing test, still green. Only a deferred-`onstop` harness
// like this one's actually exercises that specific guard.
func TestIssueReportPage_MicRecord_LateOnstopFromEndedTakeDoesNotClobberStatusOrTranscript(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorders = [];
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    // Deliberately does nothing on .stop() beyond flipping state — this
    // take's onstop is fired later, explicitly, by the test (same fake
    // shape as LateOnstopFromEndedTakeDoesNotResetNewerTakesButton above).
    this.stop = function() { self.state = "inactive"; };
    this.ondataavailable = null;
    window.__recorders.push(self);
  };
  window.__fireLateOnstop = function(index) {
    var self = window.__recorders[index];
    self.state = "inactive";
    if (self.onstop) { self.onstop(); }
  };
  window.__transcribeCallCount = 0;
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      window.__transcribeCallCount++;
      return Promise.resolve(new Response("LATE-ONSTOP-STALE-MARKER", { status: 200 }));
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred-onstop media/fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
	); err != nil {
		t.Fatalf("start take #1: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #1 to start: %v", err)
	}

	// Take #1 ends on its own, without firing onstop yet — same setup as
	// LateOnstopFromEndedTakeDoesNotResetNewerTakesButton above.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__recorders[0].state = "inactive"; void 0;`, nil)); err != nil {
		t.Fatalf("end take #1's stream on its own, without firing onstop yet: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #1: guarded-skip stop
		t.Fatalf("stop take #1: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #1: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: start
		t.Fatalf("start take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #2 to start: %v", err)
	}
	var statusBeforeLateOnstop string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusBeforeLateOnstop, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line before take #1's late onstop: %v", err)
	}
	if statusBeforeLateOnstop != "" {
		t.Fatalf("status line = %q with take #2 actively recording, want empty (idle) before even firing take #1's late onstop", statusBeforeLateOnstop)
	}

	// Take #1's onstop finally fires — late, after take #2 has already
	// started and reassigned issuereport.go's shared mediaRecorder variable
	// to take #2's instance. Pre-fix, this unconditionally wrote
	// "Transcribing…" over take #2's own idle status line, then (once the
	// fake fetch above resolves) unconditionally overwrote the transcript/
	// description fields too — exactly the uc-infra#221 bug, reached via a
	// genuinely late onstop rather than a controlled-fetch-resolution
	// timing trick.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireLateOnstop(0); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #1's late onstop: %v", err)
	}

	var statusRightAfterLateOnstop string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusRightAfterLateOnstop, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line immediately after take #1's late onstop: %v", err)
	}
	if statusRightAfterLateOnstop != "" {
		t.Fatalf(`status line = %q immediately after take #1's late onstop (before its fetch even resolved), want empty (idle) — a stale take's "Transcribing…" write must be guarded the same way its button-label reset already is (regression: uc-infra#221)`, statusRightAfterLateOnstop)
	}

	// Flush the late onstop's own fetch-and-respond promise chain (same
	// real-macrotask-boundary technique the StaleTranscribe* tests below
	// use) before asserting the stale result never landed.
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush take #1's late-onstop transcribe call: %v", err)
	}
	var transcribeCallCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeCallCount`, &transcribeCallCount)); err != nil {
		t.Fatalf("read transcribe call count: %v", err)
	}
	if transcribeCallCount != 1 {
		t.Fatalf("transcribe call count = %d, want exactly 1 (take #1's late onstop still uploads — dropping the STALE UI write is not the same as skipping the request itself)", transcribeCallCount)
	}
	var transcriptAfterLateOnstop, descriptionAfterLateOnstop, statusAfterFlush string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-transcript`, &transcriptAfterLateOnstop, chromedp.ByQuery)); err != nil {
		t.Fatalf("read transcript field after take #1's late-onstop transcribe resolved: %v", err)
	}
	if transcriptAfterLateOnstop != "" {
		t.Fatalf("transcript field = %q after take #1's late-onstop transcribe resolved, want empty — a stale take's result must be dropped (regression: uc-infra#221)", transcriptAfterLateOnstop)
	}
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-description`, &descriptionAfterLateOnstop, chromedp.ByQuery)); err != nil {
		t.Fatalf("read description field after take #1's late-onstop transcribe resolved: %v", err)
	}
	if descriptionAfterLateOnstop != "" {
		t.Fatalf("description field = %q after take #1's late-onstop transcribe resolved, want empty — a stale take's result must be dropped (regression: uc-infra#221)", descriptionAfterLateOnstop)
	}
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterFlush, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #1's late-onstop transcribe resolved: %v", err)
	}
	if statusAfterFlush != "" {
		t.Fatalf("status line = %q after take #1's late-onstop transcribe resolved, want empty (idle) — take #2 is still actively recording", statusAfterFlush)
	}

	// Take #2 still completes normally afterward — the guard must drop a
	// STALE take's result, not disable the fields for every take from then
	// on. This fake's onstop is deferred (same as take #1's above), so
	// stopping take #2 needs its own explicit __fireLateOnstop call too —
	// unlike the synchronous-onstop fakes elsewhere in this file, .stop()
	// alone does not trigger it here.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: stop
		t.Fatalf("stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireLateOnstop(1); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #2's onstop: %v", err)
	}
	var transcriptAfterTake2 string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var v = document.getElementById("uc-issue-transcript").value;
			return v === "LATE-ONSTOP-STALE-MARKER" ? v : null;
		}`,
		&transcriptAfterTake2,
	)); err != nil {
		t.Fatalf("wait for take #2's own transcript to apply: %v", err)
	}
	if transcriptAfterTake2 != "LATE-ONSTOP-STALE-MARKER" {
		t.Fatalf("take #2's own transcribe result did not apply after it stopped normally — the guard must not disable the fields for every take going forward, got %q", transcriptAfterTake2)
	}
}

// TestIssueReportPage_MicRecord_StaleTranscribeSuccessDoesNotClobberNewerTake
// is the real-browser regression test for uc-infra#221: independent review
// of uc-infra#196's fix (which stopped two overlapping takes' *audio bytes*
// from mixing) found that the same now-legitimized "two takes in flight"
// scenario still let one take's *transcribe result* clobber another's, even
// though the underlying data was by then correctly isolated. Pre-fix,
// issuereport.go's transcribe `.then()` wrote `transcriptEl`/`descriptionEl`
// unconditionally — whichever take's `/issue-report/transcribe` request
// happened to resolve LAST won, regardless of which take the user actually
// started most recently or was still looking at.
//
// window.fetch is faked to defer resolution of each transcribe call
// separately (`window.__transcribeResolvers`, a resolver per call, not
// fired until the test explicitly calls `window.__resolveTranscribe`) —
// deterministic control over which take's result arrives first, the same
// "control the moment explicitly rather than race a real timer" approach
// TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes already
// uses for onstop/ondataavailable. Each take's own MediaRecorder fires
// ondataavailable/onstop synchronously on .stop() (same simple fake
// TestIssueReportPage_TranscriptAndDescriptionAreClampedOnTranscribe uses) —
// only the fetch response itself is held back, since that is the part this
// bug's timing actually depends on.
func TestIssueReportPage_MicRecord_StaleTranscribeSuccessDoesNotClobberNewerTake(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
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
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
  window.__resolveTranscribe = function(index, text) {
    window.__transcribeResolvers[index](new Response(text, { status: 200 }));
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}

	// Take #2 starts — and, per issuereport.go's own Record-click handler,
	// takes over the shared status line — while take #1's transcribe call
	// is still unresolved.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #2: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #2: stop -> transcribe call #1, also left pending
	); err != nil {
		t.Fatalf("start then stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 2 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #2's transcribe call to be pending: %v", err)
	}

	// Take #1's call resolves LAST-STARTED-FIRST-RESOLVED relative to real
	// time, but it's the STALE take: by the time this fires, take #2 has
	// already started (and stopped), so take #1 no longer owns the shared
	// fields. Pre-fix, this unconditionally overwrote transcriptEl/
	// descriptionEl anyway.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__resolveTranscribe(0, "STALE-TAKE-ONE-TRANSCRIPT"); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #1's (stale) transcribe call: %v", err)
	}

	// Give the stale resolution's promise chain (fetch -> .then -> .then,
	// each a real microtask hop) a chance to fully run before asserting it
	// did NOT land — polling for its own absence isn't possible (there's
	// nothing to wait FOR), so this instead awaits a real macrotask
	// boundary (a setTimeout) from inside the page: every microtask queued
	// by the earlier Evaluate call above, however many hops deep, is
	// guaranteed to have drained before a subsequent timer callback fires
	// (a plain `return 1` round trip is NOT equivalent — proven false
	// pre-fix, where it let this exact assertion pass by accident because
	// resp.text()'s own promise hadn't settled yet when read that early).
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush the stale resolution's promise chain: %v", err)
	}
	var transcriptAfterStale string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-transcript`, &transcriptAfterStale, chromedp.ByQuery)); err != nil {
		t.Fatalf("read transcript field after the stale resolution: %v", err)
	}
	if strings.Contains(transcriptAfterStale, "STALE-TAKE-ONE") {
		t.Fatalf("transcript field = %q after take #1's STALE transcribe result resolved — a stale, superseded take's result must be dropped, not applied (regression: uc-infra#221)", transcriptAfterStale)
	}
	var descriptionAfterStale string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-description`, &descriptionAfterStale, chromedp.ByQuery)); err != nil {
		t.Fatalf("read description field after the stale resolution: %v", err)
	}
	if strings.Contains(descriptionAfterStale, "STALE-TAKE-ONE") {
		t.Fatalf("description field = %q after take #1's STALE transcribe result resolved — a stale, superseded take's result must be dropped, not applied (regression: uc-infra#221)", descriptionAfterStale)
	}

	// Take #2's own (current) call still resolves normally afterward — the
	// guard must drop a STALE take's result, not disable the field for
	// every take from then on.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__resolveTranscribe(1, "CURRENT-TAKE-TWO-TRANSCRIPT"); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #2's (current) transcribe call: %v", err)
	}
	var transcriptAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var v = document.getElementById("uc-issue-transcript").value;
			return v === "CURRENT-TAKE-TWO-TRANSCRIPT" ? v : null;
		}`,
		&transcriptAfterCurrent,
	)); err != nil {
		t.Fatalf("wait for take #2's own (current) transcript to apply: %v", err)
	}
	var descriptionAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-description`, &descriptionAfterCurrent, chromedp.ByQuery)); err != nil {
		t.Fatalf("read description field after take #2's current resolution: %v", err)
	}
	// Exact equality, not Contains (independent review): descriptionEl
	// starts empty, so a leaked stale write from take #1 would land FIRST
	// (its resolver fires strictly before take #2's, above) and take #2's
	// own write would then *append* to it (issuereport.go's own "combined"
	// logic), producing "STALE-TAKE-ONE-TRANSCRIPT\n\nCURRENT-TAKE-TWO-
	// TRANSCRIPT" — which a mere Contains(..., "CURRENT-TAKE-TWO-
	// TRANSCRIPT") check would NOT catch, silently losing this as a second
	// line of defense behind the earlier, timing-sensitive assertion.
	if descriptionAfterCurrent != "CURRENT-TAKE-TWO-TRANSCRIPT" {
		t.Fatalf("description field = %q after take #2's own transcribe result resolved, want exactly %q (a mismatch here — e.g. a stale prefix — means the earlier stale-write assertion above didn't actually catch a leak, only this equality check would)", descriptionAfterCurrent, "CURRENT-TAKE-TWO-TRANSCRIPT")
	}
	var statusAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterCurrent, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #2's current resolution: %v", err)
	}
	if statusAfterCurrent != "" {
		t.Fatalf("status line = %q after take #2's own transcribe result resolved, want empty (idle)", statusAfterCurrent)
	}
}

// TestIssueReportPage_MicRecord_StaleTranscribeErrorDoesNotClobberStatus is
// the companion regression test for uc-infra#221's error path: a stale
// take's transcribe request can fail (network blip, a disabled
// speechassist.Client) just as easily as it can succeed, and pre-fix that
// failure's message was written to the shared status line just as
// unconditionally as a success — capable of showing a confusing, unrelated
// error on top of a newer take that is, from the user's point of view,
// currently recording fine.
func TestIssueReportPage_MicRecord_StaleTranscribeErrorDoesNotClobberStatus(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
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
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
  // Same httpx {"data":null,"error":"..."} envelope shape
  // TestIssueReportPage_VoiceRecordShowsCleanErrorNotRawJSON already fakes,
  // so this exercises the real extractErrorMessage parsing path, not a
  // synthetic shortcut.
  window.__rejectTranscribe = function(index, errMsg) {
    window.__transcribeResolvers[index](
      new Response(JSON.stringify({ data: null, error: errMsg }), { status: 500 })
    );
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}

	// Take #2 starts (still recording, not yet stopped) — takes over the
	// shared status line, which the Record-click handler clears.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: start
		t.Fatalf("start take #2: %v", err)
	}
	var buttonAfterTake2Start string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-btn").textContent;
			return t === "Stop recording" ? t : null;
		}`,
		&buttonAfterTake2Start,
	)); err != nil {
		t.Fatalf("wait for take #2 to start: %v", err)
	}

	// Take #1's (stale) transcribe call now fails.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__rejectTranscribe(0, "stale asr failure"); void 0;`, nil)); err != nil {
		t.Fatalf("reject take #1's (stale) transcribe call: %v", err)
	}

	// Flush the rejection's promise chain (same real-macrotask-boundary
	// technique as the success-path test's own comment explains — a bare
	// round trip is NOT sufficient here either) before asserting the stale
	// error never reached the status line — take #2 is still actively
	// recording, so the status line must stay idle ("").
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush the stale rejection's promise chain: %v", err)
	}
	var statusAfterStaleError string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterStaleError, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #1's stale rejection: %v", err)
	}
	if strings.Contains(statusAfterStaleError, "stale asr failure") {
		t.Fatalf("status line = %q after take #1's STALE transcribe call failed — a stale, superseded take's error must be dropped, not shown over a still-active newer take (regression: uc-infra#221)", statusAfterStaleError)
	}

	// Take #2 still completes normally afterward — the guard must drop a
	// STALE take's error, not leave the newer, current take unable to ever
	// show its own status.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: stop
		t.Fatalf("stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 2 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #2's transcribe call to be pending: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__rejectTranscribe(1, "take two's own failure"); void 0;`, nil)); err != nil {
		t.Fatalf("reject take #2's (current) transcribe call: %v", err)
	}
	var statusAfterCurrentError string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t === "take two's own failure" ? t : null;
		}`,
		&statusAfterCurrentError,
	)); err != nil {
		t.Fatalf("wait for take #2's own (current) transcribe error to surface: %v", err)
	}
}

// TestIssueReportPage_MicRecord_FailedRestartDoesNotDropPriorTakesTranscript
// is the real-browser regression test for uc-infra#238's "Gap A": an
// independent review of uc-infra#221's fix (which guarded the three
// transcribe-write sites on `mediaRecorder === thisRecorder`) found that
// recorder *identity* is the wrong staleness proxy — it changes the instant
// `mediaRecorder = new MediaRecorder(stream)` runs, even when the very next
// line, `mediaRecorder.start()`, goes on to throw. Pre-fix, that left a
// still-in-flight EARLIER take's entirely legitimate transcript silently
// dropped, even though the later take never actually started recording and
// so never genuinely superseded it — "the user sees take #2's start error
// and take #1's voice note vanishes without a trace," per the issue's own
// description. The fix (recordAttemptGeneration/recordingGeneration,
// documented on issuereport.go's mic-handler outer scope) closes this by
// only advancing recordingGeneration — the counter transcript/description
// writes are guarded on — once a take's mediaRecorder.start() has actually
// succeeded, so a later take's failed restart never invalidates an earlier
// take's still-real data.
func TestIssueReportPage_MicRecord_FailedRestartDoesNotDropPriorTakesTranscript(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  // Take #1's MediaRecorder works normally (synchronous onstop, same shape
  // as the StaleTranscribe* tests above). Take #2's construction succeeds
  // (issuereport.go's shared mediaRecorder variable IS reassigned to it —
  // the exact pre-fix trigger for Gap A) but its own .start() throws,
  // modeling "something between construction and start() can throw" per
  // issuereport.go's own try/catch comment — reached here via .start()
  // itself, not the constructor (TestIssueReportPage_MicRecord_
  // RecorderConstructorThrowStopsStream above already covers the
  // constructor-throws case, which does NOT reassign mediaRecorder at all
  // and so was never actually Gap A's trigger).
  window.__recorderCount = 0;
  window.MediaRecorder = function() {
    var self = this;
    window.__recorderCount++;
    if (window.__recorderCount === 1) {
      this.start = function() {};
      this.stop = function() {
        if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
        if (self.onstop) { self.onstop(); }
      };
    } else {
      this.start = function() { throw new Error("second-take-start-failure"); };
      this.stop = function() {};
    }
  };
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}

	// Take #2's Record click: getUserMedia resolves, MediaRecorder is
	// constructed (reassigning issuereport.go's shared mediaRecorder
	// variable), but its own .start() throws synchronously — landing in
	// issuereport.go's inner catch, which writes this take's own error to
	// the status line unconditionally (nothing to guard: it's the newest
	// thing happening, and nothing else could have raced it — see
	// issuereport.go's own recordAttemptGeneration comment).
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("attempt (and fail) take #2: %v", err)
	}
	// Polls for the SPECIFIC expected text, not merely "non-empty"
	// (independent review): take #1's own "Transcribing…" write already
	// left the status line non-empty before this click, so a loose
	// non-empty predicate could resolve on that stale leftover text
	// instead of actually waiting for take #2's own failure to land —
	// which would let this test pass even against a mutant that
	// re-introduces Gap A, purely on incidental CDP round-trip timing.
	var statusAfterFailedRestart string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.indexOf("second-take-start-failure") !== -1 ? t : null;
		}`,
		&statusAfterFailedRestart,
	)); err != nil {
		t.Fatalf("wait for take #2's start-failure status message: %v", err)
	}
	if !strings.Contains(statusAfterFailedRestart, "second-take-start-failure") {
		t.Fatalf("status line = %q after take #2's failed restart, want it to contain take #2's own start error", statusAfterFailedRestart)
	}

	// Take #1's transcribe call, still pending throughout, now resolves.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__resolveTranscribe = function(index, text) { window.__transcribeResolvers[index](new Response(text, { status: 200 })); }; window.__resolveTranscribe(0, "TAKE-ONE-TRANSCRIPT"); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #1's transcribe call: %v", err)
	}

	// Flush the resolution's promise chain (same real-macrotask-boundary
	// technique the StaleTranscribe* tests above use — a bare round trip is
	// not sufficient here either).
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush take #1's transcribe resolution: %v", err)
	}

	var transcriptAfterResolve string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-transcript`, &transcriptAfterResolve, chromedp.ByQuery)); err != nil {
		t.Fatalf("read transcript field after take #1's transcribe resolved: %v", err)
	}
	if transcriptAfterResolve != "TAKE-ONE-TRANSCRIPT" {
		t.Fatalf("transcript field = %q after take #1's transcribe resolved, want %q — take #1 never got superseded by an ACTUAL new recording (take #2's own .start() threw), so its real, legitimate result must not be dropped (regression: uc-infra#238 Gap A)", transcriptAfterResolve, "TAKE-ONE-TRANSCRIPT")
	}
	var descriptionAfterResolve string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-description`, &descriptionAfterResolve, chromedp.ByQuery)); err != nil {
		t.Fatalf("read description field after take #1's transcribe resolved: %v", err)
	}
	if descriptionAfterResolve != "TAKE-ONE-TRANSCRIPT" {
		t.Fatalf("description field = %q after take #1's transcribe resolved, want %q (regression: uc-infra#238 Gap A)", descriptionAfterResolve, "TAKE-ONE-TRANSCRIPT")
	}

	// The status line, by contrast, must still show take #2's own error —
	// unlike the transcript/description fields, it is guarded on
	// recordAttemptGeneration (the user's most recent Record click, which
	// take #2's click already claimed), not recordingGeneration, so take
	// #1's late-arriving success must NOT silently erase take #2's
	// still-relevant "you need to try again" message.
	var statusAfterResolve string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterResolve, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #1's transcribe resolved: %v", err)
	}
	if !strings.Contains(statusAfterResolve, "second-take-start-failure") {
		t.Fatalf("status line = %q after take #1's transcribe resolved, want it to still contain take #2's own start error — take #1's late success must not silently erase it (regression: uc-infra#238 Gap A)", statusAfterResolve)
	}
}

// TestIssueReportPage_MicRecord_RejectedPermissionSurvivesPriorTakesLateSuccess
// is the real-browser regression test for uc-infra#238's "Gap B" — the
// mirror image of Gap A above: a getUserMedia rejection (mic permission
// denied for a second take) never reassigns issuereport.go's shared
// mediaRecorder variable at all (there is no new MediaRecorder to assign;
// the permission prompt never got that far), so the pre-fix
// `mediaRecorder === thisRecorder` guard stayed TRUE for the still-in-flight
// EARLIER take — meaning that take's later, entirely unrelated transcribe
// success reached its unconditional `statusEl.textContent = ""`/error write
// and silently wiped the "Permission denied" message the user had just been
// shown. The fix's recordAttemptGeneration counter closes this because it
// advances synchronously on every Record click — including one that goes on
// to fail — so the earlier take's own status-line write is correctly
// recognized as stale once a newer click has happened, independent of
// whether that newer click ever produced a MediaRecorder at all.
func TestIssueReportPage_MicRecord_RejectedPermissionSurvivesPriorTakesLateSuccess(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getUserMediaCallCount = 0;
  navigator.mediaDevices.getUserMedia = function() {
    window.__getUserMediaCallCount++;
    if (window.__getUserMediaCallCount === 1) {
      return Promise.resolve({ getTracks: function() { return []; } });
    }
    return Promise.reject(new Error("Permission denied"));
  };
  // Only ever constructed once (take #1) — take #2 never gets past
  // getUserMedia's own rejection, so it never reaches MediaRecorder at all.
  window.MediaRecorder = function() {
    var self = this;
    this.start = function() {};
    this.stop = function() {
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}

	// Take #2's Record click: getUserMedia itself rejects (permission
	// denied) — issuereport.go's outer .catch writes this unconditionally
	// (again, nothing to guard: it's the newest thing happening).
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("attempt (and get denied) take #2: %v", err)
	}
	// Polls for the SPECIFIC expected text, not merely "non-empty" — same
	// reasoning as the Gap A test's own comment above: a loose predicate
	// could resolve on take #1's stale "Transcribing…" leftover instead of
	// actually waiting for take #2's own denial to land.
	var statusAfterDenial string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.indexOf("Permission denied") !== -1 ? t : null;
		}`,
		&statusAfterDenial,
	)); err != nil {
		t.Fatalf("wait for take #2's permission-denied status message: %v", err)
	}
	if !strings.Contains(statusAfterDenial, "Permission denied") {
		t.Fatalf("status line = %q after take #2's denied permission, want it to contain the denial", statusAfterDenial)
	}

	// Take #1's transcribe call, still pending throughout, now resolves.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeResolvers[0](new Response("TAKE-ONE-TRANSCRIPT", { status: 200 })); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #1's transcribe call: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush take #1's transcribe resolution: %v", err)
	}

	var transcriptAfterResolve string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-transcript`, &transcriptAfterResolve, chromedp.ByQuery)); err != nil {
		t.Fatalf("read transcript field after take #1's transcribe resolved: %v", err)
	}
	if transcriptAfterResolve != "TAKE-ONE-TRANSCRIPT" {
		t.Fatalf("transcript field = %q after take #1's transcribe resolved, want %q — a denied permission for a take that never even reached MediaRecorder must not cause take #1's real, legitimate result to be dropped (regression: uc-infra#238 Gap B)", transcriptAfterResolve, "TAKE-ONE-TRANSCRIPT")
	}

	// The status line must still show the permission denial — take #1's
	// late success must not silently erase it (this is the actual bug: the
	// pre-fix identity guard stayed true here since mediaRecorder was never
	// reassigned by a rejected getUserMedia call).
	var statusAfterResolve string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterResolve, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #1's transcribe resolved: %v", err)
	}
	if !strings.Contains(statusAfterResolve, "Permission denied") {
		t.Fatalf("status line = %q after take #1's transcribe resolved, want it to still contain the permission denial — take #1's late success must not silently wipe it (regression: uc-infra#238 Gap B)", statusAfterResolve)
	}
}

// TestIssueReportPage_MicRecord_PendingRestartDoesNotStrandStaleTranscribingStatus
// is the regression test for a NEW bug the independent review of uc-infra
// #238's first draft found, introduced by that draft itself (never shipped):
// recordAttemptGeneration claimed ownership of the status line synchronously
// at Record-click time, but nothing was written to the status line until
// getUserMedia's own permission prompt actually resolved — which can stay
// pending indefinitely (the person simply hasn't answered the browser's mic
// permission prompt yet). During that whole window, an OLDER take's own
// still-in-flight transcribe result now correctly fails the
// recordAttemptGeneration guard (a newer click has already claimed it) but
// nothing NEW has been written either, so the status line was left
// stranded on that older take's stale "Transcribing…" — or, worse, an older
// take's real transcribe ERROR was silently swallowed instead of shown —
// for as long as the prompt sat unanswered. The fix: the click handler now
// clears the status line synchronously, in the same turn as the
// recordAttemptGeneration bump, so "claim the status line" and "show
// something for it" happen atomically. This test drives exactly that
// window using the deferred-getUserMedia pattern
// TestIssueReportPage_MicRecord_DoubleClickDoesNotStartTwoStreams above
// already uses, so the permission prompt can be held open under direct
// control while asserting on the status line.
func TestIssueReportPage_MicRecord_PendingRestartDoesNotStrandStaleTranscribingStatus(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getUserMediaCallCount = 0;
  window.__resolveGetUserMedia = null;
  navigator.mediaDevices.getUserMedia = function() {
    window.__getUserMediaCallCount++;
    // Take #1's own call resolves immediately; take #2's is held open
    // under this test's direct control, modeling an unanswered permission
    // prompt.
    if (window.__getUserMediaCallCount === 1) {
      return Promise.resolve({ getTracks: function() { return []; } });
    }
    return new Promise(function(resolve) {
      window.__resolveGetUserMedia = resolve;
    });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.start = function() {};
    this.stop = function() {
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}
	var statusBeforeTake2Click string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusBeforeTake2Click, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line before take #2's click: %v", err)
	}
	if !strings.Contains(statusBeforeTake2Click, "Transcribing") {
		t.Fatalf("status line = %q before take #2's click, want it to show take #1's own \"Transcribing…\" (test setup check)", statusBeforeTake2Click)
	}

	// Take #2's Record click: getUserMedia's own permission prompt is now
	// held open — modeling it sitting unanswered. This alone must already
	// clear take #1's stale "Transcribing…" (the fix's own guarantee),
	// well before the prompt itself is ever resolved or rejected.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click Record for take #2 (permission prompt held open): %v", err)
	}
	var statusWhilePending string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t === "" ? "EMPTY" : null;
		}`,
		&statusWhilePending,
	)); err != nil {
		t.Fatalf("wait for the status line to clear once take #2 claims it: %v", err)
	}

	// Take #1's stale transcribe call now resolves WHILE take #2's
	// permission prompt is STILL pending — the exact window the bug lived
	// in. Pre-fix, this would have re-shown "Transcribing…" or silently
	// swallowed an error; post-fix, take #1's write is correctly guarded
	// (recordAttemptGeneration already moved on) and the line stays empty,
	// not stale.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeResolvers[0](new Response("STALE-TAKE-ONE", { status: 200 })); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #1's stale transcribe call while take #2's prompt is still pending: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush take #1's stale resolution: %v", err)
	}
	var statusStillPending string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusStillPending, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #1's stale resolution, while take #2's prompt is still pending: %v", err)
	}
	if statusStillPending != "" {
		t.Fatalf(`status line = %q after take #1's stale transcribe resolved while take #2's permission prompt was still pending, want empty — a stale take's write must not strand the line on old text while a newer attempt's prompt sits unanswered (regression: found by independent review of uc-infra#238's first draft)`, statusStillPending)
	}

	// The prompt finally resolves (permission granted) — recording must
	// still start normally, proving the fix didn't just suppress writes
	// generally.
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`window.__resolveGetUserMedia({ getTracks: function() { return []; } }); void 0;`, nil,
	)); err != nil {
		t.Fatalf("resolve take #2's permission prompt: %v", err)
	}
	var recordBtnTextAfterResolve string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-btn").textContent;
			return t === "Stop recording" ? t : null;
		}`,
		&recordBtnTextAfterResolve,
	)); err != nil {
		t.Fatalf("wait for take #2 to actually start recording once its prompt resolves: %v", err)
	}
}

// TestIssueReportPage_MicRecord_FailedMiddleTakeDoesNotSkewGenerationsAcrossThreeTakes
// is the independent review's own follow-up for uc-infra#238: the two Gap
// tests above each only ever involve two takes. This one pins the axis the
// fix actually changed — recordAttemptGeneration/recordingGeneration
// advancing independently — across THREE: an in-flight take #1, a take #2
// whose own start() fails (so it must NOT consume a recordingGeneration
// slot), and a take #3 that starts and completes normally. Confirms
// recordingGeneration correctly skips the failed middle take (take #3
// becomes generation 2, not 3) and that take #1's stale result is still
// correctly dropped once a LATER take has genuinely superseded it.
func TestIssueReportPage_MicRecord_FailedMiddleTakeDoesNotSkewGenerationsAcrossThreeTakes(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  // Instance #1 (take #1) and instance #3 (take #3) work normally;
  // instance #2 (take #2) constructs fine but its own .start() throws —
  // same shape as the Gap A test above, just as the MIDDLE of three takes
  // here instead of the last one.
  window.__recorderCount = 0;
  window.MediaRecorder = function() {
    var self = this;
    window.__recorderCount++;
    if (window.__recorderCount === 2) {
      this.start = function() { throw new Error("middle-take-failure"); };
      this.stop = function() {};
    } else {
      this.start = function() {};
      this.stop = function() {
        if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
        if (self.onstop) { self.onstop(); }
      };
    }
  };
  window.__transcribeResolvers = [];
  var realFetch = window.fetch;
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return new Promise(function(resolve) {
        window.__transcribeResolvers.push(resolve);
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake media/deferred-fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: stop -> transcribe call #0, left pending
	); err != nil {
		t.Fatalf("start then stop take #1: %v", err)
	}
	var resolverCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 1 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe call to be pending: %v", err)
	}

	// Take #2: attempt (and fail) — must NOT advance recordingGeneration.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("attempt (and fail) take #2: %v", err)
	}
	var statusAfterTake2 string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-record-status").textContent;
			return t && t.indexOf("middle-take-failure") !== -1 ? t : null;
		}`,
		&statusAfterTake2,
	)); err != nil {
		t.Fatalf("wait for take #2's own start-failure status message: %v", err)
	}

	// Take #3: starts and completes normally — its own transcribe call is
	// call index #1 (take #2 never reached fetch at all).
	if err := chromedp.Run(ctx,
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #3: start
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #3: stop -> transcribe call #1, left pending
	); err != nil {
		t.Fatalf("start then stop take #3: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeResolvers.length >= 2 ? window.__transcribeResolvers.length : null; }`,
		&resolverCount,
	)); err != nil {
		t.Fatalf("wait for take #3's transcribe call to be pending: %v", err)
	}

	// Take #1's (doubly-stale) call resolves first — must be dropped, same
	// as the two-take StaleTranscribeSuccess test above, just with a
	// failed take now sitting in between it and the take that actually
	// supersedes it.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeResolvers[0](new Response("STALE-TAKE-ONE", { status: 200 })); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #1's (stale) transcribe call: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`new Promise(function(resolve) { setTimeout(resolve, 50); })`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return p.WithAwaitPromise(true)
		},
	)); err != nil {
		t.Fatalf("flush take #1's stale resolution: %v", err)
	}
	var transcriptAfterStale string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-transcript`, &transcriptAfterStale, chromedp.ByQuery)); err != nil {
		t.Fatalf("read transcript field after take #1's stale resolution: %v", err)
	}
	if strings.Contains(transcriptAfterStale, "STALE-TAKE-ONE") {
		t.Fatalf("transcript field = %q after take #1's STALE transcribe result resolved — take #3 genuinely superseded it (unlike take #2, which never started), so it must still be dropped with a failed take in between", transcriptAfterStale)
	}

	// Take #3's own call resolves — this is the real proof recordingGeneration
	// tracked exactly two successful starts (take #1, take #3), not three:
	// if the failed take #2 had incorrectly consumed a recordingGeneration
	// slot, this equality would still likely hold by coincidence, but if it
	// had instead been SKIPPED without properly advancing at take #3's own
	// start, take #1's stale write above would have wrongly been accepted
	// instead of dropped — already ruled out by the assertion just above.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeResolvers[1](new Response("CURRENT-TAKE-THREE", { status: 200 })); void 0;`, nil)); err != nil {
		t.Fatalf("resolve take #3's (current) transcribe call: %v", err)
	}
	var transcriptAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var v = document.getElementById("uc-issue-transcript").value;
			return v === "CURRENT-TAKE-THREE" ? v : null;
		}`,
		&transcriptAfterCurrent,
	)); err != nil {
		t.Fatalf("wait for take #3's own transcript to apply: %v", err)
	}
	var descriptionAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.Value(`#uc-issue-description`, &descriptionAfterCurrent, chromedp.ByQuery)); err != nil {
		t.Fatalf("read description field after take #3's resolution: %v", err)
	}
	if descriptionAfterCurrent != "CURRENT-TAKE-THREE" {
		t.Fatalf("description field = %q after take #3's own transcribe result resolved, want exactly %q (a stale prefix here would mean take #1's earlier write wasn't actually dropped)", descriptionAfterCurrent, "CURRENT-TAKE-THREE")
	}
	var statusAfterCurrent string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-issue-record-status`, &statusAfterCurrent, chromedp.ByQuery)); err != nil {
		t.Fatalf("read status line after take #3's resolution: %v", err)
	}
	if statusAfterCurrent != "" {
		t.Fatalf("status line = %q after take #3's own transcribe result resolved, want empty (idle) — take #3's own success must clear it, same as an ordinary two-take sequence", statusAfterCurrent)
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

// TestIssueReportPage_ScreenRecord_PreviewThenSubmitCreatesAttachment is
// the real-browser proof for uc-infra#92: a real (headless-Chrome-driven)
// click through the screen-record button, its review preview, and a
// real form submission actually links a durable Attachment to the
// created IssueReport — not just that issuereport.go's Go logic accepts
// a hand-built multipart request (issuereport_test.go already proves
// that at the HTTP layer). A headless browser has no real screen to
// share, so this fakes getDisplayMedia/MediaRecorder the same way
// TestIssueReportPage_VoiceRecordShowsCleanErrorNotRawJSON already fakes
// getUserMedia/MediaRecorder for the mic — the fake still drives the
// real click handler, the real File/DataTransfer assignment onto the
// hidden file input, the real <video> preview, and the real native form
// POST; only the actual screen capture itself is stubbed out.
func TestIssueReportPage_ScreenRecord_PreviewThenSubmitCreatesAttachment(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeDisplayMediaScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getDisplayMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() {};
    this.stop = function() {
      self.state = "inactive";
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake screen recording bytes"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeDisplayMediaScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake display media script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // start "recording"
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // stop
		chromedp.WaitVisible(`#uc-issue-screenrecord-preview-wrap`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("record then stop a screen recording: %v", err)
	}

	// The review-before-upload proof: the captured recording is sitting
	// in a real <video> preview element with a real playable src, and the
	// hidden file input actually carries the File — both before Submit
	// is ever clicked, same "reviewed before anything is sent" principle
	// the transcript textarea already demonstrates for voice.
	var previewSrc string
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-screenrecord-preview").src`, &previewSrc,
	)); err != nil {
		t.Fatalf("read the preview video's src: %v", err)
	}
	if !strings.HasPrefix(previewSrc, "blob:") {
		t.Fatalf(`expected the preview <video> src to be a real blob: URL, got %q`, previewSrc)
	}
	var fileCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-screenrecord-file").files.length`, &fileCount,
	)); err != nil {
		t.Fatalf("read the hidden file input's file count: %v", err)
	}
	if fileCount != 1 {
		t.Fatalf("expected the recorded Blob assigned onto the hidden file input, got %d files", fileCount)
	}

	if err := chromedp.Run(ctx,
		chromedp.SetValue(`#uc-issue-title`, "Real browser screen recording", chromedp.ByQuery),
		chromedp.SetValue(`#uc-issue-description`, "See the attached screen recording.", chromedp.ByQuery),
		chromedp.Click(`#uc-issue-report-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-issue-report-result`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit the report: %v", err)
	}

	var resultText string
	if err := chromedp.Run(ctx, chromedp.Text(`.uc-issue-report-result`, &resultText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read the result panel: %v", err)
	}
	if strings.Contains(resultText, "could not be attached") {
		t.Fatalf("expected the recording to attach successfully, got the not-attached note: %q", resultText)
	}

	// Read the created Attachment back through the real API — the
	// vacuous version of this test would still pass if the recording
	// were silently dropped from Submit, since the result page looks the
	// same either way.
	var apiResult struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		fetch('/api/records/Attachment')
			.then(function(r){ return r.text().then(function(t){ return {status: r.status, body: t}; }); })
	`, &apiResult, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})); err != nil {
		t.Fatalf("fetch the stored Attachment records from the browser: %v", err)
	}
	if apiResult.Status != 200 {
		t.Fatalf("expected 200 listing Attachment records, got %d: %.300s", apiResult.Status, apiResult.Body)
	}
	if !strings.Contains(apiResult.Body, `"entity_type":"IssueReport"`) {
		t.Fatalf("expected a real Attachment linked to the submitted IssueReport, got:\n%.500s", apiResult.Body)
	}
}

// TestIssueReportPage_BothRecordButtons_DisableWhenMediaDevicesUnavailable
// is the regression test for the bug independent review caught in the
// first version of uc-infra#92: the mic feature-detect used a bare
// `return` out of the capture page's single top-level IIFE whenever
// navigator.mediaDevices was falsy — which also skipped every line of
// the screen-recording setup below it, entirely unrelated to
// microphone capture. The screen-record button (AttachmentsEnabled is
// true here) was left rendered, enabled, and with no click handler at
// all: a silently dead control, in exactly the degraded environment the
// no_screen_share message exists to explain. This test forces BOTH
// navigator.mediaDevices and window.MediaRecorder to be absent (as they
// genuinely are in a non-secure-context page load) and asserts both
// buttons end up disabled with their respective localized messages —
// the fixed version's if/else (not "if not-available, return") is what
// makes the second half of that assertion possible at all.
func TestIssueReportPage_BothRecordButtons_DisableWhenMediaDevicesUnavailable(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const removeMediaDevicesScript = `
(function() {
  try {
    Object.defineProperty(navigator, "mediaDevices", { value: undefined, configurable: true });
  } catch (e) {}
  window.MediaRecorder = undefined;
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(removeMediaDevicesScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject the remove-media-devices script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue report page: %v", err)
	}

	var micDisabled bool
	var micStatus string
	if err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-record-btn").disabled`, &micDisabled),
		chromedp.Text(`#uc-issue-record-status`, &micStatus, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read the mic button's disabled state and status: %v", err)
	}
	if !micDisabled {
		t.Error("expected the mic button to be disabled with no mediaDevices/MediaRecorder available")
	}
	if !strings.Contains(micStatus, "isn't available") {
		t.Errorf("expected the mic button's status to show the no-mic message, got %q", micStatus)
	}

	// The actual regression assertion: before the fix, this button was
	// left enabled (and inert — no click handler ever attached) because
	// the mic guard's early `return` skipped this setup code entirely.
	var screenDisabled bool
	var screenStatus string
	if err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-screenrecord-btn").disabled`, &screenDisabled),
		chromedp.Text(`#uc-issue-screenrecord-status`, &screenStatus, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read the screen-record button's disabled state and status: %v", err)
	}
	if !screenDisabled {
		t.Fatal("expected the screen-record button to be disabled with no mediaDevices/MediaRecorder available (regression: uc-infra#92 independent review)")
	}
	if !strings.Contains(screenStatus, "isn't available") {
		t.Errorf("expected the screen-record button's status to show the no-screen-share message, got %q", screenStatus)
	}
}

// TestIssueReportPage_ScreenRecord_OversizedRecordingNotAttachedFormIntact
// is the regression test for the second bug independent review caught:
// with no client-side size check, an oversized recording rode into the
// hidden file input unconditionally and only ever surfaced server-side
// as http.MaxBytesReader 400ing the WHOLE /issue-report/submit request
// — destroying the title, description and transcript along with it, the
// exact opposite of attachScreenRecording's own "never unwind the
// report" contract. This drives the real onstop handler with a fake
// MediaRecorder that emits one chunk larger than MaxScreenRecordingBytes
// and confirms: the oversized-recording message is shown, the preview
// stays hidden, the hidden file input carries no file, and — the actual
// proof this doesn't destroy anything — a normal, unrelated Submit right
// afterward still succeeds and creates the IssueReport with no
// screen_recording part riding along.
func TestIssueReportPage_ScreenRecord_OversizedRecordingNotAttachedFormIntact(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	// Fakes a recording whose single reported chunk is one byte larger
	// than the server's maxScreenRecordingBytes (60 MiB) — big enough to
	// trip the onstop size check, small enough to stay cheap to allocate
	// in a headless tab.
	const fakeOversizedRecordingScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getDisplayMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() {};
    this.stop = function() {
      self.state = "inactive";
      if (self.ondataavailable) {
        var oversized = new Uint8Array((60 * 1024 * 1024) + 1);
        self.ondataavailable({ data: new Blob([oversized]) });
      }
      if (self.onstop) { self.onstop(); }
    };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeOversizedRecordingScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake oversized-recording script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // start "recording"
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // stop -> trips the size check
	); err != nil {
		t.Fatalf("record then stop an oversized screen recording: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-screenrecord-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the oversized-recording status message: %v", err)
	}
	if !strings.Contains(status, "too large") {
		t.Fatalf("expected the too-large message, got %q", status)
	}

	var previewHasSrc bool
	var fileCount int
	if err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-screenrecord-preview").hasAttribute("src")`, &previewHasSrc),
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-screenrecord-file").files.length`, &fileCount),
	); err != nil {
		t.Fatalf("read the preview/file-input state: %v", err)
	}
	if previewHasSrc {
		t.Error("expected no preview src for a rejected oversized recording")
	}
	if fileCount != 0 {
		t.Errorf("expected the hidden file input to carry no file after an oversized recording, got %d", fileCount)
	}

	// The actual "form intact" proof: Submit still works normally.
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`#uc-issue-title`, "Oversized recording was rejected client-side", chromedp.ByQuery),
		chromedp.SetValue(`#uc-issue-description`, "Should still submit fine without the video.", chromedp.ByQuery),
		chromedp.Click(`#uc-issue-report-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-issue-report-result`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit after an oversized recording was rejected: %v", err)
	}

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
	if apiResult.Status != 200 || !strings.Contains(apiResult.Body, "Oversized recording was rejected client-side") {
		t.Fatalf("expected the report to have been saved despite the oversized recording, got %d: %.500s", apiResult.Status, apiResult.Body)
	}
}

// TestIssueReportPage_ScreenRecord_DoubleClickDoesNotStartTwoStreams is the
// real-browser regression test for uc-infra#197: the screen-record button's
// own disable-on-click guard (issuereport.go's click handler — both the
// synchronous `screenBtn.disabled = true;` set before the async
// getDisplayMedia call, and the explicit `if (screenBtn.disabled) return;`
// early return guarding the handler itself) shipped as part of uc-infra#92's
// independent review — the same protection
// TestIssueReportPage_MicRecord_DoubleClickDoesNotStartTwoStreams already
// pins for the pre-existing mic button — but never got a regression test of
// its own. This closes that gap the same way, driven against getDisplayMedia
// instead of getUserMedia.
//
// A real (or CDP-simulated) click cannot reach an already-disabled button at
// all, so the two clicks below only ever prove the synchronous disable, not
// the explicit early-return line — the same structural gap the mic test
// this one is modeled on has. This test additionally dispatches a synthetic
// click event directly (bypassing the browser's native disabled-control
// dispatch suppression, per independent review of this card) to reach the
// handler while the button is disabled and confirm the early return itself
// is what stops it, not merely an artifact of the browser never invoking the
// listener a second time.
//
// Uses testServerWithBlobstore, not the plain testServer every other
// double-click/permission test in this file uses, because the
// screen-record button only renders at all when AttachmentsEnabled is true
// (issueReportNewPage's own gate) — a blobstore-less server would never
// show the button this test needs to click.
func TestIssueReportPage_ScreenRecord_DoubleClickDoesNotStartTwoStreams(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeDeferredDisplayMediaScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getDisplayMediaCallCount = 0;
  window.__resolveGetDisplayMedia = null;
  navigator.mediaDevices.getDisplayMedia = function() {
    window.__getDisplayMediaCallCount++;
    return new Promise(function(resolve) {
      window.__resolveGetDisplayMedia = resolve;
    });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() {};
    this.stop = function() {
      self.state = "inactive";
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake screen recording bytes"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeDeferredDisplayMediaScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred display media script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // first click: starts the (pending) getDisplayMedia call
	); err != nil {
		t.Fatalf("click the screen-record button once: %v", err)
	}

	// The guard's own proof, before the pending promise ever resolves: the
	// button must already be disabled, which is also what makes the
	// browser itself refuse to dispatch a second click below.
	var disabledWhilePending bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-screenrecord-btn").disabled`, &disabledWhilePending,
	)); err != nil {
		t.Fatalf("read the screen-record button's disabled state while getDisplayMedia is pending: %v", err)
	}
	if !disabledWhilePending {
		t.Fatal("expected the screen-record button to be disabled synchronously on click, before getDisplayMedia resolves (regression: uc-infra#197)")
	}

	// A real browser does not dispatch "click" on an already-disabled
	// button — this is the actual double-click scenario the bug report
	// describes, driven the same way a real impatient user would.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the screen-record button a second time while the first getDisplayMedia call is pending: %v", err)
	}

	var callCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__getDisplayMediaCallCount`, &callCount)); err != nil {
		t.Fatalf("read getDisplayMedia's call count: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected getDisplayMedia to have been called exactly once despite two clicks, got %d calls (regression: uc-infra#197 — an earlier stream would be left live indefinitely, the same class of bug independent review already fixed once for the mic button)", callCount)
	}

	// The native-dispatch clicks above prove the button is disabled, but a
	// disabled control's own semantics — not necessarily the handler's own
	// `if (screenBtn.disabled) return;` guard — are what block them: the
	// browser never even invokes the listener a second time, so those
	// clicks can't tell the guard apart from "the browser didn't try."
	// Dispatching the click event directly bypasses that native suppression
	// and reaches the listener regardless of the disabled attribute
	// (independent review of this card confirmed this reaches the handler
	// where a real/CDP click does not) — this is what actually exercises the
	// explicit early-return line the doc comment above claims to test.
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.getElementById("uc-issue-screenrecord-btn")
			.dispatchEvent(new MouseEvent("click", { bubbles: true }))`, nil,
	)); err != nil {
		t.Fatalf("dispatch a synthetic click event at the disabled screen-record button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__getDisplayMediaCallCount`, &callCount)); err != nil {
		t.Fatalf("read getDisplayMedia's call count after the synthetic dispatch: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected the disabled button's early-return guard to block a synthetically-dispatched click too, got %d total getDisplayMedia calls (regression: uc-infra#197's guard line itself, not just the browser's native disabled-control dispatch suppression)", callCount)
	}

	// Resolve the pending permission prompt and confirm recording still
	// starts normally afterward — the guard must not leave the button
	// stuck disabled or the handler otherwise broken.
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`window.__resolveGetDisplayMedia({ getTracks: function() { return []; } }); void 0;`, nil,
	)); err != nil {
		t.Fatalf("resolve the pending getDisplayMedia promise: %v", err)
	}

	// Polls for the exact post-resolve text, not merely "any non-empty
	// text" (same reasoning as the mic version of this test): the button
	// already reads a non-empty string ("Record screen") before the
	// resolve, so a looser predicate would prove nothing about actually
	// waiting for the promise.
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-screenrecord-btn").textContent;
			return t === "Stop recording" ? t : null;
		}`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the screen-record button to reflect the started recording: %v", err)
	}
	var enabledAfterResolve bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-screenrecord-btn").disabled`, &enabledAfterResolve,
	)); err != nil {
		t.Fatalf("read the screen-record button's disabled state after getDisplayMedia resolves: %v", err)
	}
	if !enabledAfterResolve {
		t.Fatal("expected the screen-record button to be re-enabled once getDisplayMedia resolves")
	}
}

// TestIssueReportPage_ScreenRecord_RedundantStopClickDoesNotThrow is the
// regression test for uc-infra#220, found while verifying uc-infra#196's own
// "same structure, same bug" claim against the actual code. Pre-fix, the
// screen-record button's Stop branch was a bare `if (screenRecording) {
// screenRecorder.stop(); return; }`: it never reset `screenRecording`
// synchronously — only `onstop` does, and that fires asynchronously — so a
// second click on Stop before the first click's `onstop` had run read
// `screenRecording` as still true and called `.stop()` again. Per the
// MediaStream Recording spec, `MediaRecorder.state` transitions to
// "inactive" *synchronously* as part of the first `.stop()` call, well
// before `onstop` fires, and `.stop()` is spec'd to throw `InvalidStateError`
// when called while already "inactive" — an uncaught exception inside the
// click handler. Fixed by guarding the call with `screenRecorder.state !==
// "inactive"`, the same guard the auto-stop timeout below it already uses.
//
// Every other *screen-record* fake MediaRecorder in this file (including the
// one just above, in
// TestIssueReportPage_ScreenRecord_DoubleClickDoesNotStartTwoStreams) fires
// `onstop` synchronously, inline inside `.stop()` itself — which is exactly
// why none of them can reach this bug: there is no window between the state
// transition and `onstop` for a redundant click to land in. (Two *mic* fakes
// elsewhere in this file — StopThenRecordAgainDoesNotMixTakes and its
// LateDataDoesNotLeakAcrossTakes sibling, below — also defer `onstop`, but
// can't reach this bug either, for an unrelated reason: the mic Stop branch
// resets `recording` synchronously, so a second click there is read as a new
// Record, not a second Stop.) This fake instead defers `onstop` to an
// explicit `window.__fireOnstop()` call the test controls — the same
// deterministic-timing technique those two mic tests use (not a real
// timer/scheduler race — the flakiness class uc-infra#203 already tracks for
// this file) — and has `.stop()` throw `InvalidStateError` when called on an
// already-"inactive" recorder, matching the real spec closely enough to
// actually pin this bug (though its initial `state` of "recording" in the
// constructor, matching every other fake in this file, is not itself
// spec-accurate — a real MediaRecorder starts "inactive" until `start()` —
// harmless here since the guard is only ever reached after `start()`).
func TestIssueReportPage_ScreenRecord_RedundantStopClickDoesNotThrow(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getDisplayMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__uncaughtErrors = [];
  window.addEventListener("error", function(e) {
    window.__uncaughtErrors.push(String((e && e.message) || e));
  });
  window.__recorder = null;
  window.__fireOnstop = function() {
    if (window.__recorder && window.__recorder.onstop) { window.__recorder.onstop(); }
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() { self.state = "recording"; };
    // Spec-accurate: state flips to "inactive" synchronously here, and a
    // second call while already "inactive" throws — onstop is deferred to
    // window.__fireOnstop(), not fired inline, so the test controls exactly
    // when it runs relative to a redundant click.
    this.stop = function() {
      if (self.state === "inactive") {
        throw new DOMException("The MediaRecorder is inactive.", "InvalidStateError");
      }
      self.state = "inactive";
    };
    this.ondataavailable = null;
    window.__recorder = self;
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake spec-accurate MediaRecorder script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the issue-report page: %v", err)
	}

	// Canary, before the real scenario: prove window.addEventListener("error",
	// ...) actually captures an exception thrown inside a DOM click listener
	// in this browser/page, before trusting its absence as proof of anything
	// below. Without this, the whole test would pass just as well if a future
	// change silently broke the hook (e.g. wrapping the handler body in its
	// own try/catch, or something clobbering the listener) — the same
	// vacuous-pass risk TestIssueReportPage_ScreenRecord_DoubleClickDoesNotStartTwoStreams's
	// synthetic-dispatch step above exists to avoid for a different guard.
	var canaryCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var b = document.createElement("button");
			b.id = "uc-test-canary-btn";
			b.addEventListener("click", function() { throw new Error("canary"); });
			document.body.appendChild(b);
		})();
	`, nil)); err != nil {
		t.Fatalf("inject canary button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-test-canary-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click canary button: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__uncaughtErrors.length`, &canaryCount)); err != nil {
		t.Fatalf("read canary error count: %v", err)
	}
	if canaryCount != 1 {
		t.Fatalf("canary check failed: expected the error listener to capture exactly 1 uncaught exception from a throwing click handler, got %d — the rest of this test's \"no uncaught errors\" assertion would be meaningless if this hook isn't actually working", canaryCount)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		document.getElementById("uc-test-canary-btn").remove();
		window.__uncaughtErrors = [];
		void 0;
	`, nil)); err != nil {
		t.Fatalf("remove canary button and reset captured errors: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery)); err != nil { // start
		t.Fatalf("start screen recording: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-screenrecord-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for recording to start: %v", err)
	}

	// Two Stop clicks back to back, before window.__fireOnstop() has been
	// called for the first one — the exact redundant-click window uc-infra#220
	// describes. Pre-fix, the second click's screenRecorder.stop() throws
	// inside the handler, uncaught.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // stop
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // redundant stop, before onstop fires
	); err != nil {
		t.Fatalf("click stop twice in a row: %v", err)
	}

	var uncaught []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__uncaughtErrors`, &uncaught)); err != nil {
		t.Fatalf("read captured uncaught errors: %v", err)
	}
	if len(uncaught) != 0 {
		t.Fatalf("expected no uncaught exceptions from a redundant Stop click, got %v (regression: uc-infra#220 — a second click on Stop before the deferred onstop callback fires must not call .stop() again on an already-inactive recorder)", uncaught)
	}

	// The recording still completes normally once onstop actually fires —
	// the fix's guard must skip the redundant .stop() call, not silently
	// break the real one. Fires a real data chunk first (every other
	// screen-record fake in this file does) so this proves the actual
	// attachment path completed, not merely that the button's label changed.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		window.__recorder.ondataavailable({ data: new Blob(["fake screen recording bytes"]) });
		window.__fireOnstop();
		void 0;
	`, nil)); err != nil {
		t.Fatalf("fire the deferred data event and onstop callback: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-screenrecord-btn").textContent; return t === "Record screen" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after the recording completes: %v", err)
	}

	var previewHasSrc bool
	var fileCount int
	if err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-screenrecord-preview").hasAttribute("src")`, &previewHasSrc),
		chromedp.EvaluateAsDevTools(`document.getElementById("uc-issue-screenrecord-file").files.length`, &fileCount),
	); err != nil {
		t.Fatalf("read the preview/file-input state: %v", err)
	}
	if !previewHasSrc {
		t.Error("expected a preview src once the recording actually completes — the guard must not have interfered with the real stop's own onstop handling")
	}
	if fileCount != 1 {
		t.Errorf("expected the hidden file input to carry exactly 1 file once the recording actually completes, got %d", fileCount)
	}
}

// TestIssueReportPage_ScreenRecord_RecorderConstructorThrowStopsStream is
// the screen-record counterpart of
// TestIssueReportPage_MicRecord_RecorderConstructorThrowStopsStream
// (independent review, uc-infra#198): the screen-record handler's
// try/catch only ever wrapped the primary `new MediaRecorder(stream,
// {videoBitsPerSecond: ...})` call — a throw there was handled, by
// falling back to `new MediaRecorder(stream)` — but a throw from that
// fallback itself (a throw inside a catch block isn't caught by its own
// try) or from `screenRecorder.start()` escaped into the outer
// `.then(...).catch(...)` below, which has no access to `stream` (a
// separate function's own parameter) and so could never stop its
// tracks, leaving the display-share stream (and the browser's "you are
// sharing your screen" indicator) live indefinitely. This test drives
// the fallback-throw path (the fake makes both the primary and fallback
// constructor calls throw); see
// TestIssueReportPage_ScreenRecord_RecorderStartThrowStopsStream below
// for the other escape route. Mirrors the mic handler's own fix.
func TestIssueReportPage_ScreenRecord_RecorderConstructorThrowStopsStream(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeThrowingScreenRecorderScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__trackStopCount = 0;
  navigator.mediaDevices.getDisplayMedia = function() {
    return Promise.resolve({
      getTracks: function() {
        return [
          { stop: function() { window.__trackStopCount++; } },
          { stop: function() { window.__trackStopCount++; } }
        ];
      }
    });
  };
  window.MediaRecorder = function() {
    throw new Error("unsupported MediaRecorder configuration");
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeThrowingScreenRecorderScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake throwing-MediaRecorder script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click the screen-record button: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-screenrecord-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the constructor-failure status message: %v", err)
	}
	if !strings.Contains(status, "unsupported MediaRecorder configuration") {
		t.Fatalf("expected the constructor's error to reach the status line, got %q", status)
	}

	// The actual proof this doesn't leak: both tracks of the
	// already-granted display-share stream were stopped, not left live.
	var trackStopCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__trackStopCount`, &trackStopCount)); err != nil {
		t.Fatalf("read the track-stop count: %v", err)
	}
	if trackStopCount != 2 {
		t.Fatalf("expected both display-share stream tracks to be stopped after the MediaRecorder constructor threw, got %d stopped (regression: independent review of uc-infra#198 — the stream, and the browser's screen-share indicator, would otherwise be left live indefinitely)", trackStopCount)
	}

	var enabledAfterThrow bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-screenrecord-btn").disabled`, &enabledAfterThrow,
	)); err != nil {
		t.Fatalf("read the screen-record button's disabled state after the constructor threw: %v", err)
	}
	if !enabledAfterThrow {
		t.Fatal("expected the screen-record button to be re-enabled after the constructor threw, so the person can retry")
	}
}

// TestIssueReportPage_ScreenRecord_RecorderStartThrowStopsStream is the
// other half of this fix (independent review of uc-infra#198's own
// diff): the constructor-throw test above only ever exercises the
// fallback-constructor escape route, not screenRecorder.start() itself —
// the second route the bug report named, and the one the widened
// try/catch has to cover for the fix to actually be complete. Without
// this test, an edit that narrowed the try back to close right after the
// constructor fallback (leaving start() outside it again) would leave
// every other test in this file green, including the constructor-throw
// test above, since its fake never reaches start() at all. Here the
// fake constructs successfully but throws from start() instead.
func TestIssueReportPage_ScreenRecord_RecorderStartThrowStopsStream(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeThrowingStartScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__trackStopCount = 0;
  navigator.mediaDevices.getDisplayMedia = function() {
    return Promise.resolve({
      getTracks: function() {
        return [
          { stop: function() { window.__trackStopCount++; } },
          { stop: function() { window.__trackStopCount++; } }
        ];
      }
    });
  };
  window.MediaRecorder = function() {
    this.start = function() { throw new Error("start failed: device disconnected"); };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeThrowingStartScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake throwing-start script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("click the screen-record button: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-screenrecord-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the start-failure status message: %v", err)
	}
	if !strings.Contains(status, "start failed: device disconnected") {
		t.Fatalf("expected start()'s error to reach the status line, got %q", status)
	}

	// The actual proof this doesn't leak: both tracks of the
	// already-granted display-share stream were stopped, not left live —
	// this is the assertion that would NOT have failed if the try/catch
	// only covered the constructor and not start() itself.
	var trackStopCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__trackStopCount`, &trackStopCount)); err != nil {
		t.Fatalf("read the track-stop count: %v", err)
	}
	if trackStopCount != 2 {
		t.Fatalf("expected both display-share stream tracks to be stopped after start() threw, got %d stopped (regression: independent review of uc-infra#198 — the stream, and the browser's screen-share indicator, would otherwise be left live indefinitely)", trackStopCount)
	}

	var enabledAfterThrow bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-screenrecord-btn").disabled`, &enabledAfterThrow,
	)); err != nil {
		t.Fatalf("read the screen-record button's disabled state after start() threw: %v", err)
	}
	if !enabledAfterThrow {
		t.Fatal("expected the screen-record button to be re-enabled after start() threw, so the person can retry")
	}
}

// TestIssueReportPage_ScreenRecord_PermissionDeniedReEnablesButtonForRetry
// pins the screen-record handler's existing `screenBtn.disabled = false;`
// re-enable-after-rejection line (independent review, uc-infra#198): no
// existing test exercised a getDisplayMedia rejection, so nothing would
// have caught a future edit silently dropping that line — leaving a
// denied screen-share stuck disabled until page reload, with no way to
// retry short of that. Modeled on the mic button's own
// TestIssueReportPage_MicRecord_PermissionDeniedReEnablesButtonForRetry.
func TestIssueReportPage_ScreenRecord_PermissionDeniedReEnablesButtonForRetry(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServerWithBlobstore(t)
	ctx := browserCtx(t, tenantID)

	const fakeDenyThenAllowScreenShareScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  window.__getDisplayMediaCallCount = 0;
  navigator.mediaDevices.getDisplayMedia = function() {
    window.__getDisplayMediaCallCount++;
    if (window.__getDisplayMediaCallCount === 1) {
      return Promise.reject(new Error("Permission denied"));
    }
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.MediaRecorder = function() {
    var self = this;
    this.state = "recording";
    this.start = function() {};
    this.stop = function() {
      self.state = "inactive";
      if (self.ondataavailable) { self.ondataavailable({ data: new Blob(["fake"]) }); }
      if (self.onstop) { self.onstop(); }
    };
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeDenyThenAllowScreenShareScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deny-then-allow display media script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-screenrecord-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery), // denied
	); err != nil {
		t.Fatalf("click the screen-record button: %v", err)
	}

	var status string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() {
			var t = document.getElementById("uc-issue-screenrecord-status").textContent;
			return t && t.length > 0 ? t : null;
		}`,
		&status,
	)); err != nil {
		t.Fatalf("wait for the permission-denied status message: %v", err)
	}
	if !strings.Contains(status, "Permission denied") {
		t.Fatalf("expected the permission-denial error to reach the status line, got %q", status)
	}

	var enabledAfterDenial bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`!document.getElementById("uc-issue-screenrecord-btn").disabled`, &enabledAfterDenial,
	)); err != nil {
		t.Fatalf("read the screen-record button's disabled state after denial: %v", err)
	}
	if !enabledAfterDenial {
		t.Fatal("expected the screen-record button to be re-enabled after a getDisplayMedia rejection, so the person can retry (regression: independent review of uc-infra#198)")
	}

	// The actual retry proof: a second click (the fake now allows it)
	// must be able to reach getDisplayMedia again, not be permanently
	// inert from the first denial.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-screenrecord-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the screen-record button a second time to retry: %v", err)
	}
	var callCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(`window.__getDisplayMediaCallCount`, &callCount)); err != nil {
		t.Fatalf("read getDisplayMedia's call count: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected the retry click to reach getDisplayMedia a second time, got %d total calls", callCount)
	}
}

// TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes is the
// real-browser regression test for uc-infra#196: `chunks` (issuereport.go's
// mic click handler) used to be a single outer-IIFE-scope variable, reset
// (`chunks = [];`) at the start of every Record click and read by whichever
// take's `onstop` closure happened to fire, whenever it happened to fire —
// not necessarily the take that pushed the data. A real MediaRecorder's
// `onstop`/`ondataavailable` fire asynchronously, so a fast Stop (take #1)
// followed immediately by Record again (take #2), before take #1's `onstop`
// has actually run, let take #2's `chunks = [];` wipe the array out from
// under take #1's still-pending upload — corrupting or emptying take #1's
// transcription request.
//
// Every existing fake `MediaRecorder` in this file fires `ondataavailable`/
// `onstop` synchronously, inline inside `.stop()` — which is exactly why
// none of them can reach this race (there is no window for a second
// recording to start before the first one's onstop has already completed).
// This fake instead defers both callbacks: `.stop()` only registers the
// recorder for later firing (`window.__recorders`), and the test explicitly
// controls the moment (and the content) of each take's data event via
// `window.__fireStop(index, chunkText)` — deterministic, no reliance on
// real timer/scheduler races (the flakiness class uc-infra#203 already
// tracks for this same test file).
//
// window.fetch is faked for the /issue-report/transcribe call specifically
// (real fetch untouched otherwise, same technique
// TestIssueReportPage_TranscriptAndDescriptionAreClampedOnTranscribe already
// uses) so the actual bytes POSTed as the "audio" part can be read back and
// compared against each take's own distinct marker text — the only way to
// prove the two takes' data never mixed, as opposed to merely proving two
// fetch calls happened.
//
// No screen-record equivalent of this test exists, despite uc-infra#196
// naming both handlers as "same structure, same bug": verified against the
// actual code (not assumed) that the screen-record handler's Stop branch
// (`if (screenRecording) { screenRecorder.stop(); return; }`) never
// synchronously resets `screenRecording`/the button label the way the mic
// handler's Stop branch does — that reset only happens inside `onstop`,
// which is exactly the callback this whole race depends on being deferred.
// So a screen-record "Stop then Record again" click sequence can't reach a
// second recording at all: `screenRecording` is still true, so the second
// click is interpreted as another Stop (calling `.stop()` on an
// already-inactive recorder — a distinct, real bug filed separately as
// uc-infra#220, not this one). `screenChunks`/the screen timers are still
// moved to per-instance scope below for the same defense-in-depth/
// consistency reasoning the mic fix uses, but there is no reachable,
// honest regression scenario to pin for the screen-record side of this
// specific race — writing one anyway would just be asserting a fake
// timing sequence a real user can never actually trigger.
func TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorders = [];
  window.MediaRecorder = function() {
    var self = this;
    this.start = function() {};
    this.stop = function() { window.__recorders.push(self); };
  };
  // ondataavailable and onstop are separately fireable (independent
  // review of uc-infra#196's first draft): a real MediaRecorder queues
  // them as two distinct async tasks, not one — data can arrive well
  // after a later take has already started (and, pre-fix, already reset
  // the shared chunks array) while onstop is still pending, which is a
  // materially different interleaving than firing both back to back.
  // __fireStop is kept as a convenience for tests that don't care about
  // that distinction.
  window.__fireData = function(index, chunkText) {
    var self = window.__recorders[index];
    if (self.ondataavailable) { self.ondataavailable({ data: new Blob([chunkText]) }); }
  };
  window.__fireOnstop = function(index) {
    var self = window.__recorders[index];
    if (self.onstop) { self.onstop(); }
  };
  window.__fireStop = function(index, chunkText) {
    window.__fireData(index, chunkText);
    window.__fireOnstop(index);
  };
  var realFetch = window.fetch;
  window.__transcribeBodies = [];
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return opts.body.get("audio").text().then(function(text) {
        window.__transcribeBodies.push(text);
        return new Response("ok", { status: 200 });
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred-stop media/fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
	); err != nil {
		t.Fatalf("start take #1: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #1 to start: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #1: stop (onstop deferred, not yet fired)
		t.Fatalf("stop take #1: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #1: %v", err)
	}

	// Take #2 starts here, its own Record click going through the exact
	// same handler that reset the (formerly shared) `chunks` array — while
	// take #1's `onstop` is still sitting unfired in window.__recorders.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("start take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #2 to start: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: stop (also deferred)
		t.Fatalf("stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #2: %v", err)
	}

	// Fire take #1's deferred onstop first, with its own distinct marker.
	// In THIS interleaving (both of take #1's events fired together, after
	// take #2 has already reset the — pre-fix, shared — chunks array),
	// take #1's own ondataavailable actually pushes its data into
	// whatever array is current at the moment onstop fires it, so take #1
	// itself uploads correctly even pre-fix; it's take #2 (below) whose
	// upload picks up take #1's leftover push next, since nothing ever
	// cleared the shared array between the two onstop firings. Verified
	// by simulating the pre-fix semantics against this exact fake
	// (independent review of this test's first draft, which had this
	// backwards): confirmed bodies[0] is correct and only bodies[1] fails
	// pre-fix — see that assertion's own failure message below, not this
	// one, for the actual regression signal.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireStop(0, "take-one-audio-content"); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #1's deferred onstop: %v", err)
	}
	var transcribeCallCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeBodies.length >= 1 ? window.__transcribeBodies.length : null; }`,
		&transcribeCallCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe upload: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireStop(1, "take-two-longer-audio-content"); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #2's deferred onstop: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeBodies.length >= 2 ? window.__transcribeBodies.length : null; }`,
		&transcribeCallCount,
	)); err != nil {
		t.Fatalf("wait for take #2's transcribe upload: %v", err)
	}

	var bodies []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeBodies`, &bodies)); err != nil {
		t.Fatalf("read the captured transcribe request bodies: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected exactly 2 transcribe uploads, got %d: %v", len(bodies), bodies)
	}
	if bodies[0] != "take-one-audio-content" {
		t.Fatalf("take #1's uploaded audio content = %q, want %q", bodies[0], "take-one-audio-content")
	}
	if bodies[1] != "take-two-longer-audio-content" {
		t.Fatalf("take #2's uploaded audio content = %q, want %q — pre-fix, take #1's leftover push into the shared chunks array (never cleared between the two onstop firings above) rode along into take #2's own upload (regression: uc-infra#196)", bodies[1], "take-two-longer-audio-content")
	}
}

// TestIssueReportPage_MicRecord_StopThenRecordAgain_LateDataDoesNotLeakAcrossTakes
// is the interleaving TestIssueReportPage_MicRecord_StopThenRecordAgainDoesNotMixTakes
// above doesn't reach (independent review of this fix's first draft):
// that test fires each take's ondataavailable and onstop back to back, so
// it only ever proves the "both callbacks fire once the next take has
// already reset shared state" ordering. A real MediaRecorder queues
// ondataavailable and onstop as two SEPARATE async tasks, so a take's data
// can arrive well after a second take has already started — with that
// take's own onstop not firing until later still. This is the literal
// "wipe" sequence uc-infra#196's own narrative describes (take #2's
// Record click resets the array before take #1's data has even landed,
// not just before take #1's onstop has run) — pre-fix, take #1's data
// would land in whatever the shared array currently is (already reset,
// possibly already holding take #2's own data by the time take #1's
// onstop actually builds the blob), corrupting or emptying take #1's
// upload; take #2's own later data/onstop would then also be corrupted by
// take #1's still-shared array. Fixed, each take's ondataavailable closes
// over its own local array regardless of firing order, so this ordering
// must be just as clean as the simpler one above.
func TestIssueReportPage_MicRecord_StopThenRecordAgain_LateDataDoesNotLeakAcrossTakes(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	const fakeScript = `
(function() {
  navigator.mediaDevices = navigator.mediaDevices || {};
  navigator.mediaDevices.getUserMedia = function() {
    return Promise.resolve({ getTracks: function() { return []; } });
  };
  window.__recorders = [];
  window.MediaRecorder = function() {
    var self = this;
    this.start = function() {};
    this.stop = function() { window.__recorders.push(self); };
  };
  window.__fireData = function(index, chunkText) {
    var self = window.__recorders[index];
    if (self.ondataavailable) { self.ondataavailable({ data: new Blob([chunkText]) }); }
  };
  window.__fireOnstop = function(index) {
    var self = window.__recorders[index];
    if (self.onstop) { self.onstop(); }
  };
  var realFetch = window.fetch;
  window.__transcribeBodies = [];
  window.fetch = function(url, opts) {
    if (String(url).indexOf("/issue-report/transcribe") !== -1) {
      return opts.body.get("audio").text().then(function(text) {
        window.__transcribeBodies.push(text);
        return new Response("ok", { status: 200 });
      });
    }
    return realFetch.apply(window, arguments);
  };
})();
`
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fakeScript).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("inject fake deferred-stop media/fetch script: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/issue-report/new"),
		chromedp.WaitVisible(`#uc-issue-record-btn`, chromedp.ByQuery),
		chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery), // take #1: start
	); err != nil {
		t.Fatalf("start take #1: %v", err)
	}
	var recordBtnText string
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #1 to start: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #1: stop — no data, no onstop fired yet
		t.Fatalf("stop take #1: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #1: %v", err)
	}

	// Take #2 starts — and, pre-fix, resets the shared chunks array —
	// BEFORE take #1's data has even arrived, let alone its onstop.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil {
		t.Fatalf("start take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Stop recording" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for take #2 to start: %v", err)
	}

	// NOW take #1's data arrives — after take #2's reset, well before take
	// #1's own onstop. Fixed, this lands in take #1's own local array
	// regardless; pre-fix, it lands in whatever's currently shared.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireData(0, "take-one-late-audio-content"); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #1's late data event: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Click(`#uc-issue-record-btn`, chromedp.ByQuery)); err != nil { // take #2: stop
		t.Fatalf("stop take #2: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { var t = document.getElementById("uc-issue-record-btn").textContent; return t === "Record voice note" ? t : null; }`,
		&recordBtnText,
	)); err != nil {
		t.Fatalf("wait for the button to reset after stopping take #2: %v", err)
	}

	// Fire take #1's onstop now (its data already arrived above), then
	// take #2's data+onstop together.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireOnstop(0); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #1's deferred onstop: %v", err)
	}
	var transcribeCallCount int
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeBodies.length >= 1 ? window.__transcribeBodies.length : null; }`,
		&transcribeCallCount,
	)); err != nil {
		t.Fatalf("wait for take #1's transcribe upload: %v", err)
	}

	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__fireData(1, "take-two-audio-content"); window.__fireOnstop(1); void 0;`, nil)); err != nil {
		t.Fatalf("fire take #2's data and onstop: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.PollFunction(
		`function() { return window.__transcribeBodies.length >= 2 ? window.__transcribeBodies.length : null; }`,
		&transcribeCallCount,
	)); err != nil {
		t.Fatalf("wait for take #2's transcribe upload: %v", err)
	}

	var bodies []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__transcribeBodies`, &bodies)); err != nil {
		t.Fatalf("read the captured transcribe request bodies: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected exactly 2 transcribe uploads, got %d: %v", len(bodies), bodies)
	}
	if bodies[0] != "take-one-late-audio-content" {
		t.Fatalf("take #1's uploaded audio content = %q, want %q — take #1's own data, arriving after take #2 had already started, was not isolated from take #2's reset (regression: uc-infra#196, late-data interleaving)", bodies[0], "take-one-late-audio-content")
	}
	if bodies[1] != "take-two-audio-content" {
		t.Fatalf("take #2's uploaded audio content = %q, want %q — take #1's late-arriving data leaked into take #2's upload (regression: uc-infra#196, late-data interleaving)", bodies[1], "take-two-audio-content")
	}
}
