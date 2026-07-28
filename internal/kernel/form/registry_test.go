package form

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

// freshTenantDB/humanActor mirror the convention every other DB-backed
// kernel test uses (see e.g. entity/registry_test.go).
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

	name := fmt.Sprintf("uc_test_form_%d", time.Now().UnixNano())
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

func vendorFormDef() *Definition {
	return &Definition{
		EntityType: "Vendor",
		Version:    1,
		Sections: []Section{
			{Title: "Details", Component: ComponentFields, Fields: []FormField{{Name: "name"}}},
		},
	}
}

// TestFormDefinitionRegistry_FullLifecycle is the form_definitions
// analogue of entity's full-lifecycle test — this one exists mainly to
// prove form_definitions is correctly wired (right table, right column
// set now that 003_definition_registry.sql brought it up to parity with
// entity_definitions' actor-tracking columns), not to re-prove the
// shared transition logic entity/registry_test.go already covers
// thoroughly.
func TestFormDefinitionRegistry_FullLifecycle(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewFormDefinitionRepo(db)
	def := vendorFormDef()
	actor := humanActor()

	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}

	if _, err := repo.CreateDraft(ctx, def.EntityType, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
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
	gotDef, err := Unmarshal(got.Definition)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotDef.EntityType != def.EntityType || len(gotDef.Sections) != len(def.Sections) {
		t.Fatalf("round-tripped definition doesn't match: got %+v want %+v", gotDef, def)
	}
	if got.CreatedByType != string(audit.ActorHuman) || got.CreatedBy != actor.ID {
		t.Fatalf("expected created_by_type/created_by to be recorded (the columns 003_definition_registry.sql added), got type=%q by=%q", got.CreatedByType, got.CreatedBy)
	}
	if got.ApprovedBy != actor.ID {
		t.Fatalf("expected approved_by to record the approving actor, got %q", got.ApprovedBy)
	}

	if err := repo.Rollback(ctx, def.EntityType, def.Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := repo.GetPublished(ctx, def.EntityType); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after rollback, got %v", err)
	}
}

// TestFormDefinitionRegistry_UnmarshalRejectsInvalidStoredDefinition
// confirms Unmarshal's validate-on-decode discipline: a row whose JSONB
// wouldn't pass this package's own Validate (e.g. hand-edited in the
// database, or written by some future non-Go writer) must fail loud when
// read back, not hand a broken Definition to a renderer.
func TestFormDefinitionRegistry_UnmarshalRejectsInvalidStoredDefinition(t *testing.T) {
	_, err := Unmarshal([]byte(`{"entity_type": "", "sections": []}`))
	if err == nil {
		t.Fatal("expected Unmarshal to reject a definition with no entity_type")
	}
}
