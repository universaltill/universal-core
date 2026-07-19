package csvimport

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"hash/crc32"
	"strings"
	"testing"
)

// buildXLSX assembles a minimal .xlsx (a zip archive) from the given
// entries. It deliberately includes only what readXLSX actually reads
// (xl/sharedStrings.xml, xl/worksheets/sheetN.xml) — no
// [Content_Types].xml, workbook.xml or styles.xml — since readXLSX finds
// its worksheet by globbing xl/worksheets/sheetN.xml rather than parsing
// workbook.xml/rels (see xlsx.go's package comment on that tradeoff).
// A real Excel/Sheets/LibreOffice export includes those extra parts too;
// omitting them here keeps the fixture down to exactly what's under test.
func buildXLSX(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

const xlsxNS = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`

func TestPreviewXLSX_SharedStringsAndNumbers(t *testing.T) {
	sst := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst ` + xlsxNS + `>
<si><t>Vendor Name</t></si>
<si><t>Lead Time</t></si>
<si><t>Acme Textiles</t></si>
</sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>60</v></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/sharedStrings.xml":     sst,
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("expected no error, got %v", results[0].Err)
	}
	if results[0].Data["name"] != "Acme Textiles" {
		t.Fatalf("unexpected name: %+v", results[0].Data)
	}
	if results[0].Data["lead_time_days"] != 60.0 {
		t.Fatalf("expected lead_time_days coerced to float64(60), got %v (%T)", results[0].Data["lead_time_days"], results[0].Data["lead_time_days"])
	}
}

func TestPreviewXLSX_InlineStringsAndBooleans(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c><c r="B1" t="inlineStr"><is><t>Active</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>Beta Supplies</t></is></c><c r="B2" t="b"><v>0</v></c></row>
</sheetData>
</worksheet>`
	// No xl/sharedStrings.xml at all: a workbook that never uses shared
	// strings omits the part entirely.
	data := buildXLSX(t, map[string]string{
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Active": "is_active"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected no error, got %v", results[0].Err)
	}
	if results[0].Data["name"] != "Beta Supplies" {
		t.Fatalf("unexpected name: %+v", results[0].Data)
	}
	if results[0].Data["is_active"] != false {
		t.Fatalf("expected is_active coerced to bool(false), got %v (%T)", results[0].Data["is_active"], results[0].Data["is_active"])
	}
}

func TestPreviewXLSX_RichTextRunsConcatenate(t *testing.T) {
	// Excel splits a cell's text into multiple <r><t> runs when it carries
	// mixed formatting (e.g. part bold) within the same cell — a shared
	// string entry must concatenate every run, not just read the first.
	sst := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst ` + xlsxNS + `>
<si><r><t>Vendor Name</t></r></si>
<si><r><t>Acme </t></r><r><t>Textiles</t></r></si>
</sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c></row>
<row r="2"><c r="A2" t="s"><v>1</v></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/sharedStrings.xml":     sst,
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Data["name"] != "Acme Textiles" {
		t.Fatalf("expected rich-text runs concatenated into %q, got %+v", "Acme Textiles", results[0].Data)
	}
}

// TestPreviewXLSX_InlineRichTextRunsConcatenate is the regression test for
// the code-review finding that inline strings (<is>) handled only <is><t>,
// not the <is><r><t>...</t></r> rich-text-run form — asymmetric with
// shared strings, which already concatenated runs. A cell split into runs
// (e.g. part bold) silently lost everything but the shared-string case.
func TestPreviewXLSX_InlineRichTextRunsConcatenate(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><r><t>Acme </t></r><r><t>Textiles</t></r></is></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Data["name"] != "Acme Textiles" {
		t.Fatalf("expected inline rich-text runs concatenated into %q, got %+v", "Acme Textiles", results[0].Data)
	}
}

// TestPreviewXLSX_PresentButEmptyRowSkipped is the regression test for the
// code-review finding that a <row> element which IS present in the XML
// but whose cells are all empty (e.g. a styled trailing row Excel wrote
// with no content) was becoming a phantom data row that failed required-
// field validation — contradicting this package's documented promise to
// skip blank rows the same way encoding/csv skips blank lines. Only a
// row-number *gap* (no <row> element at all) was actually being skipped.
func TestPreviewXLSX_PresentButEmptyRowSkipped(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>Acme</t></is></c></row>
<row r="3"><c r="A3" s="5"/></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the present-but-empty row 3 to be skipped, got %d rows: %+v", len(results), results)
	}
}

// TestPreviewXLSX_MissingRefAttributeInfersColumnPositionally is the
// regression test for the nit that a cell with no r attribute (legal per
// OOXML — r is optional, with column inferred from document order) used
// to fail the whole import instead of falling back to positional
// inference.
func TestPreviewXLSX_MissingRefAttributeInfersColumnPositionally(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c t="inlineStr"><is><t>Vendor Name</t></is></c><c t="inlineStr"><is><t>Rating</t></is></c></row>
<row r="2"><c t="inlineStr"><is><t>Acme</t></is></c><c t="inlineStr"><is><t>gold</t></is></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Rating": "rating"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected no error, got %v", results[0].Err)
	}
	if results[0].Data["name"] != "Acme" || results[0].Data["rating"] != "gold" {
		t.Fatalf("expected cells without an r attribute to be positioned in document order, got %+v", results[0].Data)
	}
}

// TestPreviewXLSX_ErrorCellImportsAsEmpty is the regression test for the
// nit that an Excel error literal (#DIV/0!, #N/A, ...) was importing
// verbatim as string data instead of being treated like an empty cell.
func TestPreviewXLSX_ErrorCellImportsAsEmpty(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c><c r="B1" t="inlineStr"><is><t>Rating</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>Acme</t></is></c><c r="B2" t="e"><v>#DIV/0!</v></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Rating": "rating"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if _, present := results[0].Data["rating"]; present {
		t.Fatalf("expected an Excel error-literal cell to import as absent, got %v", results[0].Data["rating"])
	}
}

func TestPreviewXLSX_SparseColumnsAlignByPosition(t *testing.T) {
	// Excel omits a <c> element entirely for an empty cell rather than
	// writing an empty one, so column B here is simply missing from the
	// XML — the reader must still place "Lead Time" and its value at
	// index 1 (not shift "Rating" left into that slot).
	sst := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst ` + xlsxNS + `>
<si><t>Vendor Name</t></si>
<si><t>Lead Time</t></si>
<si><t>Rating</t></si>
<si><t>Acme</t></si>
<si><t>gold</t></si>
</sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>
<row r="2"><c r="A2" t="s"><v>3</v></c><c r="C2" t="s"><v>4</v></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/sharedStrings.xml":     sst,
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days", "Rating": "rating"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected no error, got %v", results[0].Err)
	}
	if _, present := results[0].Data["lead_time_days"]; present {
		t.Fatalf("expected the omitted Lead Time cell to be absent, got %v", results[0].Data["lead_time_days"])
	}
	if results[0].Data["rating"] != "gold" {
		t.Fatalf("expected Rating (column C) to land correctly despite the gap at column B, got %+v", results[0].Data)
	}
}

func TestPreviewXLSX_BlankRowSkippedLikeCSVBlankLine(t *testing.T) {
	// Row 3 has no <row> element at all — Excel drops fully-empty rows
	// from the XML. Row numbering should stay positional (2 data rows
	// reported, not 3 with a phantom empty one), matching how
	// encoding/csv already treats a blank CSV line.
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>Acme</t></is></c></row>
<row r="4"><c r="A4" t="inlineStr"><is><t>Beta</t></is></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows (blank row 3 skipped), got %d", len(results))
	}
	if results[0].RowNumber != 1 || results[1].RowNumber != 2 {
		t.Fatalf("expected positional row numbers 1,2; got %d,%d", results[0].RowNumber, results[1].RowNumber)
	}
	if results[1].Data["name"] != "Beta" {
		t.Fatalf("unexpected row 2 data: %+v", results[1].Data)
	}
}

func TestPreviewXLSX_ShortRowLeavesTrailingFieldsAbsent(t *testing.T) {
	sst := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst ` + xlsxNS + `>
<si><t>Vendor Name</t></si>
<si><t>Lead Time</t></si>
<si><t>Acme</t></si>
</sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `>
<sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c></row>
</sheetData>
</worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/sharedStrings.xml":     sst,
		"xl/worksheets/sheet1.xml": sheet,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name", "Lead Time": "lead_time_days"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected short row to validate fine, got %v", results[0].Err)
	}
	if _, present := results[0].Data["lead_time_days"]; present {
		t.Fatalf("expected lead_time_days to be absent for a short row, got %v", results[0].Data["lead_time_days"])
	}
}

func TestPreviewXLSX_MultiDigitColumnAndSheetNumbers(t *testing.T) {
	// Column reference parsing must handle multi-letter columns (AA, AB,
	// ...) correctly, and sheet-file selection must sort sheetN.xml
	// numerically (sheet2.xml before sheet10.xml), not lexically.
	// Lexical sort would put "sheet10.xml" before "sheet2.xml" (the
	// character '1' < '2'), so the real data goes in sheet2.xml — the
	// numerically-lowest sheet — to prove firstSheetFile sorts by the
	// parsed integer, not the string.
	sheet2 := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData>
<row r="1"><c r="AA1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
<row r="2"><c r="AA2" t="inlineStr"><is><t>Acme</t></is></c></row>
</sheetData></worksheet>`
	sheet10 := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>should not be picked</t></is></c></row>
</sheetData></worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/worksheets/sheet10.xml": sheet10,
		"xl/worksheets/sheet2.xml":  sheet2,
	})

	results, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "name"})
	if err != nil {
		t.Fatalf("PreviewXLSX: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("expected no error, got %v", results[0].Err)
	}
	if results[0].Data["name"] != "Acme" {
		t.Fatalf("expected sheet2.xml (lower number) to be picked over sheet10.xml, got %+v", results[0].Data)
	}
}

func TestPreviewXLSX_EmptySheetHasNoHeaderRow(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData></sheetData></worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	_, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected error for an xlsx with no header row")
	}
}

func TestPreviewXLSX_NotAZipFileIsAnError(t *testing.T) {
	_, err := PreviewXLSX(strings.NewReader("this is not a zip archive"), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected error for a file that isn't a valid zip/xlsx archive")
	}
}

func TestPreviewXLSX_MissingWorksheetIsAnError(t *testing.T) {
	data := buildXLSX(t, map[string]string{"xl/sharedStrings.xml": `<sst ` + xlsxNS + `></sst>`})
	_, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected error for a zip with no xl/worksheets/sheetN.xml entry")
	}
}

func TestPreviewXLSX_BadSharedStringIndexIsAnError(t *testing.T) {
	sst := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><sst ` + xlsxNS + `><si><t>Vendor Name</t></si></sst>`
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData>
<row r="1"><c r="A1" t="s"><v>5</v></c></row>
</sheetData></worksheet>`
	data := buildXLSX(t, map[string]string{
		"xl/sharedStrings.xml":     sst,
		"xl/worksheets/sheet1.xml": sheet,
	})
	_, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected error for a shared-string index out of range")
	}
}

// TestValidateMapping_ReusedUnchangedForXLSX confirms PreviewXLSX runs the
// exact same ValidateMapping upfront check Preview (CSV) does — a broken
// mapping should still surface as one error, not per-row noise.
func TestValidateMapping_ReusedUnchangedForXLSX(t *testing.T) {
	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
<row r="2"><c r="A2" t="inlineStr"><is><t>Acme</t></is></c></row>
</sheetData></worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})

	_, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), ColumnMapping{"Vendor Name": "not_a_real_field"})
	if err == nil {
		t.Fatal("expected PreviewXLSX to fail fast on a broken mapping, same as Preview does for CSV")
	}
}

// withXLSXLimits temporarily shrinks the package-level size caps so tests
// can exercise them without materializing 100+ MiB fixtures.
func withXLSXLimits(t *testing.T, fileLimit, entryLimit int64) {
	t.Helper()
	prevFile, prevEntry := maxXLSXFileSize, maxXLSXEntrySize
	maxXLSXFileSize, maxXLSXEntrySize = fileLimit, entryLimit
	t.Cleanup(func() { maxXLSXFileSize, maxXLSXEntrySize = prevFile, prevEntry })
}

// TestPreviewXLSX_UploadOverFileSizeLimitIsRejected is the regression test
// for the code-review finding that io.ReadAll(r) buffered an .xlsx upload
// with no cap at all — a large file would be read fully into memory
// before this package ever got a chance to reject it.
func TestPreviewXLSX_UploadOverFileSizeLimitIsRejected(t *testing.T) {
	withXLSXLimits(t, 100, 500<<20)

	sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet ` + xlsxNS + `><sheetData>
<row r="1"><c r="A1" t="inlineStr"><is><t>Vendor Name</t></is></c></row>
</sheetData></worksheet>`
	data := buildXLSX(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})
	if int64(len(data)) <= 100 {
		t.Fatalf("test fixture (%d bytes) must exceed the 100-byte limit under test", len(data))
	}

	_, err := PreviewXLSX(bytes.NewReader(data), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected an upload over maxXLSXFileSize to be rejected")
	}
}

// TestPreviewXLSX_EntryDeclaringHugeUncompressedSizeIsRejected is the
// regression test for openZipEntry's upfront UncompressedSize64 check: a
// zip entry that honestly declares a decompressed size over the cap is
// rejected without decompressing anything.
func TestPreviewXLSX_EntryDeclaringHugeUncompressedSizeIsRejected(t *testing.T) {
	withXLSXLimits(t, 100<<20, 1<<20) // 1 MiB entry cap

	// A real (non-bomb) 2 MiB payload: highly compressible content still
	// declares its true uncompressed size in the zip central directory,
	// which is what openZipEntry checks before ever calling Open.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("a"), 2<<20)); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	_, err = PreviewXLSX(bytes.NewReader(buf.Bytes()), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected a zip entry declaring an uncompressed size over the cap to be rejected")
	}
}

// TestPreviewXLSX_DecompressionBombIsBoundedDuringRead is the regression
// test for boundedReader's Read-time enforcement specifically — not just
// openZipEntry's upfront UncompressedSize64 check. A crafted zip entry's
// header can LIE about its uncompressed size (declaring something small,
// within the cap) while its actual deflate stream expands far past it;
// the upfront check trusts the header and would wave this through, so
// the real defense against a decompression bomb is bounding the Read
// itself, which is what this test isolates by building a raw entry whose
// declared size and actual decompressed size disagree.
func TestPreviewXLSX_DecompressionBombIsBoundedDuringRead(t *testing.T) {
	withXLSXLimits(t, 100<<20, 1<<20) // 1 MiB entry cap

	var compressed bytes.Buffer
	fw, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatalf("new flate writer: %v", err)
	}
	bomb := bytes.Repeat([]byte("a"), 10<<20) // 10 MiB decompressed, over the 1 MiB cap
	if _, err := fw.Write(bomb); err != nil {
		t.Fatalf("compress bomb payload: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("close flate writer: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fh := &zip.FileHeader{
		Name:               "xl/worksheets/sheet1.xml",
		Method:             zip.Deflate,
		CompressedSize64:   uint64(compressed.Len()),
		UncompressedSize64: 4, // lie: declares 4 bytes, well within the cap
		CRC32:              crc32.ChecksumIEEE([]byte("fake")),
	}
	w, err := zw.CreateRaw(fh)
	if err != nil {
		t.Fatalf("create raw entry: %v", err)
	}
	if _, err := w.Write(compressed.Bytes()); err != nil {
		t.Fatalf("write raw entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	_, err = PreviewXLSX(bytes.NewReader(buf.Bytes()), vendorDef(), vendorMapping())
	if err == nil {
		t.Fatal("expected decompression to be bounded during Read even when the zip header lies about uncompressed size")
	}
}
