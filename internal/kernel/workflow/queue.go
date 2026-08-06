package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// ErrNoJobAvailable is returned by ProcessOne when no job is currently due.
var ErrNoJobAvailable = data.ErrNoJobAvailable

// StepHandler performs one step's real side effect (e.g. actually sending
// a notification, or calling into the CRUD engine). Returning an error
// triggers a retry with exponential backoff, up to the job's
// MaxAttempts, after which the job moves to dead_letter — nothing fails
// silently, a human has to look. StepRequireApproval never gets a
// handler call; Queue always handles it itself (see ProcessOne).
type StepHandler func(ctx context.Context, job data.WorkflowJob, step Step) error

// DefinitionLookup resolves the workflow.Definition a queued job was
// enqueued against. RegistryDefinitionLookup (registry.go) is the real,
// registry-backed implementation, built against one tenant's own
// database (ADR-0003) — every Queue this package constructs already
// operates against a single tenant's *sql.DB, so a lookup no longer
// needs its own tenant scope; tests build their own stub closures.
type DefinitionLookup func(ctx context.Context, name string, version int) (*Definition, error)

// Queue is the durable, Postgres-backed counterpart to the in-memory
// Execute — the "durable, transactional Postgres job queue (retries,
// dead-letter, resumable)" ADR-0001 §9 calls for. One Queue serves one
// tenant's own database (ADR-0003) and every workflow definition within
// it; behaviour comes from the Definition a job was enqueued against and
// the StepHandlers registered, never a per-entity-type branch (CLAUDE.md's
// kernel/deterministic-core boundary rule). internal/worker is
// responsible for running one Queue per tenant to cover every tenant.
//
// Execute (workflow.go) stays as the simple synchronous, no-side-effect,
// no-persistence executor — useful for validating/previewing a
// Definition's step sequence without a database. Queue.ProcessOne is the
// real execution path: it persists progress, retries transient failures,
// and can resume a job any other worker process picks up after a crash.
// The two intentionally don't share step-iteration code: Execute has no
// handler injection, no attempt counting, and no notion of "resume from
// step N", so factoring them together would mean threading unused
// durability concerns through the simple synchronous path.
type Queue struct {
	db       *sql.DB
	jobs     *data.WorkflowJobRepo
	auditLog *data.AuditRepo
	handlers map[StepKind]StepHandler
	backoff  func(attempt int) time.Duration
}

// NewQueue builds a Queue. handlers may be nil/partial; a default
// StepNotify handler (immediate success, no real side effect) is always
// registered unless overridden, matching Execute's current behaviour
// until a real notification connector exists. StepRequireApproval must
// not have a handler registered — Queue rejects that at NewQueue time,
// since accepting one would silently never be called (ProcessOne always
// intercepts require_approval before consulting handlers).
func NewQueue(db *sql.DB, handlers map[StepKind]StepHandler) (*Queue, error) {
	if _, ok := handlers[StepRequireApproval]; ok {
		return nil, fmt.Errorf("workflow queue: a handler for %q is never called — approval is handled by the queue itself", StepRequireApproval)
	}
	h := map[StepKind]StepHandler{
		StepNotify: func(context.Context, data.WorkflowJob, Step) error { return nil },
	}
	maps.Copy(h, handlers)
	return &Queue{
		db:       db,
		jobs:     data.NewWorkflowJobRepo(db),
		auditLog: data.NewAuditRepo(db),
		handlers: h,
		backoff:  defaultBackoff,
	}, nil
}

// defaultBackoff is a capped exponential backoff: 1s, 2s, 4s, 8s, ...,
// capped at 5 minutes so a job stuck against a flaky dependency doesn't
// wait indefinitely between attempts.
func defaultBackoff(attempt int) time.Duration {
	d := time.Duration(1) * time.Second
	for range attempt {
		d *= 2
		if d >= 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return d
}

// Enqueue validates def, then durably schedules a run of it against one
// record, starting at step 0. An invalid Definition is never queued —
// fail loud before persisting, same discipline as crud.Engine validating
// before it writes.
func (q *Queue) Enqueue(ctx context.Context, def *Definition, entityType, recordID string, actor audit.Actor) (data.WorkflowJob, error) {
	if err := def.Validate(); err != nil {
		return data.WorkflowJob{}, fmt.Errorf("invalid workflow: %w", err)
	}
	return q.jobs.Enqueue(ctx, data.WorkflowJob{
		WorkflowName:    def.Name,
		WorkflowVersion: def.Version,
		EntityType:      entityType,
		RecordID:        recordID,
		Actor:           actor,
	})
}

// ProcessOne claims and runs the single oldest due job, resuming from its
// StepIndex. Returns ErrNoJobAvailable (from data.ErrNoJobAvailable) when
// nothing is due — the caller's poll loop should treat that as "sleep and
// retry", not an error condition.
//
// A StepHandler panic is recovered and treated as an ordinary step
// failure (retried/dead-lettered like any other error) rather than
// crashing the worker process — a handler calling into arbitrary
// caller-supplied code (e.g. a notification connector) is exactly the
// kind of boundary where that matters. This is a second line of defence,
// not the primary one: if the process is killed outright (OOM, SIGKILL)
// between the claim and any Mark* call, no recover runs either, which is
// what ReclaimStale exists for.
//
// A step's StepHandler runs to completion before this method commits
// MarkDone (or the failure/approval-wait equivalents) — those cannot be
// made atomic without transactional-outbox semantics. A caller must not
// treat "the handler ran" (e.g. its own in-process side effect or
// callback) as proof the job is queryable as status=="done" yet; poll
// the row itself if that is what's needed (see internal/worker's
// waitForJobStatus, added for exactly this after uc-infra#115).
func (q *Queue) ProcessOne(ctx context.Context, lookup DefinitionLookup) (job data.WorkflowJob, err error) {
	job, err = q.jobs.ClaimNext(ctx)
	if err != nil {
		return data.WorkflowJob{}, err
	}

	def, err := lookup(ctx, job.WorkflowName, job.WorkflowVersion)
	if err != nil {
		return job, q.fail(ctx, job, fmt.Errorf("look up workflow definition: %w", err))
	}
	if err := def.Validate(); err != nil {
		return job, q.fail(ctx, job, fmt.Errorf("invalid workflow definition: %w", err))
	}

	for i := job.StepIndex; i < len(def.Steps); i++ {
		step := def.Steps[i]

		if step.Kind == StepRequireApproval {
			if err := q.jobs.MarkWaitingApproval(ctx, job.ID, i); err != nil {
				return job, err
			}
			return job, nil
		}

		handler, ok := q.handlers[step.Kind]
		if !ok {
			return job, q.fail(ctx, job, fmt.Errorf("step %d: no handler registered for kind %q", i, step.Kind))
		}
		if stepErr := runHandler(ctx, handler, job, step); stepErr != nil {
			return job, q.fail(ctx, job, fmt.Errorf("step %d (%s): %w", i, step.Kind, stepErr))
		}
	}

	if err := q.jobs.MarkDone(ctx, job.ID, len(def.Steps)); err != nil {
		return job, err
	}
	return job, nil
}

// runHandler calls handler, converting a panic into an error so one
// misbehaving StepHandler can't take the whole worker process down.
func runHandler(ctx context.Context, handler StepHandler, job data.WorkflowJob, step Step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("step handler panicked: %v", r)
		}
	}()
	return handler(ctx, job, step)
}

// fail records a step failure against job. The returned error is nil
// unless recording the failure itself failed (a DB error) — an ordinary
// step failure is expected queue operation, captured in the job's own
// status/last_error, not a caller-facing error from ProcessOne.
func (q *Queue) fail(ctx context.Context, job data.WorkflowJob, stepErr error) error {
	runAfter := time.Now().Add(q.backoff(job.Attempts))
	if _, err := q.jobs.MarkFailed(ctx, job.ID, stepErr, runAfter); err != nil {
		return fmt.Errorf("mark job failed (original error: %v): %w", stepErr, err)
	}
	return nil
}

// ResumeAfterApproval advances a job halted at a require_approval step to
// its next step and requeues it. Called from internal/api's
// approveWorkflowJob (POST /api/workflow-jobs/{id}/approve, added
// 2026-07-21 — see QUEUE.md), the handler for a human's approval action.
// The caller must have already resolved this Queue against the
// authenticated caller's own tenant database (internal/tenantdb.Router)
// — that's what makes a cross-tenant resume impossible now, not a
// tenantID parameter here.
func (q *Queue) ResumeAfterApproval(ctx context.Context, jobID string) error {
	return q.jobs.ResumeAfterApproval(ctx, jobID)
}

// Get returns one job by id — see data.WorkflowJobRepo.Get. The approve
// endpoint's own first step: it needs the job's WorkflowName/Version/
// StepIndex to resolve which step is actually waiting before deciding
// whether the caller may resume it, ahead of calling ResumeAfterApproval.
func (q *Queue) Get(ctx context.Context, jobID string) (data.WorkflowJob, error) {
	return q.jobs.Get(ctx, jobID)
}

// ListByStatus returns every job currently in status, oldest first — see
// data.WorkflowJobRepo.ListByStatus. The read side of the approval loop:
// ResumeAfterApproval resumes a job by id, this is how a caller finds
// which ids are actually waiting without direct DB access.
func (q *Queue) ListByStatus(ctx context.Context, status string) ([]data.WorkflowJob, error) {
	return q.jobs.ListByStatus(ctx, status)
}

// ReclaimStale requeues jobs stuck in 'running' past leaseTimeout — see
// data.WorkflowJobRepo.ReclaimStale. Call this periodically (e.g. once
// per poll loop, before ProcessOne) from whatever process runs the queue;
// without it, a killed worker's in-flight job is lost forever rather than
// resumed, despite that being this package's whole reason to exist.
func (q *Queue) ReclaimStale(ctx context.Context, leaseTimeout time.Duration) ([]string, error) {
	return q.jobs.ReclaimStale(ctx, leaseTimeout)
}

// EscalateOverdueApprovals widens approver eligibility (uc-infra#64,
// internal/api/workflow.go's userMayApprove) for every waiting_approval
// job whose require_approval step set escalate_after_hours and has sat
// past that threshold. Call this periodically (e.g. once per worker tick,
// alongside ReclaimStale/Scheduler.FireDue) from whatever process runs
// the queue — internal/worker does, once per tenant per tick.
//
// Unlike ReclaimStale's single WHERE-clause sweep, the threshold isn't a
// fixed duration: it's a per-step Definition param, so each candidate's
// Definition must be resolved and its step's escalate_after_hours read
// before deciding whether it's actually overdue. One job's Definition
// failing to resolve (a dangling/unpublished workflow_name+version, e.g.)
// is logged and skipped rather than aborting the whole sweep — the same
// per-row resilience ReclaimStale gets for free from operating in pure
// SQL, reproduced here in Go since this method can't.
//
// now is threaded in (not time.Now()) for the same reproducibility reason
// Scheduler.FireDue takes it — a fixed instant makes the "past threshold"
// decision deterministic and testable.
func (q *Queue) EscalateOverdueApprovals(ctx context.Context, now time.Time, lookup DefinitionLookup, actor audit.Actor) ([]string, error) {
	candidates, err := q.jobs.ListWaitingApproval(ctx)
	if err != nil {
		return nil, fmt.Errorf("list waiting-approval jobs: %w", err)
	}

	// Memoized per definition (name@version), not per job: an unmemoized
	// lookup here is a fresh registry SELECT (RegistryDefinitionLookup)
	// per candidate, every tick — the exact N+1 shape this codebase has
	// already paid down twice elsewhere (internal/api/workflow.go's own
	// defCache for this identical problem; worker.go's ScheduleSyncInterval
	// throttling schedule Sync for the same "O(n) scan on every tick"
	// reason). Most waiting_approval jobs share a handful of workflow
	// definitions and most steps have no escalate_after_hours at all, so
	// this collapses what would be O(candidates) registry reads to
	// O(distinct definitions actually seen this sweep).
	defs := map[string]*Definition{}
	defErrs := map[string]error{}
	resolve := func(name string, version int) (*Definition, error) {
		key := fmt.Sprintf("%s@%d", name, version)
		if d, ok := defs[key]; ok {
			return d, nil
		}
		if err, ok := defErrs[key]; ok {
			return nil, err
		}
		d, err := lookup(ctx, name, version)
		if err != nil {
			defErrs[key] = err
			return nil, err
		}
		defs[key] = d
		return d, nil
	}

	var escalated []string
	for _, job := range candidates {
		def, err := resolve(job.WorkflowName, job.WorkflowVersion)
		if err != nil {
			// A bad job shouldn't block the rest of the sweep — see the
			// method doc comment. Nothing durable happens to this job;
			// it's simply reconsidered next tick.
			continue
		}
		if job.StepIndex < 0 || job.StepIndex >= len(def.Steps) {
			continue
		}
		step := def.Steps[job.StepIndex]
		if step.Kind != StepRequireApproval {
			continue
		}
		rawHours, ok := step.Params["escalate_after_hours"]
		if !ok {
			continue
		}
		hours, ok := rawHours.(float64)
		// Definition.Validate() rejects a non-positive or out-of-range
		// value at publish time (see its own doc comment on the upper
		// bound and why); this is the same "a lookup can still return
		// something Validate would have refused" defense-in-depth this
		// package's own tests already exercise for a malformed role
		// param. hours<=0 alone would not catch NaN (NaN<=0 is false)
		// or a value whose *time.Duration conversion overflows/saturates
		// to a negative duration — both would make this branch compute a
		// negative or zero threshold and escalate immediately, the exact
		// inverse of "essentially never" a huge escalate_after_hours is
		// supposed to mean. validEscalateAfterHours applies the same
		// bound Validate does, so a corrupted Definition returned by a
		// bad lookup can't reach the threshold math with a poisoned value
		// either.
		if !ok || !validEscalateAfterHours(hours) {
			continue
		}
		threshold := time.Duration(hours * float64(time.Hour))
		if now.Sub(job.UpdatedAt) < threshold {
			continue
		}

		if err := q.escalateOne(ctx, job, actor); err != nil {
			if errors.Is(err, data.ErrNotFound) {
				// Already escalated or resumed by a concurrent
				// caller/tick between the list and here — not this
				// sweep's job to report.
				continue
			}
			// A per-job failure (e.g. a transient DB error inside this
			// one job's transaction) must not starve every candidate
			// after it in iteration order — same per-row resilience as
			// the lookup-failure branch above. Reconsidered next tick.
			continue
		}
		escalated = append(escalated, job.ID)
	}
	return escalated, nil
}

// escalateOne marks one job escalated and writes its audit row in the
// same transaction (CLAUDE.md: audit written from the same transaction as
// the mutation, never bolted on after) — mirrors
// internal/kernel/crud.Engine.Create's BeginTx -> mutate -> audit.New ->
// auditLog.Insert -> Commit shape.
func (q *Queue) escalateOne(ctx context.Context, job data.WorkflowJob, actor audit.Actor) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	if err := q.jobs.MarkEscalatedTx(ctx, tx, job.ID); err != nil {
		return err
	}

	entry, err := audit.New(job.EntityType, job.RecordID, audit.ActionUpdate, actor, map[string]any{"escalated": true})
	if err != nil {
		return fmt.Errorf("build audit entry: %w", err)
	}
	if err := q.auditLog.Insert(ctx, tx, entry); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
