package purchasing

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// TestRequestForQuotation_ShapeAndCompositionRelationships pins #9's
// header shape: rfq_number/due_date/status_id all required, status
// managed via the rfq_status StatusType (same pattern as
// PurchaseOrder/VendorInvoice — see PurchaseOrder's own doc comment),
// and exactly two composition relationships (lines, vendors) — the
// "vendor_ids[]" array field from reference-data-model.md's aspirational
// shape has to be a real child entity in this kernel (no
// multi-reference field type exists), same reasoning as "lines" not
// being a multi-valued field either.
func TestRequestForQuotation_ShapeAndCompositionRelationships(t *testing.T) {
	def := RequestForQuotation()
	if def.Module != "purchasing" {
		t.Fatalf("expected module purchasing, got %q", def.Module)
	}
	if def.StatusTypeCode != "rfq_status" {
		t.Fatalf("expected StatusTypeCode %q, got %q", "rfq_status", def.StatusTypeCode)
	}

	numberField, ok := def.FieldByName("rfq_number")
	if !ok || numberField.Type != entity.FieldString || !numberField.Required {
		t.Fatalf("expected a Required rfq_number FieldString, got %+v", numberField)
	}
	dueField, ok := def.FieldByName("due_date")
	if !ok || dueField.Type != entity.FieldDate || !dueField.Required {
		t.Fatalf("expected a Required due_date FieldDate, got %+v", dueField)
	}
	statusField, ok := def.FieldByName("status_id")
	if !ok || statusField.Type != entity.FieldReference || statusField.Target != "Status" || !statusField.Required {
		t.Fatalf("expected a Required status_id FieldReference targeting Status, got %+v", statusField)
	}

	if len(def.Relationships) != 2 {
		t.Fatalf("expected exactly 2 relationships (lines, vendors), got %d: %+v", len(def.Relationships), def.Relationships)
	}
	byName := map[string]entity.Relationship{}
	for _, r := range def.Relationships {
		byName[r.Name] = r
	}
	lines, ok := byName["lines"]
	if !ok || lines.Kind != entity.RelationComposition || lines.Target != "RequestForQuotationLine" || lines.ParentField != "request_for_quotation_id" {
		t.Fatalf("expected a composition 'lines' relationship to RequestForQuotationLine via request_for_quotation_id, got %+v", lines)
	}
	vendors, ok := byName["vendors"]
	if !ok || vendors.Kind != entity.RelationComposition || vendors.Target != "RequestForQuotationVendor" || vendors.ParentField != "request_for_quotation_id" {
		t.Fatalf("expected a composition 'vendors' relationship to RequestForQuotationVendor via request_for_quotation_id, got %+v", vendors)
	}
}

func TestRequestForQuotation_MissingRequiredFields(t *testing.T) {
	def := RequestForQuotation()
	if err := entity.ValidateRecord(def, map[string]any{"due_date": "2026-08-01", "status_id": "status-1"}); err == nil {
		t.Fatal("expected error for missing required rfq_number")
	}
	if err := entity.ValidateRecord(def, map[string]any{"rfq_number": "RFQ-1", "status_id": "status-1"}); err == nil {
		t.Fatal("expected error for missing required due_date")
	}
	if err := entity.ValidateRecord(def, map[string]any{"rfq_number": "RFQ-1", "due_date": "2026-08-01"}); err == nil {
		t.Fatal("expected error for missing required status_id")
	}
}

// TestRequestForQuotationLine_NoUnitPrice pins the deliberate absence of
// a price field on the line — unlike POLine, an RFQ line is a REQUEST
// for pricing, not a priced commitment; the price lives on
// RequestForQuotationQuoteLine instead (this package's own doc comment
// on RequestForQuotationLine).
func TestRequestForQuotationLine_NoUnitPrice(t *testing.T) {
	def := RequestForQuotationLine()
	if _, ok := def.FieldByName("unit_price"); ok {
		t.Fatal("RequestForQuotationLine must not declare unit_price — pricing lives on RequestForQuotationQuoteLine")
	}
}

func TestRequestForQuotationLine_ReferencesParentAndItem(t *testing.T) {
	def := RequestForQuotationLine()
	parentField, ok := def.FieldByName("request_for_quotation_id")
	if !ok || parentField.Type != entity.FieldReference || parentField.Target != "RequestForQuotation" || !parentField.Required {
		t.Fatalf("expected a Required request_for_quotation_id FieldReference targeting RequestForQuotation, got %+v", parentField)
	}
	itemField, ok := def.FieldByName("item_id")
	if !ok || itemField.Type != entity.FieldReference || itemField.Target != "Item" || !itemField.Required {
		t.Fatalf("expected a Required item_id FieldReference targeting Item, got %+v", itemField)
	}
	qtyField, ok := def.FieldByName("qty")
	if !ok || qtyField.Type != entity.FieldNumber || !qtyField.Required {
		t.Fatalf("expected a Required qty FieldNumber, got %+v", qtyField)
	}
}

func TestRequestForQuotationLine_MissingRequiredQty(t *testing.T) {
	def := RequestForQuotationLine()
	data := map[string]any{"request_for_quotation_id": "rfq-1", "item_id": "item-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required qty")
	}
}

// TestRequestForQuotationVendor_ReferencesPartyDirectly is
// TestPurchaseOrder_VendorReferencesPartyDirectly's counterpart for RFQ:
// vendor_id targets Party, not a separate Vendor entity — the same
// Party-Role pattern, applied to the composition-child modeling of
// reference-data-model.md's "vendor_ids[]" shape.
func TestRequestForQuotationVendor_ReferencesPartyDirectly(t *testing.T) {
	def := RequestForQuotationVendor()
	parentField, ok := def.FieldByName("request_for_quotation_id")
	if !ok || parentField.Type != entity.FieldReference || parentField.Target != "RequestForQuotation" || !parentField.Required {
		t.Fatalf("expected a Required request_for_quotation_id FieldReference targeting RequestForQuotation, got %+v", parentField)
	}
	vendorField, ok := def.FieldByName("vendor_id")
	if !ok || vendorField.Type != entity.FieldReference || vendorField.Target != "Party" || !vendorField.Required {
		t.Fatalf("expected a Required vendor_id FieldReference targeting Party, got %+v", vendorField)
	}
}

func TestRequestForQuotationVendor_MissingRequiredFields(t *testing.T) {
	def := RequestForQuotationVendor()
	if err := entity.ValidateRecord(def, map[string]any{"vendor_id": "party-1"}); err == nil {
		t.Fatal("expected error for missing required request_for_quotation_id")
	}
	if err := entity.ValidateRecord(def, map[string]any{"request_for_quotation_id": "rfq-1"}); err == nil {
		t.Fatal("expected error for missing required vendor_id")
	}
}

// TestRequestForQuotationQuoteLine_NotAComposition confirms the
// deliberate absence of any Relationship on this entity — unlike
// RequestForQuotationLine/Vendor, a quote line is naturally keyed by TWO
// independent things (which line, which vendor), so it can't have a
// single composition parent; it's an independent entity with plain
// reference fields, the same shape VendorInvoice already uses for
// PurchaseOrder+Party (this package's own doc comment on
// RequestForQuotationQuoteLine).
func TestRequestForQuotationQuoteLine_NotAComposition(t *testing.T) {
	def := RequestForQuotationQuoteLine()
	if len(def.Relationships) != 0 {
		t.Fatalf("expected no relationships on RequestForQuotationQuoteLine, got %+v", def.Relationships)
	}
}

func TestRequestForQuotationQuoteLine_ShapeAndRequiredFields(t *testing.T) {
	def := RequestForQuotationQuoteLine()
	lineField, ok := def.FieldByName("rfq_line_id")
	if !ok || lineField.Type != entity.FieldReference || lineField.Target != "RequestForQuotationLine" || !lineField.Required {
		t.Fatalf("expected a Required rfq_line_id FieldReference targeting RequestForQuotationLine, got %+v", lineField)
	}
	vendorField, ok := def.FieldByName("vendor_id")
	if !ok || vendorField.Type != entity.FieldReference || vendorField.Target != "Party" || !vendorField.Required {
		t.Fatalf("expected a Required vendor_id FieldReference targeting Party, got %+v", vendorField)
	}
	priceField, ok := def.FieldByName("unit_price")
	if !ok || priceField.Type != entity.FieldMoney || !priceField.Required {
		t.Fatalf("expected a Required unit_price FieldMoney, got %+v", priceField)
	}
	quotedAtField, ok := def.FieldByName("quoted_at")
	if !ok || quotedAtField.Type != entity.FieldDate || quotedAtField.Required {
		t.Fatalf("expected an optional quoted_at FieldDate, got %+v", quotedAtField)
	}
}

func TestRequestForQuotationQuoteLine_MissingRequiredFields(t *testing.T) {
	def := RequestForQuotationQuoteLine()
	// unit_price is FieldMoney (uc-infra#68): its value is a whole number
	// of minor units, so $9.50 is 950 here, not 9.5.
	if err := entity.ValidateRecord(def, map[string]any{"vendor_id": "party-1", "unit_price": float64(950)}); err == nil {
		t.Fatal("expected error for missing required rfq_line_id")
	}
	if err := entity.ValidateRecord(def, map[string]any{"rfq_line_id": "line-1", "unit_price": float64(950)}); err == nil {
		t.Fatal("expected error for missing required vendor_id")
	}
	if err := entity.ValidateRecord(def, map[string]any{"rfq_line_id": "line-1", "vendor_id": "party-1"}); err == nil {
		t.Fatal("expected error for missing required unit_price")
	}
	// quoted_at is optional — omitting it must still pass.
	if err := entity.ValidateRecord(def, map[string]any{"rfq_line_id": "line-1", "vendor_id": "party-1", "unit_price": float64(950)}); err != nil {
		t.Fatalf("expected no error with quoted_at omitted, got %v", err)
	}
}

// TestRequestForQuotationQuoteLine_UnitPriceRejectsMajorUnitFraction
// (uc-infra#68) pins the actual bug fix: before this field was
// FieldMoney, a caller could submit a major-unit dollar amount like 9.5
// directly and it would validate fine as a FieldNumber — which is
// exactly how the vendor-comparison report's footer total ended up
// summing fractional floats and showing an IEEE artifact. Now that same
// submission must fail loud instead of silently being treated as "9.5
// minor units."
func TestRequestForQuotationQuoteLine_UnitPriceRejectsMajorUnitFraction(t *testing.T) {
	def := RequestForQuotationQuoteLine()
	err := entity.ValidateRecord(def, map[string]any{
		"rfq_line_id": "line-1", "vendor_id": "party-1", "unit_price": float64(9.5),
	})
	if err == nil {
		t.Fatal("expected a fractional unit_price to be rejected as not a whole number of minor units")
	}
}

// TestRequestForQuotationForm_VendorsAndLinesAreMasterDetailWithNoRollUp
// mirrors TestGoodsReceiptForm_MasterDetailTargetsGoodsReceiptLine's own
// reasoning — no RollUp on either section: there's no header total field
// for a line sum to roll into (an RFQ line has no unit_price at all).
func TestRequestForQuotationForm_VendorsAndLinesAreMasterDetailWithNoRollUp(t *testing.T) {
	f := RequestForQuotationForm()
	var vendorsSection, linesSection *form.Section
	for i := range f.Sections {
		switch f.Sections[i].Target {
		case "RequestForQuotationVendor":
			vendorsSection = &f.Sections[i]
		case "RequestForQuotationLine":
			linesSection = &f.Sections[i]
		}
	}
	if vendorsSection == nil {
		t.Fatal("expected a master-detail section targeting RequestForQuotationVendor")
	}
	if vendorsSection.Component != form.ComponentMasterDetail || vendorsSection.RollUp != "" {
		t.Fatalf("expected Vendors section to be master_detail with no RollUp, got %+v", vendorsSection)
	}
	if linesSection == nil {
		t.Fatal("expected a master-detail section targeting RequestForQuotationLine")
	}
	if linesSection.Component != form.ComponentMasterDetail || linesSection.RollUp != "" {
		t.Fatalf("expected Lines section to be master_detail with no RollUp, got %+v", linesSection)
	}
}

// TestRequestForQuotationEntitiesAreInAll confirms All() actually
// carries all four new Definitions — a Definition that exists as a Go
// function but isn't in All() is invisible to Publish (seed.go) and
// never reaches a real tenant.
//
// RequestForQuotationQuoteLine expects Version 2, not 1: its unit_price
// field's TYPE changed (FieldNumber -> FieldMoney, uc-infra#68), the
// same class of change that bumps a Definition's Version (see that
// function's own doc comment on the bump).
func TestRequestForQuotationEntitiesAreInAll(t *testing.T) {
	wantVersion := map[string]int{
		"RequestForQuotation": 1, "RequestForQuotationLine": 1,
		"RequestForQuotationVendor": 1, "RequestForQuotationQuoteLine": 2,
	}
	found := map[string]bool{}
	for _, def := range All() {
		if want, ok := wantVersion[def.EntityType]; ok {
			found[def.EntityType] = true
			if def.Version != want {
				t.Errorf("%s: expected Version %d, got %d", def.EntityType, want, def.Version)
			}
		}
	}
	for entityType := range wantVersion {
		if !found[entityType] {
			t.Errorf("%s missing from All()", entityType)
		}
	}
}

// TestRequestForQuotationFormsAreInAllForms is
// TestRequestForQuotationEntitiesAreInAll's counterpart for AllForms().
func TestRequestForQuotationFormsAreInAllForms(t *testing.T) {
	want := map[string]bool{
		"RequestForQuotation": false, "RequestForQuotationLine": false,
		"RequestForQuotationVendor": false, "RequestForQuotationQuoteLine": false,
	}
	for _, f := range AllForms() {
		if _, ok := want[f.EntityType]; ok {
			want[f.EntityType] = true
		}
	}
	for entityType, found := range want {
		if !found {
			t.Errorf("%s form missing from AllForms()", entityType)
		}
	}
}
