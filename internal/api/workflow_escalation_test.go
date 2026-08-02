package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// Escalation timers (uc-infra#64) are the third axis userMayApprove
// resolves, alongside the role/department check (TestAPI_
// DepartmentScopedApproval) and delegation (workflow_delegation_test.go):
// once Queue.EscalateOverdueApprovals has flipped a job's Escalated flag,
// a holder of the step's escalate_role may ALSO approve it — additive to,
// never a replacement for, the original role/department requirement.
// These tests set Escalated directly via SQL rather than driving the real
// sweep (covered end-to-end by internal/kernel/workflow's own
// TestQueue_EscalateOverdueApprovals_* tests) — this file's job is
// eligibility given an already-escalated job, the same separation of
// concerns TestAPI_DepartmentScopedApproval already draws around
// approvalRoleFor/userMeetsApproval.

// escalationFixture publishes a workflow whose require_approval step
// demands "cfo" (escalating to "vp" after 1 hour), grants "cfo" to
// user-cfo and "vp" to user-vp, and enqueues+drives one job to
// waiting_approval. Returns the job id and a setEscalated helper.
func escalationFixture(t *testing.T) (tenantID string, db *sql.DB, mux *http.ServeMux, jobID string) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db = newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	eng := crud.NewEngine(db)
	cfo, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "cfo", "name": "CFO"}, humanActor())
	if err != nil {
		t.Fatalf("role cfo: %v", err)
	}
	vp, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "vp", "name": "VP"}, humanActor())
	if err != nil {
		t.Fatalf("role vp: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-cfo", "role_id": cfo.ID}, humanActor()); err != nil {
		t.Fatalf("grant user-cfo: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-vp", "role_id": vp.ID}, humanActor()); err != nil {
		t.Fatalf("grant user-vp: %v", err)
	}

	def := &workflow.Definition{
		Name: "escalating_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{{Kind: workflow.StepRequireApproval, Params: map[string]any{
			"role": "cfo", "escalate_after_hours": 1.0, "escalate_role": "vp",
		}}},
	}
	publishWorkflow(t, db, def)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "Vendor", "11111111-1111-1111-1111-111111111111", humanActor())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("process: %v", err)
	}

	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, db, mux, job.ID
}

func setEscalated(t *testing.T, db *sql.DB, jobID string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE workflow_jobs SET escalated = true WHERE id = $1`, jobID); err != nil {
		t.Fatalf("set escalated: %v", err)
	}
}

// TestAPI_ApproveWorkflowJob_EscalateRoleHolderDeniedBeforeEscalation
// confirms escalate_role grants nothing until the job has actually
// escalated — a step naming escalate_role does not mean "either role may
// approve from the start."
func TestAPI_ApproveWorkflowJob_EscalateRoleHolderDeniedBeforeEscalation(t *testing.T) {
	tenantID, _, mux, jobID := escalationFixture(t)
	if code := approveAs(mux, tenantID, jobID, "user-vp"); code != http.StatusForbidden {
		t.Fatalf("user-vp before escalation: expected 403, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_EscalateRoleHolderCanApproveAfterEscalation
// is the positive case: once Escalated is set, the escalate_role holder
// gains eligibility.
func TestAPI_ApproveWorkflowJob_EscalateRoleHolderCanApproveAfterEscalation(t *testing.T) {
	tenantID, db, mux, jobID := escalationFixture(t)
	setEscalated(t, db, jobID)

	if code := approveAs(mux, tenantID, jobID, "user-vp"); code != http.StatusOK {
		t.Fatalf("user-vp after escalation: expected 200, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_OriginalRoleHolderStillApprovesAfterEscalation
// is the "additive, not replaced" guarantee: escalating a job must never
// revoke the original approver's standing.
func TestAPI_ApproveWorkflowJob_OriginalRoleHolderStillApprovesAfterEscalation(t *testing.T) {
	tenantID, db, mux, jobID := escalationFixture(t)
	setEscalated(t, db, jobID)

	if code := approveAs(mux, tenantID, jobID, "user-cfo"); code != http.StatusOK {
		t.Fatalf("user-cfo (original role) after escalation: expected 200, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_NeitherRoleHolderStillDeniedAfterEscalation
// confirms escalation widens eligibility, it does not open the gate
// entirely — a user holding neither role is still denied.
func TestAPI_ApproveWorkflowJob_NeitherRoleHolderStillDeniedAfterEscalation(t *testing.T) {
	tenantID, db, mux, jobID := escalationFixture(t)
	setEscalated(t, db, jobID)

	if code := approveAs(mux, tenantID, jobID, "user-other"); code != http.StatusForbidden {
		t.Fatalf("user-other (neither role) after escalation: expected 403, got %d", code)
	}
}

// TestAPI_WorkflowInbox_ShowsApproveViaEscalation confirms the HTML inbox
// and the enforcement gate agree once a job has escalated — the same
// no-drift property TestWorkflowInbox_ShowsApproveForRoleTheViewerHolds
// and TestWorkflowInbox_ShowsApproveViaDelegation already establish for
// the role and delegation axes.
func TestAPI_WorkflowInbox_ShowsApproveViaEscalation(t *testing.T) {
	tenantID, db, mux, jobID := escalationFixture(t)
	setEscalated(t, db, jobID)

	rec := getAs(t, mux, "/workflow-jobs", tenantID, "user-vp")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workflow-jobs: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/api/workflow-jobs/"+jobID+"/approve\"") {
		t.Fatalf("expected an Approve control for job %s once escalated, body:\n%s", jobID, body)
	}
}

// TestAPI_ListWorkflowJobs_ReflectsEscalatedEligibility confirms the JSON
// list endpoint's per-row can_approve field agrees with the enforcement
// gate too — the third of the three surfaces userMayApprove exists to
// keep from disagreeing.
func TestAPI_ListWorkflowJobs_ReflectsEscalatedEligibility(t *testing.T) {
	tenantID, db, mux, jobID := escalationFixture(t)
	setEscalated(t, db, jobID)

	rec := getAs(t, mux, "/api/workflow-jobs?status=waiting_approval", tenantID, "user-vp")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/workflow-jobs: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []workflowJobResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v: %s", err, rec.Body.String())
	}
	var found bool
	for _, j := range env.Data {
		if j.ID != jobID {
			continue
		}
		found = true
		if j.CanApprove == nil || !*j.CanApprove {
			t.Fatalf("expected job %s to report can_approve=true for user-vp once escalated, got %+v", jobID, j)
		}
	}
	if !found {
		t.Fatalf("expected job %s in the waiting_approval listing", jobID)
	}
}

// TestAPI_ApproveWorkflowJob_EscalatedWithNoEscalateRoleConfigured_DeniedNotFailOpen
// is the regression test for userMayApprove's own defensive guard: a job
// somehow marked Escalated whose step names no escalate_role at all (a
// Definition drift Validate() should make impossible, but the code does
// not assume that promise holds — see the ADR-0006 addendum's own
// writeup of this). escalationRoleFor resolves an empty
// approvalRequirement{} in that case, and userMeetsApproval treats an
// empty Role as "anyone may approve" by design (the ordinary
// unrestricted-step default) — so userMayApprove must short-circuit
// before ever calling userMeetsApproval with it, or this would be a real
// fail-open bug, not a hypothetical one.
func TestAPI_ApproveWorkflowJob_EscalatedWithNoEscalateRoleConfigured_DeniedNotFailOpen(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	// An ordinary gated step — role required, no escalation params at all.
	def := gatedApprovalWorkflow("no_escalation_configured", "cfo")
	publishWorkflow(t, db, def)

	eng := crud.NewEngine(db)
	cfo, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "cfo", "name": "CFO"}, humanActor())
	if err != nil {
		t.Fatalf("role cfo: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-cfo", "role_id": cfo.ID}, humanActor()); err != nil {
		t.Fatalf("grant user-cfo: %v", err)
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "Vendor", "22222222-2222-2222-2222-222222222222", humanActor())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("process: %v", err)
	}
	// Simulates drift: Escalated set directly (never possible via the real
	// sweep, which only ever sets it for a step that named escalate_role —
	// Validate() guarantees that pairing at publish time) rather than
	// through Queue.EscalateOverdueApprovals.
	setEscalated(t, db, job.ID)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A user holding NEITHER "cfo" nor any escalate_role (there is none)
	// must still be denied — an empty escalation requirement must never
	// be read as "no restriction."
	if code := approveAs(mux, tenantID, job.ID, "user-nobody"); code != http.StatusForbidden {
		t.Fatalf("user with no role approving an escalated-but-unconfigured job: expected 403, got %d", code)
	}
	// The original role holder is unaffected by any of this.
	if code := approveAs(mux, tenantID, job.ID, "user-cfo"); code != http.StatusOK {
		t.Fatalf("user-cfo (original role) still approving: expected 200, got %d", code)
	}
}

// TestAPI_EscalateDepartment_UnsetFieldRoutesToNobody confirms
// escalate_department follows the same fail-closed semantics as the
// primary department param (TestAPI_DepartmentScopedApproval_
// UnsetFieldRoutesToNobody) — an unset routing field denies everyone,
// including the escalate_role holder, rather than falling open.
func TestAPI_EscalateDepartment_UnsetFieldRoutesToNobody(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	entDef, formDef := requisitionEntityAndForm()
	publishEntityAndForm(t, db, entDef, formDef)

	eng := crud.NewEngine(db)
	vp, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "vp", "name": "VP"}, humanActor())
	if err != nil {
		t.Fatalf("role vp: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-vp", "role_id": vp.ID, "department_id": "dept-A"}, humanActor()); err != nil {
		t.Fatalf("grant user-vp in dept-A: %v", err)
	}

	// The record names NO department — department_id left unset.
	rec, err := eng.Create(ctx, entDef, map[string]any{"title": "Escalated purchase"}, humanActor())
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	def := &workflow.Definition{
		Name: "escalating_dept_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{{Kind: workflow.StepRequireApproval, Params: map[string]any{
			"role": "cfo", "escalate_after_hours": 1.0,
			"escalate_role": "vp", "escalate_department": "department_id",
		}}},
	}
	publishWorkflow(t, db, def)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "Requisition", rec.ID, humanActor())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("process: %v", err)
	}
	setEscalated(t, db, job.ID)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// user-vp holds "vp" in dept-A, but the record names no department at
	// all — routes to nobody, same as the primary department param.
	if code := approveAs(mux, tenantID, job.ID, "user-vp"); code != http.StatusForbidden {
		t.Fatalf("user-vp (vp in dept-A) approving a record with no department: expected 403, got %d", code)
	}
}
