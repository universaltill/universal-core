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
	"sync"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
	"github.com/universaltill/universal-core/internal/tenantdb"
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
	// ScheduleSyncInterval is how often (per tenant, at most) the
	// scheduler reconciles workflow_schedules against the published
	// definitions. Zero means the 30s default. Separate from PollInterval
	// deliberately: FireDue is one cheap indexed query and belongs on
	// every tick so a due schedule fires promptly, but Sync reads every
	// published definition — running THAT every 2s per tenant is an O(n)
	// registry scan forever, and it measurably slowed ticks enough to
	// flake the multi-tenant timing test on a busy CI runner. A schedule
	// published mid-interval waits at most this long to be noticed;
	// firing latency is unaffected.
	ScheduleSyncInterval time.Duration
	// Concurrency is how many goroutines run the poll loop in parallel
	// within this process. Safe by construction:
	// data.WorkflowJobRepo.ClaimNext uses SELECT ... FOR UPDATE SKIP
	// LOCKED specifically so multiple pollers never claim the same job —
	// concurrency here just trades "jobs wait for the next poll tick" for
	// "more DB connections held polling."
	Concurrency int
	// DepreciationPostInterval is how often (per tenant, at most)
	// assets.PostDueDepreciationBatch runs (uc-infra#76, ADR-0022). Zero
	// means the 30s default. Separate from PollInterval for the same
	// reason ScheduleSyncInterval is: scanning every DepreciationSchedule
	// row in the tenant is an O(n) table scan, and a depreciation posting
	// is due at most once a day per asset — running that every 2s forever
	// is pure waste for a job nothing needs sub-minute latency on. A
	// period that came due mid-interval waits at most this long to post.
	DepreciationPostInterval time.Duration
	// DepreciationPostBatchSize caps how many DepreciationSchedule rows
	// one assets.PostDueDepreciationBatch call posts (uc-infra#137,
	// ADR-0025). Zero means the 200-row default. This is the actual fix
	// for uc-infra#137, not DepreciationPostInterval above:
	// DepreciationPostInterval only throttles how OFTEN a run starts —
	// nothing previously bounded how LONG one run could take, and
	// tick()'s sequential per-tenant loop means a slow run for one
	// tenant (a large catch-up backlog — exactly the scenario this
	// job's own "catch-up, not skip" design intentionally supports)
	// delayed every OTHER tenant's tick, including the queued-job work a
	// user is actually waiting on. A tenant whose backlog exceeds this
	// cap simply resumes posting the remainder on its next
	// DepreciationPostInterval tick, same latency model as
	// DepreciationPostInterval's own "waits at most this long" tradeoff.
	DepreciationPostBatchSize int
}

const (
	defaultPollInterval             = 2 * time.Second
	defaultLeaseTimeout             = 5 * time.Minute
	defaultConcurrency              = 2
	defaultDepreciationPostInterval = 30 * time.Second
	// defaultDepreciationPostBatchSize (uc-infra#137, ADR-0025):
	// assets.PostDueDepreciationBatch does one DB transaction per
	// attempted row (postDepreciationRow), so the POSTING portion of a
	// call's wall-clock cost is roughly linear in rows attempted, capped
	// here. This does NOT bound the whole call: PostDueDepreciationBatch
	// still reads and decodes every DepreciationSchedule row in the
	// tenant on every call (assets.records.List has no due-filter or
	// LIMIT — see that function's own doc comment) before applying this
	// cap, so a tenant with a very large total schedule row count still
	// pays that full-scan cost each call, capped or not — tracked as a
	// follow-up (uc-infra#182) rather than fixed here, since fixing it
	// for real needs a due-filtered/LIMITed internal/data query method,
	// not a worker-side change. 200 keeps the POSTING portion of a
	// single call's worst case to a small fraction of a second —
	// bounding how long one tenant can delay every other tenant's tick
	// on that part — while still making real catch-up progress every
	// DepreciationPostInterval. Deployments with unusually large
	// per-tenant asset counts can raise this via Config.
	defaultDepreciationPostBatchSize = 200
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
	if c.DepreciationPostInterval <= 0 {
		c.DepreciationPostInterval = defaultDepreciationPostInterval
	}
	if c.DepreciationPostBatchSize <= 0 {
		c.DepreciationPostBatchSize = defaultDepreciationPostBatchSize
	}
	return c
}

// Runner drives a workflow.Queue against real time and a real database
// until Run's context is cancelled. Since database-per-tenant (ADR-0003),
// a workflow_jobs table only ever holds one tenant's own jobs — Runner
// fans out across every tenant's own database each tick (see tick),
// resolving each one's *sql.DB and building a fresh workflow.Queue
// against it, rather than driving a single shared Queue the way this
// type did against the old shared database.
type Runner struct {
	router   *tenantdb.Router
	tenants  *data.TenantRepo
	handlers map[workflow.StepKind]workflow.StepHandler
	cfg      Config

	// Schedule-sync throttle state — see shouldSyncSchedules. Mutex-
	// guarded because RunConcurrent runs several poll loops on one Runner.
	syncMu           sync.Mutex
	lastScheduleSync map[string]time.Time
	// Depreciation-post throttle state — see shouldPostDepreciation.
	// Separate mutex/map from the schedule-sync ones above: the two
	// throttle independently (different intervals, different tenants
	// can be due for one and not the other), and sharing one map keyed
	// by tenant would conflate them.
	depMu                sync.Mutex
	lastDepreciationPost map[string]time.Time
	// depreciationInFlight guards against two RunConcurrent pollers
	// (Config.Concurrency, default 2) both calling
	// assets.PostDueDepreciation for the SAME tenant at once.
	// lastDepreciationPost alone only throttles by start time, so a run
	// that takes longer than DepreciationPostInterval (a large catch-up:
	// a tenant licensing Assets after periods had already accrued) would
	// still be running when the next tick's throttle check says "due
	// again" — two overlapping runs racing to post the same rows and
	// transition the same asset is exactly how independent review
	// reproduced a permanently-stuck in_service asset (see
	// shouldPostDepreciation/finishDepreciationPost). Keyed by tenant,
	// not global, so unrelated tenants' runs never block each other.
	depreciationInFlight map[string]bool
}

// New builds a Runner. handlers is passed straight through to
// workflow.NewQueue for each tenant's own Queue (nil/partial is fine —
// see that function's doc comment on the default no-op notify handler
// used until a real notification connector exists). Definitions are
// resolved via workflow.RegistryDefinitionLookup — the real,
// registry-backed lookup, not a test stub. control is the control-plane
// database connection (ADR-0003) — where the tenant registry Runner
// iterates every tick actually lives.
func New(router *tenantdb.Router, control *sql.DB, handlers map[workflow.StepKind]workflow.StepHandler, cfg Config) *Runner {
	return &Runner{
		router:   router,
		tenants:  data.NewTenantRepo(control),
		handlers: handlers,
		cfg:      cfg.withDefaults(),
	}
}

// Run blocks, polling for due jobs across every tenant until ctx is
// cancelled. Run itself is single-threaded; call it from multiple
// goroutines (or use RunConcurrent) to get Config.Concurrency pollers —
// safe because each tenant's own ClaimNext still uses SKIP LOCKED
// (Config.Concurrency's doc comment), and multiple pollers hitting the
// same tenant list concurrently just means each tenant's queue gets
// drained by whichever poller gets there first, same as within a single
// tenant's queue before this fan-out existed.
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

// tick iterates every tenant in the control-plane registry and drains
// each one's own due jobs in turn. A single tenant's database being
// unreachable (mid-provisioning, a transient network blip) is logged and
// skipped, not fatal to the whole tick — one tenant's trouble must not
// stall every other tenant's workflow processing.
func (r *Runner) tick(ctx context.Context) {
	tenantIDs, err := r.tenants.ListIDs(ctx)
	if err != nil {
		log.Printf("worker: list tenants: %v", err)
		return
	}
	for _, tenantID := range tenantIDs {
		db, err := r.router.Get(ctx, tenantID)
		if err != nil {
			log.Printf("worker: resolve tenant %s database: %v", tenantID, err)
			continue
		}
		q, err := workflow.NewQueue(db, r.handlers)
		if err != nil {
			log.Printf("worker: build queue for tenant %s: %v", tenantID, err)
			continue
		}
		r.tickTenant(ctx, tenantID, q, workflow.RegistryDefinitionLookup(db))
	}
}

// tickTenant reclaims any stale (crashed-or-hung-worker) jobs, then
// drains every currently-due job via ProcessOne — not just one — so a
// burst of work arriving between ticks doesn't sit idle until the next
// poll just because PollInterval is coarse. Scoped to one tenant's own
// queue; see tick for the fan-out across every tenant.
func (r *Runner) tickTenant(ctx context.Context, tenantID string, q *workflow.Queue, lookup workflow.DefinitionLookup) {
	// Scheduled workflows (R18) become ordinary queued jobs BEFORE this
	// tick drains the queue, so a schedule that comes due runs in the
	// same tick rather than waiting for the next one.
	//
	// Failures here are logged, not fatal: a broken schedule table must
	// not stop event-triggered jobs — which are the ones a user is
	// actually waiting on — from being processed.
	if db, err := r.router.Get(ctx, tenantID); err != nil {
		log.Printf("worker: tenant %s: resolve db for scheduling: %v", tenantID, err)
	} else {
		sched := workflow.NewScheduler(db)
		now := time.Now().UTC()
		// Sync is throttled per tenant (Config.ScheduleSyncInterval);
		// FireDue runs every tick. The reconcile reads every published
		// definition and running that O(n) scan on every 2s tick forever
		// is pure waste — it measurably slowed ticks enough to flake the
		// multi-tenant timing test on a busy CI runner, which is how the
		// cost the reviewer flagged became a proven one. Firing stays
		// per-tick: it is one indexed query, and it is the half a user
		// actually waits on.
		if r.shouldSyncSchedules(tenantID, now) {
			if err := sched.Sync(ctx, now); err != nil {
				log.Printf("worker: tenant %s: sync workflow schedules: %v", tenantID, err)
				r.forgetScheduleSync(tenantID) // retry next tick, not next interval
			}
		}
		if fired, err := sched.FireDue(ctx, now, schedulerActor()); err != nil {
			log.Printf("worker: tenant %s: fire due schedules: %v", tenantID, err)
		} else if len(fired) > 0 {
			log.Printf("worker: tenant %s: fired %d scheduled workflow(s): %v", tenantID, len(fired), fired)
		}

		// assets.PostDueDepreciationBatch (uc-infra#76, ADR-0022; capped
		// uc-infra#137, ADR-0025): a plain per-tenant function call, not
		// a workflow — see ADR-0022 for why this isn't routed through
		// the workflow StepKind engine. Throttled the same way
		// schedule-sync is, and for the same reason: it's a full
		// DepreciationSchedule table scan, and nothing here needs
		// sub-minute latency. Capped by DepreciationPostBatchSize (see
		// its own doc comment) so a large catch-up backlog can't make
		// this one call block every other tenant's tick — the remainder
		// resumes on this tenant's next due tick. Failures are logged
		// and skipped, never fatal to this tenant's tick — same
		// isolation as every other per-tenant step in this function.
		if r.shouldPostDepreciation(tenantID, now) {
			posted, err := assets.PostDueDepreciationBatch(ctx, db, assets.SchedulerActor(), r.cfg.DepreciationPostBatchSize)
			// finishDepreciationPost unconditionally, success or error:
			// this is what lets a SECOND poller (RunConcurrent) pick this
			// tenant back up as soon as this run ends, rather than staying
			// blocked until DepreciationPostInterval elapses from this
			// run's START time — see depreciationInFlight's own doc
			// comment for why an overlapping run is the dangerous case,
			// not merely a redundant one.
			r.finishDepreciationPost(tenantID)
			if err != nil {
				log.Printf("worker: tenant %s: post due depreciation: %v", tenantID, err)
				r.forgetDepreciationPost(tenantID) // retry next tick, not next interval
			} else if posted > 0 {
				log.Printf("worker: tenant %s: posted %d depreciation schedule row(s)", tenantID, posted)
			}
		}
	}

	if reclaimed, err := q.ReclaimStale(ctx, r.cfg.LeaseTimeout); err != nil {
		log.Printf("worker: tenant %s: reclaim stale jobs: %v", tenantID, err)
	} else if len(reclaimed) > 0 {
		log.Printf("worker: tenant %s: reclaimed %d stale job(s): %v", tenantID, len(reclaimed), reclaimed)
	}

	// uc-infra#64: widen approver eligibility for any waiting_approval job
	// whose require_approval step has sat past its escalate_after_hours
	// threshold. Same clock-driven-firing nature as FireDue above, so it
	// reuses schedulerActor() rather than inventing a second system
	// identity for what is, from the audit trail's point of view, the
	// same kind of event: the clock came round, nobody clicked anything.
	if escalated, err := q.EscalateOverdueApprovals(ctx, time.Now().UTC(), lookup, schedulerActor()); err != nil {
		log.Printf("worker: tenant %s: escalate overdue approvals: %v", tenantID, err)
	} else if len(escalated) > 0 {
		log.Printf("worker: tenant %s: escalated %d overdue approval(s): %v", tenantID, len(escalated), escalated)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, err := q.ProcessOne(ctx, lookup)
		if err != nil {
			if errors.Is(err, workflow.ErrNoJobAvailable) {
				return
			}
			// A DB-level error processing/recording the job (ProcessOne's
			// own doc comment: only returned when recording a failure
			// itself failed, not for an ordinary step failure, which it
			// records on the job and returns nil for). Stop draining
			// this tenant's jobs this tick rather than spin against a
			// database that's currently erroring — the next tick tries
			// again, and other tenants' jobs still get processed this
			// same tick regardless.
			log.Printf("worker: tenant %s: process job: %v", tenantID, err)
			return
		}
		log.Printf("worker: tenant %s: processed job %s (%s v%d, entity %s/%s) -> step %d",
			tenantID, job.ID, job.WorkflowName, job.WorkflowVersion, job.EntityType, job.RecordID, job.StepIndex)
	}
}

// schedulerActor is the audit identity a scheduled run is attributed to.
// Explicitly a system actor rather than borrowing whoever last published
// the workflow: nobody clicked anything, and attributing an automated
// 3am run to a real person makes the audit trail lie about who acted
// (ADR-0001 §14 — actor identity is first-class, not decoration).
func schedulerActor() audit.Actor {
	return audit.Actor{Type: audit.ActorSystem, ID: "workflow-scheduler"}
}

const defaultScheduleSyncInterval = 30 * time.Second

// shouldSyncSchedules reports whether tenantID's schedule reconcile is
// due, and records the attempt. Guarded by a mutex because RunConcurrent
// runs several poll loops over one Runner.
func (r *Runner) shouldSyncSchedules(tenantID string, now time.Time) bool {
	interval := r.cfg.ScheduleSyncInterval
	if interval <= 0 {
		interval = defaultScheduleSyncInterval
	}
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	if r.lastScheduleSync == nil {
		r.lastScheduleSync = map[string]time.Time{}
	}
	if last, ok := r.lastScheduleSync[tenantID]; ok && now.Sub(last) < interval {
		return false
	}
	r.lastScheduleSync[tenantID] = now
	return true
}

// forgetScheduleSync clears the throttle after a FAILED sync, so the next
// tick retries instead of waiting out the interval on an error.
func (r *Runner) forgetScheduleSync(tenantID string) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	delete(r.lastScheduleSync, tenantID)
}

// shouldPostDepreciation reports whether tenantID's
// assets.PostDueDepreciation run is due, and — if so — records the
// attempt AND claims the in-flight slot in the same locked section, so
// a second poller's concurrent call for the same tenant cannot both see
// "not in flight" and both proceed (see depreciationInFlight's own doc
// comment for why an overlapping run is unsafe, not just redundant).
// Every call that returns true MUST be paired with a later
// finishDepreciationPost(tenantID) call, success or failure — tickTenant
// is the only caller and does this unconditionally.
func (r *Runner) shouldPostDepreciation(tenantID string, now time.Time) bool {
	interval := r.cfg.DepreciationPostInterval
	if interval <= 0 {
		interval = defaultDepreciationPostInterval
	}
	r.depMu.Lock()
	defer r.depMu.Unlock()
	if r.depreciationInFlight == nil {
		r.depreciationInFlight = map[string]bool{}
	}
	if r.depreciationInFlight[tenantID] {
		return false
	}
	if r.lastDepreciationPost == nil {
		r.lastDepreciationPost = map[string]time.Time{}
	}
	if last, ok := r.lastDepreciationPost[tenantID]; ok && now.Sub(last) < interval {
		return false
	}
	r.lastDepreciationPost[tenantID] = now
	r.depreciationInFlight[tenantID] = true
	return true
}

// finishDepreciationPost releases tenantID's in-flight claim once its
// assets.PostDueDepreciation call has returned (success or error) — the
// counterpart every shouldPostDepreciation(tenantID, ...) == true must
// eventually call, exactly once, so a later poller can claim the slot
// again.
func (r *Runner) finishDepreciationPost(tenantID string) {
	r.depMu.Lock()
	defer r.depMu.Unlock()
	delete(r.depreciationInFlight, tenantID)
}

// forgetDepreciationPost clears the throttle after a FAILED run, so the
// next tick retries instead of waiting out the interval on an error.
// finishDepreciationPost already released the in-flight claim (it must
// run first — see tickTenant) — this only resets the start-time
// throttle, a separate concern.
func (r *Runner) forgetDepreciationPost(tenantID string) {
	r.depMu.Lock()
	defer r.depMu.Unlock()
	delete(r.lastDepreciationPost, tenantID)
}
