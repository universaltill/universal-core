package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// The workflow inbox used to offer an Approve button for every pending
// job regardless of whether the viewer held the role the step requires.
// Before role-gating existed that was harmless (every step was
// unrestricted); role-gating made it the routine case, and clicking now
// 403s — which htmx does not swap on, so the click visibly did nothing
// with no explanation at all.
//
// The property that matters most here is not "a button is hidden" but
// that the inbox and the enforcement path agree. They resolve the
// requirement through the same function (approvalRoleFor) precisely so
// they cannot drift, and drift is bad in BOTH directions: an inbox more
// permissive than the gate offers buttons that fail, an inbox stricter
// than the gate hides work the user could have done.

// gatedApprovalWorkflow is a one-step workflow whose require_approval
// step demands roleCode. An empty roleCode publishes the unrestricted
// shape instead (the backward-compatible "anyone may approve" default).
func gatedApprovalWorkflow(name, roleCode string) *workflow.Definition {
	step := workflow.Step{Kind: workflow.StepRequireApproval}
	if roleCode != "" {
		step.Params = map[string]any{"role": roleCode}
	}
	return &workflow.Definition{
		Name:    name,
		Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{step},
	}
}

// inboxFixture publishes two workflows — one gated on "finance_manager",
// one unrestricted — enqueues a job halted at each, and grants
// "user-finance" the finance_manager role. "user-other" holds no roles.
func inboxFixture(t *testing.T) (tenantID string, db *sql.DB, mux *http.ServeMux) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db = newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	engine := crud.NewEngine(db)
	role, err := engine.Create(ctx, foundation.Role(),
		map[string]any{"code": "finance_manager", "name": "Finance Manager"}, humanActor())
	if err != nil {
		t.Fatalf("create Role: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.UserRole(),
		map[string]any{"user_id": "user-finance", "role_id": role.ID}, humanActor()); err != nil {
		t.Fatalf("grant role: %v", err)
	}

	gated := gatedApprovalWorkflow("gated_approval", "finance_manager")
	open := gatedApprovalWorkflow("open_approval", "")
	publishWorkflow(t, db, gated)
	publishWorkflow(t, db, open)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	for _, def := range []*workflow.Definition{gated, open} {
		job, err := q.Enqueue(ctx, def, "Vendor", "11111111-1111-1111-1111-111111111111", humanActor())
		if err != nil {
			t.Fatalf("Enqueue %s: %v", def.Name, err)
		}
		// Stand in for the worker and drive it to its require_approval
		// halt — the state the inbox lists. RegistryDefinitionLookup is
		// the real resolution path (a nil lookup panics), matching what
		// internal/worker actually uses.
		if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
			t.Fatalf("ProcessOne(%s): %v", job.ID, err)
		}
	}

	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, db, mux
}

// A viewer WITHOUT the required role sees the gated job — the work is
// still information they may need — but gets an explanation naming the
// role instead of a button that would 403.
func TestWorkflowInbox_HidesApproveForRoleTheViewerLacks(t *testing.T) {
	tenantID, _, mux := inboxFixture(t)

	rec := getAs(t, mux, "/workflow-jobs", tenantID, "user-other")
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "finance_manager") {
		t.Fatalf("expected the blocked row to name the required role:\n%s", body)
	}
	if !strings.Contains(body, "uc-inbox-blocked") {
		t.Fatalf("expected a blocked-state element for the gated job:\n%s", body)
	}
	// The unrestricted job must still be actionable — this is the
	// "stricter than the gate" drift direction, which would hide work the
	// viewer is genuinely allowed to do.
	if !strings.Contains(body, "hx-post=") {
		t.Fatalf("the unrestricted job lost its Approve button:\n%s", body)
	}
	if got := strings.Count(body, "hx-post="); got != 1 {
		t.Fatalf("expected exactly 1 Approve button (the unrestricted job), got %d:\n%s", got, body)
	}
}

// A viewer WITH the role gets both buttons.
func TestWorkflowInbox_ShowsApproveForRoleTheViewerHolds(t *testing.T) {
	tenantID, _, mux := inboxFixture(t)

	rec := getAs(t, mux, "/workflow-jobs", tenantID, "user-finance")
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, "hx-post="); got != 2 {
		t.Fatalf("role holder should see 2 Approve buttons, got %d:\n%s", got, body)
	}
	if strings.Contains(body, "uc-inbox-blocked") {
		t.Fatalf("role holder should see no blocked rows:\n%s", body)
	}
}

// The property the whole refactor exists for: what the inbox OFFERS and
// what the approve endpoint ACCEPTS must agree, for every viewer and
// every job. Asserted by driving the real endpoint rather than by
// re-deriving the expectation, so a future change to either path that
// breaks the correspondence fails here.
func TestWorkflowInbox_OfferMatchesWhatTheEndpointAccepts(t *testing.T) {
	// A FRESH fixture per actor, deliberately. Approving does not just
	// answer a permission question — it resumes the job, so a single
	// shared fixture would leave the second actor POSTing against jobs the
	// first one already consumed, and the endpoint would answer 404
	// ("gone") where the test means to ask 403-or-not ("allowed?").
	// The first draft did exactly that and the mismatch it reported was
	// its own side effect, not a product bug.
	for _, actor := range []string{"user-other", "user-finance"} {
		t.Run(actor, func(t *testing.T) {
			tenantID, db, mux := inboxFixture(t)
			ctx := context.Background()

			q, err := workflow.NewQueue(db, nil)
			if err != nil {
				t.Fatalf("NewQueue: %v", err)
			}
			jobs, err := q.ListByStatus(ctx, "waiting_approval")
			if err != nil {
				t.Fatalf("ListByStatus: %v", err)
			}
			if len(jobs) != 2 {
				t.Fatalf("fixture should leave 2 jobs waiting, got %d", len(jobs))
			}

			body := getAs(t, mux, "/workflow-jobs", tenantID, actor).Body.String()
			for _, j := range jobs {
				offered := strings.Contains(body, "/api/workflow-jobs/"+j.ID+"/approve\"")

				req := newRequest("POST", "/api/workflow-jobs/"+j.ID+"/approve", tenantID, actor, nil)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code == http.StatusNotFound {
					t.Fatalf("job %s was already consumed — fixture isolation broken", j.ID)
				}
				accepted := w.Code != http.StatusForbidden

				if offered != accepted {
					t.Fatalf("inbox/endpoint disagree for actor %s job %s: inbox offered=%v, endpoint accepted=%v (status %d)",
						actor, j.ID, offered, accepted, w.Code)
				}
			}
		})
	}
}

// The JSON API half of the same question the HTML inbox answers. Deliberately
// DESCRIBES rather than filters: an ops dashboard counting what is pending
// must not silently under-report because its token holds no roles.
func TestAPI_ListWorkflowJobs_ReportsApprovability(t *testing.T) {
	tenantID, _, mux := inboxFixture(t)

	decode := func(actor string) []workflowJobResponse {
		t.Helper()
		rec := getAs(t, mux, "/api/workflow-jobs?status=waiting_approval", tenantID, actor)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", actor, rec.Code, rec.Body.String())
		}
		var env struct {
			Data []workflowJobResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode: %v: %s", actor, err, rec.Body.String())
		}
		return env.Data
	}

	// Every caller sees EVERY job — this is the "describe, don't filter"
	// contract. A client that lost rows based on its own permissions could
	// not be used to answer "how much is pending?".
	for _, actor := range []string{"user-other", "user-finance"} {
		if got := len(decode(actor)); got != 2 {
			t.Fatalf("%s should see both jobs, got %d", actor, got)
		}
	}

	byWorkflow := func(rows []workflowJobResponse) map[string]workflowJobResponse {
		m := map[string]workflowJobResponse{}
		for _, r := range rows {
			m[r.WorkflowName] = r
		}
		return m
	}

	other := byWorkflow(decode("user-other"))
	gated := other["gated_approval"]
	if gated.RequiredRole == nil || *gated.RequiredRole != "finance_manager" {
		t.Fatalf("gated job should name its required role, got %v", gated.RequiredRole)
	}
	// An EXPLICIT false, not an absent field — absent means "no verdict".
	if gated.CanApprove == nil || *gated.CanApprove {
		t.Fatalf("a caller without the role must be told explicitly it cannot approve, got %v", gated.CanApprove)
	}
	open_ := other["open_approval"]
	if open_.RequiredRole == nil || *open_.RequiredRole != "" {
		t.Fatalf("unrestricted job should report an empty required role, not an absent one, got %v", open_.RequiredRole)
	}
	if open_.CanApprove == nil || !*open_.CanApprove {
		t.Fatalf("unrestricted job should be approvable by anyone, got %v", open_.CanApprove)
	}

	fin := byWorkflow(decode("user-finance"))
	if fin["gated_approval"].CanApprove == nil || !*fin["gated_approval"].CanApprove {
		t.Fatal("the role holder should be told it can approve the gated job")
	}
}

// The API's verdict must match what the endpoint actually does — the same
// correspondence the HTML inbox is held to. Asserted by driving the real
// endpoint rather than restating the expectation.
func TestAPI_ListWorkflowJobs_CanApproveMatchesTheEndpoint(t *testing.T) {
	for _, actor := range []string{"user-other", "user-finance"} {
		t.Run(actor, func(t *testing.T) {
			// Fresh fixture per actor: approving RESUMES the job, so a shared
			// one would leave the second actor probing consumed jobs.
			tenantID, _, mux := inboxFixture(t)
			rec := getAs(t, mux, "/api/workflow-jobs?status=waiting_approval", tenantID, actor)
			var env struct {
				Data []workflowJobResponse `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, row := range env.Data {
				req := newRequest("POST", "/api/workflow-jobs/"+row.ID+"/approve", tenantID, actor, nil)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code == http.StatusNotFound {
					t.Fatalf("job %s already consumed — fixture isolation broken", row.ID)
				}
				accepted := w.Code != http.StatusForbidden
				if row.CanApprove == nil {
					t.Fatalf("%s job %s: API gave no verdict for a resolvable job", actor, row.ID)
				}
				if *row.CanApprove != accepted {
					t.Fatalf("%s job %s: API said can_approve=%v, endpoint %s (status %d)",
						actor, row.ID, *row.CanApprove, map[bool]string{true: "accepted", false: "refused"}[accepted], w.Code)
				}
			}
		})
	}
}

// Statuses other than waiting_approval must not gain the fields, and must not
// error: those jobs are not sitting at a require_approval step, so there is no
// meaningful answer to give.
func TestAPI_ListWorkflowJobs_NonApprovalStatusUnchanged(t *testing.T) {
	tenantID, db, mux := inboxFixture(t)
	ctx := context.Background()

	// Drive a job all the way to `done` FIRST. The original version of this
	// test asserted against an empty list — the fixture only ever produces
	// waiting_approval jobs — so it passed no matter what the code did.
	// Independent review caught it: a test named for a regression it cannot
	// detect is worse than no test, because it reads as coverage.
	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	jobs, err := q.ListByStatus(ctx, "waiting_approval")
	if err != nil || len(jobs) == 0 {
		t.Fatalf("fixture should leave waiting jobs: %v (%d)", err, len(jobs))
	}
	if err := q.ResumeAfterApproval(ctx, jobs[0].ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
			break
		}
	}
	done, err := q.ListByStatus(ctx, "done")
	if err != nil {
		t.Fatalf("list done: %v", err)
	}
	if len(done) == 0 {
		t.Fatal("no done jobs — this test would assert against an empty list again")
	}

	rec := getAs(t, mux, "/api/workflow-jobs?status=done", tenantID, "user-finance")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), jobs[0].ID) {
		t.Fatalf("the done job should appear in the listing: %s", rec.Body.String())
	}
	// Absent entirely for statuses that are not waiting_approval — existing
	// clients see a byte-identical payload.
	if strings.Contains(rec.Body.String(), "required_role") || strings.Contains(rec.Body.String(), "can_approve") {
		t.Fatalf("non-approval statuses should carry neither field: %s", rec.Body.String())
	}
}
