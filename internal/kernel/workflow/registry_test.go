package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// TestWorkflowDefinitionRegistry_FullLifecycle is the workflow_definitions
// analogue of entity/registry_test.go's full-lifecycle test — proving
// workflow_definitions (the one table 001_init.sql didn't create;
// 003_definition_registry.sql added it) is wired correctly, keyed by
// name rather than entity_type.
func TestWorkflowDefinitionRegistry_FullLifecycle(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewWorkflowDefinitionRepo(db)
	def := poApprovalWorkflow()
	actor := humanActor()

	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}

	if _, err := repo.CreateDraft(ctx, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := repo.GetPublished(ctx, def.Name)
	if err != nil {
		t.Fatalf("GetPublished: %v", err)
	}
	gotDef, err := Unmarshal(got.Definition)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotDef.Name != def.Name || len(gotDef.Steps) != len(def.Steps) {
		t.Fatalf("round-tripped definition doesn't match: got %+v want %+v", gotDef, def)
	}
}

// TestWorkflowDefinitionRegistry_RollbackLeavesNoPublishedVersion mirrors
// purchasing/seed_test.go's TestPublish_LeavesRolledBackVersionAlone for
// WorkflowDefinitionRepo specifically — the one Definition kind (of the
// three: Entity/Form/Workflow) with no existing Rollback coverage
// anywhere in this repo before this test.
func TestWorkflowDefinitionRegistry_RollbackLeavesNoPublishedVersion(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewWorkflowDefinitionRepo(db)
	def := poApprovalWorkflow()
	actor := humanActor()

	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := repo.Rollback(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if _, err := repo.GetPublished(ctx, def.Name); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected data.ErrNotFound after rollback, got %v", err)
	}
}

// TestRegistryDefinitionLookup_ResolvesExactVersionEnqueuedAgainst
// exercises RegistryDefinitionLookup end to end through Queue.ProcessOne
// — a real registry-backed lookup, not the hand-built stub every other
// test in this file uses. It also confirms the lookup resolves the
// SPECIFIC version a job was enqueued against, not just "whatever's
// published now": v1 stays published for job1 even after v2 is
// published, because job1 already captured WorkflowVersion=1 at Enqueue
// time.
func TestRegistryDefinitionLookup_ResolvesExactVersionEnqueuedAgainst(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()

	v1 := &Definition{Name: "onboarding", Version: 1, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	publish(t, defRepo, v1, actor)

	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, v1, "Employee", "33333333-3333-3333-3333-333333333333", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A newer version gets published after the job was already enqueued
	// against v1 — the running job must still resolve v1, not v2.
	v2 := &Definition{Name: "onboarding", Version: 2, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}, {Kind: StepNotify}}}
	publish(t, defRepo, v2, actor)

	lookup := RegistryDefinitionLookup(db)
	processed, err := q.ProcessOne(ctx, lookup)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if processed.ID != job.ID {
		t.Fatalf("expected to process the enqueued job, got a different one")
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected the job to run v1 (a single notify step) to completion, got status %q", got.Status)
	}
}

// TestRegistryDefinitionLookup_TenantScoped confirms the lookup can't
// resolve a definition published under a different tenant's database —
// proven here via two genuinely separate tenant databases (ADR-0003), a
// stronger proof than the old shared-DB/tenant_id version: tenant B's
// RegistryDefinitionLookup is built against a connection that simply has
// no such row, physically, not a query that happens to filter it out.
func TestRegistryDefinitionLookup_TenantScoped(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	defRepoA := data.NewWorkflowDefinitionRepo(dbA)
	actor := humanActor()

	def := &Definition{Name: "tenant_scoped_wf", Version: 1, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	publish(t, defRepoA, def, actor)

	lookupA := RegistryDefinitionLookup(dbA)
	lookupB := RegistryDefinitionLookup(dbB)
	if _, err := lookupB(ctx, def.Name, def.Version); err == nil {
		t.Fatal("expected tenant B's lookup of tenant A's workflow definition to fail")
	}
	if _, err := lookupA(ctx, def.Name, def.Version); err != nil {
		t.Fatalf("expected tenant A's own lookup to succeed, got %v", err)
	}
}

// publish drives a Definition through CreateDraft -> Approve -> Publish
// in one call, for tests that only care about the end state.
func publish(t *testing.T, repo *data.WorkflowDefinitionRepo, def *Definition, actor audit.Actor) {
	t.Helper()
	ctx := context.Background()
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
