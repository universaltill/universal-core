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
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
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
	if err := repo.Rollback(ctx, tenantID, "Item", 2, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if _, err := repo.GetPublished(ctx, tenantID, "Item"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected Item to stay rolled back (no published version), got: %v", err)
	}
}

// TestPublishForms_PublishesEveryPurchasingForm mirrors
// foundation/seed_test.go's TestPublishForms_PublishesEveryFoundationForm
// — confirms all four purchasing forms actually land in the
// form_definitions registry.
func TestPublishForms_PublishesEveryPurchasingForm(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	formRepo := data.NewFormDefinitionRepo(db)

	if err := PublishForms(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}

	forms := []struct {
		entityType string
		version    int
	}{
		{"Item", ItemForm().Version},
		{"PurchaseOrder", PurchaseOrderForm().Version},
		{"POLine", POLineForm().Version},
		{"InventoryItem", InventoryItemForm().Version},
	}
	for _, f := range forms {
		v, err := formRepo.GetPublished(ctx, tenantID, f.entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s form): %v", f.entityType, err)
		}
		if v.Version != f.version {
			t.Fatalf("%s form: expected published version %d, got %d", f.entityType, f.version, v.Version)
		}
	}
}

func TestPublishForms_IsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)

	if err := PublishForms(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("first PublishForms: %v", err)
	}
	if err := PublishForms(ctx, db, tenantID, humanActor()); err != nil {
		t.Fatalf("second PublishForms should be a no-op, got: %v", err)
	}
}

// TestPublishStatuses_SeedsGraph confirms PublishStatuses actually
// creates the purchase_order_status StatusType, its 5 Status records
// (draft the only is_initial one; received/cancelled is_terminal), and
// all 6 declared StatusTransition edges — the real data
// crud.Engine.ValidateStatusTransition needs to exist before any
// PurchaseOrder create/update through internal/api can pass (see
// PublishStatuses's doc comment). Only needs foundation.Publish first
// (StatusType/Status/StatusTransition are foundation Definitions), not
// purchasing.Publish itself — PublishStatuses never looks PurchaseOrder
// up.
func TestPublishStatuses_SeedsGraph(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	actor := humanActor()

	if err := foundation.Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := PublishStatuses(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, tenantID, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}

	statusTypes, err := engine.ListByField(ctx, def("StatusType"), tenantID, "code", "purchase_order_status")
	if err != nil {
		t.Fatalf("list StatusType: %v", err)
	}
	if len(statusTypes) != 1 {
		t.Fatalf("expected exactly 1 purchase_order_status StatusType, got %d", len(statusTypes))
	}

	statuses, err := engine.ListByField(ctx, def("Status"), tenantID, "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	wantStatuses := map[string]struct{ isInitial, isTerminal bool }{
		"draft":     {true, false},
		"submitted": {false, false},
		"approved":  {false, false},
		"received":  {false, true},
		"cancelled": {false, true},
	}
	if len(statuses) != len(wantStatuses) {
		t.Fatalf("expected %d Status records, got %d", len(wantStatuses), len(statuses))
	}
	statusIDs := map[string]string{}
	for _, s := range statuses {
		code, _ := s.Data["code"].(string)
		want, ok := wantStatuses[code]
		if !ok {
			t.Fatalf("unexpected Status code %q", code)
		}
		if got := s.Data["is_initial"]; got != want.isInitial {
			t.Errorf("Status %q: expected is_initial=%v, got %v", code, want.isInitial, got)
		}
		if got := s.Data["is_terminal"]; got != want.isTerminal {
			t.Errorf("Status %q: expected is_terminal=%v, got %v", code, want.isTerminal, got)
		}
		statusIDs[code] = s.ID
	}

	transitionDef := def("StatusTransition")
	wantEdges := [][2]string{
		{"draft", "submitted"}, {"submitted", "approved"}, {"approved", "received"},
		{"draft", "cancelled"}, {"submitted", "cancelled"}, {"approved", "cancelled"},
	}
	for _, edge := range wantEdges {
		rows, err := engine.ListByField(ctx, transitionDef, tenantID, "from_status_id", statusIDs[edge[0]])
		if err != nil {
			t.Fatalf("list StatusTransition from %s: %v", edge[0], err)
		}
		found := false
		for _, r := range rows {
			if to, _ := r.Data["to_status_id"].(string); to == statusIDs[edge[1]] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a declared %s->%s StatusTransition", edge[0], edge[1])
		}
	}
}

// TestPublishStatuses_IsIdempotent confirms a second call doesn't
// duplicate the StatusType/Status/StatusTransition rows — same
// getOrCreate-by-natural-key discipline as cmd/seed-demo-data.
func TestPublishStatuses_IsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	actor := humanActor()

	if err := foundation.Publish(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := PublishStatuses(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("first PublishStatuses: %v", err)
	}
	if err := PublishStatuses(ctx, db, tenantID, actor); err != nil {
		t.Fatalf("second PublishStatuses should be a no-op, got: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)
	v, err := entityDefs.GetPublished(ctx, tenantID, "Status")
	if err != nil {
		t.Fatalf("GetPublished(Status): %v", err)
	}
	statusDef, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Status: %v", err)
	}
	all, err := engine.List(ctx, statusDef, tenantID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected exactly 5 Status records after two PublishStatuses calls, got %d", len(all))
	}
}
