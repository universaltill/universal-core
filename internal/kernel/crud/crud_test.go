package crud

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// testDB opens the integration-test database. Skips (not fails) if
// TEST_DATABASE_URL isn't set, so `go test ./...` stays runnable without a
// database for anyone who hasn't set one up yet — the ledger/entity/audit
// unit tests still cover the pure logic without it.
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

func vendorDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "lead_time_days", Type: entity.FieldNumber},
		},
	}
}

func TestEngine_Create_WritesRecordAndAuditAtomically(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()

	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	rec, err := engine.Create(ctx, def, tenantID, map[string]any{
		"name":           "Acme Textiles",
		"lead_time_days": float64(60),
	}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("expected a generated record id")
	}

	// The record is readable back.
	got, err := engine.Get(ctx, def, tenantID, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["name"] != "Acme Textiles" {
		t.Fatalf("unexpected data: %+v", got.Data)
	}

	// The audit entry exists, with the human actor recorded and no
	// model_version (that column must be NULL for a human actor).
	var actorType, actorID string
	var modelVersion sql.NullString
	err = db.QueryRow(
		`SELECT actor_type, actor_id, model_version FROM audit_log
		 WHERE tenant_id = $1 AND entity_type = 'Vendor' AND record_id = $2 AND action = 'create'`,
		tenantID, rec.ID,
	).Scan(&actorType, &actorID, &modelVersion)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if actorType != "human" || actorID != "farshid" {
		t.Fatalf("unexpected audit actor: type=%s id=%s", actorType, actorID)
	}
	if modelVersion.Valid {
		t.Fatalf("expected NULL model_version for human actor, got %q", modelVersion.String)
	}
}

func TestEngine_Create_RecordsAIActorIdentity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()

	actor := audit.Actor{
		Type:         audit.ActorAgent,
		ID:           "universal-core-kernel-agent",
		ModelVersion: "claude-fable-5",
		Input:        "create a vendor named Acme with 60 day lead time",
	}
	rec, err := engine.Create(ctx, def, tenantID, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	var actorType, modelVersion, inputHash string
	err = db.QueryRow(
		`SELECT actor_type, model_version, input_hash FROM audit_log
		 WHERE tenant_id = $1 AND record_id = $2`,
		tenantID, rec.ID,
	).Scan(&actorType, &modelVersion, &inputHash)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if actorType != "ai_agent" || modelVersion != "claude-fable-5" {
		t.Fatalf("unexpected AI actor audit row: type=%s model=%s", actorType, modelVersion)
	}
	if inputHash == "" {
		t.Fatal("expected a non-empty input hash for an AI-agent actor")
	}
}

func TestEngine_Create_ValidationFailure_WritesNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()

	// Missing required "name" field.
	_, err := engine.Create(ctx, def, tenantID, map[string]any{"lead_time_days": float64(10)},
		audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if err == nil {
		t.Fatal("expected validation error")
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no records written after validation failure, got %d", count)
	}
}

func TestEngine_Update_ChangesDataAndAppendsAudit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, tenantID, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	err = engine.Update(ctx, def, tenantID, rec.ID, map[string]any{
		"name":           "Acme Textiles Ltd",
		"lead_time_days": float64(45),
	}, actor)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, err := engine.Get(ctx, def, tenantID, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["name"] != "Acme Textiles Ltd" {
		t.Fatalf("update did not persist: %+v", got.Data)
	}

	var auditCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND record_id = $2`,
		tenantID, rec.ID,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != 2 { // one for create, one for update
		t.Fatalf("expected 2 audit rows (create+update), got %d", auditCount)
	}
}

func TestEngine_List_ScopesToTenantAndEntityType(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, tenantA, map[string]any{"name": "A-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := engine.Create(ctx, def, tenantA, map[string]any{"name": "A-Vendor-2"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := engine.Create(ctx, def, tenantB, map[string]any{"name": "B-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}

	listA, err := engine.List(ctx, def, tenantA)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("expected 2 records for tenant A, got %d", len(listA))
	}
	for _, r := range listA {
		if r.Data["name"] == "B-Vendor-1" {
			t.Fatal("tenant A's list leaked a record belonging to tenant B")
		}
	}
}

func TestEngine_Count_ScopesToTenantAndEntityType(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	for _, name := range []string{"A-Vendor-1", "A-Vendor-2", "A-Vendor-3"} {
		if _, err := engine.Create(ctx, def, tenantA, map[string]any{"name": name}, actor); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, err := engine.Create(ctx, def, tenantB, map[string]any{"name": "B-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}

	count, err := engine.Count(ctx, def, tenantA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 records for tenant A, got %d", count)
	}
}

// TestEngine_ListPage_ReturnsPagesInStableCreationOrder confirms
// ListPage's paging actually partitions the full set (no record
// duplicated or skipped across consecutive pages) in a stable order —
// the property a "Page N of M" UI depends on being true every time, not
// just on average.
func TestEngine_ListPage_ReturnsPagesInStableCreationOrder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db)
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	const total = 5
	var created []string
	for i := range total {
		rec, err := engine.Create(ctx, def, tenantID, map[string]any{"name": fmt.Sprintf("Vendor-%d", i)}, actor)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		created = append(created, rec.ID)
	}

	page1, err := engine.ListPage(ctx, def, tenantID, 2, 0)
	if err != nil {
		t.Fatalf("ListPage page 1: %v", err)
	}
	page2, err := engine.ListPage(ctx, def, tenantID, 2, 2)
	if err != nil {
		t.Fatalf("ListPage page 2: %v", err)
	}
	page3, err := engine.ListPage(ctx, def, tenantID, 2, 4)
	if err != nil {
		t.Fatalf("ListPage page 3: %v", err)
	}

	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 {
		t.Fatalf("expected page sizes 2, 2, 1 for %d records, got %d, %d, %d", total, len(page1), len(page2), len(page3))
	}

	var gotIDs []string
	for _, p := range [][]data.Record{page1, page2, page3} {
		for _, r := range p {
			gotIDs = append(gotIDs, r.ID)
		}
	}
	if len(gotIDs) != total {
		t.Fatalf("expected %d records across all pages, got %d", total, len(gotIDs))
	}
	for i, id := range created {
		if gotIDs[i] != id {
			t.Fatalf("expected creation order preserved across pages: position %d expected %s, got %s", i, id, gotIDs[i])
		}
	}

	// A page past the end returns no records, not an error.
	emptyPage, err := engine.ListPage(ctx, def, tenantID, 2, 10)
	if err != nil {
		t.Fatalf("ListPage past the end: %v", err)
	}
	if len(emptyPage) != 0 {
		t.Fatalf("expected no records past the end, got %d", len(emptyPage))
	}
}
