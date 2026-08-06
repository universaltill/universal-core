package csvimport

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// quoteLineDef is a minimal Definition with one FieldMoney field —
// RequestForQuotationQuoteLine's own shape (uc-infra#68), without
// coupling this generic package's tests to the purchasing module.
func quoteLineDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "unit_price", Type: entity.FieldMoney, Required: true},
		},
	}
}

func TestCoerce_Money(t *testing.T) {
	cases := []struct {
		raw     string
		want    float64
		wantErr bool
	}{
		{raw: "10.50", want: 1050},
		{raw: "10", want: 1000},
		{raw: "0", want: 0},
		{raw: "-3.40", want: -340},
		{raw: "abc", wantErr: true},
		{raw: "10.505", wantErr: true},
	}
	for _, c := range cases {
		got, err := Coerce(entity.FieldMoney, c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("Coerce(FieldMoney, %q): expected error, got %v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Coerce(FieldMoney, %q): unexpected error: %v", c.raw, err)
			continue
		}
		n, ok := got.(float64)
		if !ok {
			t.Errorf("Coerce(FieldMoney, %q) returned %T, want float64", c.raw, got)
			continue
		}
		if n != c.want {
			t.Errorf("Coerce(FieldMoney, %q) = %v, want %v", c.raw, n, c.want)
		}
	}
}

// TestPreview_MoneyFieldRoundTrips is Coerce's own contract proven
// through the real Preview path: a human-typed "10.50" CSV cell must
// pass entity.ValidateRecord (which requires a whole number of minor
// units) and end up stored as 1050, not the fractional 10.5 that would
// fail FieldMoney's own validation.
func TestPreview_MoneyFieldRoundTrips(t *testing.T) {
	def := quoteLineDef()
	mapping := ColumnMapping{"Unit Price": "unit_price"}
	csvData := "Unit Price\n10.50\n"

	results, err := Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("expected the row to pass validation, got %v", results[0].Err)
	}
	if got := results[0].Data["unit_price"]; got != float64(1050) {
		t.Fatalf("expected unit_price stored as 1050 minor units, got %+v (%T)", got, got)
	}
}

func TestPreview_MoneyFieldTooManyDecimalsFailsThatRow(t *testing.T) {
	def := quoteLineDef()
	mapping := ColumnMapping{"Unit Price": "unit_price"}
	csvData := "Unit Price\n10.505\n"

	results, err := Preview(strings.NewReader(csvData), def, mapping)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected an error for a money cell with more than 2 decimal places")
	}
}

// TestExportCSV_MoneyFieldExportsMajorUnitDecimal: the stored value is
// minor units (1050), but the exported cell must be the human major-unit
// decimal ("10.50") — the same shape Coerce's own FieldMoney case parses
// back on re-import (TestExportCSV_RoundTripsThroughPreview_Money below).
func TestExportCSV_MoneyFieldExportsMajorUnitDecimal(t *testing.T) {
	def := quoteLineDef()
	records := []map[string]any{
		{"unit_price": float64(1050)},
		{"unit_price": float64(-340)},
	}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "\n10.50\n") {
		t.Fatalf("expected 1050 minor units to export as 10.50, got:\n%s", got)
	}
	if !strings.Contains(got, "\n-3.40\n") {
		t.Fatalf("expected -340 minor units to export as -3.40, got:\n%s", got)
	}
	// Negative money must not be quote-escaped as a formula (same
	// reasoning as FieldNumber's own exclusion).
	if strings.Contains(got, "'-3.40") {
		t.Fatalf("expected negative money to NOT be quote-escaped, got:\n%s", got)
	}
}

// TestExportCSV_MoneyFieldLegacyFractionalValueExportsRawRatherThanBlank
// is the regression test for the independent review's finding on
// uc-infra#68: a value money.FromAny can't coerce (here, a still-
// fractional pre-backfill legacy amount — the real, transient shape a
// row can have between the Version 1->2 publish and
// cmd/backfill-quote-line-unit-price actually running) must still
// export SOMETHING, not silently vanish as a blank cell — a blank cell
// reads as "no price recorded," which would be false, and CSV export's
// whole purpose is moving data out reliably.
func TestExportCSV_MoneyFieldLegacyFractionalValueExportsRawRatherThanBlank(t *testing.T) {
	def := quoteLineDef()
	records := []map[string]any{{"unit_price": 9.5}}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !strings.Contains(buf.String(), "\n9.5\n") {
		t.Fatalf("expected the legacy fractional value to export as 9.5 rather than a blank cell, got:\n%s", buf.String())
	}
}

func TestExportCSV_MoneyFieldAbsentExportsBlank(t *testing.T) {
	def := quoteLineDef()
	records := []map[string]any{{}}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !strings.Contains(buf.String(), "unit_price\n\n") {
		t.Fatalf("expected an absent money field to export as a blank cell, got:\n%s", buf.String())
	}
}

func TestExportCSV_RoundTripsThroughPreview_Money(t *testing.T) {
	def := quoteLineDef()
	records := []map[string]any{{"unit_price": float64(1050)}}

	var buf strings.Builder
	if err := ExportCSV(&buf, def, records); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}

	identityMapping := ColumnMapping{"unit_price": "unit_price"}
	results, err := Preview(strings.NewReader(buf.String()), def, identityMapping)
	if err != nil {
		t.Fatalf("Preview of the exported CSV: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("expected the exported row to re-import cleanly, got %+v", results)
	}
	if got := results[0].Data["unit_price"]; got != float64(1050) {
		t.Fatalf("expected unit_price to round-trip as 1050, got %+v (%T)", got, got)
	}
}
