package statusgraph

import (
	"context"
	"database/sql"
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
	"github.com/universaltill/universal-core/internal/kernel/foundation"
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

	name := fmt.Sprintf("uc_test_statusgraph_%d", time.Now().UnixNano())
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

// TestSeed_CodeCollisionScoping is the direct proof of this package's
// load-bearing property (the 2026-07-29 bug): two StatusTypes can both
// declare a "draft" Status, and each graph's rows stay scoped to its
// own status_type_id — a code-only lookup would silently reuse the
// first "draft" row's id for the second graph. Also proves idempotency:
// a full re-seed of both graphs creates nothing new.
func TestSeed_CodeCollisionScoping(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "statusgraph-test"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	defs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := defs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	statusTypeDef, statusDef, transitionDef := def("StatusType"), def("Status"), def("StatusTransition")

	seedBoth := func() (a, b map[string]string) {
		t.Helper()
		a, err := Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
			"EntityA", "a_status", "A Status",
			[]Spec{
				{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true},
				{Code: "done", Name: "Done", Sequence: 2, IsTerminal: true},
			},
			[][2]string{{"draft", "done"}}, actor)
		if err != nil {
			t.Fatalf("seed a_status: %v", err)
		}
		b, err = Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
			"EntityB", "b_status", "B Status",
			[]Spec{
				{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true},
				{Code: "done", Name: "Done", Sequence: 2, IsTerminal: true},
			},
			[][2]string{{"draft", "done"}}, actor)
		if err != nil {
			t.Fatalf("seed b_status: %v", err)
		}
		return a, b
	}

	a1, b1 := seedBoth()
	if a1["draft"] == b1["draft"] || a1["done"] == b1["done"] {
		t.Fatalf("colliding codes must get distinct rows per StatusType: a=%v b=%v", a1, b1)
	}

	// Idempotent re-seed: same ids back, no new rows.
	a2, b2 := seedBoth()
	if a2["draft"] != a1["draft"] || b2["draft"] != b1["draft"] {
		t.Errorf("re-seed changed ids: a1=%v a2=%v b1=%v b2=%v", a1, a2, b1, b2)
	}
	statuses, err := engine.List(ctx, statusDef)
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	if len(statuses) != 4 {
		t.Errorf("expected exactly 4 Status rows after re-seed, got %d", len(statuses))
	}
	transitions, err := engine.List(ctx, transitionDef)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(transitions) != 2 {
		t.Errorf("expected exactly 2 StatusTransition rows after re-seed, got %d", len(transitions))
	}
}
