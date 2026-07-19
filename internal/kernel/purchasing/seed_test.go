package purchasing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

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
	return id
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

// TestPublish_PublishesEveryPurchasingDefinition confirms every All()
// Definition actually lands in the registry as 'published' for the
// tenant — Publish doesn't itself require the foundation layer to be
// published first (entity.Definition.Validate doesn't resolve
// FieldReference targets against the registry, only against the
// Definition's own shape — see entity/validate.go), so this test alone
// doesn't need foundation.Publish first. A real deployment would always
// run foundation.Publish before offering Purchasing (ADR-0001 §8), but
// nothing in this package enforces that yet (module-gating isn't built —
// QUEUE.md).
func TestPublish_PublishesEveryPurchasingDefinition(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)

	if err := Publish(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned no Definitions — test would pass vacuously")
	}
	for _, def := range all {
		v, err := repo.GetPublished(ctx, tenantID, def.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", def.EntityType, err)
		}
		if v.Version != def.Version {
			t.Fatalf("%s: expected published version %d, got %d", def.EntityType, def.Version, v.Version)
		}
	}
}

// TestPublish_IsIdempotent confirms a second call is a safe no-op — no
// duplicate-version errors, nothing changes.
func TestPublish_IsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)

	if err := Publish(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := Publish(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("second Publish should be a no-op, got: %v", err)
	}
}

// TestPublish_ResumesFromPartiallyDraftedState is the same regression
// coverage as foundation/seed_test.go's test of the same name, applied
// to this package's own copy of publishOne: draft one Definition by
// hand (bypassing Publish), then confirm Publish still drives it all the
// way to published rather than skipping it because a row already exists.
func TestPublish_ResumesFromPartiallyDraftedState(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	itemDef := Item()
	raw, err := json.Marshal(itemDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, tenantID, itemDef.EntityType, itemDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	// Deliberately do NOT approve/publish — simulating a crash right here.

	if err := Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, tenantID, "Item")
	if err != nil {
		t.Fatalf("expected Item to be published after resuming from a draft-only state, got: %v", err)
	}
	if v.Version != itemDef.Version {
		t.Fatalf("expected published version %d, got %d", itemDef.Version, v.Version)
	}
}

// TestPublish_ResumesFromApprovedState is
// TestPublish_ResumesFromPartiallyDraftedState's counterpart for the
// other partial-failure point: a crash after Approve but before Publish.
func TestPublish_ResumesFromApprovedState(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	itemDef := Item()
	raw, err := json.Marshal(itemDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, tenantID, itemDef.EntityType, itemDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, tenantID, itemDef.EntityType, itemDef.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Deliberately do NOT publish — simulating a crash right here.

	if err := Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, tenantID, "Item")
	if err != nil {
		t.Fatalf("expected Item to be published after resuming from an approved-only state, got: %v", err)
	}
	if v.Version != itemDef.Version {
		t.Fatalf("expected published version %d, got %d", itemDef.Version, v.Version)
	}
}

// TestPublish_LeavesRolledBackVersionAlone confirms a deliberately
// rolled-back version is never silently re-published by a later Publish
// call.
func TestPublish_LeavesRolledBackVersionAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	if err := Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := repo.Rollback(ctx, tenantID, "Item", 1, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if _, err := repo.GetPublished(ctx, tenantID, "Item"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected Item to stay rolled back (no published version), got: %v", err)
	}
}
