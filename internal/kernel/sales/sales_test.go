package sales

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

func TestAllSalesDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("%s: expected valid definition, got %v", def.EntityType, err)
		}
	}
}

func TestAllSalesFormsAreValid(t *testing.T) {
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
		}
	}
}

// TestAllSalesFormFieldsExistOnTheirEntity closes the same gap
// purchasing's equivalent test does: form.Definition.Validate() never
// cross-checks a FormField.Name against the target entity's real fields.
func TestAllSalesFormFieldsExistOnTheirEntity(t *testing.T) {
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

// TestSalesOrder_CustomerReferencesPartyDirectly is the whole point of
// the Party-Role pattern applied to Sales (this package's own doc
// comment on SalesOrder): customer_id targets Party, not a separate
// Customer entity.
func TestSalesOrder_CustomerReferencesPartyDirectly(t *testing.T) {
	def := SalesOrder()
	f, ok := def.FieldByName("customer_id")
	if !ok {
		t.Fatal("expected a customer_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Party" {
		t.Fatalf("expected customer_id to be a FieldReference targeting Party, got type=%s target=%s", f.Type, f.Target)
	}
}

func TestSalesOrder_StatusIsManagedByStatusType(t *testing.T) {
	def := SalesOrder()
	if def.StatusTypeCode != "sales_order_status" {
		t.Fatalf("expected StatusTypeCode %q, got %q", "sales_order_status", def.StatusTypeCode)
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

func TestSalesOrder_RequiresSONumber(t *testing.T) {
	def := SalesOrder()
	f, ok := def.FieldByName("so_number")
	if !ok {
		t.Fatal("expected a so_number field")
	}
	if f.Type != entity.FieldString || !f.Required {
		t.Fatalf("expected so_number to be a required FieldString, got type=%s required=%v", f.Type, f.Required)
	}
	data := map[string]any{"customer_id": "party-1", "order_date": "2026-07-20"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required so_number")
	}
}

func TestSalesOrder_MissingRequiredOrderDate(t *testing.T) {
	def := SalesOrder()
	data := map[string]any{"so_number": "SO-TEST-1", "customer_id": "party-1"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required order_date")
	}
}

func TestSalesOrder_HasCompositionRelationshipToLines(t *testing.T) {
	def := SalesOrder()
	if len(def.Relationships) != 1 {
		t.Fatalf("expected exactly one relationship, got %d", len(def.Relationships))
	}
	rel := def.Relationships[0]
	if rel.Kind != entity.RelationComposition || rel.Target != "SOLine" || rel.ParentField != "sales_order_id" {
		t.Fatalf("expected a composition relationship to SOLine via sales_order_id, got %+v", rel)
	}
}

func TestSOLine_ReferencesParentAndItem(t *testing.T) {
	def := SOLine()
	soField, ok := def.FieldByName("sales_order_id")
	if !ok || soField.Type != entity.FieldReference || soField.Target != "SalesOrder" {
		t.Fatal("expected sales_order_id to be a FieldReference targeting SalesOrder")
	}
	itemField, ok := def.FieldByName("item_id")
	if !ok || itemField.Type != entity.FieldReference || itemField.Target != "Item" {
		t.Fatal("expected item_id to be a FieldReference targeting Item")
	}
}

func TestSOLine_MissingRequiredQty(t *testing.T) {
	def := SOLine()
	data := map[string]any{"sales_order_id": "so-1", "item_id": "item-1", "unit_price": float64(10)}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required qty")
	}
}

// TestCustomerInvoice_ReferencesSalesOrderAndCustomer confirms
// CustomerInvoice carries both its parent sales_order_id and its own
// customer_id directly — this package's own doc comment on
// CustomerInvoice explains why customer_id isn't only derivable through
// sales_order_id.
func TestCustomerInvoice_ReferencesSalesOrderAndCustomer(t *testing.T) {
	def := CustomerInvoice()
	for _, tc := range []struct {
		field, target string
	}{
		{"sales_order_id", "SalesOrder"},
		{"customer_id", "Party"},
	} {
		f, ok := def.FieldByName(tc.field)
		if !ok || f.Type != entity.FieldReference || f.Target != tc.target || !f.Required {
			t.Fatalf("expected a Required %s FieldReference targeting %s, got %+v", tc.field, tc.target, f)
		}
	}
}

func TestCustomerInvoice_StatusIsManagedByStatusType(t *testing.T) {
	def := CustomerInvoice()
	if def.StatusTypeCode != "customer_invoice_status" {
		t.Fatalf("expected StatusTypeCode %q, got %q", "customer_invoice_status", def.StatusTypeCode)
	}
	f, ok := def.FieldByName("status_id")
	if !ok || f.Type != entity.FieldReference || f.Target != "Status" || !f.Required {
		t.Fatalf("expected a Required status_id FieldReference targeting Status, got %+v", f)
	}
}

func TestCustomerInvoice_MissingRequiredInvoiceNumber(t *testing.T) {
	def := CustomerInvoice()
	data := map[string]any{
		"sales_order_id": "so-1", "customer_id": "party-1", "invoice_date": "2026-07-20",
		"status_id": "status-1",
	}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required invoice_number")
	}
}

// TestCustomerInvoice_HasNoLineRelationship confirms this first slice
// deliberately has no InvoiceLine composition child (this package's own
// doc comment on CustomerInvoice) — a regression here would mean someone
// added a Relationship without also adding the corresponding form
// section and entity, an inconsistency TestAllSalesFormFieldsExistOnTheirEntity
// wouldn't catch (Relationships aren't FormFields).
func TestCustomerInvoice_HasNoLineRelationship(t *testing.T) {
	def := CustomerInvoice()
	if len(def.Relationships) != 0 {
		t.Fatalf("expected no relationships on CustomerInvoice yet, got %+v", def.Relationships)
	}
}

func TestSalesOrderForm_RollsUpLineTotalsIntoTotal(t *testing.T) {
	f := SalesOrderForm()
	var lines *form.Section
	for i := range f.Sections {
		if f.Sections[i].Component == form.ComponentMasterDetail {
			lines = &f.Sections[i]
		}
	}
	if lines == nil {
		t.Fatal("expected a master-detail section")
	}
	if lines.Target != "SOLine" {
		t.Fatalf("expected master-detail target SOLine, got %s", lines.Target)
	}
	if _, ok := SOLine().FieldByName(lines.RollUp); !ok {
		t.Fatalf("RollUp field %q doesn't exist on SOLine", lines.RollUp)
	}
	if _, ok := SalesOrder().FieldByName(lines.RollUpTarget); !ok {
		t.Fatalf("RollUpTarget field %q doesn't exist on SalesOrder", lines.RollUpTarget)
	}
}

func TestCustomerInvoiceForm_HasNoMasterDetailSection(t *testing.T) {
	for _, s := range CustomerInvoiceForm().Sections {
		if s.Component == form.ComponentMasterDetail {
			t.Fatalf("expected no master-detail section on CustomerInvoiceForm, found one targeting %s", s.Target)
		}
	}
}
