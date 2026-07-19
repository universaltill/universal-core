package csvimport

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// readXLSX reads an .xlsx file's first worksheet into the same
// (headers, rows) shape readCSV produces, so ValidateMapping,
// buildRowData and coerce work unchanged on either format.
//
// Known limitation, deliberately not handled here: a date-typed Excel
// cell is stored as a bare numeric serial (e.g. 45000), with the fact
// that it's a date living in xl/styles.xml's number-format metadata —
// which this reader doesn't parse (styles are out of scope, see below).
// A date column therefore round-trips as that raw serial number, not a
// date string, and — because a FieldDate is validated as a plain string —
// import validation won't catch it; it silently lands wrong. Worth a
// styles.xml pass before this reader is trusted with real date columns;
// tracked in QUEUE.md rather than fixed here.
//
// "First worksheet" means the lowest-numbered xl/worksheets/sheetN.xml
// entry in the zip, not necessarily the leftmost visible tab — Excel
// assigns that internal filename when a sheet is first created and
// doesn't rename it on reorder. Every export this package has been
// exercised against (Excel, Google Sheets, LibreOffice) keeps a
// single-sheet workbook's data in sheet1.xml, which covers the CSV-import
// wizard's use case; multi-sheet workbook selection is out of scope, same
// as it is for CSV (one file, one table).
//
// A blank row — either no <row> element at all for that row number (Excel
// omits fully-empty rows from the XML rather than writing an empty one),
// or a <row> that's present but every one of its cells is empty (e.g. a
// styled-but-contentless trailing row) — is skipped entirely rather than
// reconstructed as an empty data row, the same way encoding/csv skips
// blank lines. RowResult.RowNumber is therefore positional (which
// non-blank row this is), not the worksheet's own row number —
// consistent with how Preview/Commit already number CSV rows.
func readXLSX(r io.Reader) (headers []string, rows [][]string, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read xlsx: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("xlsx is not a valid zip archive: %w", err)
	}

	sharedStrings, err := readSharedStrings(zr)
	if err != nil {
		return nil, nil, err
	}

	sheetFile, err := firstSheetFile(zr)
	if err != nil {
		return nil, nil, err
	}
	ws, err := readWorksheet(sheetFile)
	if err != nil {
		return nil, nil, err
	}
	grid := make([][]string, 0, len(ws.SheetData.Rows))
	for _, row := range ws.SheetData.Rows {
		cells, err := cellValues(row, sharedStrings)
		if err != nil {
			return nil, nil, err
		}
		if isBlankRow(cells) {
			continue
		}
		grid = append(grid, cells)
	}
	if len(grid) == 0 {
		return nil, nil, fmt.Errorf("xlsx has no header row")
	}
	return grid[0], grid[1:], nil
}

func isBlankRow(cells []string) bool {
	for _, v := range cells {
		if v != "" {
			return false
		}
	}
	return true
}

// xlsxWorksheet, xlsxRow and xlsxCell mirror only the subset of
// SpreadsheetML this package reads: sheetData/row/c/v (and c/is/t for
// inline strings). Styling, formulas, merged-cell metadata and everything
// else in a real sheetN.xml is left unparsed by encoding/xml's default
// "ignore unknown elements" behaviour.
type xlsxWorksheet struct {
	SheetData struct {
		Rows []xlsxRow `xml:"row"`
	} `xml:"sheetData"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref  string    `xml:"r,attr"` // e.g. "C7" — column C, row 7
	Type string    `xml:"t,attr"` // "s"=shared string, "inlineStr", "b"=bool, "e"=error, "str"=formula string, ""/"n"=number
	V    string    `xml:"v"`
	Is   *xlsxText `xml:"is"` // present only when Type == "inlineStr"
}

// xlsxText is the shape both a shared-string entry (<si>) and an inline
// string (<is>) share: either a plain <t>, or one or more <r><t>...</t></r>
// rich-text runs (e.g. a cell that's part bold) that must be concatenated
// back into one string — a reader only wants the cell's full text, not
// just its first run.
type xlsxText struct {
	T string    `xml:"t"`
	R []xlsxRun `xml:"r"`
}

type xlsxRun struct {
	T string `xml:"t"`
}

func (x xlsxText) String() string {
	if x.T != "" || len(x.R) == 0 {
		return x.T
	}
	var b strings.Builder
	for _, run := range x.R {
		b.WriteString(run.T)
	}
	return b.String()
}

type xlsxSST struct {
	Items []xlsxText `xml:"si"`
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f, err := zr.Open("xl/sharedStrings.xml")
	if err != nil {
		return nil, nil // no shared strings table: workbook uses only inline strings/numbers
	}
	defer f.Close()

	var sst xlsxSST
	if err := xml.NewDecoder(f).Decode(&sst); err != nil {
		return nil, fmt.Errorf("parse xl/sharedStrings.xml: %w", err)
	}
	out := make([]string, len(sst.Items))
	for i, item := range sst.Items {
		out[i] = item.String()
	}
	return out, nil
}

// firstSheetFile opens the lowest-numbered xl/worksheets/sheetN.xml entry.
func firstSheetFile(zr *zip.Reader) (io.ReadCloser, error) {
	type candidate struct {
		n    int
		file *zip.File
	}
	var candidates []candidate
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "xl/worksheets/")
		if name == f.Name || !strings.HasPrefix(name, "sheet") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "sheet"), ".xml")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{n: n, file: f})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("xlsx has no xl/worksheets/sheetN.xml entry")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].n < candidates[j].n })
	return candidates[0].file.Open()
}

func readWorksheet(f io.ReadCloser) (*xlsxWorksheet, error) {
	defer f.Close()
	var ws xlsxWorksheet
	if err := xml.NewDecoder(f).Decode(&ws); err != nil {
		return nil, fmt.Errorf("parse worksheet xml: %w", err)
	}
	return &ws, nil
}

// cellValues turns one <row>'s cells into a positional []string sized to
// the highest column index present, with gaps (columns Excel omits from
// the XML because they're empty) left as "" — the same "absent, not a
// zero value" treatment buildRowData already gives an empty CSV cell.
//
// A cell's r attribute (e.g. "C7") is optional per OOXML — when absent,
// its column is the previous cell's column plus one (or A if it's the
// row's first cell), never a parse error; every mainstream exporter
// writes r, but a spec-legal file that omits it shouldn't fail the whole
// import.
func cellValues(row xlsxRow, sharedStrings []string) ([]string, error) {
	if len(row.Cells) == 0 {
		return nil, nil
	}
	values := make(map[int]string, len(row.Cells))
	maxCol := -1
	nextCol := 0
	for _, c := range row.Cells {
		col := nextCol
		if c.Ref != "" {
			parsed, err := columnFromRef(c.Ref)
			if err != nil {
				return nil, err
			}
			col = parsed
		}
		nextCol = col + 1

		v, err := cellText(c, sharedStrings)
		if err != nil {
			return nil, err
		}
		values[col] = v
		if col > maxCol {
			maxCol = col
		}
	}
	out := make([]string, maxCol+1)
	for col, v := range values {
		out[col] = v
	}
	return out, nil
}

func cellText(c xlsxCell, sharedStrings []string) (string, error) {
	switch c.Type {
	case "s":
		idx, err := strconv.Atoi(c.V)
		if err != nil {
			return "", fmt.Errorf("cell %s: shared-string index %q is not an integer", c.Ref, c.V)
		}
		if idx < 0 || idx >= len(sharedStrings) {
			return "", fmt.Errorf("cell %s: shared-string index %d out of range (table has %d entries)", c.Ref, idx, len(sharedStrings))
		}
		return sharedStrings[idx], nil
	case "inlineStr":
		if c.Is == nil {
			return "", nil
		}
		return c.Is.String(), nil
	case "b":
		if c.V == "1" {
			return "true", nil
		}
		return "false", nil
	case "e":
		// An Excel error literal (#DIV/0!, #N/A, ...) isn't data — treat it
		// the same as an empty cell rather than importing the error text.
		return "", nil
	default: // "", "n" (number), "str" (formula result string)
		return c.V, nil
	}
}

// columnFromRef parses the column-letter prefix of a cell reference like
// "C7" or "AA12" into a zero-based column index (A=0, B=1, ..., Z=25,
// AA=26, ...) — base-26 with no zero digit, matching spreadsheet column
// naming.
func columnFromRef(ref string) (int, error) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("cell reference %q has no column letters", ref)
	}
	col := 0
	for _, ch := range ref[:i] {
		col = col*26 + int(ch-'A'+1)
	}
	return col - 1, nil
}
