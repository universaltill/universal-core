package purchasing

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

func TestAllPurchasingDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("%s: expected valid definition, got %v", def.EntityType, err)
		}
	}
}

func TestAllPurchasingFormsAreValid(t *testing.T) {
	forms := []*form.Definition{
		ItemForm(), PurchaseOrderForm(), POLineForm(), InventoryItemForm(),
		StockTransferForm(),
		GoodsReceiptForm(), GoodsReceiptLineForm(), ReorderRuleForm(),
		RequestForQuotationForm(), RequestForQuotationLineForm(),
		RequestForQuotationVendorForm(), RequestForQuotationQuoteLineForm(),
	}
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
		}
	}
}

// TestAllPurchasingFormFieldsExistOnTheirEntity closes the same gap
// foundation's own equivalent test (internal/kernel/foundation/
// foundation_test.go) does: form.Definition.Validate() never cross-
// checks a FormField.Name against the target entity's real fields, so a
// typo'd/stale field name here would pass Validate() cleanly and only
// fail at real render time (formrender.Render's "form field %q has no
// matching field on entity %q" error) the first time someone opens that
// form. Skips ComponentMasterDetail/ComponentRelatedList sections (their
// Fields is always empty — see form.Section's own doc comment; they're
// keyed by Target, a different entity type's fields entirely, not
// something to validate against this form's own EntityType).
func TestAllPurchasingFormFieldsExistOnTheirEntity(t *testing.T) {
	entityDefs := map[string]*entity.Definition{}
	for _, def := range All() {
		entityDefs[def.EntityType] = def
	}
	for _, f := range AllForms() {
		def, ok := entityDefs[f.EntityType]
		if !ok {
			t.Errorf("form %s targets an entity type not in All()", f.EntityType)
			continue
		}
		for _, section := range f.Sections {
			for _, ff := range section.Fields {
				if _, ok := def.FieldByName(ff.Name); !ok {
					t.Errorf("%s form section %q references field %q, which doesn't exist on the %s entity", f.EntityType, section.Title, ff.Name, f.EntityType)
				}
			}
		}
	}
}

func TestItem_DefaultsToStockType(t *testing.T) {
	def := Item()
	f, ok := def.FieldByName("item_type")
	if !ok {
		t.Fatal("expected an item_type field")
	}
	if f.Default != "stock" {
		t.Fatalf("expected default item_type of stock, got %v", f.Default)
	}
}

func TestItem_RejectsUnknownItemType(t *testing.T) {
	def := Item()
	data := map[string]any{"sku": "WIDGET-1", "name": "Widget", "item_type": "consumable"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for item_type not in the declared enum")
	}
}

func TestItem_MissingRequiredSKU(t *testing.T) {
	def := Item()
	data := map[string]any{"name": "Widget"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required sku")
	}
}

// TestPurchaseOrder_VendorReferencesPartyDirectly is the whole point of
// the Party-Role pattern applied to Purchasing (see this package's doc
// comment on PurchaseOrder): vendor_id targets Party, not a separate
// Vendor entity, so a company that's both a customer and a vendor is one
// Party record, not two.
func TestPurchaseOrder_VendorReferencesPartyDirectly(t *testing.T) {
	def := PurchaseOrder()
	f, ok := def.FieldByName("vendor_id")
	if !ok {
		t.Fatal("expected a vendor_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Party" {
		t.Fatalf("expected vendor_id to be a FieldReference targeting Party, got type=%s target=%s", f.Type, f.Target)
	}
}

// TestPurchaseOrder_VendorMustHoldVendorRole (uc-infra#78): vendor_id
// declares a TargetFilter requiring the referenced Party to hold the
// vendor PartyRole — a customer-only Party is no longer a valid pick.
func TestPurchaseOrder_VendorMustHoldVendorRole(t *testing.T) {
	f, ok := PurchaseOrder().FieldByName("vendor_id")
	if !ok {
		t.Fatal("expected a vendor_id field")
	}
	if len(f.TargetFilter) != 1 {
		t.Fatalf("expected exactly one target_filter condition, got %+v", f.TargetFilter)
	}
	cond := f.TargetFilter[0]
	if cond.Entity != "PartyRole" || cond.EntityField != "party_id" || cond.Field != "role_type" || cond.Value != "vendor" {
		t.Fatalf("expected a PartyRole(party_id)=role_type:vendor condition, got %+v", cond)
	}
}

// TestPurchaseOrder_StatusIsManagedByStatusType replaces the old plain
// FieldEnum "status" field's default/enum checks — PurchaseOrder is the
// first entity to opt into foundation.go's generic Status/StatusType
// pattern (see this package's doc comment on PurchaseOrder), so "is this
// a legal status" and "what does a new record start in" are now
// crud.Engine.ValidateStatusTransition's job against the
// purchase_order_status graph purchasing.PublishStatuses seeds, not
// something entity.ValidateRecord can check from the Definition alone —
// that generic mechanism is exercised in internal/kernel/crud/
// status_test.go, not duplicated here.
func TestPurchaseOrder_StatusIsManagedByStatusType(t *testing.T) {
	def := PurchaseOrder()
	if def.StatusTypeCode != "purchase_order_status" {
		t.Fatalf("expected StatusTypeCode %q, got %q", "purchase_order_status", def.StatusTypeCode)
	}
	f, ok := def.FieldByName("status_id")
	if !ok {
		t.Fatal("expected a status_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Status" || !f.Required {
		t.Fatalf("expected status_id to be a Required FieldReference targeting Status, got type=%s target=%s required=%v",
			f.Type, f.Target, f.Required)
	}
}

func TestPurchaseOrder_RequiresStatusID(t *testing.T) {
	def := PurchaseOrder()
	data := map[string]any{"po_number": "PO-TEST-1", "vendor_id": "party-1", "order_date": "2026-07-20"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required status_id")
	}
}

func TestPurchaseOrder_MissingRequiredOrderDate(t *testing.T) {
	def := PurchaseOrder()
	// po_number included so this isolates order_date specifically —
	// without it, this would "pass" even if order_date's Required flag
	// were removed, since po_number is required too.
	data := map[string]any{"po_number": "PO-TEST-1", "vendor_id": "party-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required order_date")
	}
}

func TestPurchaseOrder_RequiresPONumber(t *testing.T) {
	def := PurchaseOrder()
	f, ok := def.FieldByName("po_number")
	if !ok {
		t.Fatal("expected a po_number field")
	}
	if f.Type != entity.FieldString || !f.Required {
		t.Fatalf("expected po_number to be a required FieldString, got type=%s required=%v", f.Type, f.Required)
	}
	data := map[string]any{"vendor_id": "party-1", "order_date": "2026-07-20"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required po_number")
	}
}

func TestPOLine_ReferencesParentAndItem(t *testing.T) {
	def := POLine()
	poField, ok := def.FieldByName("purchase_order_id")
	if !ok || poField.Type != entity.FieldReference || poField.Target != "PurchaseOrder" {
		t.Fatal("expected purchase_order_id to be a FieldReference targeting PurchaseOrder")
	}
	itemField, ok := def.FieldByName("item_id")
	if !ok || itemField.Type != entity.FieldReference || itemField.Target != "Item" {
		t.Fatal("expected item_id to be a FieldReference targeting Item")
	}
}

func TestPOLine_MissingRequiredQty(t *testing.T) {
	def := POLine()
	data := map[string]any{"purchase_order_id": "po-1", "item_id": "item-1", "unit_price": float64(10)}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required qty")
	}
}

func TestPurchaseOrder_HasCompositionRelationshipToLines(t *testing.T) {
	def := PurchaseOrder()
	if len(def.Relationships) != 1 {
		t.Fatalf("expected exactly one relationship, got %d", len(def.Relationships))
	}
	rel := def.Relationships[0]
	if rel.Kind != entity.RelationComposition || rel.Target != "POLine" || rel.ParentField != "purchase_order_id" {
		t.Fatalf("expected a composition relationship to POLine via purchase_order_id, got %+v", rel)
	}
}

func TestInventoryItem_QuantitiesDefaultToZero(t *testing.T) {
	def := InventoryItem()
	for _, name := range []string{"qty_on_hand", "qty_available_to_promise"} {
		f, ok := def.FieldByName(name)
		if !ok {
			t.Fatalf("expected a %s field", name)
		}
		if f.Default != float64(0) {
			t.Fatalf("expected default %s of 0, got %v", name, f.Default)
		}
	}
}

func TestInventoryItem_MissingRequiredItemID(t *testing.T) {
	def := InventoryItem()
	data := map[string]any{"qty_on_hand": float64(5), "qty_available_to_promise": float64(5)}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required item_id")
	}
}

// TestPurchaseOrder_StagedLeadTimeStages pins #29's six staged lead-time
// timestamps: exactly these six fields carry a NotBefore chain, declared
// in reference-data-model.md §2's stage order, each a plain optional
// FieldDate (an in-flight PO has only a prefix filled in — deliberately
// censored data for #30's forecast), chained to its predecessor with
// sourced_at anchored on order_date.
func TestPurchaseOrder_StagedLeadTimeStages(t *testing.T) {
	def := PurchaseOrder()
	want := []struct{ name, notBefore string }{
		{"sourced_at", "order_date"},
		{"production_start_at", "sourced_at"},
		{"production_ready_at", "production_start_at"},
		{"shipped_at", "production_ready_at"},
		{"customs_cleared_at", "shipped_at"},
		{"received_at", "customs_cleared_at"},
	}
	// Collect every #29 chained field in declaration order — this asserts
	// both "exactly six" and "declared in reference-model order" at once.
	// #11's promised_delivery_date also carries a NotBefore chain (its own
	// test, TestPurchaseOrder_PromisedDeliveryDate, pins that separately)
	// but is deliberately excluded here BY NAME, not by membership in
	// `want`: it records a vendor COMMITMENT made at order time, not one
	// of #29's ACTUAL-outcome stages, so it isn't part of what this test
	// is pinning. Excluding by name (rather than "keep only fields also
	// in `want`") is deliberate — the latter would make the count check a
	// tautology that can never catch an UNLISTED seventh stage field
	// added later, only a deletion (independent review of #11,
	// 2026-08-01: an earlier version of this test did exactly that).
	var staged []entity.Field
	for _, f := range def.Fields {
		if f.NotBefore != "" && f.Name != "promised_delivery_date" {
			staged = append(staged, f)
		}
	}
	if len(staged) != len(want) {
		t.Fatalf("expected exactly %d NotBefore-chained stage fields, got %d: %+v", len(want), len(staged), staged)
	}
	for i, w := range want {
		f := staged[i]
		if f.Name != w.name {
			t.Fatalf("stage %d: expected %q (reference-model order), got %q", i, w.name, f.Name)
		}
		if f.Type != entity.FieldDate {
			t.Errorf("stage %q: expected FieldDate, got %q", f.Name, f.Type)
		}
		if f.Required {
			t.Errorf("stage %q must be optional — an in-flight PO has only a prefix of stages", f.Name)
		}
		if f.NotBefore != w.notBefore {
			t.Errorf("stage %q: expected NotBefore %q, got %q", f.Name, w.notBefore, f.NotBefore)
		}
	}
	// The chain passes the same publish-time validation the registry runs
	// (Definition.Validate's NotBefore second pass).
	if err := def.Validate(); err != nil {
		t.Fatalf("PurchaseOrder with staged lead-time chain must validate: %v", err)
	}
}

// TestPurchaseOrder_PromisedDeliveryDate pins #11's on-time-delivery
// prerequisite: an optional FieldDate, chained via NotBefore to
// order_date (a promise can't predate the order itself), that validates
// cleanly both with and without a value present — an old PO written
// before this field existed, or a PO whose promise was simply never
// pinned down, must still pass.
func TestPurchaseOrder_PromisedDeliveryDate(t *testing.T) {
	def := PurchaseOrder()
	f, ok := def.FieldByName("promised_delivery_date")
	if !ok {
		t.Fatal("expected a promised_delivery_date field")
	}
	if f.Type != entity.FieldDate {
		t.Fatalf("expected promised_delivery_date to be a FieldDate, got %s", f.Type)
	}
	if f.Required {
		t.Fatal("promised_delivery_date must be optional — not every PO pins one down, and old rows have none")
	}
	if f.NotBefore != "order_date" {
		t.Fatalf("expected NotBefore %q, got %q", "order_date", f.NotBefore)
	}

	withoutPromise := map[string]any{
		"po_number": "PO-TEST-1", "vendor_id": "party-1", "order_date": "2026-07-20", "status_id": "status-1",
	}
	if err := entity.ValidateRecord(def, withoutPromise); err != nil {
		t.Fatalf("PurchaseOrder without a promised_delivery_date must still validate: %v", err)
	}

	withPromise := map[string]any{
		"po_number": "PO-TEST-1", "vendor_id": "party-1", "order_date": "2026-07-20",
		"promised_delivery_date": "2026-07-27", "status_id": "status-1",
	}
	if err := entity.ValidateRecord(def, withPromise); err != nil {
		t.Fatalf("PurchaseOrder with a promised_delivery_date on/after order_date must validate: %v", err)
	}

	beforeOrder := map[string]any{
		"po_number": "PO-TEST-1", "vendor_id": "party-1", "order_date": "2026-07-20",
		"promised_delivery_date": "2026-07-19", "status_id": "status-1",
	}
	if err := entity.ValidateRecord(def, beforeOrder); err == nil {
		t.Fatal("expected error for promised_delivery_date before order_date")
	}
}

// TestPurchaseOrderForm_LeadTimeStagesSectionListsAllSixStages: the form
// (v4) surfaces #29's stages in their own "Lead-time stages" section, all
// six, in the same order the entity declares them — a stage missing here
// would make its timestamp uncapturable from the UI (the generated form
// is the only write path a demo user has).
func TestPurchaseOrderForm_LeadTimeStagesSectionListsAllSixStages(t *testing.T) {
	f := PurchaseOrderForm()
	var stagesSection *form.Section
	for i := range f.Sections {
		if f.Sections[i].Title == "Lead-time stages" {
			stagesSection = &f.Sections[i]
		}
	}
	if stagesSection == nil {
		t.Fatal("expected a section titled \"Lead-time stages\"")
	}
	if stagesSection.Component != form.ComponentFields {
		t.Fatalf("expected a plain fields section, got component %q", stagesSection.Component)
	}
	want := []string{
		"sourced_at", "production_start_at", "production_ready_at",
		"shipped_at", "customs_cleared_at", "received_at",
	}
	if len(stagesSection.Fields) != len(want) {
		t.Fatalf("expected the section to list exactly the %d stages, got %d: %+v", len(want), len(stagesSection.Fields), stagesSection.Fields)
	}
	for i, name := range want {
		if stagesSection.Fields[i].Name != name {
			t.Errorf("section field %d: expected %q, got %q", i, name, stagesSection.Fields[i].Name)
		}
	}
}

// TestReorderRule_ShapeMatchesBAContract pins #30's R2 field contract:
// item_id a Required reference to Item, reorder_point a Required
// number, safety_stock optional defaulting to 0, and
// target_lead_time_confidence a p50|p90 enum defaulting to p90 (the
// conservative choice — see the Definition's own doc comment). Also the
// deliberate ABSENCE of warehouse_id: reference-data-model.md's
// ReorderRule row includes it, but Warehouse is card #12 and adding the
// field now would reference an entity that doesn't exist.
func TestReorderRule_ShapeMatchesBAContract(t *testing.T) {
	def := ReorderRule()
	if def.Module != "purchasing" {
		t.Fatalf("expected module purchasing, got %q", def.Module)
	}

	itemField, ok := def.FieldByName("item_id")
	if !ok || itemField.Type != entity.FieldReference || itemField.Target != "Item" || !itemField.Required {
		t.Fatalf("expected a Required item_id FieldReference targeting Item, got %+v", itemField)
	}
	pointField, ok := def.FieldByName("reorder_point")
	if !ok || pointField.Type != entity.FieldNumber || !pointField.Required {
		t.Fatalf("expected a Required reorder_point FieldNumber, got %+v", pointField)
	}
	safetyField, ok := def.FieldByName("safety_stock")
	if !ok || safetyField.Type != entity.FieldNumber || safetyField.Required {
		t.Fatalf("expected an optional safety_stock FieldNumber, got %+v", safetyField)
	}
	if safetyField.Default != float64(0) {
		t.Fatalf("expected safety_stock default 0, got %v", safetyField.Default)
	}
	confField, ok := def.FieldByName("target_lead_time_confidence")
	if !ok || confField.Type != entity.FieldEnum {
		t.Fatalf("expected a target_lead_time_confidence FieldEnum, got %+v", confField)
	}
	if len(confField.EnumValues) != 2 || confField.EnumValues[0] != "p50" || confField.EnumValues[1] != "p90" {
		t.Fatalf("expected enum values [p50 p90], got %v", confField.EnumValues)
	}
	if confField.Default != "p90" {
		t.Fatalf("expected default confidence p90, got %v", confField.Default)
	}
	if _, ok := def.FieldByName("warehouse_id"); ok {
		t.Fatal("warehouse_id must stay deferred to #12 — InventoryItem is item-keyed today")
	}
}

func TestReorderRule_MissingRequiredFields(t *testing.T) {
	def := ReorderRule()
	if err := entity.ValidateRecord(def, map[string]any{"reorder_point": float64(10)}); err == nil {
		t.Fatal("expected error for missing required item_id")
	}
	if err := entity.ValidateRecord(def, map[string]any{"item_id": "item-1"}); err == nil {
		t.Fatal("expected error for missing required reorder_point")
	}
}

func TestReorderRule_RejectsUnknownConfidence(t *testing.T) {
	def := ReorderRule()
	data := map[string]any{
		"item_id": "item-1", "reorder_point": float64(10),
		"target_lead_time_confidence": "p99",
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for a confidence outside the p50|p90 enum")
	}
}

// TestGoodsReceipt_ReferencesPurchaseOrder confirms GoodsReceipt links
// back to the PurchaseOrder it's receiving against — the whole point of
// this entity existing rather than just a status flag (this package's
// own doc comment on GoodsReceipt).
func TestGoodsReceipt_ReferencesPurchaseOrder(t *testing.T) {
	def := GoodsReceipt()
	f, ok := def.FieldByName("purchase_order_id")
	if !ok || f.Type != entity.FieldReference || f.Target != "PurchaseOrder" || !f.Required {
		t.Fatalf("expected a Required purchase_order_id FieldReference targeting PurchaseOrder, got %+v", f)
	}
}

func TestGoodsReceipt_MissingRequiredReceivedDate(t *testing.T) {
	def := GoodsReceipt()
	data := map[string]any{"purchase_order_id": "po-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required received_date")
	}
}

func TestGoodsReceipt_HasCompositionRelationshipToLines(t *testing.T) {
	def := GoodsReceipt()
	if len(def.Relationships) != 1 {
		t.Fatalf("expected exactly one relationship, got %d", len(def.Relationships))
	}
	rel := def.Relationships[0]
	if rel.Kind != entity.RelationComposition || rel.Target != "GoodsReceiptLine" || rel.ParentField != "goods_receipt_id" {
		t.Fatalf("expected a composition relationship to GoodsReceiptLine via goods_receipt_id, got %+v", rel)
	}
}

// TestGoodsReceiptLine_ReferencesParentPOLineAndItem confirms the line
// carries both its own goods_receipt_id (the master-detail parent) and
// po_line_id (which specific ordered line this receipt is against) plus
// item_id directly — see this package's own doc comment on
// GoodsReceiptLine for why item_id isn't only derivable through po_line_id.
func TestGoodsReceiptLine_ReferencesParentPOLineAndItem(t *testing.T) {
	def := GoodsReceiptLine()
	for _, tc := range []struct {
		field, target string
	}{
		{"goods_receipt_id", "GoodsReceipt"},
		{"po_line_id", "POLine"},
		{"item_id", "Item"},
	} {
		f, ok := def.FieldByName(tc.field)
		if !ok || f.Type != entity.FieldReference || f.Target != tc.target || !f.Required {
			t.Fatalf("expected a Required %s FieldReference targeting %s, got %+v", tc.field, tc.target, f)
		}
	}
}

func TestGoodsReceiptLine_MissingRequiredQtyReceived(t *testing.T) {
	def := GoodsReceiptLine()
	data := map[string]any{"goods_receipt_id": "gr-1", "po_line_id": "poline-1", "item_id": "item-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required qty_received")
	}
}

// TestGoodsReceiptForm_MasterDetailTargetsGoodsReceiptLine mirrors
// TestPurchaseOrderForm_RollsUpLineTotalsIntoTotal's own reasoning, minus
// the roll-up check — GoodsReceipt has no header total field for a line
// sum to roll into (this package's own doc comment on GoodsReceiptForm).
func TestGoodsReceiptForm_MasterDetailTargetsGoodsReceiptLine(t *testing.T) {
	f := GoodsReceiptForm()
	var lines *form.Section
	for i := range f.Sections {
		if f.Sections[i].Component == form.ComponentMasterDetail {
			lines = &f.Sections[i]
		}
	}
	if lines == nil {
		t.Fatal("expected a master-detail section")
	}
	if lines.Target != "GoodsReceiptLine" {
		t.Fatalf("expected master-detail target GoodsReceiptLine, got %s", lines.Target)
	}
}

// TestPurchaseOrderForm_RollsUpLineTotalsIntoTotal confirms the form
// wires the same RollUp/RollUpTarget field names that actually exist on
// PurchaseOrder/POLine — a typo here would silently produce a form whose
// roll-up never fires (formrender's computeRollUp looks the field name
// up in each child row and just gets nothing back), not a build or
// validation failure.
func TestPurchaseOrderForm_RollsUpLineTotalsIntoTotal(t *testing.T) {
	f := PurchaseOrderForm()
	var lines *form.Section
	for i := range f.Sections {
		if f.Sections[i].Component == form.ComponentMasterDetail {
			lines = &f.Sections[i]
		}
	}
	if lines == nil {
		t.Fatal("expected a master-detail section")
	}
	if lines.Target != "POLine" {
		t.Fatalf("expected master-detail target POLine, got %s", lines.Target)
	}
	if _, ok := POLine().FieldByName(lines.RollUp); !ok {
		t.Fatalf("RollUp field %q doesn't exist on POLine", lines.RollUp)
	}
	if _, ok := PurchaseOrder().FieldByName(lines.RollUpTarget); !ok {
		t.Fatalf("RollUpTarget field %q doesn't exist on PurchaseOrder", lines.RollUpTarget)
	}
}

// Facility is the location dimension #12/R19 added. Pinned as shape
// rather than behaviour because it has no behaviour of its own — it is
// the thing InventoryItem is keyed by.
func TestFacility_Shape(t *testing.T) {
	def := Facility()
	if def.Module != "purchasing" {
		t.Errorf("Facility.Module = %q, want purchasing (ADR-0015 — relocating it to foundation later is a Definition change, but until a second module needs it, it lives here)", def.Module)
	}
	// Labelled by `name` through the kernel's default name/title
	// convention. Employee shipped without a label and rendered as a
	// raw UUID in every picker (#16); a facility picker is exactly where
	// that would bite again.
	f, ok := def.FieldByName("name")
	if !ok || f.Type != entity.FieldString || !f.Required {
		t.Errorf("Facility needs a required string `name` to be labellable; got ok=%v %+v", ok, f)
	}
	code, ok := def.FieldByName("code")
	if !ok || !code.Required {
		t.Error("Facility.code is the natural key a human quotes and the handle the backfill resolves by; it must be required")
	}
	ftype, ok := def.FieldByName("facility_type")
	if !ok || ftype.Type != entity.FieldEnum || ftype.Default != "warehouse" {
		t.Errorf("facility_type must be an enum defaulting to warehouse; got %+v", ftype)
	}
	want := map[string]bool{"warehouse": true, "store": true, "virtual": true}
	if len(ftype.EnumValues) != len(want) {
		t.Errorf("facility_type values = %v, want exactly %v (reference-data-model.md §3)", ftype.EnumValues, want)
	}
	for _, v := range ftype.EnumValues {
		if !want[v] {
			t.Errorf("unexpected facility_type %q", v)
		}
	}
	// A closed depot must stop appearing in pickers without deleting
	// the stock history pointing at it.
	if act, ok := def.FieldByName("is_active"); !ok || act.Type != entity.FieldBool || act.Default != true {
		t.Errorf("Facility.is_active must be a bool defaulting to true; got ok=%v %+v", ok, act)
	}
	if def.StatusTypeCode != "" {
		t.Errorf("Facility declares StatusTypeCode %q — open/closed is is_active, not a four-state graph (ADR-0015)", def.StatusTypeCode)
	}
}

func TestFacility_RejectsUnknownType(t *testing.T) {
	def := Facility()
	if err := entity.ValidateRecord(def, map[string]any{
		"code": "X", "name": "X", "facility_type": "spaceport",
	}); err == nil {
		t.Fatal("expected an error for a facility_type outside the declared enum")
	}
}

// The heart of #12: InventoryItem is keyed by (item, facility), and
// facility_id is REQUIRED. Optional would make "stock at no location" a
// permanently legal state and mean the entity was never really re-keyed
// (ADR-0015) — so this asserts required-ness explicitly rather than
// merely that the field exists.
func TestInventoryItem_IsKeyedByItemAndFacility(t *testing.T) {
	def := InventoryItem()
	if def.Version != 4 {
		t.Errorf("InventoryItem is v%d, want v4 — the facility bump (v3) plus qty_on_hand's Min:0 bound (v4, uc-infra#80)", def.Version)
	}
	f, ok := def.FieldByName("facility_id")
	if !ok {
		t.Fatal("InventoryItem has no facility_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Facility" {
		t.Errorf("facility_id = %+v, want a reference to Facility", f)
	}
	if !f.Required {
		t.Error("facility_id must be REQUIRED — an optional facility is the same entity with a hint attached, not a re-keyed one (ADR-0015)")
	}

	// A v2-shaped record — the thing every pre-existing row looks like
	// — must now fail validation. That failure is exactly what
	// cmd/backfill-inventory-facility exists to resolve, and if it
	// stopped failing, the backfill would silently become a no-op while
	// rows stayed unkeyed.
	legacy := map[string]any{
		"item_id":     "00000000-0000-0000-0000-000000000001",
		"qty_on_hand": float64(5), "qty_available_to_promise": float64(5),
	}
	if err := entity.ValidateRecord(def, legacy); err == nil {
		t.Fatal("a v2-shaped InventoryItem (no facility_id) must fail validation under v3")
	}
	legacy["facility_id"] = "00000000-0000-0000-0000-000000000002"
	if err := entity.ValidateRecord(def, legacy); err != nil {
		t.Fatalf("the same record with a facility_id must validate: %v", err)
	}
}

func TestStockTransfer_Shape(t *testing.T) {
	def := StockTransfer()
	if def.Module != "purchasing" {
		t.Errorf("StockTransfer.Module = %q, want purchasing", def.Module)
	}
	if def.StatusTypeCode != "stock_transfer_status" {
		t.Errorf("StockTransfer.StatusTypeCode = %q, want stock_transfer_status", def.StatusTypeCode)
	}
	// ADR-0013's closing rule: no name/title field, so it must declare
	// its own label rather than let a picker fall back to a raw uuid.
	if def.LabelField == "" {
		t.Error("StockTransfer declares no name or title field, so it must declare a LabelField (ADR-0013)")
	}
	if _, ok := def.FieldByName(def.LabelField); !ok {
		t.Errorf("StockTransfer.LabelField = %q, which is not one of its own fields", def.LabelField)
	}
	wantRequiredRefs := map[string]string{
		"item_id":          "Item",
		"from_facility_id": "Facility",
		"to_facility_id":   "Facility",
		"status_id":        "Status",
	}
	for name, target := range wantRequiredRefs {
		f, ok := def.FieldByName(name)
		if !ok || f.Type != entity.FieldReference || !f.Required || f.Target != target {
			t.Errorf("StockTransfer.%s: expected required FieldReference -> %s, got ok=%v %+v", name, target, ok, f)
		}
	}
	if f, ok := def.FieldByName("qty"); !ok || f.Type != entity.FieldNumber || !f.Required {
		t.Errorf("StockTransfer.qty: expected required FieldNumber, got ok=%v %+v", ok, f)
	}
	if f, ok := def.FieldByName("transfer_date"); !ok || f.Type != entity.FieldDate || !f.Required {
		t.Errorf("StockTransfer.transfer_date: expected required FieldDate, got ok=%v %+v", ok, f)
	}
	if f, ok := def.FieldByName("notes"); !ok || f.Type != entity.FieldString || f.Required {
		t.Errorf("StockTransfer.notes: expected optional FieldString, got ok=%v %+v", ok, f)
	}
}

// TestStockTransfer_MissingRequiredFields confirms every required field
// is actually enforced by entity.ValidateRecord, not just declared —
// TestStockTransfer_Shape only checks the Definition's own shape.
func TestStockTransfer_MissingRequiredFields(t *testing.T) {
	def := StockTransfer()
	full := map[string]any{
		"item_id":          "00000000-0000-0000-0000-000000000001",
		"from_facility_id": "00000000-0000-0000-0000-000000000002",
		"to_facility_id":   "00000000-0000-0000-0000-000000000003",
		"qty":              float64(5),
		"transfer_date":    "2026-08-01",
		"status_id":        "00000000-0000-0000-0000-000000000004",
	}
	if err := entity.ValidateRecord(def, full); err != nil {
		t.Fatalf("a fully-populated StockTransfer must validate, got: %v", err)
	}
	for _, missing := range []string{"item_id", "from_facility_id", "to_facility_id", "qty", "transfer_date", "status_id"} {
		partial := map[string]any{}
		for k, v := range full {
			if k != missing {
				partial[k] = v
			}
		}
		if err := entity.ValidateRecord(def, partial); err == nil {
			t.Errorf("expected validation to fail with %q missing", missing)
		}
	}
	// notes is optional — omitting it alone must still validate.
	withoutNotes := map[string]any{}
	for k, v := range full {
		withoutNotes[k] = v
	}
	if err := entity.ValidateRecord(def, withoutNotes); err != nil {
		t.Errorf("notes is optional; omitting it alone must still validate, got: %v", err)
	}
}
