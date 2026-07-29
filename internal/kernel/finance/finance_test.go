package finance

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestAllFinanceDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("%s: expected valid definition, got %v", def.EntityType, err)
		}
	}
}

func TestAllFinanceFormsAreValid(t *testing.T) {
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
		}
	}
}

// TestAllFinanceFormFieldsExistOnTheirEntity closes the same gap
// purchasing's/sales' equivalent test does: form.Definition.Validate()
// never cross-checks a FormField.Name against the target entity's real
// fields.
func TestAllFinanceFormFieldsExistOnTheirEntity(t *testing.T) {
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

// TestAccount_ParentAccountIsASelfReference is the whole point of the
// hierarchical chart-of-accounts shape (this package's own doc comment):
// parent_account_id targets Account itself, not a separate parent entity.
func TestAccount_ParentAccountIsASelfReference(t *testing.T) {
	def := Account()
	f, ok := def.FieldByName("parent_account_id")
	if !ok {
		t.Fatal("expected a parent_account_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Account" {
		t.Fatalf("expected parent_account_id to be a FieldReference targeting Account, got type=%s target=%s", f.Type, f.Target)
	}
	if f.Required {
		t.Fatal("expected parent_account_id to be optional (top-level accounts have no parent)")
	}
}

func TestAccount_TypeIsARequiredClosedEnum(t *testing.T) {
	def := Account()
	f, ok := def.FieldByName("type")
	if !ok {
		t.Fatal("expected a type field")
	}
	if f.Type != entity.FieldEnum || !f.Required {
		t.Fatalf("expected type to be a Required FieldEnum, got type=%s required=%v", f.Type, f.Required)
	}
	want := []string{"asset", "liability", "equity", "income", "expense"}
	if len(f.EnumValues) != len(want) {
		t.Fatalf("expected enum values %v, got %v", want, f.EnumValues)
	}
	for i, v := range want {
		if f.EnumValues[i] != v {
			t.Fatalf("expected enum values %v, got %v", want, f.EnumValues)
		}
	}
}

func TestAccount_MissingRequiredCode(t *testing.T) {
	def := Account()
	data := map[string]any{"name": "Cash", "type": "asset"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required code")
	}
}

func TestAccount_HasNoRelationships(t *testing.T) {
	// parent_account_id is a plain reference Field, not a
	// entity.Relationship — self-reference hierarchy here works the same
	// way ProductCategory's parent_category does elsewhere in this
	// codebase (reference-data-model.md), not composition/master-detail.
	def := Account()
	if len(def.Relationships) != 0 {
		t.Fatalf("expected no Relationships on Account (parent_account_id is a plain reference field), got %+v", def.Relationships)
	}
}

func TestFiscalYear_StatusDefaultsToOpen(t *testing.T) {
	def := FiscalYear()
	f, ok := def.FieldByName("status")
	if !ok {
		t.Fatal("expected a status field")
	}
	if f.Default != "open" {
		t.Fatalf("expected status to default to %q, got %v", "open", f.Default)
	}
}

func TestFiscalYear_MissingRequiredDates(t *testing.T) {
	def := FiscalYear()
	data := map[string]any{"name": "FY2026", "status": "open"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required start_date/end_date")
	}
}

func TestPeriod_ReferencesFiscalYear(t *testing.T) {
	def := Period()
	f, ok := def.FieldByName("fiscal_year_id")
	if !ok || f.Type != entity.FieldReference || f.Target != "FiscalYear" || !f.Required {
		t.Fatalf("expected a Required fiscal_year_id FieldReference targeting FiscalYear, got %+v", f)
	}
}

// TestPeriod_StatusHasThreeStatesUnlikeFiscalYear confirms Period's
// status enum has the extra "locked" state reference-data-model.md's own
// field list names (this package's own doc comment explains why it isn't
// yet enforced anywhere).
func TestPeriod_StatusHasThreeStatesUnlikeFiscalYear(t *testing.T) {
	def := Period()
	f, ok := def.FieldByName("status")
	if !ok {
		t.Fatal("expected a status field")
	}
	want := []string{"open", "closed", "locked"}
	if len(f.EnumValues) != len(want) {
		t.Fatalf("expected enum values %v, got %v", want, f.EnumValues)
	}
	for i, v := range want {
		if f.EnumValues[i] != v {
			t.Fatalf("expected enum values %v, got %v", want, f.EnumValues)
		}
	}
}

func TestTaxCode_RequiresRateAndTaxType(t *testing.T) {
	def := TaxCode()
	data := map[string]any{"code": "VAT5", "name": "VAT Standard 5%"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required rate/tax_type")
	}
	f, ok := def.FieldByName("tax_type")
	if !ok || f.Type != entity.FieldEnum {
		t.Fatal("expected tax_type to be a FieldEnum")
	}
	want := []string{"vat", "withholding", "sales"}
	if len(f.EnumValues) != len(want) {
		t.Fatalf("expected enum values %v, got %v", want, f.EnumValues)
	}
}

func TestTaxCode_JurisdictionIsOptional(t *testing.T) {
	def := TaxCode()
	f, ok := def.FieldByName("jurisdiction")
	if !ok {
		t.Fatal("expected a jurisdiction field")
	}
	if f.Required {
		t.Fatal("expected jurisdiction to be optional — not every tax code is jurisdiction-specific")
	}
}

func TestCostCenter_RequiresCodeAndName(t *testing.T) {
	def := CostCenter()
	data := map[string]any{"type": "operational"}
	if err := entity.ValidateRecord(def, data); err == nil {
		t.Fatal("expected error for missing required code/name")
	}
}
