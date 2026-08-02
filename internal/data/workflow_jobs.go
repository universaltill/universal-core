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
	UpdatedAt       time.Time
	// Escalated is set once Queue.EscalateOverdueApprovals (uc-infra#64)
	// has widened this job's approver eligibility past its
	// escalate_after_hours threshold — see MarkEscalatedTx and
	// internal/api/workflow.go's userMayApprove. Meaningless outside
	// status='waiting_approval'; always false otherwise.
	Escalated bool
	Actor     audit.Actor
}

// ErrNoJobAvailable is returned by ClaimNext when no job is currently due.
var ErrNoJobAvailable = errors.New("data: no workflow job available to claim")

// WorkflowJobRepo is the repository for the durable workflow job queue —
// the only place that runs raw SQL against workflow_jobs (CLAUDE.md).
// Every method operates against one tenant's own database (ADR-0003) —
// internal/worker is responsible for iterating every tenant's database
// when it needs to poll across all of them (ClaimNext/ReclaimStale below
// are each scoped to whichever database this repo was constructed
// against, not "every tenant" the way they were under the old shared-DB
// design).
type WorkflowJobRepo struct {
	db *sql.DB
}

func NewWorkflowJobRepo(db *sql.DB) *WorkflowJobRepo {
	return &WorkflowJobRepo{db: db}
}

// Enqueue durably schedules a workflow run, starting at step 0. Defaults
// MaxAttempts to 5 when unset.
// EnqueueTx is Enqueue against a caller-supplied transaction. Scheduled
// firing needs it: the job insert and the schedule's advance must commit
// together, or a crash between them either loses the run or fires it
// twice forever.
func (r *WorkflowJobRepo) EnqueueTx(ctx context.Context, tx *sql.Tx, job WorkflowJob) (WorkflowJob, error) {
	return r.enqueue(ctx, tx, job)
}

func (r *WorkflowJobRepo) Enqueue(ctx context.Context, job WorkflowJob) (WorkflowJob, error) {
	return r.enqueue(ctx, r.db, job)
}

func (r *WorkflowJobRepo) enqueue(ctx context.Context, q querier, job WorkflowJob) (WorkflowJob, error) {
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	var modelVersion any
	if job.Actor.ModelVersion != "" {
		modelVersion = job.Actor.ModelVersion
	}
	// A scheduled run has no triggering record (R18). NULL rather than a
	// placeholder: record_id is a UUID column, "" is not a UUID, and a
	// synthetic one would look like a real record to every reader.
	var entityType, recordID any
	if job.EntityType != "" {
		entityType = job.EntityType
	}
	if job.RecordID != "" {
		recordID = job.RecordID
	}
	err := q.QueryRowContext(ctx,
		`INSERT INTO workflow_jobs
		 (workflow_name, workflow_version, entity_type, record_id,
		  max_attempts, actor_type, actor_id, model_version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, step_index, status, attempts, run_after`,
		job.WorkflowName, job.WorkflowVersion, entityType, recordID,
		job.MaxAttempts, string(job.Actor.Type), job.Actor.ID, modelVersion,
	).Scan(&job.ID, &job.StepIndex, &job.Status, &job.Attempts, &job.RunAfter)
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("enqueue workflow job: %w", err)
	}
	return job, nil
}

// ClaimNext atomically claims the oldest due, queued job in this
// database and marks it running, using SELECT ... FOR UPDATE SKIP LOCKED
// so multiple worker processes can poll this table concurrently without
// claiming the same job or blocking on each other's claims. Returns
// ErrNoJobAvailable when nothing is due yet.
func (r *WorkflowJobRepo) ClaimNext(ctx context.Context) (WorkflowJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowJob{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	var j WorkflowJob
	var modelVersion sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, attempts, max_attempts, actor_type, actor_id, model_version
		 FROM workflow_jobs
		 WHERE status = 'queued' AND run_after <= now()
		 ORDER BY run_after
		 FOR UPDATE SKIP LOCKED
		 LIMIT 1`,
	).Scan(&j.ID, &j.WorkflowName, &j.WorkflowVersion, &nullEntityType{&j.EntityType}, &nullEntityType{&j.RecordID},
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
// step ran). Guarded by status = 'running': see the package-level note
// above ReclaimStale on why every Mark* method needs this guard, not
// just an id match.
func (r *WorkflowJobRepo) MarkDone(ctx context.Context, id string, stepIndex int) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs SET status = 'done', step_index = $2, updated_at = now()
		 WHERE id = $1 AND status = 'running'`,
		id, stepIndex)
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
func (r *WorkflowJobRepo) MarkWaitingApproval(ctx context.Context, id string, stepIndex int) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs SET status = 'waiting_approval', step_index = $2, updated_at = now()
		 WHERE id = $1 AND status = 'running'`,
		id, stepIndex)
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
func (r *WorkflowJobRepo) MarkFailed(ctx context.Context, id string, stepErr error, runAfter time.Time) (status string, err error) {
	row := r.db.QueryRowContext(ctx,
		`UPDATE workflow_jobs
		 SET attempts = attempts + 1,
		     last_error = $2,
		     status = CASE WHEN attempts + 1 >= max_attempts THEN 'dead_letter' ELSE 'queued' END,
		     run_after = CASE WHEN attempts + 1 >= max_attempts THEN run_after ELSE $3 END,
		     updated_at = now()
		 WHERE id = $1 AND status = 'running'
		 RETURNING status`,
		id, stepErr.Error(), runAfter,
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
// pending step. This is the method a future HTTP approval endpoint calls
// directly with a caller-supplied job ID — the *sql.DB this repo was
// constructed against must already be the caller's own tenant's
// database (resolved via internal/tenantdb.Router before this call),
// which is what makes a cross-tenant resume impossible now: there is no
// row belonging to another tenant in this database to accidentally
// match. Only valid from 'waiting_approval'; returns ErrNotFound if the
// job isn't in that state (already resumed, never halted, or doesn't
// exist in this database).
//
// Also resets `escalated` back to false (uc-infra#64 regression an
// independent review caught): without this, a job whose FIRST
// require_approval step escalated would carry that flag straight into
// its NEXT require_approval step — that step's own escalate_role holder
// would be granted approval with zero elapsed time, and the step would
// also become permanently invisible to ListWaitingApproval's
// `escalated = false` filter. Each require_approval step gets its own
// escalation clock, starting fresh.
func (r *WorkflowJobRepo) ResumeAfterApproval(ctx context.Context, id string) error {
	n, err := execRows(ctx, r.db,
		`UPDATE workflow_jobs
		 SET status = 'queued', step_index = step_index + 1, run_after = now(), updated_at = now(), escalated = false
		 WHERE id = $1 AND status = 'waiting_approval'`,
		id)
	if err != nil {
		return fmt.Errorf("resume workflow job: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *WorkflowJobRepo) Get(ctx context.Context, id string) (WorkflowJob, error) {
	var j WorkflowJob
	var modelVersion, lastError sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, status, attempts, max_attempts, last_error, run_after,
		        updated_at, escalated, actor_type, actor_id, model_version
		 FROM workflow_jobs WHERE id = $1`,
		id,
	).Scan(&j.ID, &j.WorkflowName, &j.WorkflowVersion, &nullEntityType{&j.EntityType}, &nullEntityType{&j.RecordID},
		&j.StepIndex, &j.Status, &j.Attempts, &j.MaxAttempts, &lastError, &j.RunAfter,
		&j.UpdatedAt, &j.Escalated, &j.Actor.Type, &j.Actor.ID, &modelVersion)
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

// ListByStatus returns every job currently in status, oldest first —
// what a task-list/inbox needs to show a human what's actually waiting
// on them ("waiting_approval") without requiring the caller to already
// know a job id (approveWorkflowJob's own gap: it resumes a job by id,
// but nothing before this could tell a caller which ids exist).
// idx_workflow_jobs_status backs this directly.
func (r *WorkflowJobRepo) ListByStatus(ctx context.Context, status string) ([]WorkflowJob, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, status, attempts, max_attempts, last_error, run_after,
		        updated_at, escalated, actor_type, actor_id, model_version
		 FROM workflow_jobs WHERE status = $1 ORDER BY created_at`,
		status,
	)
	if err != nil {
		return nil, fmt.Errorf("list workflow jobs by status: %w", err)
	}
	defer rows.Close()

	var out []WorkflowJob
	for rows.Next() {
		var j WorkflowJob
		var modelVersion, lastError sql.NullString
		if err := rows.Scan(&j.ID, &j.WorkflowName, &j.WorkflowVersion, &nullEntityType{&j.EntityType}, &nullEntityType{&j.RecordID},
			&j.StepIndex, &j.Status, &j.Attempts, &j.MaxAttempts, &lastError, &j.RunAfter,
			&j.UpdatedAt, &j.Escalated, &j.Actor.Type, &j.Actor.ID, &modelVersion); err != nil {
			return nil, fmt.Errorf("scan workflow job: %w", err)
		}
		if modelVersion.Valid {
			j.Actor.ModelVersion = modelVersion.String
		}
		if lastError.Valid {
			j.LastError = lastError.String
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListWaitingApproval returns every job currently parked at
// waiting_approval that has not yet escalated — the candidate set
// Queue.EscalateOverdueApprovals (uc-infra#64) sweeps each tick. Unlike
// ReclaimStale, the elapsed-time-vs-threshold comparison can't happen in
// this query: escalate_after_hours lives on the step's own Params in the
// workflow.Definition, not on the job row, so the caller resolves each
// job's Definition/Step and compares against UpdatedAt itself. Scoped to
// this database only, same as every other method here (ADR-0003).
func (r *WorkflowJobRepo) ListWaitingApproval(ctx context.Context) ([]WorkflowJob, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, workflow_name, workflow_version, entity_type, record_id,
		        step_index, status, attempts, max_attempts, last_error, run_after,
		        updated_at, escalated, actor_type, actor_id, model_version
		 FROM workflow_jobs WHERE status = 'waiting_approval' AND escalated = false ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list waiting-approval workflow jobs: %w", err)
	}
	defer rows.Close()

	var out []WorkflowJob
	for rows.Next() {
		var j WorkflowJob
		var modelVersion, lastError sql.NullString
		if err := rows.Scan(&j.ID, &j.WorkflowName, &j.WorkflowVersion, &nullEntityType{&j.EntityType}, &nullEntityType{&j.RecordID},
			&j.StepIndex, &j.Status, &j.Attempts, &j.MaxAttempts, &lastError, &j.RunAfter,
			&j.UpdatedAt, &j.Escalated, &j.Actor.Type, &j.Actor.ID, &modelVersion); err != nil {
			return nil, fmt.Errorf("scan workflow job: %w", err)
		}
		if modelVersion.Valid {
			j.Actor.ModelVersion = modelVersion.String
		}
		if lastError.Valid {
			j.LastError = lastError.String
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkEscalatedTx flips a waiting_approval job's Escalated flag, within a
// caller-supplied transaction so the flag and its audit row commit or
// roll back together (CLAUDE.md: audit written from the same transaction
// as the mutation, never bolted on after) — see
// Queue.EscalateOverdueApprovals, the only caller. Guarded by
// `status = 'waiting_approval' AND escalated = false`, the same
// optimistic idiom as every other Mark* method: a second sweep tick (or
// a racing approval that resumed the job first) affects zero rows and
// gets ErrNotFound back rather than double-escalating or resurrecting a
// job that already moved on.
func (r *WorkflowJobRepo) MarkEscalatedTx(ctx context.Context, tx *sql.Tx, id string) error {
	// Deliberately does NOT touch updated_at, unlike every other Mark*
	// method here: this column doubles as "entered waiting_approval at"
	// (see this file's own package doc / the migration's comment on why
	// no separate timestamp column exists), and escalating a job must not
	// reset that clock — an independent review flagged an earlier draft
	// that did as contradicting the exact invariant the whole
	// no-new-column decision rests on. escalated=false already excludes
	// this row from ListWaitingApproval's candidate set once escalated,
	// so there is nothing left to time here anyway.
	n, err := execRows(ctx, tx,
		`UPDATE workflow_jobs SET escalated = true
		 WHERE id = $1 AND status = 'waiting_approval' AND escalated = false`,
		id)
	if err != nil {
		return fmt.Errorf("mark workflow job escalated: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
// forever. A sweep over this database only — internal/worker iterates
// every tenant's own database to cover all of them (ADR-0003), this
// method itself has no cross-tenant concept anymore. Returns the
// reclaimed job IDs so a caller can log/alert; call this periodically
// (e.g. once per poll loop) from whatever process runs ProcessOne.
//
// Reclaiming isn't the only case this table's writes need to guard
// against: a worker isn't always dead just because it's stale — it can
// simply be slow (a step handler running longer than leaseTimeout while
// still perfectly alive). If ReclaimStale requeues that job out from
// under it, and the original worker later finishes and calls MarkDone/
// MarkFailed/MarkWaitingApproval anyway, an UPDATE keyed only on id
// would happily resurrect a job ReclaimStale had already requeued or
// dead-lettered — completed work un-completing itself, or a
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
// distinguish "no such row" from a real driver error.
func execRows(ctx context.Context, ex execer, query string, args ...any) (int64, error) {
	res, err := ex.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nullEntityType scans a nullable text/uuid column into a plain string,
// mapping SQL NULL to "". Scheduled workflow runs (R18) have no
// triggering record, so entity_type/record_id are NULL for them — see
// migrations/tenant/0003_system_actor.sql. A plain *string cannot scan
// NULL, and sql.NullString at every call site would push this detail
// into every caller for a case only one of them cares about.
type nullEntityType struct{ dst *string }

func (n *nullEntityType) Scan(v any) error {
	if v == nil {
		*n.dst = ""
		return nil
	}
	switch t := v.(type) {
	case string:
		*n.dst = t
	case []byte:
		*n.dst = string(t)
	default:
		return fmt.Errorf("cannot scan %T into a string column", v)
	}
	return nil
}
