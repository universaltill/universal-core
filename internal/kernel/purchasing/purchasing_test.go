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
	forms := []*form.Definition{ItemForm(), PurchaseOrderForm(), POLineForm(), InventoryItemForm()}
	for _, f := range forms {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
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

func TestPurchaseOrder_DefaultsToDraftStatus(t *testing.T) {
	def := PurchaseOrder()
	f, ok := def.FieldByName("status")
	if !ok {
		t.Fatal("expected a status field")
	}
	if f.Default != "draft" {
		t.Fatalf("expected default status of draft, got %v", f.Default)
	}
}

func TestPurchaseOrder_RejectsUnknownStatus(t *testing.T) {
	def := PurchaseOrder()
	data := map[string]any{"vendor_id": "party-1", "order_date": "2026-07-20", "status": "shipped"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for status not in the declared enum")
	}
}

func TestPurchaseOrder_MissingRequiredOrderDate(t *testing.T) {
	def := PurchaseOrder()
	data := map[string]any{"vendor_id": "party-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required order_date")
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
