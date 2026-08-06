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
	"slices"
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
// the same way a broken reference-label lookup degrading silently
// (loadCurrentReferenceLabels) is a deliberate choice elsewhere in this
// file, not an oversight.
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
//
// This is the one call site that also LOGS a successful delegated
// approval — deliberately, since this function's only caller is
// approveWorkflowJob, the actual mutation. userMeetsApproval itself
// stays silent: it's also called by the JSON list endpoint and the HTML
// inbox on every page render, and logging there would fire once per
// eligible row per view rather than once per real approval (independent
// review caught the first draft doing exactly that).
func requireApprovalRole(ctx context.Context, ts tenantScope, job data.WorkflowJob, userID string) error {
	req, ok, viaDelegator, err := userMayApprove(ctx, ts, job, nil, userID, newApprovalMemo())
	if err != nil {
		return err
	}
	if ok {
		if viaDelegator != "" {
			log.Printf("api: workflow job %s: %s approved as substitute via delegation from %s", job.ID, userID, viaDelegator)
		}
		return nil
	}
	if req.DepartmentField != "" {
		return fmt.Errorf("%w: requires role %q in the record's own department", errApprovalRoleDenied, req.Role)
	}
	return fmt.Errorf("%w: requires role %q", errApprovalRoleDenied, req.Role)
}

// approvalMemo caches userMeetsApproval's own per-request lookups across
// a page's worth of jobs — the JSON list endpoint and the HTML inbox
// both call userMeetsApproval once per row for the SAME viewer, so a
// viewer's delegator set, and either viewer's or a delegator's role
// codes (keyed with the department id when the requirement is
// department-scoped, since the answer depends on both), are identical
// across every row that shares a department. Independent review
// measured the unmemoized version at a 3.24x slowdown on a 40-row inbox
// with 8 delegators; requireApprovalRole's own single-job call still
// gets a fresh one-shot memo (a map allocation is free at that scale),
// so every caller shares one code path rather than two.
type approvalMemo struct {
	delegators map[string][]string
	codes      map[string][]string
}

func newApprovalMemo() *approvalMemo {
	return &approvalMemo{delegators: map[string][]string{}, codes: map[string][]string{}}
}

// approvalRequirement is what a require_approval step demands: a Role, and
// optionally the department it must be held in. DepartmentField is the
// NAME of a field on the triggering record carrying the department id —
// empty means the role is checked tenant-wide, the original behaviour.
type approvalRequirement struct {
	Role            string
	DepartmentField string
}

// userMeetsApproval is the ONE place that decides whether userID may
// approve job given req — used by the enforcement gate, the HTML inbox,
// and the JSON API, so none of the three can disagree about who may act
// (the same single-source discipline approvalRoleFor already established
// for the role name).
//
// When req has a DepartmentField, the department id is read from that
// field on the triggering record and the role must be held FOR that
// department (foundation.RoleCodesForUserInDepartment). A record that
// carries no value in that field routes to nobody rather than to
// everyone: an unset department is not "any department", and treating it
// as such would be exactly the fail-open the whole gate exists to
// prevent.
//
// Delegation (uc-infra#8, uc-infra ADR-0006's delegation addendum):
// after userID's own standing fails to meet req, every user who
// currently has an active Delegation naming userID as delegate
// (foundation.ActiveDelegatorsFor) is checked in turn, against their OWN
// role/department standing for this same req — if any of them would
// meet it directly, userID may act as their substitute. This is
// single-hop only: a delegator's OWN grants are what's checked, never
// standing THEY in turn hold only via someone else's delegation to them,
// so authority cannot chain past one substitution.
//
// Returns the delegator id whose standing granted eligibility, or "" if
// userID met req directly (or not at all) — requireApprovalRole's own
// doc comment explains the one thing this is used for (logging a real
// approval, not every render). memo must not be nil; pass a fresh
// newApprovalMemo() for a single call, or share one across every job on
// a page so the (typically) unchanging viewer's delegator set and role
// codes resolve once instead of once per row.
func userMeetsApproval(ctx context.Context, ts tenantScope, job data.WorkflowJob, req approvalRequirement, userID string, memo *approvalMemo) (bool, string, error) {
	if req.Role == "" {
		return true, "", nil
	}

	var deptID string
	if req.DepartmentField != "" {
		var err error
		deptID, err = recordDepartmentID(ctx, ts, job, req.DepartmentField)
		if err != nil {
			return false, "", err
		}
		if deptID == "" {
			// The record names no department in that field — routes to
			// nobody, deliberately (see doc comment), including via
			// delegation: there is no department standing for anyone
			// to delegate.
			return false, "", nil
		}
	}
	codesFor := func(uid string) ([]string, error) {
		key := uid
		if req.DepartmentField != "" {
			key = uid + "|" + deptID
		}
		if codes, ok := memo.codes[key]; ok {
			return codes, nil
		}
		var codes []string
		var err error
		if req.DepartmentField == "" {
			codes, err = foundation.RoleCodesForUser(ctx, ts.db, uid)
		} else {
			codes, err = foundation.RoleCodesForUserInDepartment(ctx, ts.db, uid, deptID)
		}
		if err != nil {
			return nil, err
		}
		memo.codes[key] = codes
		return codes, nil
	}

	codes, err := codesFor(userID)
	if err != nil {
		return false, "", fmt.Errorf("resolve roles for user %s: %w", userID, err)
	}
	if slices.Contains(codes, req.Role) {
		return true, "", nil
	}

	delegators, ok := memo.delegators[userID]
	if !ok {
		delegators, err = foundation.ActiveDelegatorsFor(ctx, ts.db, userID)
		if err != nil {
			return false, "", fmt.Errorf("resolve delegators for user %s: %w", userID, err)
		}
		memo.delegators[userID] = delegators
	}
	for _, delegatorID := range delegators {
		delegatorCodes, err := codesFor(delegatorID)
		if err != nil {
			return false, "", fmt.Errorf("resolve roles for delegator %s: %w", delegatorID, err)
		}
		if slices.Contains(delegatorCodes, req.Role) {
			return true, delegatorID, nil
		}
	}
	return false, "", nil
}

// recordDepartmentID reads the department id from the named field of the
// job's triggering record. A scheduled run (no record) or a blank field
// yields "" — routing to nobody, per userMeetsApproval's contract.
func recordDepartmentID(ctx context.Context, ts tenantScope, job data.WorkflowJob, field string) (string, error) {
	if job.EntityType == "" || job.RecordID == "" {
		return "", nil
	}
	def, err := ts.entityDef(ctx, job.EntityType)
	if err != nil {
		return "", fmt.Errorf("look up %s definition for department routing: %w", job.EntityType, err)
	}
	// SystemFieldForRouting, not the guarded Get: routing is the kernel
	// deciding who may approve, and it must not depend on whether the
	// approver is allowed to READ the routing field. Reading it through
	// their own permissions would wrongly deny a legitimate approver whose
	// role hides department_id, or 500 one with no read grant on the type
	// — both found by independent review.
	id, err := ts.crud.SystemFieldForRouting(ctx, def, job.RecordID, field)
	if err != nil {
		return "", fmt.Errorf("read %s %s.%s for department routing: %w", job.EntityType, job.RecordID, field, err)
	}
	return id, nil
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
func approvalRoleFor(ctx context.Context, ts tenantScope, job data.WorkflowJob, defCache map[string]*cachedWorkflowDef) (approvalRequirement, error) {
	step, err := requireApprovalStepFor(ctx, ts, job, defCache)
	if err != nil {
		return approvalRequirement{}, err
	}
	raw, present := step.Params["role"]
	if !present {
		return approvalRequirement{}, nil
	}
	// workflow.Definition.Validate (run by Unmarshal above) already
	// rejects a non-string/empty "role" param at publish time, so this
	// can't actually be false for a definition that made it this far —
	// checked explicitly anyway rather than discarding it (a silently
	// ignored malformed value here would mean "no restriction", which is
	// exactly the fail-open bug this whole check exists to prevent).
	requiredRole, ok := raw.(string)
	if !ok || requiredRole == "" {
		return approvalRequirement{}, fmt.Errorf("workflow job %s: step %d's role param is %#v, not a valid Role code", job.ID, job.StepIndex, raw)
	}
	// department is optional and already validated at publish time; a
	// blank/absent one leaves DepartmentField "" = tenant-wide check.
	deptField, _ := step.Params["department"].(string)
	return approvalRequirement{Role: requiredRole, DepartmentField: deptField}, nil
}

// escalationRoleFor is approvalRoleFor's counterpart for the escalate_role/
// escalate_department pair a require_approval step may name (uc-infra#64).
// An empty approvalRequirement{} (never an error) means the step set no
// escalate_role — there is nothing to widen eligibility with, which is a
// perfectly ordinary step shape, not a data problem. Shares defCache with
// approvalRoleFor: both resolve the exact same Definition/step for the
// same job, so callers that already have a warm cache from one get it for
// the other for free.
func escalationRoleFor(ctx context.Context, ts tenantScope, job data.WorkflowJob, defCache map[string]*cachedWorkflowDef) (approvalRequirement, error) {
	step, err := requireApprovalStepFor(ctx, ts, job, defCache)
	if err != nil {
		return approvalRequirement{}, err
	}
	raw, present := step.Params["escalate_role"]
	if !present {
		return approvalRequirement{}, nil
	}
	// Same publish-time-guaranteed shape as approvalRoleFor's role check —
	// see workflow.Definition.Validate's escalate_role validation.
	requiredRole, ok := raw.(string)
	if !ok || requiredRole == "" {
		return approvalRequirement{}, fmt.Errorf("workflow job %s: step %d's escalate_role param is %#v, not a valid Role code", job.ID, job.StepIndex, raw)
	}
	deptField, _ := step.Params["escalate_department"].(string)
	return approvalRequirement{Role: requiredRole, DepartmentField: deptField}, nil
}

// requireApprovalStepFor resolves the workflow.Step a job is currently
// halted at, validating it's actually a require_approval step — the
// shared definition-lookup/cache/bounds-check core approvalRoleFor and
// escalationRoleFor both need, factored out so the two param pairs they
// read (role/department vs. escalate_role/escalate_department) don't
// duplicate the lookup itself.
func requireApprovalStepFor(ctx context.Context, ts tenantScope, job data.WorkflowJob, defCache map[string]*cachedWorkflowDef) (workflow.Step, error) {
	cacheKey := fmt.Sprintf("%s@%d", job.WorkflowName, job.WorkflowVersion)
	var def *workflow.Definition
	entry, cached := (*cachedWorkflowDef)(nil), false
	if defCache != nil {
		entry, cached = defCache[cacheKey]
	}
	if cached {
		if entry.err != nil {
			return workflow.Step{}, entry.err
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
			return workflow.Step{}, err
		}
		def = resolved
	}
	if job.StepIndex < 0 || job.StepIndex >= len(def.Steps) {
		return workflow.Step{}, fmt.Errorf("workflow job %s: step index %d out of range for %s v%d (%d steps)", job.ID, job.StepIndex, job.WorkflowName, job.WorkflowVersion, len(def.Steps))
	}
	step := def.Steps[job.StepIndex]
	if step.Kind != workflow.StepRequireApproval {
		return workflow.Step{}, fmt.Errorf("workflow job %s: step %d is a %q step, not require_approval — nothing should have halted it waiting for approval", job.ID, job.StepIndex, step.Kind)
	}
	return step, nil
}

// userMayApprove is the ONE place that decides whether userID may approve
// job, combining the primary role/department requirement with the
// escalation one once a job has escalated (uc-infra#64) — used by the
// enforcement gate, the HTML inbox, and the JSON API so none of the three
// can disagree. Escalation is additive, never a replacement: a user who
// meets the ORIGINAL requirement may always still approve, even after
// escalation widened who else may too — see workflow.go's Validate and
// this package's own escalationRoleFor for why (nothing about escalating
// a job revokes the original approver's standing).
//
// req is the primary approvalRequirement (still returned so callers that
// display "what role is required" — the JSON list endpoint — keep doing
// so unchanged; escalation eligibility is not surfaced there, matching
// this task's "no UI changes beyond eligibility" scope).
func userMayApprove(ctx context.Context, ts tenantScope, job data.WorkflowJob, defCache map[string]*cachedWorkflowDef, userID string, memo *approvalMemo) (req approvalRequirement, meets bool, viaDelegator string, err error) {
	req, err = approvalRoleFor(ctx, ts, job, defCache)
	if err != nil {
		return approvalRequirement{}, false, "", err
	}
	meets, viaDelegator, err = userMeetsApproval(ctx, ts, job, req, userID, memo)
	if err != nil {
		return req, false, "", err
	}
	if meets || !job.Escalated {
		return req, meets, viaDelegator, nil
	}
	escalationReq, err := escalationRoleFor(ctx, ts, job, defCache)
	if err != nil {
		return req, false, "", err
	}
	if escalationReq.Role == "" {
		// Defensive: a job can only be marked Escalated by
		// Queue.EscalateOverdueApprovals, which only ever does so for a
		// step that set escalate_role (workflow.Validate requires one
		// alongside escalate_after_hours). An empty Role here would mean
		// the Definition drifted since escalation — userMeetsApproval
		// treats Role == "" as "anyone may approve", which would be a
		// fail-open bug if reached with an escalation requirement, so
		// this never falls through to that call.
		return req, false, "", nil
	}
	meets, viaDelegator, err = userMeetsApproval(ctx, ts, job, escalationReq, userID, memo)
	if err != nil {
		// Explicit rather than returning the (already-false-on-every-
		// error-path-today) meets/viaDelegator as-is — that relied on an
		// invariant of userMeetsApproval's own error handling instead of
		// this function's own contract, the fragile shape an independent
		// review flagged.
		return req, false, "", err
	}
	return req, meets, viaDelegator, nil
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
	// RequiredRole is the Role code this job's current require_approval step
	// demands, empty when it names none (the "anyone may approve" default) or
	// when the job is not waiting for approval at all. CanApprove is whether
	// THIS caller could approve it right now.
	//
	// Both are omitempty so every existing client sees a byte-identical
	// payload for the statuses it already queries — this is additive, not a
	// wire-format change.
	//
	// Advisory, not a security boundary: the approve endpoint re-derives the
	// same answer at click time and is what actually refuses. A client that
	// ignored CanApprove entirely would be no more able to approve than
	// before.
	// Pointers, so the wire can express THREE states rather than two.
	// Independent review proved the earlier value-typed version conflated
	// them: a job whose definition could not be resolved looked byte-for-byte
	// identical to a freely-approvable unrestricted one (both simply omitted
	// the key), so a dashboard filtering on "no required role" would report a
	// broken job — one that 500s for every caller who tries — as open work.
	//
	//	both nil              -> not applicable, or could not be resolved
	//	required_role ""      -> unrestricted, anyone may approve
	//	required_role "x"     -> gated on role "x"; can_approve says whether
	//	                         THIS caller holds it
	//
	// Advisory, not a security boundary: the approve endpoint re-derives the
	// same answer and is what actually refuses.
	RequiredRole *string `json:"required_role,omitempty"`
	CanApprove   *bool   `json:"can_approve,omitempty"`
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

	// Role awareness is DESCRIBED, not enforced by filtering. An API client
	// may legitimately want the whole queue — an ops dashboard counting what
	// is pending should not silently under-report because the token it used
	// happens to hold no roles. So every job is still returned; each one just
	// says whether this caller could approve it, and what it needs.
	//
	// Only resolved for waiting_approval. A job in any other status is not
	// sitting at a require_approval step, and asking approvalRoleFor about it
	// would be asking a question with no meaningful answer (it would report
	// the step-kind mismatch as an error).
	// Department-scoped approval resolves per-job (userMeetsApproval), but
	// the viewer is the same caller on every row of this one request —
	// the definition cache AND userMeetsApproval's own memo (the
	// viewer's delegator set, and any role-code lookup keyed by a
	// user+department pair already seen on an earlier row) are both
	// shared across the page's jobs, not just the definition lookup.
	var defCache map[string]*cachedWorkflowDef
	var memo *approvalMemo
	if status == "waiting_approval" {
		defCache = map[string]*cachedWorkflowDef{}
		memo = newApprovalMemo()
	}

	out := make([]workflowJobResponse, len(jobs))
	for i, j := range jobs {
		out[i] = workflowJobResponse{
			ID: j.ID, WorkflowName: j.WorkflowName, WorkflowVersion: j.WorkflowVersion,
			EntityType: j.EntityType, RecordID: j.RecordID, StepIndex: j.StepIndex, Status: j.Status,
		}
		if defCache == nil {
			continue
		}
		// Same function the approve endpoint and the HTML inbox use, so all
		// three surfaces cannot disagree about what a step requires.
		req, ok, _, err := userMayApprove(r.Context(), ts, j, defCache, rc.Actor.ID, memo)
		if err != nil {
			// A job whose definition or step index will not resolve is a data
			// problem, not this caller's fault. Report the job with BOTH
			// fields nil rather than failing the whole listing: nil is the
			// wire's "no verdict", distinct from an explicit false, so a
			// client is never told it may act when the server could not
			// decide — nor told it may not, which would be equally untrue.
			log.Printf("api: list workflow jobs: resolve approvability for job %s: %v", j.ID, err)
			continue
		}
		role := req.Role
		out[i].RequiredRole = &role
		out[i].CanApprove = &ok
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

	// Department-scoped approval (R17) resolves per-job against the record's
	// own department, so the viewer's roles can no longer be reduced to one
	// tenant-wide set up front — userMeetsApproval does the resolution per
	// row, sharing the definition cache AND its own memo (the viewer's
	// delegator set, and any role-code lookup already seen for a
	// user+department pair) below.
	defCache := map[string]*cachedWorkflowDef{}
	memo := newApprovalMemo()

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
		req, meets, _, err := userMayApprove(r.Context(), ts, j, defCache, rc.Actor.ID, memo)
		switch {
		case err != nil:
			// A job whose definition/step/record can't be resolved is a
			// data problem, not this viewer's fault. Show the row without
			// an Approve button rather than failing the whole page —
			// hiding every other pending approval because one job is
			// malformed would be a worse outcome — and log it, since a
			// silently unactionable row is exactly the confusion this
			// task exists to remove.
			log.Printf("api: workflow inbox: resolve approvability for job %s: %v", j.ID, err)
			row.BlockedReason = h.catalog.T(locale, "workflow_inbox.unavailable")
		case meets:
			row.CanApprove = true
		default:
			// Gated on a role (optionally in a department) the viewer does
			// not hold. Before role-gating existed every step was
			// unrestricted, so this was unreachable; now it is the routine
			// case, and the button used to be offered anyway — clicking it
			// 403s, and htmx does not swap on a non-2xx response, so the
			// click visibly did nothing with no explanation.
			msg := h.catalog.T(locale, "workflow_inbox.requires_role")
			if req.DepartmentField != "" {
				msg = h.catalog.T(locale, "workflow_inbox.requires_role_in_department")
			}
			row.BlockedReason = strings.ReplaceAll(msg, "{role}", req.Role)
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
