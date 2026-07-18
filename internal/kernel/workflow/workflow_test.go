package workflow

import "testing"

// poApprovalWorkflow is the worked example from ADR-0017's Ansar-derived
// purchasing scenario, generalised: a PO over a threshold requires
// approval, then finance is notified.
func poApprovalWorkflow() *Definition {
	return &Definition{
		Name:    "po_approval",
		Version: 1,
		Trigger: Trigger{Type: TriggerOnCreate, EntityType: "PurchaseOrder"},
		Steps: []Step{
			{Kind: StepRequireApproval, Params: map[string]any{"role": "cfo"}},
			{Kind: StepNotify, Params: map[string]any{"channel": "finance"}},
		},
	}
}

func TestDefinitionValidate_Valid(t *testing.T) {
	if err := poApprovalWorkflow().Validate(); err != nil {
		t.Fatalf("expected valid workflow, got %v", err)
	}
}

func TestDefinitionValidate_MissingName(t *testing.T) {
	d := &Definition{Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestDefinitionValidate_OnCreateTriggerMissingEntityType(t *testing.T) {
	d := &Definition{Name: "x", Trigger: Trigger{Type: TriggerOnCreate}, Steps: []Step{{Kind: StepNotify}}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for on_create trigger missing entity_type")
	}
}

func TestDefinitionValidate_NoSteps(t *testing.T) {
	d := &Definition{Name: "x", Trigger: Trigger{Type: TriggerManual}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for workflow with no steps")
	}
}

func TestDefinitionValidate_RejectsArbitraryStepKind(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: "run_arbitrary_code"}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: arbitrary step kinds must be rejected")
	}
}

func TestExecute_HaltsAtApprovalStep(t *testing.T) {
	results, err := Execute(poApprovalWorkflow())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result (halted at approval), got %d: %+v", len(results), results)
	}
	if results[0].Kind != StepRequireApproval || results[0].Status != "pending" {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
}

func TestExecute_RunsAllStepsWhenNoApprovalGate(t *testing.T) {
	d := &Definition{
		Name:    "notify_only",
		Trigger: Trigger{Type: TriggerManual},
		Steps: []Step{
			{Kind: StepNotify},
			{Kind: StepNotify},
		},
	}
	results, err := Execute(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 completed steps, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != "done" {
			t.Fatalf("expected all notify steps to be done, got %+v", r)
		}
	}
}

func TestExecute_RejectsInvalidDefinition(t *testing.T) {
	d := &Definition{Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	if _, err := Execute(d); err == nil {
		t.Fatal("expected Execute to reject an invalid definition")
	}
}
