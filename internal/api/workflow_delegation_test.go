package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// Delegation (uc-infra#8, "substitute approver") is the second axis
// userMeetsApproval resolves, alongside the role/department check
// TestAPI_DepartmentScopedApproval already exercises: after a caller's
// own standing fails, every user who has actively delegated TO them is
// checked, one hop, against their OWN role/department grants. These
// tests drive that axis through the same real HTTP surface the existing
// role/department tests use, so a drift between the enforcement gate
// and the inbox/list would be caught here exactly the same way it would
// there.

// delegationFixture publishes a workflow whose require_approval step
// demands "finance_manager" tenant-wide (no department), grants that
// role to "user-a" only, and enqueues+drives one job to waiting_approval.
// Returns everything a test needs to both call the approve endpoint and
// author Delegation/UserRole records directly.
func delegationFixture(t *testing.T) (tenantID string, db *sql.DB, mux *http.ServeMux, jobID string, eng *crud.Engine) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db = newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	eng = crud.NewEngine(db)
	fin, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "finance_manager", "name": "FM"}, humanActor())
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-a", "role_id": fin.ID}, humanActor()); err != nil {
		t.Fatalf("grant user-a: %v", err)
	}

	def := gatedApprovalWorkflow("delegation_gated_approval", "finance_manager")
	publishWorkflow(t, db, def)

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

	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, db, mux, job.ID, eng
}

// requisitionEntityAndForm is the same minimal "a record carrying a
// routing department_id" fixture TestAPI_DepartmentScopedApproval
// defines inline — factored out here since two tests in this file need
// it (one department-scoped, one not).
func requisitionEntityAndForm() (*entity.Definition, *form.Definition) {
	entDef := &entity.Definition{
		EntityType: "Requisition", Version: 1, Module: "foundation",
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true},
			{Name: "department_id", Type: entity.FieldString},
		},
	}
	formDef := &form.Definition{
		EntityType: "Requisition", Version: 1,
		Sections: []form.Section{{Title: "D", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "title"}, {Name: "department_id"}}}},
	}
	return entDef, formDef
}

func approveAs(mux *http.ServeMux, tenantID, jobID, actor string) int {
	req := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantID, actor, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code
}

// TestAPI_ApproveWorkflowJob_ViaDelegation is the basic substitute-
// approver case: user-b holds no role at all, but user-a (who holds
// finance_manager) has delegated to user-b indefinitely — user-b may
// approve in user-a's stead.
func TestAPI_ApproveWorkflowJob_ViaDelegation(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	// Before any delegation exists, user-b is denied — establishes the
	// baseline this test's whole point depends on.
	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusForbidden {
		t.Fatalf("user-b with no role and no delegation: expected 403, got %d", code)
	}

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusOK {
		t.Fatalf("user-b as user-a's delegate: expected 200, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_RevokedDelegationDenied confirms revoking a
// delegation is an immediate off switch for approval eligibility, not
// just a UI-level hide.
func TestAPI_ApproveWorkflowJob_RevokedDelegationDenied(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b", "revoked": true}, humanActor()); err != nil {
		t.Fatalf("create revoked Delegation: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusForbidden {
		t.Fatalf("user-b via a revoked delegation: expected 403, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_ExpiredDelegationDenied confirms an
// ends_at in the past is treated the same as revoked.
func TestAPI_ApproveWorkflowJob_ExpiredDelegationDenied(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b", "ends_at": yesterday}, humanActor()); err != nil {
		t.Fatalf("create expired Delegation: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusForbidden {
		t.Fatalf("user-b via an expired delegation: expected 403, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_DelegationIsNonTransitive is the security-
// relevant edge case ADR-0006's delegation addendum decided explicitly:
// user-a delegates to user-b, and SEPARATELY user-b delegates to
// user-c. user-b (a direct delegate of user-a, who holds the role) may
// act; a sibling test then confirms user-c may NOT, against a second job
// (this one is consumed by user-b's approval, since ResumeAfterApproval
// only fires once per job).
func TestAPI_ApproveWorkflowJob_DelegationIsNonTransitive(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation A->B: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-b", "delegate_user_id": "user-c"}, humanActor()); err != nil {
		t.Fatalf("create Delegation B->C: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusOK {
		t.Fatalf("user-b, user-a's direct delegate: expected 200, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_DelegationIsNonTransitive_Chain is the
// negative half: user-c, who only holds standing via user-b's
// delegation (never user-a's role directly, nor a direct delegation
// from user-a), must be denied — authority does not chain past one
// substitution.
func TestAPI_ApproveWorkflowJob_DelegationIsNonTransitive_Chain(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation A->B: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-b", "delegate_user_id": "user-c"}, humanActor()); err != nil {
		t.Fatalf("create Delegation B->C: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-c"); code != http.StatusForbidden {
		t.Fatalf("user-c, only a delegate of a delegate: expected 403 (non-transitive), got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_DepartmentScopedDelegation confirms
// delegation composes with department-scoped routing: the delegate
// inherits the delegator's DEPARTMENT-scoped standing specifically,
// not a tenant-wide grant the delegator never actually held.
func TestAPI_ApproveWorkflowJob_DepartmentScopedDelegation(t *testing.T) {
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
	fin, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "finance_manager", "name": "FM"}, humanActor())
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	// user-a holds finance_manager ONLY in dept-A.
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-a", "role_id": fin.ID, "department_id": "dept-A"}, humanActor()); err != nil {
		t.Fatalf("grant user-a: %v", err)
	}
	// user-a delegates to user-b.
	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	recA, err := eng.Create(ctx, entDef, map[string]any{"title": "dept-A item", "department_id": "dept-A"}, humanActor())
	if err != nil {
		t.Fatalf("record A: %v", err)
	}
	recB, err := eng.Create(ctx, entDef, map[string]any{"title": "dept-B item", "department_id": "dept-B"}, humanActor())
	if err != nil {
		t.Fatalf("record B: %v", err)
	}

	def := &workflow.Definition{
		Name: "req_approval_delegation", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{{Kind: workflow.StepRequireApproval,
			Params: map[string]any{"role": "finance_manager", "department": "department_id"}}},
	}
	publishWorkflow(t, db, def)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := q.Enqueue(ctx, def, "Requisition", recA.ID, humanActor()); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if _, err := q.Enqueue(ctx, def, "Requisition", recB.ID, humanActor()); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
			t.Fatalf("process %d: %v", i, err)
		}
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	jobs, _ := q.ListByStatus(ctx, "waiting_approval")
	if len(jobs) != 2 {
		t.Fatalf("expected 2 waiting jobs, got %d", len(jobs))
	}
	jobByRecord := map[string]string{}
	for _, j := range jobs {
		jobByRecord[j.RecordID] = j.ID
	}

	// user-b, standing in for user-a, may approve the dept-A record...
	if code := approveAs(mux, tenantID, jobByRecord[recA.ID], "user-b"); code != http.StatusOK {
		t.Fatalf("user-b delegated dept-A standing on a dept-A record: expected 200, got %d", code)
	}
	// ...but NOT the dept-B record — user-a never held the role there,
	// so there is nothing for user-b to inherit.
	if code := approveAs(mux, tenantID, jobByRecord[recB.ID], "user-b"); code != http.StatusForbidden {
		t.Fatalf("user-b delegated dept-A standing on a dept-B record: expected 403, got %d", code)
	}
}

// TestAPI_ApproveWorkflowJob_SelfDelegationIsInert confirms a
// self-delegation (delegator == delegate) is accepted at write time
// (Delegation has no cross-field inequality validation, see its own doc
// comment) but grants nothing beyond what the user already has, and
// does not somehow let a roleless user approve.
func TestAPI_ApproveWorkflowJob_SelfDelegationIsInert(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-b", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create self-Delegation: %v", err)
	}

	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusForbidden {
		t.Fatalf("user-b's self-delegation must not grant anything it doesn't already have: expected 403, got %d", code)
	}
}

// TestWorkflowInbox_ShowsApproveViaDelegation confirms the inbox and the
// enforcement gate agree about delegation too — the same single-source
// discipline TestWorkflowInbox_ShowsApproveForRoleTheViewerHolds already
// establishes for direct role grants, since both read through the same
// userMeetsApproval.
func TestWorkflowInbox_ShowsApproveViaDelegation(t *testing.T) {
	tenantID, _, mux, _, eng := delegationFixture(t)
	ctx := context.Background()

	// Before delegation: the inbox must NOT offer user-b a button.
	rec := getAs(t, mux, "/workflow-jobs", tenantID, "user-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox: expected 200, got %d", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), "hx-post="); got != 0 {
		t.Fatalf("user-b with no delegation should see zero Approve buttons, got %d", got)
	}

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	rec = getAs(t, mux, "/workflow-jobs", tenantID, "user-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox: expected 200, got %d", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), "hx-post="); got != 1 {
		t.Fatalf("user-b as an active delegate should see exactly 1 Approve button, got %d:\n%s", got, rec.Body.String())
	}
}

// TestAPI_ListWorkflowJobs_CanApproveViaDelegation closes the coverage
// gap independent review found: the ADR claims all THREE surfaces
// (enforcement gate, HTML inbox, JSON list) agree via userMeetsApproval,
// but only the gate and the inbox had a delegation-specific test before
// this one. Mirrors TestAPI_ListWorkflowJobs_CanApproveMatchesTheEndpoint
// (workflow_inbox_role_test.go) for the delegation axis specifically:
// the JSON listing's own can_approve verdict must match what the real
// approve endpoint actually does, for a delegate.
func TestAPI_ListWorkflowJobs_CanApproveViaDelegation(t *testing.T) {
	tenantID, _, mux, jobID, eng := delegationFixture(t)
	ctx := context.Background()

	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-a", "delegate_user_id": "user-b"}, humanActor()); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	rec := getAs(t, mux, "/api/workflow-jobs?status=waiting_approval", tenantID, "user-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var env struct {
		Data []workflowJobResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var row *workflowJobResponse
	for i := range env.Data {
		if env.Data[i].ID == jobID {
			row = &env.Data[i]
		}
	}
	if row == nil {
		t.Fatalf("expected job %s in the listing, got %+v", jobID, env.Data)
	}
	if row.CanApprove == nil || !*row.CanApprove {
		t.Fatalf("expected can_approve=true for user-b via delegation, got %+v", row.CanApprove)
	}

	// The listing's verdict must match what actually approving does.
	if code := approveAs(mux, tenantID, jobID, "user-b"); code != http.StatusOK {
		t.Fatalf("list said can_approve=true but the endpoint returned %d", code)
	}
}

// TestDelegation_DoesNotGrantGeneralRBACAccess is the design-invariant
// regression guard for the narrow-blast-radius decision (ADR-0006's
// delegation addendum): a Delegation is consulted ONLY by
// userMeetsApproval. It must never make its way into
// internal/kernel/authz's entity/field permission resolution — a
// delegate does not inherit the delegator's Permission/FieldPermission
// grants, read/write access, or admin status. There is today no code
// path connecting the two at all; this test pins that absence so a
// future change that (accidentally or otherwise) wires Delegation into
// authz.Resolver fails loudly here first.
func TestDelegation_DoesNotGrantGeneralRBACAccess(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	eng := crud.NewEngine(db)

	admin, err := eng.Create(ctx, foundation.Role(), map[string]any{"code": "tenant_admin", "name": "Admin"}, humanActor())
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	if _, err := eng.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-admin", "role_id": admin.ID}, humanActor()); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	// Configure RBAC narrowly on some ordinary type so the control plane
	// is "configured" (a Permission row exists) without granting anyone
	// but the admin anything on it.
	if _, err := eng.Create(ctx, foundation.Permission(),
		map[string]any{"role_id": admin.ID, "entity_type": "Party", "can_read": true, "can_write": true}, humanActor()); err != nil {
		t.Fatalf("permission: %v", err)
	}
	// user-mallory has NO role at all, but user-admin delegates
	// approval authority to her.
	if _, err := eng.Create(ctx, foundation.Delegation(),
		map[string]any{"delegator_user_id": "user-admin", "delegate_user_id": "user-mallory"}, humanActor()); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	// Delegation must not let user-mallory read Party — that would be
	// full impersonation, a materially different and larger feature
	// this design deliberately does not build.
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	rec := getAs(t, mux, "/api/records/Party", tenantID, "user-mallory")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegation must not grant Party read access: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}
