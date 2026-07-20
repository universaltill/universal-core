package formrender

import (
	"encoding/json"
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// purchaseOrderEntity/purchaseOrderForm are the same worked example as
// internal/kernel/form's TestDefinitionValidate_ValidMasterDetailForm,
// extended with a related_list section and a navigate action so the
// renderer exercises every component and op kind in one form.
func purchaseOrderEntity() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
			{Name: "order_date", Type: entity.FieldDate},
			{Name: "payment_method", Type: entity.FieldEnum, EnumValues: []string{"Wire", "LC"}},
			{Name: "lc_reference", Type: entity.FieldString},
			{Name: "is_urgent", Type: entity.FieldBool},
			{Name: "total", Type: entity.FieldNumber},
		},
	}
}

func purchaseOrderForm() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "vendor_id", Label: "Vendor"},
					{Name: "order_date", Label: "Order Date"},
					{Name: "payment_method", Label: "Payment Method"},
					{Name: "lc_reference", Label: "LC Reference", VisibleIf: "payment_method == 'LC'"},
					{Name: "is_urgent", Label: "Urgent"},
					{Name: "total", Label: "Total"},
				},
			},
			{
				Title:        "Lines",
				Component:    form.ComponentMasterDetail,
				Target:       "POLine",
				RollUp:       "line_total",
				RollUpTarget: "total",
			},
			{
				Title:     "Past Orders",
				Component: form.ComponentRelatedList,
				Target:    "PurchaseOrder",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
			{Label: "Submit for Approval", Op: form.OpWorkflowStart, Workflow: "po_approval"},
			{Label: "Print", Op: form.OpReportRender, Report: "po_print"},
			{Label: "Back", Op: form.OpNavigate, Route: "/purchase-orders"},
		},
	}
}

// TestRender_ReferenceFieldRendersAsTextInput pins down current behaviour
// for entity.FieldReference: buildFields' type switch only special-cases
// FieldBool/FieldEnum, so a reference field (e.g. a vendor picker) falls
// into the generic text-input branch rather than a picker widget. Fine as
// a spike default — this test exists so a future picker-widget change is
// a deliberate decision against a known baseline, not an unnoticed drift.
// TestRender_ReferenceFieldRendersAsSelectWithLabels is the regression
// test for a real usability gap: a FieldReference field used to render
// as a plain text input, meaning the only way to fill it in was typing
// a target record's raw id (a UUID) from memory — every reference
// field in the whole kernel (PurchaseOrder.vendor_id, POLine.item_id,
// Item.base_uom_id, ...) was effectively unusable for real data entry.
// Now it's a <select> populated from Data.ReferenceOptions (built by
// internal/api's loadReferenceOptions, which this package has no
// access to itself — see ReferenceOption's own doc comment), showing
// each option's human label, not its id, with the record's current
// value pre-selected.
func TestRender_ReferenceFieldRendersAsSelectWithLabels(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}},
		}},
	}
	data := Data{
		Record: map[string]any{"vendor_id": "vendor-42"},
		ReferenceOptions: map[string][]ReferenceOption{
			"vendor_id": {
				{ID: "vendor-1", Label: "Acme Textiles"},
				{ID: "vendor-42", Label: "Beta Supplies"},
			},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<select id="vendor_id" name="vendor_id" required>`) {
		t.Fatalf("expected reference field to render as a required select, got:\n%s", body)
	}
	if !strings.Contains(body, `<option value="vendor-1" >Acme Textiles</option>`) {
		t.Fatalf("expected an unselected option with a human label, not a raw id, got:\n%s", body)
	}
	if !strings.Contains(body, `<option value="vendor-42" selected>Beta Supplies</option>`) {
		t.Fatalf("expected the current value's option to be selected, got:\n%s", body)
	}
}

// TestRender_OptionalReferenceFieldGetsEmptyOption confirms an
// optional (not required) reference field always offers a real "leave
// unset" choice — without this, a browser's own <select> default
// (whichever option renders first) would look selected on an untouched
// new-record form even though nothing was actually chosen, and
// submitting it would silently write that first option's id.
func TestRender_UnsetOptionalReferenceFieldGetsEmptyOption(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields:     []entity.Field{{Name: "base_uom_id", Type: entity.FieldReference, Target: "UnitOfMeasure"}},
	}
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "base_uom_id", Label: "Unit"}},
		}},
	}
	data := Data{
		Record: map[string]any{},
		ReferenceOptions: map[string][]ReferenceOption{
			"base_uom_id": {{ID: "uom-1", Label: "Each"}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<option value="" selected></option>`) {
		t.Fatalf("expected a selected empty option on an unset optional reference field, got:\n%s", body)
	}
}

// TestRender_UnsetRequiredReferenceFieldGetsEmptyOptionToo is the
// regression test for a real gap independent review found once
// reference fields actually became usable dropdowns (previously masked
// by the field being an unusable text input): a *required* reference
// field with no current value used to have no selectable empty state
// at all, so the browser's own <select> default (whichever option
// happened to render first) counted as a value present — `required`
// never actually blocked submitting an unmade choice. Same fix now
// applies uniformly regardless of Required: the empty option only
// disappears once a real value exists.
func TestRender_UnsetRequiredReferenceFieldGetsEmptyOptionToo(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}},
		}},
	}
	data := Data{
		Record: map[string]any{},
		ReferenceOptions: map[string][]ReferenceOption{
			"vendor_id": {{ID: "vendor-1", Label: "Acme Textiles"}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<select id="vendor_id" name="vendor_id" required>`+"\n"+`<option value="" selected></option>`) {
		t.Fatalf("expected a required select to still start with a selected empty option when unset, got:\n%s", body)
	}
}

// TestRender_UnsetRequiredEnumFieldGetsEmptyOption is FieldEnum's
// sibling of the reference-field fix above — the identical gap existed
// there first (found pre-existing during the reference-field review),
// now fixed for both field types the same way.
func TestRender_UnsetRequiredEnumFieldGetsEmptyOption(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Party",
		Fields: []entity.Field{
			{Name: "party_type", Type: entity.FieldEnum, Required: true, EnumValues: []string{"person", "organization"}},
		},
	}
	def := &form.Definition{
		EntityType: "Party",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "party_type", Label: "Type"}},
		}},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<select id="party_type" name="party_type" required>`+"\n"+`<option value="" selected></option>`) {
		t.Fatalf("expected a required enum select to start with a selected empty option when unset, got:\n%s", body)
	}
}

// TestRender_SetEnumFieldHasNoEmptyOption confirms the fix doesn't
// regress the ordinary case: once a field has a real current value, no
// spurious empty option appears alongside it.
func TestRender_SetEnumFieldHasNoEmptyOption(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Party",
		Fields: []entity.Field{
			{Name: "party_type", Type: entity.FieldEnum, Required: true, EnumValues: []string{"person", "organization"}},
		},
	}
	def := &form.Definition{
		EntityType: "Party",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "party_type", Label: "Type"}},
		}},
	}
	data := Data{Record: map[string]any{"party_type": "organization"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), `<option value="" selected>`) {
		t.Fatalf("expected no empty option once a real value is set, got:\n%s", buf.String())
	}
}

// TestRender_UnsetEnumFieldHonorsDeclaredDefault is the regression test
// for a real e2e failure this same fix caused and then fixed: forcing
// an empty "please choose" option onto every unset enum field broke
// TestFormSaveButton_RealBrowser, because Item.item_type declares
// Default: "stock" but nothing ever consulted entity.Field.Default —
// the old "browser auto-selects whichever option renders first"
// behavior only ever honored it by coincidence (EnumValues[0] happened
// to match). Now Default is actually read: an unset field with a
// declared Default pre-selects it (no empty option shown), and only a
// genuinely undefaulted unset field gets the forced empty choice.
func TestRender_UnsetEnumFieldHonorsDeclaredDefault(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields: []entity.Field{
			{Name: "item_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"stock", "service", "non_stock"}, Default: "stock"},
		},
	}
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "item_type", Label: "Type"}},
		}},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, `<option value="" selected>`) {
		t.Fatalf("expected no empty option when a Default is declared, got:\n%s", body)
	}
	if !strings.Contains(body, `<option value="stock" selected>Stock</option>`) {
		t.Fatalf("expected the declared Default (\"stock\") pre-selected, got:\n%s", body)
	}
}

// TestRender_EnumOptionLabelsAreTranslated confirms an enum field's
// options show a translated label ("field.{EntityType}.{FieldName}.
// {Value}" in the i18n catalog), not the raw stored value — Farshid
// asked directly for enum field data (e.g. status) to be multilingual,
// not just UI chrome. The stored/submitted <option value=""> stays the
// raw enum value regardless of locale — only the visible text changes.
func TestRender_EnumOptionLabelsAreTranslated(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields: []entity.Field{
			{Name: "item_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"stock", "service", "non_stock"}},
		},
	}
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "item_type", Label: "Type"}},
		}},
	}
	data := Data{Record: map[string]any{"item_type": "non_stock"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<option value="non_stock" selected>Non-Stock</option>`) {
		t.Fatalf("expected the translated label \"Non-Stock\" for the raw value \"non_stock\", got:\n%s", body)
	}
}

// TestRender_FieldLabelIsTranslated confirms a form's <label> text comes
// from the "field.{EntityType}.{FieldName}" i18n key, not just the
// hard-coded English form.FormField.Label — Arabic is used specifically
// because "الحالة" can't be mistaken for a coincidental match with the Go-
// declared "Status" fallback the way an untranslated locale might.
func TestRender_FieldLabelIsTranslated(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields: []entity.Field{
			{Name: "status", Type: entity.FieldEnum, Required: true, EnumValues: []string{"draft"}},
		},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "status", Label: "Status"}},
		}},
	}
	data := Data{Record: map[string]any{"status": "draft"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<label for="status">الحالة`) {
		t.Fatalf("expected the Arabic field label \"الحالة\", got:\n%s", buf.String())
	}
}

// TestRender_UntranslatedFieldLabelFallsBackToDeclaredLabel confirms a
// field with no "field.{EntityType}.{FieldName}" key yet still renders
// exactly as it did before this convention existed — additive, not a
// requirement to translate every field before it can render.
func TestRender_UntranslatedFieldLabelFallsBackToDeclaredLabel(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "NoSuchEntity",
		Fields:     []entity.Field{{Name: "widget_count", Type: entity.FieldNumber}},
	}
	def := &form.Definition{
		EntityType: "NoSuchEntity",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "widget_count", Label: "Widget Count"}},
		}},
	}
	data := Data{Record: map[string]any{"widget_count": float64(3)}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<label for="widget_count">Widget Count`) {
		t.Fatalf("expected the declared fallback label \"Widget Count\", got:\n%s", buf.String())
	}
}

// TestRender_ReferenceFieldWithNoOptionsRendersEmptySelect confirms a
// missing/broken target (internal/api's loadReferenceOptions degrades
// to no entry rather than failing the whole render) still produces a
// usable, if incomplete, form — not a render error.
func TestRender_ReferenceFieldWithNoOptionsRendersEmptySelect(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}},
		}},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<select id="vendor_id" name="vendor_id">`) {
		t.Fatalf("expected an empty but valid select, got:\n%s", buf.String())
	}
}

// TestRender_BoolFieldHasHiddenFalseFallbackAndTrueCheckboxValue is the
// regression test for a real bug caught by independent review on
// internal/api's form-submit-htmx branch: an unchecked HTML checkbox is
// omitted from a form submission entirely (never sent as "false"), and
// this renderer used to emit <input type="checkbox" ...> with no value
// attribute at all, meaning a browser defaults an unset checkbox's
// submitted value to "on" when checked — which
// internal/kernel/csvimport.Coerce's strconv.ParseBool rejects outright
// (it only accepts 1/t/T/TRUE/true/True and their false counterparts,
// not "on"). Every real "save a checked box" click 400'd. Fixed by
// pairing every bool field with a hidden fallback (value="false", so an
// unchecked box submits exactly that) followed by the checkbox itself
// explicitly given value="true" — the browser preserves DOM order in the
// submission, so a checked box submits "false" then "true", and the
// server takes the *last* value for that key (see
// internal/api/handlers.go's parseRecordFields).
func TestRender_BoolFieldHasHiddenFalseFallbackAndTrueCheckboxValue(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields:     []entity.Field{{Name: "is_urgent", Type: entity.FieldBool}},
	}
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "is_urgent", Label: "Urgent"}},
		}},
	}
	data := Data{Record: map[string]any{"is_urgent": true}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<input type="hidden" name="is_urgent" value="false"><input type="checkbox" id="is_urgent" name="is_urgent" value="true" checked>`) {
		t.Fatalf("expected a hidden false-fallback immediately before a checkbox with an explicit true value, got:\n%s", body)
	}
}

// TestRender_HiddenFieldsPreserveEntityFieldsNotShownOnForm is the
// regression test for the more severe of the two bugs independent
// review found: internal/data.RecordRepo.UpdateTx is a full replacement
// (SET data = $1), not a merge, so a deliberately partial form (this
// package's own foundation.go doc comment explicitly encourages building
// one field at a time, "as each is actually needed by a real screen")
// used to silently drop every entity field it doesn't visibly show, the
// very first time that form was saved. Fixed: every entDef field not
// referenced by any fields section now gets a hidden input carrying its
// current value, so a partial form still round-trips the complete
// record on submit.
func TestRender_HiddenFieldsPreserveEntityFieldsNotShownOnForm(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString},
			{Name: "internal_note", Type: entity.FieldString},
		},
	}
	// Deliberately only shows "sku" — "internal_note" is a real entity
	// field this form was never built to display.
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "sku", Label: "SKU"}},
		}},
	}
	data := Data{Record: map[string]any{"sku": "STEEL-BAR-10", "internal_note": "IMPORTANT, DO NOT LOSE"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `<input type="hidden" name="internal_note" value="IMPORTANT, DO NOT LOSE">`) {
		t.Fatalf("expected a hidden field preserving the off-form entity field's current value, got:\n%s", body)
	}
	if strings.Contains(body, `name="internal_note" value="IMPORTANT, DO NOT LOSE"><input type="hidden" name="internal_note"`) {
		t.Fatalf("expected internal_note to appear exactly once (no duplicate hidden field), got:\n%s", body)
	}
}

// TestRender_HiddenFieldsSkipFieldsAlreadyShownOnForm confirms a field
// that IS visibly shown doesn't also get a redundant separate hidden
// fallback (which would submit two different values for the same name,
// with the hidden one — the record's last-saved value, not whatever the
// user just typed — silently winning if it happened to be ordered last).
func TestRender_HiddenFieldsSkipFieldsAlreadyShownOnForm(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Item",
		Fields:     []entity.Field{{Name: "sku", Type: entity.FieldString}},
	}
	def := &form.Definition{
		EntityType: "Item",
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "sku", Label: "SKU"}},
		}},
	}
	data := Data{Record: map[string]any{"sku": "STEEL-BAR-10"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, `<input type="hidden" name="sku"`) {
		t.Fatalf("expected no redundant hidden fallback for a field already shown on the form, got:\n%s", body)
	}
}

func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	cat, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(cat)
}

func TestRender_HidesFieldWhenVisibleIfFalse(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"vendor_id": "v1", "payment_method": "Wire"},
		Children: map[string][]map[string]any{},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), `<label for="lc_reference"`) {
		t.Fatalf("expected lc_reference to have no visible input when payment_method != LC, got:\n%s", buf.String())
	}
}

// TestRender_VisibleIfHiddenFieldStillPreservesItsValue is the
// regression test for a real bug independent review found re-verifying
// the off-form-field data-loss fix: a field the form DOES list, but
// whose VisibleIf currently evaluates false for this record, was
// neither rendered as a visible input NOR preserved as a hidden
// fallback — buildHiddenFields' first version only checked whether a
// field was *listed* in the Definition, not whether it actually
// rendered, so a conditionally-hidden field's stored value fell through
// both paths and was silently wiped on the next save (proved by the
// reviewer using this exact fixture: an LC purchase order's
// lc_reference, saved while temporarily displaying as a Wire order).
func TestRender_VisibleIfHiddenFieldStillPreservesItsValue(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		// payment_method is "Wire", so lc_reference's VisibleIf
		// ("payment_method == 'LC'") is currently false — but the order
		// still carries a real lc_reference value from when it was
		// previously an LC order.
		Record:   map[string]any{"vendor_id": "v1", "payment_method": "Wire", "lc_reference": "LC-OLD-VALUE"},
		Children: map[string][]map[string]any{},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, `<label for="lc_reference"`) {
		t.Fatalf("expected no visible lc_reference input when payment_method != LC, got:\n%s", body)
	}
	if !strings.Contains(body, `<input type="hidden" name="lc_reference" value="LC-OLD-VALUE">`) {
		t.Fatalf("expected lc_reference's value preserved as a hidden field despite being VisibleIf-hidden, got:\n%s", body)
	}
}

func TestRender_ShowsFieldWhenVisibleIfTrue(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"vendor_id": "v1", "payment_method": "LC"},
		Children: map[string][]map[string]any{},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `name="lc_reference"`) {
		t.Fatalf("expected lc_reference to be shown when payment_method == LC, got:\n%s", buf.String())
	}
}

func TestRender_MasterDetailRollUp(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {
				{"line_total": 100.0},
				{"line_total": 250.5},
			},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "total: 350.5") {
		t.Fatalf("expected roll-up total 350.5, got:\n%s", out)
	}
	if !strings.Contains(out, `id="total" name="total" value="350.5"`) {
		t.Fatalf("expected the roll-up target field on the Header section to carry the freshly computed total, got:\n%s", out)
	}
}

func TestRender_MasterDetailEmptyShowsI18nMessage(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "لا توجد بنود بعد.") {
		t.Fatalf("expected Arabic empty-state message, got:\n%s", buf.String())
	}
}

func TestRender_RelatedListRowsAndEmptyState(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"id": "po-old", "status": "closed"}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `data-field="status"`) || !strings.Contains(out, "closed") {
		t.Fatalf("expected related record's status field rendered, got:\n%s", out)
	}
	if strings.Contains(out, "No related records.") {
		t.Fatalf("related list has a child row, should not show the empty state, got:\n%s", out)
	}
}

func TestRender_AllActionKindsRendered(t *testing.T) {
	r := testRenderer(t)
	data := Data{RecordID: "po-1", Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `type="submit"`) {
		t.Fatalf("expected save action to render a submit button, got:\n%s", out)
	}
	if !strings.Contains(out, `hx-post="/api/workflows/po_approval/start"`) {
		t.Fatalf("expected workflow.start action to render its hx-post, got:\n%s", out)
	}
	if !strings.Contains(out, `href="/api/reports/po_print?record_id=po-1"`) {
		t.Fatalf("expected report.render action to render its link, got:\n%s", out)
	}
	if !strings.Contains(out, `href="/purchase-orders"`) {
		t.Fatalf("expected navigate action to render its route, got:\n%s", out)
	}
}

func TestRender_RequiredFieldGetsSuffix(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Vendor *") {
		t.Fatalf("expected required field's label to carry the i18n required suffix, got:\n%s", out)
	}
	if !strings.Contains(out, `id="vendor_id" name="vendor_id" value="" required`) {
		t.Fatalf("expected required field's input to carry the HTML required attribute, got:\n%s", out)
	}
}

func TestRender_EscapesFieldValues(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		Record:   map[string]any{"vendor_id": `"><script>alert(1)</script>`, "payment_method": "Wire"},
		Children: map[string][]map[string]any{},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "<script>alert(1)</script>") {
		t.Fatalf("expected record value to be HTML-escaped, got:\n%s", buf.String())
	}
}

// TestRender_RecordIDCannotBreakHxAttributes is the regression test for
// the code-review finding that html/template's HTML-attribute escaping
// alone isn't URL- or JSON-safe: a RecordID containing "&" or `"` used to
// be interpolated directly into the hx-get query string and the hx-vals
// JSON literal, both of which the browser would HTML-decode back to the
// raw character before htmx parsed it as a URL/JSON — letting a crafted
// record ID smuggle an extra query parameter or JSON key. Both are now
// built with net/url and encoding/json server-side instead. A follow-up
// independent review found the form's own hx-post (the one sink this
// test didn't originally cover) still interpolated EntityType/RecordID
// raw — the identical bug class, just missed in the first hardening
// pass — now closed via the same url.PathEscape-built PostHref the other
// hrefs already use.
func TestRender_RecordIDCannotBreakHxAttributes(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: `1&admin=true","injected":"y`,
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"id": "po-old"}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// The form's own hx-post must be built via url.PathEscape, same as the
	// other hrefs, so the RecordID round-trips through the path segment
	// exactly. Two escaping layers stack here: url.PathEscape leaves
	// "&"/"=" literal (legal, unescaped pchar per RFC 3986 — harmless
	// since there's no "?" to make them look like query syntax; see the
	// "?" case below for the character that actually matters), and then
	// html/template's HTML-attribute-context escaping entity-encodes
	// that literal "&" into "&amp;" on top, the same double layer the
	// hx-vals assertion above already unwinds with html.UnescapeString
	// before json.Unmarshal — so this must unwind both layers in the
	// same order (HTML-unescape, then PathUnescape) to get back the
	// original RecordID.
	gotPostRaw := attrValueDQ(t, out, `hx-post="/api/records/PurchaseOrder/`)
	gotPostRecordID, err := url.PathUnescape(html.UnescapeString(gotPostRaw))
	if err != nil {
		t.Fatalf("hx-post record ID segment doesn't PathUnescape: %v", err)
	}
	if gotPostRecordID != data.RecordID {
		t.Fatalf("expected hx-post record ID to round-trip exactly, got %q want %q", gotPostRecordID, data.RecordID)
	}

	// The related_list hx-get URL must percent-encode the record ID, not
	// emit a literal unescaped "&" that would parse as an extra query param.
	if strings.Contains(out, `ref=PurchaseOrder:1&admin=true`) {
		t.Fatalf("record ID's '&' leaked into the query string unescaped, got:\n%s", out)
	}
	if !strings.Contains(out, url.QueryEscape(data.RecordID)) {
		t.Fatalf("expected the related_list href to contain the percent-encoded record ID, got:\n%s", out)
	}

	// The workflow.start hx-vals JSON must come from json.Marshal, so an
	// embedded '"' can never terminate the JSON string early.
	var vals map[string]string
	valsAttr := attrValue(t, out, `hx-vals='`)
	if err := json.Unmarshal([]byte(html.UnescapeString(valsAttr)), &vals); err != nil {
		t.Fatalf("hx-vals is not valid JSON after HTML-unescaping: %v\nattr: %s", err, valsAttr)
	}
	if vals["record_id"] != data.RecordID {
		t.Fatalf("expected hx-vals record_id to round-trip exactly, got %q want %q", vals["record_id"], data.RecordID)
	}
}

// TestRender_RecordIDQuestionMarkCannotBreakHxPostIntoQueryString is the
// regression test for the actual exploitable character in the hx-post
// path-segment injection: unlike "&"/"=" (legal, inert pchar per RFC
// 3986 — see TestRender_RecordIDCannotBreakHxAttributes), an unescaped
// "?" would end the path and start a query string, letting a crafted
// record ID append real query parameters to the form's own submit
// target. url.PathEscape must turn it into %3F.
func TestRender_RecordIDQuestionMarkCannotBreakHxPostIntoQueryString(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: `1?admin=true`,
		Record:   map[string]any{"payment_method": "Wire"},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `hx-post="/api/records/PurchaseOrder/1?admin=true"`) {
		t.Fatalf("record ID's '?' leaked into hx-post unescaped, turning the record-ID path segment into a query string: got:\n%s", out)
	}
	if !strings.Contains(out, `hx-post="/api/records/PurchaseOrder/1%3Fadmin=true"`) {
		t.Fatalf("expected hx-post's '?' to be percent-encoded to %%3F, got:\n%s", out)
	}
}

// TestRender_PostHrefOmitsRecordIDForNewRecord confirms the hx-post
// refactor preserved the existing new-vs-existing-record URL shape
// (/api/records/{EntityType} vs /api/records/{EntityType}/{RecordID}),
// not just that it's now escaped.
func TestRender_PostHrefOmitsRecordIDForNewRecord(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `hx-post="/api/records/PurchaseOrder"`) {
		t.Fatalf("expected a new (unsaved) record's hx-post to omit the record ID segment, got:\n%s", out)
	}
}

// attrValue extracts the text between the first occurrence of prefix and
// the next single quote — good enough for a test fixture's known markup.
func attrValue(t *testing.T, page, prefix string) string {
	t.Helper()
	return attrValueUntil(t, page, prefix, `'`)
}

// attrValueDQ is attrValue for a double-quoted attribute (e.g. hx-post="...").
func attrValueDQ(t *testing.T, page, prefix string) string {
	t.Helper()
	return attrValueUntil(t, page, prefix, `"`)
}

func attrValueUntil(t *testing.T, page, prefix, closing string) string {
	t.Helper()
	i := strings.Index(page, prefix)
	if i < 0 {
		t.Fatalf("prefix %q not found in:\n%s", prefix, page)
	}
	rest := page[i+len(prefix):]
	j := strings.Index(rest, closing)
	if j < 0 {
		t.Fatalf("unterminated attribute after prefix %q in:\n%s", prefix, page)
	}
	return rest[:j]
}

func TestRender_ErrorsWhenFormFieldMissingFromEntity(t *testing.T) {
	r := testRenderer(t)
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "not_a_real_field"}},
		}},
	}
	var buf strings.Builder
	err := r.Render(&buf, def, purchaseOrderEntity(), Data{}, "en")
	if err == nil {
		t.Fatal("expected error when a form field has no matching entity field")
	}
}

func TestRender_ErrorsOnMalformedVisibleIf(t *testing.T) {
	r := testRenderer(t)
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", VisibleIf: "payment_method LC"}},
		}},
	}
	var buf strings.Builder
	err := r.Render(&buf, def, purchaseOrderEntity(), Data{Record: map[string]any{}}, "en")
	if err == nil {
		t.Fatal("expected error for malformed visible_if expression")
	}
}
