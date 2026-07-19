package moduleseed

import (
	"context"
	"database/sql"
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

// testItems is arbitrary, already-"validated" content — PublishAll
// doesn't interpret Raw at all, only moves it through the registry
// lifecycle, so these don't need to be real entity/form Definitions.
func testItems() []Item {
	return []Item{
		{Key: "Widget", Version: 1, Raw: []byte(`{"entity_type":"Widget"}`)},
		{Key: "Gadget", Version: 1, Raw: []byte(`{"entity_type":"Gadget"}`)},
	}
}

// TestPublishAll_PublishesEveryItem confirms every item actually lands
// in the registry as 'published' for the tenant, not just that
// PublishAll returns nil. Exercised against EntityDefinitionRepo — any
// Repo-satisfying implementation (FormDefinitionRepo included) behaves
// identically since they share the same underlying definitionRepo.
func TestPublishAll_PublishesEveryItem(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)

	items := testItems()
	if err := PublishAll(ctx, repo, tenantID, items, humanActor()); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}
	for _, item := range items {
		v, err := repo.GetPublished(ctx, tenantID, item.Key)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", item.Key, err)
		}
		if v.Version != item.Version {
			t.Fatalf("%s: expected published version %d, got %d", item.Key, item.Version, v.Version)
		}
	}
}

func TestPublishAll_IsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	items := testItems()

	if err := PublishAll(ctx, repo, tenantID, items, humanActor()); err != nil {
		t.Fatalf("first PublishAll: %v", err)
	}
	if err := PublishAll(ctx, repo, tenantID, items, humanActor()); err != nil {
		t.Fatalf("second PublishAll should be a no-op, got: %v", err)
	}
}

// TestPublishAll_ResumesFromPartiallyDraftedState is the regression test
// for the bug internal/kernel/foundation's original Publish had before
// this logic was extracted here: checking only "does a row exist"
// (rather than its status) would leave an item stuck in draft forever if
// a prior call crashed between CreateDraft and Publish.
func TestPublishAll_ResumesFromPartiallyDraftedState(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()
	item := testItems()[0]

	if _, err := repo.CreateDraft(ctx, tenantID, item.Key, item.Version, item.Raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	// Deliberately do NOT approve/publish — simulating a crash right here.

	if err := PublishAll(ctx, repo, tenantID, []Item{item}, actor); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}

	v, err := repo.GetPublished(ctx, tenantID, item.Key)
	if err != nil {
		t.Fatalf("expected %s to be published after resuming from a draft-only state, got: %v", item.Key, err)
	}
	if v.Version != item.Version {
		t.Fatalf("expected published version %d, got %d", item.Version, v.Version)
	}
}

// TestPublishAll_ResumesFromApprovedState is
// TestPublishAll_ResumesFromPartiallyDraftedState's counterpart for the
// other partial-failure point: a crash after Approve but before Publish.
func TestPublishAll_ResumesFromApprovedState(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()
	item := testItems()[0]

	if _, err := repo.CreateDraft(ctx, tenantID, item.Key, item.Version, item.Raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, tenantID, item.Key, item.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Deliberately do NOT publish — simulating a crash right here.

	if err := PublishAll(ctx, repo, tenantID, []Item{item}, actor); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}

	v, err := repo.GetPublished(ctx, tenantID, item.Key)
	if err != nil {
		t.Fatalf("expected %s to be published after resuming from an approved-only state, got: %v", item.Key, err)
	}
	if v.Version != item.Version {
		t.Fatalf("expected published version %d, got %d", item.Version, v.Version)
	}
}

// TestPublishAll_LeavesRolledBackVersionAlone confirms a deliberately
// rolled-back version is never silently re-published by a later
// PublishAll call.
func TestPublishAll_LeavesRolledBackVersionAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()
	item := testItems()[0]

	if err := PublishAll(ctx, repo, tenantID, []Item{item}, actor); err != nil {
		t.Fatalf("first PublishAll: %v", err)
	}
	if err := repo.Rollback(ctx, tenantID, item.Key, item.Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := PublishAll(ctx, repo, tenantID, []Item{item}, actor); err != nil {
		t.Fatalf("second PublishAll: %v", err)
	}

	if _, err := repo.GetPublished(ctx, tenantID, item.Key); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected %s to stay rolled back (no published version), got: %v", item.Key, err)
	}
}
