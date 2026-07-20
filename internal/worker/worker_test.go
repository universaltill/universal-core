package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
)

// testDB, seedTenant, humanActor, and publish mirror
// internal/kernel/workflow's identically-named unexported test helpers —
// same convention as every other package's *_test.go in this repo
// (crud_test.go, queue_test.go): each package owns its own copy rather
// than exporting test-only helpers across package boundaries.
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

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

// publish drives a Definition through CreateDraft -> Approve -> Publish —
// worker.Runner resolves definitions via workflow.RegistryDefinitionLookup
// (the real registry-backed lookup), unlike internal/kernel/workflow's own
// tests which mostly use a hand-built stub, so every test here needs a
// genuinely published definition.
func publish(t *testing.T, repo *data.WorkflowDefinitionRepo, tenantID string, def *workflow.Definition, actor audit.Actor) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition %s: %v", def.Name, err)
	}
	if _, err := repo.CreateDraft(ctx, tenantID, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft %s v%d: %v", def.Name, def.Version, err)
	}
	if err := repo.Approve(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve %s v%d: %v", def.Name, def.Version, err)
	}
	if err := repo.Publish(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish %s v%d: %v", def.Name, def.Version, err)
	}
}

// fastTestConfig polls aggressively so tests don't wait a real
// PollInterval-scale amount of wall-clock time.
func fastTestConfig() Config {
	return Config{PollInterval: 20 * time.Millisecond, LeaseTimeout: 200 * time.Millisecond, Concurrency: 1}
}

func TestRunner_ProcessesEnqueuedJobViaPolling(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tenantID := seedTenant(t, db)

	def := &workflow.Definition{
		Name: "poll_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()
	publish(t, defRepo, tenantID, def, actor)

	processed := make(chan string, 1)
	r, err := New(db, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			processed <- job.ID
			return nil
		},
	}, fastTestConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "11111111-1111-1111-1111-111111111111", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	go r.Run(ctx)

	select {
	case gotID := <-processed:
		if gotID != job.ID {
			t.Fatalf("expected the enqueued job %s to be processed, got %s", job.ID, gotID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the poll loop to pick up the enqueued job")
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected status done, got %q", got.Status)
	}
}

func TestRunner_ReclaimsStaleJobsEachTick(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tenantID := seedTenant(t, db)

	def := &workflow.Definition{
		Name: "reclaim_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()
	publish(t, defRepo, tenantID, def, actor)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", "22222222-2222-2222-2222-222222222222", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate a worker that claimed the job and vanished (SIGKILL, OOM)
	// before ever calling MarkDone/MarkFailed/MarkWaitingApproval — same
	// setup as workflow package's own ReclaimStale test.
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_jobs SET status = 'running', updated_at = now() - interval '1 hour' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("simulate orphaned job: %v", err)
	}

	processed := make(chan string, 1)
	cfg := fastTestConfig()
	r, err := New(db, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			processed <- job.ID
			return nil
		},
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	go r.Run(ctx)

	select {
	case gotID := <-processed:
		if gotID != job.ID {
			t.Fatalf("expected the reclaimed job %s to be processed, got %s", job.ID, gotID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the orphaned job to be reclaimed and processed")
	}
}

func TestRunner_StopsOnContextCancellation(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	r, err := New(db, nil, fastTestConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Let it poll at least once against an empty queue, then stop it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

func TestRunner_RunConcurrent_ProcessesAllJobsExactlyOnce(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tenantID := seedTenant(t, db)

	def := &workflow.Definition{
		Name: "concurrent_worker_test", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	}
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()
	publish(t, defRepo, tenantID, def, actor)

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	const n = 8
	recordIDs := make([]string, n)
	for i := range n {
		recordIDs[i] = "33333333-3333-3333-3333-33333333333" + string(rune('0'+i))
		if _, err := q.Enqueue(ctx, def, tenantID, "PurchaseOrder", recordIDs[i], actor); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	var processedCount atomic.Int64
	var mu sync.Mutex
	seen := map[string]bool{}
	cfg := fastTestConfig()
	cfg.Concurrency = 4
	r, err := New(db, map[workflow.StepKind]workflow.StepHandler{
		workflow.StepNotify: func(_ context.Context, job data.WorkflowJob, _ workflow.Step) error {
			mu.Lock()
			if seen[job.ID] {
				mu.Unlock()
				t.Errorf("job %s processed more than once", job.ID)
				return nil
			}
			seen[job.ID] = true
			mu.Unlock()
			processedCount.Add(1)
			return nil
		},
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r.RunConcurrent(ctx)

	deadline := time.After(5 * time.Second)
	for {
		if processedCount.Load() == n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: only %d/%d jobs processed", processedCount.Load(), n)
		case <-time.After(20 * time.Millisecond):
		}
	}

	for _, id := range recordIDs {
		var status string
		if err := db.QueryRowContext(ctx,
			`SELECT status FROM workflow_jobs WHERE tenant_id = $1 AND record_id = $2`, tenantID, id,
		).Scan(&status); err != nil {
			t.Fatalf("query status for record %s: %v", id, err)
		}
		if status != "done" {
			t.Errorf("record %s: expected status done, got %q", id, status)
		}
	}
}
