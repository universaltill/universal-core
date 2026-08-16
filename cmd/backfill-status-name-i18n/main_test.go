// Smoke tests for the real compiled cmd/backfill-status-name-i18n
// binary, against real "legacy" Status rows (pre-Version-2: "name" a
// plain string) inserted directly via SQL — exactly the shape a row
// written before the Version 1->2 bump actually has (uc-infra#214,
// ADR-0030). Raw SQL is the only way to produce them: v2 makes "name"
// i18n_text, so crud.Engine.Create would reject the very rows under
// test.
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
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-status-name-i18n")
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

// tenantDatabase creates a fresh tenant database with foundation
// published at its current Go-literal Version — i.e. Status already at
// v2 (FieldI18nText "name"). Any "legacy" rows a test needs are
// inserted separately, directly via SQL (insertLegacyStatus below).
func tenantDatabase(t *testing.T) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_status_i18n_control")
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

	id, err := router.Create(ctx, "Backfill Status i18n Smoke Test", "eu-west")
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

	var dbName string
	if err := control.QueryRowContext(ctx, `SELECT db_name FROM tenants WHERE id = $1`, id).Scan(&dbName); err != nil {
		t.Fatalf("look up tenant db_name: %v", err)
	}
	dsn = dsnWithDatabase(t, controlDSN, dbName)
	return dsn, tenantDB
}

// insertLegacyStatus inserts a Status row directly via SQL, bypassing
// entity.ValidateRecord entirely — the only way to reproduce a
// genuinely pre-Version-2 row (a real one would have failed
// FieldI18nText validation on any post-bump write). A fake but
// well-formed status_type_id is fine: entity.FieldReference validates
// only "is a string," and nothing in this migration's Transform
// dereferences it.
func insertLegacyStatus(t *testing.T, tenantDB *sql.DB, name any) string {
	t.Helper()
	fields := map[string]any{
		"status_type_id": "00000000-0000-0000-0000-000000000001",
		"code":           "draft",
		"name":           name,
		"sequence":       float64(1),
		"is_initial":     true,
		"is_terminal":    false,
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal legacy status data: %v", err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('Status', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy status: %v", err)
	}
	return id
}

func recordStatusName(t *testing.T, tenantDB *sql.DB, id string) any {
	t.Helper()
	return recordFields(t, tenantDB, id)["name"]
}

// recordFields is recordStatusName's whole-row counterpart, for
// confirming this command's full-blob-replacement Update (CopyExcept
// "name", then re-add the converted value) doesn't drop or corrupt any
// OTHER field on the row — the classic "backfill nuked the rest of the
// record" failure mode a name-only assertion can't catch.
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

// TestBackfill_StillVersion1_FailsCleanly confirms the guard against
// running this migration before foundation.Status's Version 2 is
// actually published — a hand-published v1-only Definition (the
// pre-bump shape: "name" still declared FieldString), never touched by
// foundation.Publish.
func TestBackfill_StillVersion1_FailsCleanly(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_status_i18n_v1_control")
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
	id, err := router.Create(ctx, "Backfill Status i18n V1 Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, control, id)
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	v1 := &entity.Definition{
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
	}
	raw, err := json.Marshal(v1)
	if err != nil {
		t.Fatalf("marshal v1 definition: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	if _, err := entityDefs.CreateDraft(ctx, v1.EntityType, v1.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft v1: %v", err)
	}
	if err := entityDefs.Approve(ctx, v1.EntityType, v1.Version, actor); err != nil {
		t.Fatalf("Approve v1: %v", err)
	}
	if err := entityDefs.Publish(ctx, v1.EntityType, v1.Version, actor); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}

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

// TestBackfill_ActorTypeAI_WritesRealAuditRow proves a successful
// ai_agent run reaches the migrated record's own audit_log row with the
// right values — not just that the guardrail exists.
func TestBackfill_ActorTypeAI_WritesRealAuditRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyStatus(t, tenantDB, "Draft")

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
		`SELECT actor_type, actor_id, model_version, input_hash FROM audit_log WHERE record_id = $1 ORDER BY id DESC LIMIT 1`, id,
	).Scan(&actorType, &actorID, &modelVersion, &inputHash); err != nil {
		t.Fatalf("read audit_log row for %s: %v", id, err)
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

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyStatus(t, tenantDB, "Draft")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 Status record(s)") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}
	if !strings.Contains(stderr, `name "Draft" -> {"en": "Draft"}`) {
		t.Fatalf("expected a dry-run preview naming the conversion, got stderr: %q", stderr)
	}
	if got := recordStatusName(t, tenantDB, id); got != "Draft" {
		t.Fatalf("dry run must not write anything, got name %v", got)
	}
}

// TestBackfill_ConvertsLegacyStrings_SkipsUnexpectedShapes is the core
// proof: a legacy plain-string "name" is converted to {"en": <value>};
// an already-migrated i18n_text row is left alone (AlreadyDone, not
// re-written); a row with neither shape is skipped for manual review
// rather than guessed at.
func TestBackfill_ConvertsLegacyStrings_SkipsUnexpectedShapes(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	legacyID := insertLegacyStatus(t, tenantDB, "Draft")
	alreadyMigratedID := insertLegacyStatus(t, tenantDB, map[string]any{"en": "Approved", "tr": "Onaylandı"})
	weirdID := insertLegacyStatus(t, tenantDB, float64(1))

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 Status record(s); 1 already had i18n_text name; 1 skipped for manual review") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, "unexpected type float64") {
		t.Fatalf("expected a warning naming the unexpected type, got stderr: %q", stderr)
	}

	if got := recordStatusName(t, tenantDB, legacyID); !equalI18n(got, map[string]any{"en": "Draft"}) {
		t.Fatalf("expected the legacy row converted to {\"en\": \"Draft\"}, got %#v", got)
	}
	// The rest of the blob must survive the full-replacement Update
	// untouched — this is what would catch a CopyExcept mistake that
	// silently dropped or corrupted a sibling field while converting
	// "name", the "backfill nuked the other fields" failure class.
	if got := recordFields(t, tenantDB, legacyID); got["code"] != "draft" ||
		got["status_type_id"] != "00000000-0000-0000-0000-000000000001" ||
		got["sequence"] != float64(1) || got["is_initial"] != true || got["is_terminal"] != false {
		t.Fatalf("expected every OTHER field on the migrated row to survive intact, got %#v", got)
	}
	if got := recordStatusName(t, tenantDB, alreadyMigratedID); !equalI18n(got, map[string]any{"en": "Approved", "tr": "Onaylandı"}) {
		t.Fatalf("expected the already-migrated row left UNTOUCHED, got %#v", got)
	}
	if got := recordStatusName(t, tenantDB, weirdID); got != float64(1) {
		t.Fatalf("expected the unexpected-shape row left untouched, got %#v", got)
	}

	// Idempotent: re-running must not touch the now-migrated row again.
	stdout, _, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 0 Status record(s); 2 already had i18n_text name; 1 skipped for manual review") {
		t.Fatalf("expected a re-run to convert nothing further, got: %q", stdout)
	}
}

func equalI18n(got any, want map[string]any) bool {
	m, ok := got.(map[string]any)
	if !ok || len(m) != len(want) {
		return false
	}
	for k, v := range want {
		if m[k] != v {
			return false
		}
	}
	return true
}
