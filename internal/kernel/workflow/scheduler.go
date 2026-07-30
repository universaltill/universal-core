package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// Scheduler turns TriggerScheduled workflows into queued jobs (R18) —
// the stateful half of scheduling, on top of cron.go's pure
// next-occurrence logic.
//
// It feeds the EXISTING queue rather than being a second execution
// system: a scheduled run becomes an ordinary workflow_jobs row that the
// same worker, retry and dead-letter machinery already handles. The only
// thing scheduling adds is deciding WHEN to insert it.
//
// Separate from Queue because Queue is deliberately built on repositories
// rather than a *sql.DB, and firing a schedule needs a transaction
// spanning two tables.
type Scheduler struct {
	db        *sql.DB
	schedules *data.WorkflowScheduleRepo
	jobs      *data.WorkflowJobRepo
	defs      *data.WorkflowDefinitionRepo
}

func NewScheduler(db *sql.DB) *Scheduler {
	return &Scheduler{
		db:        db,
		schedules: data.NewWorkflowScheduleRepo(db),
		jobs:      data.NewWorkflowJobRepo(db),
		defs:      data.NewWorkflowDefinitionRepo(db),
	}
}

// Sync reconciles the schedule table against the published workflow
// definitions: every published TriggerScheduled workflow gets a row, and
// rows for workflows that are no longer published-and-scheduled are
// removed.
//
// Reconciled on every tick rather than hooked into publishing. Publishing
// happens through the generic definition registry, which knows nothing
// about scheduling and should not have to — and a reconcile that runs
// continuously is self-healing: a row lost to a restore, or a definition
// published while the worker was down, corrects itself on the next tick
// instead of needing someone to notice.
func (s *Scheduler) Sync(ctx context.Context, now time.Time) error {
	names, err := s.defs.ListPublishedNames(ctx)
	if err != nil {
		return fmt.Errorf("list published workflows: %w", err)
	}
	var scheduled []string
	for _, name := range names {
		v, err := s.defs.GetPublished(ctx, name)
		if errors.Is(err, data.ErrNotFound) {
			// Unpublished between the list and this read — a narrow race,
			// not an error. The next tick sees the settled state.
			continue
		}
		if err != nil {
			// Anything else is a real backend problem, and swallowing it
			// here is not harmless: the trailing DeleteExcept would treat
			// this workflow as no-longer-scheduled and DELETE its row, so
			// a transient DB error would silently unschedule a job. It
			// self-heals next tick, but silent is the dangerous kind of
			// failure in scheduling code. Abort the sync instead — leaving
			// the table untouched is strictly safer than pruning it from
			// incomplete information. (Independent review found this.)
			return fmt.Errorf("read published workflow %s: %w", name, err)
		}
		def, err := Unmarshal(v.Definition)
		if err != nil || def.Trigger.Type != TriggerScheduled {
			continue
		}
		sched, err := ParseSchedule(def.Trigger.Cron)
		if err != nil {
			// Unreachable for anything that passed Validate at publish
			// time, but a definition predating this feature could carry
			// anything. Skip rather than abort the whole sync — one bad
			// definition must not stop every other tenant schedule.
			continue
		}
		next, err := sched.Next(now)
		if err != nil {
			continue
		}
		if err := s.schedules.Upsert(ctx, def.Name, def.Version, def.Trigger.Cron, next); err != nil {
			return err
		}
		scheduled = append(scheduled, def.Name)
	}
	// Prune, so a workflow switched from scheduled to manual — or
	// unpublished — stops firing. Without this the row outlives the
	// intent and keeps creating jobs nothing points at.
	return s.schedules.DeleteExcept(ctx, scheduled)
}

// FireDue enqueues one job per due schedule and advances each to its next
// occurrence, returning the workflow names it fired.
//
// Each schedule is claimed, enqueued and advanced inside ONE transaction
// (FOR UPDATE SKIP LOCKED), so concurrent workers cannot both fire the
// same schedule, and a crash cannot leave a schedule marked fired without
// the job it was meant to create.
//
// The next due time is computed from NOW, not from the missed due time.
// That is the anti-stampede rule: a nightly job whose cluster was down
// for a week is due 7 times over, and catching up would fire 7 runs at
// once — a thundering herd precisely when the system is recovering. One
// run, then back on schedule. Scheduling here means "at least this
// often", never "exactly this many times".
func (s *Scheduler) FireDue(ctx context.Context, now time.Time, actor audit.Actor) ([]string, error) {
	var fired []string
	// Bounded: each iteration claims at most one schedule, and a schedule
	// advanced past `now` cannot be claimed again this call, so the loop
	// drains rather than spins. The cap is a backstop against a clock
	// jump making everything permanently due.
	const maxPerTick = 100
	for i := 0; i < maxPerTick; i++ {
		name, err := s.fireOne(ctx, now, actor)
		if errors.Is(err, data.ErrNoScheduleDue) {
			return fired, nil
		}
		if err != nil {
			return fired, err
		}
		fired = append(fired, name)
	}
	// Hitting the cap means schedules remain due and are deferred to the
	// next tick. Logged rather than silent: truncating work without
	// saying so is how "the nightly job didn't run" becomes unexplainable.
	log.Printf("workflow: fired the per-tick maximum of %d schedules; more remain due and will fire next tick", maxPerTick)
	return fired, nil
}

func (s *Scheduler) fireOne(ctx context.Context, now time.Time, actor audit.Actor) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin schedule tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	sched, err := s.schedules.ClaimDueTx(ctx, tx, now)
	if err != nil {
		return "", err
	}
	parsed, err := ParseSchedule(sched.Cron)
	if err != nil {
		return "", fmt.Errorf("schedule %s has an unparseable expression %q: %w", sched.WorkflowName, sched.Cron, err)
	}
	next, err := parsed.Next(now)
	if err != nil {
		return "", fmt.Errorf("schedule %s: %w", sched.WorkflowName, err)
	}

	// EntityType/RecordID are empty: a scheduled run is not about one
	// record. Steps that need a record are the wrong steps for a
	// scheduled workflow, and leaving these blank makes that explicit
	// rather than inventing a placeholder id that looks real.
	if _, err := s.jobs.EnqueueTx(ctx, tx, data.WorkflowJob{
		WorkflowName:    sched.WorkflowName,
		WorkflowVersion: sched.WorkflowVersion,
		Actor:           actor,
	}); err != nil {
		return "", fmt.Errorf("enqueue scheduled run of %s: %w", sched.WorkflowName, err)
	}
	if err := s.schedules.AdvanceTx(ctx, tx, sched.WorkflowName, now, next); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit schedule fire for %s: %w", sched.WorkflowName, err)
	}
	return sched.WorkflowName, nil
}
