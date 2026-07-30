package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

func systemActor() audit.Actor { return audit.Actor{Type: audit.ActorSystem, ID: "workflow-scheduler"} }

func publishScheduled(t *testing.T, db *sql.DB, name, cron string) {
	t.Helper()
	ctx := context.Background()
	def := &Definition{
		Name: name, Version: 1,
		Trigger: Trigger{Type: TriggerScheduled, Cron: cron},
		Steps:   []Step{{Kind: StepNotify}},
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	repo := data.NewWorkflowDefinitionRepo(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "test"}
	if _, err := repo.CreateDraft(ctx, name, 1, raw, actor); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := repo.Approve(ctx, name, 1, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := repo.Publish(ctx, name, 1, actor); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// Sync registers published scheduled workflows and, crucially, does NOT
// keep pushing next_due_at forward on every tick — recomputing it each
// time would leave the schedule looking perfectly healthy in the table
// while never actually firing.
func TestScheduler_SyncIsIdempotent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	publishScheduled(t, db, "nightly", "0 3 * * *")

	s := NewScheduler(db)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Sync(ctx, now); err != nil {
		t.Fatalf("sync: %v", err)
	}
	first, err := data.NewWorkflowScheduleRepo(db).List(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("expected 1 schedule, got %v (%v)", len(first), err)
	}

	// A later tick must not move the due time.
	if err := s.Sync(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second, _ := data.NewWorkflowScheduleRepo(db).List(ctx)
	if !second[0].NextDueAt.Equal(first[0].NextDueAt) {
		t.Fatalf("sync moved next_due_at from %s to %s — the schedule would never fire",
			first[0].NextDueAt, second[0].NextDueAt)
	}
}

// A workflow that stops being a published scheduled one must stop firing.
func TestScheduler_SyncPrunesUnpublished(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	publishScheduled(t, db, "nightly", "0 3 * * *")
	s := NewScheduler(db)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Sync(ctx, now); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM workflow_definitions`); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if err := s.Sync(ctx, now); err != nil {
		t.Fatalf("resync: %v", err)
	}
	left, _ := data.NewWorkflowScheduleRepo(db).List(ctx)
	if len(left) != 0 {
		t.Fatalf("an unpublished workflow kept its schedule and would keep firing: %v", left)
	}
}

func TestScheduler_FiresWhenDueAndAdvances(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	publishScheduled(t, db, "nightly", "0 3 * * *")
	s := NewScheduler(db)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Sync(ctx, base); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Not due yet.
	fired, err := s.FireDue(ctx, base.Add(time.Hour), systemActor())
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("fired before due: %v", fired)
	}

	// Due at 03:00.
	fired, err = s.FireDue(ctx, base.Add(3*time.Hour), systemActor())
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(fired) != 1 || fired[0] != "nightly" {
		t.Fatalf("expected one fire of nightly, got %v", fired)
	}
	jobs, err := data.NewWorkflowJobRepo(db).ListByStatus(ctx, "queued")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected 1 queued job, got %d (%v)", len(jobs), err)
	}
	if jobs[0].Actor.Type != audit.ActorSystem {
		t.Fatalf("a scheduled run must be attributed to the system actor, got %q", jobs[0].Actor.Type)
	}

	// Immediately firing again must do nothing — the schedule advanced.
	fired, err = s.FireDue(ctx, base.Add(3*time.Hour), systemActor())
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("fired twice for one occurrence: %v", fired)
	}
}

// The anti-stampede rule. A nightly job whose worker was down for a week
// is due seven times over; catching up would fire seven runs at once,
// precisely while the system is recovering. It fires ONCE and returns to
// schedule — "at least this often", never "exactly this many times".
func TestScheduler_DoesNotStampedeAfterDowntime(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	publishScheduled(t, db, "nightly", "0 3 * * *")
	s := NewScheduler(db)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Sync(ctx, base); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// A week later — seven missed occurrences.
	weekLater := base.AddDate(0, 0, 7).Add(10 * time.Hour)
	fired, err := s.FireDue(ctx, weekLater, systemActor())
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("a week of downtime should produce ONE catch-up run, got %d: %v", len(fired), fired)
	}
	jobs, _ := data.NewWorkflowJobRepo(db).ListByStatus(ctx, "queued")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 queued job after downtime, got %d", len(jobs))
	}
	// And the next due time is computed from NOW, not from the backlog.
	list, _ := data.NewWorkflowScheduleRepo(db).List(ctx)
	if !list[0].NextDueAt.After(weekLater) {
		t.Fatalf("next due %s should be after now %s", list[0].NextDueAt, weekLater)
	}
}

// Several workers tick the same tenant concurrently. Without FOR UPDATE
// SKIP LOCKED they would each read the same due row and enqueue the same
// run, which is the failure this whole transaction exists to prevent.
func TestScheduler_ConcurrentWorkersFireOnce(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	publishScheduled(t, db, "nightly", "0 3 * * *")
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := NewScheduler(db).Sync(ctx, base); err != nil {
		t.Fatalf("sync: %v", err)
	}

	due := base.Add(3 * time.Hour)
	var wg sync.WaitGroup
	results := make([][]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := NewScheduler(db).FireDue(ctx, due, systemActor())
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
			results[i] = f
		}(i)
	}
	wg.Wait()

	total := 0
	for _, r := range results {
		total += len(r)
	}
	if total != 1 {
		t.Fatalf("8 concurrent workers fired %d times for one occurrence — expected exactly 1", total)
	}
	jobs, _ := data.NewWorkflowJobRepo(db).ListByStatus(ctx, "queued")
	if len(jobs) != 1 {
		t.Fatalf("expected exactly 1 queued job, got %d", len(jobs))
	}
}
