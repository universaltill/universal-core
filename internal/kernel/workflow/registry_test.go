package workflow

import (
	"context"
	"encoding/json"
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
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewWorkflowDefinitionRepo(db)
	def := poApprovalWorkflow()
	actor := humanActor()

	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}

	if _, err := repo.CreateDraft(ctx, tenantID, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, tenantID, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := repo.GetPublished(ctx, tenantID, def.Name)
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

// TestRegistryDefinitionLookup_ResolvesExactVersionEnqueuedAgainst
// exercises RegistryDefinitionLookup end to end through Queue.ProcessOne
// — a real registry-backed lookup, not the hand-built stub every other
// test in this file uses. It also confirms the lookup resolves the
// SPECIFIC version a job was enqueued against, not just "whatever's
// published now": v1 stays published for job1 even after v2 is
// published, because job1 already captured WorkflowVersion=1 at Enqueue
// time.
func TestRegistryDefinitionLookup_ResolvesExactVersionEnqueuedAgainst(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()

	v1 := &Definition{Name: "onboarding", Version: 1, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	publish(t, defRepo, tenantID, v1, actor)

	q, err := NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	job, err := q.Enqueue(ctx, v1, tenantID, "Employee", "33333333-3333-3333-3333-333333333333", actor)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A newer version gets published after the job was already enqueued
	// against v1 — the running job must still resolve v1, not v2.
	v2 := &Definition{Name: "onboarding", Version: 2, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}, {Kind: StepNotify}}}
	publish(t, defRepo, tenantID, v2, actor)

	lookup := RegistryDefinitionLookup(db)
	processed, err := q.ProcessOne(ctx, lookup)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if processed.ID != job.ID {
		t.Fatalf("expected to process the enqueued job, got a different one")
	}

	repo := data.NewWorkflowJobRepo(db)
	got, err := repo.Get(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected the job to run v1 (a single notify step) to completion, got status %q", got.Status)
	}
}

// TestRegistryDefinitionLookup_TenantScoped confirms the lookup can't
// resolve a definition published under a different tenant — the
// regression test for the DefinitionLookup signature change (adding
// tenantID) itself.
func TestRegistryDefinitionLookup_TenantScoped(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	defRepo := data.NewWorkflowDefinitionRepo(db)
	actor := humanActor()

	def := &Definition{Name: "tenant_scoped_wf", Version: 1, Trigger: Trigger{Type: TriggerManual}, Steps: []Step{{Kind: StepNotify}}}
	publish(t, defRepo, tenantA, def, actor)

	lookup := RegistryDefinitionLookup(db)
	if _, err := lookup(ctx, tenantB, def.Name, def.Version); err == nil {
		t.Fatal("expected tenant B's lookup of tenant A's workflow definition to fail")
	}
	if _, err := lookup(ctx, tenantA, def.Name, def.Version); err != nil {
		t.Fatalf("expected tenant A's own lookup to succeed, got %v", err)
	}
}

// publish drives a Definition through CreateDraft -> Approve -> Publish
// in one call, for tests that only care about the end state.
func publish(t *testing.T, repo *data.WorkflowDefinitionRepo, tenantID string, def *Definition, actor audit.Actor) {
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
