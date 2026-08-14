package formrender

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// TestRender_MasterDetailBoolChildCellIsLocalized (uc-infra#236
// independent review finding F6): before this, childCellValue had no
// entity.FieldBool case, so a composition/related-list child's own
// boolean column fell through to FormatFieldValue and rendered the
// literal, untranslated Go string "true"/"false" in every locale —
// unreadable on an Arabic/Persian/Turkish screen once
// DepreciationSchedule.overridden (uc-infra#236's own new field) made a
// boolean child column common instead of theoretical. Same
// "common.value.true"/"common.value.false" catalog convention
// internal/api/handlers.go's uniqueConstraintMessage already uses for a
// FieldBool WhenValue.
func TestRender_MasterDetailBoolChildCellIsLocalized(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "POLine", Version: 1,
		Fields: []entity.Field{
			{Name: "billable", Type: entity.FieldBool},
			{Name: "parent_id", Type: entity.FieldReference, Target: "PurchaseOrder"},
		},
	}

	for _, tc := range []struct {
		name, locale string
		value        bool
		want         string
	}{
		{"en true", "en", true, "Yes"},
		{"en false", "en", false, "No"},
		{"ar true", "ar", true, "نعم"},
		{"ar false", "ar", false, "لا"},
		{"fa true", "fa", true, "بله"},
		{"fa false", "fa", false, "خیر"},
		{"tr true", "tr", true, "Evet"},
		{"tr false", "tr", false, "Hayır"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := Data{
				Record: map[string]any{"payment_method": "Wire"},
				Children: map[string][]map[string]any{
					"POLine": {{"billable": tc.value}},
				},
				ChildDefs: map[string]*entity.Definition{"POLine": childDef},
			}
			var buf strings.Builder
			if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, tc.locale); err != nil {
				t.Fatalf("render: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, `data-field="billable">`+tc.want+`<`) {
				t.Errorf("expected the localized value %q in the billable cell for locale %q, got:\n%s", tc.want, tc.locale, out)
			}
			rawGoString := "false"
			if tc.value {
				rawGoString = "true"
			}
			if strings.Contains(out, `data-field="billable">`+rawGoString+`<`) {
				t.Errorf("raw untranslated Go bool %q leaked into a %s-locale child cell, got:\n%s", rawGoString, tc.locale, out)
			}
		})
	}
}

// TestRender_MasterDetailBoolChildCellFallsBackForNonBoolValue mirrors
// this package's existing FieldMoney un-coercible-value fallback test
// (money_test.go) for the same reason: a legacy/malformed stored value
// (nil, or a non-bool type from data drift) must render something via
// FormatFieldValue rather than panic on a failed type assertion.
func TestRender_MasterDetailBoolChildCellFallsBackForNonBoolValue(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "POLine", Version: 1,
		Fields: []entity.Field{
			{Name: "billable", Type: entity.FieldBool},
			{Name: "parent_id", Type: entity.FieldReference, Target: "PurchaseOrder"},
		},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"billable": "not-a-bool"}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `data-field="billable">not-a-bool<`) {
		t.Errorf("expected the raw stored value to render as a fallback, got:\n%s", buf.String())
	}
}
