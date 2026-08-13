package data

import (
	"context"
	"database/sql"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

func agentActor() audit.Actor {
	return audit.Actor{Type: audit.ActorAgent, ID: "kernel-agent", ModelVersion: "claude-fable-5", Input: "run monthly close"}
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

// TestWorkflowJobRepo_ActorAgentRoundTrips_ValidatesAfterEveryScanSite is
// uc-infra#190's regression test: before the fix, every scan site below
// rebuilt an ActorAgent job's Actor with Input == "", so
// Actor.Validate() failed on the rebuilt value even though the actor
// that enqueued the job was perfectly valid. ADR-0027's fix (persist
// input_hash, restore Input as audit.RedactedInput) must make
// Validate() succeed again at every one of them — this exercises all
// four (ClaimNext, Get, ListByStatus, ListWaitingApproval), not just
// one, since uc-infra#190 named all four as broken.
func TestWorkflowJobRepo_ActorAgentRoundTrips_ValidatesAfterEveryScanSite(t *testing.T) {
	db := freshTenantDB(t)
	repo := NewWorkflowJobRepo(db)
	ctx := context.Background()

	actor := agentActor()
	wantHash := actor.InputHash()
	if wantHash == "" {
		t.Fatal("test setup bug: agentActor() must have a non-empty InputHash()")
	}

	enqueued, err := repo.Enqueue(ctx, WorkflowJob{
		WorkflowName:    "close-books",
		WorkflowVersion: 1,
		EntityType:      "FiscalYear",
		RecordID:        "11111111-1111-1111-1111-111111111111",
		Actor:           actor,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// The enqueuing caller's own actor is unaffected by any of this —
	// only what gets read back changes.
	if err := enqueued.Actor.Validate(); err != nil {
		t.Fatalf("actor passed to Enqueue must already be valid, got: %v", err)
	}
	// Enqueue's own return value carries the real hash too, not just the
	// scan sites below — this is what closes the "stored but unreachable"
	// gap independent review of uc-infra#190 found in this fix's first
	// draft: the column's whole point is being retrievable.
	if enqueued.ActorInputHash != wantHash {
		t.Fatalf("Enqueue's returned ActorInputHash = %q, want %q (agentActor().InputHash())", enqueued.ActorInputHash, wantHash)
	}

	// Confirm what actually landed in the column is the real hash, not a
	// placeholder — the one property that makes hash-only storage worth
	// anything over a plain "has_input BOOLEAN". Raw SQL in an
	// internal/data test is established precedent here (e.g.
	// records_test.go, ledger_test.go).
	var storedHash sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT input_hash FROM workflow_jobs WHERE id = $1`, enqueued.ID).Scan(&storedHash); err != nil {
		t.Fatalf("query stored input_hash: %v", err)
	}
	if !storedHash.Valid || storedHash.String != wantHash {
		t.Fatalf("stored workflow_jobs.input_hash = %v, want %q", storedHash, wantHash)
	}

	checkRestored := func(t *testing.T, label string, a audit.Actor, actorInputHash string) {
		t.Helper()
		if err := a.Validate(); err != nil {
			t.Fatalf("%s's rebuilt actor failed Validate(): %v", label, err)
		}
		if a.Input != audit.RedactedInput {
			t.Fatalf("%s's rebuilt Actor.Input = %q, want the redaction sentinel %q", label, a.Input, audit.RedactedInput)
		}
		if a.ModelVersion != "claude-fable-5" {
			t.Fatalf("%s's rebuilt Actor.ModelVersion = %q, want %q", label, a.ModelVersion, "claude-fable-5")
		}
		// The real hash, not the sentinel's own (meaningless, constant)
		// InputHash() — see WorkflowJob.ActorInputHash's own doc comment.
		if actorInputHash != wantHash {
			t.Fatalf("%s's rebuilt ActorInputHash = %q, want %q", label, actorInputHash, wantHash)
		}
	}

	t.Run("ClaimNext", func(t *testing.T) {
		claimed, err := repo.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		if claimed.ID != enqueued.ID {
			t.Fatalf("claimed job %q, want %q", claimed.ID, enqueued.ID)
		}
		checkRestored(t, "ClaimNext", claimed.Actor, claimed.ActorInputHash)
	})

	t.Run("Get", func(t *testing.T) {
		got, err := repo.Get(ctx, enqueued.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		checkRestored(t, "Get", got.Actor, got.ActorInputHash)
	})

	t.Run("ListByStatus", func(t *testing.T) {
		// ClaimNext (above) already moved this job to 'running'.
		jobs, err := repo.ListByStatus(ctx, "running")
		if err != nil {
			t.Fatalf("ListByStatus: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("ListByStatus(running) = %d jobs, want 1", len(jobs))
		}
		checkRestored(t, "ListByStatus", jobs[0].Actor, jobs[0].ActorInputHash)
	})

	t.Run("ListWaitingApproval", func(t *testing.T) {
		if err := repo.MarkWaitingApproval(ctx, enqueued.ID, 0); err != nil {
			t.Fatalf("MarkWaitingApproval: %v", err)
		}
		jobs, err := repo.ListWaitingApproval(ctx)
		if err != nil {
			t.Fatalf("ListWaitingApproval: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("ListWaitingApproval() = %d jobs, want 1", len(jobs))
		}
		checkRestored(t, "ListWaitingApproval", jobs[0].Actor, jobs[0].ActorInputHash)
	})
}

// TestWorkflowJobRepo_HumanActorRoundTrips_InputStaysEmpty confirms this
// change is a no-op for every actor type but ai_agent: a human actor's
// row has a NULL input_hash (nothing to hash — Actor.InputHash()
// returns "" for it), so a restored human actor's Input stays "" exactly
// as it did before this column existed, and Validate() never required
// Input to be non-empty for a human actor anyway.
func TestWorkflowJobRepo_HumanActorRoundTrips_InputStaysEmpty(t *testing.T) {
	db := freshTenantDB(t)
	repo := NewWorkflowJobRepo(db)
	ctx := context.Background()

	enqueued, err := repo.Enqueue(ctx, WorkflowJob{
		WorkflowName: "close-books",
		Actor:        humanActor(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := repo.Get(ctx, enqueued.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := got.Actor.Validate(); err != nil {
		t.Fatalf("human actor failed Validate(): %v", err)
	}
	if got.Actor.Input != "" {
		t.Fatalf("human actor's restored Input = %q, want empty", got.Actor.Input)
	}
}

// TestWorkflowJobRepo_Enqueue_RejectsInvalidActor is uc-infra#190's other
// half, caught by independent review: the original fix only repaired the
// *read* side (a round-tripped actor failing Validate()) and left the
// *write* side open — an invalid ActorAgent (no Input) could still be
// enqueued, reproducing the same failure mode later at read time instead
// of catching it here, at write time, per Queue.Enqueue's own "fail loud
// before persisting" doc comment.
func TestWorkflowJobRepo_Enqueue_RejectsInvalidActor(t *testing.T) {
	db := freshTenantDB(t)
	repo := NewWorkflowJobRepo(db)
	ctx := context.Background()

	invalid := audit.Actor{Type: audit.ActorAgent, ID: "kernel-agent", ModelVersion: "claude-fable-5"} // no Input
	if err := invalid.Validate(); err == nil {
		t.Fatal("test setup bug: this actor must actually be invalid")
	}

	if _, err := repo.Enqueue(ctx, WorkflowJob{WorkflowName: "close-books", Actor: invalid}); err == nil {
		t.Fatal("Enqueue with an invalid actor must return an error, got nil")
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_jobs`).Scan(&count); err != nil {
		t.Fatalf("count workflow_jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("Enqueue with an invalid actor must not persist a row, found %d", count)
	}
}

// TestWorkflowJobRepo_ScheduledSystemActorRoundTrips is the same
// no-op-for-non-agent-actors check as the human-actor test above, for
// ActorSystem specifically (R18 scheduled runs — the actor type
// scheduler.go's FireDue enqueues with).
func TestWorkflowJobRepo_ScheduledSystemActorRoundTrips(t *testing.T) {
	db := freshTenantDB(t)
	repo := NewWorkflowJobRepo(db)
	ctx := context.Background()

	enqueued, err := repo.Enqueue(ctx, WorkflowJob{
		WorkflowName: "month-end-close",
		Actor:        audit.Actor{Type: audit.ActorSystem, ID: "workflow-scheduler"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := repo.Get(ctx, enqueued.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := got.Actor.Validate(); err != nil {
		t.Fatalf("system actor failed Validate(): %v", err)
	}
	if got.Actor.Input != "" {
		t.Fatalf("system actor's restored Input = %q, want empty", got.Actor.Input)
	}
}
