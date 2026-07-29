package sales

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

	name := fmt.Sprintf("uc_test_sales_%d", time.Now().UnixNano())
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

func TestPublish_PublishesEverySalesDefinition(t *testing.T) {
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

// TestPublish_ResumesFromPartiallyDraftedState is the same regression
// coverage as purchasing/seed_test.go's test of the same name.
func TestPublish_ResumesFromPartiallyDraftedState(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	soDef := SalesOrder()
	raw, err := json.Marshal(soDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, soDef.EntityType, soDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	// Deliberately do NOT approve/publish — simulating a crash right here.

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	v, err := repo.GetPublished(ctx, "SalesOrder")
	if err != nil {
		t.Fatalf("expected SalesOrder to be published after resuming from a draft-only state, got: %v", err)
	}
	if v.Version != soDef.Version {
		t.Fatalf("expected published version %d, got %d", soDef.Version, v.Version)
	}
}

func TestPublishForms_PublishesEverySalesForm(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	formRepo := data.NewFormDefinitionRepo(db)

	if err := PublishForms(ctx, db, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}

	for _, f := range AllForms() {
		v, err := formRepo.GetPublished(ctx, f.EntityType)
		if err != nil {
			t.Fatalf("GetPublished(%s form): %v", f.EntityType, err)
		}
		if v.Version != f.Version {
			t.Fatalf("%s form: expected published version %d, got %d", f.EntityType, f.Version, v.Version)
		}
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

// TestPublishStatuses_SeedsBothGraphs confirms PublishStatuses creates
// both sales_order_status and customer_invoice_status StatusTypes, each
// with their own Status/StatusTransition rows — this package seeds two
// StatusTypes, unlike purchasing's one, so this test asserts both
// independently rather than assuming a single shared graph.
func TestPublishStatuses_SeedsBothGraphs(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	statusTypeDef := def("StatusType")
	statusDef := def("Status")
	transitionDef := def("StatusTransition")

	checkGraph := func(code string, wantStatuses map[string]struct{ isInitial, isTerminal bool }, wantEdges [][2]string) {
		t.Helper()
		statusTypes, err := engine.ListByField(ctx, statusTypeDef, "code", code)
		if err != nil {
			t.Fatalf("list StatusType %s: %v", code, err)
		}
		if len(statusTypes) != 1 {
			t.Fatalf("expected exactly 1 %s StatusType, got %d", code, len(statusTypes))
		}

		statuses, err := engine.ListByField(ctx, statusDef, "status_type_id", statusTypes[0].ID)
		if err != nil {
			t.Fatalf("list Status for %s: %v", code, err)
		}
		if len(statuses) != len(wantStatuses) {
			t.Fatalf("%s: expected %d Status records, got %d", code, len(wantStatuses), len(statuses))
		}
		statusIDs := map[string]string{}
		for _, s := range statuses {
			c, _ := s.Data["code"].(string)
			want, ok := wantStatuses[c]
			if !ok {
				t.Fatalf("%s: unexpected Status code %q", code, c)
			}
			if got := s.Data["is_initial"]; got != want.isInitial {
				t.Errorf("%s Status %q: expected is_initial=%v, got %v", code, c, want.isInitial, got)
			}
			if got := s.Data["is_terminal"]; got != want.isTerminal {
				t.Errorf("%s Status %q: expected is_terminal=%v, got %v", code, c, want.isTerminal, got)
			}
			statusIDs[c] = s.ID
		}

		for _, edge := range wantEdges {
			rows, err := engine.ListByField(ctx, transitionDef, "from_status_id", statusIDs[edge[0]])
			if err != nil {
				t.Fatalf("%s: list StatusTransition from %s: %v", code, edge[0], err)
			}
			found := false
			for _, r := range rows {
				if to, _ := r.Data["to_status_id"].(string); to == statusIDs[edge[1]] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: expected a declared %s->%s StatusTransition", code, edge[0], edge[1])
			}
		}
	}

	checkGraph("sales_order_status", map[string]struct{ isInitial, isTerminal bool }{
		"draft":     {true, false},
		"confirmed": {false, false},
		"fulfilled": {false, false},
		"invoiced":  {false, true},
		"cancelled": {false, true},
	}, [][2]string{
		{"draft", "confirmed"}, {"confirmed", "fulfilled"}, {"fulfilled", "invoiced"},
		{"draft", "cancelled"}, {"confirmed", "cancelled"},
	})

	checkGraph("customer_invoice_status", map[string]struct{ isInitial, isTerminal bool }{
		"draft":  {true, false},
		"issued": {false, false},
		"paid":   {false, true},
		"void":   {false, true},
	}, [][2]string{
		{"draft", "issued"}, {"issued", "paid"}, {"draft", "void"}, {"issued", "void"},
	})
}

// TestPublishStatuses_IsIdempotent confirms a second call doesn't
// duplicate rows across either graph — same discipline as purchasing's
// own equivalent test.
func TestPublishStatuses_IsIdempotent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("first PublishStatuses: %v", err)
	}
	if err := PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("second PublishStatuses should be a no-op, got: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(db)
	engine := crud.NewEngine(db)
	v, err := entityDefs.GetPublished(ctx, "Status")
	if err != nil {
		t.Fatalf("GetPublished(Status): %v", err)
	}
	statusDef, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Status: %v", err)
	}
	all, err := engine.List(ctx, statusDef)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	// 5 sales_order_status + 4 customer_invoice_status = 9, regardless of
	// how many times PublishStatuses runs.
	if len(all) != 9 {
		t.Fatalf("expected exactly 9 Status records after two PublishStatuses calls, got %d", len(all))
	}
}

// TestPublish_LeavesRolledBackVersionAlone confirms a deliberately
// rolled-back version is never silently re-published by a later Publish
// call — same coverage as purchasing's own equivalent test.
func TestPublish_LeavesRolledBackVersionAlone(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(db)
	actor := humanActor()

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := repo.Rollback(ctx, "SalesOrder", SalesOrder().Version, actor); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := Publish(ctx, db, actor); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if _, err := repo.GetPublished(ctx, "SalesOrder"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected SalesOrder to stay rolled back (no published version), got: %v", err)
	}
}
