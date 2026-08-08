// Smoke tests for the real compiled cmd/backfill-poline-money binary,
// against real "legacy" POLine rows (pre-Version-4: unit_price and
// line_total both FieldNumber major-unit decimals) inserted directly via
// SQL — exactly the shape a row written before the Version 3->4 bump
// actually has (uc-infra#136).
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
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-poline-money")
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

func tenantDatabase(t *testing.T) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_plm_control")
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

	id, err := router.Create(ctx, "Backfill Smoke Test", "eu-west")
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

	var dbName string
	if err := control.QueryRowContext(ctx, `SELECT db_name FROM tenants WHERE id = $1`, id).Scan(&dbName); err != nil {
		t.Fatalf("look up tenant db_name: %v", err)
	}
	dsn = dsnWithDatabase(t, controlDSN, dbName)
	return dsn, tenantDB
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

// insertLegacyPOLine inserts a POLine row directly via SQL, bypassing
// entity.ValidateRecord entirely — the only way to reproduce a
// genuinely pre-Version-4 row. unitPrice/lineTotal are `any` so a test
// can also seed a non-numeric or absent value.
func insertLegacyPOLine(t *testing.T, tenantDB *sql.DB, unitPrice, lineTotal any) string {
	t.Helper()
	fields := map[string]any{
		"purchase_order_id": "00000000-0000-0000-0000-000000000001",
		"item_id":           "00000000-0000-0000-0000-000000000002",
		"qty":               float64(1),
	}
	if unitPrice != nil {
		fields["unit_price"] = unitPrice
	}
	if lineTotal != nil {
		fields["line_total"] = lineTotal
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal legacy POLine data: %v", err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('POLine', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy POLine: %v", err)
	}
	return id
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

// TestBackfill_StillVersion3_FailsCleanly confirms the guard against
// running this migration before POLine's Version 4 is actually
// published.
func TestBackfill_StillVersion3_FailsCleanly(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_plm_v3_control")
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
	id, err := router.Create(ctx, "Backfill V3 Smoke Test", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	testexec.DropTenantDatabase(t, control, id)
	tenantDB, err := router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}

	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	v3 := &entity.Definition{
		EntityType: "POLine",
		Version:    3,
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldReference, Required: true, Target: "PurchaseOrder"},
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty", Type: entity.FieldNumber, Required: true},
			{Name: "unit_price", Type: entity.FieldNumber, Required: true},
			{Name: "line_total", Type: entity.FieldNumber, Default: float64(0)},
		},
	}
	raw, err := json.Marshal(v3)
	if err != nil {
		t.Fatalf("marshal v3 definition: %v", err)
	}
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	if _, err := entityDefs.CreateDraft(ctx, v3.EntityType, v3.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft v3: %v", err)
	}
	if err := entityDefs.Approve(ctx, v3.EntityType, v3.Version, actor); err != nil {
		t.Fatalf("Approve v3: %v", err)
	}
	if err := entityDefs.Publish(ctx, v3.EntityType, v3.Version, actor); err != nil {
		t.Fatalf("Publish v3: %v", err)
	}

	var dbName string
	if err := control.QueryRowContext(ctx, `SELECT db_name FROM tenants WHERE id = $1`, id).Scan(&dbName); err != nil {
		t.Fatalf("look up tenant db_name: %v", err)
	}
	dsn := dsnWithDatabase(t, controlDSN, dbName)

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit against a tenant whose POLine is still published at Version 3")
	}
	if !strings.Contains(stderr, "still Version 3") {
		t.Fatalf("expected a clear Version guard error, got stderr: %q", stderr)
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

// TestBackfill_ActorTypeValidation was missing from this binary's test
// file even though every one of its 9 siblings has the equivalent case
// (independent review of uc-infra#167, finding 2) — closed as debt in
// the same change that centralized the validation logic itself into
// audit.ResolveCLIActor. DATABASE_URL points at an unreachable-but-
// parseable address, so a rejection reaching the actor-validation error
// (not a "open database" connection error) proves validation runs
// before any database work, the same way
// cmd/sync-tenant-modules's TestSync_ActorTypeValidation does.
func TestBackfill_ActorTypeValidation(t *testing.T) {
	const unreachableDSN = "postgres://postgres:postgres@127.0.0.1:1/nonexistent?sslmode=disable"

	_, stderr, code := run(t, []string{"DATABASE_URL=" + unreachableDSN}, "-actor-id=pipeline", "-actor-type=ai_agent")
	if code == 0 || !strings.Contains(stderr, "model_version") {
		t.Errorf("expected ai_agent to require -model-version, got code %d: %s", code, stderr)
	}

	_, stderr, code = run(t, []string{"DATABASE_URL=" + unreachableDSN}, "-actor-id=pipeline", "-actor-type=wizard")
	if code == 0 || !strings.Contains(stderr, "invalid actor") {
		t.Errorf("expected an unknown actor type to be rejected, got code %d: %s", code, stderr)
	}

	// The other half of the same falsification, the other direction —
	// a human row carrying a model version (uc-infra#72 independent
	// review, finding 4): Validate() alone only rejects an EMPTY
	// ModelVersion on an agent, never a populated one on a human.
	_, stderr, code = run(t, []string{"DATABASE_URL=" + unreachableDSN}, "-actor-id=pipeline", "-model-version=claude-x")
	if code == 0 || !strings.Contains(stderr, "-model-version is only meaningful") {
		t.Errorf("expected a human actor with -model-version set to be rejected, got code %d: %s", code, stderr)
	}
}

// TestBackfill_ActorTypeAI_WritesRealAuditRow proves a successful
// ai_agent run reaches the migrated record's own audit_log row with the
// right values — not just that the guardrail exists.
func TestBackfill_ActorTypeAI_WritesRealAuditRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	// Both fields genuinely fractional (unambiguous) — 95.0 alone would
	// be a whole number and trip the all-or-nothing ambiguity guard
	// TestBackfill_OneFieldAmbiguous_SkipsWholeRow exercises deliberately.
	id := insertLegacyPOLine(t, tenantDB, 9.5, 95.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-actor-type=ai_agent", "-model-version=claude-test-1")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 POLine") {
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
	if !inputHash.Valid || inputHash.String == "" {
		t.Errorf("expected a non-null input_hash for the ai_agent actor, got %+v", inputHash)
	}
}

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 9.5, 95.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 POLine") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != 9.5 || got["line_total"] != 95.5 {
		t.Fatalf("dry run must not write anything, got %v", got)
	}
}

// TestBackfill_BothFractional_ConvertsBoth is the clean happy path: both
// fields have a genuine fractional component, so both convert.
func TestBackfill_BothFractional_ConvertsBoth(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 9.5, 95.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 POLine record(s); 0 already had nothing to convert") || !strings.Contains(stdout, "0 skipped") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != float64(950) {
		t.Errorf("expected unit_price converted to 950 minor units, got %v", got["unit_price"])
	}
	if got["line_total"] != float64(9550) {
		t.Errorf("expected line_total converted to 9550 minor units, got %v", got["line_total"])
	}
	_ = stderr
}

// TestBackfill_OneFieldAmbiguous_SkipsWholeRow is the core proof of the
// all-or-nothing row policy: unit_price is a genuine fraction but
// line_total happens to be a whole number — the whole row is skipped
// (neither field written), not partially converted.
func TestBackfill_OneFieldAmbiguous_SkipsWholeRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 9.5, 95.0) // unit_price fractional, line_total whole

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 0 POLine record(s)") || !strings.Contains(stdout, "1 skipped for manual review") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, "line_total 95 is already a whole number") {
		t.Fatalf("expected the skip reason to name line_total as the ambiguous field, got stderr: %q", stderr)
	}
	got := recordFields(t, tenantDB, id)
	// Neither field converted — unit_price must NOT have been migrated
	// even though it alone was unambiguous, since line_total blocked the
	// whole row.
	if got["unit_price"] != 9.5 || got["line_total"] != 95.0 {
		t.Fatalf("expected BOTH fields left untouched when either is ambiguous, got %v", got)
	}
}

func TestBackfill_NonNumericField_SkipsRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, "not-a-number", 95.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 0 POLine record(s)") || !strings.Contains(stdout, "1 skipped for manual review") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, "unit_price is not a number") {
		t.Fatalf("expected a warning about the non-numeric unit_price, got stderr: %q", stderr)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != "not-a-number" || got["line_total"] != 95.5 {
		t.Fatalf("expected both fields left untouched, got %v", got)
	}
}

// TestBackfill_IncludeWholeNumbers_ConvertsThemToo proves the explicit
// opt-in flag converts both fields even when both are whole numbers.
func TestBackfill_IncludeWholeNumbers_ConvertsThemToo(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 10.0, 100.0)

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-include-whole-numbers")
	if code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 1 POLine record(s); 0 already had nothing to convert") || !strings.Contains(stdout, "0 skipped") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != float64(1000) || got["line_total"] != float64(10000) {
		t.Fatalf("expected both whole-number fields converted with -include-whole-numbers, got %v", got)
	}
}

// TestBackfill_AbsentSiblingField_DoesNotBlockConversion (independent
// review of uc-infra#136's first pass) is the corrected behavior for a
// row missing line_total entirely — an ORDINARY, reachable shape
// (line_total is optional; this repo's own e2e fixtures create exactly
// this), not a rare edge case. The first draft treated an absent field
// as ambiguous and let it block the WHOLE row, including its
// unambiguous unit_price sibling, with no flag able to fix it (a
// missing value is never "included" by -include-whole-numbers). Absent
// must convert its sibling and simply leave itself alone.
func TestBackfill_AbsentSiblingField_DoesNotBlockConversion(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 9.5, nil)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 POLine record(s)") {
		t.Fatalf("expected the row to migrate despite the absent sibling field, got: %q", stdout)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != float64(950) {
		t.Fatalf("expected unit_price converted to 950 minor units despite the absent line_total, got %v", got["unit_price"])
	}
	if _, present := got["line_total"]; present {
		t.Fatalf("expected line_total to stay absent (nothing to convert), got %v", got["line_total"])
	}
	_ = stderr
}

// TestBackfill_ZeroValue_NotAmbiguous proves a stored 0 is never treated
// as ambiguous — 0 major units and 0 minor units are the identical
// value, so it never needs -include-whole-numbers and never blocks a
// sibling field.
func TestBackfill_ZeroValue_NotAmbiguous(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyPOLine(t, tenantDB, 9.5, 0.0)

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 1 POLine record(s)") {
		t.Fatalf("expected the row to migrate — a stored 0 must not be treated as ambiguous, got: %q", stdout)
	}
	got := recordFields(t, tenantDB, id)
	if got["unit_price"] != float64(950) {
		t.Fatalf("expected unit_price converted to 950 minor units, got %v", got["unit_price"])
	}
	if got["line_total"] != 0.0 {
		t.Fatalf("expected line_total to stay 0, got %v", got["line_total"])
	}
}

// TestBackfill_BothAbsentOrZero_ReportsAlreadyDone covers a row with
// nothing to convert on either field at all.
func TestBackfill_BothAbsentOrZero_ReportsAlreadyDone(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	insertLegacyPOLine(t, tenantDB, 0.0, nil)

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 0 POLine record(s); 1 already had nothing to convert") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
}
