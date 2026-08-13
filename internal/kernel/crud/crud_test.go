package crud

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// freshTenantDB returns a connection to a brand-new, uniquely-named
// tenant database (ADR-0003) with the tenant migration set applied.
// Skips (not fails) if TEST_DATABASE_URL isn't set, so `go test ./...`
// stays runnable without a database for anyone who hasn't set one up yet
// — the ledger/entity/audit unit tests still cover the pure logic
// without it.
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

	name := fmt.Sprintf("uc_test_crud_%d", time.Now().UnixNano())
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
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	rec, err := engine.Create(ctx, def, map[string]any{
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
	got, err := engine.Get(ctx, def, rec.ID)
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
		 WHERE entity_type = 'Vendor' AND record_id = $1 AND action = 'create'`,
		rec.ID,
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
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	actor := audit.Actor{
		Type:         audit.ActorAgent,
		ID:           "universal-core-kernel-agent",
		ModelVersion: "claude-fable-5",
		Input:        "create a vendor named Acme with 60 day lead time",
	}
	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	var actorType, modelVersion, inputHash string
	err = db.QueryRow(
		`SELECT actor_type, model_version, input_hash FROM audit_log
		 WHERE record_id = $1`,
		rec.ID,
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

// defaultingVendorDef mirrors vendorDef but with a Default declared on
// each non-string field type this package's own Definition.Validate
// type-checks Default for (bool, enum), used to prove engine.Create
// applies entity.Field.Default end-to-end against a real tenant
// database, not just at the entity.ApplyDefaults unit level.
func defaultingVendorDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "lead_time_days", Type: entity.FieldNumber},
			{Name: "active", Type: entity.FieldBool, Default: true},
			{Name: "payment_terms", Type: entity.FieldEnum, EnumValues: []string{"prepaid", "DP", "TT", "LC"}, Default: "DP"},
		},
	}
}

// TestEngine_Create_AppliesFieldDefaultForOmittedFields is the
// integration-level proof for uc-infra#212: a field genuinely absent
// from the create payload — not just zero-valued — gets its
// Definition-declared Default, for every field type that carries one,
// through the real validate-then-persist path against a real tenant
// database. See internal/kernel/entity's TestApplyDefaults for the pure
// per-type unit coverage this exercises end-to-end.
func TestEngine_Create_AppliesFieldDefaultForOmittedFields(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := defaultingVendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme Textiles"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["active"] != true {
		t.Errorf("Data[\"active\"] = %v, want Definition-declared Default true", got.Data["active"])
	}
	if got.Data["payment_terms"] != "DP" {
		t.Errorf(`Data["payment_terms"] = %v, want Definition-declared Default "DP"`, got.Data["payment_terms"])
	}
	if _, present := got.Data["lead_time_days"]; present {
		t.Errorf(`Data["lead_time_days"] = %v, want left absent (no Default declared, field not required)`, got.Data["lead_time_days"])
	}
}

// TestEngine_Create_ExplicitValueNotOverriddenByDefault is the
// companion case: a caller that explicitly submits a value for a
// defaulted field — including bool's own zero value, false, which
// entity.ValidateRecord accepts unlike an empty string against
// FieldEnum's closed EnumValues set — is making a real choice, not
// omitting the field, and engine.Create must persist exactly that,
// never substitute the Definition's Default over an explicitly-set
// value.
func TestEngine_Create_ExplicitValueNotOverriddenByDefault(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := defaultingVendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"name":          "Acme Textiles",
		"active":        false, // bool's own zero value — a real, explicit choice
		"payment_terms": "TT",  // valid, but not the declared Default "DP"
	}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["active"] != false {
		t.Errorf(`Data["active"] = %v, want explicit false preserved, not overridden by Default true`, got.Data["active"])
	}
	if got.Data["payment_terms"] != "TT" {
		t.Errorf(`Data["payment_terms"] = %v, want explicit "TT" preserved, not overridden by Default "DP"`, got.Data["payment_terms"])
	}
}

// TestEngine_Create_HookMutatingI18nDefaultDoesNotLeakToNextCreate is the
// integration-level proof for uc-infra#219, end to end through the real
// path a shipped Hook would take: data.Record.Data is the very map
// entity.ApplyDefaults wrote a FieldI18nText Default into
// (data.RecordRepo.CreateTx returns the same map it was handed), and
// runHook passes that same rec straight to any registered Hook. Before
// the fix, a hook mutating rec.Data[i18nField] in place here would
// corrupt the shared *entity.Definition's own Field.Default for every
// later Create against that Definition — this simulates exactly that
// hook shape and proves it can no longer happen. See
// internal/kernel/entity's TestApplyDefaults/TestCloneDefault for the
// pure per-function unit coverage this exercises through the real
// database-backed write path.
func TestEngine_Create_HookMutatingI18nDefaultDoesNotLeakToNextCreate(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := &entity.Definition{
		EntityType: "Asset",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "notes", Type: entity.FieldI18nText, Default: map[string]any{"en": "No notes yet"}},
		},
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A hook that mutates the record's own i18n_text field in place —
	// not a hypothetical, just an ordinary in-place edit a real Hook
	// could plausibly make (e.g. appending an audit note to a shared
	// text field) with no reason to suspect it's touching Definition
	// state rather than its own record's data.
	engine.SetHook("Asset", func(_ context.Context, _ *sql.Tx, _ *entity.Definition, rec data.Record, _ audit.Action, _ audit.Actor) error {
		if notes, ok := rec.Data["notes"].(map[string]any); ok {
			notes["en"] = "mutated by record #1's hook"
		}
		return nil
	})

	rec1, err := engine.Create(ctx, def, map[string]any{"name": "Forklift"}, actor)
	if err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	// The hook fires after data.RecordRepo.CreateTx has already inserted
	// (marshaled-to-JSON) record #1's row, so its in-place mutation of
	// rec.Data never reaches what's actually stored for record #1 either
	// way — that's not the invariant this test is after. What matters is
	// whether the hook's mutation reached the map ApplyDefaults handed
	// out, i.e. the *Definition's own Default, checked directly below.
	rec1Notes, ok := rec1.Data["notes"].(map[string]any)
	if !ok || rec1Notes["en"] != "mutated by record #1's hook" {
		t.Fatalf("expected the hook's own mutation to have actually landed on rec1.Data (sanity check on the test itself): %v", rec1.Data["notes"])
	}

	// The Definition's own Default must be untouched by record #1's
	// hook — this is the actual invariant uc-infra#219 protects.
	defDefault, ok := def.Fields[1].Default.(map[string]any)
	if !ok {
		t.Fatalf("def.Fields[1].Default = %v (%T), want map[string]any", def.Fields[1].Default, def.Fields[1].Default)
	}
	if defDefault["en"] != "No notes yet" {
		t.Fatalf("def.Fields[1].Default[\"en\"] = %v, want the Definition's own stored Default left untouched by record #1's hook", defDefault["en"])
	}

	rec2, err := engine.Create(ctx, def, map[string]any{"name": "Pallet Jack"}, actor)
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	got2, err := engine.Get(ctx, def, rec2.ID)
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	notes2, ok := got2.Data["notes"].(map[string]any)
	if !ok || notes2["en"] != "No notes yet" {
		t.Errorf(`record #2's notes = %v, want the pristine Default "No notes yet" — record #1's hook mutation must not leak into record #2`, got2.Data["notes"])
	}
}

// TestEngine_Create_NilFieldsMapDoesNotPanic proves Create's own nil
// normalization (immediately above the entity.ApplyDefaults call) does
// its job. entity.ApplyDefaults writes into the map it's given, and
// writing into a nil map panics — a Definition with no Required fields
// was previously a valid nil-fields Create (entity.ValidateRecord reads
// a nil map safely, and there's nothing Required to fail on), so it
// must not start panicking now that Create also writes into that map to
// apply Defaults. Every field here is optional and has a Default, so
// this test only passes if the nil map actually got normalized AND
// Defaults actually got applied to the (former-nil) map — either
// omitted step would either panic or leave the record undefaulted.
func TestEngine_Create_NilFieldsMapDoesNotPanic(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "active", Type: entity.FieldBool, Default: true},
		},
	}
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, nil, actor)
	if err != nil {
		t.Fatalf("Create with a nil fields map should succeed (no Required fields), got: %v", err)
	}

	got, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["active"] != true {
		t.Errorf(`Data["active"] = %v, want the Default true applied to the normalized (former-nil) map`, got.Data["active"])
	}
}

func TestEngine_Create_ValidationFailure_WritesNothing(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	// Missing required "name" field.
	_, err := engine.Create(ctx, def, map[string]any{"lead_time_days": float64(10)},
		audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if err == nil {
		t.Fatal("expected validation error")
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records`).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no records written after validation failure, got %d", count)
	}
}

// TestEngine_Create_RequiredFieldAsEmptyString_WritesNothing is the
// integration-level pin for #86: a direct API call (unlike the form
// handler and CSV importer, which both convert a blank input to absent
// before this same engine ever sees it) can submit a required field as
// "" rather than omitting it. Engine.Create must reject that the same as
// missing/nil, through the real validate-then-write path against a real
// tenant database — not just at the entity.ValidateRecord unit level.
func TestEngine_Create_RequiredFieldAsEmptyString_WritesNothing(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	_, err := engine.Create(ctx, def, map[string]any{"name": "", "lead_time_days": float64(10)},
		audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("expected a required-field error for \"name\" submitted as \"\", got: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records`).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no records written after validation failure, got %d", count)
	}
}

// TestEngine_Create_RequiredFieldAsWhitespaceOnlyString_WritesNothing is
// the integration-level pin for uc-infra#105, the same shape as the
// empty-string test above: a direct API call can submit a required
// field as "   " rather than "" or omitting it, and Engine.Create must
// reject that through the real validate-then-write path too, not just
// at the entity.ValidateRecord unit level.
func TestEngine_Create_RequiredFieldAsWhitespaceOnlyString_WritesNothing(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()

	_, err := engine.Create(ctx, def, map[string]any{"name": "   ", "lead_time_days": float64(10)},
		audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("expected a required-field error for \"name\" submitted as \"   \", got: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records`).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no records written after validation failure, got %d", count)
	}
}

func TestEngine_Update_ChangesDataAndAppendsAudit(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	_, err = engine.Update(ctx, def, rec.ID, map[string]any{
		"name":           "Acme Textiles Ltd",
		"lead_time_days": float64(45),
	}, nil, actor)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["name"] != "Acme Textiles Ltd" {
		t.Fatalf("update did not persist: %+v", got.Data)
	}

	var auditCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM audit_log WHERE record_id = $1`,
		rec.ID,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != 2 { // one for create, one for update
		t.Fatalf("expected 2 audit rows (create+update), got %d", auditCount)
	}
}

// TestEngine_Create_StartsAtVersion1 pins the documented starting value
// (005_record_version.sql: "version starts at 1, not 0") — 0 is reserved
// to mean "never checked" in the pointer-based expectedVersion API, so a
// real record must never legitimately have version 0.
func TestEngine_Create_StartsAtVersion1(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if rec.Version != 1 {
		t.Fatalf("expected a freshly created record at version 1, got %d", rec.Version)
	}
}

// TestEngine_Update_NilExpectedVersionSkipsCheck confirms the backward-
// compatible path: a caller that never passes an expectedVersion (every
// caller written before optimistic locking existed) keeps updating
// unconditionally, exactly as before — the version field increments as a
// side effect, but nothing rejects the write.
func TestEngine_Update_NilExpectedVersionSkipsCheck(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Two consecutive unconditional updates, neither checking a version —
	// the second must not fail just because the first already moved the
	// record's version on from what it was at Create time.
	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "First Edit"}, nil, actor); err != nil {
		t.Fatalf("first unconditional Update failed: %v", err)
	}
	newVersion, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "Second Edit"}, nil, actor)
	if err != nil {
		t.Fatalf("second unconditional Update failed: %v", err)
	}
	if newVersion != 3 { // 1 at create, 2 after first edit, 3 after second
		t.Fatalf("expected version 3 after two edits from version 1, got %d", newVersion)
	}
}

// TestEngine_Update_StaleExpectedVersionRejected is optimistic locking's
// whole reason to exist: two "concurrent" edits of the same record — the
// second one's expectedVersion was captured before the first one saved,
// so it must be rejected with data.ErrVersionConflict instead of silently
// overwriting the first edit.
func TestEngine_Update_StaleExpectedVersionRejected(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	staleVersion := rec.Version // both "concurrent" edits read the record at this version

	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{"name": "Editor A's change"}, &staleVersion, actor); err != nil {
		t.Fatalf("first Update (the one that actually wins the race) failed: %v", err)
	}

	_, err = engine.Update(ctx, def, rec.ID, map[string]any{"name": "Editor B's change"}, &staleVersion, actor)
	if !errors.Is(err, data.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict for a stale expectedVersion, got %v", err)
	}

	// Editor A's change survived; Editor B's was correctly rejected, not
	// silently applied on top.
	got, err := engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Data["name"] != "Editor A's change" {
		t.Fatalf("expected Editor A's change to have won, got %v", got.Data["name"])
	}
}

// TestEngine_Update_NonexistentRecordReturnsNotFoundNotConflict confirms
// the two failure modes stay distinguishable — a version mismatch and a
// genuinely missing record must not both collapse into the same error,
// since a caller needs to tell "reload and retry" (409) apart from "this
// is gone" (404).
func TestEngine_Update_NonexistentRecordReturnsNotFoundNotConflict(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	v := 1
	_, err := engine.Update(ctx, def, "00000000-0000-0000-0000-000000000000", map[string]any{"name": "Ghost"}, &v, actor)
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a nonexistent record, got %v", err)
	}
}

func TestEngine_List_ScopesToOwnTenantDatabase(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	engineA := NewEngine(dbA)
	engineB := NewEngine(dbB)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engineA.Create(ctx, def, map[string]any{"name": "A-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := engineA.Create(ctx, def, map[string]any{"name": "A-Vendor-2"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := engineB.Create(ctx, def, map[string]any{"name": "B-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}

	listA, err := engineA.List(ctx, def)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("expected 2 records for tenant A, got %d", len(listA))
	}
	for _, r := range listA {
		if r.Data["name"] == "B-Vendor-1" {
			t.Fatal("tenant A's list leaked a record belonging to tenant B's own database")
		}
	}
}

func TestEngine_Count_ScopesToOwnTenantDatabase(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	engineA := NewEngine(dbA)
	engineB := NewEngine(dbB)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	for _, name := range []string{"A-Vendor-1", "A-Vendor-2", "A-Vendor-3"} {
		if _, err := engineA.Create(ctx, def, map[string]any{"name": name}, actor); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, err := engineB.Create(ctx, def, map[string]any{"name": "B-Vendor-1"}, actor); err != nil {
		t.Fatalf("create: %v", err)
	}

	count, err := engineA.Count(ctx, def)
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
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := vendorDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	const total = 5
	var created []string
	for i := range total {
		rec, err := engine.Create(ctx, def, map[string]any{"name": fmt.Sprintf("Vendor-%d", i)}, actor)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		created = append(created, rec.ID)
	}

	page1, err := engine.ListPage(ctx, def, 2, 0)
	if err != nil {
		t.Fatalf("ListPage page 1: %v", err)
	}
	page2, err := engine.ListPage(ctx, def, 2, 2)
	if err != nil {
		t.Fatalf("ListPage page 2: %v", err)
	}
	page3, err := engine.ListPage(ctx, def, 2, 4)
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
	emptyPage, err := engine.ListPage(ctx, def, 2, 10)
	if err != nil {
		t.Fatalf("ListPage past the end: %v", err)
	}
	if len(emptyPage) != 0 {
		t.Fatalf("expected no records past the end, got %d", len(emptyPage))
	}
}
