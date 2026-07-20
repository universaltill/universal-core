// Package worker runs the durable workflow job queue
// (internal/kernel/workflow.Queue) as a continuous background process —
// the "worker process, not built yet" every doc comment in that package
// points to (registry.go's RegistryDefinitionLookup, queue.go's
// ResumeAfterApproval). The wiring lives here rather than in
// internal/kernel/workflow itself: a poll loop with goroutine lifecycle
// and OS-level configuration is application wiring, not kernel behaviour
// (CLAUDE.md's kernel/deterministic-core boundary rule) — Queue stays a
// pure library any caller can drive, this package is one caller.
package worker

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// Config controls the poll loop. Zero-value fields fall back to sensible
// defaults (see withDefaults), so callers only need to set what they're
// overriding.
type Config struct {
	// PollInterval is how often an idle worker checks for newly-due jobs.
	PollInterval time.Duration
	// LeaseTimeout is how long a job may stay 'running' before
	// ReclaimStale treats its worker as dead and requeues it. Must be
	// comfortably longer than any real step handler's expected duration —
	// see workflow.Queue.ReclaimStale's own doc comment on the tradeoff
	// between reclaiming too eagerly (stealing a job from a worker that's
	// merely slow) and too late (an orphaned job sitting invisible).
	LeaseTimeout time.Duration
	// Concurrency is how many goroutines run the poll loop in parallel
	// within this process. Safe by construction:
	// data.WorkflowJobRepo.ClaimNext uses SELECT ... FOR UPDATE SKIP
	// LOCKED specifically so multiple pollers never claim the same job —
	// concurrency here just trades "jobs wait for the next poll tick" for
	// "more DB connections held polling."
	Concurrency int
}

const (
	defaultPollInterval = 2 * time.Second
	defaultLeaseTimeout = 5 * time.Minute
	defaultConcurrency  = 2
)

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.LeaseTimeout <= 0 {
		c.LeaseTimeout = defaultLeaseTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	return c
}

// Runner drives a workflow.Queue against real time and a real database
// until Run's context is cancelled.
type Runner struct {
	queue  *workflow.Queue
	lookup workflow.DefinitionLookup
	cfg    Config
}

// New builds a Runner. handlers is passed straight through to
// workflow.NewQueue (nil/partial is fine — see that function's doc
// comment on the default no-op notify handler used until a real
// notification connector exists). Definitions are resolved via
// workflow.RegistryDefinitionLookup — the real, registry-backed lookup,
// not a test stub.
func New(db *sql.DB, handlers map[workflow.StepKind]workflow.StepHandler, cfg Config) (*Runner, error) {
	q, err := workflow.NewQueue(db, handlers)
	if err != nil {
		return nil, err
	}
	return &Runner{
		queue:  q,
		lookup: workflow.RegistryDefinitionLookup(db),
		cfg:    cfg.withDefaults(),
	}, nil
}

// Run blocks, polling for due jobs until ctx is cancelled. Run itself is
// single-threaded; call it from multiple goroutines (or use RunConcurrent)
// to get Config.Concurrency pollers sharing one underlying Queue/db,
// matching ClaimNext's SKIP LOCKED design (Config.Concurrency's doc
// comment).
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// RunConcurrent starts Config.Concurrency copies of Run, each in its own
// goroutine, and returns immediately — the convenience entry point a
// process's main function calls. It does not wait for the goroutines to
// finish; callers that need to know when every poller has actually
// stopped after cancelling ctx should not rely on RunConcurrent returning
// for that (it never blocks) and should track shutdown themselves.
func (r *Runner) RunConcurrent(ctx context.Context) {
	for range r.cfg.Concurrency {
		go r.Run(ctx)
	}
}

// tick reclaims any stale (crashed-or-hung-worker) jobs, then drains every
// currently-due job via ProcessOne — not just one — so a burst of work
// arriving between ticks doesn't sit idle until the next poll just because
// PollInterval is coarse.
func (r *Runner) tick(ctx context.Context) {
	if reclaimed, err := r.queue.ReclaimStale(ctx, r.cfg.LeaseTimeout); err != nil {
		log.Printf("worker: reclaim stale jobs: %v", err)
	} else if len(reclaimed) > 0 {
		log.Printf("worker: reclaimed %d stale job(s): %v", len(reclaimed), reclaimed)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, err := r.queue.ProcessOne(ctx, r.lookup)
		if err != nil {
			if errors.Is(err, workflow.ErrNoJobAvailable) {
				return
			}
			// A DB-level error processing/recording the job (ProcessOne's
			// own doc comment: only returned when recording a failure
			// itself failed, not for an ordinary step failure, which it
			// records on the job and returns nil for). Stop draining
			// this tick rather than spin against a database that's
			// currently erroring — the next tick tries again.
			log.Printf("worker: process job: %v", err)
			return
		}
		log.Printf("worker: processed job %s (%s v%d, entity %s/%s) -> step %d",
			job.ID, job.WorkflowName, job.WorkflowVersion, job.EntityType, job.RecordID, job.StepIndex)
	}
}
