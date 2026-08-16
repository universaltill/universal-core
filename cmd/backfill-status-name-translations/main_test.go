// Smoke tests for the real compiled cmd/backfill-status-name-translations
// binary, against real tenant Status rows produced by the actual
// production seed path (purchasing.PublishStatuses -> statusgraph.Seed)
// and then rolled back to a partially/un-translated shape to simulate an
// already-provisioned tenant seeded before uc-infra#244 shipped real
// content — the exact gap this command exists to close.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-status-name-translations")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func run(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return testexec.Run(t, binPath, env, args...)
}

func dsnWithDatabase(t *testing.T, base, dbName string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// tenantDatabase creates a fresh tenant database with foundation and
// purchasing published (Status already i18n_text, purchase_order_status
// seeded with its real Translations content) — the state a real
// freshly-provisioned tenant is in today. Tests that want to simulate an
// OLDER, already-provisioned tenant roll individual rows back afterward
// (rollbackToEnOnly / rollbackToEnPlusAr below).
func tenantDatabase(t *testing.T) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_status_translations_control")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	id, err := router.Create(ctx, "Backfill Status Translations Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, control, id)
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	var dbName string
	if err := control.QueryRowContext(ctx, `SELECT db_name FROM tenants WHERE id = $1`, id).Scan(&dbName); err != nil {
		t.Fatalf("look up tenant db_name: %v", err)
	}
	dsn = dsnWithDatabase(t, controlDSN, dbName)
	return dsn, tenantDB
}

// purchaseOrderDraftID finds the real, production-seeded "draft" Status
// row under purchase_order_status.
func purchaseOrderDraftID(t *testing.T, tenantDB *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	v, err := entityDefs.GetPublished(ctx, "Status")
	if err != nil {
		t.Fatalf("GetPublished(Status): %v", err)
	}
	statusDef, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Status: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	recs, err := engine.ListByField(ctx, statusDef, "code", "draft")
	if err != nil {
		t.Fatalf("list Status by code=draft: %v", err)
	}
	for _, r := range recs {
		// purchase_order_status is the only StatusType this tenant has
		// (only purchasing was published), so any "draft" row found is
		// the right one — still confirmed via its own name content below
		// by the caller, not assumed blindly here.
		return r.ID
	}
	t.Fatalf("no draft Status row found")
	return ""
}

// rollbackName overwrites a Status row's "name" directly via SQL,
// simulating an already-provisioned tenant whose row predates real
// Translations content (uc-infra#244) — the exact shape this command
// exists to repair. Bypasses crud.Engine entirely so no audit_log row
// or Version bump from this setup step could be confused with the
// command's own.
func rollbackName(t *testing.T, tenantDB *sql.DB, id string, name map[string]any) {
	t.Helper()
	raw, err := json.Marshal(name)
	if err != nil {
		t.Fatalf("marshal rollback name: %v", err)
	}
	res, err := tenantDB.ExecContext(context.Background(),
		`UPDATE records SET data = jsonb_set(data, '{name}', $1::jsonb) WHERE id = $2`, raw, id)
	if err != nil {
		t.Fatalf("rollback name for %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expected to roll back exactly 1 row, affected %d", n)
	}
}

func recordName(t *testing.T, tenantDB *sql.DB, id string) map[string]any {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT data FROM records WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal record %s data: %v", id, err)
	}
	name, ok := fields["name"].(map[string]any)
	if !ok {
		t.Fatalf("expected record %s \"name\" to be an object, got %#v", id, fields["name"])
	}
	return name
}

func recordFields(t *testing.T, tenantDB *sql.DB, id string) map[string]any {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT data FROM records WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal record %s data: %v", id, err)
	}
	return fields
}

func TestBackfill_MissingDatabaseURL_FailsFast(t *testing.T) {
	_, stderr, code := run(t, []string{}, "-actor-id=a")
	if code == 0 {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(stderr, "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got stderr: %q", stderr)
	}
}

func TestBackfill_MissingActorID_FailsFast(t *testing.T) {
	dsn, _ := tenantDatabase(t)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn})
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

func TestBackfill_ActorTypeValidation(t *testing.T) {
	dsn, _ := tenantDatabase(t)

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-actor-type=ai_agent")
	if code == 0 || !strings.Contains(stderr, "model_version") {
		t.Errorf("expected ai_agent to require -model-version, got code %d: %s", code, stderr)
	}

	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-actor-type=wizard")
	if code == 0 || !strings.Contains(stderr, "invalid actor") {
		t.Errorf("expected an unknown actor type to be rejected, got code %d: %s", code, stderr)
	}
}

// TestBackfill_AddsMissingTranslations_NeverOverwritesExisting is the
// core proof: a row rolled back to English-only gets every missing
// locale added, matching purchasing.StatusSpecs' own content exactly; a
// row that already carries one non-English locale keeps that value
// UNTOUCHED even though it deliberately disagrees with StatusSpecs
// (simulating a manual correction this tool must not clobber), while
// still gaining the locales it's missing.
func TestBackfill_AddsMissingTranslations_NeverOverwritesExisting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	draftID := purchaseOrderDraftID(t, tenantDB)

	// Confirm the real production seed already wrote full content, then
	// roll it back to simulate a pre-#244 tenant.
	full := recordName(t, tenantDB, draftID)
	if full["ar"] != "مسودة" || full["fa"] != "پیش‌نویس" || full["tr"] != "Taslak" {
		t.Fatalf("expected purchasing.PublishStatuses to have seeded real translations, got %#v", full)
	}
	rollbackName(t, tenantDB, draftID, map[string]any{"en": "Draft", "ar": "PLACEHOLDER-DO-NOT-OVERWRITE"})

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 1 Status record(s)") {
		t.Fatalf("expected exactly 1 record migrated, got stdout: %q", stdout)
	}

	got := recordName(t, tenantDB, draftID)
	if got["en"] != "Draft" {
		t.Errorf("expected \"en\" untouched, got %#v", got["en"])
	}
	if got["ar"] != "PLACEHOLDER-DO-NOT-OVERWRITE" {
		t.Errorf("expected the pre-existing \"ar\" value left UNTOUCHED (not overwritten with StatusSpecs' own \"مسودة\"), got %#v", got["ar"])
	}
	if got["fa"] != "پیش‌نویس" {
		t.Errorf("expected the missing \"fa\" locale added from StatusSpecs, got %#v", got["fa"])
	}
	if got["tr"] != "Taslak" {
		t.Errorf("expected the missing \"tr\" locale added from StatusSpecs, got %#v", got["tr"])
	}

	// Every other field on the row must survive the full-replacement
	// Update untouched.
	fields := recordFields(t, tenantDB, draftID)
	if fields["code"] != "draft" || fields["is_initial"] != true || fields["is_terminal"] != false {
		t.Fatalf("expected every OTHER field on the migrated row to survive intact, got %#v", fields)
	}

	// Idempotent: re-running must not touch the now-complete row again.
	stdout, _, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 0 Status record(s)") {
		t.Fatalf("expected a re-run to migrate nothing further, got: %q", stdout)
	}
}

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	draftID := purchaseOrderDraftID(t, tenantDB)
	rollbackName(t, tenantDB, draftID, map[string]any{"en": "Draft"})

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 Status record(s)") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "name +map[") {
		t.Fatalf("expected a dry-run preview naming the added locales, got stderr: %q", stderr)
	}
	got := recordName(t, tenantDB, draftID)
	if len(got) != 1 || got["en"] != "Draft" {
		t.Fatalf("dry run must not write anything, got name %#v", got)
	}
}

// TestBackfill_StillPlainStringName_SkippedForManualReview proves this
// command refuses to guess at a row that predates even the i18n_text
// shape bump (uc-infra#214) — that is cmd/backfill-status-name-i18n's
// job, run first, not this one's.
func TestBackfill_StillPlainStringName_SkippedForManualReview(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)

	fields := map[string]any{
		"status_type_id": "00000000-0000-0000-0000-000000000001",
		"code":           "draft",
		"name":           "Draft", // still a bare string, pre-#214 shape
		"sequence":       float64(1),
		"is_initial":     true,
		"is_terminal":    false,
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal legacy status data: %v", err)
	}
	var legacyID string
	if err := tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('Status', $1) RETURNING id`, raw,
	).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy status: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "not yet an i18n_text object — run cmd/backfill-status-name-i18n first") {
		t.Fatalf("expected a warning pointing at the sibling tool, got stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "1 skipped for manual review") {
		t.Fatalf("expected the legacy row counted as skipped, got stdout: %q", stdout)
	}
	if got, ok := recordFields(t, tenantDB, legacyID)["name"].(string); !ok || got != "Draft" {
		t.Fatalf("expected the legacy row left completely untouched, got %#v", recordFields(t, tenantDB, legacyID)["name"])
	}
}

// TestBackfill_StillVersion1_FailsCleanly mirrors cmd/backfill-status-
// name-i18n's own TestBackfill_StillVersion1_FailsCleanly: this tool
// must refuse cleanly, with the same clear guard message, against a
// tenant whose published Status Definition is still Version 1 (plain
// FieldString "name") rather than guessing at rows this tool has no
// business touching yet.
func TestBackfill_StillVersion1_FailsCleanly(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_status_translations_v1_control")
	control := testexec.Open(t, controlDSN)
	ctx := context.Background()
	if err := db.ApplyControl(ctx, control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, controlDSN)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	id, err := router.Create(ctx, "Backfill Status Translations V1 Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, control, id)
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	publish := func(def *entity.Definition) {
		t.Helper()
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("marshal %s definition: %v", def.EntityType, err)
		}
		if _, err := entityDefs.CreateDraft(ctx, def.EntityType, def.Version, raw, actor); err != nil {
			t.Fatalf("CreateDraft %s: %v", def.EntityType, err)
		}
		if err := entityDefs.Approve(ctx, def.EntityType, def.Version, actor); err != nil {
			t.Fatalf("Approve %s: %v", def.EntityType, err)
		}
		if err := entityDefs.Publish(ctx, def.EntityType, def.Version, actor); err != nil {
			t.Fatalf("Publish %s: %v", def.EntityType, err)
		}
	}

	// This tool looks up StatusType before it ever gets to the Status
	// version check under test — publish a minimal real one first (its
	// own fields don't matter here, execution never reaches anything
	// that reads them) so that lookup succeeds and the fatal this test
	// actually cares about is the Status one.
	publish(foundation.StatusType())
	publish(&entity.Definition{
		EntityType: "Status",
		Version:    1,
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "sequence", Type: entity.FieldNumber, Default: float64(0)},
			{Name: "is_initial", Type: entity.FieldBool, Default: false},
			{Name: "is_terminal", Type: entity.FieldBool, Default: false},
		},
	})

	var dbName string
	if err := control.QueryRowContext(ctx, `SELECT db_name FROM tenants WHERE id = $1`, id).Scan(&dbName); err != nil {
		t.Fatalf("look up tenant db_name: %v", err)
	}
	dsn := dsnWithDatabase(t, controlDSN, dbName)

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit against a tenant whose Status is still published at Version 1")
	}
	if !strings.Contains(stderr, "is not i18n_text yet") {
		t.Fatalf("expected a clear field-type guard error, got stderr: %q", stderr)
	}
}

// TestBackfill_KnownStatusTypeUnknownCode_SkippedForManualReview proves
// the THIRD Skip branch — a StatusType this tool DOES know about (so not
// the "out of scope, no action needed" case), but a Status row whose own
// "code" isn't in that StatusType's StatusSpecs — real StatusSpecs
// drift, not a case to wave through, so it must be flagged as genuinely
// worth investigating rather than the reassuring wording the unknown-
// StatusType case gets.
func TestBackfill_KnownStatusTypeUnknownCode_SkippedForManualReview(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
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
	statusDef := def("Status")
	engine := crud.NewEngine(tenantDB)

	// purchase_order_status IS a known StatusType (purchasing was
	// published by tenantDatabase), but "mystery_code" is not one of its
	// declared Specs.
	poDraftID := purchaseOrderDraftID(t, tenantDB)
	statusTypeID := recordFields(t, tenantDB, poDraftID)["status_type_id"]
	rec, err := engine.Create(ctx, statusDef, map[string]any{
		"status_type_id": statusTypeID, "code": "mystery_code", "name": map[string]any{"en": "Mystery"},
		"sequence": float64(99), "is_initial": false, "is_terminal": false,
	}, actor)
	if err != nil {
		t.Fatalf("create Status with unknown code: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, `code "mystery_code" has no known Spec for its (known) StatusType — possible StatusSpecs drift, worth investigating`) {
		t.Fatalf("expected a warning distinguishing this from the out-of-scope case, got stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "1 skipped for manual review") {
		t.Fatalf("expected the unknown-code row counted as skipped, got stdout: %q", stdout)
	}
	got := recordFields(t, tenantDB, rec.ID)["name"]
	if name, ok := got.(map[string]any); !ok || len(name) != 1 || name["en"] != "Mystery" {
		t.Fatalf("expected the unknown-code row left completely untouched, got %#v", got)
	}
}

// TestBackfill_ActorTypeAI_WritesRealAuditRow mirrors cmd/backfill-
// status-name-i18n's own identically-named test: a successful ai_agent
// run must reach the migrated record's own audit_log row with the right
// values (CLAUDE.md's AI-actor-identity-is-first-class rule), not just
// pass its own flag-validation guardrail.
func TestBackfill_ActorTypeAI_WritesRealAuditRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	draftID := purchaseOrderDraftID(t, tenantDB)
	rollbackName(t, tenantDB, draftID, map[string]any{"en": "Draft"})

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-actor-type=ai_agent", "-model-version=claude-test-1")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 Status record(s)") {
		t.Fatalf("expected the migration to actually run, got stdout: %q", stdout)
	}

	var actorType, actorID string
	var modelVersion, inputHash sql.NullString
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT actor_type, actor_id, model_version, input_hash FROM audit_log WHERE record_id = $1 ORDER BY id DESC LIMIT 1`, draftID,
	).Scan(&actorType, &actorID, &modelVersion, &inputHash); err != nil {
		t.Fatalf("read audit_log row for %s: %v", draftID, err)
	}
	if actorType != "ai_agent" || actorID != "pipeline" {
		t.Errorf("expected actor_type=ai_agent, actor_id=pipeline, got actor_type=%q, actor_id=%q", actorType, actorID)
	}
	if !modelVersion.Valid || modelVersion.String != "claude-test-1" {
		t.Errorf("expected model_version=claude-test-1, got %+v", modelVersion)
	}
	// uc-infra#124: an ai_agent actor's input_hash must be populated too,
	// not just model_version.
	if !inputHash.Valid || inputHash.String == "" {
		t.Errorf("expected a non-null input_hash for the ai_agent actor, got %+v", inputHash)
	}
}

// TestBackfill_UnknownStatusType_LeftUntouched proves a Status row under
// a StatusType none of the six modules' StatusSpecs() know about (a
// test-only fixture, or a future module this binary predates) is left
// alone rather than erroring the whole run out.
func TestBackfill_UnknownStatusType_LeftUntouched(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
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
	statusTypeDef, statusDef := def("StatusType"), def("Status")
	engine := crud.NewEngine(tenantDB)

	st, err := engine.Create(ctx, statusTypeDef, map[string]any{
		"entity_type": "WidgetX", "code": "widget_x_status", "name": "Widget X Status",
	}, actor)
	if err != nil {
		t.Fatalf("create unknown StatusType: %v", err)
	}
	rec, err := engine.Create(ctx, statusDef, map[string]any{
		"status_type_id": st.ID, "code": "draft", "name": map[string]any{"en": "Draft"},
		"sequence": float64(1), "is_initial": true, "is_terminal": false,
	}, actor)
	if err != nil {
		t.Fatalf("create Status under unknown StatusType: %v", err)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no known StatusSpecs for StatusType") {
		t.Fatalf("expected a log line naming the unknown StatusType, got stderr: %q", stderr)
	}
	// The known purchase_order_status rows (already fully seeded by
	// tenantDatabase's own real purchasing.PublishStatuses call) are
	// still processed normally in the same run, not aborted by the one
	// unknown StatusType — reported AlreadyDone since real
	// purchasing.PublishStatuses already wrote full content, with
	// exactly the one unknown-StatusType row skipped.
	if !strings.Contains(stdout, "Migrated 0 Status record(s)") || !strings.Contains(stdout, "1 skipped for manual review") {
		t.Fatalf("expected the known rows processed as AlreadyDone (not aborted) and exactly the unknown row skipped, got stdout: %q", stdout)
	}
	got := recordFields(t, tenantDB, rec.ID)["name"]
	if name, ok := got.(map[string]any); !ok || len(name) != 1 || name["en"] != "Draft" {
		t.Fatalf("expected the unknown-StatusType row left completely untouched, got %#v", got)
	}
}
