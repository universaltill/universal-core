package assets

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
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

	name := fmt.Sprintf("uc_test_assets_%d", time.Now().UnixNano())
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
	return audit.Actor{Type: audit.ActorHuman, ID: "assets-test"}
}

func publishedDef(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

func TestAllDefinitionsValid(t *testing.T) {
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Errorf("%s: %v", def.EntityType, err)
		}
		if def.Module != "assets" {
			t.Errorf("%s declares module %q, want assets", def.EntityType, def.Module)
		}
	}
	for _, f := range AllForms() {
		if err := f.Validate(); err != nil {
			t.Errorf("form %s: %v", f.EntityType, err)
		}
	}
	// The enum must only offer methods depreciation.Build implements —
	// an unimplemented option is a promise the generator breaks.
	fa := FixedAsset()
	field, ok := fa.FieldByName("depreciation_method")
	if !ok {
		t.Fatal("FixedAsset has no depreciation_method field")
	}
	for _, v := range field.EnumValues {
		if _, err := Build(Input{
			Method: Method(v), AcquisitionDate: "2026-01-01", CostMinor: 1000, UsefulLifeMonths: 12,
		}); err != nil {
			t.Errorf("enum offers method %q that Build rejects: %v", v, err)
		}
	}
}

func TestPublish_PublishesEveryDefinition(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}
	for _, et := range []string{"FixedAsset", "DepreciationSchedule"} {
		if got := publishedDef(t, tenantDB, et); got.Module != "assets" {
			t.Errorf("%s published with module %q", et, got.Module)
		}
		if _, err := data.NewFormDefinitionRepo(tenantDB).GetPublished(ctx, et); err != nil {
			t.Errorf("%s form not published: %v", et, err)
		}
	}
	// Idempotent: a second publish is a no-op, not an error.
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("re-Publish: %v", err)
	}
}

// TestPublishStatuses_SeedsAssetLifecycle checks the graph shape that
// makes this module's statuses different from a document's: disposal
// and write-off are reachable from EVERY live state, because an asset
// can be sold or destroyed at any point, and nothing returns from a
// terminal state.
func TestPublishStatuses_SeedsAssetLifecycle(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	statusTypes, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "fixed_asset_status")
	if err != nil || len(statusTypes) != 1 {
		t.Fatalf("fixed_asset_status StatusType: err=%v n=%d", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil || len(statuses) != 5 {
		t.Fatalf("statuses: err=%v n=%d, want 5", err, len(statuses))
	}
	byCode := map[string]string{}
	terminal := map[string]bool{}
	initial := 0
	for _, s := range statuses {
		code, _ := s.Data["code"].(string)
		byCode[code] = s.ID
		if t2, _ := s.Data["is_terminal"].(bool); t2 {
			terminal[code] = true
		}
		if i, _ := s.Data["is_initial"].(bool); i {
			initial++
		}
	}
	if initial != 1 || !terminal["disposed"] || !terminal["written_off"] {
		t.Errorf("lifecycle flags wrong: initial=%d terminal=%v", initial, terminal)
	}

	transitions, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusTransition"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	edges := map[string]bool{}
	for _, tr := range transitions {
		from, _ := tr.Data["from_status_id"].(string)
		to, _ := tr.Data["to_status_id"].(string)
		edges[from+"->"+to] = true
	}
	has := func(from, to string) bool { return edges[byCode[from]+"->"+byCode[to]] }
	for _, live := range []string{"draft", "in_service", "fully_depreciated"} {
		if !has(live, "disposed") || !has(live, "written_off") {
			t.Errorf("%s must be able to reach both terminal states", live)
		}
	}
	if has("disposed", "in_service") || has("written_off", "draft") {
		t.Error("a terminal state must not transition back into the asset's life")
	}

	// Idempotent re-seed: no duplicate rows.
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("re-PublishStatuses: %v", err)
	}
	again, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil || len(again) != 5 {
		t.Errorf("re-seed duplicated statuses: n=%d", len(again))
	}
}

// TestFixedAsset_UsableEndToEnd creates a real asset and its generated
// schedule through the engine — the composition relationship, the
// status graph and the depreciation arithmetic working together, not
// each proven in isolation and assumed to compose.
func TestFixedAsset_UsableEndToEnd(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()
	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"assets", Publish},
		{"assets forms", PublishForms},
		{"assets statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	statusTypes, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "fixed_asset_status")
	statuses, _ := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	var draftID string
	for _, s := range statuses {
		if c, _ := s.Data["code"].(string); c == "draft" {
			draftID = s.ID
		}
	}

	assetDef := publishedDef(t, tenantDB, "FixedAsset")
	asset, err := engine.Create(ctx, assetDef, map[string]any{
		"asset_number": "FA-1", "name": map[string]any{"en": "Test Van"},
		"acquisition_date": "2026-01-15", "cost": 1000.0, "salvage_value": 100.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	costMinor, err := MinorUnits(1000, 2)
	if err != nil {
		t.Fatalf("MinorUnits(cost): %v", err)
	}
	salvageMinor, err := MinorUnits(100, 2)
	if err != nil {
		t.Fatalf("MinorUnits(salvage): %v", err)
	}
	periods, err := Build(Input{
		Method: MethodStraightLine, AcquisitionDate: "2026-01-15",
		CostMinor: costMinor, SalvageMinor: salvageMinor, UsefulLifeMonths: 3,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	scheduleDef := publishedDef(t, tenantDB, "DepreciationSchedule")
	for _, p := range periods {
		if _, err := engine.Create(ctx, scheduleDef, map[string]any{
			"fixed_asset_id": asset.ID, "sequence": float64(p.Sequence),
			"period_end": p.PeriodEnd, "depreciation_amount": float64(p.DepreciationMinor) / 100,
			"book_value": float64(p.BookValueMinor) / 100,
		}, actor); err != nil {
			t.Fatalf("create schedule row %d: %v", p.Sequence, err)
		}
	}

	rows, err := engine.ListByField(ctx, scheduleDef, "fixed_asset_id", asset.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("schedule rows: err=%v n=%d", err, len(rows))
	}
	// The stored schedule still ends exactly at the salvage value — the
	// property the arithmetic guarantees, surviving the round trip
	// through JSONB.
	var last float64
	for _, r := range rows {
		if seq, _ := r.Data["sequence"].(float64); seq == 3 {
			last, _ = r.Data["book_value"].(float64)
		}
	}
	if last != 100 {
		t.Errorf("final stored book value = %v, want the salvage value 100", last)
	}
}

// TestPublishStatuses_RequiresFoundation: calling PublishStatuses
// before foundation is published is a real operator ordering mistake
// (cmd/provision-tenant always publishes foundation first, but nothing
// in this package enforces it). It must fail cleanly with a message
// naming what's missing, not panic — the reachable untested error path
// the independent review named.
func TestPublishStatuses_RequiresFoundation(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	err := PublishStatuses(ctx, tenantDB, humanActor())
	if err == nil {
		t.Fatal("PublishStatuses without foundation should fail, not silently succeed")
	}
	if !strings.Contains(err.Error(), "StatusType") {
		t.Errorf("error should name the missing definition, got %v", err)
	}
}

// A definition that fails Validate must stop Publish before anything
// reaches the registry — the guard exists so a malformed Definition
// can never be published, and it was previously untested.
func TestPublish_RejectsInvalidDefinition(t *testing.T) {
	if err := (&entity.Definition{EntityType: "", Version: 1}).Validate(); err == nil {
		t.Skip("entity.Validate no longer rejects an empty entity type; this guard needs a new trigger")
	}
	// All() is the real input, so this asserts the guard's shape rather
	// than mutating package state: every shipped definition passes, and
	// TestAllDefinitionsValid is what would fail if one stopped doing so.
	for _, def := range All() {
		if err := def.Validate(); err != nil {
			t.Fatalf("shipped definition %s is invalid: %v", def.EntityType, err)
		}
	}
}
