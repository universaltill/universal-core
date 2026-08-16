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

// TestCopySpecs_MutatingResultDoesNotAffectOriginal is the proof behind
// every module's StatusSpecs() doc comment claim ("a fresh copy every
// call, safe for a caller to mutate"): a caller that mutates a returned
// Spec's Translations map — the exact hazard a bare "read-only" comment
// doesn't actually prevent — must not corrupt the original slice/map the
// package's own PublishStatuses seeds from.
func TestCopySpecs_MutatingResultDoesNotAffectOriginal(t *testing.T) {
	original := []Spec{
		{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة"}},
	}
	got := CopySpecs(original)

	got[0].Translations["ar"] = "CORRUPTED"
	got[0].Translations["tr"] = "ADDED"
	if original[0].Translations["ar"] != "مسودة" {
		t.Errorf("mutating the copy's Translations map corrupted the original: %#v", original[0].Translations)
	}
	if _, ok := original[0].Translations["tr"]; ok {
		t.Errorf("adding a key to the copy's Translations map leaked into the original: %#v", original[0].Translations)
	}

	// A nil Translations map must not panic and must round-trip as nil,
	// not an empty-but-non-nil map that would then compare unequal to a
	// Spec built with a literal omitting Translations entirely.
	nilCase := CopySpecs([]Spec{{Code: "done", Name: "Done"}})
	if nilCase[0].Translations != nil {
		t.Errorf("expected nil Translations to copy as nil, got %#v", nilCase[0].Translations)
	}
}

// TestSeed_StatusNameIsWrappedAsI18nText is the unit-level proof for
// uc-infra#214 (ADR-0030): foundation.Status's "name" field is
// FieldI18nText, but every Spec still carries a plain Go string — Seed
// itself must be the thing that wraps it as {"en": Spec.Name}, since no
// caller (purchasing/sales/hr/projects/crm/assets's own PublishStatuses)
// changed. A caller-visible regression here (Seed writing the bare
// string again, or a caller starting to pass an already-wrapped value)
// would fail every StatusType/Status seed at Create time via
// entity.ValidateRecord's i18n_text shape check — this test catches the
// narrower, silent failure mode: Seed writing *some* valid i18n_text
// shape that just isn't the one callers actually asked for.
func TestSeed_StatusNameIsWrappedAsI18nText(t *testing.T) {
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

	ids, err := Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"WidgetName", "widget_name_status", "Widget Name Status",
		[]Spec{{Code: "draft", Name: "Draft", Sequence: 1, IsInitial: true}},
		nil, actor,
	)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	rec, err := engine.Get(ctx, statusDef, ids["draft"])
	if err != nil {
		t.Fatalf("Get seeded Status: %v", err)
	}
	name, ok := rec.Data["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected \"name\" stored as an i18n_text object, got %T: %#v", rec.Data["name"], rec.Data["name"])
	}
	if len(name) != 1 || name["en"] != "Draft" {
		t.Fatalf(`expected exactly {"en": "Draft"}, got %#v`, name)
	}
}

// TestBuildI18nName is the unit-level proof for uc-infra#244's merge
// contract: Translations' locales are merged in alongside Name's "en",
// and a Translations "en" entry (a caller mistake) is silently
// overwritten by Name rather than left to win — see BuildI18nName's own
// doc comment on why that's the deliberate choice, not an oversight.
func TestBuildI18nName(t *testing.T) {
	got := BuildI18nName("Draft", map[string]string{"ar": "مسودة", "tr": "Taslak"})
	want := map[string]any{"en": "Draft", "ar": "مسودة", "tr": "Taslak"}
	if len(got) != len(want) {
		t.Fatalf("expected %d keys, got %d: %#v", len(want), len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: expected %q, got %#v", k, v, got[k])
		}
	}

	// nil Translations: behaves exactly like the pre-uc-infra#244 shape.
	if got := BuildI18nName("Draft", nil); len(got) != 1 || got["en"] != "Draft" {
		t.Fatalf(`expected {"en": "Draft"} for nil Translations, got %#v`, got)
	}

	// A Translations "en" entry must not win over Name.
	if got := BuildI18nName("Draft", map[string]string{"en": "WRONG"}); got["en"] != "Draft" {
		t.Fatalf(`expected Name to override a Translations["en"] entry, got %#v`, got)
	}
}

// TestSeed_TranslationsAreMergedIntoI18nName is the integration-level
// proof that Seed actually wires Spec.Translations through
// BuildI18nName end to end, against a real Postgres row — not just that
// the pure helper is correct in isolation.
func TestSeed_TranslationsAreMergedIntoI18nName(t *testing.T) {
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

	ids, err := Seed(ctx, engine, statusTypeDef, statusDef, transitionDef,
		"WidgetTranslated", "widget_translated_status", "Widget Translated Status",
		[]Spec{
			{Code: "draft", Name: "Draft", Translations: map[string]string{"ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}, Sequence: 1, IsInitial: true},
			// No Translations at all: must behave exactly like the
			// pre-uc-infra#244 English-only shape, proving this is
			// additive/optional per-Spec, not a new requirement.
			{Code: "done", Name: "Done", Sequence: 2, IsTerminal: true},
		},
		nil, actor,
	)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	draftRec, err := engine.Get(ctx, statusDef, ids["draft"])
	if err != nil {
		t.Fatalf("Get draft Status: %v", err)
	}
	draftName, ok := draftRec.Data["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected draft \"name\" as an i18n_text object, got %#v", draftRec.Data["name"])
	}
	want := map[string]any{"en": "Draft", "ar": "مسودة", "fa": "پیش‌نویس", "tr": "Taslak"}
	if len(draftName) != len(want) {
		t.Fatalf("expected %d locales, got %d: %#v", len(want), len(draftName), draftName)
	}
	for k, v := range want {
		if draftName[k] != v {
			t.Errorf("locale %q: expected %q, got %#v", k, v, draftName[k])
		}
	}

	doneRec, err := engine.Get(ctx, statusDef, ids["done"])
	if err != nil {
		t.Fatalf("Get done Status: %v", err)
	}
	doneName, ok := doneRec.Data["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected done \"name\" as an i18n_text object, got %#v", doneRec.Data["name"])
	}
	if len(doneName) != 1 || doneName["en"] != "Done" {
		t.Fatalf(`expected a Spec with no Translations to still produce exactly {"en": "Done"}, got %#v`, doneName)
	}
}
