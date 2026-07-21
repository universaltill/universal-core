// This file wires the CRUD API to the workflow engine — the missing
// piece connecting R9's workflow definitions and the durable job queue
// (internal/worker, wired into cmd/universal-core's main() 2026-07-21) to
// anything that could actually start a workflow run in a real
// deployment. Before this, workflow.Queue.Enqueue was reachable only
// from tests: creating or updating a record never looked for a matching
// on_create/on_update workflow at all.
package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
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
func (h *Handler) triggerWorkflows(ctx context.Context, tenantID, entityType, recordID string, triggerType workflow.TriggerType, actor audit.Actor) {
	names, err := h.workflowDefs.ListPublishedNames(ctx, tenantID)
	if err != nil {
		log.Printf("api: trigger workflows for %s %s: list published workflow names: %v", entityType, recordID, err)
		return
	}
	for _, name := range names {
		v, err := h.workflowDefs.GetPublished(ctx, tenantID, name)
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
		if _, err := h.workflowQueue.Enqueue(ctx, def, tenantID, entityType, recordID, actor); err != nil {
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
// Actually running the resumed job's remaining steps is the worker's
// job (internal/worker), not this handler's — ResumeAfterApproval only
// flips the job back to 'queued' and requeues it; the next poll picks
// it up. This endpoint returns as soon as that's durably recorded, not
// after the workflow finishes running.
func (h *Handler) approveWorkflowJob(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid workflow job id")
		return
	}

	if err := h.workflowQueue.ResumeAfterApproval(r.Context(), rc.TenantID, id); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("workflow job %q not found or not waiting for approval", id))
			return
		}
		writeInternalError(w, fmt.Sprintf("approve workflow job %s", id), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
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
	status := r.URL.Query().Get("status")
	if status == "" {
		httpx.WriteError(w, http.StatusBadRequest, "status query parameter is required (e.g. ?status=waiting_approval)")
		return
	}

	jobs, err := h.workflowQueue.ListByStatus(r.Context(), rc.TenantID, status)
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
