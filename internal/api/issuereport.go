// The in-app issue logger captures a voice recording and an optional
// screen recording alongside typed context, transcribes the voice note
// (internal/kernel/speechassist, a self-hosted Whisper ASR instance —
// verified end-to-end against the platform's reference deployment
// before this was written), and stores it as a real IssueReport record
// (foundation.go) via the same generic entity/crud/audit machinery
// every other entity in this kernel uses, with any screen recording
// stored separately as a linked Attachment (attachScreenRecording
// below) rather than on the IssueReport record itself.
//
// Deliberately NOT in this slice, each for its own reason:
//   - Automatic GitHub issue filing: needs a GitHub credential with
//     issue-write access to the target repo, which per this kernel's
//     secret-creation-needs-explicit-authorization discipline has to be
//     provisioned deliberately, not created unilaterally by a session
//     building app code. This entity is the durable store either way —
//     filing to GitHub later is a step that reads from here, not a
//     replacement for storing reports here.
//   - Redaction/consent review before anything leaves the browser: the
//     capture page shows the full description+transcript, and a video
//     preview for any screen recording, for review before Submit is
//     ever clickable, so nothing captured is sent anywhere the human
//     submitting it hasn't already read or watched — but there's no
//     separate confirmation step beyond that yet.
package api

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// maxVoiceNoteBytes bounds an uploaded voice-note recording — generous
// for a few minutes of compressed (webm/opus, what MediaRecorder
// produces by default in every real browser) audio, same "cap it at the
// HTTP boundary" reasoning maxUploadBytes documents for the CSV import
// wizard.
const maxVoiceNoteBytes = 10 << 20 // 10 MiB

// issueReportFieldMaxLength reads a declared entity.Field.MaxLength
// straight off the compiled-in foundation.IssueReport() Definition
// (uc-infra#174, independent review) rather than a second, hand-kept
// Go constant: an earlier draft duplicated each bound as its own
// constant here, with nothing pinning the two together — exactly the
// drift maxScreenRecordingBytes's OWN doc comment above warns against
// ("one source of truth, not two numbers that happen to agree today").
// foundation.IssueReport() is a plain Go function with no I/O, so
// calling it here at render time costs nothing worth caching. Panics if
// name isn't a FieldString with a declared MaxLength — a programmer
// error (a typo'd field name, or forgetting to declare the bound), not
// a runtime condition any caller of this page could trigger, so failing
// loud immediately beats silently rendering maxlength="0" and rejecting
// every real submission.
//
// Rendered as the capture page's own textarea/input maxlength=
// attributes below (ADR-0017 §5's "validation defined once, applied
// identically client- and server-side"); the real enforcement either
// way is entity.ValidateRecord in issueReportSubmit, which reads the
// TENANT'S published Definition (possibly a different, later-migrated
// version than this compiled-in default) — so this is a same-process,
// same-commit UI hint, not a promise that every tenant's actual bound
// is identical to it forever.
func issueReportFieldMaxLength(name string) int {
	f, ok := foundation.IssueReport().FieldByName(name)
	if !ok || f.MaxLength == nil {
		panic(fmt.Sprintf("issueReportFieldMaxLength: %q has no declared MaxLength on foundation.IssueReport()", name))
	}
	return *f.MaxLength
}

// screenRecordingDurationCapSeconds bounds how long the capture page's
// JS lets a screen recording run before auto-stopping it (uc-infra#92):
// long enough to actually show a bug reproducing, short enough that the
// video stays a reasonable size at the bitrate below without needing a
// mid-recording server round-trip to check. Referenced from Go only for
// the size-cap arithmetic in the doc comment below — the actual
// enforcement is client-side (issueReportTmpl's own script) plus the
// server-side byte cap as a hard backstop.
const screenRecordingDurationCapSeconds = 180 // 3 minutes

// maxScreenRecordingBytes bounds an uploaded screen-recording video. At
// the videoBitsPerSecond hint the capture page's MediaRecorder requests
// (2 Mbps — see issueReportTmpl), a full screenRecordingDurationCapSeconds
// recording is ~45 MiB; this leaves headroom for encoder/keyframe
// variance without landing anywhere near the CSV import wizard's 50 MiB
// cap (import.go) — video legitimately needs more than either that or
// the 20 MiB generic attachment cap (attachment.go), same "cap it at the
// HTTP boundary" reasoning as every other upload in this package.
const maxScreenRecordingBytes = 60 << 20 // 60 MiB

// maxIssueReportSubmitBytes bounds the WHOLE /issue-report/submit
// request body — text fields plus the optional screen-recording file
// part together. maxScreenRecordingBytes dominates; the extra 1 MiB is
// headroom for the form's own text fields (title/description/transcript/
// console_log can each carry a fair amount of pasted text) and
// multipart boundary/header overhead, neither of which individually
// approaches this cap on its own.
const maxIssueReportSubmitBytes = maxScreenRecordingBytes + (1 << 20) // 61 MiB

// screenRecordingFormField is the multipart field name the capture
// page's JS attaches the recorded video Blob under — a hidden
// <input type="file"> set programmatically via a DataTransfer (see
// issueReportTmpl's own doc comment on why: no HTMX equivalent exists
// for assigning a captured Blob to a file input either). Mirrors
// issueReportTranscribe's "audio" field name and attachmentUpload's
// "file" field name — one fixed name per upload shape in this package.
const screenRecordingFormField = "screen_recording"

// isMultipartFormData reports whether r's Content-Type is
// multipart/form-data — the explicit branch issueReportSubmit uses to
// decide between ParseMultipartForm and plain ParseForm, rather than
// calling ParseMultipartForm unconditionally and swallowing its
// http.ErrNotMultipart return (see issueReportSubmit's own doc comment
// on why that would silently discard a real ParseForm error on a
// malformed urlencoded body instead).
func isMultipartFormData(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// issueReportNewPage renders the capture form: a title field, a
// description textarea, a record-voice-note button (real browser
// MediaRecorder JS — no HTMX equivalent exists for microphone capture),
// and a transcript preview the recording fills in via
// issueReportTranscribe, all editable before Submit.
func (h *Handler) issueReportNewPage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)

	var buf bytes.Buffer
	err := issueReportTmpl.ExecuteTemplate(&buf, "page", issueReportPageView{
		TranscribeHref:               "/issue-report/transcribe",
		SubmitHref:                   "/issue-report/submit",
		TitleLabel:                   h.catalog.T(locale, "issue_report.title_label"),
		DescriptionLabel:             h.catalog.T(locale, "issue_report.description_label"),
		RecordLabel:                  h.catalog.T(locale, "issue_report.record_button"),
		StopLabel:                    h.catalog.T(locale, "issue_report.stop_button"),
		TranscribingLabel:            h.catalog.T(locale, "issue_report.transcribing"),
		TranscriptLabel:              h.catalog.T(locale, "issue_report.transcript_label"),
		ConsoleLogLabel:              h.catalog.T(locale, "issue_report.console_log_label"),
		SubmitLabel:                  h.catalog.T(locale, "issue_report.submit_button"),
		NoMicLabel:                   h.catalog.T(locale, "issue_report.no_mic"),
		ScreenRecordLabel:            h.catalog.T(locale, "issue_report.screen_record_button"),
		RecordingLabel:               h.catalog.T(locale, "issue_report.screen_recording_label"),
		ScreenPreviewLabel:           h.catalog.T(locale, "issue_report.screen_preview_label"),
		NoScreenShareLabel:           h.catalog.T(locale, "issue_report.no_screen_share"),
		ScreenRecordingTooLargeLabel: h.catalog.T(locale, "issue_report.screen_recording_too_large"),
		// AttachmentsEnabled gates the screen-record control client-side
		// (issueReportTmpl): a real deployment always wires a blobstore
		// (handlers.go's own doc comment on the field), so this is
		// primarily a dev/test-environment degrade, not a production
		// path — attachScreenRecording below still re-checks
		// defensively, since a page rendered before a hot-reconfigure
		// could still submit with this true.
		AttachmentsEnabled:                h.blobstore != nil,
		ScreenRecordingDurationCapSeconds: screenRecordingDurationCapSeconds,
		MaxScreenRecordingBytes:           maxScreenRecordingBytes,
		TitleMaxLength:                    issueReportFieldMaxLength("title"),
		DescriptionMaxLength:              issueReportFieldMaxLength("description"),
		TranscriptMaxLength:               issueReportFieldMaxLength("transcript"),
		ConsoleLogMaxLength:               issueReportFieldMaxLength("console_log"),
	})
	if err != nil {
		writeInternalError(w, "render issue report page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render issue report page shell", err)
	}
}

// issueReportTranscribe accepts an uploaded voice-note recording and
// returns its transcript as plain text — a disabled or failing
// speechassist.Client is reported as a real error to the caller (unlike
// the import wizard's AI mapping assist, which always has a non-AI
// fallback to degrade to, there's nothing to fall back to for "the user
// asked to transcribe a recording" — the browser-side JS surfaces this
// as "couldn't transcribe, type it instead," not a silent failure).
//
// No per-tenant rate limit or quota on this endpoint yet: each call is
// a real, synchronous request to the self-hosted Whisper server (up to
// speechassist's own timeout), so an authenticated-but-abusive caller
// could repeatedly call this to tie up request goroutines / Whisper
// compute. Accepted as a v1 tradeoff (auth is still required — see
// requestContext below, an anonymous caller can't reach this at all),
// not a gap to silently ignore if this ever needs hardening further.
func (h *Handler) issueReportTranscribe(w http.ResponseWriter, r *http.Request) {
	if _, ok := requestContext(w, r); !ok {
		return
	}
	if !h.speech.Enabled() {
		httpx.WriteError(w, http.StatusServiceUnavailable, "voice transcription is not configured for this deployment")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceNoteBytes)
	// multipartParseMemory (not maxVoiceNoteBytes) as ParseMultipartForm's
	// maxMemory bound — see that constant's own doc comment for why.
	// Passing maxVoiceNoteBytes itself here (this endpoint's own
	// pre-uc-infra#171 bug, the one sibling call site independent review
	// found this fix had missed on its first pass — this handler has no
	// per-tenant rate limit, see this function's own doc comment above,
	// making it the worst candidate of the three for unbounded heap
	// buffering) meant any recording near that cap stayed entirely in
	// memory instead of spilling. Note this call site gets only the
	// same PARTIAL benefit readUploadedFile's own call site does, not
	// attachmentUpload's full one: speechassist.Client.Transcribe
	// re-buffers file into its own in-memory multipart body before
	// forwarding it to the Whisper server, so this only avoids
	// duplicating the in-heap copy during PARSING, not the one
	// Transcribe itself makes afterward.
	if err := r.ParseMultipartForm(multipartParseMemory); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid or oversized recording (max %d MiB): %s", maxVoiceNoteBytes>>20, err.Error()))
		return
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, `no recording uploaded (expected an "audio" form field)`)
		return
	}
	defer file.Close()

	filename := header.Filename
	if filename == "" {
		filename = "note.webm"
	}
	// The page's own current UI locale is the best language hint this
	// handler has for what the recording is actually spoken in — see
	// speechassist.Client.Transcribe's own doc comment on why leaving
	// this to the server's auto-detect was unreliable for a short
	// recording against the reference deployment's smallest model.
	// localeFromRequest never mutates anything on a POST with no
	// "?lang=" query param (this endpoint's URL never carries one) —
	// it only reads the existing locale cookie the page load already set.
	locale := localeFromRequest(w, r)
	transcript, err := h.speech.Transcribe(r.Context(), file, filename, locale)
	if err != nil {
		// uc-infra#239, independent review: a superseded take's client-side
		// AbortController closes this connection, which cancels r.Context()
		// and — since Transcribe threads it through to the outbound Whisper
		// call — surfaces here as a plain request-failed error wrapping
		// context.Canceled, exactly like any other network failure. That is
		// now a ROUTINE outcome of a normal fast re-take (the whole point of
		// #239's own fix), not a real transcription failure: the client that
		// issued this request is already gone and never sees this response
		// either way, so writeInternalError's log.Printf would misreport an
		// ordinary abort as a genuine ASR/network fault indistinguishable
		// from one in the logs. r.Context().Err() (rather than
		// errors.Is(err, context.Canceled) against speechassist's wrapped
		// error) is checked directly: it's the actual, authoritative signal
		// for "this request's own context ended," independent of exactly how
		// speechassist/net/http happened to wrap the underlying error.
		//
		// Independent review, nitpick: this is a superset of "client
		// aborted" — it also silences a genuine Whisper-side failure on the
		// rarer coincidence that the client happened to disconnect first
		// (e.g. the person closed the tab while a real outage was in
		// progress), and would (in principle, not reachable through this
		// deployment's own net/http server + a real browser fetch(), which
		// always closes the connection on abort) mask a reverse-proxy-level
		// cancellation unrelated to any client AbortController. Accepted:
		// either way the original caller is already gone and was never
		// going to see this response, so the only real cost is a missed
		// log line in an already-rare coincidence — a smaller cost than
		// this endpoint's other alternative, misreporting every ordinary
		// fast re-take as a genuine ASR fault.
		if r.Context().Err() != nil {
			return
		}
		writeInternalError(w, "transcribe voice note", err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, transcript)
}

// issueReportSubmit creates the actual IssueReport record. description
// is exactly what the human submitting it last saw in the textarea
// (typed text, possibly with the transcript already merged in by the
// page's own JS, possibly further hand-edited) — this handler doesn't
// re-derive or re-merge anything, it trusts the reviewed, submitted
// text the same way any other form submission in this kernel does.
// transcript is kept separately, verbatim, purely as an audit trail of
// what the ASR actually produced before any human edit.
func (h *Handler) issueReportSubmit(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	def, err := ts.entityDef(r.Context(), "IssueReport")
	if err != nil {
		writeDefinitionLookupError(w, "IssueReport", err)
		return
	}

	// The capture page now posts multipart/form-data unconditionally
	// (issueReportTmpl) so a screen-recording file part can ride along
	// with the ordinary text fields on the exact same native, non-fetch
	// browser POST — but a plain application/x-www-form-urlencoded
	// submission (every test in this package predating uc-infra#92, and
	// any client that never captured a recording) must still work.
	// Branching on Content-Type explicitly, rather than always calling
	// ParseMultipartForm and swallowing its http.ErrNotMultipart return,
	// is deliberate: ParseMultipartForm calls ParseForm internally and
	// only surfaces ParseForm's OWN error on its success path (Go's
	// ParseMultipartForm returns `parseFormErr` only after a successful
	// multipart parse; if multipartReader itself fails — e.g. because
	// this isn't a multipart body at all — it returns THAT error
	// instead, discarding parseFormErr). Blindly ignoring
	// ErrNotMultipart would therefore also silently ignore a REAL
	// ParseForm failure on a genuinely malformed urlencoded body (a
	// stray query-string separator in a pasted console_log stack trace,
	// for one) and proceed on partially-populated fields instead of
	// 400ing — independent review caught this on the first pass.
	r.Body = http.MaxBytesReader(w, r.Body, maxIssueReportSubmitBytes)
	if isMultipartFormData(r) {
		// multipartParseMemory (not maxIssueReportSubmitBytes) as
		// ParseMultipartForm's maxMemory bound — see that constant's own
		// doc comment for why. A spilled part still reads back fine
		// through header.Open() in attachScreenRecording below; this
		// constant only controls where the bytes sit while parsing, not
		// what the handler can do with them afterward.
		if err := r.ParseMultipartForm(multipartParseMemory); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid or oversized submission (max %d MiB): %s", maxIssueReportSubmitBytes>>20, err.Error()))
			return
		}
	} else if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid form submission: "+err.Error())
		return
	}
	// An empty form value is NOT set into fields at all — same "empty
	// means absent, not a zero value" discipline csvimport.buildRowData's
	// own doc comment establishes for exactly this reason: it keeps a
	// blank "title" indistinguishable from an omitted one, which matters
	// on any Definition where NotBefore or similar treats presence and
	// absence differently, even though entity.ValidateRecord's Required
	// check (#86) would now catch an explicit "" here too. status is the
	// one field set outside this loop — always "new" for a fresh
	// submission, never sourced from the form.
	fields := map[string]any{"status": "new"}
	for name, value := range map[string]string{
		"title":       r.PostForm.Get("title"),
		"description": r.PostForm.Get("description"),
		"transcript":  r.PostForm.Get("transcript"),
		"console_log": r.PostForm.Get("console_log"),
		"page_url":    r.PostForm.Get("page_url"),
		"user_agent":  r.Header.Get("User-Agent"),
	} {
		if value != "" {
			fields[name] = value
		}
	}

	if err := entity.ValidateRecord(def, fields); err != nil {
		log.Printf("api: validate new IssueReport: %v", err)
		// nil: no EffectiveWriteFields call on this path either (uc-infra#178).
		httpx.WriteError(w, http.StatusBadRequest, h.validationErrorMessage(locale, err, nil))
		return
	}
	rec, err := ts.crud.Create(r.Context(), def, fields, rc.Actor)
	if err != nil {
		writeInternalError(w, "create issue report", err)
		return
	}

	recordingNote := h.attachScreenRecording(r, ts, rec.ID, rc, locale)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := issueReportTmpl.ExecuteTemplate(w, "result", issueReportResultView{
		Heading:       h.catalog.T(locale, "issue_report.result_heading"),
		Body:          h.catalog.T(locale, "issue_report.result_body"),
		ID:            rec.ID,
		RecordingNote: recordingNote,
	}); err != nil {
		writeInternalError(w, "render issue report result", err)
	}
}

// attachScreenRecording durably stores an optional screen-recording
// video the capture page's JS attached under screenRecordingFormField,
// linking it to the just-created IssueReport record recordID via the
// same generic Attachment machinery attachmentUpload (attachment.go)
// uses — reusing its storage-key/orphan-cleanup helpers rather than
// reimplementing them. This orchestration stays here, entity-specific,
// rather than inside attachment.go's own entity-agnostic handlers (same
// "no `if entityType == ...`" boundary attachment.go's own doc comment
// draws for the generic engine).
//
// By the time this runs, the IssueReport record itself is ALREADY
// durably created (issueReportSubmit only calls this after ts.crud.
// Create succeeds) — a failure here (no storage configured, a transient
// write error) never unwinds the report the human just filed. It
// returns a non-empty, already-localized note for the result page
// instead, so nobody is left silently wondering whether their recording
// actually made it in — same "never a silent failure" principle
// no_screen_share/NoMicLabel already follow on the capture side. This is
// deliberately NOT attachmentUpload's own "fail closed, nothing saved"
// contract: that endpoint's only job IS the attachment, so refusing
// outright is correct there; here the attachment is a secondary
// enhancement to a primary submission that already succeeded, and the
// capture page's own AttachmentsEnabled flag (issueReportNewPage) already
// keeps this the rare/defensive path in practice — a real deployment
// always wires a blobstore (handlers.go's own doc comment on the field).
func (h *Handler) attachScreenRecording(r *http.Request, ts tenantScope, recordID string, rc httpx.RequestContext, locale string) string {
	if r.MultipartForm == nil {
		return ""
	}
	files := r.MultipartForm.File[screenRecordingFormField]
	if len(files) == 0 {
		return ""
	}
	header := files[0]

	fail := func(reason string, err error) string {
		if err != nil {
			log.Printf("api: attach screen recording to IssueReport %s: %s: %v", recordID, reason, err)
		} else {
			log.Printf("api: attach screen recording to IssueReport %s: %s", recordID, reason)
		}
		return h.catalog.T(locale, "issue_report.recording_not_attached")
	}

	if h.blobstore == nil {
		return fail("attachment storage is not configured for this deployment", nil)
	}
	attachmentDef, err := ts.entityDef(r.Context(), "Attachment")
	if err != nil {
		return fail("resolve Attachment definition", err)
	}
	file, err := header.Open()
	if err != nil {
		return fail("open uploaded recording", err)
	}
	defer file.Close()

	key, err := attachmentStorageKey(rc.TenantID, "IssueReport", recordID, header.Filename)
	if err != nil {
		return fail("compute attachment storage key", err)
	}
	if err := h.blobstore.Put(r.Context(), key, file); err != nil {
		return fail("store recording blob", err)
	}

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "video/webm"
	}
	fields := map[string]any{
		"entity_type":  "IssueReport",
		"record_id":    recordID,
		"file_name":    header.Filename,
		"mime_type":    mimeType,
		"size_bytes":   float64(header.Size),
		"storage_path": key,
	}
	if err := entity.ValidateRecord(attachmentDef, fields); err != nil {
		h.cleanupOrphanBlob(r.Context(), key)
		return fail("validate Attachment record", err)
	}
	if _, err := ts.crud.Create(r.Context(), attachmentDef, fields, rc.Actor); err != nil {
		h.cleanupOrphanBlob(r.Context(), key)
		return fail("create Attachment record", err)
	}
	return ""
}

// --- view models ---

type issueReportPageView struct {
	TranscribeHref     string
	SubmitHref         string
	TitleLabel         string
	DescriptionLabel   string
	RecordLabel        string
	StopLabel          string
	TranscribingLabel  string
	TranscriptLabel    string
	ConsoleLogLabel    string
	SubmitLabel        string
	NoMicLabel         string
	ScreenRecordLabel  string
	RecordingLabel     string
	ScreenPreviewLabel string
	NoScreenShareLabel string
	// ScreenRecordingTooLargeLabel is shown (independent review,
	// uc-infra#92: the earlier version had no client-side size check at
	// all, so an oversized recording only surfaced as the server's
	// MaxBytesReader 400ing the WHOLE submission — title, description,
	// transcript and all) when the captured Blob exceeds
	// MaxScreenRecordingBytes: the recording is dropped from the
	// hidden file input (never rides along with Submit) but the rest of
	// the form is left completely intact.
	ScreenRecordingTooLargeLabel string
	// AttachmentsEnabled hides the screen-record control entirely when
	// false (no configured blobstore) rather than letting someone record
	// a video that can never actually be attached — see its doc comment
	// at the call site (issueReportNewPage).
	AttachmentsEnabled                bool
	ScreenRecordingDurationCapSeconds int
	// MaxScreenRecordingBytes mirrors the Go-side maxScreenRecordingBytes
	// constant so the client-side size check above and the server's own
	// MaxBytesReader boundary can never drift apart — one source of
	// truth, not two numbers that happen to agree today.
	MaxScreenRecordingBytes int
	// TitleMaxLength/DescriptionMaxLength/TranscriptMaxLength/
	// ConsoleLogMaxLength (uc-infra#174) are each read straight from
	// foundation.IssueReport()'s own declared entity.Field.MaxLength via
	// issueReportFieldMaxLength — see that function's own doc comment.
	TitleMaxLength       int
	DescriptionMaxLength int
	TranscriptMaxLength  int
	ConsoleLogMaxLength  int
}

type issueReportResultView struct {
	Heading string
	Body    string
	ID      string
	// RecordingNote is empty when no screen recording was submitted, or
	// when one was submitted and attached successfully — it is only
	// ever populated (already localized) by attachScreenRecording's rare
	// failure path, so the person who filed the report is never left
	// silently wondering whether their recording made it in.
	RecordingNote string
}

// The record-voice-note control is real JS, not HTMX: capturing a
// microphone needs MediaRecorder's start/stop lifecycle and a Blob to
// upload, none of which HTMX's declarative attributes can express —
// same reasoning the CSV import wizard's own file-input step already
// isn't a pure-HTMX flow either. Once recording stops, the blob is
// POSTed via fetch to TranscribeHref and the response fills the
// transcript textarea and gets appended into description — both stay
// fully editable afterward, nothing is hidden from the person
// submitting before they click Submit.
var issueReportTmpl = template.Must(template.New("issue-report").Parse(`
{{define "page"}}
<div class="uc-issue-report">
<form id="uc-issue-report-form" class="uc-form" method="post" action="{{.SubmitHref}}" enctype="multipart/form-data">
<label for="uc-issue-title">{{.TitleLabel}}</label>
<input type="text" id="uc-issue-title" name="title" maxlength="{{.TitleMaxLength}}" required>

<label for="uc-issue-description">{{.DescriptionLabel}}</label>
<textarea id="uc-issue-description" name="description" rows="6" maxlength="{{.DescriptionMaxLength}}" required></textarea>

<div class="uc-issue-voice">
<button type="button" id="uc-issue-record-btn">{{.RecordLabel}}</button>
<span id="uc-issue-record-status"></span>
</div>

<label for="uc-issue-transcript">{{.TranscriptLabel}}</label>
<textarea id="uc-issue-transcript" name="transcript" rows="4" maxlength="{{.TranscriptMaxLength}}" readonly></textarea>

{{if .AttachmentsEnabled}}
<div class="uc-issue-screenrecord">
<button type="button" id="uc-issue-screenrecord-btn">{{.ScreenRecordLabel}}</button>
<span id="uc-issue-screenrecord-status"></span>
<div id="uc-issue-screenrecord-preview-wrap" style="display:none">
<label for="uc-issue-screenrecord-preview">{{.ScreenPreviewLabel}}</label>
<video id="uc-issue-screenrecord-preview" controls playsinline></video>
</div>
<input type="file" name="screen_recording" id="uc-issue-screenrecord-file" style="display:none">
</div>
{{end}}

<label for="uc-issue-console-log">{{.ConsoleLogLabel}}</label>
<textarea id="uc-issue-console-log" name="console_log" rows="4" maxlength="{{.ConsoleLogMaxLength}}"></textarea>

<input type="hidden" name="page_url" id="uc-issue-page-url">
<button type="submit">{{.SubmitLabel}}</button>
</form>
<div id="uc-issue-report-result"></div>
</div>
<script>
(function() {
  document.getElementById("uc-issue-page-url").value = window.location.href;

  // Same per-tenant key derivation as internal/api/layout.go's shellTmpl
  // (ucTenantKey there) — duplicated rather than shared, since this page
  // has no JS module system to share it through; both must agree on the
  // key shape or the prefill silently finds nothing.
  function ucTenantKey() {
    var nav = document.querySelector("[data-uc-tenant]");
    return "ucConsoleLog:" + (nav ? nav.getAttribute("data-uc-tenant") : "anonymous");
  }

  // Pre-fill the console-log textarea from whatever internal/api/layout.
  // go's shellTmpl has been buffering into sessionStorage, scoped to this
  // tenant, since this tab's session started — best-effort, same as the
  // capture itself: a browser that never ran that script (or cleared
  // storage) just leaves this textarea blank, exactly like "no mic"
  // leaves the transcript blank. Left editable (not readonly) so a human
  // who spots something they'd rather not send — a token, another
  // tenant's data leaking through some bug — can strike it before Submit.
  try {
    var rawLog = window.sessionStorage.getItem(ucTenantKey());
    if (rawLog) {
      var entries = JSON.parse(rawLog);
      if (Array.isArray(entries) && entries.length > 0) {
        // Clamped to ConsoleLogMaxLength defensively (uc-infra#174,
        // independent review): this is a script assignment to .value,
        // not a typed keystroke, so the textarea's own maxlength
        // attribute above does NOT apply here — a real browser only
        // enforces maxlength against user input, never against .value
        // set programmatically. layout.go's ucAppendLog already caps
        // what it buffers per-entry (its own doc comment), so this
        // should be a no-op in practice; kept as the actual enforcement
        // point rather than relying solely on that upstream discipline
        // holding forever.
        var joined = entries.join("\n");
        document.getElementById("uc-issue-console-log").value =
          joined.length > {{.ConsoleLogMaxLength}} ? joined.slice(0, {{.ConsoleLogMaxLength}}) : joined;
      }
    }
  } catch (e) {
    // malformed/unavailable sessionStorage — leave the textarea blank.
  }

  // Clear this tenant's buffered log once a submission is underway, so a
  // second report filed later in the same tab session doesn't keep
  // re-surfacing (and re-storing) console output that already made it
  // into a filed report. This is a plain (non-htmx) form post — the
  // browser navigates away regardless of whether the submission actually
  // succeeds, so "before Submit's native navigation proceeds" is the only
  // hook available; a failed submission (e.g. a validation 400) also
  // clears it, an accepted trade-off for not needing a fetch-based
  // submit flow just for this.
  document.getElementById("uc-issue-report-form").addEventListener("submit", function() {
    try {
      window.sessionStorage.removeItem(ucTenantKey());
    } catch (e) {
      // best-effort, same as capture itself.
    }
  });

  var recordBtn = document.getElementById("uc-issue-record-btn");
  var statusEl = document.getElementById("uc-issue-record-status");
  var transcriptEl = document.getElementById("uc-issue-transcript");
  var descriptionEl = document.getElementById("uc-issue-description");
  // mediaRecorder identifies "the currently active recording" for the
  // Stop branch below (mediaRecorder.stop()) AND, since uc-infra#223, for
  // onstop's own thisRecorder identity check further down — reassigning
  // it on every Record click is still safe: whenever recording === true,
  // mediaRecorder is provably the take that owns that flag (both are set
  // together, synchronously, in the same task), so a stale take's onstop
  // comparing against the reassigned value is exactly how it tells it no
  // longer owns the current recording/button state. chunks is NOT
  // declared here (uc-infra#196): it
  // used to be a shared outer-scope array, reset in place on every Record
  // click and read by whichever take's onstop happened to fire, whenever
  // that was. Stop resets recording/the button label synchronously, but
  // a real MediaRecorder's onstop/ondataavailable fire asynchronously — a
  // fast Stop-then-Record-again, before the first take's onstop has
  // actually run, let the second take's reset wipe the shared array out
  // from under the first take's still-pending upload. chunks is now
  // declared fresh with var inside the getUserMedia .then() callback
  // below instead, so every take gets its own array that its own
  // ondataavailable/onstop close over — a var inside a function body is
  // scoped to that specific invocation, not shared across the callback's
  // repeated invocations, so no other closure machinery is needed.
  var mediaRecorder = null;
  var recording = false;
  // uc-infra#238: recordAttemptGeneration/recordingGeneration replace the
  // three transcribe-write guards' old "mediaRecorder === thisRecorder"
  // check (mediaRecorder/thisRecorder above are now used ONLY for the Stop
  // button branch and onstop's own button-label resync — an unrelated
  // concern this issue leaves untouched). Independent review of uc-infra
  // #221 found recorder *identity* is the wrong proxy for "is a stale
  // take's write safe to drop": it changes the moment a NEW MediaRecorder
  // is merely constructed, not when a take actually starts recording or
  // definitively fails — two narrow, reachable gaps fell out of that
  // mismatch (this issue's own "Gap A"/"Gap B"). Two independent counters
  // close both:
  //   - recordAttemptGeneration bumps once per Record click, synchronously,
  //     before getUserMedia is even called — it marks "the user's most
  //     recent intent," the thing that should own the STATUS LINE
  //     (statusEl) regardless of whether this attempt goes on to succeed or
  //     fail. Gap B (a getUserMedia rejection never reassigns
  //     mediaRecorder, so a still-in-flight older take's later success used
  //     to silently wipe the freshly-shown "permission denied" message) is
  //     closed because the click alone — independent of any MediaRecorder
  //     ever existing — already claims this generation.
  //   - recordingGeneration bumps only once a take's mediaRecorder.start()
  //     actually succeeds — it marks "the most recent take whose data is
  //     real," the thing that should own the TRANSCRIPT/DESCRIPTION
  //     fields. Gap A (mediaRecorder.start() throwing after
  //     "mediaRecorder = new MediaRecorder(stream)" already reassigned the
  //     shared variable, which used to make an older, entirely legitimate
  //     still-in-flight take look "superseded" and silently drop its
  //     transcript even though nothing ever actually replaced it) is
  //     closed because a failed start never bumps this counter — the older
  //     take's data-generation check still passes.
  // recordAttemptGeneration bumps synchronously at click time, strictly
  // before recordingGeneration can bump (which needs a round trip through
  // getUserMedia first) — so for the ordinary case where a click goes on
  // to a successful start, there IS a real window (the whole
  // permission-prompt wait) where the two counters disagree about which
  // take currently owns the status line, not just on a click that fails.
  // Independent review: that window is harmless only because the click
  // handler clears statusEl synchronously, in the same turn as the
  // recordAttemptGeneration bump (see that call site's own comment) — an
  // OLDER take's status write is correctly suppressed starting at the
  // click, and something new is visibly shown starting at the exact same
  // moment, so the disagreement is never user-observable. Without that
  // clear, the two counters would have left a real, user-visible gap
  // (the status line stuck on a stale "Transcribing…", or a stale take's
  // own error silently swallowed) for as long as the permission prompt
  // stayed unanswered — this was caught by review, not shipped.
  var recordAttemptGeneration = 0;
  var recordingGeneration = 0;
  // transcribeAbortController identifies "the most recent recordingGeneration-
  // owning attempt's own request-cancellation handle" — one level down
  // from recordingGeneration itself (deliberately NOT recordAttemptGeneration
  // — see below), one level below: aborting it cancels that take's
  // in-flight /issue-report/transcribe fetch (uc-infra#239, the
  // AbortController design pass uc-infra#238's own commit deferred here)
  // instead of only letting the recordingGeneration guard drop its result
  // silently once it eventually resolves. Dropping the result already
  // stopped a stale take's response from being *applied*; this stops the
  // request from being *sent at all* past the point it's genuinely
  // superseded, cutting real ASR load on a fast re-take sequence.
  // Reassigned (after aborting whatever it previously pointed to) at the
  // exact same moment myRecordingGeneration = ++recordingGeneration runs
  // in the getUserMedia().then() callback below — i.e. once a NEW take
  // has actually reached a successful mediaRecorder.start(), the same
  // event that makes recordingGeneration itself advance.
  //
  // Deliberately NOT tied to recordAttemptGeneration / the Record click
  // itself, despite that being the more obvious "supersede now" signal
  // (and this fix's own first draft did exactly that): independent review
  // caught that a click alone doesn't mean the PREVIOUS take's data has
  // actually been superseded — recordAttemptGeneration and
  // recordingGeneration deliberately disagree while a new attempt's own
  // getUserMedia/mediaRecorder.start() hasn't yet succeeded (uc-infra#238's
  // whole point: recordAttemptGeneration owns the status line eagerly,
  // recordingGeneration owns the transcript/description only once data is
  // real). Aborting at click time would cancel a still-legitimate EARLIER
  // take's real, in-flight transcribe call whenever the new attempt's own
  // start() throws or its getUserMedia rejects (#238's Gap A/Gap B,
  // respectively) — destroying a transcript recordingGeneration's own
  // .then guard would otherwise have correctly let land, since nothing
  // genuinely superseded it in that case. Tying cancellation to
  // recordingGeneration's own bump instead means "this take's request was
  // cancelled" and "this take's eventual result would have been dropped
  // anyway" are now the same condition, not two independently-timed ones.
  var transcribeAbortController = null;

  // Deliberately an if/else, NOT an early "return" out of the whole IIFE
  // (independent review, uc-infra#92: the earlier version DID return
  // here, which meant the screen-recording setup below — entirely
  // unrelated to microphone capture — never ran at all whenever
  // navigator.mediaDevices was falsy, e.g. any non-secure-context page
  // load. AttachmentsEnabled would still render the screen-record button
  // into the DOM, but with no click handler and never disabled — a
  // silently dead control in exactly the degraded environment the
  // NoScreenShareLabel degrade exists to handle instead).
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia || !window.MediaRecorder) {
    recordBtn.disabled = true;
    statusEl.textContent = {{.NoMicLabel}};
  } else {
    // extractErrorMessage reads httpx's own {"data":null,"error":"..."}
    // envelope (internal/httpx/envelope.go) out of a failed response body
    // and returns just the human message — the raw envelope text used to
    // be dumped verbatim into statusEl on any failure (e.g. a disabled
    // speechassist.Client's 503), showing a user the whole raw JSON
    // envelope instead of a readable sentence. Falls back to the raw text
    // only if it isn't the JSON shape this endpoint actually returns
    // (never expected in practice, but safer than throwing here).
    var extractErrorMessage = function(rawText) {
      try {
        var body = JSON.parse(rawText);
        if (body && typeof body.error === "string" && body.error !== "") {
          return body.error;
        }
      } catch (e) {
        // Not JSON — fall through to the raw text.
      }
      return rawText;
    };

    recordBtn.addEventListener("click", function() {
      if (recording) {
        // uc-infra#223: mediaRecorder can already be "inactive" here
        // without this click being the cause — the underlying
        // MediaStream can end on its own (mic permission revoked
        // mid-recording, the input device unplugged/disabled, or any
        // other browser-initiated track-ended event), which a real
        // MediaRecorder reacts to by transitioning itself to "inactive"
        // and firing onstop, exactly as an explicit .stop() call would.
        // Before this fix nothing here checked for that: the button
        // still read "Stop recording" (only onstop's own reset below
        // resyncs it, and — before this fix — onstop never did), so a
        // click on it read as a genuine Stop and called .stop() again on
        // an already-inactive recorder, throwing InvalidStateError per
        // the MediaStream Recording spec — uncaught, inside this click
        // handler. Same guard shape uc-infra#220 already uses for the
        // screen-record handler's own version of this bug.
        if (mediaRecorder && mediaRecorder.state !== "inactive") {
          mediaRecorder.stop();
        }
        recording = false;
        recordBtn.textContent = {{.RecordLabel}};
        return;
      }
      if (recordBtn.disabled) {
        // Already awaiting a getUserMedia permission prompt from an
        // earlier click on this same button — same race the
        // screen-record button's own click handler below guards against
        // (independent review, uc-infra#92), just for the mic
        // (uc-infra#173): without this guard, two clicks before the
        // first prompt resolves start two separate MediaRecorders
        // against one shared mediaRecorder variable, and Stop only
        // ever reaches whichever one the variable currently points at,
        // leaving the other stream's tracks live indefinitely. The two
        // lines right below (setting disabled, before the async
        // permission prompt even opens) are what close that window
        // entirely, rather than trying to reconcile two in-flight
        // recorders after the fact.
        return;
      }
      // uc-infra#238: claims this click's own attempt generation
      // synchronously, before getUserMedia is even called — see the outer
      // recordAttemptGeneration/recordingGeneration comment above for why
      // this alone (independent of whether the attempt below goes on to
      // succeed or fail) is what closes Gap B. The immediate clear right
      // below is NOT cosmetic (independent review): claiming the status
      // line here without also writing to it left a real window open —
      // getUserMedia's own permission prompt can stay pending indefinitely
      // (the person simply doesn't answer it yet), during which an OLDER
      // take's still-in-flight transcribe result now fails this guard (as
      // intended) but nothing NEW has been written yet either, so the
      // status line was left stuck showing that older take's stale
      // "Transcribing…" — or, worse, silently swallowing that older take's
      // own real transcribe error — for as long as the prompt sits
      // unanswered. Clearing synchronously here makes "claim the status
      // line" and "show something for it" atomic, the same guarantee the
      // old identity guard got for free by only transferring ownership at
      // "new MediaRecorder(stream)", a point always immediately followed
      // by a write (the success/throw branches below).
      var myAttemptGeneration = ++recordAttemptGeneration;
      statusEl.textContent = "";
      recordBtn.disabled = true;
      navigator.mediaDevices.getUserMedia({ audio: true }).then(function(stream) {
        recordBtn.disabled = false;
        // try/catch around everything from here through mediaRecorder.
        // start(), not just the getUserMedia promise itself (independent
        // review, uc-infra#173): getUserMedia can resolve fine and then
        // MediaRecorder's own constructor (or any other synchronous step
        // before start()) throw — e.g. an unsupported mimeType in a
        // future edit — and without this, that path fell through to
        // nothing, leaving the just-granted stream's tracks live
        // indefinitely with no code path left that could ever stop them.
        // Mirrors the screen-record handler's own try/catch below, just
        // without an options object to catch a rejection of (the mic
        // recorder takes none), so this catches any other constructor
        // failure instead.
        try {
          // Local to this .then() invocation (uc-infra#196) — see the
          // outer-scope comment above for why this can no longer be the
          // shared chunks the old code reassigned here.
          var chunks = [];
          mediaRecorder = new MediaRecorder(stream);
          // uc-infra#223: this take's own recorder instance, captured at
          // creation time so onstop below can tell whether it's still the
          // current take by the time it actually fires — see onstop's own
          // comment for why that check matters.
          var thisRecorder = mediaRecorder;
          // uc-infra#238: declared here (before use), assigned only once
          // mediaRecorder.start() below actually succeeds — see the
          // outer-scope comment for why that (as opposed to claiming it at
          // construction, like thisRecorder above, or at click time, like
          // recordAttemptGeneration) is what closes Gap A. Left undefined
          // if start() throws, which is what a failed take's onstop/
          // transcribe callbacks — which never fire in that case — would
          // otherwise have read.
          var myRecordingGeneration;
          // uc-infra#239: thisController mirrors myRecordingGeneration
          // exactly — declared here (undefined), only actually ARMED
          // (previous attempt's controller aborted, this attempt's own
          // controller stored) once mediaRecorder.start() below actually
          // succeeds. Independent review of this fix's own first draft
          // caught a real bug in aborting any earlier (e.g. at click time,
          // right alongside recordAttemptGeneration's own bump): cancelling
          // the PREVIOUS attempt's transcribe request must track
          // recordingGeneration, not recordAttemptGeneration — the exact
          // distinction uc-infra#238 drew between which counter owns the
          // status line (recordAttemptGeneration) versus the transcript/
          // description (recordingGeneration, deliberately looser — see
          // that counter's own outer-scope comment). Aborting eagerly
          // would cancel a still-legitimate EARLIER take's real, in-flight
          // transcribe call whenever THIS attempt's own start() throws or
          // its getUserMedia never even resolves (uc-infra#238's Gap A/
          // Gap B) — silently destroying a transcript that
          // recordingGeneration's own guard would otherwise have let land
          // normally, since nothing genuinely superseded it in that case.
          var thisController;
          mediaRecorder.ondataavailable = function(e) { if (e.data.size > 0) chunks.push(e.data); };
          mediaRecorder.onstop = function() {
            // uc-infra#223: resync recording/the button label here too,
            // not just on an explicit Stop click, so a browser-initiated
            // end of the stream (see the click handler's own comment
            // above) re-syncs the button on its own instead of leaving it
            // stuck reading Stop until another click limps it back via
            // the guard there. Guarded on mediaRecorder === thisRecorder
            // (uc-infra#196's own "fast Stop-then-Record-again" scenario,
            // documented on the outer mediaRecorder var above): a stale
            // take's onstop can fire after a newer take has already
            // started and reassigned the shared mediaRecorder variable —
            // resetting unconditionally here would stomp that newer
            // take's own "Stop recording" state right out from under it.
            // Harmless/idempotent on the ordinary explicit-Stop path,
            // where the click handler has already reset both
            // synchronously before this async callback even runs.
            if (mediaRecorder === thisRecorder) {
              recording = false;
              recordBtn.textContent = {{.RecordLabel}};
            }
            stream.getTracks().forEach(function(t) { t.stop(); });
            var blob = new Blob(chunks, { type: "audio/webm" });
            // uc-infra#221 (guard introduced) / uc-infra#238 (guard
            // corrected): #196 legitimized a fast Stop-then-Record-again as
            // a supported flow (isolating each take's audio bytes so they
            // can no longer mix), but every write below this point used to
            // be unconditional, so a still-in-flight earlier take's
            // "Transcribing…" status, its eventual transcript/description,
            // or its transcribe error could each land on screen *after* a
            // newer take had already started — clobbering whatever the
            // newer, currently-visible take had shown, or (for transcript/
            // description) silently overwriting user edits made in the
            // meantime with a stale take's result. Since uc-infra#239, this
            // take's request is also cancelled outright the moment a LATER
            // take genuinely supersedes it — i.e. the moment that later
            // take's own recordingGeneration bump fires (see
            // transcribeAbortController's own outer-scope comment for why
            // that specific event, not the Record click itself). That is
            // deliberately the *same* condition the myRecordingGeneration
            // !== recordingGeneration guard below already checks — not a
            // separate, independently-timed layer — so cancellation and
            // "would this result have been dropped anyway" never disagree:
            // a request only ever gets cancelled when its own result was
            // already going to be discarded. The status-line write
            // immediately below stays governed by recordAttemptGeneration
            // exactly as before (uc-infra#238) — recordAttemptGeneration
            // and recordingGeneration can and do diverge while a newer
            // attempt's getUserMedia/start() is still pending, so a stale
            // take's status write can still be (and needs to still be)
            // guarded out here well before that take's own request is ever
            // cancelled. Re-checked at every write below (not just once
            // here), since a newer take can start (or fail to start) at
            // any point while this fetch is still in flight. Guarded on
            // recordAttemptGeneration, NOT recorder identity (uc-infra
            // #238: identity changes the instant a new MediaRecorder is
            // merely constructed, even one whose own .start() goes on to
            // throw — recordAttemptGeneration instead tracks the user's
            // actual Record clicks, see the outer-scope comment for the
            // full rationale).
            if (myAttemptGeneration === recordAttemptGeneration) {
              statusEl.textContent = {{.TranscribingLabel}};
            }
            var form = new FormData();
            form.append("audio", blob, "note.webm");
            fetch({{.TranscribeHref}}, { method: "POST", body: form, signal: thisController.signal })
              .then(function(resp) {
                if (!resp.ok) { return resp.text().then(function(t) { throw new Error(extractErrorMessage(t)); }); }
                return resp.text();
              })
              .then(function(text) {
                // uc-infra#238: transcript/description are guarded on
                // recordingGeneration specifically (not recordAttemptGeneration,
                // which the statusEl write above and the catch below use) —
                // this take's actual recorded data must still land even if a
                // LATER take's own attempt went on to fail (Gap A: a failed
                // start must not silently drop a still-legitimate earlier
                // take's transcript, since nothing genuine ever superseded
                // it). recordingGeneration only advances on a take that
                // actually reached a successful mediaRecorder.start(), so a
                // failed later attempt never invalidates this check.
                if (myRecordingGeneration !== recordingGeneration) {
                  return;
                }
                // Both assignments below are clamped defensively
                // (uc-infra#174, independent review): text is the ASR
                // server's own response, script-assigned to .value, so
                // neither field's maxlength attribute above applies (a
                // browser only enforces maxlength against typed/pasted
                // user input, never a programmatic .value set) — without
                // this, a long enough transcript could still make the
                // eventual /issue-report/submit 400 on a field the user
                // never directly touched, discarding the whole
                // submission (title, screen recording and all) with no
                // chance to fix it first.
                if (text.length > {{.TranscriptMaxLength}}) {
                  text = text.slice(0, {{.TranscriptMaxLength}});
                }
                transcriptEl.value = text;
                // description's own bound is deliberately SMALLER than
                // transcript's (see foundation.IssueReport's own doc
                // comment on why) — the appended COMBINATION is clamped
                // to description's bound here, independent of (and on
                // top of) the transcript-only clamp just above.
                var combined = descriptionEl.value.trim() !== ""
                  ? descriptionEl.value.replace(/\s+$/, "") + "\n\n" + text
                  : text;
                if (combined.length > {{.DescriptionMaxLength}}) {
                  combined = combined.slice(0, {{.DescriptionMaxLength}});
                }
                descriptionEl.value = combined;
                // uc-infra#238: this clear is a STATUS-LINE write (unlike
                // the transcript/description writes just above), so it uses
                // recordAttemptGeneration, same as the "Transcribing…" write
                // that opened this onstop handler — a later take's own
                // failure (Gap B: e.g. a getUserMedia rejection, which never
                // even reaches this file's mediaRecorder var) must keep
                // owning the status line, not have this take's own
                // late-arriving success silently erase it.
                if (myAttemptGeneration === recordAttemptGeneration) {
                  statusEl.textContent = "";
                }
              })
              .catch(function(err) {
                // uc-infra#238: same recordAttemptGeneration guard as the
                // success path's statusEl clear above — a stale take's
                // transcribe failure must not stomp a newer attempt's own
                // in-progress/idle/error status line.
                if (myAttemptGeneration === recordAttemptGeneration) {
                  statusEl.textContent = String(err.message || err);
                }
              });
          };
          mediaRecorder.start();
          recording = true;
          // uc-infra#238: assigned only now that mediaRecorder.start() has
          // actually succeeded (declared earlier, next to thisRecorder —
          // see that declaration's own comment for why this timing is what
          // closes Gap A).
          myRecordingGeneration = ++recordingGeneration;
          // uc-infra#239: only NOW — once this attempt has genuinely
          // become the new recordingGeneration-owning take — is the
          // PREVIOUS attempt's own transcribe request actually cancelled
          // (see thisController's own declaration above for why any
          // earlier point would be wrong). Safe to call unconditionally
          // even if the previous attempt's fetch already settled, was
          // never sent (its own getUserMedia never resolved, so its
          // thisController was never armed either), or doesn't exist yet
          // (the very first successful take): AbortController.abort() on
          // an already-settled/never-armed/undefined-checked-via-the-if
          // controller is a spec no-op, and aborting a controller before
          // fetch() is ever called on it just makes that later fetch()
          // call reject immediately with AbortError once it does run —
          // caught by the same recordAttemptGeneration guard every
          // stale-take status write site below already uses.
          if (transcribeAbortController) {
            transcribeAbortController.abort();
          }
          thisController = new AbortController();
          transcribeAbortController = thisController;
          recordBtn.textContent = {{.StopLabel}};
          statusEl.textContent = "";
        } catch (e) {
          stream.getTracks().forEach(function(t) { t.stop(); });
          statusEl.textContent = String(e.message || e);
        }
      }).catch(function(err) {
        recordBtn.disabled = false;
        statusEl.textContent = String(err.message || err);
      });
    });
  }

  // Screen recording (uc-infra#92): same "no HTMX equivalent" reasoning
  // as the mic capture above — getDisplayMedia's permission prompt and
  // MediaRecorder's start/stop lifecycle are both real browser APIs HTMX
  // has no declarative attribute for. Only present in the DOM at all
  // when AttachmentsEnabled was true at page-render time (see
  // issueReportNewPage's own doc comment on why: no configured
  // blobstore means a recording could never actually be attached).
  var screenBtn = document.getElementById("uc-issue-screenrecord-btn");
  if (screenBtn) {
    var screenStatusEl = document.getElementById("uc-issue-screenrecord-status");
    var previewWrap = document.getElementById("uc-issue-screenrecord-preview-wrap");
    var previewEl = document.getElementById("uc-issue-screenrecord-preview");
    var fileInput = document.getElementById("uc-issue-screenrecord-file");
    // screenRecorder identifies "the currently active recording" for the
    // Stop branch below AND the auto-stop timeout's own guard further
    // down — both are fine to read the reassigned outer value, since both
    // only ever act on whatever recording is current right now. (The mic
    // handler's mediaRecorder is read in one more place since uc-infra#223
    // — its onstop's own thisRecorder identity check — for the same
    // "always fine to read the reassigned current value" reason.)
    // screenChunks/screenTimerId/screenAutoStopId/screenStartedAt
    // are NOT declared here (uc-infra#196, same defense-in-depth fix as
    // the mic handler) — each is now declared fresh with var inside the
    // getDisplayMedia .then() callback below, so every take gets its own
    // copies. Verified against this handler's actual Stop branch that,
    // unlike the mic handler, screenRecording/the button label are only
    // ever reset inside onstop, never synchronously on Stop click — so a
    // screen-record "Stop then Record again" click sequence can't reach a
    // second recording the way the mic race description depends on (the
    // second click is read as another Stop, not a new Record, until
    // onstop has already run). This fix is kept anyway for consistency
    // with the mic handler and because it closes a latent hazard rather
    // than a currently-reachable one — if Stop's reset timing is ever
    // changed to be synchronous (matching the mic handler), this would
    // otherwise become live at that point instead of being caught here.
    var screenRecorder = null;
    var screenRecording = false;

    // revokePreviousPreview releases the previous recording's blob: URL
    // (if any) before a new one replaces it — independent review,
    // uc-infra#92: re-recording (explicitly supported — clicking Record
    // Screen again just overwrites screenChunks) previously left every
    // earlier take's blob pinned in memory for the rest of the page's
    // lifetime, since URL.createObjectURL was never paired with a
    // matching revokeObjectURL.
    var revokePreviousPreview = function() {
      if (previewEl.src) {
        URL.revokeObjectURL(previewEl.src);
      }
    };

    if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia || !window.MediaRecorder) {
      screenBtn.disabled = true;
      screenStatusEl.textContent = {{.NoScreenShareLabel}};
    } else {
      screenBtn.addEventListener("click", function() {
        if (screenRecording) {
          // uc-infra#220: screenRecording is only ever reset inside onstop
          // (see the outer-scope comment above), never synchronously here,
          // so a redundant Stop click before the first click's onstop has
          // fired would otherwise call .stop() a second time on a recorder
          // the spec already transitioned to "inactive" synchronously as
          // part of the first .stop() call — an uncaught InvalidStateError
          // inside this handler. Same guard the auto-stop timeout below
          // already uses against the same spec behavior.
          if (screenRecorder && screenRecorder.state !== "inactive") {
            screenRecorder.stop();
          }
          return;
        }
        if (screenBtn.disabled) {
          // Already awaiting a getDisplayMedia permission prompt from an
          // earlier click on this same button — independent review,
          // uc-infra#92: without this guard, two clicks before the first
          // prompt resolves opened two separate capture streams, and
          // Stop only ever stopped the second one, leaving the first
          // stream's tracks (and the browser's "you are sharing your
          // screen" indicator) live indefinitely. Disabling synchronously
          // here, before the async permission prompt even opens, closes
          // that window entirely rather than trying to reconcile two
          // in-flight recorders after the fact.
          return;
        }
        screenBtn.disabled = true;
        navigator.mediaDevices.getDisplayMedia({ video: true }).then(function(stream) {
          screenBtn.disabled = false;
          // Local to this .then() invocation (uc-infra#196) — see the
          // outer-scope comment above.
          var screenChunks = [];
          // try/catch around everything from the MediaRecorder
          // constructor fallback through screenRecorder.start(), not
          // just the primary constructor call (independent review,
          // uc-infra#198): a throw reaching either the primary
          // constructor's fallback or start() itself previously escaped
          // into the outer .then(...).catch(...) below, which surfaces
          // the error message and re-enables the button but has no
          // access to stream (a separate function's own parameter) to
          // stop its tracks — leaving the just-granted display-share
          // stream (and the browser's "you are sharing your screen"
          // indicator) live indefinitely. Mirrors the mic handler's own
          // try/catch above.
          try {
            try {
              screenRecorder = new MediaRecorder(stream, { videoBitsPerSecond: 2000000 });
            } catch (e) {
              // A browser that rejects the videoBitsPerSecond hint (or the
              // whole options object) still gets a recording, just without
              // the explicit bitrate hint — the duration cap still bounds
              // recording length either way, and the onstop handler below
              // independently checks the resulting blob's actual size
              // before ever attaching it, regardless of what bitrate the
              // browser chose on its own.
              screenRecorder = new MediaRecorder(stream);
            }
            // onstop, not the click handler that calls .stop(), is where
            // this cleanup has to live: reaching the duration cap calls
            // screenRecorder.stop() directly (see screenAutoStopId below)
            // without ever going through this button's own click handler,
            // so onstop is the one place both paths converge.
            screenRecorder.onstop = function() {
              stream.getTracks().forEach(function(t) { t.stop(); });
              clearInterval(screenTimerId);
              clearTimeout(screenAutoStopId);
              var blob = new Blob(screenChunks, { type: "video/webm" });
              // Independent review, uc-infra#92: the videoBitsPerSecond
              // hint above is only a hint (a multi-monitor/4K share, or
              // the no-options MediaRecorder fallback just above, routinely
              // encodes well past it) — this is the ONE place that checks
              // what actually got recorded, before it ever touches the
              // hidden file input. Without it, an oversized recording only
              // surfaced server-side as MaxBytesReader 400ing the ENTIRE
              // submission — title, description, transcript and all —
              // destroying everything the person typed over a video they
              // may not even have cared about keeping.
              if (blob.size > {{.MaxScreenRecordingBytes}}) {
                revokePreviousPreview();
                fileInput.value = ""; // clear any earlier (valid-sized) file that rode in fileInput.files
                previewEl.removeAttribute("src");
                previewWrap.style.display = "none";
                screenStatusEl.textContent = {{.ScreenRecordingTooLargeLabel}};
                screenBtn.textContent = {{.ScreenRecordLabel}};
                screenRecording = false;
                return;
              }
              try {
                var file = new File([blob], "screen-recording.webm", { type: "video/webm" });
                var dt = new DataTransfer();
                dt.items.add(file);
                fileInput.files = dt.files;
              } catch (e) {
                // File/DataTransfer construction unsupported — the
                // recording is still shown for review below, it just won't
                // ride along with Submit. Same "degrade, never silently
                // break the rest of the form" posture as every other
                // capture control on this page.
              }
              revokePreviousPreview();
              previewEl.src = URL.createObjectURL(blob);
              previewWrap.style.display = "";
              screenStatusEl.textContent = "";
              screenBtn.textContent = {{.ScreenRecordLabel}};
              screenRecording = false;
            };
            screenRecorder.ondataavailable = function(e) { if (e.data.size > 0) screenChunks.push(e.data); };
            screenRecorder.start();
            screenRecording = true;
            screenBtn.textContent = {{.StopLabel}};
            // Local to this .then() invocation (uc-infra#196), same as
            // screenChunks above — independent review: this was left
            // shared in an earlier draft even though only the per-take
            // interval callback just below reads it, which would have
            // let a still-running earlier take's ticker read a newer
            // take's start time once started.
            var screenStartedAt = Date.now();
            // Also local (uc-infra#196) — onstop's clearInterval/
            // clearTimeout calls above (in the onstop handler defined
            // earlier in this same .then() callback) close over these via
            // the normal closure/hoisting rules, same as every other
            // local var in this scope.
            var screenTimerId = setInterval(function() {
              var elapsed = Math.floor((Date.now() - screenStartedAt) / 1000);
              var m = Math.floor(elapsed / 60);
              var s = elapsed % 60;
              screenStatusEl.textContent = {{.RecordingLabel}} + " " + m + ":" + (s < 10 ? "0" : "") + s;
            }, 1000);
            // Auto-stop at the duration cap (screenRecordingDurationCapSeconds,
            // issuereport.go) rather than relying on the person to remember
            // to click Stop — bounds recording length without a
            // mid-recording server round-trip; the blob.size check above
            // is what actually bounds the upload, since duration alone
            // doesn't bound bytes at an unpredictable real-world bitrate.
            var screenAutoStopId = setTimeout(function() {
              if (screenRecording && screenRecorder && screenRecorder.state !== "inactive") {
                screenRecorder.stop();
              }
            }, {{.ScreenRecordingDurationCapSeconds}} * 1000);
          } catch (e) {
            stream.getTracks().forEach(function(t) { t.stop(); });
            screenStatusEl.textContent = String(e.message || e);
          }
        }).catch(function(err) {
          // Covers both "no screen to share" denial (NotAllowedError)
          // and any other getDisplayMedia rejection — same status-line
          // surfacing the mic button's own .catch already uses.
          screenBtn.disabled = false;
          screenStatusEl.textContent = String(err.message || err);
        });
      });
    }
  }
})();
</script>
{{end}}

{{define "result"}}
<div class="uc-issue-report-result">
<h2>{{.Heading}}</h2>
<p>{{.Body}} ({{.ID}})</p>
{{if .RecordingNote}}<p class="uc-issue-report-recording-note">{{.RecordingNote}}</p>{{end}}
</div>
{{end}}
`))
