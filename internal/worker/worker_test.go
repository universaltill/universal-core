package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

// freshControlDB returns a connection to a brand-new, uniquely-named
// control-plane database (ADR-0003) with the control migration set
// applied. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other DB-backed test in this repo.
func freshControlDB(t *testing.T) (controlDB *sql.DB, controlDSN string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_worker_control_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	dsn := u.String()
	control, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open control database %s: %v", name, err)
	}
	t.Cleanup(func() { control.Close() })
	if err := control.Ping(); err != nil {
		t.Fatalf("ping control database %s: %v", name, err)
	}
	if _, err := control.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	return control, dsn
}

// newTestRouter builds a Router against a fresh control database — the
// Runner under test always resolves tenants through this, exactly as
// cmd/universal-core wires it in production.
func newTestRouter(t *testing.T) (router *tenantdb.Router, control *sql.DB) {
	t.Helper()
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	return router, control
}

// newTestTenant provisions a brand-new tenant via router.Create and
// returns its id and own *sql.DB — the real provisioning path, not a
// hand-rolled fixture, so these tests exercise the same route
// cmd/provision-tenant does.
func newTestTenant(t *testing.T, router *tenantdb.Router) (tenantID string, tenantDB *sql.DB) {
	t.Helper()
	ctx := context.Background()
	tenantID, err := router.Create(ctx, "Test Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	// See internal/api's identical helper: the tenant's own physical
	// database (ADR-0003) is not dropped by the control database's
	// cleanup, and leaked permanently without this.
	testexec.DropConnectedDatabase(t, tenantDB)
	return tenantID, tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

// publish drives a Definition through CreateDraft -> Approve -> Publish
// against a tenant's own database — worker.Runner resolves definitions
// via workflow.RegistryDefinitionLookup (the real registry-backed
// lookup), unlike internal/kernel/workflow's own tests which mostly use
// a hand-built stub, so every test here needs a genuinely published
// definition.
func publish(t *testing.T, tenantDB *sql.DB, def *workflow.Definition, actor audit.Actor) {
	t.Helper()
	ctx := context.Background()
	repo := data.NewWorkflowDefinitionRepo(tenantDB)
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition %s: %v", def.Name, err)
	}
	if _, err := repo.CreateDraft(ctx, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft %s v%d: %v", def.Name, def.Version, err)
	}
	if err := repo.Approve(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve %s v%d: %v", def.Name, def.Version, err)
	}
	if err := repo.Publish(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish %s v%d: %v", def.Name, def.Version, err)
	}
}

// fastTestConfig polls aggressively so tests don't wait a real
// PollInterval-scale amount of wall-clock time.
func fastTestConfig() Config {
	return Config{PollInterval: 20 * time.Millisecond, LeaseTimeout: 200 * time.Millisecond, Concurrency: 1, ScheduleSyncInterval: 20 * time.Millisecond}
}

// startRunner runs r.Run(ctx) in the background and returns a function
// that blocks until Run has actually returned. Every test must call the
// returned stop func (after cancelling ctx) before letting t.Cleanup close
// db out from under it — without this, Run's goroutine can still be
// mid-query when cleanup runs, racing the test's own teardown (found by
// independent review: logged "context canceled" errors after several
// tests had already returned).
func startRunner(r *Runner, ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	return func() { <-done }
}

func TestRunner_ProcessesEnqueuedJobViaPolling(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	def := &workflow.Definition{
		Name: "poll_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	actor := humanActor()
	publish(t, tenantDB, def, actor)

	processed := make(chan string, 1)
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			processed <- job.ID
			return nil
		},
	}, fastTestConfig())

	q, err := workflow.NewQueue(tenantDB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "11111111-1111-1111-1111-111111111111", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	select {
	case gotID := <-processed:
		if gotID != job.ID {
			t.Fatalf("expected the enqueued job %s to be processed, got %s", job.ID, gotID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the poll loop to pick up the enqueued job")
	}

	// ProcessOne (internal/kernel/workflow/queue.go) calls the step
	// handler — which sends to processed above — *before* its own
	// separate MarkDone DB write; receiving from processed only proves
	// the handler ran, not that MarkDone has committed yet. A single
	// immediate Get here raced that write often enough to flake under
	// `go test -race` (found via a real CI failure, not by inspection
	// alone) — poll briefly instead of assuming synchronous-with-the-
	// channel-send ordering that ProcessOne never promises.
	repo := data.NewWorkflowJobRepo(tenantDB)
	deadline := time.Now().Add(time.Second)
	var got data.WorkflowJob
	for {
		var err error
		got, err = repo.Get(ctx, job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status == "done" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != "done" {
		t.Fatalf("expected status done, got %q", got.Status)
	}
}

func TestRunner_ReclaimsStaleJobsEachTick(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	def := &workflow.Definition{
		Name: "reclaim_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	actor := humanActor()
	publish(t, tenantDB, def, actor)

	q, err := workflow.NewQueue(tenantDB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "22222222-2222-2222-2222-222222222222", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate a worker that claimed the job and vanished (SIGKILL, OOM)
	// before ever calling MarkDone/MarkFailed/MarkWaitingApproval — same
	// setup as workflow package's own ReclaimStale test.
	if _, err := tenantDB.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() - interval '1 hour' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("simulate orphaned job: %v", err)
	}

	processed := make(chan string, 1)
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			processed <- job.ID
			return nil
		},
	}, fastTestConfig())

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	select {
	case gotID := <-processed:
		if gotID != job.ID {
			t.Fatalf("expected the reclaimed job %s to be processed, got %s", job.ID, gotID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the orphaned job to be reclaimed and processed")
	}
}

// TestRunner_EscalatesOverdueApprovalsEachTick confirms
// Queue.EscalateOverdueApprovals (uc-infra#64) is actually wired into
// tickTenant, not just unit-tested in isolation — the same "prove the
// wiring, not just the mechanism" property TestRunner_
// ReclaimsStaleJobsEachTick establishes for ReclaimStale and
// TestRunner_FiresScheduledWorkflowEndToEnd establishes for FireDue.
func TestRunner_EscalatesOverdueApprovalsEachTick(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	def := &workflow.Definition{
		Name: "escalation_wiring_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{{Kind: workflow.StepRequireApproval, Params: map[string]any{
			"role": "cfo", "escalate_after_hours": 1.0, "escalate_role": "vp",
		}}},
	}
	actor := humanActor()
	publish(t, tenantDB, def, actor)

	q, err := workflow.NewQueue(tenantDB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "66666666-6666-6666-6666-666666666666", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	r := New(router, control, nil, fastTestConfig())
	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	repo := data.NewWorkflowJobRepo(tenantDB)

	// Wait for the runner to actually halt the job at waiting_approval
	// (require_approval never gets a handler call — Queue always
	// intercepts it, see queue.go's ProcessOne doc comment) before
	// backdating it, same two-phase wait TestRunner_
	// FiresScheduledWorkflowEndToEnd uses for the schedule to register
	// before forcing it due.
	waitFor(t, ctx, 3*time.Second, func() bool {
		got, err := repo.Get(ctx, job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got.Status == "waiting_approval"
	})

	if _, err := tenantDB.ExecContext(ctx,
		`UPDATE workflow_jobs SET updated_at = now() - interval '2 hours' WHERE id = $1`, job.ID,
	); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	waitFor(t, ctx, 3*time.Second, func() bool {
		got, err := repo.Get(ctx, job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got.Escalated
	})

	// An audit row backs the escalation (CLAUDE.md: every mutation writes
	// an audit row), attributed to the system actor, the same identity
	// FireDue's own audit rows use.
	var actorType string
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT actor_type FROM audit_log WHERE record_id = $1 AND action = 'update'`,
		"66666666-6666-6666-6666-666666666666",
	).Scan(&actorType); err != nil {
		t.Fatalf("read audit row for escalation: %v", err)
	}
	if actorType != "system" {
		t.Fatalf("expected the escalation's audit row attributed to 'system', got %q", actorType)
	}
}

// waitFor polls cond every 20ms until it returns true or timeout elapses,
// failing the test on timeout — the shared shape TestRunner_
// FiresScheduledWorkflowEndToEnd and TestRunner_
// ProcessesMultipleTenantsIndependentlyInOneTick each inline their own
// copy of; factored out here since this test needs it twice.
func waitFor(t *testing.T, ctx context.Context, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		select {
		case <-ctx.Done():
			t.Fatal("context cancelled while waiting for condition")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestRunner_StopsOnContextCancellation(t *testing.T) {
	router, control := newTestRouter(t)
	ctx, cancel := context.WithCancel(context.Background())

	r := New(router, control, nil, fastTestConfig())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Let it poll at least once against an empty tenant list, then stop it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

func TestRunner_RunConcurrent_ProcessesAllJobsExactlyOnce(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	def := &workflow.Definition{
		Name: "concurrent_worker_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	actor := humanActor()
	publish(t, tenantDB, def, actor)

	q, err := workflow.NewQueue(tenantDB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	const n = 8
	recordIDs := make([]string, n)
	for i := range n {
		recordIDs[i] = "33333333-3333-3333-3333-33333333333" + string(rune('0'+i))
		if _, err := q.Enqueue(ctx, def, "PurchaseOrder", recordIDs[i], actor); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	cfg := fastTestConfig()
	cfg.Concurrency = 4
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			mu.Lock()
			if seen[job.ID] {
				mu.Unlock()
				t.Errorf("job %s processed more than once", job.ID)
				return nil
			}
			seen[job.ID] = true
			mu.Unlock()
			return nil
		},
	}, cfg)

	// Spawn Concurrency pollers manually (rather than r.RunConcurrent,
	// which by design doesn't hand back a way to wait for them) so this
	// test can join every goroutine before returning — otherwise a
	// poller can still be mid-query against db when t.Cleanup closes it.
	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Run(ctx)
		}()
	}
	defer func() { cancel(); wg.Wait() }()

	// Poll the database directly for the terminal state, not the
	// in-process seen count — the StepHandler above runs before
	// Queue.ProcessOne's own MarkDone commits, so counting inside the
	// handler could observe "processed" before status='done' is
	// actually visible to a fresh query.
	countDone := func() int {
		var n int
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT count(*) FROM workflow_jobs WHERE status = 'done'`,
		).Scan(&n); err != nil {
			t.Fatalf("count done jobs: %v", err)
		}
		return n
	}
	deadline := time.After(5 * time.Second)
	for countDone() < n {
		select {
		case <-deadline:
			t.Fatalf("timed out: only %d/%d jobs done", countDone(), n)
		case <-time.After(20 * time.Millisecond):
		}
	}

	for _, id := range recordIDs {
		var status string
		if err := tenantDB.QueryRowContext(ctx,
			`SELECT status FROM workflow_jobs WHERE record_id = $1`, id,
		).Scan(&status); err != nil {
			t.Fatalf("query status for record %s: %v", id, err)
		}
		if status != "done" {
			t.Errorf("record %s: expected status done, got %q", id, status)
		}
	}
}

// TestRunner_ProcessesMultipleTenantsIndependentlyInOneTick is the
// regression test for ADR-0003's own flagged gap: before database-per-
// tenant, ClaimNext/ReclaimStale scanned across every tenant in one
// shared-table query and had zero test coverage proving multiple
// tenants' jobs actually coexist correctly. Now that Runner fans out
// across every tenant's own database each tick (tick/tickTenant in
// worker.go), this proves that fan-out: two tenants, each with their own
// enqueued job, both get processed by the same Runner in the same
// running process — and, since ClaimNext is now scoped to a single
// tenant's own database, there is no shared table for one tenant's job
// to accidentally be claimed against another's connection.
func TestRunner_ProcessesMultipleTenantsIndependentlyInOneTick(t *testing.T) {
	router, control := newTestRouter(t)
	tenantAID, tenantADB := newTestTenant(t, router)
	tenantBID, tenantBDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	def := &workflow.Definition{
		Name: "multi_tenant_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	actor := humanActor()
	publish(t, tenantADB, def, actor)
	publish(t, tenantBDB, def, actor)

	qA, err := workflow.NewQueue(tenantADB, nil)
	if err != nil {
		t.Fatalf("NewQueue tenant A: %v", err)
	}
	qB, err := workflow.NewQueue(tenantBDB, nil)
	if err != nil {
		t.Fatalf("NewQueue tenant B: %v", err)
	}
	jobA, err := qA.Enqueue(ctx, def, "PurchaseOrder", "44444444-4444-4444-4444-444444444444", actor)
	if err != nil {
		t.Fatalf("Enqueue tenant A: %v", err)
	}
	jobB, err := qB.Enqueue(ctx, def, "PurchaseOrder", "55555555-5555-5555-5555-555555555555", actor)
	if err != nil {
		t.Fatalf("Enqueue tenant B: %v", err)
	}

	var mu sync.Mutex
	processedBy := map[string]string{} // job id -> which tenant's handler saw it
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			mu.Lock()
			defer mu.Unlock()
			switch job.ID {
			case jobA.ID:
				processedBy[job.ID] = tenantAID
			case jobB.ID:
				processedBy[job.ID] = tenantBID
			}
			return nil
		},
	}, fastTestConfig())

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		done := len(processedBy) == 2
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out: expected both tenants' jobs processed, got %v", processedBy)
			mu.Unlock()
		case <-time.After(20 * time.Millisecond):
		}
	}

	repoA := data.NewWorkflowJobRepo(tenantADB)
	gotA, err := repoA.Get(ctx, jobA.ID)
	if err != nil {
		t.Fatalf("Get tenant A job: %v", err)
	}
	if gotA.Status != "done" {
		t.Fatalf("expected tenant A's job done, got %q", gotA.Status)
	}

	repoB := data.NewWorkflowJobRepo(tenantBDB)
	gotB, err := repoB.Get(ctx, jobB.ID)
	if err != nil {
		t.Fatalf("Get tenant B job: %v", err)
	}
	if gotB.Status != "done" {
		t.Fatalf("expected tenant B's job done, got %q", gotB.Status)
	}

	// tenant A's own connection must never see tenant B's job row (and
	// vice versa) — physical isolation, not just a WHERE clause.
	var crossCount int
	if err := tenantADB.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_jobs WHERE id = $1`, jobB.ID,
	).Scan(&crossCount); err != nil {
		t.Fatalf("check tenant A database for tenant B's job: %v", err)
	}
	if crossCount != 0 {
		t.Fatal("tenant A's own database contains tenant B's job row")
	}
}

// TestRunner_RunConcurrent_ReturnsImmediatelyAndSpawnsPollers exercises
// RunConcurrent directly (unlike TestRunner_RunConcurrent_ProcessesAllJobsExactlyOnce,
// which deliberately calls r.Run manually in its own goroutines instead —
// see that test's own comment on why). Confirms RunConcurrent's own
// documented contract: it returns immediately without waiting for any
// poller, and the goroutines it spawned are real and actually tick (an
// atomic counter incremented inside a StepHandler proves at least one
// poller genuinely ran, not just that the function returned).
func TestRunner_RunConcurrent_ReturnsImmediatelyAndSpawnsPollers(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := &workflow.Definition{
		Name: "run_concurrent_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	actor := humanActor()
	publish(t, tenantDB, def, actor)

	q, err := workflow.NewQueue(tenantDB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.Enqueue(ctx, def, "PurchaseOrder", "44444444-4444-4444-4444-444444444444", actor); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var ticked atomic.Bool
	cfg := fastTestConfig()
	cfg.Concurrency = 3
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			ticked.Store(true)
			return nil
		},
	}, cfg)

	done := make(chan struct{})
	go func() {
		r.RunConcurrent(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunConcurrent must return immediately, without waiting for its pollers")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !ticked.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ticked.Load() {
		t.Fatal("expected at least one spawned poller to actually process the enqueued job")
	}
}

// TestRunner_FiresScheduledWorkflowEndToEnd covers the wiring the two
// package-level test suites each miss: internal/kernel/workflow proves a
// schedule fires into the queue, and this package proves the queue
// drains, but nothing proved tickTenant actually connects them. A bug
// between "fired" and "drained" would have passed both.
func TestRunner_FiresScheduledWorkflowEndToEnd(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Every minute, so it is due almost immediately without the test
	// having to manipulate time.
	def := &workflow.Definition{
		Name: "scheduled_smoke", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerScheduled, Cron: "* * * * *"},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	publish(t, tenantDB, def, humanActor())

	processed := make(chan data.WorkflowJob, 4)
	r := New(router, control, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			processed <- job
			return nil
		},
	}, fastTestConfig())

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	// Wait for Sync to register the schedule, then force it due. "* * * * *"
	// alone would mean waiting up to a full minute for the next boundary —
	// this asserts the WIRING (Sync -> FireDue -> ProcessOne), not the clock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM workflow_schedules`).Scan(&n); err != nil {
			t.Fatalf("count schedules: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tickTenant never registered the schedule — Sync is not wired in")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := tenantDB.ExecContext(ctx,
		`UPDATE workflow_schedules SET next_due_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("force due: %v", err)
	}

	select {
	case job := <-processed:
		if job.WorkflowName != "scheduled_smoke" {
			t.Fatalf("processed the wrong workflow: %s", job.WorkflowName)
		}
		// A scheduled run is not about a record, and must say so rather
		// than carrying a placeholder that looks real.
		if job.EntityType != "" || job.RecordID != "" {
			t.Fatalf("a scheduled run should carry no entity/record, got %q/%q", job.EntityType, job.RecordID)
		}
		// And is attributed to the system actor, not a person who was
		// asleep (ADR-0008).
		if job.Actor.Type != audit.ActorSystem {
			t.Fatalf("scheduled run attributed to %q, want the system actor", job.Actor.Type)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the worker never fired and processed a due scheduled workflow — FireDue is not wired into tickTenant")
	}
}
