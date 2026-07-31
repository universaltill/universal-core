package csvimport

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// TestCoerce_I18nText covers CSV/form coercion of the i18n_text field type
// (ADR-0009): a JSON object string parses to a map, an empty cell is an
// empty object (absent), and malformed JSON is a clear error rather than a
// silent bad value.
func TestCoerce_I18nText(t *testing.T) {
	got, err := Coerce(entity.FieldI18nText, `{"en":"Each","tr":"Adet"}`)
	if err != nil {
		t.Fatalf("valid JSON object: %v", err)
	}
	want := map[string]any{"en": "Each", "tr": "Adet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}

	empty, err := Coerce(entity.FieldI18nText, "   ")
	if err != nil {
		t.Fatalf("blank cell: %v", err)
	}
	if m, ok := empty.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("blank cell should coerce to an empty object, got %#v", empty)
	}

	if _, err := Coerce(entity.FieldI18nText, `not json`); err == nil {
		t.Fatal("expected malformed i18n_text JSON to error")
	}
	if _, err := Coerce(entity.FieldI18nText, `["array","not","object"]`); err == nil {
		t.Fatal("expected a JSON array (not an object) to error for i18n_text")
	}
}

// TestExportCSV_I18nTextRoundTrips proves an i18n_text value exports as the
// exact JSON object string Coerce parses back — so an export/re-import
// leaves the multilingual value intact — and that an empty object exports
// as a blank cell (absent), consistent with every other absent field.
func TestExportCSV_I18nTextRoundTrips(t *testing.T) {
	def := &entity.Definition{
		EntityType: "Unit",
		Version:    1,
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString},
			{Name: "label", Type: entity.FieldI18nText},
		},
	}
	records := []map[string]any{
		{"code": "ea", "label": map[string]any{"en": "Each", "tr": "Adet"}},
		{"code": "kg"}, // no label at all -> blank cell
	}
	var buf bytes.Buffer
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	out := buf.String()

	// The exported label cell must be the JSON object, and it must parse
	// back through Coerce to the same map.
	// (Both keys present; JSON key order isn't guaranteed, so assert via
	// re-coercion rather than a substring of the whole object.)
	var exportedLabelCell string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "ea,") {
			exportedLabelCell = strings.TrimPrefix(line, "ea,")
			break
		}
	}
	if exportedLabelCell == "" {
		t.Fatalf("did not find the 'ea' row's label cell in export:\n%s", out)
	}
	// CSV-quotes around a value containing commas are stripped by any real
	// reader; strip the surrounding quotes and un-double any internal ones.
	unquoted := exportedLabelCell
	if strings.HasPrefix(unquoted, `"`) && strings.HasSuffix(unquoted, `"`) {
		unquoted = strings.ReplaceAll(unquoted[1:len(unquoted)-1], `""`, `"`)
	}
	back, err := Coerce(entity.FieldI18nText, unquoted)
	if err != nil {
		t.Fatalf("re-coerce exported label %q: %v", unquoted, err)
	}
	if !reflect.DeepEqual(back, map[string]any{"en": "Each", "tr": "Adet"}) {
		t.Fatalf("round-trip changed the value: %#v (from cell %q)", back, exportedLabelCell)
	}

	// The 'kg' row has no label -> the label cell is blank.
	if !strings.Contains(out, "kg,\n") && !strings.HasSuffix(strings.TrimSpace(out), "kg,") {
		t.Fatalf("expected an empty label cell for the label-less row, got:\n%s", out)
	}
}
