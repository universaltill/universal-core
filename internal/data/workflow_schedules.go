package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WorkflowSchedule is one scheduled workflow's standing intent to run —
// see migrations/tenant/0002_workflow_schedules.sql.
type WorkflowSchedule struct {
	WorkflowName    string
	WorkflowVersion int
	Cron            string
	NextDueAt       time.Time
	LastFiredAt     *time.Time
}

type WorkflowScheduleRepo struct {
	db *sql.DB
}

func NewWorkflowScheduleRepo(db *sql.DB) *WorkflowScheduleRepo {
	return &WorkflowScheduleRepo{db: db}
}

// Upsert registers a scheduled workflow, or updates it when its version
// or expression has changed.
//
// nextDueAt is only applied on INSERT, or when the expression actually
// changed. A schedule whose cron is unchanged keeps its existing
// next_due_at: recomputing it on every worker tick would push the due
// time forward continuously and the job would never fire — the schedule
// would look perfectly healthy in the table while doing nothing at all.
func (r *WorkflowScheduleRepo) Upsert(ctx context.Context, name string, version int, cron string, nextDueAt time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO workflow_schedules (workflow_name, workflow_version, cron, next_due_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (workflow_name) DO UPDATE SET
		     workflow_version = EXCLUDED.workflow_version,
		     cron             = EXCLUDED.cron,
		     next_due_at      = CASE
		                          WHEN workflow_schedules.cron IS DISTINCT FROM EXCLUDED.cron
		                          THEN EXCLUDED.next_due_at
		                          ELSE workflow_schedules.next_due_at
		                        END,
		     updated_at       = now()`,
		name, version, cron, nextDueAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("upsert workflow schedule %s: %w", name, err)
	}
	return nil
}

// DeleteExcept removes schedules whose workflow is no longer published as
// a scheduled one — otherwise a workflow changed from `scheduled` to
// `manual`, or unpublished entirely, would keep firing forever from a row
// nothing points at any more.
func (r *WorkflowScheduleRepo) DeleteExcept(ctx context.Context, keep []string) error {
	if len(keep) == 0 {
		if _, err := r.db.ExecContext(ctx, `DELETE FROM workflow_schedules`); err != nil {
			return fmt.Errorf("clear workflow schedules: %w", err)
		}
		return nil
	}
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM workflow_schedules WHERE workflow_name <> ALL($1)`, keep,
	); err != nil {
		return fmt.Errorf("prune workflow schedules: %w", err)
	}
	return nil
}

// ErrNoScheduleDue is returned by ClaimDueTx when nothing is due.
var ErrNoScheduleDue = errors.New("data: no workflow schedule due")

// ClaimDueTx locks one due schedule inside the caller's transaction and
// returns it, or ErrNoScheduleDue.
//
// FOR UPDATE SKIP LOCKED, exactly as the job queue itself claims work:
// several workers tick the same tenant concurrently, and without the lock
// two of them would read the same due row and enqueue the same run twice.
// SKIP LOCKED rather than plain FOR UPDATE so a second worker moves on to
// another schedule instead of blocking on this one.
//
// The caller is expected to enqueue the job and call AdvanceTx in the
// SAME transaction, so a schedule can never be marked fired without the
// job it was supposed to create actually existing.
func (r *WorkflowScheduleRepo) ClaimDueTx(ctx context.Context, tx *sql.Tx, now time.Time) (WorkflowSchedule, error) {
	var s WorkflowSchedule
	err := tx.QueryRowContext(ctx,
		`SELECT workflow_name, workflow_version, cron, next_due_at, last_fired_at
		   FROM workflow_schedules
		  WHERE next_due_at <= $1
		  ORDER BY next_due_at
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1`,
		now.UTC(),
	).Scan(&s.WorkflowName, &s.WorkflowVersion, &s.Cron, &s.NextDueAt, &s.LastFiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowSchedule{}, ErrNoScheduleDue
	}
	if err != nil {
		return WorkflowSchedule{}, fmt.Errorf("claim due workflow schedule: %w", err)
	}
	return s, nil
}

// AdvanceTx records a fire and sets the next due time, in the caller's
// transaction.
func (r *WorkflowScheduleRepo) AdvanceTx(ctx context.Context, tx *sql.Tx, name string, firedAt, nextDueAt time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_schedules
		    SET last_fired_at = $2, next_due_at = $3, updated_at = now()
		  WHERE workflow_name = $1`,
		name, firedAt.UTC(), nextDueAt.UTC(),
	); err != nil {
		return fmt.Errorf("advance workflow schedule %s: %w", name, err)
	}
	return nil
}

// List returns every schedule, for tests and diagnostics.
func (r *WorkflowScheduleRepo) List(ctx context.Context) ([]WorkflowSchedule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT workflow_name, workflow_version, cron, next_due_at, last_fired_at
		   FROM workflow_schedules ORDER BY workflow_name`)
	if err != nil {
		return nil, fmt.Errorf("list workflow schedules: %w", err)
	}
	defer rows.Close()
	var out []WorkflowSchedule
	for rows.Next() {
		var s WorkflowSchedule
		if err := rows.Scan(&s.WorkflowName, &s.WorkflowVersion, &s.Cron, &s.NextDueAt, &s.LastFiredAt); err != nil {
			return nil, fmt.Errorf("scan workflow schedule: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
