package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// WorkflowJob is one durable, queued run of a workflow.Definition against
// one record — the persisted counterpart of the in-memory Execute run,
// resumable across worker restarts via StepIndex.
type WorkflowJob struct {
	ID              string
	TenantID        string
	WorkflowName    string
	WorkflowVersion int
	EntityType      string
	RecordID        string
	StepIndex       int
	Status          string
	Attempts        int
	MaxAttempts     int
	LastError       string
	RunAfter        time.Time
	Actor           audit.Actor
}

// ErrNoJobAvailable is returned by ClaimNext when no job is currently due.
var ErrNoJobAvailable = errors.New("data: no workflow job available to claim")

// WorkflowJobRepo is the repository for the durable workflow job queue —
// the only place that runs raw SQL against workflow_jobs (CLAUDE.md).
type WorkflowJobRepo struct {
	db *sql.DB
}

func NewWorkflowJobRepo(db *sql.DB) *WorkflowJobRepo {
	return &WorkflowJobRepo{db: db}
}

// Enqueue durably schedules a workflow run, starting at step 0. Defaults
// MaxAttempts to 5 when unset.
func (r *WorkflowJobRepo) Enqueue(ctx context.Context, job WorkflowJob) (WorkflowJob, error) {
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	var modelVersion any
	if job.Actor.ModelVersion != "" {
		modelVersion = job.Actor.ModelVersion
	}
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO workflow_jobs
		 (tenant_id, workflow_name, workflow_version, entity_type, record_id,
		  max_attempts, actor_type, actor_id, model_version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, step_index, status, attempts, run_after`,
		job.TenantID, job.WorkflowName, job.WorkflowVersion, job.EntityType, job.RecordID,
		job.MaxAttempts, string(job.Actor.Type), job.Actor.ID, modelVersion,
	).Scan(&job.ID, &job.StepIndex, &job.Status, &job.Attempts, &job.RunAfter)
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("enqueue workflow job: %w", err)
	}
	return job, nil
}

// ClaimNext atomically claims the oldest due, queued job and marks it
// running, using SELECT ... FOR UPDATE SKIP LOCKED so multiple worker
// processes can poll this table concurrently without claiming the same
// job or blocking on each other's claims. Returns ErrNoJobAvailable when
// nothing is due yet.
func (r *WorkflowJobRepo) ClaimNext(ctx context.Context) (WorkflowJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	var j WorkflowJob
	var modelVersion sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, tenant_id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, attempts, max_attempts, actor_type, actor_id, model_version
		 FROM workflow_jobs
		 WHERE status = 'queued' AND run_after <= now()
		 ORDER BY run_after
		 FOR UPDATE SKIP LOCKED
		 LIMIT 1`,
	).Scan(&j.ID, &j.TenantID, &j.WorkflowName, &j.WorkflowVersion, &j.EntityType, &j.RecordID,
		&j.StepIndex, &j.Attempts, &j.MaxAttempts, &j.Actor.Type, &j.Actor.ID, &modelVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowJob{}, ErrNoJobAvailable
	}
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("claim workflow job: %w", err)
	}
	if modelVersion.Valid {
		j.Actor.ModelVersion = modelVersion.String
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() WHERE id = $1`, j.ID,
	); err != nil {
		return WorkflowJob{}, fmt.Errorf("mark workflow job running: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkflowJob{}, fmt.Errorf("commit claim: %w", err)
	}
	j.Status = "running"
	return j, nil
}

// MarkDone completes a job at stepIndex (== len(def.Steps) when every
// step ran). Scoped by tenantID (CLAUDE.md's multi-tenancy rule) even
// though today's only caller (Queue.ProcessOne) already has the job's own
// tenant_id from ClaimNext — cheap defense in depth, and consistent with
// every other by-ID method below. Guarded by status = 'running': see the
// package-level note above ReclaimStale on why every Mark* method needs
// this guard, not just a tenant/id match.
func (r *WorkflowJobRepo) MarkDone(ctx context.Context, tenantID, id string, stepIndex int) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs SET status = 'done', step_index = $3, updated_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND status = 'running'`,
		id, tenantID, stepIndex)
	if err != nil {
		return fmt.Errorf("mark workflow job done: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkWaitingApproval halts a job at a require_approval step. stepIndex
// is the approval step's own index, so ResumeAfterApproval's step_index+1
// resumes at the step after it. Guarded by status = 'running' — see the
// package-level note above ReclaimStale.
func (r *WorkflowJobRepo) MarkWaitingApproval(ctx context.Context, tenantID, id string, stepIndex int) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs SET status = 'waiting_approval', step_index = $3, updated_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND status = 'running'`,
		id, tenantID, stepIndex)
	if err != nil {
		return fmt.Errorf("mark workflow job waiting_approval: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFailed records a step failure. If attempts remain, the job goes
// back to 'queued' with the given backoff run_after; once max_attempts is
// reached it moves to 'dead_letter' and stops being retried — a human has
// to look at it, it never disappears silently. Computed in one atomic
// UPDATE (not read-attempts-then-write) so two workers can't race past
// max_attempts. Guarded by status = 'running' — see the package-level
// note above ReclaimStale.
func (r *WorkflowJobRepo) MarkFailed(ctx context.Context, tenantID, id string, stepErr error, runAfter time.Time) (status string, err error) {
	row := r.db.QueryRowContext(ctx,
		`UPDATE workflow_jobs
		 SET attempts = attempts + 1,
		     last_error = $3,
		     status = CASE WHEN attempts + 1 >= max_attempts THEN 'dead_letter' ELSE 'queued' END,
		     run_after = CASE WHEN attempts + 1 >= max_attempts THEN run_after ELSE $4 END,
		     updated_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND status = 'running'
		 RETURNING status`,
		id, tenantID, stepErr.Error(), runAfter,
	)
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("mark workflow job failed: %w", err)
	}
	return status, nil
}

// ResumeAfterApproval requeues a job halted at a require_approval step,
// advancing past it — the durable counterpart of a human approving a
// pending step. Tenant-scoped: this is the method a future HTTP approval
// endpoint calls directly with a caller-supplied job ID, so the tenant
// check here is load-bearing, not just defense in depth (a request for
// tenant A must not be able to resume tenant B's job). Only valid from
// 'waiting_approval'; returns ErrNotFound if the job isn't in that state
// (already resumed, never halted, or belongs to a different tenant).
func (r *WorkflowJobRepo) ResumeAfterApproval(ctx context.Context, tenantID, id string) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs
		 SET status = 'queued', step_index = step_index + 1, run_after = now(), updated_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND status = 'waiting_approval'`,
		id, tenantID)
	if err != nil {
		return fmt.Errorf("resume workflow job: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *WorkflowJobRepo) Get(ctx context.Context, tenantID, id string) (WorkflowJob, error) {
	var j WorkflowJob
	var modelVersion, lastError sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, status, attempts, max_attempts, last_error, run_after,
		        actor_type, actor_id, model_version
		 FROM workflow_jobs WHERE id = $1 AND tenant_id = $2`,
		id, tenantID,
	).Scan(&j.ID, &j.TenantID, &j.WorkflowName, &j.WorkflowVersion, &j.EntityType, &j.RecordID,
		&j.StepIndex, &j.Status, &j.Attempts, &j.MaxAttempts, &lastError, &j.RunAfter,
		&j.Actor.Type, &j.Actor.ID, &modelVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowJob{}, ErrNotFound
	}
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("get workflow job: %w", err)
	}
	if modelVersion.Valid {
		j.Actor.ModelVersion = modelVersion.String
	}
	if lastError.Valid {
		j.LastError = lastError.String
	}
	return j, nil
}

// ReclaimStale requeues jobs stuck in 'running' whose updated_at is older
// than leaseTimeout — the reaper for a worker that was SIGKILL'd, OOM'd,
// or panicked between ClaimNext's commit (which releases the row lock)
// and its next MarkDone/MarkFailed/MarkWaitingApproval call. Without this,
// such a job is invisible to ClaimNext forever (it only matches
// status='queued') despite the migration's "picked up by another worker
// rather than lost" intent. Treated like a failure for attempt-counting —
// not an unconditional reset to 'queued' — so a job that reliably crashes
// its worker (a poison pill) still dead-letters instead of being reclaimed
// forever. Global across tenants, like ClaimNext: this is a background
// maintenance sweep, not a tenant-scoped request. Returns the reclaimed
// job IDs so a caller can log/alert; call this periodically (e.g. once
// per poll loop) from whatever process runs ProcessOne.
//
// Reclaiming isn't the only case this table's writes need to guard
// against: a worker isn't always dead just because it's stale — it can
// simply be slow (a step handler running longer than leaseTimeout while
// still perfectly alive). If ReclaimStale requeues that job out from
// under it, and the original worker later finishes and calls MarkDone/
// MarkFailed/MarkWaitingApproval anyway, an UPDATE keyed only on id+
// tenant_id would happily resurrect a job ReclaimStale had already
// requeued or dead-lettered — completed work un-completing itself, or a
// dead-lettered job silently un-dead-lettering. That's why every Mark*
// method above adds `AND status = 'running'`: once ReclaimStale (or
// another worker, in principle) has moved a job off 'running', the
// original worker's own Mark* call now affects zero rows and returns
// ErrNotFound instead of clobbering whatever state the job moved to —
// the stale worker's result is simply discarded, which is correct,
// since ReclaimStale already counted that lease timeout as a failed
// attempt. This doesn't close the race entirely (a fence token tied to
// the specific claim would be needed for that — see QUEUE.md), but it
// closes the resurrection/clobbering failure mode, which is the
// dangerous part: silent data corruption rather than a merely-redundant
// retry.
func (r *WorkflowJobRepo) ReclaimStale(ctx context.Context, leaseTimeout time.Duration) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`UPDATE workflow_jobs
		 SET attempts = attempts + 1,
		     last_error = 'reclaimed: worker did not report completion within lease',
		     status = CASE WHEN attempts + 1 >= max_attempts THEN 'dead_letter' ELSE 'queued' END,
		     run_after = CASE WHEN attempts + 1 >= max_attempts THEN run_after ELSE now() END,
		     updated_at = now()
		 WHERE status = 'running' AND updated_at < now() - ($1::float8 * interval '1 second')
		 RETURNING id`,
		leaseTimeout.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("reclaim stale workflow jobs: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan reclaimed workflow job id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// execRows runs an UPDATE and returns rows affected, so by-ID methods can
// distinguish "no such row for this tenant" from a real driver error.
func execRows(ctx context.Context, ex execer, query string, args ...any) (int64, error) {
	res, err := ex.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
