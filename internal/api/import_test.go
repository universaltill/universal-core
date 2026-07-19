package api

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	oversized := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	req := newMultipartRequest(t, "/import/Vendor/preview", tenantID, "farshid", "huge.csv", oversized, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized upload, got %d: %s", rec.Code, rec.Body.String())
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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

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
