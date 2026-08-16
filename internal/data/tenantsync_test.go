package data_test

import (
	"context"
	"encoding/json"
	"errors"
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

// TestEntityDefinitionRepo_GetPublishedTx covers the Tx-capable read
// uc-infra#165 added — first caller: purchasing.creditInventoryOnReceipt,
// which needs InventoryItem's published Definition from inside the SAME
// transaction as a GoodsReceiptLine write, not a separate connection.
func TestEntityDefinitionRepo_GetPublishedTx(t *testing.T) {
	tenantDB := freshTenantDB(t)
	repo := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	publishDef(t, repo, defWithModule("Alpha", "purchasing"), data.StatusPublished)

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	got, err := repo.GetPublishedTx(ctx, tx, "Alpha")
	if err != nil {
		t.Fatalf("GetPublishedTx: %v", err)
	}
	if got.Version != 1 || got.Status != data.StatusPublished {
		t.Fatalf("got %+v, want version 1, status published", got)
	}
}

func TestEntityDefinitionRepo_GetPublishedTx_NotFound(t *testing.T) {
	tenantDB := freshTenantDB(t)
	repo := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	// Published BEFORE BeginTx (not after, as an earlier draft of this
	// test did) — independent review, uc-infra#165: under READ COMMITTED
	// a tx started first would still see a commit landing on a separate
	// connection mid-tx, so publishing-after-BeginTx only happened to
	// pass rather than genuinely proving anything about ordering. This
	// way the assertion doesn't lean on that isolation-level detail.
	publishDef(t, repo, defWithModule("StillDraft", "purchasing"), data.StatusApproved)

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Nothing published at all for this key — not even a draft.
	_, err = repo.GetPublishedTx(ctx, tx, "NeverPublished")
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("got %v, want data.ErrNotFound", err)
	}

	// A draft/approved-but-never-published version must not satisfy the
	// lookup either — same "published means published" semantics
	// GetPublished (the non-Tx sibling) already enforces.
	_, err = repo.GetPublishedTx(ctx, tx, "StillDraft")
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("got %v, want data.ErrNotFound for an approved-not-published definition", err)
	}
}

func TestEntityDefinitionRepo_GetPublishedTx_ReturnsHighestVersion(t *testing.T) {
	tenantDB := freshTenantDB(t)
	repo := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	def1 := defWithModule("Alpha", "purchasing")
	publishDef(t, repo, def1, data.StatusPublished)
	def2 := defWithModule("Alpha", "purchasing")
	def2.Version = 2
	publishDef(t, repo, def2, data.StatusPublished)

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	got, err := repo.GetPublishedTx(ctx, tx, "Alpha")
	if err != nil {
		t.Fatalf("GetPublishedTx: %v", err)
	}
	// getPublished's own doc comment: publishing a new version never
	// touches older published rows, so both versions stay 'published'
	// and the highest one wins — GetPublishedTx must honor the same
	// ordering as its non-Tx sibling, not just "some published row."
	if got.Version != 2 {
		t.Fatalf("got version %d, want 2 (the highest published version)", got.Version)
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
	mk(map[string]any{"target": "  \t\n "})
	if n := count(); n != 4 {
		t.Errorf("a whitespace-only string must count (see the method's doc comment and #105 — entity.ValidateRecord's Required check now rejects this the same as \"\"): got %d, want 4", n)
	}

	for _, present := range []any{false, float64(0), "real"} {
		mk(map[string]any{"target": present})
		if n := count(); n != 4 {
			t.Errorf("%#v is a present value and must not count: got %d, want 4", present, n)
		}
	}

	if _, err := records.Create(ctx, "Other", map[string]any{"other": "x"}); err != nil {
		t.Fatalf("create Other: %v", err)
	}
	if n := count(); n != 4 {
		t.Errorf("another entity type must not contribute: got %d, want 4", n)
	}

	// Two, so dropping the soft-delete filter cannot coincidentally
	// restore a count that some other dropped clause reduced.
	for i := 0; i < 2; i++ {
		doomed := mk(map[string]any{"other": "x"})
		if err := records.Delete(ctx, "Widget", doomed); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if n := count(); n != 4 {
		t.Errorf("soft-deleted rows must not contribute: got %d, want 4", n)
	}
}

// CountFieldTypeMismatch (uc-infra#214/ADR-0031) is CountMissingField's
// counterpart for a field's declared TYPE changing rather than becoming
// Required — cmd/sync-tenant-modules' typeChangeWarnings is its only
// caller. Same "assert each case as its own delta" discipline as
// TestCountMissingField above, for the same reason: a mixed fixture
// checked as one total can hide two cancelling mistakes.
func TestCountFieldTypeMismatch(t *testing.T) {
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
		// "object" mirrors the Status.name string->i18n_text bump this
		// method was built for: the NEW type is i18n_text (an object),
		// so anything not shaped like an object is a mismatch.
		n, err := records.CountFieldTypeMismatch(ctx, "Widget", "name", "object")
		if err != nil {
			t.Fatalf("CountFieldTypeMismatch: %v", err)
		}
		return n
	}

	if n := count(); n != 0 {
		t.Fatalf("empty fixture counts %d", n)
	}

	// A field with NO value at all is not this method's concern —
	// requiredFieldWarnings' job, not typeChangeWarnings'.
	mk(map[string]any{"other": "x"})
	if n := count(); n != 0 {
		t.Errorf("an absent field must not count as a type mismatch: got %d, want 0", n)
	}

	// A JSON null is excluded the same way, explicitly (see the method's
	// own doc comment on why: CountMissingField already claims it).
	mk(map[string]any{"name": nil})
	if n := count(); n != 0 {
		t.Errorf("a JSON null must not count as a type mismatch: got %d, want 0", n)
	}

	// The actual motivating case: a legacy plain string where the new
	// Definition now declares i18n_text (an object).
	mk(map[string]any{"name": "Draft"})
	if n := count(); n != 1 {
		t.Errorf("a legacy plain string must count as a type mismatch against \"object\": got %d, want 1", n)
	}

	// A number and a bool are equally "not an object" — this method
	// doesn't special-case string, it flags anything jsonb_typeof
	// disagrees with.
	mk(map[string]any{"name": float64(5)})
	if n := count(); n != 2 {
		t.Errorf("a number must count as a type mismatch against \"object\": got %d, want 2", n)
	}
	mk(map[string]any{"name": true})
	if n := count(); n != 3 {
		t.Errorf("a bool must count as a type mismatch against \"object\": got %d, want 3", n)
	}

	// Already the right shape — not a mismatch, backfilled or freshly
	// created either way.
	mk(map[string]any{"name": map[string]any{"en": "Draft"}})
	if n := count(); n != 3 {
		t.Errorf("an already-object value must not count: got %d, want 3", n)
	}
	// An empty object is still an object — no translation is a content
	// question (a follow-up card's job), not a shape mismatch.
	mk(map[string]any{"name": map[string]any{}})
	if n := count(); n != 3 {
		t.Errorf("an empty object must not count as a type mismatch: got %d, want 3", n)
	}

	if _, err := records.Create(ctx, "Other", map[string]any{"name": "Draft"}); err != nil {
		t.Fatalf("create Other: %v", err)
	}
	if n := count(); n != 3 {
		t.Errorf("another entity type must not contribute: got %d, want 3", n)
	}

	doomed := mk(map[string]any{"name": "Draft"})
	if err := records.Delete(ctx, "Widget", doomed); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := count(); n != 3 {
		t.Errorf("a soft-deleted row must not contribute: got %d, want 3", n)
	}

	// The reverse direction (declared type reverts to a plain string —
	// not the Status.name bump's own direction, but the method is
	// symmetric, per its own doc comment: it just compares against
	// whatever expectedJSONType the caller passes): an existing object
	// value now mismatches "string".
	n, err := records.CountFieldTypeMismatch(ctx, "Widget", "name", "string")
	if err != nil {
		t.Fatalf("CountFieldTypeMismatch: %v", err)
	}
	// Live Widget rows with a present, non-null "name" at this point:
	// the plain string (now a MATCH against "string", not counted), the
	// number, the bool, and the two object values (non-empty and empty,
	// both now mismatches against "string") — 4 mismatches.
	if n != 4 {
		t.Fatalf("CountFieldTypeMismatch against \"string\" = %d, want 4", n)
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
