package hr

import (
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// TestEmployee_IsVersion2WithCostRateField (uc-infra#134) pins the exact
// shape of the version bump: cost_rate exists, is FieldMoney, has a
// Min:0 bound, and — unlike ProjectBudgetLine.planned_amount — carries
// no Default, so an omitted value stays genuinely absent rather than
// being coerced to a stored 0 (see the field's own doc comment in
// hr.go). Table-driven in the same spirit as projects_test.go's
// TestProjectBudgetLine_HasNoCurrencyOrComputedActual, but asserting the
// field DOES exist here, not that it's absent.
func TestEmployee_IsVersion2WithCostRateField(t *testing.T) {
	def := Employee()
	if def.Version != 2 {
		t.Fatalf("Employee.Version = %d, want 2 (uc-infra#134's cost_rate addition)", def.Version)
	}
	f, ok := def.FieldByName("cost_rate")
	if !ok {
		t.Fatal("expected a cost_rate field")
	}
	if f.Type != entity.FieldMoney {
		t.Fatalf("expected cost_rate to be FieldMoney, got %s", f.Type)
	}
	if f.Required {
		t.Error("cost_rate must NOT be required — most existing Employee records have no known rate")
	}
	if f.Default != nil {
		t.Errorf("cost_rate must have no Default (got %v) — a 0 default would silently price every unrated employee's hours as free", f.Default)
	}
	if f.Min == nil || *f.Min != 0 {
		t.Fatalf("expected cost_rate's Min:0 bound, got %+v", f.Min)
	}
}

// TestEmployee_CostRateAbsentValidatesFine confirms an Employee record
// that never sets cost_rate at all — the common case for every record
// that predates uc-infra#134 — still validates cleanly: cost_rate is
// optional, so its complete absence from the submitted data must not be
// an error.
func TestEmployee_CostRateAbsentValidatesFine(t *testing.T) {
	def := Employee()
	data := map[string]any{
		"employee_number": "EMP-1",
		"party_id":        "party-1",
		"hire_date":       "2026-01-01",
		"status_id":       "status-1",
	}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected no error with cost_rate absent, got %v", err)
	}
}

// TestEmployee_CostRatePresentAndValidValidates confirms a real,
// non-negative money value (whole minor units, per money.FromAny) is
// accepted.
func TestEmployee_CostRatePresentAndValidValidates(t *testing.T) {
	def := Employee()
	data := map[string]any{
		"employee_number": "EMP-1",
		"party_id":        "party-1",
		"hire_date":       "2026-01-01",
		"status_id":       "status-1",
		"cost_rate":       float64(2500), // $25.00/hr in minor units
	}
	if err := entity.ValidateRecord(def, data); err != nil {
		t.Fatalf("expected no error with a valid cost_rate, got %v", err)
	}
}

// TestEmployee_CostRateRejectsNegative confirms Min:0 actually rejects a
// negative value — same pattern purchasing_test.go uses for
// unit_price's Min:0 bound (TestPOLine_UnitPriceAndLineTotalAreFieldMoney
// and friends).
func TestEmployee_CostRateRejectsNegative(t *testing.T) {
	def := Employee()
	data := map[string]any{
		"employee_number": "EMP-1",
		"party_id":        "party-1",
		"hire_date":       "2026-01-01",
		"status_id":       "status-1",
		"cost_rate":       float64(-100),
	}
	err := entity.ValidateRecord(def, data)
	if err == nil {
		t.Fatal("expected error for negative cost_rate")
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *entity.ValidationError, got %T: %v", err, err)
	}
	if verr.Kind != entity.KindBelowMinimum {
		t.Fatalf("expected KindBelowMinimum, got %s", verr.Kind)
	}
	if verr.FieldName != "cost_rate" {
		t.Fatalf("expected the error to name cost_rate, got %q", verr.FieldName)
	}
}

// TestAllHRDefinitionsAreValid mirrors every other module's own smoke
// test (e.g. purchasing_test.go's TestAllPurchasingDefinitionsAreValid):
// every Definition this module publishes must pass entity.Definition.
// Validate() — this is what a human reviews before approving a
// published Definition (ADR-0017 §14), and a Min/Max declared on the
// wrong field type, or any other static shape mistake, must fail here
// rather than only surface at real publish time against a tenant.
func TestAllHRDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("%s: expected valid definition, got %v", def.EntityType, err)
		}
	}
}

// TestAllHRFormsAreValid is TestAllHRDefinitionsAreValid's form
// counterpart.
func TestAllHRFormsAreValid(t *testing.T) {
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: expected valid form definition, got %v", f.EntityType, err)
		}
	}
}

// TestAllHRFormFieldsExistOnTheirEntity closes the same gap
// purchasing_test.go's TestAllPurchasingFormFieldsExistOnTheirEntity
// does: form.Definition.Validate() never cross-checks a FormField.Name
// against the target entity's real fields, so a typo'd/stale field name
// (e.g. EmployeeForm's new "cost_rate" FormField not actually matching
// Employee's own field name) would pass Validate() cleanly and only
// fail at real render time.
func TestAllHRFormFieldsExistOnTheirEntity(t *testing.T) {
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
