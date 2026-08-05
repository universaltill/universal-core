package projects

import (
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// TestTask_AssigneeMustHoldEmployeeRole (uc-infra#78): assignee_id
// declares a TargetFilter requiring the referenced Party to hold the
// employee PartyRole — a pure vendor/customer Party is no longer a
// valid pick, closing the gap this package's own doc comment used to
// state plainly was unenforced.
func TestTask_AssigneeMustHoldEmployeeRole(t *testing.T) {
	f, ok := Task().FieldByName("assignee_id")
	if !ok {
		t.Fatal("expected an assignee_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Party" {
		t.Fatalf("expected assignee_id to be a FieldReference targeting Party, got type=%s target=%s", f.Type, f.Target)
	}
	if len(f.TargetFilter) != 1 {
		t.Fatalf("expected exactly one target_filter condition, got %+v", f.TargetFilter)
	}
	cond := f.TargetFilter[0]
	if cond.Entity != "PartyRole" || cond.EntityField != "party_id" || cond.Field != "role_type" || cond.Value != "employee" {
		t.Fatalf("expected a PartyRole(party_id)=role_type:employee condition, got %+v", cond)
	}
}

// TestTask_ParentTaskMustShareProject (uc-infra#78): parent_task_id
// declares MustMatchParentField: "project_id" — a parent task in a
// different project is no longer accepted, closing the gap this
// package's own doc comment used to state was latent (only the cycle
// guard applied).
func TestTask_ParentTaskMustShareProject(t *testing.T) {
	f, ok := Task().FieldByName("parent_task_id")
	if !ok {
		t.Fatal("expected a parent_task_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Task" {
		t.Fatalf("expected parent_task_id to be a FieldReference targeting Task, got type=%s target=%s", f.Type, f.Target)
	}
	if f.MustMatchParentField != "project_id" {
		t.Fatalf("expected must_match_parent_field %q, got %q", "project_id", f.MustMatchParentField)
	}
}

// TestTimeEntry_EmployeeMustHoldEmployeeRole (uc-infra#78): employee_id
// declares the same employee-role TargetFilter as Task.assignee_id.
func TestTimeEntry_EmployeeMustHoldEmployeeRole(t *testing.T) {
	f, ok := TimeEntry().FieldByName("employee_id")
	if !ok {
		t.Fatal("expected an employee_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Party" {
		t.Fatalf("expected employee_id to be a FieldReference targeting Party, got type=%s target=%s", f.Type, f.Target)
	}
	if len(f.TargetFilter) != 1 {
		t.Fatalf("expected exactly one target_filter condition, got %+v", f.TargetFilter)
	}
	cond := f.TargetFilter[0]
	if cond.Entity != "PartyRole" || cond.EntityField != "party_id" || cond.Field != "role_type" || cond.Value != "employee" {
		t.Fatalf("expected a PartyRole(party_id)=role_type:employee condition, got %+v", cond)
	}
}

// TestAllProjectsDefinitionsAreValid mirrors the same sanity check every
// other module's own test suite runs (see sales/purchasing's
// TestAll*DefinitionsAreValid) — this package didn't have one yet.
func TestAllProjectsDefinitionsAreValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Errorf("%s: %v", def.EntityType, err)
		}
	}
}

// TestProject_HasBudgetLinesComposition (uc-infra#79): Project declares
// a composition relationship to ProjectBudgetLine, the same shape
// PurchaseOrder->POLine uses, so the master-detail section in
// ProjectForm has something to render against.
func TestProject_HasBudgetLinesComposition(t *testing.T) {
	rels := Project().Relationships
	var rel *entity.Relationship
	for i, r := range rels {
		if r.Name == "budget_lines" {
			rel = &rels[i]
		}
	}
	if rel == nil {
		t.Fatal("expected Project to declare a budget_lines relationship")
	}
	if rel.Kind != entity.RelationComposition {
		t.Errorf("budget_lines kind = %q, want composition", rel.Kind)
	}
	if rel.Target != "ProjectBudgetLine" {
		t.Errorf("budget_lines target = %q, want ProjectBudgetLine", rel.Target)
	}
	if rel.ParentField != "project_id" {
		t.Errorf("budget_lines parent_field = %q, want project_id", rel.ParentField)
	}
}

// TestProjectBudgetLine_CategoryIsAClosedEnum (uc-infra#79): category
// must be a fixed EnumValues list, not free text — see the field's own
// doc comment for why (a reporting/grouping dimension only works
// un-normalized if the values are already closed).
func TestProjectBudgetLine_CategoryIsAClosedEnum(t *testing.T) {
	f, ok := ProjectBudgetLine().FieldByName("category")
	if !ok {
		t.Fatal("expected a category field")
	}
	if f.Type != entity.FieldEnum {
		t.Fatalf("category type = %q, want enum", f.Type)
	}
	if !f.Required {
		t.Error("category should be required — an unclassified budget line defeats the point of the field")
	}
	want := []string{"labour", "materials", "travel", "equipment", "other"}
	if len(f.EnumValues) != len(want) {
		t.Fatalf("category enum values = %v, want %v", f.EnumValues, want)
	}
	for i, v := range want {
		if f.EnumValues[i] != v {
			t.Errorf("category enum value[%d] = %q, want %q", i, f.EnumValues[i], v)
		}
	}
	if f.Default != "labour" {
		t.Errorf("category default = %v, want %q", f.Default, "labour")
	}
}

// TestProjectBudgetLine_HasNoCurrencyOrComputedActual (uc-infra#79)
// pins the two deliberate omissions the package doc comment explains:
// no currency_id (inherits Project.currency_id, same as every other
// composition-child line item — POLine/SOLine/GoodsReceiptLine all
// have none either), and no computed "actual" field (nothing in this
// kernel can honestly price one yet). A future change adding either
// should have to touch this test, not silently drift past it.
func TestProjectBudgetLine_HasNoCurrencyOrComputedActual(t *testing.T) {
	def := ProjectBudgetLine()
	for _, name := range []string{"currency_id", "actual", "actual_amount", "computed_actual"} {
		if _, ok := def.FieldByName(name); ok {
			t.Errorf("ProjectBudgetLine unexpectedly has a %q field — update the package doc comment if this is now intentional", name)
		}
	}
	want := []string{"project_id", "category", "planned_amount"}
	if len(def.Fields) != len(want) {
		t.Fatalf("ProjectBudgetLine fields = %v, want exactly %v", fieldNames(def.Fields), want)
	}
	for i, name := range want {
		if def.Fields[i].Name != name {
			t.Errorf("field[%d] = %q, want %q", i, def.Fields[i].Name, name)
		}
	}
}

func fieldNames(fields []entity.Field) []string {
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.Name
	}
	return names
}

// TestProjectBudgetLine_ProjectIDRequiredAndTargetsProject (uc-infra#79):
// the parent-pointing field POLine's own pattern requires — Required so
// an orphaned budget line can't be created, and targeting Project so
// the composition relationship's ParentField actually resolves.
func TestProjectBudgetLine_ProjectIDRequiredAndTargetsProject(t *testing.T) {
	f, ok := ProjectBudgetLine().FieldByName("project_id")
	if !ok {
		t.Fatal("expected a project_id field")
	}
	if f.Type != entity.FieldReference || f.Target != "Project" {
		t.Fatalf("expected project_id to be a FieldReference targeting Project, got type=%s target=%s", f.Type, f.Target)
	}
	if !f.Required {
		t.Error("project_id should be required — a budget line has no meaning detached from a project")
	}
}
