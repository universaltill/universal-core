package workflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// testDB opens the integration-test database, skipping (not failing) if
// TEST_DATABASE_URL isn't set — same convention as crud_test.go, so
// `go test ./...` stays runnable without a database.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// seedTenant creates a fresh tenant and registers cleanup for any
// workflow_jobs rows the test leaves behind under it. This matters more
// here than in crud_test.go's identical-looking helper: ClaimNext is
// deliberately tenant-global (a background dispatcher polls across every
// tenant), so a job left in 'queued' by one test — even one that's
// exercising a failure path and stops right after observing
// attempts=1/status=queued, never driving it to a terminal state — is
// fair game for the very next test's ProcessOne call in this same shared
// database. Without this cleanup, test order becomes load-bearing.
func seedTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
		"Test Tenant", "eu-west",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM workflow_jobs WHERE tenant_id = $1`, id); err != nil {
			t.Errorf("cleanup workflow_jobs for tenant %s: %v", id, err)
		}
	})
	return id
}

func lookupFor(def *Definition) DefinitionLookup {
	return func(name string, version int) (*Definition, error) {
		if name == def.Name && version == def.Version {
			return def, nil
		}
		return nil, errors.New("no such workflow definition")
	}
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func TestQueue_ProcessOne_HaltsAtRequireApprovalThenResumesToDone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := poApprovalWorkflow()

	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	recordID := "11111111-1111-1111-1111-111111111111"
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", recordID, humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// poApprovalWorkflow's first step is require_approval, so the very
	// first ProcessOne halts immediately without running any handler.
	processed, err := q.ProcessOne(ctx, lookupFor(def))
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if processed.ID != job.ID {
		t.Fatalf("expected to process the enqueued job, got a different one: %s vs %s", processed.ID, job.ID)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "waiting_approval" {
		t.Fatalf("expected status waiting_approval, got %q", got.Status)
	}
	if got.StepIndex != 0 {
		t.Fatalf("expected step_index 0 (the require_approval step itself), got %d", got.StepIndex)
	}

	// Nothing else to claim while waiting_approval.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("expected ErrNoJobAvailable while job is waiting_approval, got %v", err)
	}

	// A human approves: resume, then the last notify step runs to completion.
	if err := q.ResumeAfterApproval(ctx, tenantID, job.ID); err != nil {
		t.Fatalf("ResumeAfterApproval: %v", err)
	}
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne after resume: %v", err)
	}

	got, err = repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected status done after resuming past approval, got %q", got.Status)
	}
	if got.StepIndex != len(def.Steps) {
		t.Fatalf("expected step_index %d (all steps run), got %d", len(def.Steps), got.StepIndex)
	}
}

func TestQueue_ProcessOne_NoJobAvailable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, lookupFor(poApprovalWorkflow())); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("expected ErrNoJobAvailable on an empty queue, got %v", err)
	}
}

func TestQueue_ProcessOne_RetriesTransientFailureThenSucceeds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "flaky_notify", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}

	attempts := 0
	q, err := NewQueue(db, map[StepKind]StepHandler{
		StepNotify: func(context.Context, data.WorkflowJob, Step) error {
			attempts++
			if attempts < 2 {
				return errors.New("transient failure: notification service unavailable")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	recordID := "22222222-2222-2222-2222-222222222222"
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", recordID, humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First attempt fails and is requeued with a future run_after, so
	// nothing is immediately claimable.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (1st attempt): %v", err)
	}
	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected status=queued attempts=1 after a transient failure, got status=%q attempts=%d", got.Status, got.Attempts)
	}
	if got.LastError == "" {
		t.Fatal("expected last_error to be recorded")
	}

	// Force the retry due now (defaultBackoff would otherwise make us wait).
	if _, err := db.ExecContext(ctx, `UPDATE workflow_jobs SET run_after = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("force run_after: %v", err)
	}

	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (2nd attempt): %v", err)
	}
	got, err = repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected status done after the retry succeeds, got %q", got.Status)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly 2 handler invocations, got %d", attempts)
	}
}

func TestQueue_ProcessOne_DeadLettersAfterMaxAttempts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "always_fails", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}

	q, err := NewQueue(db, map[StepKind]StepHandler{
		StepNotify: func(context.Context, data.WorkflowJob, Step) error {
			return errors.New("permanent failure")
		},
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	recordID := "33333333-3333-3333-3333-333333333333"
	job, err := q.jobs.Enqueue(ctx, data.WorkflowJob{
		TenantID: tenantID, WorkflowName: def.Name, WorkflowVersion: def.Version,
		EntityType: "PurchaseOrder", RecordID: recordID, MaxAttempts: 2, Actor: humanActor(),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	for i := range 2 {
		if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
			t.Fatalf("ProcessOne (attempt %d): %v", i+1, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE workflow_jobs SET run_after = now() WHERE id = $1`, job.ID); err != nil {
			t.Fatalf("force run_after: %v", err)
		}
	}

	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "dead_letter" {
		t.Fatalf("expected status dead_letter after exhausting max_attempts, got %q", got.Status)
	}
	if got.Attempts != 2 {
		t.Fatalf("expected attempts=2, got %d", got.Attempts)
	}

	// Dead-lettered jobs are never claimable again.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("expected ErrNoJobAvailable for a dead-lettered job, got %v", err)
	}
}

func TestQueue_ProcessOne_ConcurrentWorkersDoNotClaimSameJob(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "concurrent", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}, {Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	const n = 10
	for i := range n {
		if _, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder",
			"44444444-4444-4444-4444-44444444444"+string(rune('0'+i)), humanActor()); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	results := make(chan error, n)
	for range n {
		go func() {
			_, err := q.ProcessOne(ctx, lookupFor(def))
			results <- err
		}()
	}
	for range n {
		if err := <-results; err != nil {
			t.Fatalf("concurrent ProcessOne: %v", err)
		}
	}

	var doneCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_jobs WHERE tenant_id = $1 AND status = 'done'`, tenantID,
	).Scan(&doneCount); err != nil {
		t.Fatalf("count done jobs: %v", err)
	}
	if doneCount != n {
		t.Fatalf("expected all %d jobs done exactly once (no double-claim), got %d", n, doneCount)
	}
}

func TestNewQueue_RejectsHandlerForRequireApproval(t *testing.T) {
	db := testDB(t)
	_, err := NewQueue(db, map[StepKind]StepHandler{
		StepRequireApproval: func(context.Context, data.WorkflowJob, Step) error { return nil },
	})
	if err == nil {
		t.Fatal("expected NewQueue to reject a handler registered for require_approval")
	}
}

func TestWorkflowJobRepo_Get_NotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewWorkflowJobRepo(db)
	if _, err := repo.Get(ctx, tenantID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a nonexistent job, got %v", err)
	}
}

// TestWorkflowJobRepo_TenantIsolation is the regression test for the
// code-review finding that by-ID methods without a tenant check would let
// one tenant's request read or resume another tenant's job — Get and
// ResumeAfterApproval are the two a future HTTP handler would call
// directly with a caller-supplied job ID and an authenticated tenant
// scope, so the isolation has to be enforced in the query itself.
func TestWorkflowJobRepo_TenantIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	def := poApprovalWorkflow()
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	job, err := q.Enqueue(ctx, def, tenantA, "PurchaseOrder", "55555555-5555-5555-5555-555555555555", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	if _, err := repo.Get(ctx, tenantB, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected tenant B's Get of tenant A's job to return ErrNotFound, got %v", err)
	}
	if err := q.ResumeAfterApproval(ctx, tenantB, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected tenant B's ResumeAfterApproval on tenant A's job to return ErrNotFound, got %v", err)
	}

	// The rightful tenant can still see and resume it.
	if _, err := repo.Get(ctx, tenantA, job.ID); err != nil {
		t.Fatalf("expected tenant A's Get to succeed, got %v", err)
	}
	if err := q.ResumeAfterApproval(ctx, tenantA, job.ID); err != nil {
		t.Fatalf("expected tenant A's ResumeAfterApproval to succeed, got %v", err)
	}
}

func TestWorkflowJobRepo_ResumeAfterApproval_NotWaitingApproval(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "notify_only_resume_test", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "66666666-6666-6666-6666-666666666666", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Job is still 'queued' (never processed), not 'waiting_approval'.
	repo := data.NewWorkflowJobRepo(db)
	if err := repo.ResumeAfterApproval(ctx, tenantID, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound resuming a job that was never waiting_approval, got %v", err)
	}

	// Run it to done, then a second "approve" click must also fail —
	// resuming isn't idempotent past the point where there's nothing to
	// resume.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if err := repo.ResumeAfterApproval(ctx, tenantID, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound double-resuming a done job, got %v", err)
	}
}

func TestQueue_ProcessOne_DefinitionLookupError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "unresolvable", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "77777777-7777-7777-7777-777777777777", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	failingLookup := func(name string, version int) (*Definition, error) {
		return nil, errors.New("definition store unavailable")
	}
	if _, err := q.ProcessOne(ctx, failingLookup); err != nil {
		t.Fatalf("ProcessOne should record the lookup failure on the job, not return it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected a lookup failure to be recorded like any other step failure (status=queued attempts=1), got status=%q attempts=%d", got.Status, got.Attempts)
	}
	if got.LastError == "" {
		t.Fatal("expected last_error to record the lookup failure")
	}
}

func TestQueue_ProcessOne_InvalidDefinitionFromLookup(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "will_be_invalid", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "88888888-8888-8888-8888-888888888888", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The lookup returns something that fails Validate (no steps) — this
	// can't happen via Enqueue (which validates first) but could if a
	// workflow_definitions-backed lookup returns a corrupted/rolled-back
	// version.
	invalidLookup := func(name string, version int) (*Definition, error) {
		return &Definition{Name: name, Version: version, Trigger: Trigger{Type: TriggerManual}}, nil
	}
	if _, err := q.ProcessOne(ctx, invalidLookup); err != nil {
		t.Fatalf("ProcessOne should record the validation failure on the job, not return it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected an invalid definition to be recorded like any other step failure, got status=%q attempts=%d", got.Status, got.Attempts)
	}
}

func TestQueue_ReclaimStale_RequeuesOrphanedRunningJob(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "orphaned", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "99999999-9999-9999-9999-999999999999", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate a worker that claimed the job and then vanished (SIGKILL,
	// OOM) before ever calling MarkDone/MarkFailed/MarkWaitingApproval:
	// set status='running' with an old updated_at directly, bypassing the
	// Queue entirely.
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() - interval '1 hour' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("simulate orphaned job: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	reclaimed, err := q.ReclaimStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != job.ID {
		t.Fatalf("expected exactly job %s to be reclaimed, got %v", job.ID, reclaimed)
	}

	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected reclaimed job back to queued, got %q", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("expected reclaiming to count as an attempt (so a poison-pill job still dead-letters eventually), got attempts=%d", got.Attempts)
	}

	// It's claimable again.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne after reclaim: %v", err)
	}
	got, err = repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected the reclaimed job to run to completion, got %q", got.Status)
	}
}

func TestQueue_ReclaimStale_LeavesFreshRunningJobAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "still_running", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() WHERE id = $1`, job.ID,
	); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	reclaimed, err := q.ReclaimStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	for _, id := range reclaimed {
		if id == job.ID {
			t.Fatalf("a job still within its lease must not be reclaimed: %v", reclaimed)
		}
	}
}

func TestQueue_ProcessOne_PanicInHandlerIsRecoveredAndRetried(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	def := &Definition{
		Name: "panics_once", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}

	calls := 0
	q, err := NewQueue(db, map[StepKind]StepHandler{
		StepNotify: func(context.Context, data.WorkflowJob, Step) error {
			calls++
			if calls == 1 {
				panic("boom")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne should recover the panic and record it as a failure, not propagate it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected the panic to be recorded like an ordinary failure, got status=%q attempts=%d", got.Status, got.Attempts)
	}
	if got.LastError == "" {
		t.Fatal("expected last_error to mention the panic")
	}
}
