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
