package data_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func syncActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "tenantsync-test"}
}

// publishDef drives one Definition through draft -> approve -> publish,
// leaving it at the requested final status so each branch of
// PublishedModules' filter can be exercised.
func publishDef(t *testing.T, repo *data.EntityDefinitionRepo, def *entity.Definition, finalStatus string) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal %s: %v", def.EntityType, err)
	}
	if _, err := repo.CreateDraft(ctx, def.EntityType, def.Version, raw, syncActor()); err != nil {
		t.Fatalf("draft %s: %v", def.EntityType, err)
	}
	if finalStatus == data.StatusDraft {
		return
	}
	if err := repo.Approve(ctx, def.EntityType, def.Version, syncActor()); err != nil {
		t.Fatalf("approve %s: %v", def.EntityType, err)
	}
	if finalStatus == data.StatusApproved {
		return
	}
	if err := repo.Publish(ctx, def.EntityType, def.Version, syncActor()); err != nil {
		t.Fatalf("publish %s: %v", def.EntityType, err)
	}
	if finalStatus == data.StatusRolledBack {
		if err := repo.Rollback(ctx, def.EntityType, def.Version, syncActor()); err != nil {
			t.Fatalf("rollback %s: %v", def.EntityType, err)
		}
	}
}

func defWithModule(entityType, module string) *entity.Definition {
	return &entity.Definition{
		EntityType: entityType, Version: 1, Module: module,
		Fields: []entity.Field{{Name: "name", Type: entity.FieldString, Required: true}},
	}
}

// PublishedModules is the whole basis of ADR-0017's claim that a
// tenant's module set needs no stored column. Every clause of its query
// is load-bearing and, until this test, none was covered: an
// independent review replaced the entire WHERE with `status IS NOT NULL`
// and the suite stayed green.
func TestPublishedModules(t *testing.T) {
	tenantDB := freshTenantDB(t)
	repo := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	publishDef(t, repo, defWithModule("Alpha", "purchasing"), data.StatusPublished)
	publishDef(t, repo, defWithModule("Beta", "purchasing"), data.StatusPublished) // distinct
	publishDef(t, repo, defWithModule("Gamma", "foundation"), data.StatusPublished)
	// Not published: must not contribute a module.
	publishDef(t, repo, defWithModule("Delta", "sales"), data.StatusDraft)
	publishDef(t, repo, defWithModule("Epsilon", "finance"), data.StatusApproved)
	publishDef(t, repo, defWithModule("Zeta", "assets"), data.StatusRolledBack)
	// Published but carrying no module key: skipped, per the method's
	// own promise, rather than yielding a module named "".
	publishDef(t, repo, defWithModule("Eta", ""), data.StatusPublished)

	got, err := repo.PublishedModules(ctx)
	if err != nil {
		t.Fatalf("PublishedModules: %v", err)
	}
	want := []string{"foundation", "purchasing"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (distinct and sorted)", got, want)
		}
	}
}

func TestPublishedModules_EmptyRegistry(t *testing.T) {
	repo := data.NewEntityDefinitionRepo(freshTenantDB(t))
	got, err := repo.PublishedModules(context.Background())
	if err != nil {
		t.Fatalf("PublishedModules: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a tenant with nothing published must yield no modules, got %v", got)
	}
}

// CountMissingField decides whether an operator is told to run a
// backfill. A false positive sends them chasing a migration that isn't
// needed; a false negative leaves a tenant broken on next edit. The
// boundary cases are the whole point: `false` and `0` are present
// values, not missing ones.
func TestCountMissingField(t *testing.T) {
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	ctx := context.Background()

	mk := func(fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, "Widget", fields)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return rec.ID
	}
	count := func() int {
		t.Helper()
		n, err := records.CountMissingField(ctx, "Widget", "target")
		if err != nil {
			t.Fatalf("CountMissingField: %v", err)
		}
		return n
	}

	// Each case is asserted on its own, as a delta. An earlier version
	// of this test checked only one total over a mixed fixture, and a
	// mutation that both dropped the soft-delete filter and stopped
	// counting empty strings produced the same number — two errors
	// cancelling exactly. Deltas cannot cancel.
	if n := count(); n != 0 {
		t.Fatalf("empty fixture counts %d", n)
	}

	mk(map[string]any{"other": "x"})
	if n := count(); n != 1 {
		t.Errorf("an absent field must count: got %d, want 1", n)
	}
	mk(map[string]any{"target": nil})
	if n := count(); n != 2 {
		t.Errorf("a JSON null must count: got %d, want 2", n)
	}
	mk(map[string]any{"target": ""})
	if n := count(); n != 3 {
		t.Errorf("an empty string must count (see the method's doc comment and #86): got %d, want 3", n)
	}

	for _, present := range []any{false, float64(0), "real"} {
		mk(map[string]any{"target": present})
		if n := count(); n != 3 {
			t.Errorf("%#v is a present value and must not count: got %d, want 3", present, n)
		}
	}

	if _, err := records.Create(ctx, "Other", map[string]any{"other": "x"}); err != nil {
		t.Fatalf("create Other: %v", err)
	}
	if n := count(); n != 3 {
		t.Errorf("another entity type must not contribute: got %d, want 3", n)
	}

	// Two, so dropping the soft-delete filter cannot coincidentally
	// restore a count that some other dropped clause reduced.
	for i := 0; i < 2; i++ {
		doomed := mk(map[string]any{"other": "x"})
		if err := records.Delete(ctx, "Widget", doomed); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if n := count(); n != 3 {
		t.Errorf("soft-deleted rows must not contribute: got %d, want 3", n)
	}
}

// false and 0 are the cases most likely to be broken by a naive
// "is it falsy" implementation, and the ones whose failure is loudest:
// every boolean and every zero quantity in the tenant would be reported
// as needing a migration.
func TestCountMissingField_FalseAndZeroAreNotMissing(t *testing.T) {
	records := data.NewRecordRepo(freshTenantDB(t))
	ctx := context.Background()
	for _, v := range []any{false, float64(0)} {
		if _, err := records.Create(ctx, "Widget", map[string]any{"target": v}); err != nil {
			t.Fatalf("create %v: %v", v, err)
		}
	}
	n, err := records.CountMissingField(ctx, "Widget", "target")
	if err != nil {
		t.Fatalf("CountMissingField: %v", err)
	}
	if n != 0 {
		t.Errorf("CountMissingField = %d, want 0 — false and 0 are present values", n)
	}
}

// f64 takes the address of a float64 literal — Go has no `&0.0` for a
// literal, same reason entity.Float64Ptr exists; kept local to this test
// file rather than importing the entity package for one helper.
func f64(v float64) *float64 { return &v }

// CountOutOfRangeField is CountMissingField's counterpart for
// entity.Field.Min/Max (uc-infra#80) — same "an operator must be told
// before a record silently becomes uneditable" stakes, so the boundary
// cases get the same delta-per-case treatment TestCountMissingField
// uses.
func TestCountOutOfRangeField(t *testing.T) {
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	ctx := context.Background()

	mk := func(fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, "Widget", fields)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return rec.ID
	}
	count := func(min, max *float64) int {
		t.Helper()
		n, err := records.CountOutOfRangeField(ctx, "Widget", "qty", min, max)
		if err != nil {
			t.Fatalf("CountOutOfRangeField: %v", err)
		}
		return n
	}

	if n := count(f64(0), nil); n != 0 {
		t.Fatalf("empty fixture counts %d", n)
	}

	mk(map[string]any{"qty": -1.0})
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("a value below Min must count: got %d, want 1", n)
	}
	mk(map[string]any{"qty": 0.0})
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("a value AT the inclusive Min must not count: got %d, want 1", n)
	}
	mk(map[string]any{"qty": 100.0})
	if n := count(f64(0), f64(24)); n != 2 {
		t.Errorf("a value above Max must count: got %d, want 2", n)
	}
	if n := count(f64(0), f64(100)); n != 1 {
		t.Errorf("a value AT the inclusive Max must not count: got %d, want 1", n)
	}
	if n := count(nil, nil); n != 0 {
		t.Errorf("no declared bound must never count anything: got %d, want 0", n)
	}
	mk(map[string]any{"qty": "not a number"})
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("a non-numeric value must not count (jsonb_typeof guard, not a cast error): got %d, want 1", n)
	}
	mk(map[string]any{"other": "x"})
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("an absent field must not count — CountMissingField's job, not this one's: got %d, want 1", n)
	}

	if _, err := records.Create(ctx, "Other", map[string]any{"qty": -1.0}); err != nil {
		t.Fatalf("create Other: %v", err)
	}
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("another entity type must not contribute: got %d, want 1", n)
	}

	for i := 0; i < 2; i++ {
		doomed := mk(map[string]any{"qty": -1.0})
		if err := records.Delete(ctx, "Widget", doomed); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if n := count(f64(0), nil); n != 1 {
		t.Errorf("soft-deleted rows must not contribute: got %d, want 1", n)
	}
}
