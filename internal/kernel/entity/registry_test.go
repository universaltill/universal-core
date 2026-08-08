package entity

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
)

// freshTenantDB returns a connection to a brand-new, uniquely-named
// tenant database (ADR-0003) with the tenant migration set applied.
// Skips (not fails) if TEST_DATABASE_URL isn't set — same convention as
// the rest of the kernel's DB-backed tests (e.g. csvimport_test.go,
// queue_test.go).
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

	name := fmt.Sprintf("uc_test_entity_%d", time.Now().UnixNano())
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

func marshalDef(t *testing.T, def *Definition) []byte {
	t.Helper()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	return raw
}

// TestEntityDefinitionRegistry_FullLifecycle exercises the whole
// draft -> approved -> published -> rolled_back state machine end to
// end, round-tripping through json.Marshal on the way in and
// entity.Unmarshal (this package's own decode+validate function) on the
// way out — proving the registry and the Go type actually agree on the
// wire format, not just that the repo's SQL runs.
func TestEntityDefinitionRegistry_FullLifecycle(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	def := vendorDef()
	actor := humanActor()

	created, err := repo.CreateDraft(ctx, def.EntityType, def.Version, marshalDef(t, def), actor)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if created.Status != data.StatusDraft {
		t.Fatalf("expected new version to be %q, got %q", data.StatusDraft, created.Status)
	}

	// Not published yet: no current version to look up.
	if _, err := repo.GetPublished(ctx, def.EntityType); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before publishing, got %v", err)
	}

	if err := repo.Approve(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := repo.GetPublished(ctx, def.EntityType)
	if err != nil {
		t.Fatalf("GetPublished: %v", err)
	}
	if got.Status != data.StatusPublished {
		t.Fatalf("expected published status, got %q", got.Status)
	}
	gotDef, err := Unmarshal(got.Definition)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotDef.EntityType != def.EntityType || len(gotDef.Fields) != len(def.Fields) {
		t.Fatalf("round-tripped definition doesn't match: got %+v want %+v", gotDef, def)
	}

	if err := repo.Rollback(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := repo.GetPublished(ctx, def.EntityType); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after rolling back the only published version, got %v", err)
	}
}

// TestEntityDefinitionRegistry_PublishingNewerVersionSupersedesOlder is
// the regression test for the registry's whole "current = highest
// published version" design: publishing v2 must make GetPublished return
// v2 without any write touching v1's row at all.
func TestEntityDefinitionRegistry_PublishingNewerVersionSupersedesOlder(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	v1 := vendorDef()
	if _, err := repo.CreateDraft(ctx, v1.EntityType, v1.Version, marshalDef(t, v1), actor); err != nil {
		t.Fatalf("CreateDraft v1: %v", err)
	}
	if err := repo.Approve(ctx, v1.EntityType, v1.Version, actor); err != nil {
		t.Fatalf("Approve v1: %v", err)
	}
	if err := repo.Publish(ctx, v1.EntityType, v1.Version, actor); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}

	v2 := &Definition{EntityType: "Vendor", Version: 2, Fields: append(v1.Fields, Field{Name: "rating", Type: FieldNumber})}
	if _, err := repo.CreateDraft(ctx, v2.EntityType, v2.Version, marshalDef(t, v2), actor); err != nil {
		t.Fatalf("CreateDraft v2: %v", err)
	}
	if err := repo.Approve(ctx, v2.EntityType, v2.Version, actor); err != nil {
		t.Fatalf("Approve v2: %v", err)
	}
	if err := repo.Publish(ctx, v2.EntityType, v2.Version, actor); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}

	got, err := repo.GetPublished(ctx, v1.EntityType)
	if err != nil {
		t.Fatalf("GetPublished: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("expected the highest published version (2) to be current, got %d", got.Version)
	}

	// Rolling back v2 falls back to v1, which is still published — no
	// write to v1's row was ever needed.
	if err := repo.Rollback(ctx, v2.EntityType, v2.Version, actor); err != nil {
		t.Fatalf("Rollback v2: %v", err)
	}
	got, err = repo.GetPublished(ctx, v1.EntityType)
	if err != nil {
		t.Fatalf("GetPublished after rollback: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("expected rollback of v2 to fall back to v1, got version %d", got.Version)
	}
}

// TestEntityDefinitionRegistry_TransitionRejectsWrongStatus is the
// regression test for the atomic check-and-set guard on every status
// transition — approving an already-approved version, or publishing a
// version still in draft, must fail rather than silently succeed or
// (worse) silently no-op.
func TestEntityDefinitionRegistry_TransitionRejectsWrongStatus(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	def := vendorDef()
	actor := humanActor()

	if _, err := repo.CreateDraft(ctx, def.EntityType, def.Version, marshalDef(t, def), actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	// Can't publish a draft that was never approved.
	if err := repo.Publish(ctx, def.EntityType, def.Version, actor); !errors.Is(err, data.ErrInvalidStatusTransition) {
		t.Fatalf("expected ErrInvalidStatusTransition publishing an unapproved draft, got %v", err)
	}

	if err := repo.Approve(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Can't approve an already-approved version.
	if err := repo.Approve(ctx, def.EntityType, def.Version, actor); !errors.Is(err, data.ErrInvalidStatusTransition) {
		t.Fatalf("expected ErrInvalidStatusTransition re-approving an already-approved version, got %v", err)
	}

	// Can't roll back a version that was never published.
	if err := repo.Rollback(ctx, def.EntityType, def.Version, actor); !errors.Is(err, data.ErrInvalidStatusTransition) {
		t.Fatalf("expected ErrInvalidStatusTransition rolling back an unpublished version, got %v", err)
	}
}

// TestEntityDefinitionRegistry_TenantIsolation confirms a definition
// drafted under one tenant's database is invisible to another tenant's
// lookups — CLAUDE.md's "single most consequential line of defence"
// rule, applied to the registry same as every other table. Proven here
// via two genuinely separate tenant databases (ADR-0003), a stronger
// proof than the old shared-DB/tenant_id WHERE clause version: there is
// no shared table for a bug to accidentally query across.
func TestEntityDefinitionRegistry_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	repoA := data.NewEntityDefinitionRepo(dbA)
	repoB := data.NewEntityDefinitionRepo(dbB)
	def := vendorDef()
	actor := humanActor()

	if _, err := repoA.CreateDraft(ctx, def.EntityType, def.Version, marshalDef(t, def), actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repoA.Approve(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repoA.Publish(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := repoB.GetPublished(ctx, def.EntityType); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected tenant B's database to see no published Vendor definition, got %v", err)
	}
	// Tenant B's repo also can't transition tenant A's version by ID
	// collision — there is no row with that id in tenant B's own
	// database at all, so this is just ErrInvalidStatusTransition (no
	// row matched), not a cross-database write.
	if err := repoB.Approve(ctx, def.EntityType, def.Version, actor); !errors.Is(err, data.ErrInvalidStatusTransition) {
		t.Fatalf("expected tenant B's Approve call on tenant A's version to affect no rows, got %v", err)
	}
}

// TestEntityDefinitionRegistry_CreateDraftWritesAuditEntry confirms a
// definition draft never exists without an audit_log row, same
// atomic-with-mutation discipline crud.Engine.Create already applies to
// records (CLAUDE.md's audit rule).
func TestEntityDefinitionRegistry_CreateDraftWritesAuditEntry(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	def := vendorDef()
	defJSON := marshalDef(t, def)
	// uc-infra#161: Input must be the prompt/request that produced this
	// change (ADR-0001 §14), not the drafted Definition itself — the
	// drafted JSON is the model's *output*, and CreateDraft already
	// stores it as the audit diff (internal/data/definitions.go), so
	// hashing it as Input too would be tautological. This package's own
	// production ActorAgent callers (install-module/sync-tenant-modules,
	// via internal/kernel/moduleseed) populate Input from the CLI
	// invocation, not the definition bytes (audit.CLIInvocationInput) —
	// this fixture uses an equally representative prompt-shaped string
	// instead, matching this package's own house convention
	// (audit_test.go's "add a field").
	actor := audit.Actor{Type: audit.ActorAgent, ID: "definition-registry-agent", ModelVersion: "claude-fable-5", Input: "add a Vendor entity with a lead_time_days field"}

	if _, err := repo.CreateDraft(ctx, def.EntityType, def.Version, defJSON, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	var count int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = $1 AND actor_type = 'ai_agent'`,
		"entity_definition:Vendor",
	).Scan(&count)
	if err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 audit_log row prefixed entity_definition:Vendor, got %d", count)
	}
}

// TestEntityDefinitionRegistry_CreateDraftRejectsInvalidActor confirms
// audit.Actor.Validate runs before any write — an unattributable
// definition draft must never land.
func TestEntityDefinitionRegistry_CreateDraftRejectsInvalidActor(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	def := vendorDef()

	_, err := repo.CreateDraft(ctx, def.EntityType, def.Version, marshalDef(t, def), audit.Actor{Type: audit.ActorAgent, ID: "no-model-version"})
	if err == nil {
		t.Fatal("expected an ai_agent actor with no ModelVersion to be rejected")
	}
}
