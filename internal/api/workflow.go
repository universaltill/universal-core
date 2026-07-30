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
	"strings"

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

// cachedWorkflowDef is one memoized definition lookup — the resolved
// Definition or the error that resolving it produced. Both are cached:
// see approvalRoleFor.
type cachedWorkflowDef struct {
	def *workflow.Definition
	err error
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
	requiredRole, err := approvalRoleFor(ctx, ts, job, nil)
	if err != nil {
		return err
	}
	if requiredRole == "" {
		return nil
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

// approvalRoleFor returns the Role code job's current require_approval
// step demands, or "" when the step names none (the backward-compatible
// "anyone may approve" default).
//
// Extracted so the enforcement path (requireApprovalRole, called by
// approveWorkflowJob) and the DISPLAY path (the workflow inbox, deciding
// whether to offer an Approve button) resolve the requirement through the
// same code rather than each implementing it. Two implementations would
// drift, and both directions of drift are bad: an inbox more permissive
// than the gate offers buttons that 403, and an inbox stricter than the
// gate hides work a user could actually have done.
//
// What that does NOT buy is freedom from a race. Sharing this function
// removes IMPLEMENTATION drift, not TEMPORAL drift: the inbox resolves at
// render time and the gate re-resolves at click time, so a job that
// advances to a differently-gated step, or a role grant that changes in
// between, still leaves a stale button that 403s. The window is small and
// the failure is the same silent htmx no-swap this task exists to reduce
// — narrowed from the routine case to a race, not eliminated. Closing it
// properly needs the inbox to handle a non-2xx approve response, which is
// the separately-tracked htmx-error-surfacing gap (QUEUE.md, alongside
// the optimistic-locking 409).
//
// defCache, when non-nil, memoizes the definition lookup per
// (name, version) for the caller's lifetime. The inbox resolves a whole
// page of jobs that overwhelmingly share a handful of workflow
// definitions, so without it the page is an N+1 of identical registry
// reads; approveWorkflowJob handles exactly one job and passes nil.
func approvalRoleFor(ctx context.Context, ts tenantScope, job data.WorkflowJob, defCache map[string]*cachedWorkflowDef) (string, error) {
	cacheKey := fmt.Sprintf("%s@%d", job.WorkflowName, job.WorkflowVersion)
	var def *workflow.Definition
	entry, cached := (*cachedWorkflowDef)(nil), false
	if defCache != nil {
		entry, cached = defCache[cacheKey]
	}
	if cached {
		if entry.err != nil {
			return "", entry.err
		}
		def = entry.def
	} else {
		resolved, err := func() (*workflow.Definition, error) {
			defVersion, err := ts.workflowDefs.GetVersion(ctx, job.WorkflowName, job.WorkflowVersion)
			if err != nil {
				return nil, fmt.Errorf("get workflow definition %s v%d: %w", job.WorkflowName, job.WorkflowVersion, err)
			}
			d, err := workflow.Unmarshal(defVersion.Definition)
			if err != nil {
				return nil, fmt.Errorf("unmarshal workflow definition %s v%d: %w", job.WorkflowName, job.WorkflowVersion, err)
			}
			return d, nil
		}()
		// Failures are memoized too. Caching only successes meant that a
		// single missing or malformed definition reproduced exactly the
		// N+1 of identical failing lookups this cache exists to prevent —
		// and a broken definition is precisely when an inbox is most
		// likely to hold many jobs pointing at it. (Independent review.)
		if defCache != nil {
			defCache[cacheKey] = &cachedWorkflowDef{def: resolved, err: err}
		}
		if err != nil {
			return "", err
		}
		def = resolved
	}
	if job.StepIndex < 0 || job.StepIndex >= len(def.Steps) {
		return "", fmt.Errorf("workflow job %s: step index %d out of range for %s v%d (%d steps)", job.ID, job.StepIndex, job.WorkflowName, job.WorkflowVersion, len(def.Steps))
	}
	step := def.Steps[job.StepIndex]
	if step.Kind != workflow.StepRequireApproval {
		return "", fmt.Errorf("workflow job %s: step %d is a %q step, not require_approval — nothing should have halted it waiting for approval", job.ID, job.StepIndex, step.Kind)
	}
	raw, present := step.Params["role"]
	if !present {
		return "", nil
	}
	// workflow.Definition.Validate (run by Unmarshal above) already
	// rejects a non-string/empty "role" param at publish time, so this
	// can't actually be false for a definition that made it this far —
	// checked explicitly anyway rather than discarding it (a silently
	// ignored malformed value here would mean "no restriction", which is
	// exactly the fail-open bug this whole check exists to prevent).
	requiredRole, ok := raw.(string)
	if !ok || requiredRole == "" {
		return "", fmt.Errorf("workflow job %s: step %d's role param is %#v, not a valid Role code", job.ID, job.StepIndex, raw)
	}
	return requiredRole, nil
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
		ColumnAction:   h.catalog.T(locale, "workflow_inbox.column_action"),
		ApproveLabel:   h.catalog.T(locale, "workflow_inbox.approve_button"),
	}

	// The viewer's own roles, resolved ONCE for the whole page rather than
	// per row — this is the same set for every job, and the inbox is the
	// one place that renders many jobs at a time.
	viewerRoles, err := foundation.RoleCodesForUser(r.Context(), ts.db, rc.Actor.ID)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("resolve roles for user %s", rc.Actor.ID), err)
		return
	}
	holds := make(map[string]bool, len(viewerRoles))
	for _, c := range viewerRoles {
		holds[c] = true
	}
	defCache := map[string]*cachedWorkflowDef{}

	for _, j := range jobs {
		row := workflowInboxRowView{
			ID:           j.ID,
			WorkflowName: j.WorkflowName,
			EntityLabel:  h.entityDisplayName(locale, j.EntityType),
			RecordHref:   "/forms/" + j.EntityType + "/" + j.RecordID,
			RecordID:     j.RecordID,
			ApproveHref:  "/api/workflow-jobs/" + j.ID + "/approve",
		}

		// Resolved through the SAME function approveWorkflowJob's own gate
		// uses, so what the inbox offers and what the API will accept
		// cannot disagree.
		requiredRole, err := approvalRoleFor(r.Context(), ts, j, defCache)
		switch {
		case err != nil:
			// A job whose definition or step index can't be resolved is a
			// data problem, not this viewer's fault. Show the row without
			// an Approve button rather than failing the whole page —
			// hiding every other pending approval because one job is
			// malformed would be a worse outcome — and log it, since a
			// silently unactionable row is exactly the confusion this
			// task exists to remove.
			log.Printf("api: workflow inbox: resolve required role for job %s: %v", j.ID, err)
			row.BlockedReason = h.catalog.T(locale, "workflow_inbox.unavailable")
		case requiredRole == "" || holds[requiredRole]:
			// Unrestricted step, or the viewer holds the role.
			row.CanApprove = true
		default:
			// The case this task is about. Before role-gating existed
			// every step was unrestricted, so this was unreachable;
			// role-gating turned it into the routine case, and the button
			// was still being offered — clicking it now 403s, and htmx
			// does not swap on a non-2xx response, so the click visibly
			// did nothing with no explanation at all.
			row.BlockedReason = strings.ReplaceAll(
				h.catalog.T(locale, "workflow_inbox.requires_role"), "{role}", requiredRole)
		}
		view.Rows = append(view.Rows, row)
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
	ColumnAction   string
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
	// CanApprove and BlockedReason are mutually exclusive: exactly one of
	// an Approve button or an explanation renders, never both and never
	// neither. The row itself always renders — a pending approval the
	// viewer cannot action is still information they may need (it is why
	// their purchase order is sitting there), so this greys the action
	// out rather than hiding the work.
	CanApprove    bool
	BlockedReason string
}

var workflowInboxTmpl = template.Must(template.New("workflowInbox").Parse(`
<div class="uc-list-toolbar">
<h1>{{.Title}}</h1>
</div>
{{if not .Rows}}
<p class="uc-empty">{{.Empty}}</p>
{{else}}
<table class="uc-table">
<thead><tr><th>{{.ColumnWorkflow}}</th><th>{{.ColumnEntity}}</th><th>{{.ColumnRecord}}</th><th>{{.ColumnAction}}</th></tr></thead>
<tbody>
{{range .Rows}}
<tr id="workflow-job-{{.ID}}">
<td>{{.WorkflowName}}</td>
<td>{{.EntityLabel}}</td>
<td><a href="{{.RecordHref}}">{{.RecordID}}</a></td>
<td>{{if .CanApprove}}<button hx-post="{{.ApproveHref}}" hx-target="closest tr" hx-swap="outerHTML">{{$.ApproveLabel}}</button>{{else}}<span class="uc-inbox-blocked" title="{{.BlockedReason}}">{{.BlockedReason}}</span>{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}
`))
