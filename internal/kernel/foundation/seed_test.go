package foundation

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

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

	name := fmt.Sprintf("uc_test_foundation_%d", time.Now().UnixNano())
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

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

// TestPublish_PublishesEveryFoundationDefinition confirms every All()
// Definition actually lands in the registry as 'published' for the
// tenant, not just that Publish returns nil.
func TestPublish_PublishesEveryFoundationDefinition(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)

	if err := Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned no Definitions — test would pass vacuously")
	}
	for _, def := range all {
		v, err := repo.GetPublished(ctx, def.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", def.EntityType, err)
		}
		if v.Version != def.Version {
			t.Fatalf("%s: expected published version %d, got %d", def.EntityType, def.Version, v.Version)
		}
	}
}

// TestPublish_IsIdempotent confirms a second call is a safe no-op —
// no duplicate-version errors, nothing changes.
func TestPublish_IsIdempotent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()

	if err := Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("second Publish should be a no-op, got: %v", err)
	}
}

// TestPublish_ResumesFromPartiallyDraftedState is the regression test
// for the bug an earlier draft of this function had: checking only
// "does a row exist" (rather than its status) would leave a Definition
// stuck in draft forever if a prior call crashed between CreateDraft and
// Publish. This simulates exactly that: draft one Definition by hand
// (bypassing Publish), then confirm a Publish call still drives it all
// the way to published, not skip it because a row already exists.
func TestPublish_ResumesFromPartiallyDraftedState(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	partyDef := Party()
	raw, err := json.Marshal(partyDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, partyDef.EntityType, partyDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	// Deliberately do NOT approve/publish — simulating a crash right here.

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, "Party")
	if err != nil {
		t.Fatalf("expected Party to be published after resuming from a draft-only state, got: %v", err)
	}
	if v.Version != partyDef.Version {
		t.Fatalf("expected published version %d, got %d", partyDef.Version, v.Version)
	}
}

// TestPublish_ResumesFromApprovedState is TestPublish_ResumesFromPartiallyDraftedState's
// counterpart for the other partial-failure point: a crash after Approve
// but before Publish. publishOne must not re-approve an already-approved
// row (that would hit definitionRepo.transition's atomic status guard
// and error) — it must go straight to Publish.
func TestPublish_ResumesFromApprovedState(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	partyDef := Party()
	raw, err := json.Marshal(partyDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, partyDef.EntityType, partyDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, partyDef.EntityType, partyDef.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Deliberately do NOT publish — simulating a crash right here.

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, "Party")
	if err != nil {
		t.Fatalf("expected Party to be published after resuming from an approved-only state, got: %v", err)
	}
	if v.Version != partyDef.Version {
		t.Fatalf("expected published version %d, got %d", partyDef.Version, v.Version)
	}
}

// TestPublish_LeavesRolledBackVersionAlone confirms a deliberately
// rolled-back version is never silently re-published by a later
// Publish call.
func TestPublish_LeavesRolledBackVersionAlone(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	// Party().Version, not a hardcoded literal: a fresh tenant DB only
	// ever sees the current version published, so a literal here goes
	// stale (and this test starts failing for an unrelated reason) the
	// next time Party's Version bumps — uc-infra#63 hit exactly that
	// rolling from 2 to 3.
	if err := repo.Rollback(ctx, "Party", Party().Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if _, err := repo.GetPublished(ctx, "Party"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected Party to stay rolled back (no published version), got: %v", err)
	}
}

// TestPublishForms_PublishesEveryFoundationForm is PublishForms' proof
// that a published Party form is actually reachable through the
// form_definitions registry — the real gap found by dogfooding the
// purchasing module: without this, GET /forms/Party/... always 404s
// regardless of whether Publish (entities only) has run, since no code
// path ever published the form itself outside a test.
func TestPublishForms_PublishesEveryFoundationForm(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	formRepo := data.NewFormDefinitionRepo(db)

	if err := PublishForms(ctx, db, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}

	v, err := formRepo.GetPublished(ctx, "Party")
	if err != nil {
		t.Fatalf("GetPublished(Party form): %v", err)
	}
	if v.Version != PartyForm().Version {
		t.Fatalf("expected published form version %d, got %d", PartyForm().Version, v.Version)
	}
}

func TestPublishForms_IsIdempotent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()

	if err := PublishForms(ctx, db, humanActor()); err != nil {
		t.Fatalf("first PublishForms: %v", err)
	}
	if err := PublishForms(ctx, db, humanActor()); err != nil {
		t.Fatalf("second PublishForms should be a no-op, got: %v", err)
	}
}

// TestCurrency_DuplicateCodeRejected is the real-Postgres end-to-end
// case for uc-infra#181's Currency.code Unique declaration (the
// Definition-shape half is TestCurrency_UniqueOnCode in
// foundation_test.go): a second Currency created with an already-used
// code must be rejected by crud.Engine itself — record_unique_keys' real
// Postgres UNIQUE index (ADR-0018 §3(c)), not an application-level
// convention a caller could bypass. Mirrors purchasing's
// TestPurchaseOrder_DuplicatePONumberRejected/TestItem_DuplicateSKURejected
// exactly, one field over.
func TestCurrency_DuplicateCodeRejected(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	def := currencyDef(t, tenantDB)
	engine := crud.NewEngine(tenantDB)

	if _, err := engine.Create(ctx, def, map[string]any{
		"code": "USD", "name": "US Dollar",
	}, actor); err != nil {
		t.Fatalf("create first Currency: %v", err)
	}

	_, err := engine.Create(ctx, def, map[string]any{
		"code": "USD", "name": "United States Dollar (duplicate)",
	}, actor)
	if err == nil {
		t.Fatal("expected the second Currency with code \"USD\" to be rejected")
	}
	var uniqueErr *crud.UniqueConstraintError
	if !errors.As(err, &uniqueErr) {
		t.Fatalf("expected a *crud.UniqueConstraintError, got %T: %v", err, err)
	}
	if !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected errors.Is(err, crud.ErrUniqueConstraintViolation): %v", err)
	}
	if uniqueErr.EntityType != "Currency" {
		t.Errorf("UniqueConstraintError.EntityType = %q, want %q", uniqueErr.EntityType, "Currency")
	}
	if len(uniqueErr.Fields) != 1 || uniqueErr.Fields[0] != "code" {
		t.Errorf("UniqueConstraintError.Fields = %v, want [code]", uniqueErr.Fields)
	}

	// A DIFFERENT code must still succeed — this isn't "at most one
	// Currency," only "code itself must be distinct."
	if _, err := engine.Create(ctx, def, map[string]any{
		"code": "GBP", "name": "British Pound",
	}, actor); err != nil {
		t.Fatalf("expected a Currency with a distinct code to succeed: %v", err)
	}
}

// currencyDef fetches the published Currency Definition from tenantDB,
// the same "read back what Publish actually wrote" pattern
// TestDepartment_HierarchyResolvesEndToEnd below uses for Department —
// exercising the real published/unmarshalled shape a crud.Engine call
// sees, not just the in-process Currency() literal.
func currencyDef(t *testing.T, tenantDB *sql.DB) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), "Currency")
	if err != nil {
		t.Fatalf("GetPublished(Currency): %v", err)
	}
	def, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Currency definition: %v", err)
	}
	return def
}

// TestDepartment_HierarchyResolvesEndToEnd is the org-chart mirror of
// finance's TestAccount_HierarchyResolvesEndToEnd: proves
// parent_department_id round-trips through the real crud.Engine against
// real Postgres, not just that Department() declares a reference field.
func TestDepartment_HierarchyResolvesEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	v, err := entityDefs.GetPublished(ctx, "Department")
	if err != nil {
		t.Fatalf("GetPublished(Department): %v", err)
	}
	def, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Department definition: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	parent, err := engine.Create(ctx, def, map[string]any{
		"code": "co", "name": "Company",
	}, actor)
	if err != nil {
		t.Fatalf("create parent Department: %v", err)
	}

	child, err := engine.Create(ctx, def, map[string]any{
		"code": "eng", "name": "Engineering", "parent_department_id": parent.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create child Department: %v", err)
	}

	got, err := engine.Get(ctx, def, child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if got.Data["parent_department_id"] != parent.ID {
		t.Fatalf("expected child's parent_department_id to be %q, got %v", parent.ID, got.Data["parent_department_id"])
	}
}

// TestPosition_HierarchyResolvesEndToEnd mirrors the Department test
// above for Position's own self-reference (reports_to_position_id) and
// confirms department_id (a reference to a DIFFERENT entity type, not a
// self-reference) round-trips in the same Create call.
func TestPosition_HierarchyResolvesEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	deptV, err := entityDefs.GetPublished(ctx, "Department")
	if err != nil {
		t.Fatalf("GetPublished(Department): %v", err)
	}
	deptDef, err := entity.Unmarshal(deptV.Definition)
	if err != nil {
		t.Fatalf("unmarshal Department definition: %v", err)
	}
	posV, err := entityDefs.GetPublished(ctx, "Position")
	if err != nil {
		t.Fatalf("GetPublished(Position): %v", err)
	}
	posDef, err := entity.Unmarshal(posV.Definition)
	if err != nil {
		t.Fatalf("unmarshal Position definition: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	dept, err := engine.Create(ctx, deptDef, map[string]any{
		"code": "fin", "name": "Finance",
	}, actor)
	if err != nil {
		t.Fatalf("create Department: %v", err)
	}

	manager, err := engine.Create(ctx, posDef, map[string]any{
		"title": "Finance Manager", "department_id": dept.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create manager Position: %v", err)
	}

	report, err := engine.Create(ctx, posDef, map[string]any{
		"title": "Accountant", "department_id": dept.ID, "reports_to_position_id": manager.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create report Position: %v", err)
	}

	got, err := engine.Get(ctx, posDef, report.ID)
	if err != nil {
		t.Fatalf("Get(report): %v", err)
	}
	if got.Data["department_id"] != dept.ID {
		t.Fatalf("expected report's department_id to be %q, got %v", dept.ID, got.Data["department_id"])
	}
	if got.Data["reports_to_position_id"] != manager.ID {
		t.Fatalf("expected report's reports_to_position_id to be %q, got %v", manager.ID, got.Data["reports_to_position_id"])
	}
}
