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
