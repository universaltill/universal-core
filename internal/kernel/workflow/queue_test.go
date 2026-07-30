package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// freshTenantDB returns a connection to a brand-new, uniquely-named
// tenant database (ADR-0003) with the tenant migration set applied —
// every test in this file now gets its own database instead of sharing
// one and manually cleaning up workflow_jobs rows between tests: since
// ClaimNext/ReclaimStale scan the whole database they're given (a
// "tenant" is a database now, not a row filter), a fresh database per
// test makes cross-test job contamination structurally impossible
// rather than relying on cleanup discipline.
func freshTenantDB(t *testing.T) *sql.DB {
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

	name := fmt.Sprintf("uc_test_workflow_%d", time.Now().UnixNano())
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
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func lookupFor(def *Definition) DefinitionLookup {
	return func(ctx context.Context, name string, version int) (*Definition, error) {
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
	db := freshTenantDB(t)
	ctx := context.Background()
	def := poApprovalWorkflow()

	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	recordID := "11111111-1111-1111-1111-111111111111"
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", recordID, humanActor())
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
	got, err := repo.Get(ctx, job.ID)
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
	if err := q.ResumeAfterApproval(ctx, job.ID); err != nil {
		t.Fatalf("ResumeAfterApproval: %v", err)
	}
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne after resume: %v", err)
	}

	got, err = repo.Get(ctx, job.ID)
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
	db := freshTenantDB(t)
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
	db := freshTenantDB(t)
	ctx := context.Background()
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
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", recordID, humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First attempt fails and is requeued with a future run_after, so
	// nothing is immediately claimable.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (1st attempt): %v", err)
	}
	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
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
	got, err = repo.Get(ctx, job.ID)
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
	db := freshTenantDB(t)
	ctx := context.Background()
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
		WorkflowName: def.Name, WorkflowVersion: def.Version,
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

	got, err := repo.Get(ctx, job.ID)
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
	db := freshTenantDB(t)
	ctx := context.Background()
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
		if _, err := q.Enqueue(ctx, def, "PurchaseOrder",
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
		`SELECT count(*) FROM workflow_jobs WHERE status = 'done'`,
	).Scan(&doneCount); err != nil {
		t.Fatalf("count done jobs: %v", err)
	}
	if doneCount != n {
		t.Fatalf("expected all %d jobs done exactly once (no double-claim), got %d", n, doneCount)
	}
}

func TestNewQueue_RejectsHandlerForRequireApproval(t *testing.T) {
	db := freshTenantDB(t)
	_, err := NewQueue(db, map[StepKind]StepHandler{
		StepRequireApproval: func(context.Context, data.WorkflowJob, Step) error { return nil },
	})
	if err == nil {
		t.Fatal("expected NewQueue to reject a handler registered for require_approval")
	}
}

func TestWorkflowJobRepo_Get_NotFound(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewWorkflowJobRepo(db)
	if _, err := repo.Get(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a nonexistent job, got %v", err)
	}
}

// TestQueue_ListByStatus_ReturnsOnlyMatchingStatus confirms a caller
// finds jobs actually waiting_approval without already knowing an id,
// and doesn't see jobs in a different status mixed in.
func TestQueue_ListByStatus_ReturnsOnlyMatchingStatus(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := poApprovalWorkflow()
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	// One job halted at waiting_approval, one job left queued.
	waitingJob, err := q.Enqueue(ctx, def, "PurchaseOrder", "11111111-1111-1111-1111-111111111111", humanActor())
	if err != nil {
		t.Fatalf("Enqueue waiting job: %v", err)
	}
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	notifyOnly := &Definition{
		Name: "notify_only", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	if _, err := q.Enqueue(ctx, notifyOnly, "PurchaseOrder", "22222222-2222-2222-2222-222222222222", humanActor()); err != nil {
		t.Fatalf("Enqueue queued job: %v", err)
	}

	waiting, err := q.ListByStatus(ctx, "waiting_approval")
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(waiting) != 1 || waiting[0].ID != waitingJob.ID {
		t.Fatalf("expected exactly the one waiting_approval job, got %+v", waiting)
	}

	queued, err := q.ListByStatus(ctx, "queued")
	if err != nil {
		t.Fatalf("ListByStatus queued: %v", err)
	}
	if len(queued) != 1 || queued[0].WorkflowName != "notify_only" {
		t.Fatalf("expected exactly the one queued job, got %+v", queued)
	}
}

// TestQueue_Get_ReturnsJobByID confirms the passthrough approveWorkflowJob
// (internal/api) needs to resolve WorkflowName/Version/StepIndex before
// deciding whether a caller may resume a waiting_approval job — same
// shape as ListByStatus's own test, one level down (by id instead of by
// status).
func TestQueue_Get_ReturnsJobByID(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := poApprovalWorkflow()
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	enqueued, err := q.Enqueue(ctx, def, "PurchaseOrder", "33333333-3333-3333-3333-333333333333", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := q.Get(ctx, enqueued.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != enqueued.ID || got.WorkflowName != def.Name || got.WorkflowVersion != def.Version {
		t.Fatalf("expected the enqueued job back, got %+v", got)
	}

	if _, err := q.Get(ctx, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown job id, got %v", err)
	}
}

// TestWorkflowJobRepo_TenantIsolation is the regression test for the
// code-review finding that by-ID methods without a tenant check would let
// one tenant's request read or resume another tenant's job. Proven here
// via two genuinely separate tenant databases (ADR-0003) — a stronger
// proof than the old shared-DB/tenant_id WHERE clause version: tenant B's
// connection has no row with that id at all, physically, not just a
// query that happens to filter it out.
func TestWorkflowJobRepo_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	def := poApprovalWorkflow()
	qA, err := NewQueue(dbA, nil)
	if err != nil {
		t.Fatalf("NewQueue (tenant A): %v", err)
	}
	qB, err := NewQueue(dbB, nil)
	if err != nil {
		t.Fatalf("NewQueue (tenant B): %v", err)
	}

	job, err := qA.Enqueue(ctx, def, "PurchaseOrder", "55555555-5555-5555-5555-555555555555", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := qA.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}

	repoA := data.NewWorkflowJobRepo(dbA)
	repoB := data.NewWorkflowJobRepo(dbB)
	if _, err := repoB.Get(ctx, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected tenant B's Get of tenant A's job id to return ErrNotFound, got %v", err)
	}
	if err := qB.ResumeAfterApproval(ctx, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected tenant B's ResumeAfterApproval on tenant A's job id to return ErrNotFound, got %v", err)
	}

	// The rightful tenant can still see and resume it.
	if _, err := repoA.Get(ctx, job.ID); err != nil {
		t.Fatalf("expected tenant A's Get to succeed, got %v", err)
	}
	if err := qA.ResumeAfterApproval(ctx, job.ID); err != nil {
		t.Fatalf("expected tenant A's ResumeAfterApproval to succeed, got %v", err)
	}
}

func TestWorkflowJobRepo_ResumeAfterApproval_NotWaitingApproval(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "notify_only_resume_test", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "66666666-6666-6666-6666-666666666666", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Job is still 'queued' (never processed), not 'waiting_approval'.
	repo := data.NewWorkflowJobRepo(db)
	if err := repo.ResumeAfterApproval(ctx, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound resuming a job that was never waiting_approval, got %v", err)
	}

	// Run it to done, then a second "approve" click must also fail —
	// resuming isn't idempotent past the point where there's nothing to
	// resume.
	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if err := repo.ResumeAfterApproval(ctx, job.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound double-resuming a done job, got %v", err)
	}
}

func TestQueue_ProcessOne_DefinitionLookupError(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "unresolvable", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "77777777-7777-7777-7777-777777777777", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	failingLookup := func(ctx context.Context, name string, version int) (*Definition, error) {
		return nil, errors.New("definition store unavailable")
	}
	if _, err := q.ProcessOne(ctx, failingLookup); err != nil {
		t.Fatalf("ProcessOne should record the lookup failure on the job, not return it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
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
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "will_be_invalid", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "88888888-8888-8888-8888-888888888888", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The lookup returns something that fails Validate (no steps) — this
	// can't happen via Enqueue (which validates first) but could if a
	// workflow_definitions-backed lookup returns a corrupted/rolled-back
	// version.
	invalidLookup := func(ctx context.Context, name string, version int) (*Definition, error) {
		return &Definition{Name: name, Version: version, Trigger: Trigger{Type: TriggerManual}}, nil
	}
	if _, err := q.ProcessOne(ctx, invalidLookup); err != nil {
		t.Fatalf("ProcessOne should record the validation failure on the job, not return it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected an invalid definition to be recorded like any other step failure, got status=%q attempts=%d", got.Status, got.Attempts)
	}
}

// TestQueue_ProcessOne_MalformedApprovalRoleNeverReachesWaitingApproval is
// the queue-level sibling of TestDefinitionValidate_
// RequireApprovalRoleParamMustBeString: even if a require_approval step's
// role param were somehow malformed by the time ProcessOne's lookup
// returns it (Enqueue's own upfront Validate call is the normal defense,
// but this proves the second, independent layer holds too — the same
// "defense in depth, not defense in one place" reasoning
// TestQueue_ProcessOne_InvalidDefinitionFromLookup already established
// for a lookup returning a corrupted definition), the job fails at
// ProcessOne and is never marked waiting_approval — it can never reach
// the point internal/api's approval gate would have to reason about a
// bad role value at all.
func TestQueue_ProcessOne_MalformedApprovalRoleNeverReachesWaitingApproval(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "malformed_role", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}}, // valid at Enqueue time
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "66666666-6666-6666-6666-666666666666", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulates a lookup returning a definition whose role param is
	// malformed (e.g. written out-of-band, bypassing Enqueue's check) —
	// same "lookup can return something Enqueue would have refused"
	// premise as TestQueue_ProcessOne_InvalidDefinitionFromLookup.
	malformedLookup := func(ctx context.Context, name string, version int) (*Definition, error) {
		return &Definition{
			Name: name, Version: version, Trigger: Trigger{Type: TriggerManual},
			Steps: []Step{{Kind: StepRequireApproval, Params: map[string]any{"role": 42}}},
		}, nil
	}
	if _, err := q.ProcessOne(ctx, malformedLookup); err != nil {
		t.Fatalf("ProcessOne should record the validation failure on the job, not return it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "waiting_approval" {
		t.Fatal("a malformed role param must never reach waiting_approval — that would mean approveWorkflowJob has to reason about it")
	}
	if got.Status != "queued" || got.Attempts != 1 {
		t.Fatalf("expected the malformed definition to be recorded like any other step failure, got status=%q attempts=%d", got.Status, got.Attempts)
	}
}

func TestQueue_ReclaimStale_RequeuesOrphanedRunningJob(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "orphaned", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "99999999-9999-9999-9999-999999999999", humanActor())
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

	got, err := repo.Get(ctx, job.ID)
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
	got, err = repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected the reclaimed job to run to completion, got %q", got.Status)
	}
}

func TestQueue_ReclaimStale_LeavesFreshRunningJobAlone(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "still_running", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", humanActor())
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

// TestWorkflowJobRepo_MarkFailedCannotResurrectAnAlreadyDoneJob is the
// regression test for the code-review finding that MarkDone/MarkFailed/
// MarkWaitingApproval matched only on id+tenant_id, with no status guard
// — so a stale-but-alive worker (one whose step ran past leaseTimeout
// but hadn't actually crashed) could call Mark* on a job another process
// had already moved on from, silently clobbering its state. This
// reproduces the reviewer's exact demonstrated case: enqueue -> claim ->
// MarkDone (job legitimately completes) -> a later MarkFailed call on
// the SAME id, simulating the original worker's zombie retry, must NOT
// flip a 'done' job back to 'queued'.
func TestWorkflowJobRepo_MarkFailedCannotResurrectAnAlreadyDoneJob(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewWorkflowJobRepo(db)

	job, err := repo.Enqueue(ctx, data.WorkflowJob{
		WorkflowName: "resurrection_check", WorkflowVersion: 1,
		EntityType: "PurchaseOrder", RecordID: "11111111-1111-1111-1111-111111111111",
		Actor: humanActor(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() WHERE id = $1`, job.ID,
	); err != nil {
		t.Fatalf("simulate claim: %v", err)
	}
	if err := repo.MarkDone(ctx, job.ID, 1); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	// The zombie retry: a worker that (unbeknownst to it) already had
	// this job's result recorded calls MarkFailed anyway.
	if _, err := repo.MarkFailed(ctx, job.ID, errors.New("zombie worker's late failure"), time.Now()); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected MarkFailed on an already-done job to return ErrNotFound, got %v", err)
	}

	got, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected the completed job to stay 'done', got %q — MarkFailed resurrected it", got.Status)
	}
	if got.Attempts != 0 {
		t.Fatalf("expected the rejected MarkFailed to leave attempts untouched, got %d", got.Attempts)
	}
}

// TestQueue_ReclaimStale_OriginalStaleWorkerCannotUndoTheReclaim is the
// companion regression test at the ReclaimStale/Queue level: a job whose
// lease expired and was reclaimed (moved back to 'queued', counted as an
// attempt) must not have that bookkeeping undone by the original worker
// finally calling MarkDone after the fact — it was slow, not
// necessarily dead, and ReclaimStale already made the authoritative call
// that its lease expired.
func TestQueue_ReclaimStale_OriginalStaleWorkerCannotUndoTheReclaim(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	def := &Definition{
		Name: "slow_not_dead", Version: 1,
		Trigger: Trigger{Type: TriggerManual},
		Steps:   []Step{{Kind: StepNotify}},
	}
	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "22222222-2222-2222-2222-222222222222", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() - interval '1 hour' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("simulate long-running claim: %v", err)
	}

	if _, err := q.ReclaimStale(ctx, 5*time.Minute); err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}

	// The original worker wasn't actually dead — it was just slow — and
	// now finishes its step and reports success, unaware it's been
	// reclaimed out from under it.
	repo := data.NewWorkflowJobRepo(db)
	if err := repo.MarkDone(ctx, job.ID, 1); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected the original worker's late MarkDone to be rejected, got %v", err)
	}

	got, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected the job to stay in ReclaimStale's 'queued' state, got %q — the stale worker undid the reclaim", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("expected ReclaimStale's attempt count to survive the rejected MarkDone, got %d", got.Attempts)
	}
}

func TestQueue_ProcessOne_PanicInHandlerIsRecoveredAndRetried(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
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
	job, err := q.Enqueue(ctx, def, "PurchaseOrder", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", humanActor())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessOne(ctx, lookupFor(def)); err != nil {
		t.Fatalf("ProcessOne should recover the panic and record it as a failure, not propagate it: %v", err)
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
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
