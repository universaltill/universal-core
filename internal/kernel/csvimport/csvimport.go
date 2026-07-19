// Package csvimport is the CSV import wizard's engine (BACKLOG.md R1:
// "CSV/XLSX everywhere — import wizards with mapping + validation
// preview"), the first piece of the connector spike (ADR-0001's rollout
// §1 item 4). Given an entity.Definition, an explicit column mapping, and
// a CSV file, it validates every row before writing anything (Preview)
// and writes only the rows that pass (Commit) — a bad row is reported and
// skipped, it never blocks the rest of the batch. Like every generic
// engine in this kernel, behaviour comes only from the Definition and
// mapping passed in, never a per-entity-type branch (CLAUDE.md).
//
// Column-mapping UI/suggestion (an HTMX form, or an AI-assisted "guess
// the mapping from header names" step per BACKLOG.md's R4 sync-engine
// vision) is out of scope here — this package's contract is an explicit
// mapping in, a per-row result out. NAV 2009 connectivity (SQL views —
// NAV 2009 has no OData) is a separate, larger piece: it needs a real
// NAV 2009 schema to map against, which this kernel spike doesn't have
// access to yet. CSV is the connector spike's first, self-contained slice.
package csvimport

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ColumnMapping maps a CSV header name to the entity field it fills.
type ColumnMapping map[string]string

// RowResult is the outcome of importing (or previewing) one CSV data row.
// RowNumber is 1-based and excludes the header row, so RowNumber 1 is the
// first row of data — matching how a human reading the CSV file counts.
type RowResult struct {
	RowNumber int
	Data      map[string]any
	Err       error
	// RecordID is set only after Commit successfully writes the row; empty
	// for Preview results and for rows Commit skipped due to Err.
	RecordID string
}

// ValidateMapping checks the mapping itself before touching any row: every
// mapping target must be a real field on def, every mapping source must be
// a real CSV header, and every Required field must have something mapped
// to it. Without this upfront check, a broken mapping (a typo'd column
// name, or simply forgetting to map a required field) would make every
// single row fail with the same error — a mapping problem, not a
// per-row data problem, and worth surfacing as one clear error instead of
// drowning it in N identical row failures.
func ValidateMapping(def *entity.Definition, headers []string, mapping ColumnMapping) error {
	headerSet := make(map[string]bool, len(headers))
	for _, h := range headers {
		headerSet[h] = true
	}
	// mappedFrom tracks which CSV column already claimed each target field,
	// so two different columns mapped to the same field is caught here —
	// map iteration order in Go is randomized per range, so without this
	// check buildRowData's `data[fieldName] = v` would silently let
	// whichever column happens to be visited last win, nondeterministically
	// differing row to row within the same import.
	mappedFrom := make(map[string]string, len(mapping))
	for csvHeader, fieldName := range mapping {
		if !headerSet[csvHeader] {
			return fmt.Errorf("mapping references column %q, which isn't in the CSV header row", csvHeader)
		}
		if _, ok := def.FieldByName(fieldName); !ok {
			return fmt.Errorf("mapping targets field %q, which doesn't exist on entity %q", fieldName, def.EntityType)
		}
		if other, ok := mappedFrom[fieldName]; ok {
			return fmt.Errorf("field %q is mapped from both column %q and column %q — a field can only have one source column", fieldName, other, csvHeader)
		}
		mappedFrom[fieldName] = csvHeader
	}
	for _, f := range def.Fields {
		if f.Required && mappedFrom[f.Name] == "" {
			return fmt.Errorf("required field %q has no column mapped to it", f.Name)
		}
	}
	return nil
}

// Preview parses r as CSV (first row is headers) and validates every data
// row against def via mapping, without writing anything — the
// "validation preview" step BACKLOG.md's import-wizard requirement calls
// for. Every row is reported, valid or not, so a human reviews the whole
// batch before anything commits.
func Preview(r io.Reader, def *entity.Definition, mapping ColumnMapping) ([]RowResult, error) {
	headers, rows, err := readCSV(r)
	if err != nil {
		return nil, err
	}
	if err := ValidateMapping(def, headers, mapping); err != nil {
		return nil, err
	}

	results := make([]RowResult, 0, len(rows))
	for i, row := range rows {
		data, err := buildRowData(headers, row, def, mapping)
		if err == nil {
			err = entity.ValidateRecord(def, data)
		}
		results = append(results, RowResult{RowNumber: i + 1, Data: data, Err: err})
	}
	return results, nil
}

// Commit previews the same way Preview does, then writes every row that
// passed validation via engine — atomically per row (crud.Engine.Create
// already writes the record and its audit entry together, one
// transaction per row). A row that fails validation is never written;
// its RowResult carries the validation error instead of a RecordID and
// doesn't block the rows around it, so a batch of 500 rows with 3 bad
// ones still imports the other 497.
func Commit(ctx context.Context, r io.Reader, def *entity.Definition, mapping ColumnMapping, engine *crud.Engine, tenantID string, actor audit.Actor) ([]RowResult, error) {
	results, err := Preview(r, def, mapping)
	if err != nil {
		return nil, err
	}
	for i, res := range results {
		if res.Err != nil {
			continue
		}
		rec, err := engine.Create(ctx, def, tenantID, res.Data, actor)
		if err != nil {
			results[i].Err = err
			continue
		}
		results[i].RecordID = rec.ID
	}
	return results, nil
}

func readCSV(r io.Reader) (headers []string, rows [][]string, err error) {
	cr := csv.NewReader(r)
	// Ragged rows are reported per-row (a short/long row fails that row's
	// validation) rather than aborting the whole file on one bad line.
	cr.FieldsPerRecord = -1
	all, err := cr.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("csv has no header row")
	}
	// Excel (and other spreadsheet tools) commonly prefix a "CSV UTF-8"
	// export with a byte-order mark; encoding/csv doesn't strip it, so
	// left alone it silently lands inside the first header's name and
	// breaks every mapping targeting that column (the mapping's header
	// string would need the invisible BOM bytes to match, which no caller
	// would ever think to include).
	all[0][0] = strings.TrimPrefix(all[0][0], "\uFEFF")
	return all[0], all[1:], nil
}

func buildRowData(headers, row []string, def *entity.Definition, mapping ColumnMapping) (map[string]any, error) {
	data := make(map[string]any, len(mapping))
	for csvHeader, fieldName := range mapping {
		idx := columnIndex(headers, csvHeader) // present: guaranteed by ValidateMapping
		if idx >= len(row) {
			continue // short row: leave the field absent, Required (if any) catches it
		}
		raw := strings.TrimSpace(row[idx])
		if raw == "" {
			continue // empty cell means absent, not a zero value
		}
		ef, _ := def.FieldByName(fieldName) // present: guaranteed by ValidateMapping
		v, err := coerce(ef.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", fieldName, err)
		}
		data[fieldName] = v
	}
	return data, nil
}

func columnIndex(headers []string, name string) int {
	for i, h := range headers {
		if h == name {
			return i
		}
	}
	return -1
}

// coerce converts a raw CSV cell — always a string — into the Go type
// entity.ValidateRecord expects for t (see entity/validate.go:
// FieldNumber needs float64/int/int64, FieldBool needs an actual bool;
// everything else is validated as a plain string already). Without this,
// every non-string field would fail validation on any CSV import, since a
// CSV cell is never anything but text.
func coerce(t entity.FieldType, raw string) (any, error) {
	switch t {
	case entity.FieldNumber:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", raw)
		}
		return n, nil
	case entity.FieldBool:
		// strconv.ParseBool's accepted set: 1/t/T/TRUE/true/True and
		// 0/f/F/FALSE/false/False. Accepting bare "1"/"0" is a known
		// footgun for a mis-mapped CSV column — a numeric-looking column
		// mapped to a bool field by mistake coerces silently instead of
		// erroring — but it's also how spreadsheet exports commonly
		// represent booleans, so this stays permissive rather than
		// inventing a narrower accepted set; get the mapping right.
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a bool (use true/false/1/0/t/f)", raw)
		}
		return b, nil
	default: // FieldString, FieldDate, FieldReference, FieldEnum
		return raw, nil
	}
}
