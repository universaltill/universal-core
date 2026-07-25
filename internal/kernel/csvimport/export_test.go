package csvimport

import (
	"strings"
	"testing"
)

func TestExportCSV_WritesHeaderAndRows(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": "Acme Textiles", "lead_time_days": float64(60), "is_active": true, "rating": "gold"},
		{"name": "Beta Supplies", "lead_time_days": float64(45), "is_active": false, "rating": "silver"},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}

	got := buf.String()
	wantHeader := "name,lead_time_days,is_active,rating\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("expected header row %q, got:\n%s", wantHeader, got)
	}
	if !strings.Contains(got, "Acme Textiles,60,true,gold\n") {
		t.Fatalf("expected the first row rendered in def.Fields order, got:\n%s", got)
	}
	if !strings.Contains(got, "Beta Supplies,45,false,silver\n") {
		t.Fatalf("expected the second row rendered in def.Fields order, got:\n%s", got)
	}
}

// TestExportCSV_AbsentFieldExportsAsBlankCell confirms a field never set
// on a record (not present in its Data map at all — the ordinary shape
// for an optional field nobody filled in) renders as an empty cell, not
// "<nil>" or some other stringified placeholder.
func TestExportCSV_AbsentFieldExportsAsBlankCell(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": "Acme Textiles"}, // lead_time_days, is_active, rating all absent
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !strings.Contains(buf.String(), "Acme Textiles,,,\n") {
		t.Fatalf("expected absent fields to render as blank cells, got:\n%s", buf.String())
	}
}

// TestExportCSV_EscapesFormulaLikeStringValues is escapeFormulaPrefix's
// own end-to-end proof: a tenant-typed FieldString value that would
// otherwise execute as a formula the moment the exported file is opened
// in Excel/Sheets/LibreOffice (CWE-1236) must come out prefixed with a
// literal-text-forcing single quote.
func TestExportCSV_EscapesFormulaLikeStringValues(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": `=HYPERLINK("http://evil.example/","click")`},
		{"name": "+1+1"},
		{"name": "-DANGEROUS()"},
		{"name": "@SUM(A1:A9)"},
		{"name": "a perfectly ordinary name"},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		`'=HYPERLINK`,
		`'+1+1`,
		`'-DANGEROUS()`,
		`'@SUM(A1:A9)`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected formula-like value escaped as %q, got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "a perfectly ordinary name") || strings.Contains(got, "'a perfectly ordinary name") {
		t.Fatalf("expected an ordinary value to pass through unescaped, got:\n%s", got)
	}
}

// TestExportCSV_NegativeNumberIsNotEscapedAsFormula confirms
// FieldNumber is excluded from escapeFormulaPrefix: a genuine negative
// number legitimately starts with '-', and must reach the file as an
// actual number Excel still evaluates as one, not a quote-prefixed
// string.
func TestExportCSV_NegativeNumberIsNotEscapedAsFormula(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": "Acme Textiles", "lead_time_days": float64(-7)},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "'-7") {
		t.Fatalf("expected a negative FieldNumber to NOT be quote-escaped, got:\n%s", got)
	}
	if !strings.Contains(got, ",-7,") && !strings.Contains(got, ",-7\n") {
		t.Fatalf("expected the negative number to appear as a plain -7 cell, got:\n%s", got)
	}
}

// TestExportCSV_LargeAndFractionalFloatsFormatPrecisely locks the
// numeric-formatting contract formatValue rests on: strconv.FormatFloat
// with precision -1 round-trips the exact float64 value, for both a
// large integer-valued number and a genuinely fractional one, not just
// the small whole numbers the other tests happen to use.
func TestExportCSV_LargeAndFractionalFloatsFormatPrecisely(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": "Big", "lead_time_days": float64(123456789)},
		{"name": "Fractional", "lead_time_days": 3.14159},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Big,123456789,") {
		t.Fatalf("expected the large integer-valued float formatted without loss, got:\n%s", got)
	}
	if !strings.Contains(got, "Fractional,3.14159,") {
		t.Fatalf("expected the fractional value formatted precisely, got:\n%s", got)
	}
}

// TestExportCSV_RoundTripsThroughPreview is the core contract this
// function exists to hold: a file it writes must re-import through this
// same package's own Preview (an identity mapping — CSV header equals
// field name, exactly what a re-import of an export naturally is)
// without any data loss or type-coercion drift, proving formatValue is
// genuinely Coerce's inverse, not just superficially similar.
func TestExportCSV_RoundTripsThroughPreview(t *testing.T) {
	def := vendorDef()
	records := []map[string]any{
		{"name": "Acme Textiles", "lead_time_days": float64(60), "is_active": true, "rating": "gold"},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}

	identityMapping := ColumnMapping{}
	for _, f := range def.Fields {
		identityMapping[f.Name] = f.Name
	}
	results, err := Preview(strings.NewReader(buf.String()), def, identityMapping)
	if err != nil {
		t.Fatalf("Preview of the exported CSV: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 previewed row, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("expected the exported row to pass validation on re-import, got %v", results[0].Err)
	}
	got := results[0].Data
	if got["name"] != "Acme Textiles" {
		t.Fatalf("expected name to round-trip, got %+v", got)
	}
	if got["lead_time_days"] != float64(60) {
		t.Fatalf("expected lead_time_days to round-trip as float64(60), got %+v (%T)", got["lead_time_days"], got["lead_time_days"])
	}
	if got["is_active"] != true {
		t.Fatalf("expected is_active to round-trip as true, got %+v", got)
	}
	if got["rating"] != "gold" {
		t.Fatalf("expected rating to round-trip, got %+v", got)
	}
}
