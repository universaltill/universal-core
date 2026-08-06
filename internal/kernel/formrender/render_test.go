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

// TestRender_ReferenceFieldRendersAsSearchableCombobox is the regression
// test for #24's scaling fix. A FieldReference field first rendered as a
// plain text input (fillable only by typing a raw UUID from memory), then
// as a <select> of every target record — which fell over at real
// customer-list scale. It now renders as a searchable combobox
// (.uc-ref: a hidden id input the form submits, a visible search box, and
// an async results div) that fetches candidates on demand from
// internal/api's /api/references endpoint. Data.ReferenceOptions no longer
// carries every candidate — only the CURRENT value's label, pre-loaded by
// internal/api's loadCurrentReferenceLabels so an existing record shows a
// name rather than a raw id on load (see ReferenceOption's own doc
// comment; this package has no registry/crud access itself).
func TestRender_ReferenceFieldRendersAsSearchableCombobox(t *testing.T) {
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
	// A searchable combobox targeting the referenced entity, not a
	// <select> of every record (#24 — the full-list select doesn't scale).
	if !strings.Contains(body, `class="uc-ref" data-target="Party" data-field="vendor_id"`) {
		t.Fatalf("expected a reference combobox targeting Party, got:\n%s", body)
	}
	// The actual submitted value is the id, in a hidden input. `required`
	// is NOT on the hidden input — HTML5 constraint validation ignores
	// hidden inputs, so `required` there is inert; it belongs on the
	// visible search box (asserted below).
	if !strings.Contains(body, `<input type="hidden" name="vendor_id" value="vendor-42">`) {
		t.Fatalf("expected the id in a plain hidden input (no inert required), got:\n%s", body)
	}
	// The visible search box shows the current record's LABEL, not its id,
	// carries id="vendor_id" so the field's <label for> associates with it,
	// and carries `required` where the browser actually enforces it.
	if !strings.Contains(body, `<input type="text" id="vendor_id" class="uc-ref-search" autocomplete="off" value="Beta Supplies" placeholder="Search…" required>`) {
		t.Fatalf("expected a required, id'd search box showing the current label, got:\n%s", body)
	}
	if strings.Contains(body, "vendor-42\">Beta") { // no leftover <option>
		t.Fatalf("reference field should no longer render <option>s:\n%s", body)
	}
}

// TestRender_ReferenceFieldCreateNewButtonRendersWhenLabelProvided is the
// formrender-layer test for part 2 of #24 (universaltill/uc-infra#51):
// when the caller (internal/api) has decided the viewer may create the
// referenced entity and hands down a label for it via
// Data.ReferenceCreateLabels, the combobox renders a quick-create
// button carrying that exact text and the field's own RefTarget as
// data-target — the JS in layout.go reads data-target to know which
// entity's /forms/{target}/new to fetch.
func TestRender_ReferenceFieldCreateNewButtonRendersWhenLabelProvided(t *testing.T) {
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
	data := Data{
		Record:                map[string]any{},
		ReferenceCreateLabels: map[string]string{"vendor_id": "+ Create new Party"},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	// "+" renders HTML-entity-escaped ("&#43;", Go html/template's own
	// text-node escaping table) — a browser decodes it right back to "+"
	// (confirmed via the real-browser test, internal/e2e's
	// TestReferencePickerQuickCreate_..., which reads .textContent and
	// sees plain "+"), so this is what the served HTML source actually
	// looks like, not a bug in this feature.
	if !strings.Contains(body, `<button type="button" class="uc-ref-create" data-target="Party">&#43; Create new Party</button>`) {
		t.Fatalf("expected a quick-create button for Party, got:\n%s", body)
	}
}

// TestRender_ReferenceFieldNoCreateButtonWithoutLabel is the inverse of
// the above: a field with no entry in ReferenceCreateLabels (the viewer
// lacks create permission on the target, or the caller simply never
// populated it) must render no quick-create affordance at all — the
// button's mere presence in the DOM is what part 2 of #24's RBAC
// requirement rests on, so its absence has to be just as deliberate as
// its presence.
func TestRender_ReferenceFieldNoCreateButtonWithoutLabel(t *testing.T) {
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
	if strings.Contains(buf.String(), "uc-ref-create") {
		t.Fatalf("expected no quick-create button without a ReferenceCreateLabels entry, got:\n%s", buf.String())
	}
}

// TestRender_ReferenceFieldMustMatchParentFieldRendersDataAttribute is
// the formrender-layer test for uc-infra#78's MustMatchParentField
// wiring — the review flagged this had zero coverage (finding #7). A
// field declaring MustMatchParentField must render the
// data-must-match-field attribute the client-side picker script reads
// to know which sibling input's current value to submit as
// sibling_value on its next /api/references search.
func TestRender_ReferenceFieldMustMatchParentFieldRendersDataAttribute(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Task",
		Fields: []entity.Field{
			{Name: "project_id", Type: entity.FieldReference, Target: "Project"},
			{Name: "parent_task_id", Type: entity.FieldReference, Target: "Task", MustMatchParentField: "project_id"},
		},
	}
	def := &form.Definition{
		EntityType: "Task",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{
				{Name: "project_id", Label: "Project"},
				{Name: "parent_task_id", Label: "Parent Task"},
			},
		}},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `class="uc-ref" data-target="Task" data-field="parent_task_id" data-must-match-field="project_id"`) {
		t.Fatalf("expected parent_task_id's combobox to carry data-must-match-field=\"project_id\", got:\n%s", body)
	}
	// project_id itself declares no MustMatchParentField and must NOT
	// pick up the attribute from its neighbour.
	if !strings.Contains(body, `class="uc-ref" data-target="Project" data-field="project_id">`) {
		t.Fatalf("expected project_id's combobox to carry no data-must-match-field attribute, got:\n%s", body)
	}
}

// TestRender_ReferenceFieldWithoutMustMatchParentFieldOmitsDataAttribute
// is the inverse: a plain FieldReference with no MustMatchParentField
// declared must render no data-must-match-field attribute at all — its
// mere absence in the DOM is what the client script's "skip the sibling
// read" branch relies on, so it needs to be asserted as deliberately as
// its presence above.
func TestRender_ReferenceFieldWithoutMustMatchParentFieldOmitsDataAttribute(t *testing.T) {
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
	if strings.Contains(buf.String(), "data-must-match-field") {
		t.Fatalf("expected no data-must-match-field attribute for a field with no MustMatchParentField, got:\n%s", buf.String())
	}
}

// TestRender_SavedRecordCarriesDataRecordIDAndLabel proves the two data
// attributes the quick-create modal's JS relies on to read back a just-
// created record (layout.go's htmx:afterSettle handler) actually appear
// on a saved record's form tag: data-record-id (the new id) and
// data-record-label (the human label, resolved by the caller the same
// way an existing reference's CurrentLabel already is).
func TestRender_SavedRecordCarriesDataRecordIDAndLabel(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Party",
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldString}},
	}
	def := &form.Definition{
		EntityType: "Party",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}},
		}},
	}
	data := Data{
		RecordID:    "party-7",
		RecordLabel: "Acme Textiles",
		Record:      map[string]any{"name": "Acme Textiles"},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `data-record-id="party-7"`) {
		t.Fatalf("expected data-record-id on the saved form, got:\n%s", body)
	}
	if !strings.Contains(body, `data-record-label="Acme Textiles"`) {
		t.Fatalf("expected data-record-label on the saved form, got:\n%s", body)
	}
}

// TestRender_NewRecordHasNoDataRecordIDAttribute is the negative
// counterpart: a brand-new, unsaved record (RecordID == "") must not
// carry data-record-id/data-record-label at all — the quick-create
// modal's success detection (layout.go: `form.uc-form[data-record-id]`)
// depends on this attribute being entirely absent, not merely empty,
// for an unsaved form (e.g. a failed validation re-render must not look
// like a success).
func TestRender_NewRecordHasNoDataRecordIDAttribute(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Party",
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldString}},
	}
	def := &form.Definition{
		EntityType: "Party",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}},
		}},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "data-record-id") {
		t.Fatalf("expected no data-record-id attribute on a new/unsaved record's form, got:\n%s", buf.String())
	}
}

// TestRender_OptionalReferenceFieldGetsEmptyOption confirms an
// optional (not required) reference field always offers a real "leave
// unset" choice — without this, a browser's own <select> default
// (whichever option renders first) would look selected on an untouched
// new-record form even though nothing was actually chosen, and
// submitting it would silently write that first option's id.
func TestRender_UnsetOptionalReferenceFieldGetsEmptyCombobox(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{Title: "H", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}}}},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, Data{ReferenceOptions: map[string][]ReferenceOption{}}, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	// Unset, optional: an empty hidden id (no `required`) and a blank
	// search box.
	if !strings.Contains(body, `<input type="hidden" name="vendor_id" value="">`) {
		t.Fatalf("unset optional reference should have an empty, non-required hidden id, got:\n%s", body)
	}
	if strings.Contains(body, `name="vendor_id" value="" required`) {
		t.Fatalf("an OPTIONAL reference must not be required:\n%s", body)
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
func TestRender_UnsetRequiredReferenceFieldComboboxIsRequired(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{Title: "H", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}}}},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, Data{ReferenceOptions: map[string][]ReferenceOption{}}, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	// A required reference with no value: the hidden id is empty (and
	// plain — `required` on a hidden input is inert), while the visible
	// search box carries `required` so the browser actually blocks submit
	// until the user picks an option (which fills the hidden id via the
	// combobox JS). The server still enforces the requirement regardless.
	body := buf.String()
	if !strings.Contains(body, `<input type="hidden" name="vendor_id" value="">`) {
		t.Fatalf("unset required reference should have an empty plain hidden id, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-ref-search" autocomplete="off" value="" placeholder="Search…" required>`) {
		t.Fatalf("unset required reference should mark the VISIBLE search box required, got:\n%s", body)
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
// Piggybacks on the real "field.PurchaseOrder.status_id" locale key
// (purchasing.PurchaseOrder's actual field, since it opted into
// foundation.go's Status/StatusType pattern — see that entity's own doc
// comment) rather than fabricating a synthetic one; the fixture below is
// still a hand-built ad hoc Definition, not an import of the real
// purchasing package, matching every other formrender test's own
// module-independence.
func TestRender_FieldLabelIsTranslated(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields: []entity.Field{
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "status_id", Label: "Status"}},
		}},
	}
	data := Data{Record: map[string]any{"status_id": "draft"}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<label for="status_id">الحالة`) {
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

// TestRender_ReferenceFieldWithNoOptionsStillRendersCombobox confirms a
// reference field with no pre-loaded current-value label (a new/unset
// field, or a target whose label lookup degraded to no entry rather than
// failing the whole render — see internal/api's loadCurrentReferenceLabels)
// still produces a usable combobox with an empty search box, not a render
// error.
func TestRender_ReferenceFieldWithNoOptionsStillRendersCombobox(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields:     []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{Title: "H", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}}}},
	}
	var buf strings.Builder
	// No ReferenceOptions at all — the combobox still renders (it searches
	// the endpoint on demand; it never needed the options preloaded).
	if err := r.Render(&buf, def, ent, Data{}, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `class="uc-ref" data-target="Party"`) {
		t.Fatalf("a reference with no preloaded options should still render a combobox, got:\n%s", buf.String())
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
	// The label is "Total" (resolved through field.PurchaseOrder.total,
	// #99), not the raw "total" field name; data-field stays raw so a
	// selector keyed on it still works across locales.
	if !strings.Contains(out, `<p class="uc-rollup" data-field="total">Total: 350.5</p>`) {
		t.Fatalf("expected roll-up total 350.5 with a resolved label, got:\n%s", out)
	}
	if !strings.Contains(out, `id="total" name="total" value="350.5"`) {
		t.Fatalf("expected the roll-up target field on the Header section to carry the freshly computed total, got:\n%s", out)
	}
}

// TestRender_MasterDetailColumnsAndRollUpLabelResolveThroughCatalog (#99):
// master-detail column headers and the roll-up label previously rendered
// the raw snake_case field name verbatim in every locale — the one
// rendering path in this form that never got the field.{EntityType}.
// {FieldName} treatment #53/#85's sibling field labels already have. Uses
// the real POLine/PurchaseOrder catalog entries (not a synthetic test
// key) so this is a proof the production wiring resolves, the same shape
// TestRender_SectionTitleResolvesThroughCatalog uses for #53. Turkish is
// asserted, not English, because an English-locale assertion here could
// pass by coincidence (Total/total, Quantity/quantity) without proving
// translation actually happened (same reasoning that test documents).
func TestRender_MasterDetailColumnsAndRollUpLabelResolveThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "POLine", Version: 1,
		Fields: []entity.Field{{Name: "qty", Type: entity.FieldNumber}},
	}
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"qty": 5.0, "line_total": 5.0}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<th data-field="qty">Miktar</th>`) {
		t.Errorf("expected the qty column header resolved to Turkish via field.POLine.qty, got:\n%s", out)
	}
	if !strings.Contains(out, `data-field="total">Toplam: 5</p>`) {
		t.Errorf("expected the roll-up label resolved to Turkish via field.PurchaseOrder.total, got:\n%s", out)
	}
}

// TestRender_MasterDetailColumnsFallBackToRawFieldNameWhenUntranslated
// (#99): a column/roll-up label with no catalog entry — or no child
// Definition at all — must still show the raw field name, not go blank.
// This is the branch that protects every module from #99's own
// regression (GoodsReceiptLine's four fields shipping with no catalog
// keys, caught by independent review): TOrDefault degrading silently to
// its fallback is correct behavior at render time, but only if the
// fallback is actually wired through, which is exactly what this test
// pins down.
func TestRender_MasterDetailColumnsFallBackToRawFieldNameWhenUntranslated(t *testing.T) {
	r := testRenderer(t)

	// No catalog entry exists for field.Untranslated.* in any locale.
	childDef := &entity.Definition{
		EntityType: "Untranslated", Version: 1,
		Fields: []entity.Field{{Name: "widget_count", Type: entity.FieldNumber}},
	}
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"widget_count": 3.0}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<th data-field="widget_count">widget_count</th>`) {
		t.Errorf("expected an untranslated column header to fall back to the raw field name, got:\n%s", out)
	}

	// No ChildDefs entry at all (childDef == nil): buildChildRows falls
	// back to the union of the rows' own keys, and childColumns must
	// still label each with its own raw name, not an empty string.
	dataNoChildDef := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"widget_count": 3.0}},
		},
	}
	var buf2 strings.Builder
	if err := r.Render(&buf2, purchaseOrderForm(), purchaseOrderEntity(), dataNoChildDef, "en"); err != nil {
		t.Fatalf("render (no ChildDefs): %v", err)
	}
	if !strings.Contains(buf2.String(), `<th data-field="widget_count">widget_count</th>`) {
		t.Errorf("expected a column header with no child Definition at all to still show the raw field name, got:\n%s", buf2.String())
	}
}

// TestRender_MasterDetailEnumCellIsLocalized (uc-infra#79's
// childCellValue fix): a FieldEnum child column's CELL value, not just
// its column header, resolves through the same "field.{EntityType}.
// {FieldName}.{Value}" catalog key buildFields' <select> options
// already use — before this, no composition child anywhere declared a
// FieldEnum field, so a table cell holding one had never actually been
// exercised. Uses ProjectBudgetLine's real, shipped catalog keys rather
// than a synthetic fixture, so this pins the real production behavior,
// not just the mechanism in isolation.
func TestRender_MasterDetailEnumCellIsLocalized(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "ProjectBudgetLine", Version: 1,
		Fields: []entity.Field{
			{Name: "category", Type: entity.FieldEnum, EnumValues: []string{"labour", "materials"}},
		},
	}
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"category": "labour"}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<td data-field="category">İşçilik</td>`) {
		t.Errorf("expected the Turkish translation of category=labour in the cell, got:\n%s", out)
	}
	if strings.Contains(out, `>labour<`) {
		t.Errorf("raw untranslated enum value leaked into a Turkish-locale cell, got:\n%s", out)
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

// TestRender_RelatedListColumnHeadersResolveThroughCatalog (uc-infra#103):
// buildChildRows computes a fully catalog-resolved Columns for related_list
// sections exactly like master_detail (see TestRender_MasterDetail
// ColumnsAndRollUpLabelResolveThroughCatalog, the sibling test this
// mirrors), but the template used to discard it — no header of any kind
// rendered, so every related_list section in the app showed unlabeled
// cells. Uses the real PurchaseOrder catalog entry (not a synthetic test
// key), same reasoning as the master_detail test: an English assertion
// here could pass by coincidence, Turkish proves translation actually
// happened.
func TestRender_RelatedListColumnHeadersResolveThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "PurchaseOrder", Version: 1,
		Fields: []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"vendor_id": "vendor-1"}},
		},
		ChildDefs: map[string]*entity.Definition{"PurchaseOrder": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<span class="uc-related-header-cell" data-field="vendor_id">Tedarikçi</span>`) {
		t.Errorf("expected the vendor_id column header resolved to Turkish via field.PurchaseOrder.vendor_id, got:\n%s", out)
	}
}

// TestRender_RelatedListColumnHeadersFallBackToRawFieldNameWhenUntranslated
// (uc-infra#103): mirrors TestRender_MasterDetailColumnsFallBackToRaw
// FieldNameWhenUntranslated — an untranslated field, or no child
// Definition at all, must still show the raw field name as the header
// rather than going blank or omitting the header entirely.
func TestRender_RelatedListColumnHeadersFallBackToRawFieldNameWhenUntranslated(t *testing.T) {
	r := testRenderer(t)

	// No catalog entry exists for field.Untranslated.* in any locale.
	childDef := &entity.Definition{
		EntityType: "Untranslated", Version: 1,
		Fields: []entity.Field{{Name: "widget_count", Type: entity.FieldNumber}},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"widget_count": 3.0}},
		},
		ChildDefs: map[string]*entity.Definition{"PurchaseOrder": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<span class="uc-related-header-cell" data-field="widget_count">widget_count</span>`) {
		t.Errorf("expected an untranslated column header to fall back to the raw field name, got:\n%s", out)
	}

	// No ChildDefs entry at all (childDef == nil): buildChildRows falls
	// back to the union of the rows' own keys, and childColumns must still
	// label each with its own raw name, not an empty string.
	dataNoChildDef := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"widget_count": 3.0}},
		},
	}
	var buf2 strings.Builder
	if err := r.Render(&buf2, purchaseOrderForm(), purchaseOrderEntity(), dataNoChildDef, "en"); err != nil {
		t.Fatalf("render (no ChildDefs): %v", err)
	}
	if !strings.Contains(buf2.String(), `<span class="uc-related-header-cell" data-field="widget_count">widget_count</span>`) {
		t.Errorf("expected a column header with no child Definition at all to still show the raw field name, got:\n%s", buf2.String())
	}
}

// TestRender_RelatedListReferenceCellResolvesToLabel (uc-infra#85): a
// related-list/master-detail FieldReference cell must resolve through
// ChildReferenceLabels the same way a top-level list column resolves
// through internal/api's pageReferenceLabels — before this fix,
// childCellValue only special-cased FieldI18nText, so this cell printed
// the raw stored id no matter what the caller passed. Regression test:
// written failing against the unfixed childCellValue (returned the raw
// "vendor-1" id), now passes against the fix.
func TestRender_RelatedListReferenceCellResolvesToLabel(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "PurchaseOrder", Version: 1,
		Fields: []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"vendor_id": "vendor-1"}},
		},
		ChildDefs:            map[string]*entity.Definition{"PurchaseOrder": childDef},
		ChildReferenceLabels: map[string]map[string]map[string]string{"PurchaseOrder": {"vendor_id": {"vendor-1": "Acme Supply Co"}}},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Acme Supply Co") {
		t.Errorf("expected the vendor_id cell resolved to its label, got:\n%s", out)
	}
	if strings.Contains(out, "vendor-1") {
		t.Errorf("expected the raw vendor id NOT to leak into the cell once a label was available, got:\n%s", out)
	}
}

// TestRender_RelatedListReferenceCellFallsBackToRawIDWhenUnresolved
// (uc-infra#85): a reference whose label wasn't resolved by the caller
// (dangling id, or this viewer lacking read access to the target — same
// cases cellText's own referenceLabels already tolerates) must still
// show something rather than silently vanish or error the whole render —
// same "visible-but-broken beats hidden" reasoning as the top-level list
// view. Covers both a nil ChildReferenceLabels entirely and a present
// section/field map that simply doesn't have this particular id.
func TestRender_RelatedListReferenceCellFallsBackToRawIDWhenUnresolved(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "PurchaseOrder", Version: 1,
		Fields: []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	baseData := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"vendor_id": "vendor-dangling"}},
		},
		ChildDefs: map[string]*entity.Definition{"PurchaseOrder": childDef},
	}

	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), baseData, "en"); err != nil {
		t.Fatalf("render (no ChildReferenceLabels at all): %v", err)
	}
	if !strings.Contains(buf.String(), "vendor-dangling") {
		t.Errorf("expected the raw id as a fallback when no reference labels were supplied at all, got:\n%s", buf.String())
	}

	withEmptyEntry := baseData
	withEmptyEntry.ChildReferenceLabels = map[string]map[string]map[string]string{"PurchaseOrder": {"vendor_id": {"some-other-id": "Someone Else"}}}
	var buf2 strings.Builder
	if err := r.Render(&buf2, purchaseOrderForm(), purchaseOrderEntity(), withEmptyEntry, "en"); err != nil {
		t.Fatalf("render (id not in ChildReferenceLabels): %v", err)
	}
	if !strings.Contains(buf2.String(), "vendor-dangling") {
		t.Errorf("expected the raw id as a fallback when this specific id has no resolved label, got:\n%s", buf2.String())
	}
}

// TestRender_MasterDetailEnumCellResolvesThroughCatalog (uc-infra#85): a
// child FieldEnum cell resolves through the same
// "field.{EntityType}.{FieldName}.{Value}" catalog convention buildFields
// already uses for a top-level enum <select>'s option labels. Turkish is
// asserted, not English, for the same reason the sibling column-header
// test in this file uses Turkish: an English assertion could pass by
// coincidence (raw value happening to equal its own translation).
func TestRender_MasterDetailEnumCellResolvesThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "Lead", Version: 1,
		Fields: []entity.Field{{Name: "source", Type: entity.FieldEnum, EnumValues: []string{"event"}}},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"source": "event"}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Etkinlik") {
		t.Errorf("expected the source cell resolved to Turkish via field.Lead.source.event, got:\n%s", out)
	}
	if strings.Contains(out, `data-field="source">event<`) {
		t.Errorf("expected the raw enum value NOT to leak into the cell once a translation was available, got:\n%s", out)
	}
}

// TestRender_MasterDetailEnumCellFallsBackToRawValueWhenUntranslated
// (uc-infra#85): mirrors the column-header and reference-cell fallback
// tests — an enum value with no catalog entry for this child entity type
// must still render its raw stored value, not go blank.
func TestRender_MasterDetailEnumCellFallsBackToRawValueWhenUntranslated(t *testing.T) {
	r := testRenderer(t)
	// No catalog entry exists for field.Untranslated.* in any locale.
	childDef := &entity.Definition{
		EntityType: "Untranslated", Version: 1,
		Fields: []entity.Field{{Name: "status", Type: entity.FieldEnum, EnumValues: []string{"open"}}},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"POLine": {{"status": "open"}},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `data-field="status">open<`) {
		t.Errorf("expected an untranslated enum cell to fall back to its raw value, got:\n%s", buf.String())
	}
}

// TestRender_RelatedListNoHeaderWhenNoColumns (uc-infra#103): the
// {{if .Columns}} guard itself is untested by every other case above —
// they all supply either a ChildDef or at least one row to derive
// columns from. With neither (a related_list with zero rows and no
// child Definition at all — e.g. a section whose Target has no matching
// Relationship, which loadMasterDetailChildren silently skips), Columns
// is empty and no .uc-related-header element should render at all.
// Removing the guard entirely still passes every other test in this
// file; only this one pins it down.
func TestRender_RelatedListNoHeaderWhenNoColumns(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{"PurchaseOrder": {}},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "uc-related-header") {
		t.Errorf("expected no header element when there are no columns to describe, got:\n%s", buf.String())
	}
}

// TestRender_RelatedListHeaderRendersEvenWhenEmpty (uc-infra#103): the
// header describes the child Definition's declared field order (see
// buildChildRows), independent of whether any rows exist — same as
// master_detail's <thead>, which always renders regardless of .Children.
// A related list with zero rows today should still show what its columns
// WOULD be, not just the empty-state message.
func TestRender_RelatedListHeaderRendersEvenWhenEmpty(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "PurchaseOrder", Version: 1,
		Fields: []entity.Field{{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"}},
	}
	data := Data{
		Record:    map[string]any{"payment_method": "Wire"},
		Children:  map[string][]map[string]any{"PurchaseOrder": {}},
		ChildDefs: map[string]*entity.Definition{"PurchaseOrder": childDef},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<span class="uc-related-header-cell" data-field="vendor_id">Vendor</span>`) {
		t.Errorf("expected the column header to render even with zero rows, got:\n%s", out)
	}
	if !strings.Contains(out, "No related records.") {
		t.Errorf("expected the empty-state message to still render alongside the header, got:\n%s", out)
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

	// A related_list section no longer emits a lazy-load URL at all —
	// its rows are rendered server-side (board #20: the endpoint that
	// href pointed at ignored the ref filter and returned every record
	// of the target type, so an asset's history listed other assets'
	// work). Assert the href is gone, so a future change can't quietly
	// reintroduce an unfiltered fetch.
	if strings.Contains(out, "uc-related-list\" hx-get") || strings.Contains(out, "ref=PurchaseOrder") {
		t.Fatalf("related_list must not lazy-load from an unfiltered endpoint, got:\n%s", out)
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

// TestRender_RedactedFieldIsAbsentEntirely covers ADR-0006's field-level
// commit: a field the viewer's RBAC rules hide must render neither as a
// visible input NOR as one of buildHiddenFields' preservation inputs.
//
// The second half is the one worth a test of its own. Every other
// off-form field DOES get a hidden input here (that's the fix for a real
// data-loss bug — see buildHiddenFields' own doc comment), so the
// obvious implementation of redaction — skip it in buildFields and stop
// — would leave the field name sitting in the DOM and submit it back
// empty on every save. Its preservation has to happen server-side
// instead (authz.GuardedEngine.EffectiveWriteFields), precisely because
// the browser must never hold the value.
func TestRender_RedactedFieldIsAbsentEntirely(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		Record: map[string]any{
			"vendor_id":      "v1",
			"payment_method": "LC",
			"lc_reference":   "LC-99",
			"total":          1234.0,
		},
		Children:       map[string][]map[string]any{},
		RedactedFields: map[string]bool{"total": true},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if strings.Contains(body, `name="total"`) {
		t.Fatalf("redacted field rendered an input (visible or hidden), got:\n%s", body)
	}
	if strings.Contains(body, "1234") {
		t.Fatalf("redacted field's value reached the DOM, got:\n%s", body)
	}
	// Everything else still renders exactly as before — redaction must be
	// surgical, not a reason for the rest of the form to change shape.
	if !strings.Contains(body, `name="vendor_id"`) {
		t.Fatalf("visible field missing after redacting another, got:\n%s", body)
	}
	if !strings.Contains(body, `name="lc_reference"`) {
		t.Fatalf("conditionally-visible field missing after redacting another, got:\n%s", body)
	}
}

// A redacted field must stay redacted even when its VisibleIf would
// otherwise show it: the permission decision outranks the layout
// expression, which is exactly R5's "a hidden field is hidden by RBAC,
// not by the layout."
func TestRender_RedactionOutranksVisibleIf(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID:       "po-1",
		Record:         map[string]any{"vendor_id": "v1", "payment_method": "LC", "lc_reference": "LC-77"},
		Children:       map[string][]map[string]any{},
		RedactedFields: map[string]bool{"lc_reference": true},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, "lc_reference") || strings.Contains(body, "LC-77") {
		t.Fatalf("visible_if=true field survived redaction, got:\n%s", body)
	}
}

// Nothing redacted -> byte-identical output to before this mechanism
// existed. This is the backward-compatibility guarantee for every tenant
// that has never authored a FieldPermission row (i.e. all of them today).
func TestRender_NoRedactionMatchesUnfilteredRender(t *testing.T) {
	r := testRenderer(t)
	base := func(redacted map[string]bool) string {
		t.Helper()
		var buf strings.Builder
		err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), Data{
			RecordID:       "po-1",
			Record:         map[string]any{"vendor_id": "v1", "payment_method": "LC", "lc_reference": "LC-1"},
			Children:       map[string][]map[string]any{},
			RedactedFields: redacted,
		}, "en")
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}
	if got, want := base(map[string]bool{}), base(nil); got != want {
		t.Fatalf("empty redaction set changed the render:\n%s\n---\n%s", got, want)
	}
}

// TestRender_I18nTextRendersOneInputPerLocale covers the i18n_text field
// type (ADR-0009): a multilingual field renders one text input per catalog
// locale, each named "{field}.{locale}" so the API form decoder can
// reassemble the object, pre-filled from the stored per-locale values,
// with `required` on the fallback (primary) locale only — not every input.
func TestRender_I18nTextRendersOneInputPerLocale(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "Unit",
		Fields:     []entity.Field{{Name: "label", Type: entity.FieldI18nText, Required: true}},
	}
	def := &form.Definition{
		EntityType: "Unit",
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "label", Label: "Label"}},
		}},
	}
	data := Data{Record: map[string]any{"label": map[string]any{"en": "Each", "tr": "Adet"}}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// One input per catalog locale (ar, en, fa, tr), each named field.locale.
	for _, loc := range []string{"ar", "en", "fa", "tr"} {
		if !strings.Contains(body, `name="label.`+loc+`"`) {
			t.Fatalf("expected an input for locale %q (name=\"label.%s\"), got:\n%s", loc, loc, body)
		}
	}
	// Stored values are pre-filled.
	if !strings.Contains(body, `name="label.en" value="Each"`) {
		t.Fatalf("expected the en value pre-filled, got:\n%s", body)
	}
	if !strings.Contains(body, `name="label.tr" value="Adet"`) {
		t.Fatalf("expected the tr value pre-filled, got:\n%s", body)
	}
	// A locale with no stored value renders an empty input, not an error.
	if !strings.Contains(body, `name="label.fa" value=""`) {
		t.Fatalf("expected an empty fa input, got:\n%s", body)
	}
	// `required` is ONLY on the fallback locale (en), not on every input:
	// a required multilingual field must be named in the primary language,
	// not in all four at once.
	if !strings.Contains(body, `name="label.en" value="Each" autocomplete="off" aria-label="label en" required>`) {
		t.Fatalf("expected required on the en (fallback) input, got:\n%s", body)
	}
	if strings.Contains(body, `name="label.tr" value="Adet" autocomplete="off" aria-label="label tr" required>`) {
		t.Fatalf("required must NOT be on a non-fallback locale input, got:\n%s", body)
	}
}

// TestRender_RelatedListIsServerRenderedNotLazyLoaded is the regression
// test for board #20's second blocker. The section used to render empty
// with hx-trigger="load", and the endpoint it fetched ignored the ref
// filter — so an asset's "Maintenance History" was replaced on load by
// a JSON dump of every record of that type in the tenant, other
// assets' work orders included. Rows must now come from the server,
// already filtered, with no fetch at all.
func TestRender_RelatedListIsServerRenderedNotLazyLoaded(t *testing.T) {
	r := testRenderer(t)
	data := Data{
		RecordID: "po-1",
		Record:   map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			"PurchaseOrder": {{"id": "po-mine", "status": "mine-only"}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "mine-only") {
		t.Fatalf("related row must be rendered server-side, got:\n%s", out)
	}
	// No load-triggered fetch anywhere in the related-list markup: one
	// would swap the correct rows out for whatever the endpoint returns.
	relatedIdx := strings.Index(out, "uc-related-list")
	if relatedIdx < 0 {
		t.Fatalf("no related list rendered:\n%s", out)
	}
	segment := out[relatedIdx:]
	if end := strings.Index(segment, "</div>"); end > 0 {
		segment = segment[:end]
	}
	if strings.Contains(segment, "hx-trigger=\"load\"") || strings.Contains(segment, "hx-get") {
		t.Fatalf("related list must not fetch on load, got:\n%s", segment)
	}
}

// TestRender_ChildRowsResolveI18nAndAlignColumns is the regression test
// for board #18's blocker. A child entity with an i18n_text field
// (Task.title is the repo's first) previously rendered the raw stored
// map — "map[en:Design tr:Tasarım]" — identical in every locale: Go
// internals shown to a user. And because columns were derived from each
// row's own keys, a row with an optional field set got an extra cell,
// shifting every column after it out of alignment with its neighbours.
func TestRender_ChildRowsResolveI18nAndAlignColumns(t *testing.T) {
	r := testRenderer(t)
	childDef := &entity.Definition{
		EntityType: "Sub", Version: 1,
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldI18nText, Required: true},
			{Name: "parent_id", Type: entity.FieldReference, Target: "Sub"},
			{Name: "hours", Type: entity.FieldNumber},
		},
	}
	data := Data{
		Record: map[string]any{"payment_method": "Wire"},
		Children: map[string][]map[string]any{
			// Deliberately ragged: only the second row has parent_id.
			"POLine": {
				{"title": map[string]any{"en": "Design", "tr": "Tasarım"}, "hours": 8.0},
				{"title": map[string]any{"en": "Sub", "tr": "Alt"}, "parent_id": "t1", "hours": 2.0},
			},
		},
		ChildDefs: map[string]*entity.Definition{"POLine": childDef},
	}

	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "map[") {
		t.Errorf("a raw Go map leaked into the rendered page:\n%s", out)
	}
	if !strings.Contains(out, ">Design<") {
		t.Errorf("i18n child cell not resolved to the viewer's locale:\n%s", out)
	}

	// Same data, Turkish viewer: the cell must actually differ.
	var tr strings.Builder
	if err := r.Render(&tr, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render tr: %v", err)
	}
	if !strings.Contains(tr.String(), ">Tasarım<") {
		t.Errorf("Turkish viewer did not get the Turkish title:\n%s", tr.String())
	}

	// Every row has one cell per declared column, in Definition order,
	// so a missing optional value cannot shift its neighbours.
	rows := strings.Count(out, "<tr>") - strings.Count(out, "<thead><tr>")
	if rows < 2 {
		t.Fatalf("expected two child rows, got %d:\n%s", rows, out)
	}
	for _, want := range []string{
		`<td data-field="title">Design</td><td data-field="parent_id"></td><td data-field="hours">8</td>`,
		`<td data-field="title">Sub</td><td data-field="parent_id">t1</td><td data-field="hours">2</td>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected aligned row %q in:\n%s", want, out)
		}
	}
	// And the columns are labelled, which they never were before.
	if !strings.Contains(out, `<th data-field="title">`) {
		t.Errorf("child table has no header row:\n%s", out)
	}
}

// TestRender_SectionTitleResolvesThroughCatalog (#53): a section whose
// EntityType+Title has a real form.{EntityType}.section.{slug} catalog
// entry renders the translation, not the Go Definition's literal Title —
// same proof-of-wiring shape as the field-label tests already establish
// for field.{EntityType}.{FieldName} (and the same reason
// TestPurchaseOrderFormStages_RealBrowser in internal/e2e deliberately
// asserts a non-English string: an English-locale assertion here would
// pass identically whether the catalog is actually consulted or not,
// since purchaseOrderForm's own Header/Lines fallback text already
// equals the en catalog value).
func TestRender_SectionTitleResolvesThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "tr"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<h2>Header</h2>") {
		t.Errorf("Turkish viewer got the literal English section title, catalog not consulted:\n%s", out)
	}
	if !strings.Contains(out, "<h2>Başlık</h2>") {
		t.Errorf("expected the Turkish translation of the Header section title, got:\n%s", out)
	}
}

// TestRender_SectionTitleWithoutCatalogKeyFallsBackToLiteral (#53): a
// section whose EntityType+Title has NO catalog entry yet must still
// render exactly as before this change — the additive-fallback
// guarantee TOrDefault gives every other caller (buildFields' field
// labels, entityDisplayName) applies here too, so introducing this
// mechanism can never itself break an existing, not-yet-translated
// form.
func TestRender_SectionTitleWithoutCatalogKeyFallsBackToLiteral(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{
		EntityType: "ZzyzxWidget",
		Fields:     []entity.Field{{Name: "x", Type: entity.FieldString}},
	}
	def := &form.Definition{
		EntityType: "ZzyzxWidget",
		Version:    1,
		Sections: []form.Section{
			{Title: "A Totally Untranslated Section", Component: form.ComponentFields,
				Fields: []form.FormField{{Name: "x", Label: "X"}}},
		},
	}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, Data{}, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "<h2>A Totally Untranslated Section</h2>") {
		t.Errorf("expected the literal title to survive unchanged when no catalog key exists, got:\n%s", buf.String())
	}
}

// TestRender_SaveActionLabelResolvesThroughCatalog (#53): the Save
// button's Label goes through the global form.action.save key exactly
// like section titles go through their per-entity key — proven the same
// non-English way, for the same tautology-avoidance reason.
func TestRender_SaveActionLabelResolvesThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "<button type=\"submit\">حفظ</button>") {
		t.Errorf("expected the Arabic translation of the Save action, got:\n%s", buf.String())
	}
}

// TestRender_NavigateActionSubstitutesRecordID (uc-infra#66): a navigate
// Route carrying "{id}" resolves against the current record's own id —
// the mechanism the "Download UBL file" action on PurchaseOrder/
// SalesOrder/CustomerInvoice relies on to link at
// /export/{EntityType}/{id}/ubl (already-shipped, #27) instead of a
// dead static route.
func TestRender_NavigateActionSubstitutesRecordID(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{EntityType: "ZzyzxWidget", Fields: []entity.Field{{Name: "x", Type: entity.FieldString}}}
	def := &form.Definition{
		EntityType: "ZzyzxWidget",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "x", Label: "X"}},
		}},
		Actions: []form.Action{
			{Label: "Download UBL file", Op: form.OpNavigate, Route: "/export/ZzyzxWidget/{id}/ubl"},
		},
	}
	data := Data{RecordID: "widget-77", Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<a href="/export/ZzyzxWidget/widget-77/ubl">Download UBL file</a>`) {
		t.Fatalf("expected the {id} placeholder substituted with the record id, got:\n%s", buf.String())
	}
}

// TestRender_NavigateActionWithIDPlaceholderOmittedOnNewRecord
// (uc-infra#66): the same action on a not-yet-saved record (RecordID ==
// "", the "new" form) has nothing to substitute — it must not render a
// dead "/export/ZzyzxWidget//ubl" link, so the whole action is omitted,
// same degrade-rather-than-dead-link reasoning form.Action.Route's own
// doc comment gives.
func TestRender_NavigateActionWithIDPlaceholderOmittedOnNewRecord(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{EntityType: "ZzyzxWidget", Fields: []entity.Field{{Name: "x", Type: entity.FieldString}}}
	def := &form.Definition{
		EntityType: "ZzyzxWidget",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "x", Label: "X"}},
		}},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
			{Label: "Download UBL file", Op: form.OpNavigate, Route: "/export/ZzyzxWidget/{id}/ubl"},
		},
	}
	data := Data{Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "Download UBL file") || strings.Contains(buf.String(), "/export/ZzyzxWidget/") {
		t.Fatalf("expected no download action on a new/unsaved record, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `<button type="submit">Save</button>`) {
		t.Fatalf("expected the unrelated Save action to still render, got:\n%s", buf.String())
	}
}

// TestRender_NavigateActionWithoutIDPlaceholderIgnoresRecordID confirms
// the {id} substitution is opt-in per Route, not a blanket behavior
// change to every navigate action: a plain static route (e.g.
// render_test.go's own "Back" fixture, "/purchase-orders") must render
// unconditionally on a new/unsaved record exactly as it always has —
// the omit-on-empty-RecordID rule above only fires when the Route
// actually contains "{id}".
func TestRender_NavigateActionWithoutIDPlaceholderIgnoresRecordID(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{EntityType: "ZzyzxWidget", Fields: []entity.Field{{Name: "x", Type: entity.FieldString}}}
	def := &form.Definition{
		EntityType: "ZzyzxWidget",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "x", Label: "X"}},
		}},
		Actions: []form.Action{
			{Label: "Back", Op: form.OpNavigate, Route: "/widgets"},
		},
	}
	data := Data{Record: map[string]any{}} // RecordID == "" — new/unsaved
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "en"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<a href="/widgets">Back</a>`) {
		t.Fatalf("expected the static-route action to render unconditionally, got:\n%s", buf.String())
	}
}

// TestRender_NonSaveActionLabelResolvesThroughCatalog (uc-infra#66)
// proves the general case ActionCatalogKey documents: any non-Save
// action's Label resolves through its own per-entity-and-action key,
// not just the ones this card happens to add. Uses the real Arabic
// translation the "Download UBL file" action shipped with (locales/
// ar.json) on the real production entity type, the same
// non-tautological proof
// TestRender_SaveActionLabelResolvesThroughCatalog gives for Save.
func TestRender_NonSaveActionLabelResolvesThroughCatalog(t *testing.T) {
	r := testRenderer(t)
	ent := &entity.Definition{EntityType: "PurchaseOrder", Fields: []entity.Field{{Name: "po_number", Type: entity.FieldString}}}
	def := &form.Definition{
		EntityType: "PurchaseOrder",
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "po_number", Label: "PO Number"}},
		}},
		Actions: []form.Action{
			{Label: "Download UBL file", Op: form.OpNavigate, Route: "/export/PurchaseOrder/{id}/ubl"},
		},
	}
	data := Data{RecordID: "po-1", Record: map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, def, ent, data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<a href="/export/PurchaseOrder/po-1/ubl">تنزيل ملف UBL</a>`) {
		t.Errorf("expected the Arabic translation of the Download UBL file action, got:\n%s", buf.String())
	}
}

// TestRender_NonSaveActionLabelWithoutCatalogKeyFallsBackToLiteral is
// TestRender_SectionTitleWithoutCatalogKeyFallsBackToLiteral's
// counterpart for actions: introducing ActionCatalogKey resolution must
// not break an existing, not-yet-translated action (e.g.
// render_test.go's own "Submit for Approval"/"Print"/"Back" fixture
// actions, none of which carry a catalog key) — same additive-fallback
// guarantee TOrDefault gives everywhere else.
func TestRender_NonSaveActionLabelWithoutCatalogKeyFallsBackToLiteral(t *testing.T) {
	r := testRenderer(t)
	data := Data{Record: map[string]any{"payment_method": "Wire"}, Children: map[string][]map[string]any{}}
	var buf strings.Builder
	if err := r.Render(&buf, purchaseOrderForm(), purchaseOrderEntity(), data, "ar"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `<a href="/purchase-orders">Back</a>`) {
		t.Errorf("expected the untranslated Back action to fall back to its literal label, got:\n%s", buf.String())
	}
}

// TestSlugifyTitle locks in the exact deterministic Title->slug
// transform: both this package's own lookup and whatever authors a
// locale JSON's keys (internal/i18n/locales/*.json) must agree on it
// byte-for-byte, since a mismatch would silently look up a key nothing
// ever populates (TOrDefault degrades to the literal Title rather than
// erroring, so a slugify drift would NOT fail loudly on its own —
// i18n_coverage_test.go is the backstop that catches it in practice, but
// this test pins the transform itself).
func TestSlugifyTitle(t *testing.T) {
	cases := []struct{ title, want string }{
		{"Header", "header"},
		{"Lead-time stages", "lead_time_stages"},
		{"Schedule and Budget", "schedule_and_budget"},
		{"  Trim Me  ", "trim_me"},
	}
	for _, c := range cases {
		if got := slugifyTitle(c.title); got != c.want {
			t.Errorf("slugifyTitle(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}
