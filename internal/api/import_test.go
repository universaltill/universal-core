package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// newMultipartRequest builds a POST request with a multipart/form-data
// body: a "file" field (filename + content) plus any extra string
// fields (e.g. "mapping.<header>" values for the commit step).
func newMultipartRequest(t *testing.T, target, tenantID, actorID, filename string, fileContent []byte, extraFields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileContent); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	for k, v := range extraFields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
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

func TestImport_UploadPage_RendersForm(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/import/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-entity-type="Vendor"`) {
		t.Fatalf("expected the upload page to reference the Vendor entity type, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `type="file"`) {
		t.Fatalf("expected a file input, got:\n%s", rec.Body.String())
	}
}

func TestImport_UploadPage_UnknownEntityTypeIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/import/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestImport_Preview_SuggestsMappingAndShowsRows is the core end-to-end
// proof: upload a CSV whose headers exactly match Vendor's field names,
// confirm SuggestMapping's guess is pre-selected in the rendered
// mapping <select>s, and that the preview table shows the row's data.
func TestImport_Preview_SuggestsMappingAndShowsRows(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	csvContent := []byte("name\nAcme Textiles\n")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="mapping.name"`) {
		t.Fatalf("expected a mapping select for the 'name' header, got:\n%s", body)
	}
	if !strings.Contains(body, `value="name" selected`) {
		t.Fatalf("expected SuggestMapping's guess (name->name) to be pre-selected, got:\n%s", body)
	}
	if !strings.Contains(body, "Acme Textiles") {
		t.Fatalf("expected the preview to show the uploaded row's data, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/import/Vendor/commit"`) {
		t.Fatalf("expected a Commit button targeting the commit endpoint, got:\n%s", body)
	}
}

// TestImport_Preview_IncompleteMappingShowsEditorNotHardError is the
// regression test for a real bug found by manually driving the wizard
// against a synthetic CSV (see import.go's importPreview doc comment):
// a header that doesn't exactly name-match a required field ("Vendor
// Name" vs. the "name" field) used to make the whole preview request
// fail with a raw JSON 400 and never show the mapping table at all —
// there was no way to fix the mapping through the wizard itself. It must
// instead render the mapping table (so the column can be mapped
// manually) with the validation error surfaced inline, and no rows or
// Commit button until the mapping is actually complete.
func TestImport_Preview_IncompleteMappingShowsEditorNotHardError(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	csvContent := []byte("Vendor Name\nAcme Textiles\n")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (mapping editor, not a hard error), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="mapping.Vendor Name"`) {
		t.Fatalf("expected a mapping select for the 'Vendor Name' header so it can be fixed, got:\n%s", body)
	}
	if !strings.Contains(body, `required field &#34;name&#34; has no column mapped to it`) {
		t.Fatalf("expected the mapping validation error surfaced inline, got:\n%s", body)
	}
	if strings.Contains(body, `hx-post="/import/Vendor/commit"`) {
		t.Fatalf("expected no Commit button while the mapping is incomplete, got:\n%s", body)
	}

	// Re-submit with the column manually mapped, same as a user picking
	// it from the <select> and clicking "Preview again".
	req2 := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent,
		map[string]string{"mapping.Vendor Name": "name"})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "Acme Textiles") {
		t.Fatalf("expected the completed mapping to show the row's data, got:\n%s", body2)
	}
	if !strings.Contains(body2, `hx-post="/import/Vendor/commit"`) {
		t.Fatalf("expected a Commit button once the mapping is complete, got:\n%s", body2)
	}
}

func TestImport_Preview_InvalidRowShowsError(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// "name" is required; the row has an empty value for it. A second
	// column with real content keeps this from being a fully blank
	// line, which encoding/csv (and Headers/Preview, which share its
	// reader) skip entirely rather than reporting as a row — a
	// single-column CSV can't express "one empty required field" at all,
	// since that's indistinguishable from a blank line to skip.
	csvContent := []byte("name,note\n,some note\n")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (preview reports row errors, doesn't fail the request), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `uc-row-error`) {
		t.Fatalf("expected the invalid row to be marked as an error row, got:\n%s", rec.Body.String())
	}
}

// TestImport_Commit_WritesRowsAndReportsResult drives the full two-step
// flow: preview (to get the suggested mapping), then commit with that
// same mapping submitted as form fields — exactly what the rendered
// <select>s would submit — and confirms the record actually landed via
// a real GET /api/records/Vendor afterward, not just that Commit
// reported success.
func TestImport_Commit_WritesRowsAndReportsResult(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	csvContent := []byte("name\nAcme Textiles\nBeta Supplies\n")
	commitReq := newMultipartRequest(t, "/import/Vendor/commit", tenantID, "farshid", "vendors.csv", csvContent,
		map[string]string{"mapping.name": "name"})
	commitRec := httptest.NewRecorder()
	mux.ServeHTTP(commitRec, commitReq)

	if commitRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	if !strings.Contains(commitRec.Body.String(), "2") {
		t.Fatalf("expected the result to report 2 succeeded, got:\n%s", commitRec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Vendor", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), "Acme Textiles") || !strings.Contains(listRec.Body.String(), "Beta Supplies") {
		t.Fatalf("expected both committed rows to actually be queryable afterward, got:\n%s", listRec.Body.String())
	}
}

func TestImport_Preview_NoFileIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "", nil, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a request with no file, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestImport_Preview_OversizedUploadIs400 is the regression test for the
// upload-size cap: csvimport's CSV path has no internal size limit of
// its own (unlike XLSX), so this HTTP-layer cap is the only thing
// bounding it once an upload endpoint exists.
func TestImport_Preview_OversizedUploadIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	oversized := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "huge.csv", oversized, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized upload, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestImport_ReadUploadedFile_LargeFileSpillsToDiskNotHeap is a
// regression test for uc-infra#171 (see attachment_test.go's identical
// test for the full explanation of why this matters): ParseMultipartForm's
// second argument bounds how much of the request multipart.Reader buffers
// in the process heap before spilling the rest to a temp file — it is
// NOT a size cap (http.MaxBytesReader, via maxUploadBytes, is the actual
// ceiling). Passing the full maxUploadBytes cap as that argument (the
// pre-fix behavior) meant an upload anywhere near the cap was retained
// entirely in memory instead of spilling.
//
// This calls the real readUploadedFile function directly (not a
// reimplementation of its parsing logic) with an upload comfortably
// over multipartParseMemory but well under maxUploadBytes, then inspects
// the same *http.Request's MultipartForm afterward — readUploadedFile
// takes no auth/context dependency, so the request pointer the test
// holds is exactly the one ParseMultipartForm populated in production
// code, no copy involved. Asserts the resulting file part is backed by
// *os.File — i.e. actually spilled to a temp file — which fails against
// the pre-fix code (ParseMultipartForm(maxUploadBytes) keeps a file
// this size entirely in memory) and passes after.
func TestImport_ReadUploadedFile_LargeFileSpillsToDiskNotHeap(t *testing.T) {
	// 1 MiB over the in-memory threshold, ~41 MiB under the hard cap —
	// big enough to prove spilling happened, nowhere near maxUploadBytes.
	content := bytes.Repeat([]byte("y"), multipartParseMemory+(1<<20))
	req := newMultipartRequest(t, "/import/Vendor/preview", "", "", "big.csv", content, nil)
	rec := httptest.NewRecorder()

	if _, _, ok := readUploadedFile(rec, req); !ok {
		t.Fatalf("readUploadedFile failed: %s", rec.Body.String())
	}
	if req.MultipartForm == nil {
		t.Fatal("expected req.MultipartForm to be populated after readUploadedFile ran")
	}
	defer func() {
		if err := req.MultipartForm.RemoveAll(); err != nil {
			t.Fatalf("cleanup temp files: %v", err)
		}
	}()

	fhs := req.MultipartForm.File["file"]
	if len(fhs) != 1 {
		t.Fatalf("expected exactly 1 file part, got %d", len(fhs))
	}
	f, err := fhs[0].Open()
	if err != nil {
		t.Fatalf("Open uploaded file part: %v", err)
	}
	defer f.Close()

	if _, ok := f.(*os.File); !ok {
		// See attachment_test.go's identical assertion for the caveat:
		// this only holds because this request has exactly one file part
		// (FileHeader.Open() only returns a bare *os.File when nothing
		// shares its temp file — two-or-more spilled parts would return
		// the same wrapper type an in-memory part does).
		t.Fatalf("file part of %d bytes (> multipartParseMemory=%d) was not spilled to a temp file at the real readUploadedFile call site — got %T, want *os.File; it is being buffered entirely in the process heap", len(content), multipartParseMemory, f)
	}
}

// TestImport_ReadUploadedFile_LargeFile_RoundTrips confirms the smaller
// multipartParseMemory (uc-infra#171) doesn't change observable behavior
// for a caller: readUploadedFile still returns the exact uploaded bytes
// for a file well over that threshold, still under maxUploadBytes, even
// though the multipart part itself now spills to a temp file during
// parsing rather than staying in memory.
func TestImport_ReadUploadedFile_LargeFile_RoundTrips(t *testing.T) {
	content := bytes.Repeat([]byte("y"), multipartParseMemory+(1<<20))
	req := newMultipartRequest(t, "/import/Vendor/preview", "", "", "big.csv", content, nil)
	rec := httptest.NewRecorder()

	got, xlsx, ok := readUploadedFile(rec, req)
	if !ok {
		t.Fatalf("readUploadedFile failed: %s", rec.Body.String())
	}
	// readUploadedFile spills the multipart part to disk (see the spill
	// test above) but never removes the temp file itself — production
	// relies on Go's own net/http server doing that automatically once a
	// real request finishes (net/http/server.go's finishRequest); this
	// direct call bypasses that, so the test cleans up by hand. An
	// earlier version of this test didn't, and independent review caught
	// the resulting leaked ~9 MiB temp file per run.
	if req.MultipartForm != nil {
		t.Cleanup(func() {
			if err := req.MultipartForm.RemoveAll(); err != nil {
				t.Fatalf("cleanup temp files: %v", err)
			}
		})
	}
	if xlsx {
		t.Fatal("expected xlsx=false for a .csv filename")
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("round-tripped content mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
}

// buildTestXLSX assembles a minimal .xlsx with a single "name" column
// header and one data row, matching csvimport's own xlsx_test.go
// fixture-building convention (only the parts this reader actually
// reads — no [Content_Types].xml/workbook.xml, see that file's comment
// on why).
func buildTestXLSX(t *testing.T, value string) []byte {
	t.Helper()
	const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + ns + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>name</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>` + value + `</t></is></c></row>
</sheetData>
</worksheet>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("create xlsx entry: %v", err)
	}
	if _, err := w.Write([]byte(sheet)); err != nil {
		t.Fatalf("write xlsx entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close xlsx: %v", err)
	}
	return buf.Bytes()
}

func TestImport_Preview_XLSXFile(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	xlsxContent := buildTestXLSX(t, "Gamma Traders")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.xlsx", xlsxContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Gamma Traders") {
		t.Fatalf("expected the .xlsx upload to be read (format detected by extension), got:\n%s", rec.Body.String())
	}
}

func TestImport_Commit_XLSXFile(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	xlsxContent := buildTestXLSX(t, "Delta Holdings")
	req := newMultipartRequest(t, "/import/Vendor/commit", tenantID, "farshid", "vendors.xlsx", xlsxContent,
		map[string]string{"mapping.name": "name"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Vendor", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), "Delta Holdings") {
		t.Fatalf("expected the .xlsx-committed row to actually be queryable afterward, got:\n%s", listRec.Body.String())
	}
}

// --- AI-assisted mapping suggestion ---

// orderEntityDef/orderFormDef deliberately use field names ("vendor_id",
// "order_date") that don't normalize-name-match plausible real-world CSV
// headers ("Vendor Nm", "Order Dt") — exactly the gap SuggestMapping's
// exact match can't close and csvimport.SuggestMappingAI exists for.
func orderEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Order",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
			{Name: "order_date", Type: entity.FieldDate, Required: true},
		},
	}
}

func orderFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Order",
		Version:    1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}, {Name: "order_date", Label: "Order Date"}},
		}},
	}
}

// testHandlerWithAI is testHandler plus an aiassist.Client — kept
// separate rather than adding an ai parameter to testHandler itself so
// every other test in this package stays exactly as it was (AI
// disabled, matching a deployment with no OLLAMA_URL configured).
func testHandlerWithAI(t *testing.T, router *tenantdb.Router, ai *aiassist.Client) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(router, catalog, nil, nil, ai, nil, nil)
}

// fakeAIServer stands in for Ollama's /api/generate, always returning
// mappings — the same wire shape aiassist.Client.GenerateJSON expects
// (see aiassist's and csvimport's own tests for the contract this
// mirrors). called is set true the first time the server actually
// receives a request, so a test can assert the AI path was (or, more
// importantly, was NOT) exercised at all.
func fakeAIServer(t *testing.T, mappings []map[string]string, called *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if called != nil {
			*called = true
		}
		respJSON, err := json.Marshal(map[string]any{"mappings": mappings})
		if err != nil {
			t.Fatalf("marshal fake mappings: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": string(respJSON), "done": true})
	}))
}

// TestImport_Preview_AIFillsGapsAndMarksSuggestedRows is the API-layer
// end-to-end proof, mirroring what was manually verified against a real
// self-hosted Ollama instance: a CSV whose headers don't exact-match
// any field gets AI-suggested selections pre-filled, each visibly
// flagged in the rendered HTML — still just the same editable <select>
// every mapping row already had.
func TestImport_Preview_AIFillsGapsAndMarksSuggestedRows(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, orderEntityDef(), orderFormDef())

	var called bool
	srv := fakeAIServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "vendor_id"},
		{"column": "Order Dt", "database_field": "order_date"},
	}, &called)
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mux := http.NewServeMux()
	testHandlerWithAI(t, router, ai).Routes(mux)

	csvContent := []byte("Vendor Nm,Order Dt\nAcme Textiles,2026-07-01\n")
	req := newMultipartRequest(t, "/import/Order/preview", tenantID, "farshid", "orders.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("expected the AI server to actually be called for headers SuggestMapping couldn't resolve")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="vendor_id" selected`) {
		t.Fatalf("expected Vendor Nm -> vendor_id pre-selected, got:\n%s", body)
	}
	if !strings.Contains(body, `value="order_date" selected`) {
		t.Fatalf("expected Order Dt -> order_date pre-selected, got:\n%s", body)
	}
	if strings.Count(body, "uc-ai-suggested") != 2 {
		t.Fatalf("expected exactly 2 AI-suggested badges (one per AI-filled row), got:\n%s", body)
	}
}

// TestImport_Preview_UsesTenantBYOKProviderInsteadOfPlatformDefault is
// aiProviderFor's own end-to-end proof (import.go): once a tenant has
// saved an AIProviderConnection override (aiprovidersettings.go), the
// import wizard's mapping assist must call THAT server, not the
// platform's own default h.ai — two separate fake Ollama-shaped servers
// stand in for each, so this fails loud (wrong server called, or the
// right one never is) if the resolver ever regresses to always using
// h.ai regardless of what a tenant configured.
func TestImport_Preview_UsesTenantBYOKProviderInsteadOfPlatformDefault(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, orderEntityDef(), orderFormDef())

	var platformCalled, tenantCalled bool
	platformSrv := fakeAIServer(t, nil, &platformCalled)
	defer platformSrv.Close()
	tenantSrv := fakeAIServer(t, []map[string]string{
		{"column": "Vendor Nm", "database_field": "vendor_id"},
		{"column": "Order Dt", "database_field": "order_date"},
	}, &tenantCalled)
	defer tenantSrv.Close()

	platformAI := aiassist.NewClient(platformSrv.URL, "llama3.2:3b")
	mux := http.NewServeMux()
	testHandlerWithAI(t, router, platformAI).Routes(mux)

	saveForm := "provider=ollama&base_url=" + tenantSrv.URL + "&model=llama3.2:3b"
	saveReq := newRequest("POST", "/settings/ai-provider", tenantID, "farshid", []byte(saveForm))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveRec := httptest.NewRecorder()
	mux.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected 200 saving the tenant's AI provider override, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	csvContent := []byte("Vendor Nm,Order Dt\nAcme Textiles,2026-07-01\n")
	req := newMultipartRequest(t, "/import/Order/preview", tenantID, "farshid", "orders.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if platformCalled {
		t.Fatal("expected the platform default AI server to NOT be called once a tenant override is configured")
	}
	if !tenantCalled {
		t.Fatal("expected the tenant's own configured Ollama server to be called")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="vendor_id" selected`) {
		t.Fatalf("expected Vendor Nm -> vendor_id pre-selected from the tenant's own AI, got:\n%s", body)
	}
}

// TestImport_Preview_ResubmissionNeverCallsAI confirms the design
// contract previewUpload's own doc comment describes: once a human has
// mapping.* fields to resubmit (they've started editing the table),
// their choices are the only source of truth for that mapping — an AI
// call overwriting a mid-review edit would be exactly the silent
// surprise the wizard's whole confirm-before-write design exists to
// prevent.
func TestImport_Preview_ResubmissionNeverCallsAI(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, orderEntityDef(), orderFormDef())

	var called bool
	srv := fakeAIServer(t, nil, &called)
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mux := http.NewServeMux()
	testHandlerWithAI(t, router, ai).Routes(mux)

	csvContent := []byte("Vendor Nm,Order Dt\nAcme Textiles,2026-07-01\n")
	req := newMultipartRequest(t, "/import/Order/preview", tenantID, "farshid", "orders.csv", csvContent,
		map[string]string{"mapping.Vendor Nm": "vendor_id", "mapping.Order Dt": "order_date"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("expected the AI server to never be called on a resubmission with an explicit mapping already present")
	}
}

// TestImport_Preview_AIServerErrorFallsBackGracefully confirms an AI
// failure (network/server error) never surfaces to the user as a
// broken request — the wizard just falls back to whatever
// SuggestMapping's exact-match pass alone produced, per
// SuggestMappingAI's own "err is advisory only" contract.
func TestImport_Preview_AIServerErrorFallsBackGracefully(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ai := aiassist.NewClient(srv.URL, "llama3.2:3b")

	mux := http.NewServeMux()
	testHandlerWithAI(t, router, ai).Routes(mux)

	// "name" exact-matches Vendor's own field, so this must still work
	// via SuggestMapping alone even though the AI call for it will fail.
	csvContent := []byte("name\nAcme Textiles\n")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite the AI server failing, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `value="name" selected`) {
		t.Fatalf("expected SuggestMapping's own exact match to still apply, got:\n%s", rec.Body.String())
	}
}

// TestImport_Preview_AIDisabledShowsNoBadges is the regression
// safeguard for every other test in this file (all built with
// testHandler, which always passes a nil ai) — no OLLAMA_URL configured
// must render byte-for-byte the same mapping UI this wizard always had,
// no stray "AI-suggested" copy appearing when there's no AI involved.
func TestImport_Preview_AIDisabledShowsNoBadges(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	csvContent := []byte("name\nAcme Textiles\n")
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "vendors.csv", csvContent, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "uc-ai-suggested") {
		t.Fatalf("expected no AI-suggested badge with no AI client configured, got:\n%s", rec.Body.String())
	}
}

// TestDecryptStoredAPIKey exercises every branch directly (no HTTP): no
// encrypted value present, encryption disabled (no SECRET_ENCRYPTION_KEY
// configured — the common case in most deployments today), a genuine
// round-trip, and a value that fails to decrypt (wrong key/corrupted
// ciphertext) — decryptStoredAPIKey's own doc comment is explicit that
// none of these should ever stop the request, only fall back silently.
func TestDecryptStoredAPIKey(t *testing.T) {
	router := newTestRouter(t)
	cryptor := testCryptor(t)
	h := testHandlerWithSecretCryptor(t, router, cryptor)

	if _, ok := h.decryptStoredAPIKey(map[string]any{}); ok {
		t.Fatal("expected ok=false when there's no api_key_encrypted field at all")
	}

	disabled := testHandler(t, router) // no cryptor — mirrors New() with SECRET_ENCRYPTION_KEY unset
	encrypted, err := cryptor.Encrypt("sk-real-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, ok := disabled.decryptStoredAPIKey(map[string]any{"api_key_encrypted": encrypted}); ok {
		t.Fatal("expected ok=false when this deployment has no secret encryption configured")
	}

	key, ok := h.decryptStoredAPIKey(map[string]any{"api_key_encrypted": encrypted})
	if !ok {
		t.Fatal("expected ok=true for a genuinely encrypted value")
	}
	if key != "sk-real-secret" {
		t.Fatalf("expected the decrypted key back, got %q", key)
	}

	if _, ok := h.decryptStoredAPIKey(map[string]any{"api_key_encrypted": "not-valid-ciphertext"}); ok {
		t.Fatal("expected ok=false for a value that fails to decrypt")
	}
}
