package api

import (
	"bytes"
	"context"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/speechassist"
)

// testHandlerWithSpeech is testHandler plus a speechassist.Client —
// kept separate rather than adding a speech parameter to testHandler
// itself so every other test in this package stays exactly as it was
// (voice transcription disabled, matching a deployment with no
// WHISPER_URL configured), same pattern import_test.go's
// testHandlerWithAI already establishes for aiassist.
func testHandlerWithSpeech(t *testing.T, db *sql.DB, speech *speechassist.Client) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(db, catalog, nil, nil, speech)
}

func publishFoundation(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, db, tenantID, actor); err != nil {
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

func TestIssueReport_NewPage_RendersCaptureForm(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
}

func TestIssueReport_NewPage_RequiresAuth(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux) // no speech client

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

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
	testHandlerWithSpeech(t, db, speech).Routes(mux)

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

// TestIssueReport_Submit_CreatesRecordAndItsQueryable is the core
// end-to-end proof: submitting the form actually creates a real
// IssueReport record (tenant-scoped, audit-tracked, via the exact same
// crud.Engine every other entity in this kernel uses), not a bespoke
// side-channel — and it's genuinely queryable afterward through the
// generic /api/records route, same as anything else.
func TestIssueReport_Submit_CreatesRecordAndItsQueryable(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishFoundation(t, db, tenantID)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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

func TestIssueReport_Submit_MissingRequiredFieldIs400(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishFoundation(t, db, tenantID)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	// title omitted entirely.
	req := newRequest("POST", "/issue-report/submit", tenantID, "farshid", []byte("description=Something+is+wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing required title, got %d: %s", rec.Code, rec.Body.String())
	}
}
