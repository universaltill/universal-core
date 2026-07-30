// This file wires the CRUD API to the workflow engine — the missing
// piece connecting R9's workflow definitions and the durable job queue
// (internal/worker, wired into cmd/universal-core's main() 2026-07-21) to
// anything that could actually start a workflow run in a real
// deployment. Before this, workflow.Queue.Enqueue was reachable only
// from tests: creating or updating a record never looked for a matching
// on_create/on_update workflow at all.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// triggerWorkflows looks for every published workflow Definition
// tenantID has whose Trigger matches (triggerType, entityType), and
// enqueues one against recordID for each match. Called after a create/
// update has already committed — a trigger match failure (a malformed
// stored Definition, a DB error listing names) is logged and otherwise
// ignored, never surfaced as a failure of the create/update itself: the
// record write already succeeded, and "your save failed" would be a lie
// the same way a broken reference-option lookup degrading silently
// (loadReferenceOptions) is a deliberate choice elsewhere in this file,
// not an oversight.
//
// O(published workflow count) per create/update — reads every published
// workflow Definition for the tenant and checks its Trigger in Go, since
// workflow_definitions stores Trigger inside the JSONB definition column
// with no query support for "find by trigger.entity_type" (the DB schema
// staying generic, CLAUDE.md's kernel/deterministic-core boundary rule,
// same reasoning ListPublishedNames' own doc comment gives). Fine at
// this kernel's current stage — a real deployment scaling to hundreds of
// workflow definitions per tenant is exactly the kind of future problem
// dashboardModules' own N+1 note already named as "revisit if it ever
// matters," not a reason to add trigger-matching SQL today.
func (h *Handler) triggerWorkflows(ctx context.Context, ts tenantScope, entityType, recordID string, triggerType workflow.TriggerType, actor audit.Actor) {
	names, err := ts.workflowDefs.ListPublishedNames(ctx)
	if err != nil {
		log.Printf("api: trigger workflows for %s %s: list published workflow names: %v", entityType, recordID, err)
		return
	}
	for _, name := range names {
		v, err := ts.workflowDefs.GetPublished(ctx, name)
		if err != nil {
			log.Printf("api: trigger workflows for %s %s: get published workflow %q: %v", entityType, recordID, name, err)
			continue
		}
		def, err := workflow.Unmarshal(v.Definition)
		if err != nil {
			log.Printf("api: trigger workflows for %s %s: unmarshal workflow %q: %v", entityType, recordID, name, err)
			continue
		}
		if def.Trigger.Type != triggerType || def.Trigger.EntityType != entityType {
			continue
		}
		if _, err := ts.workflowQueue.Enqueue(ctx, def, entityType, recordID, actor); err != nil {
			log.Printf("api: trigger workflow %q for %s %s: enqueue: %v", name, entityType, recordID, err)
		}
	}
}

// approveWorkflowJob resumes a job halted at a require_approval step —
// the HTTP handler workflow.Queue.ResumeAfterApproval's own doc comment
// says "isn't built yet" pointing at. Only a job actually waiting for
// approval can be resumed; anything else (wrong tenant, wrong id, not
// currently waiting_approval, already resumed once) reports the same
// 404 as any other "no such thing here" — resuming isn't idempotent past
// the point there's nothing left to resume (see data.WorkflowJobRepo's
// own tests), and a caller doesn't need to distinguish those cases from
// "you got the id wrong."
//
// Role-gated (R17's first slice — see uc-infra/docs/adr/0006's
// addendum): the require_approval step's own `role` param (a Role.code,
// e.g. `poApprovalWorkflow`'s `{"role": "cfo"}`) previously named the
// approver but was never checked against anyone — any authenticated
// tenant-scoped caller could resume any job regardless of the step's
// `role`. A step with no `role` param keeps today's behaviour (anyone in
// the tenant may approve it) — this only starts enforcing a role that a
// workflow author actually named. Department-scoped routing (resolving
// *which* role/user from the org chart, not just checking a statically
// named one) is a separate, still-open backlog item.
//
// Actually running the resumed job's remaining steps is the worker's
// job (internal/worker), not this handler's — ResumeAfterApproval only
// flips the job back to 'queued' and requeues it; the next poll picks
// it up. This endpoint returns as soon as that's durably recorded, not
// after the workflow finishes running.
//
// On success, an htmx caller (the inbox page's own Approve button, see
// renderWorkflowInbox) gets an empty 200 body instead of the JSON
// envelope every other caller gets: the button's hx-target="closest tr"
// hx-swap="outerHTML" removes the whole row by replacing it with nothing,
// the standard htmx "delete this row" idiom — a JSON body there would
// render as literal text inside the table. Error responses stay JSON
// either way (httpx.WriteError): htmx doesn't swap on a non-2xx response
// by default, so the row simply stays and the request fails silently in
// the UI for now — see QUEUE.md's note on the equivalent gap already
// documented for optimistic locking's 409.
func (h *Handler) approveWorkflowJob(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid workflow job id")
		return
	}

	job, err := ts.workflowQueue.Get(r.Context(), id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("workflow job %q not found or not waiting for approval", id))
		return
	}
	if err != nil {
		writeInternalError(w, fmt.Sprintf("look up workflow job %s", id), err)
		return
	}
	// Not found and wrong status report the same 404 — see this func's
	// own doc comment on why a caller doesn't need to distinguish them.
	if job.Status != "waiting_approval" {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("workflow job %q not found or not waiting for approval", id))
		return
	}

	if !rc.Machine {
		if err := requireApprovalRole(r.Context(), ts, job, rc.Actor.ID); err != nil {
			if errors.Is(err, errApprovalRoleDenied) {
				httpx.WriteError(w, http.StatusForbidden, err.Error())
				return
			}
			writeInternalError(w, fmt.Sprintf("resolve required approval role for workflow job %s", id), err)
			return
		}
	}

	if err := ts.workflowQueue.ResumeAfterApproval(r.Context(), id); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("workflow job %q not found or not waiting for approval", id))
			return
		}
		writeInternalError(w, fmt.Sprintf("approve workflow job %s", id), err)
		return
	}
	if isHTMXRequest(r) {
		// Content-Type set explicitly, same convention
		// writeRecordFormFragment's own HTML fragment responses use —
		// harmless and correct even though the body is empty here.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// errApprovalRoleDenied is requireApprovalRole's own sentinel (as opposed
// to any other error resolving the step/role, which is a genuine 500) —
// mapped to 403 by its one caller, approveWorkflowJob.
var errApprovalRoleDenied = errors.New("caller does not hold the role required to approve this workflow step")

// requireApprovalRole resolves the require_approval step job is currently
// waiting at and checks userID actually holds the Role its `role` param
// names, returning errApprovalRoleDenied if not. A step with no `role`
// param (or an empty one) is a no-op — see approveWorkflowJob's own doc
// comment on why that's the deliberate backward-compatible default, not
// an oversight.
func requireApprovalRole(ctx context.Context, ts tenantScope, job data.WorkflowJob, userID string) error {
	defVersion, err := ts.workflowDefs.GetVersion(ctx, job.WorkflowName, job.WorkflowVersion)
	if err != nil {
		return fmt.Errorf("get workflow definition %s v%d: %w", job.WorkflowName, job.WorkflowVersion, err)
	}
	def, err := workflow.Unmarshal(defVersion.Definition)
	if err != nil {
		return fmt.Errorf("unmarshal workflow definition %s v%d: %w", job.WorkflowName, job.WorkflowVersion, err)
	}
	if job.StepIndex < 0 || job.StepIndex >= len(def.Steps) {
		return fmt.Errorf("workflow job %s: step index %d out of range for %s v%d (%d steps)", job.ID, job.StepIndex, job.WorkflowName, job.WorkflowVersion, len(def.Steps))
	}
	step := def.Steps[job.StepIndex]
	if step.Kind != workflow.StepRequireApproval {
		return fmt.Errorf("workflow job %s: step %d is a %q step, not require_approval — nothing should have halted it waiting for approval", job.ID, job.StepIndex, step.Kind)
	}
	raw, present := step.Params["role"]
	if !present {
		return nil
	}
	// workflow.Definition.Validate (run by Unmarshal above) already
	// rejects a non-string/empty "role" param at publish time, so this
	// can't actually be false for a definition that made it this far —
	// checked explicitly anyway rather than discarding it (a silently
	// ignored malformed value here would mean "no restriction", which is
	// exactly the fail-open bug this whole check exists to prevent).
	requiredRole, ok := raw.(string)
	if !ok || requiredRole == "" {
		return fmt.Errorf("workflow job %s: step %d's role param is %#v, not a valid Role code", job.ID, job.StepIndex, raw)
	}

	codes, err := foundation.RoleCodesForUser(ctx, ts.db, userID)
	if err != nil {
		return fmt.Errorf("resolve roles for user %s: %w", userID, err)
	}
	for _, code := range codes {
		if code == requiredRole {
			return nil
		}
	}
	return fmt.Errorf("%w: requires role %q", errApprovalRoleDenied, requiredRole)
}

// workflowJobResponse is the JSON shape for one row of listWorkflowJobs —
// a caller-facing view of data.WorkflowJob, same reasoning as
// recordResponse existing separately from data.Record (snake_case JSON
// tags, only the fields a caller actually needs, per CLAUDE.md's API
// conventions).
type workflowJobResponse struct {
	ID              string `json:"id"`
	WorkflowName    string `json:"workflow_name"`
	WorkflowVersion int    `json:"workflow_version"`
	EntityType      string `json:"entity_type"`
	RecordID        string `json:"record_id"`
	StepIndex       int    `json:"step_index"`
	Status          string `json:"status"`
}

// validWorkflowJobStatuses mirrors the CHECK constraint on
// workflow_jobs.status (002_workflow_jobs.sql) — kept here, not in
// internal/kernel/workflow, since validating untrusted external input is
// this HTTP layer's job, not the kernel's. Without this, a caller's typo
// (?status=waitng_approval) would 200 with an empty list indistinguishable
// from "nothing is actually waiting" — the one case this endpoint most
// needs to get right, since its whole purpose is telling a human what's
// stuck.
var validWorkflowJobStatuses = map[string]bool{
	"queued": true, "running": true, "waiting_approval": true, "done": true, "dead_letter": true,
}

// listWorkflowJobs is the read side of the approval loop —
// approveWorkflowJob resumes a job by id, but nothing before this told a
// caller which ids actually exist to resume. GET /api/workflow-jobs?
// status=waiting_approval is the minimal task list: which jobs, for
// which records, are actually waiting on a human right now. Deliberately
// not a full inbox UI (no role-based filtering, no pagination, no
// notification) — QUEUE.md scopes that as R17's broader remaining work;
// this is the mechanism a UI would call, built first because the
// mechanism has to exist before any UI can be built on top of it.
func (h *Handler) listWorkflowJobs(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		httpx.WriteError(w, http.StatusBadRequest, "status query parameter is required (e.g. ?status=waiting_approval)")
		return
	}
	if !validWorkflowJobStatuses[status] {
		httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("unknown status %q (must be one of: queued, running, waiting_approval, done, dead_letter)", status))
		return
	}

	jobs, err := ts.workflowQueue.ListByStatus(r.Context(), status)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("list workflow jobs with status %s", status), err)
		return
	}
	out := make([]workflowJobResponse, len(jobs))
	for i, j := range jobs {
		out[i] = workflowJobResponse{
			ID: j.ID, WorkflowName: j.WorkflowName, WorkflowVersion: j.WorkflowVersion,
			EntityType: j.EntityType, RecordID: j.RecordID, StepIndex: j.StepIndex, Status: j.Status,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// renderWorkflowInbox is the actual human-facing surface listWorkflowJobs
// was built to serve — "GET /api/workflow-jobs?status=waiting_approval
// gives a human something to look at" was the API alone; this is the
// page. Fixed to waiting_approval (not a general status browser — a
// human's inbox is specifically "what needs me right now", the other
// statuses are ops/debugging views this page isn't trying to be).
//
// Each row's Approve button is a real htmx interaction (hx-post + row
// removal, see approveWorkflowJob's doc comment) — deliberately not a
// plain <form> POST, since the approve endpoint returns JSON/empty
// bodies, not a redirect a plain form submission could follow to a
// sensible next page.
func (h *Handler) renderWorkflowInbox(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	jobs, err := ts.workflowQueue.ListByStatus(r.Context(), "waiting_approval")
	if err != nil {
		writeInternalError(w, "list waiting-approval workflow jobs for inbox", err)
		return
	}

	view := workflowInboxView{
		Title:          h.catalog.T(locale, "workflow_inbox.title"),
		Empty:          h.catalog.T(locale, "workflow_inbox.empty"),
		ColumnWorkflow: h.catalog.T(locale, "workflow_inbox.column_workflow"),
		ColumnEntity:   h.catalog.T(locale, "workflow_inbox.column_entity"),
		ColumnRecord:   h.catalog.T(locale, "workflow_inbox.column_record"),
		ApproveLabel:   h.catalog.T(locale, "workflow_inbox.approve_button"),
	}
	for _, j := range jobs {
		view.Rows = append(view.Rows, workflowInboxRowView{
			ID:           j.ID,
			WorkflowName: j.WorkflowName,
			EntityLabel:  h.entityDisplayName(locale, j.EntityType),
			RecordHref:   "/forms/" + j.EntityType + "/" + j.RecordID,
			RecordID:     j.RecordID,
			ApproveHref:  "/api/workflow-jobs/" + j.ID + "/approve",
		})
	}

	var buf bytes.Buffer
	if err := workflowInboxTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render workflow inbox", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render workflow inbox shell", err)
	}
}

type workflowInboxView struct {
	Title          string
	Empty          string
	ColumnWorkflow string
	ColumnEntity   string
	ColumnRecord   string
	ApproveLabel   string
	Rows           []workflowInboxRowView
}

type workflowInboxRowView struct {
	ID           string
	WorkflowName string
	EntityLabel  string
	RecordHref   string
	RecordID     string
	ApproveHref  string
}

var workflowInboxTmpl = template.Must(template.New("workflowInbox").Parse(`
<div class="uc-list-toolbar">
<h1>{{.Title}}</h1>
</div>
{{if not .Rows}}
<p class="uc-empty">{{.Empty}}</p>
{{else}}
<table class="uc-table">
<thead><tr><th>{{.ColumnWorkflow}}</th><th>{{.ColumnEntity}}</th><th>{{.ColumnRecord}}</th><th></th></tr></thead>
<tbody>
{{range .Rows}}
<tr id="workflow-job-{{.ID}}">
<td>{{.WorkflowName}}</td>
<td>{{.EntityLabel}}</td>
<td><a href="{{.RecordHref}}">{{.RecordID}}</a></td>
<td><button hx-post="{{.ApproveHref}}" hx-target="closest tr" hx-swap="outerHTML">{{$.ApproveLabel}}</button></td>
</tr>
{{end}}
</tbody>
</table>
{{end}}
`))
