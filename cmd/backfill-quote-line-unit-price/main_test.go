// Smoke tests for the real compiled cmd/backfill-quote-line-unit-price
// binary, against real "legacy" RequestForQuotationQuoteLine rows
// (pre-Version-2: unit_price a FieldNumber major-unit decimal) inserted
// directly via SQL — exactly the shape a row written before the
// Version 1->2 bump actually has (uc-infra#68).
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
	path, cleanup, err := testexec.Build(".", "backfill-quote-line-unit-price")
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

// tenantDatabase creates a fresh tenant database with foundation and
// purchasing (entity Definitions, including RequestForQuotationQuoteLine
// at its current Go-literal Version) published — mirrors
// cmd/backfill-purchase-order-status's own helper.
func tenantDatabase(t *testing.T) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_qlup_control")
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

// insertLegacyQuoteLine inserts a RequestForQuotationQuoteLine row
// directly via SQL, bypassing entity.ValidateRecord entirely — the
// only way to reproduce a genuinely pre-Version-2 row (a real one would
// have failed FieldMoney validation on any post-bump write). Fake but
// well-formed rfq_line_id/vendor_id are fine: entity.FieldReference
// validates only "is a string," and nothing in this migration's
// Transform dereferences either.
func insertLegacyQuoteLine(t *testing.T, tenantDB *sql.DB, unitPrice any) string {
	t.Helper()
	data := map[string]any{
		"rfq_line_id": "00000000-0000-0000-0000-000000000001",
		"vendor_id":   "00000000-0000-0000-0000-000000000002",
		"unit_price":  unitPrice,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal legacy quote line data: %v", err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('RequestForQuotationQuoteLine', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy quote line: %v", err)
	}
	return id
}

func recordUnitPrice(t *testing.T, tenantDB *sql.DB, id string) any {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT data FROM records WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal record %s data: %v", id, err)
	}
	return data["unit_price"]
}

// TestBackfill_StillVersion1_FailsCleanly confirms the guard against
// running this migration before RequestForQuotationQuoteLine's Version 2
// is actually published — a hand-published v1-only Definition (the
// pre-bump shape: unit_price still declared FieldNumber), never touched
// by purchasing.Publish.
func TestBackfill_StillVersion1_FailsCleanly(t *testing.T) {
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_qlup_v1_control")
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
	id, err := router.Create(ctx, "Backfill V1 Smoke Test", "eu-west")
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
		EntityType: "RequestForQuotationQuoteLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "rfq_line_id", Type: entity.FieldReference, Required: true, Target: "RequestForQuotationLine"},
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "unit_price", Type: entity.FieldNumber, Required: true},
			{Name: "quoted_at", Type: entity.FieldDate},
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
		t.Fatal("expected non-zero exit against a tenant whose RequestForQuotationQuoteLine is still published at Version 1")
	}
	if !strings.Contains(stderr, "still Version 1") {
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

	_, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-model-version=claude-x")
	if code == 0 || !strings.Contains(stderr, "-model-version is only meaningful") {
		t.Errorf("expected a human actor with -model-version set to be rejected, got code %d: %s", code, stderr)
	}
}

// TestBackfill_ActorTypeAI_WritesRealAuditRow proves a successful
// ai_agent run reaches the migrated record's own audit_log row with the
// right values — not just that the guardrail exists.
func TestBackfill_ActorTypeAI_WritesRealAuditRow(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	id := insertLegacyQuoteLine(t, tenantDB, 9.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=pipeline", "-actor-type=ai_agent", "-model-version=claude-test-1")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 RequestForQuotationQuoteLine") {
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
	id := insertLegacyQuoteLine(t, tenantDB, 9.5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 RequestForQuotationQuoteLine") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}
	if got := recordUnitPrice(t, tenantDB, id); got != 9.5 {
		t.Fatalf("dry run must not write anything, got unit_price %v", got)
	}
}

// TestBackfill_ConvertsFractionalValues_SkipsWholeNumbersByDefault is
// the core proof: a genuinely fractional legacy value (unambiguous) is
// converted; a whole-number value is left alone by default because it
// cannot be told apart from an already-migrated minor-units amount.
func TestBackfill_ConvertsFractionalValues_SkipsWholeNumbersByDefault(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	fractionalID := insertLegacyQuoteLine(t, tenantDB, 9.5)
	wholeID := insertLegacyQuoteLine(t, tenantDB, 4.0)
	nonNumericID := insertLegacyQuoteLine(t, tenantDB, "not-a-number")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 RequestForQuotationQuoteLine record(s); 2 skipped for manual review") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, "ambiguous whether this is an already-migrated minor-units amount") {
		t.Fatalf("expected a warning explaining the whole-number ambiguity, got stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "unit_price is not a number") {
		t.Fatalf("expected a warning about the non-numeric value, got stderr: %q", stderr)
	}

	if got := recordUnitPrice(t, tenantDB, fractionalID); got != float64(950) {
		t.Fatalf("expected the fractional row converted to 950 minor units, got %v", got)
	}
	if got := recordUnitPrice(t, tenantDB, wholeID); got != float64(4) {
		t.Fatalf("expected the whole-number row left UNTOUCHED by default, got %v", got)
	}
	if got := recordUnitPrice(t, tenantDB, nonNumericID); got != "not-a-number" {
		t.Fatalf("expected the non-numeric row left untouched, got %v", got)
	}

	// Idempotent: re-running must not re-convert the now-integer
	// fractional row a second time (950 has no fractional component
	// left, so it's treated the same as any other whole number — left
	// alone by default).
	stdout, _, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 0 RequestForQuotationQuoteLine record(s); 3 skipped for manual review") {
		t.Fatalf("expected a re-run to convert nothing further, got: %q", stdout)
	}
	if got := recordUnitPrice(t, tenantDB, fractionalID); got != float64(950) {
		t.Fatalf("expected the already-migrated row to stay at 950 after a re-run, got %v", got)
	}
}

// TestBackfill_IncludeWholeNumbers_ConvertsThemToo proves the explicit
// opt-in flag does what its own doc comment promises — for an operator
// who has confirmed it's safe to run.
func TestBackfill_IncludeWholeNumbers_ConvertsThemToo(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t)
	wholeID := insertLegacyQuoteLine(t, tenantDB, 4.0)

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-include-whole-numbers")
	if code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if !strings.Contains(stdout, "Migrated 1 RequestForQuotationQuoteLine record(s); 0 skipped") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if got := recordUnitPrice(t, tenantDB, wholeID); got != float64(400) {
		t.Fatalf("expected the whole-number row converted to 400 minor units with -include-whole-numbers, got %v", got)
	}
}
