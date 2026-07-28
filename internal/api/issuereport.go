// The in-app issue logger — Farshid's ask: "capture a voice record and
// screen record and the logs... text the voice with ai as well and open
// an issue for us maybe in the github issue." This is the first slice:
// voice capture + transcription (internal/kernel/speechassist, a
// self-hosted Whisper ASR instance — confirmed live end-to-end against
// the real homelab deployment before this was written) plus typed
// context, stored as a real IssueReport record (foundation.go) via the
// same generic entity/crud/audit machinery every other entity in this
// kernel uses.
//
// Deliberately NOT in this slice, each for its own reason:
//   - Screen recording: a materially bigger feature (video capture,
//     encoding, much larger uploads) — voice-only ships a real, useful
//     first version rather than blocking on everything the original ask
//     named. Fast-follow, not forgotten (see QUEUE.md).
//   - Automatic GitHub issue filing: needs a GitHub credential with
//     issue-write access to the target repo, which per this kernel's
//     secret-creation-needs-explicit-authorization discipline has to be
//     provisioned deliberately, not created unilaterally by a session
//     building app code. This entity is the durable store either way —
//     filing to GitHub later is a step that reads from here, not a
//     replacement for storing reports here.
//   - Redaction/consent review before anything leaves the browser: the
//     capture page shows the full description+transcript for editing
//     before Submit is ever clickable, so nothing captured is sent
//     anywhere the human submitting it hasn't already read — but there's
//     no separate confirmation step beyond that yet.
package api

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// maxVoiceNoteBytes bounds an uploaded voice-note recording — generous
// for a few minutes of compressed (webm/opus, what MediaRecorder
// produces by default in every real browser) audio, same "cap it at the
// HTTP boundary" reasoning maxUploadBytes documents for the CSV import
// wizard.
const maxVoiceNoteBytes = 10 << 20 // 10 MiB

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
		TranscribeHref:    "/issue-report/transcribe",
		SubmitHref:        "/issue-report/submit",
		TitleLabel:        h.catalog.T(locale, "issue_report.title_label"),
		DescriptionLabel:  h.catalog.T(locale, "issue_report.description_label"),
		RecordLabel:       h.catalog.T(locale, "issue_report.record_button"),
		StopLabel:         h.catalog.T(locale, "issue_report.stop_button"),
		TranscribingLabel: h.catalog.T(locale, "issue_report.transcribing"),
		TranscriptLabel:   h.catalog.T(locale, "issue_report.transcript_label"),
		SubmitLabel:       h.catalog.T(locale, "issue_report.submit_button"),
		NoMicLabel:        h.catalog.T(locale, "issue_report.no_mic"),
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
	if err := r.ParseMultipartForm(maxVoiceNoteBytes); err != nil {
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
	ts, err := h.scope(r.Context(), rc.TenantID)
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

	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid form submission: "+err.Error())
		return
	}
	// An empty form value is NOT set into fields at all — same "empty
	// means absent, not a zero value" discipline csvimport.buildRowData's
	// own doc comment establishes for exactly this reason: entity.
	// ValidateRecord only treats an absent key (or a nil value) as
	// missing for a Required field, not a present-but-empty string, so
	// setting fields["title"] = "" unconditionally would let a blank
	// title silently pass validation instead of being rejected. status
	// is the one field set outside this loop — always "new" for a fresh
	// submission, never sourced from the form.
	fields := map[string]any{"status": "new"}
	for name, value := range map[string]string{
		"title":       r.PostForm.Get("title"),
		"description": r.PostForm.Get("description"),
		"transcript":  r.PostForm.Get("transcript"),
		"page_url":    r.PostForm.Get("page_url"),
		"user_agent":  r.Header.Get("User-Agent"),
	} {
		if value != "" {
			fields[name] = value
		}
	}

	if err := entity.ValidateRecord(def, fields); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := ts.crud.Create(r.Context(), def, fields, rc.Actor)
	if err != nil {
		writeInternalError(w, "create issue report", err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := issueReportTmpl.ExecuteTemplate(w, "result", issueReportResultView{
		Heading: h.catalog.T(locale, "issue_report.result_heading"),
		Body:    h.catalog.T(locale, "issue_report.result_body"),
		ID:      rec.ID,
	}); err != nil {
		writeInternalError(w, "render issue report result", err)
	}
}

// --- view models ---

type issueReportPageView struct {
	TranscribeHref    string
	SubmitHref        string
	TitleLabel        string
	DescriptionLabel  string
	RecordLabel       string
	StopLabel         string
	TranscribingLabel string
	TranscriptLabel   string
	SubmitLabel       string
	NoMicLabel        string
}

type issueReportResultView struct {
	Heading string
	Body    string
	ID      string
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
<form id="uc-issue-report-form" class="uc-form" method="post" action="{{.SubmitHref}}">
<label for="uc-issue-title">{{.TitleLabel}}</label>
<input type="text" id="uc-issue-title" name="title" required>

<label for="uc-issue-description">{{.DescriptionLabel}}</label>
<textarea id="uc-issue-description" name="description" rows="6" required></textarea>

<div class="uc-issue-voice">
<button type="button" id="uc-issue-record-btn">{{.RecordLabel}}</button>
<span id="uc-issue-record-status"></span>
</div>

<label for="uc-issue-transcript">{{.TranscriptLabel}}</label>
<textarea id="uc-issue-transcript" name="transcript" rows="4" readonly></textarea>

<input type="hidden" name="page_url" id="uc-issue-page-url">
<button type="submit">{{.SubmitLabel}}</button>
</form>
<div id="uc-issue-report-result"></div>
</div>
<script>
(function() {
  document.getElementById("uc-issue-page-url").value = window.location.href;

  var recordBtn = document.getElementById("uc-issue-record-btn");
  var statusEl = document.getElementById("uc-issue-record-status");
  var transcriptEl = document.getElementById("uc-issue-transcript");
  var descriptionEl = document.getElementById("uc-issue-description");
  var mediaRecorder = null;
  var chunks = [];
  var recording = false;

  if (!navigator.mediaDevices || !window.MediaRecorder) {
    recordBtn.disabled = true;
    statusEl.textContent = {{.NoMicLabel}};
    return;
  }

  // extractErrorMessage reads httpx's own {"data":null,"error":"..."}
  // envelope (internal/httpx/envelope.go) out of a failed response body
  // and returns just the human message — the raw envelope text used to
  // be dumped verbatim into statusEl on any failure (e.g. a disabled
  // speechassist.Client's 503), showing a user the whole raw JSON
  // envelope instead of a readable sentence. Falls back to the raw text
  // only if it isn't the JSON shape this endpoint actually returns
  // (never expected in practice, but safer than throwing here).
  function extractErrorMessage(rawText) {
    try {
      var body = JSON.parse(rawText);
      if (body && typeof body.error === "string" && body.error !== "") {
        return body.error;
      }
    } catch (e) {
      // Not JSON — fall through to the raw text.
    }
    return rawText;
  }

  recordBtn.addEventListener("click", function() {
    if (!recording) {
      navigator.mediaDevices.getUserMedia({ audio: true }).then(function(stream) {
        chunks = [];
        mediaRecorder = new MediaRecorder(stream);
        mediaRecorder.ondataavailable = function(e) { if (e.data.size > 0) chunks.push(e.data); };
        mediaRecorder.onstop = function() {
          stream.getTracks().forEach(function(t) { t.stop(); });
          var blob = new Blob(chunks, { type: "audio/webm" });
          statusEl.textContent = {{.TranscribingLabel}};
          var form = new FormData();
          form.append("audio", blob, "note.webm");
          fetch({{.TranscribeHref}}, { method: "POST", body: form })
            .then(function(resp) {
              if (!resp.ok) { return resp.text().then(function(t) { throw new Error(extractErrorMessage(t)); }); }
              return resp.text();
            })
            .then(function(text) {
              transcriptEl.value = text;
              if (descriptionEl.value.trim() !== "") {
                descriptionEl.value = descriptionEl.value.replace(/\s+$/, "") + "\n\n" + text;
              } else {
                descriptionEl.value = text;
              }
              statusEl.textContent = "";
            })
            .catch(function(err) {
              statusEl.textContent = String(err.message || err);
            });
        };
        mediaRecorder.start();
        recording = true;
        recordBtn.textContent = {{.StopLabel}};
        statusEl.textContent = "";
      }).catch(function(err) {
        statusEl.textContent = String(err.message || err);
      });
    } else {
      mediaRecorder.stop();
      recording = false;
      recordBtn.textContent = {{.RecordLabel}};
    }
  });
})();
</script>
{{end}}

{{define "result"}}
<div class="uc-issue-report-result">
<h2>{{.Heading}}</h2>
<p>{{.Body}} ({{.ID}})</p>
</div>
{{end}}
`))
