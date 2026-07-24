package csvimport

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// maxSampleRowsForAI caps how many data rows SampleRows/SampleRowsXLSX
// read for an AI mapping suggestion's prompt — enough for a model to see
// real variation in a column's values without the prompt (and the
// model's limited context budget on the small, homelab-scale model this
// platform runs — see aiassist's own doc comment) growing with file
// size.
const maxSampleRowsForAI = 5

// maxSampleValuesPerHeader caps how many of a header's sample values
// actually go into the prompt, independent of how many rows were read —
// a column that repeats the same value across the first few sample rows
// shouldn't pad the prompt with duplicates, and a mostly-blank column
// shouldn't pad it with empty strings.
const maxSampleValuesPerHeader = 3

// SampleRows reads r as CSV and returns its headers plus up to
// maxSampleRowsForAI data rows, in the same raw string-per-cell shape
// Headers/Preview already use — the material an AI mapping suggestion
// needs to see actual column values, not just header text. Unlike
// Preview, this never requires or validates a mapping: it runs before
// one exists.
func SampleRows(r io.Reader) (headers []string, rows [][]string, err error) {
	return sampleRows(r, readCSV)
}

// SampleRowsXLSX is SampleRows for an .xlsx file's first worksheet.
func SampleRowsXLSX(r io.Reader) (headers []string, rows [][]string, err error) {
	return sampleRows(r, readXLSX)
}

func sampleRows(r io.Reader, read reader) (headers []string, rows [][]string, err error) {
	headers, rows, err = read(r)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) > maxSampleRowsForAI {
		rows = rows[:maxSampleRowsForAI]
	}
	return headers, rows, nil
}

// aiMappingSchema is the JSON Schema handed to aiassist.Client.GenerateJSON
// as Ollama's structured-output "format" constraint — proven directly
// against the live homelab-k8s Ollama instance (llama3.2:3b) before this
// was written: an earlier, more ambiguous schema/prompt pairing (generic
// "field" property name, no explicit "don't invent new values"
// instruction) had the model echo the column header back as the field
// name instead of actually mapping it. "database_field" as the property
// name plus the explicit constraint in aiSuggestPrompt's wording fixed
// that — this schema and that prompt shape are a matched pair, don't
// change one without re-verifying against a real model.
var aiMappingSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"mappings": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"column":         map[string]any{"type": "string"},
					"database_field": map[string]any{"type": "string"},
				},
				"required": []string{"column", "database_field"},
			},
		},
	},
	"required": []string{"mappings"},
}

type aiMappingResponse struct {
	Mappings []struct {
		Column        string `json:"column"`
		DatabaseField string `json:"database_field"`
	} `json:"mappings"`
}

// SuggestMappingAI augments existing (typically SuggestMapping's own
// exact normalized-name guess) by asking ai about whatever headers and
// fields existing left unresolved — it never reconsiders a mapping
// existing already made, only fills gaps, so a confident exact-name
// match is never second-guessed by a much weaker, non-deterministic
// model. headers and sampleRows come from SampleRows/SampleRowsXLSX
// (same row order, same column order).
//
// Returns the augmented mapping plus aiSuggested, the subset of that
// mapping's keys (CSV headers) whose value came from the AI rather than
// existing — a caller (internal/api/import.go's mapping-table UI) uses
// this to visibly distinguish an AI guess from a deterministic match,
// since both still land in the exact same editable <select> a human
// reviews before Preview/Commit ever runs (this function itself never
// bypasses ValidateMapping, same as SuggestMapping).
//
// err is advisory only — a disabled client, a network/timeout failure,
// or a response that doesn't parse is never fatal to the caller: this
// always returns a valid, usable mapping (at minimum, existing
// unchanged) alongside a non-nil err a caller may choose to log.
func SuggestMappingAI(ctx context.Context, ai *aiassist.Client, headers []string, sampleRows [][]string, def *entity.Definition, existing ColumnMapping) (mapping ColumnMapping, aiSuggested map[string]bool, err error) {
	mapping = make(ColumnMapping, len(existing))
	maps.Copy(mapping, existing)
	aiSuggested = map[string]bool{}

	if !ai.Enabled() {
		return mapping, aiSuggested, nil
	}

	claimedFields := make(map[string]bool, len(existing))
	for _, fieldName := range existing {
		claimedFields[fieldName] = true
	}
	var unmappedHeaders []string
	for _, h := range headers {
		if _, ok := existing[h]; !ok {
			unmappedHeaders = append(unmappedHeaders, h)
		}
	}
	var availableFields []string
	for _, f := range def.Fields {
		if !claimedFields[f.Name] {
			availableFields = append(availableFields, f.Name)
		}
	}
	if len(unmappedHeaders) == 0 || len(availableFields) == 0 {
		return mapping, aiSuggested, nil
	}

	prompt := buildMappingPrompt(unmappedHeaders, headers, sampleRows, availableFields)
	var resp aiMappingResponse
	if err := ai.GenerateJSON(ctx, prompt, aiMappingSchema, &resp); err != nil {
		return mapping, aiSuggested, fmt.Errorf("csvimport: AI mapping suggestion: %w", err)
	}

	unmappedSet := make(map[string]bool, len(unmappedHeaders))
	for _, h := range unmappedHeaders {
		unmappedSet[h] = true
	}
	availableSet := make(map[string]bool, len(availableFields))
	for _, f := range availableFields {
		availableSet[f] = true
	}

	// Never trust the model's output blindly (same discipline every
	// mapping this kernel accepts is held to, e.g. ValidateMapping
	// itself, run later on whatever the caller ultimately submits): a
	// suggestion is only accepted if it names a header that was
	// genuinely unmapped and a field that was genuinely available and
	// neither has already been claimed by an earlier suggestion in this
	// same response — first-wins for a repeated field (mirroring
	// SuggestMapping's own "claimed" discipline) AND for a repeated
	// column (a model that lists the same header twice must not have
	// its second, possibly different, answer silently override its
	// first) — anything else (a hallucinated field name, a header not
	// in this file, a duplicate) is silently dropped, not an error.
	for _, m := range resp.Mappings {
		if !unmappedSet[m.Column] || !availableSet[m.DatabaseField] {
			continue
		}
		mapping[m.Column] = m.DatabaseField
		aiSuggested[m.Column] = true
		delete(unmappedSet, m.Column)
		delete(availableSet, m.DatabaseField)
	}
	return mapping, aiSuggested, nil
}

func buildMappingPrompt(unmappedHeaders, allHeaders []string, sampleRows [][]string, availableFields []string) string {
	var b strings.Builder
	b.WriteString("You are mapping spreadsheet columns to database field names.\n\n")
	b.WriteString("Spreadsheet columns (with example values from real rows):\n")
	for _, h := range unmappedHeaders {
		samples := columnSamples(h, allHeaders, sampleRows)
		fmt.Fprintf(&b, "Column %q: ", h)
		if len(samples) == 0 {
			b.WriteString("(no example values available)\n")
			continue
		}
		quoted := make([]string, len(samples))
		for i, s := range samples {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&b, "example values are %s\n", strings.Join(quoted, ", "))
	}
	b.WriteString("\nAvailable database field names you must choose from (choose the exact string, do not invent new ones): [")
	quotedFields := make([]string, len(availableFields))
	for i, f := range availableFields {
		quotedFields[i] = fmt.Sprintf("%q", f)
	}
	b.WriteString(strings.Join(quotedFields, ", "))
	b.WriteString("]\n\n")
	b.WriteString("For each spreadsheet column above, if one of the available database fields is clearly the right match, include it in your answer. If no available field is a good match for a column, omit that column entirely rather than guessing.")
	return b.String()
}

// columnSamples collects up to maxSampleValuesPerHeader distinct,
// non-empty values header actually held across sampleRows.
func columnSamples(header string, allHeaders []string, sampleRows [][]string) []string {
	idx := columnIndex(allHeaders, header)
	if idx < 0 {
		return nil
	}
	seen := map[string]bool{}
	var samples []string
	for _, row := range sampleRows {
		if idx >= len(row) {
			continue
		}
		v := strings.TrimSpace(row[idx])
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		samples = append(samples, v)
		if len(samples) >= maxSampleValuesPerHeader {
			break
		}
	}
	return samples
}
