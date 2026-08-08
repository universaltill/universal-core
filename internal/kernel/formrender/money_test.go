package formrender

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	uclocale "github.com/universaltill/universal-core/internal/kernel/locale"
)

// quoteLineEntity/quoteLineForm are a minimal single-money-field worked
// example — RequestForQuotationQuoteLine's own shape (uc-infra#68),
// without coupling this generic package's tests to the purchasing
// module.
func quoteLineEntity() *entity.Definition {
	return &entity.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Fields: []entity.Field{
			{Name: "unit_price", Type: entity.FieldMoney, Required: true},
		},
	}
}

func quoteLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Quote",
			Component: form.ComponentFields,
			Fields:    []form.FormField{{Name: "unit_price", Label: "Unit Price"}},
		}},
	}
}

// TestRender_MoneyFieldShowsMajorUnitDecimal is the actual bug proof:
// the record carries the stored minor-units value (1050), but the
// rendered input's value attribute must be the human major-unit decimal
// ("10.50"), never the raw integer a naive FormatFieldValue would show.
func TestRender_MoneyFieldShowsMajorUnitDecimal(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"unit_price": float64(1050)}}
	var buf strings.Builder
	if err := r.Render(&buf, quoteLineForm(), quoteLineEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `value="10.50"`) {
		t.Fatalf("expected the input value to show the major-unit decimal 10.50, got:\n%s", got)
	}
	if strings.Contains(got, `value="1050"`) {
		t.Fatalf("expected the raw minor-units integer NOT to leak into the input value, got:\n%s", got)
	}
	if !strings.Contains(got, `type="number" step="0.01"`) {
		t.Fatalf("expected a step=0.01 number input for a money field, got:\n%s", got)
	}
}

// TestRender_MoneyFieldNegativeAmount confirms the same holds for a
// negative stored amount (a credit/adjustment line).
func TestRender_MoneyFieldNegativeAmount(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"unit_price": float64(-340)}}
	var buf strings.Builder
	if err := r.Render(&buf, quoteLineForm(), quoteLineEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `value="-3.40"`) {
		t.Fatalf("expected value=\"-3.40\", got:\n%s", got)
	}
}

// TestRender_HiddenMoneyFieldPreservesMinorUnitsNotMajorUnits is the
// regression test for the independent review's finding on uc-infra#68:
// a money field declared on the entity but NOT shown on the form (a
// VisibleIf-hidden field, or a form Definition that simply omits it)
// must still round-trip its stored MINOR-units value correctly through
// the hidden input. Before this fix, buildHiddenFields used the generic
// FormatFieldValue, which emitted the raw minor-units integer (e.g.
// "1050") into the hidden input's value attribute — internal/api's
// parseRecordFields then feeds that back through csvimport.Coerce's
// FieldMoney case, which treats hidden-input text as a human-typed
// MAJOR-unit decimal, turning 1050 into 105000 on every single save.
func TestRender_HiddenMoneyFieldPreservesMinorUnitsNotMajorUnits(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString},
			{Name: "unit_price", Type: entity.FieldMoney},
		},
	}
	// Deliberately only shows "vendor_id" — unit_price is a real entity
	// field this form was never built to display (or is VisibleIf-hidden
	// — the mechanism is identical either way).
	def := &form.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Sections: []form.Section{{
			Title: "Quote", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}},
		}},
	}
	data := Data{Record: map[string]any{"vendor_id": "v1", "unit_price": float64(1050)}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<input type="hidden" name="unit_price" value="10.50">`) {
		t.Fatalf("expected the hidden money field to carry the major-unit decimal 10.50, got:\n%s", body)
	}
	if strings.Contains(body, `name="unit_price" value="1050"`) {
		t.Fatalf("expected the raw minor-units integer NOT to leak into the hidden input, got:\n%s", body)
	}
}

// TestRender_MasterDetailChildMoneyCellShowsMajorUnitDecimal is the
// regression test for the independent review's finding on uc-infra#68:
// childCellValue had no FieldMoney case, so a money field shown in a
// master_detail/related_list child grid rendered the raw stored
// minor-units integer verbatim instead of the major-unit decimal every
// other money surface (the plain field input, hidden-field
// preservation, the list-page column) already shows.
func TestRender_MasterDetailChildMoneyCellShowsMajorUnitDecimal(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "POLine", Version: 1,
		Fields: []entity.Field{
			{Name: "unit_price", Type: entity.FieldMoney},
			{Name: "parent_id", Type: entity.FieldReference, Target: "PurchaseOrder"},
		},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"unit_price": float64(1050)}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `data-field="unit_price">10.50<`) {
		t.Fatalf("expected the child cell to show the major-unit decimal 10.50, got:\n%s", out)
	}
	if strings.Contains(out, `data-field="unit_price">1050<`) {
		t.Fatalf("expected the raw minor-units integer NOT to leak into the child cell, got:\n%s", out)
	}
}

// TestRender_MasterDetailMoneyChildCellIsRegionallyFormatted (uc-infra#166):
// childCellValue's FieldMoney case now renders through the viewer's
// regional number format (grouping/decimal separator) — matching
// cellText's own FieldMoney case on the top-level list page
// (internal/api/listview.go) and this package's own sibling
// FieldDate/FieldNumber fix (uc-infra#133,
// TestRender_MasterDetailDateAndNumberCellsAreRegionallyFormatted). Uses
// the same worked value (123456789 minor units == $1,234,567.89) as
// internal/api's TestListPage_MoneyFieldDisplaysMajorUnitDecimal, so a
// mismatch here would mean the two surfaces actually disagree, not just
// that this test's own math is wrong.
func TestRender_MasterDetailMoneyChildCellIsRegionallyFormatted(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "POLine", Version: 1,
		Fields: []entity.Field{
			{Name: "unit_price", Type: entity.FieldMoney},
			{Name: "parent_id", Type: entity.FieldReference, Target: "PurchaseOrder"},
		},
	}
	baseData := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"unit_price": float64(123456789)}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}

	for name, tc := range map[string]struct {
		regionTag string
		wantAmt   string
	}{
		"american": {"en-US", "1,234,567.89"},
		"turkish":  {"tr-TR", "1.234.567,89"},
	} {
		t.Run(name, func(t *testing.T) {
			loc, err := uclocale.Parse(tc.regionTag)
			if err != nil {
				t.Fatalf("uclocale.Parse(%q): %v", tc.regionTag, err)
			}
			data := baseData
			data.RegionalLocale = loc
			var buf strings.Builder
			if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
				t.Fatalf("render: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, `data-field="unit_price">`+tc.wantAmt+`<`) {
				t.Errorf("expected the regionally formatted amount %q in the unit_price cell, got:\n%s", tc.wantAmt, out)
			}
			if strings.Contains(out, `data-field="unit_price">1234567.89<`) {
				t.Errorf("ungrouped amount leaked into a %s-locale child cell, got:\n%s", name, out)
			}
			if strings.Contains(out, `data-field="unit_price">123456789<`) {
				t.Errorf("raw minor-units integer leaked into a %s-locale child cell, got:\n%s", name, out)
			}
		})
	}
}

// TestRender_MoneyFieldUnsetOnCreateFormRendersEmpty confirms a create
// form (no stored value yet) renders an empty input rather than "0.00" —
// so a Required money field's browser-native validation still fires on
// an untouched field, the same reasoning FieldEnum/FieldReference's own
// "genuinely unset" handling documents.
func TestRender_MoneyFieldUnsetOnCreateFormRendersEmpty(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, quoteLineForm(), quoteLineEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `id="unit_price" name="unit_price" value=""`) {
		t.Fatalf("expected an empty value for an unset required money field, got:\n%s", got)
	}
	if !strings.Contains(got, "required") {
		t.Fatalf("expected the required attribute to still be present, got:\n%s", got)
	}
}
