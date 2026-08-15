package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/speechassist"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// testHandlerWithSpeech is testHandler plus a speechassist.Client —
// kept separate rather than adding a speech parameter to testHandler
// itself so every other test in this package stays exactly as it was
// (voice transcription disabled, matching a deployment with no
// WHISPER_URL configured), same pattern import_test.go's
// testHandlerWithAI already establishes for aiassist.
func testHandlerWithSpeech(t *testing.T, router *tenantdb.Router, speech *speechassist.Client) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(router, catalog, nil, nil, nil, speech, nil)
}

func publishFoundation(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
}

// newAudioUploadRequest builds a multipart request with the "audio"
// field name issueReportTranscribe expects — deliberately not reusing
// newMultipartRequest (import_test.go), which hardcodes "file" as its
// field name for the CSV/XLSX wizard's own contract.
func newAudioUploadRequest(t *testing.T, target, tenantID, actorID string, audio []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("audio", "note.webm")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-Tenant-ID", tenantID)
	r.Header.Set("X-Actor-ID", actorID)
	return r
}

// TestIssueReportFieldMaxLength_ReadsFromDefinition (uc-infra#174,
// independent review) pins issueReportFieldMaxLength's own contract: it
// must read the exact bound foundation.IssueReport() declares, not a
// second hand-kept number — the entire point of replacing the two
// duplicate Go constants an earlier draft used.
func TestIssueReportFieldMaxLength_ReadsFromDefinition(t *testing.T) {
	def := foundation.IssueReport()
	for _, name := range []string{"title", "description", "transcript", "console_log"} {
		f, ok := def.FieldByName(name)
		if !ok || f.MaxLength == nil {
			t.Fatalf("expected foundation.IssueReport() field %q to declare a MaxLength", name)
		}
		if got, want := issueReportFieldMaxLength(name), *f.MaxLength; got != want {
			t.Fatalf("issueReportFieldMaxLength(%q) = %d, want %d (the Definition's own declared bound)", name, got, want)
		}
	}
}

// TestIssueReportFieldMaxLength_PanicsOnFieldWithNoDeclaredBound confirms
// the fail-loud contract: a field name that exists but declares no
// MaxLength (page_url, status — anything not in the four checked above)
// is a programmer error at a call site, not a runtime condition to
// silently render as maxlength="0" and reject every real submission.
func TestIssueReportFieldMaxLength_PanicsOnFieldWithNoDeclaredBound(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a field with no declared MaxLength")
		}
	}()
	issueReportFieldMaxLength("page_url")
}

// TestIssueReportFieldMaxLength_PanicsOnUnknownField is the same
// fail-loud contract for a field name that doesn't exist on the
// Definition at all (a typo at a call site).
func TestIssueReportFieldMaxLength_PanicsOnUnknownField(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an unknown field name")
		}
	}()
	issueReportFieldMaxLength("no_such_field")
}

func TestIssueReport_NewPage_RendersCaptureForm(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/issue-report/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="uc-issue-record-btn"`) {
		t.Fatalf("expected the record-voice-note button, got:\n%s", body)
	}
	if !strings.Contains(body, `name="title"`) || !strings.Contains(body, `name="description"`) {
		t.Fatalf("expected title and description fields, got:\n%s", body)
	}
	if !strings.Contains(body, `action="/issue-report/submit"`) {
		t.Fatalf("expected the form to post to /issue-report/submit, got:\n%s", body)
	}
	if !strings.Contains(body, `id="uc-issue-console-log"`) || !strings.Contains(body, `name="console_log"`) {
		t.Fatalf("expected the console-log capture field (universaltill/uc-infra#46), got:\n%s", body)
	}
}

func TestIssueReport_NewPage_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/issue-report/new", nil) // no auth headers
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIssueReport_Transcribe_RequiresAuth confirms an anonymous caller
// can't reach this endpoint at all — the one that actually costs real
// self-hosted Whisper compute per call (see issueReportTranscribe's own
// doc comment on the accepted-but-real per-tenant rate-limit gap; auth
// being required at all is the one thing standing between that gap and
// letting a fully anonymous caller burn compute for free).
func TestIssueReport_Transcribe_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("POST", "/issue-report/transcribe", nil) // no auth headers
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIssueReport_Transcribe_DisabledReturns503 confirms a deployment
// with no WHISPER_URL configured fails loud and clear on this specific
// endpoint (unlike the import wizard's AI mapping assist, transcription
// has no non-AI fallback to silently degrade to — see issuereport.go's
// own doc comment on issueReportTranscribe).
func TestIssueReport_Transcribe_DisabledReturns503(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux) // no speech client

	req := newAudioUploadRequest(t, "/issue-report/transcribe", tenantID, "farshid", []byte("fake audio"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no speech client is configured, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIssueReport_Transcribe_ReturnsTranscript confirms the endpoint
// actually calls through to the configured speechassist.Client and
// returns its transcript verbatim as the response body.
func TestIssueReport_Transcribe_ReturnsTranscript(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	var gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		_, header, err := r.FormFile("audio_file")
		if err != nil {
			t.Fatalf("read audio_file: %v", err)
		}
		gotFilename = header.Filename
		w.Write([]byte("this is the transcript"))
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)

	mux := http.NewServeMux()
	testHandlerWithSpeech(t, router, speech).Routes(mux)

	req := newAudioUploadRequest(t, "/issue-report/transcribe", tenantID, "farshid", []byte("fake audio bytes"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "this is the transcript" {
		t.Fatalf("expected the transcript verbatim, got %q", rec.Body.String())
	}
	if gotFilename == "" {
		t.Fatal("expected the audio to actually reach the speech server")
	}
}

// TestIssueReport_Transcribe_LargeRecordingSpillsToDiskNotHeap is a
// regression test for uc-infra#171 (see attachment_test.go's identical
// test for the full explanation of the technique): issueReportTranscribe
// was the one sibling ParseMultipartForm call site independent review
// found this fix had missed on its first pass — it still passed
// maxVoiceNoteBytes (the hard http.MaxBytesReader ceiling) as
// ParseMultipartForm's own maxMemory argument, keeping any recording
// near that cap entirely in the process heap instead of letting Go
// spill it to disk. Worse than the other two call sites: this endpoint
// has no per-tenant rate limit (see issueReportTranscribe's own doc
// comment), so it's the least-defended of the three against exactly
// this kind of buffering pressure.
//
// Calls issueReportTranscribe directly (bypassing DevAuth's
// r.WithContext, a request copy the test could no longer inspect
// afterward), same reasoning as attachment_test.go's own direct-handler
// tests.
func TestIssueReport_Transcribe_LargeRecordingSpillsToDiskNotHeap(t *testing.T) {
	router := newTestRouter(t)
	tenantID, _ := newTestTenant(t, router)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("fake speech server: parse multipart form: %v", err)
		}
		w.Write([]byte("transcript"))
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)
	h := testHandlerWithSpeech(t, router, speech)

	// 1 MiB over the in-memory threshold, comfortably under maxVoiceNoteBytes.
	audio := bytes.Repeat([]byte("z"), multipartParseMemory+(1<<20))
	req := newAudioUploadRequest(t, "/issue-report/transcribe", "", "", audio)
	req = req.WithContext(httpx.WithRequestContext(req.Context(), httpx.RequestContext{TenantID: tenantID, Actor: humanActor()}))

	rec := httptest.NewRecorder()
	h.issueReportTranscribe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("issueReportTranscribe: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if req.MultipartForm == nil {
		t.Fatal("expected req.MultipartForm to be populated after issueReportTranscribe ran")
	}
	defer func() {
		if err := req.MultipartForm.RemoveAll(); err != nil {
			t.Fatalf("cleanup temp files: %v", err)
		}
	}()

	fhs := req.MultipartForm.File["audio"]
	if len(fhs) != 1 {
		t.Fatalf("expected exactly 1 file part, got %d", len(fhs))
	}
	f, err := fhs[0].Open()
	if err != nil {
		t.Fatalf("Open uploaded file part: %v", err)
	}
	defer f.Close()

	if _, ok := f.(*os.File); !ok {
		// See attachment_test.go's identical assertion for the
		// single-file-part caveat — harmless here (this request has
		// exactly one file part).
		t.Fatalf("file part of %d bytes (> multipartParseMemory=%d) was not spilled to a temp file at the real issueReportTranscribe call site — got %T, want *os.File; it is being buffered entirely in the process heap", len(audio), multipartParseMemory, f)
	}
}

// TestIssueReport_Transcribe_LargeRecording_RoundTrips confirms the
// smaller multipartParseMemory (uc-infra#171) doesn't change observable
// behavior for a caller: a recording well over that threshold, still
// under maxVoiceNoteBytes, still reaches the speech server and comes
// back as a real transcript, exactly like the small-recording case in
// TestIssueReport_Transcribe_ReturnsTranscript.
//
// Calls issueReportTranscribe directly rather than routing through the
// real mux, same reasoning (and same leak this avoids) as
// TestIssueReport_Transcribe_LargeRecordingSpillsToDiskNotHeap and
// attachment_test.go's TestAttachmentUpload_Download_LargeFile_RoundTrips:
// DevAuth's r.WithContext would make the mutated request unreachable
// afterward, leaving no way to remove the spilled temp file by hand.
func TestIssueReport_Transcribe_LargeRecording_RoundTrips(t *testing.T) {
	router := newTestRouter(t)
	tenantID, _ := newTestTenant(t, router)

	var gotSize int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		_, header, err := r.FormFile("audio_file")
		if err != nil {
			t.Fatalf("read audio_file: %v", err)
		}
		gotSize = header.Size
		w.Write([]byte("this is the transcript"))
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)
	h := testHandlerWithSpeech(t, router, speech)

	audio := bytes.Repeat([]byte("z"), multipartParseMemory+(1<<20))
	req := newAudioUploadRequest(t, "/issue-report/transcribe", "", "", audio)
	req = req.WithContext(httpx.WithRequestContext(req.Context(), httpx.RequestContext{TenantID: tenantID, Actor: humanActor()}))

	rec := httptest.NewRecorder()
	h.issueReportTranscribe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "this is the transcript" {
		t.Fatalf("expected the transcript verbatim, got %q", rec.Body.String())
	}
	if gotSize != int64(len(audio)) {
		t.Fatalf("expected the full %d-byte recording to reach the speech server, got %d", len(audio), gotSize)
	}
	if req.MultipartForm != nil {
		t.Cleanup(func() {
			if err := req.MultipartForm.RemoveAll(); err != nil {
				t.Fatalf("cleanup temp files: %v", err)
			}
		})
	}
}

// TestIssueReport_Transcribe_ForwardsCurrentUILocaleAsLanguageHint is the
// real-server-request-shape proof for the reported bug where transcription
// only worked in English: the page's own current UI locale (Arabic, here)
// must actually reach speechassist as a language hint, not be silently
// dropped — see speechassist.Client.Transcribe's own doc comment on why
// leaving this to the Whisper server's auto-detect was unreliable for a
// short recording.
func TestIssueReport_Transcribe_ForwardsCurrentUILocaleAsLanguageHint(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	var gotLanguage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLanguage = r.URL.Query().Get("language")
		_ = r.ParseMultipartForm(1 << 20)
		w.Write([]byte("هذا هو النص"))
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)

	mux := http.NewServeMux()
	testHandlerWithSpeech(t, router, speech).Routes(mux)

	req := newAudioUploadRequest(t, "/issue-report/transcribe", tenantID, "farshid", []byte("fake audio bytes"))
	req.AddCookie(&http.Cookie{Name: localeCookie, Value: "ar"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotLanguage != "ar" {
		t.Fatalf("expected the current UI locale (ar) forwarded as the language hint, got %q", gotLanguage)
	}
}

// TestIssueReport_Transcribe_ServerErrorLogsAndReturns500 is the control
// case for TestIssueReport_Transcribe_ClientAbortDoesNotLogOrWriteResponse
// below: a genuine speechassist.Transcribe failure (not a client abort)
// must still log (via writeInternalError) and still return a real 500 —
// proving uc-infra#239's r.Context().Err() carve-out is a targeted
// exception for cancellation specifically, not a blanket "stop logging
// transcribe errors" regression. No test in this file exercised a real
// Transcribe failure at the Go-handler level before this (only the
// browser-side StaleTranscribeError e2e test, which fakes fetch/Response
// entirely and never reaches this handler at all).
func TestIssueReport_Transcribe_ServerErrorLogsAndReturns500(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "asr backend on fire", http.StatusInternalServerError)
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)

	mux := http.NewServeMux()
	testHandlerWithSpeech(t, router, speech).Routes(mux)

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(origOutput); log.SetFlags(origFlags) })

	req := newAudioUploadRequest(t, "/issue-report/transcribe", tenantID, "farshid", []byte("fake audio bytes"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on a genuine ASR backend failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "transcribe voice note") {
		t.Fatalf("expected a genuine ASR failure to still be logged via writeInternalError, got log output: %q", logBuf.String())
	}
}

// TestIssueReport_Transcribe_ClientAbortDoesNotLogOrWriteResponse is the
// regression test for uc-infra#239's independent-review finding #1: once
// #239's client-side AbortController fix made a superseded take's mic
// capture actually cancel its own /issue-report/transcribe request, a
// client abort became a ROUTINE outcome of any ordinary fast re-take —
// not a rare edge case — but issueReportTranscribe still funneled it
// through writeInternalError exactly like a genuine ASR/network fault,
// spraying an error-level "transcribe voice note: ... context canceled"
// log line indistinguishable from a real outage on every such re-take.
//
// Simulates the abort by cancelling the request's own context before
// issueReportTranscribe ever runs — http.Client.Do (inside
// speechassist.Transcribe) returns immediately with a context-cancellation
// error without ever reaching the network, the same effective shape a real
// browser closing the connection produces server-side. Calls
// issueReportTranscribe directly (bypassing DevAuth's own r.WithContext,
// which would produce a request the test can no longer control the
// context of afterward — same reasoning
// TestIssueReport_Transcribe_LargeRecordingSpillsToDiskNotHeap's own doc
// comment already gives for this pattern).
func TestIssueReport_Transcribe_ClientAbortDoesNotLogOrWriteResponse(t *testing.T) {
	router := newTestRouter(t)
	tenantID, _ := newTestTenant(t, router)

	// A REAL, reachable server (independent review, uc-infra#239: the
	// quiet early-return must be reached because the request's own
	// context was already cancelled BEFORE Do() ever runs — not because
	// some unrelated early return, or an unreachable baseURL, happened to
	// produce the same observable log/body-emptiness by coincidence). If
	// speechassist.Client.Transcribe were ever reached, this server would
	// answer with a real 200 and a real transcript — which the assertions
	// below would then contradict, proving the request actually never
	// went out.
	var serverWasDialed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverWasDialed = true
		w.Write([]byte("should never be seen"))
	}))
	defer srv.Close()
	speech := speechassist.NewClient(srv.URL)
	h := testHandlerWithSpeech(t, router, speech)

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(origOutput); log.SetFlags(origFlags) })

	req := newAudioUploadRequest(t, "/issue-report/transcribe", "", "", []byte("fake audio bytes"))
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // superseded take's AbortController firing, from the server's point of view
	req = req.WithContext(httpx.WithRequestContext(ctx, httpx.RequestContext{TenantID: tenantID, Actor: humanActor()}))

	rec := httptest.NewRecorder()
	h.issueReportTranscribe(rec, req)

	if rec.Body.Len() != 0 {
		t.Fatalf("expected no response body written for an aborted request, got %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Fatalf("expected no Content-Type header written for an aborted request (implies a body was written), got %q", ct)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("expected a client-aborted (context-cancelled) transcribe request to log nothing — an ordinary fast re-take must not spray an error-level log line indistinguishable from a real ASR outage; got log output: %q", logBuf.String())
	}
	if serverWasDialed {
		t.Fatal("expected the speech server to never be dialed at all for an already-cancelled request context — a request that reached the network (even one whose response was then discarded) is not what this test claims to prove")
	}
}

// TestIssueReport_Submit_CreatesRecordAndItsQueryable is the core
// end-to-end proof: submitting the form actually creates a real
// IssueReport record (tenant-scoped, audit-tracked, via the exact same
// crud.Engine every other entity in this kernel uses), not a bespoke
// side-channel — and it's genuinely queryable afterward through the
// generic /api/records route, same as anything else.
func TestIssueReport_Submit_CreatesRecordAndItsQueryable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	form := "title=" + "Button+does+nothing" +
		"&description=" + "Clicking+the+save+button+has+no+effect." +
		"&transcript=" + "Clicking+the+save+button+has+no+effect+spoken." +
		"&page_url=" + "https%3A%2F%2Fexample.com%2Fforms%2FVendor%2Fnew"
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "submitted") {
		t.Fatalf("expected a confirmation message, got:\n%s", rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/IssueReport", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 listing IssueReport records, got %d: %s", listRec.Code, listRec.Body.String())
	}
	body := listRec.Body.String()
	if !strings.Contains(body, "Button does nothing") {
		t.Fatalf("expected the submitted title to be queryable afterward, got:\n%s", body)
	}
	if !strings.Contains(body, `"status":"new"`) {
		t.Fatalf("expected the new report to default to status \"new\", got:\n%s", body)
	}
}

// TestIssueReport_Submit_StoresConsoleLog is the storage-layer proof for
// universaltill/uc-infra#46's log-capture slice: whatever the capture
// page's JS pre-filled the (visible, human-reviewed) console-log textarea
// with is exactly what ends up on the record, same as transcript already
// does — this handler doesn't re-derive or filter it.
func TestIssueReport_Submit_StoresConsoleLog(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	form := "title=" + "Save+button+throws" +
		"&description=" + "Clicking+save+throws+a+JS+error." +
		"&console_log=" + "%5Berror%5D+TypeError%3A+cannot+read+properties+of+undefined"
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/IssueReport", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 listing IssueReport records, got %d: %s", listRec.Code, listRec.Body.String())
	}
	body := listRec.Body.String()
	if !strings.Contains(body, "TypeError: cannot read properties of undefined") {
		t.Fatalf("expected the submitted console_log to be queryable afterward, got:\n%s", body)
	}
}

// TestIssueReport_Submit_NoConsoleLogIsFine confirms console_log stays
// genuinely optional: a browser that never captured anything (or an old
// client without this field) must not fail submission — same "empty
// means absent" contract page_url/user_agent already have.
func TestIssueReport_Submit_NoConsoleLogIsFine(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	form := "title=" + "No+console+activity" + "&description=" + "Nothing+to+report."
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no console_log submitted, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIssueReport_Submit_MissingRequiredFieldIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// title omitted entirely.
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte("description=Something+is+wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing required title, got %d: %s", rec.Code, rec.Body.String())
	}
	// Translated entity.validation.required envelope, not
	// ValidateRecord's raw Detail text (uc-infra#96, independent review:
	// this call site reuses validationErrorMessage directly rather than
	// through writeValidationErrorLocalized, and had no assertion pinning
	// that it actually translates).
	if want := `title is required.`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("expected the translated envelope message %q, got: %s", want, rec.Body.String())
	}
}

// TestIssueReport_Submit_OversizedDescriptionIs400 (uc-infra#174) is the
// HTTP-handler-level regression test for the actual bug: description had
// no length bound at all before entity.Field.MaxLength existed, so a
// caller posting directly to this endpoint (no browser, no maxlength
// attribute in the way — issueReportTmpl's own textarea attribute is
// UI-only, never a security boundary) could write an arbitrarily large
// text blob here, bounded only by maxIssueReportSubmitBytes (61 MiB,
// sized for the accompanying screen-recording upload, not for a text
// field). Plain urlencoded, deliberately, to prove this doesn't depend
// on the multipart path uc-infra#92 introduced.
func TestIssueReport_Submit_OversizedDescriptionIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	oversized := strings.Repeat("a", 20001) // one over foundation.IssueReport's declared MaxLength
	form := "title=" + "Something+is+wrong" + "&description=" + oversized
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized description, got %d: %s", rec.Code, rec.Body.String())
	}
	if want := `description is too long.`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("expected the translated envelope message %q, got: %s", want, rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/IssueReport", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), "Something is wrong") {
		t.Fatal("rejected submission must not have been stored")
	}
}

// TestIssueReport_Submit_OversizedConsoleLogIs400 is console_log's own
// version of TestIssueReport_Submit_OversizedDescriptionIs400 immediately
// above — the other field the originating review found unbounded.
func TestIssueReport_Submit_OversizedConsoleLogIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	oversized := strings.Repeat("a", 30001) // one over foundation.IssueReport's declared MaxLength
	form := "title=" + "Something+is+wrong" + "&description=" + "Fine." + "&console_log=" + oversized
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized console_log, got %d: %s", rec.Code, rec.Body.String())
	}
	if want := `console_log is too long.`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("expected the translated envelope message %q, got: %s", want, rec.Body.String())
	}
}

// TestIssueReport_Submit_OversizedTitleIs400 (uc-infra#174, independent
// review) is title's own version of the two tests above — the original
// fix only bounded description/console_log, but title rides in the exact
// same request through the identical unbounded-FieldString gap and was
// just as reachable by a caller posting directly here.
func TestIssueReport_Submit_OversizedTitleIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	oversized := strings.Repeat("a", 501) // one over foundation.IssueReport's declared MaxLength
	form := "title=" + oversized + "&description=" + "Fine."
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized title, got %d: %s", rec.Code, rec.Body.String())
	}
	if want := `title is too long.`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("expected the translated envelope message %q, got: %s", want, rec.Body.String())
	}
}

// newIssueReportSubmitRequest builds a multipart/form-data
// /issue-report/submit request — the shape issueReportTmpl's form now
// always posts as (uc-infra#92), text fields plus an optional
// screen_recording file part. recording == nil omits the file part
// entirely (the common case: no screen recording captured).
func newIssueReportSubmitRequest(t *testing.T, target, tenantID, actorID string, fields map[string]string, recording []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if recording != nil {
		// Not mw.CreateFormFile: that hardcodes Content-Type:
		// application/octet-stream, but a real browser sends the
		// captured File's own .type ("video/webm" — see issueReportTmpl's
		// `new File([blob], ..., { type: "video/webm" })`) as this part's
		// Content-Type. CreatePart with an explicit header is what
		// actually reproduces that.
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="`+screenRecordingFormField+`"; filename="screen-recording.webm"`)
		header.Set("Content-Type", "video/webm")
		fw, err := mw.CreatePart(header)
		if err != nil {
			t.Fatalf("create screen_recording form part: %v", err)
		}
		if _, err := fw.Write(recording); err != nil {
			t.Fatalf("write screen_recording content: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if tenantID != "" {
		r.Header.Set("X-Tenant-ID", tenantID)
	}
	if actorID != "" {
		r.Header.Set("X-Actor-ID", actorID)
	}
	return r
}

// TestIssueReport_NewPage_ShowsScreenRecordButtonWhenAttachmentsEnabled
// confirms issueReportNewPage's AttachmentsEnabled gate (uc-infra#92):
// with a blobstore wired, the screen-record control renders.
func TestIssueReport_NewPage_ShowsScreenRecordButtonWhenAttachmentsEnabled(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	req := newRequest("GET", "/issue-report/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="uc-issue-screenrecord-btn"`) {
		t.Fatalf("expected the screen-record button when a blobstore is configured, got:\n%s", body)
	}
	if !strings.Contains(body, `enctype="multipart/form-data"`) {
		t.Fatalf("expected the capture form to post multipart/form-data, got:\n%s", body)
	}
}

// TestIssueReport_NewPage_HidesScreenRecordButtonWhenAttachmentsDisabled
// is the other half of the same gate: no blobstore configured (the
// default testHandler, matching a deployment that never called
// SetBlobstore) means the control is never rendered at all — never a
// dead, always-failing button (uc-infra#92 design note: this is the
// primary defense against attachScreenRecording's rare fallback path
// ever being reachable through the real UI).
func TestIssueReport_NewPage_HidesScreenRecordButtonWhenAttachmentsDisabled(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux) // no blobstore wired

	req := newRequest("GET", "/issue-report/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, `id="uc-issue-screenrecord-btn"`) {
		t.Fatalf("expected no screen-record button with no blobstore configured, got:\n%s", body)
	}
}

// TestIssueReport_Submit_MultipartWithoutRecording_StillCreatesRecord
// confirms issueReportTmpl's move to enctype="multipart/form-data" for
// EVERY submission (not just ones carrying a recording) didn't break the
// ordinary text-only path — ParseMultipartForm's own doc comment says it
// "calls ParseForm if necessary", but that's worth pinning with a real
// multipart (not urlencoded) request specifically, since every other
// test in this file predates uc-infra#92 and only ever exercises the
// urlencoded shape.
func TestIssueReport_Submit_MultipartWithoutRecording_StillCreatesRecord(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newIssueReportSubmitRequest(t, "/issue-report/submit", tenantID, "farshid", map[string]string{
		"title":       "Multipart, no recording",
		"description": "Plain multipart submission with no screen_recording part.",
	}, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "recording") {
		t.Fatalf("expected no recording-related note when nothing was recorded, got:\n%s", rec.Body.String())
	}
}

// TestIssueReport_Submit_WithScreenRecording_CreatesLinkedAttachment is
// the core end-to-end proof for uc-infra#92: a submission carrying a
// screen_recording part durably stores the blob and links it to the new
// IssueReport via a real Attachment record (entity_type="IssueReport"),
// discoverable and downloadable exactly like any other attachment
// (attachment.go's own already-tested machinery) — not a bespoke
// side-channel.
func TestIssueReport_Submit_WithScreenRecording_CreatesLinkedAttachment(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	recording := bytes.Repeat([]byte("screen-bytes"), 100)
	req := newIssueReportSubmitRequest(t, "/issue-report/submit", tenantID, "farshid", map[string]string{
		"title":       "Save throws on click",
		"description": "See attached recording.",
	}, recording)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "could not be attached") {
		t.Fatalf("expected no attach-failure note on the happy path, got:\n%s", rec.Body.String())
	}

	// Find the created IssueReport's id.
	listReq := newRequest("GET", "/api/records/IssueReport", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	var issueList struct {
		Data []struct {
			ID   string `json:"id"`
			Data struct {
				Title string `json:"title"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &issueList); err != nil {
		t.Fatalf("decode IssueReport list: %v (%s)", err, listRec.Body.String())
	}
	var issueID string
	for _, rec := range issueList.Data {
		if rec.Data.Title == "Save throws on click" {
			issueID = rec.ID
		}
	}
	if issueID == "" {
		t.Fatalf("expected to find the created IssueReport, got: %s", listRec.Body.String())
	}

	// Find the linked Attachment and confirm its shape.
	attReq := newRequest("GET", "/api/records/Attachment", tenantID, "farshid", nil)
	attRec := httptest.NewRecorder()
	mux.ServeHTTP(attRec, attReq)
	var attList struct {
		Data []struct {
			ID   string `json:"id"`
			Data struct {
				EntityType  string  `json:"entity_type"`
				RecordID    string  `json:"record_id"`
				FileName    string  `json:"file_name"`
				MimeType    string  `json:"mime_type"`
				SizeBytes   float64 `json:"size_bytes"`
				StoragePath string  `json:"storage_path"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(attRec.Body.Bytes(), &attList); err != nil {
		t.Fatalf("decode Attachment list: %v (%s)", err, attRec.Body.String())
	}
	var attachmentID string
	for _, a := range attList.Data {
		if a.Data.EntityType == "IssueReport" && a.Data.RecordID == issueID {
			attachmentID = a.ID
			if a.Data.MimeType != "video/webm" {
				t.Errorf("expected mime_type video/webm, got %q", a.Data.MimeType)
			}
			if int(a.Data.SizeBytes) != len(recording) {
				t.Errorf("expected size_bytes %d, got %v", len(recording), a.Data.SizeBytes)
			}
			if !strings.HasPrefix(a.Data.StoragePath, tenantID+"/IssueReport/"+issueID+"/") {
				t.Errorf("expected storage_path namespaced under %s/IssueReport/%s/, got %q", tenantID, issueID, a.Data.StoragePath)
			}
		}
	}
	if attachmentID == "" {
		t.Fatalf("expected a linked Attachment for IssueReport %s, got: %s", issueID, attRec.Body.String())
	}

	// And the blob is genuinely retrievable, byte-for-byte, through the
	// same generic download endpoint every other attachment uses.
	dlReq := newRequest("GET", "/api/attachments/"+attachmentID, tenantID, "farshid", nil)
	dlRec := httptest.NewRecorder()
	mux.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("expected 200 downloading the recording, got %d: %s", dlRec.Code, dlRec.Body.String())
	}
	if !bytes.Equal(dlRec.Body.Bytes(), recording) {
		t.Fatalf("expected the downloaded bytes to round-trip exactly, got %d bytes, want %d", dlRec.Body.Len(), len(recording))
	}
}

// TestIssueReport_Submit_ScreenRecordingWithNoBlobstoreConfigured_
// SavesReportAndNotesFailure is attachScreenRecording's defensive
// fallback path (uc-infra#92 design note): even though
// issueReportNewPage's AttachmentsEnabled gate keeps this unreachable
// through the real UI in practice (no blobstore means the button was
// never rendered), a request that somehow still carries a
// screen_recording part must not lose the report itself — the IssueReport
// is saved regardless, with a translated note instead of a silent drop.
func TestIssueReport_Submit_ScreenRecordingWithNoBlobstoreConfigured_SavesReportAndNotesFailure(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux) // no blobstore wired

	req := newIssueReportSubmitRequest(t, "/issue-report/submit", tenantID, "farshid", map[string]string{
		"title":       "Recording with no storage configured",
		"description": "Should still save.",
	}, []byte("some video bytes"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 — the report itself must still be saved, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not be attached") {
		t.Fatalf("expected the translated recording_not_attached note, got:\n%s", rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/IssueReport", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), "Recording with no storage configured") {
		t.Fatalf("expected the IssueReport to be saved despite the attach failure, got:\n%s", listRec.Body.String())
	}

	attReq := newRequest("GET", "/api/records/Attachment", tenantID, "farshid", nil)
	attRec := httptest.NewRecorder()
	mux.ServeHTTP(attRec, attReq)
	if strings.Contains(attRec.Body.String(), `"entity_type":"IssueReport"`) {
		t.Fatalf("expected no Attachment record created when storage isn't configured, got:\n%s", attRec.Body.String())
	}
}

// TestIssueReport_Submit_OversizedRecordingIs400 confirms
// maxIssueReportSubmitBytes is actually enforced at the HTTP boundary,
// same "cap it before it ever reaches application logic" contract every
// other upload in this package documents.
func TestIssueReport_Submit_OversizedRecordingIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	oversized := bytes.Repeat([]byte("x"), maxIssueReportSubmitBytes+1024)
	req := newIssueReportSubmitRequest(t, "/issue-report/submit", tenantID, "farshid", map[string]string{
		"title":       "Too big",
		"description": "This recording is oversized.",
	}, oversized)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized submission, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIssueReport_Submit_MalformedUrlencodedBodyIs400 is the regression
// test for independent review's finding on the first version of
// uc-infra#92: issueReportSubmit used to call r.ParseMultipartForm
// unconditionally and swallow any http.ErrNotMultipart it returned,
// treating that as "this is just a legacy urlencoded submission, safe to
// proceed." But Go's ParseMultipartForm returns http.ErrNotMultipart
// whenever the content type isn't multipart/form-data REGARDLESS of
// whether its own internal ParseForm call succeeded — it only surfaces
// ParseForm's own error on ParseMultipartForm's success path. So a
// genuinely malformed urlencoded body (a bare semicolon — rejected by
// net/url's ParseQuery since Go 1.17 as an invalid separator, and a real
// character a pasted JS stack trace can easily contain) used to be
// silently accepted with the offending field just dropped from
// r.PostForm, rather than 400ing like every other malformed submission.
// The fix branches on Content-Type explicitly instead of relying on
// ParseMultipartForm's dual-purpose error.
func TestIssueReport_Submit_MalformedUrlencodedBodyIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	form := "title=Bug&description=ok&console_log=a;b"
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed urlencoded body (bare semicolon), got %d: %s", rec.Code, rec.Body.String())
	}
}
