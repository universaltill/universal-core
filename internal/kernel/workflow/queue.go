package workflow

import (
	"context"
	"database/sql"
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
// enqueued against, scoped to tenantID — required, not optional: workflow
// definitions are per-tenant (a tenant can customize/version its own
// workflows), and ClaimNext is deliberately tenant-global (a background
// dispatcher polls every tenant's due jobs), so a lookup keyed only on
// name+version with no tenant scope would resolve one tenant's workflow
// definition against another tenant's job — exactly the kind of ambient,
// implicit-tenant-context bug CLAUDE.md's multi-tenancy rule exists to
// rule out. RegistryDefinitionLookup (registry.go) is the real,
// registry-backed implementation; tests build their own stub closures.
type DefinitionLookup func(ctx context.Context, tenantID, name string, version int) (*Definition, error)

// Queue is the durable, Postgres-backed counterpart to the in-memory
// Execute — the "durable, transactional Postgres job queue (retries,
// dead-letter, resumable)" ADR-0001 §9 calls for. One Queue serves every
// tenant and workflow definition; behaviour comes from the Definition a
// job was enqueued against and the StepHandlers registered, never a
// per-entity-type branch (CLAUDE.md's kernel/deterministic-core boundary
// rule).
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
	jobs     *data.WorkflowJobRepo
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
		jobs:     data.NewWorkflowJobRepo(db),
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
func (q *Queue) Enqueue(ctx context.Context, def *Definition, tenantID, entityType, recordID string, actor audit.Actor) (data.WorkflowJob, error) {
	if err := def.Validate(); err != nil {
		return data.WorkflowJob{}, fmt.Errorf("invalid workflow: %w", err)
	}
	return q.jobs.Enqueue(ctx, data.WorkflowJob{
		TenantID:        tenantID,
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
func (q *Queue) ProcessOne(ctx context.Context, lookup DefinitionLookup) (job data.WorkflowJob, err error) {
	job, err = q.jobs.ClaimNext(ctx)
	if err != nil {
		return data.WorkflowJob{}, err
	}

	def, err := lookup(ctx, job.TenantID, job.WorkflowName, job.WorkflowVersion)
	if err != nil {
		return job, q.fail(ctx, job, fmt.Errorf("look up workflow definition: %w", err))
	}
	if err := def.Validate(); err != nil {
		return job, q.fail(ctx, job, fmt.Errorf("invalid workflow definition: %w", err))
	}

	for i := job.StepIndex; i < len(def.Steps); i++ {
		step := def.Steps[i]

		if step.Kind == StepRequireApproval {
			if err := q.jobs.MarkWaitingApproval(ctx, job.TenantID, job.ID, i); err != nil {
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

	if err := q.jobs.MarkDone(ctx, job.TenantID, job.ID, len(def.Steps)); err != nil {
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
	if _, err := q.jobs.MarkFailed(ctx, job.TenantID, job.ID, stepErr, runAfter); err != nil {
		return fmt.Errorf("mark job failed (original error: %v): %w", stepErr, err)
	}
	return nil
}

// ResumeAfterApproval advances a job halted at a require_approval step to
// its next step and requeues it. Called from internal/api's
// approveWorkflowJob (POST /api/workflow-jobs/{id}/approve, added
// 2026-07-21 — see QUEUE.md), the handler for a human's approval action.
// tenantID must be the caller's authenticated tenant scope — this is the
// one method in this package a request handler calls directly with a
// caller-supplied job ID, so the tenant check is load-bearing, not just
// defense in depth.
func (q *Queue) ResumeAfterApproval(ctx context.Context, tenantID, jobID string) error {
	return q.jobs.ResumeAfterApproval(ctx, tenantID, jobID)
}

// ListByStatus returns every tenantID job currently in status, oldest
// first — see data.WorkflowJobRepo.ListByStatus. The read side of the
// approval loop: ResumeAfterApproval resumes a job by id, this is how a
// caller finds which ids are actually waiting without direct DB access.
func (q *Queue) ListByStatus(ctx context.Context, tenantID, status string) ([]data.WorkflowJob, error) {
	return q.jobs.ListByStatus(ctx, tenantID, status)
}

// ReclaimStale requeues jobs stuck in 'running' past leaseTimeout — see
// data.WorkflowJobRepo.ReclaimStale. Call this periodically (e.g. once
// per poll loop, before ProcessOne) from whatever process runs the queue;
// without it, a killed worker's in-flight job is lost forever rather than
// resumed, despite that being this package's whole reason to exist.
func (q *Queue) ReclaimStale(ctx context.Context, leaseTimeout time.Duration) ([]string, error) {
	return q.jobs.ReclaimStale(ctx, leaseTimeout)
}
