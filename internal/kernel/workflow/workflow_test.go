package workflow

import (
	"math"
	"testing"
)

// poApprovalWorkflow is the worked example from ADR-0017's purchasing
// scenario: a PO over a threshold requires approval, then finance is
// notified.
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

// TestDefinitionValidate_RequireApprovalRoleParamMustBeString is the
// regression test for a real security bug an independent review caught:
// internal/api's approval-role gate read `Params["role"].(string)` with
// the "ok" discarded, so a non-string role param (e.g. published as
// `{"role": 42}`, easy to produce since Params is a plain map[string]any
// with no compile-time shape) silently resolved to "" and was treated as
// "no restriction" — the exact fail-open bug this whole gate exists to
// prevent. Fixed by rejecting it here, at Validate (which both Enqueue
// and every resumed job's definition lookup already call), so a
// malformed role param can never reach a running job at all.
func TestDefinitionValidate_RequireApprovalRoleParamMustBeString(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"role": 42}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: a non-string role param must be rejected, not silently treated as no restriction")
	}
}

func TestDefinitionValidate_RequireApprovalRoleParamMustBeNonEmpty(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"role": ""}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: an explicit empty-string role param must be rejected")
	}
}

// TestDefinitionValidate_RequireApprovalWithNoRoleParamIsValid confirms
// the fix above doesn't overreach: omitting "role" entirely is still the
// deliberate, backward-compatible "anyone may approve" case, not an error.
func TestDefinitionValidate_RequireApprovalWithNoRoleParamIsValid(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("expected a require_approval step with no role param to stay valid, got %v", err)
	}
}

// uc-infra#64: escalation timers. escalate_after_hours/escalate_role/
// escalate_department follow the exact same fail-at-publish discipline as
// role/department above — a malformed value here must be rejected at
// Validate, not silently discovered hours later when the sweep
// (Queue.EscalateOverdueApprovals) or the approval gate first reads it.

func TestDefinitionValidate_EscalateAfterHoursRequiresEscalateRole(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"role": "cfo", "escalate_after_hours": 24.0}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: escalate_after_hours with no escalate_role must be rejected")
	}
}

// TestDefinitionValidate_EscalateAfterHoursMustBePositiveNumber's "huge"
// and "NaN" cases are the regression test for a real fail-open bug an
// independent review caught: hours<=0 alone rejects neither NaN
// (NaN<=0 is false) nor a value so large that
// Queue.EscalateOverdueApprovals' hours*time.Hour conversion to
// time.Duration overflows int64 nanoseconds and saturates to a NEGATIVE
// duration — turning an intended "essentially never" threshold into
// "escalate on the very next tick." Reviewer measured 2.6e6 hours and
// 1e9 hours both saturating to the same negative duration; "huge" here
// uses a value comfortably past maxEscalateAfterHours but still well
// under where the float64->int64 overflow itself would kick in, so this
// asserts Validate's OWN bound is doing the rejecting, not incidentally
// relying on the overflow to produce a signed value some other check
// happens to catch.
func TestDefinitionValidate_EscalateAfterHoursMustBePositiveNumber(t *testing.T) {
	cases := map[string]any{
		"non-number": "24",
		"zero":       0.0,
		"negative":   -1.0,
		"huge":       maxEscalateAfterHours + 1,
		"NaN":        math.NaN(),
		"+Inf":       math.Inf(1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			d := &Definition{
				Name:    "x",
				Trigger: Trigger{Type: TriggerManual},
				Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"escalate_after_hours": raw, "escalate_role": "cfo"}}},
			}
			if err := d.Validate(); err == nil {
				t.Fatalf("expected error: escalate_after_hours %v must be rejected", raw)
			}
		})
	}
}

func TestDefinitionValidate_EscalateDepartmentRequiresEscalateRole(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"escalate_after_hours": 24.0, "escalate_department": "department_id"}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: escalate_after_hours with no escalate_role must be rejected, even with escalate_department set")
	}
}

func TestDefinitionValidate_EscalateRoleMustBeNonEmptyString(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"escalate_after_hours": 24.0, "escalate_role": ""}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: an explicit empty-string escalate_role param must be rejected")
	}
}

func TestDefinitionValidate_EscalateDepartmentMustBeNonEmptyString(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"escalate_after_hours": 24.0, "escalate_role": "cfo", "escalate_department": ""}}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error: an explicit empty-string escalate_department param must be rejected")
	}
}

func TestDefinitionValidate_ValidEscalationCombination(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps: []Step{{Kind: StepRequireApproval, Params: map[string]any{
			"role": "finance_manager", "department": "department_id",
			"escalate_after_hours": 24.0, "escalate_role": "cfo", "escalate_department": "department_id",
		}}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("expected valid escalation step, got %v", err)
	}
}

func TestDefinitionValidate_RequireApprovalWithNoEscalateParamsIsValid(t *testing.T) {
	d := &Definition{
		Name:    "x",
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepRequireApproval, Params: map[string]any{"role": "cfo"}}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("expected a require_approval step with no escalation params to stay valid, got %v", err)
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
