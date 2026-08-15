// Smoke tests for the real compiled
// cmd/backfill-fixedasset-posted-period-count binary, against a real
// tenant database with Assets published and a FixedAsset whose
// DepreciationSchedule has a genuine mix of posted/unposted rows —
// exactly the shape a pre-Version-4 (pre-uc-infra#202) FixedAsset row,
// never backfilled, actually has.
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
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-fixedasset-posted-period-count")
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

// tenantDatabase creates a fresh tenant database (real schema via
// tenantdb.Router.Create) with foundation always published, and Assets'
// entity Definition + StatusType/Status graph published when
// withAssets is true — TestBackfill_NoAssetsPublished_FailsCleanly
// deliberately needs it not to have run. Returns the tenant's own DSN
// (what a real operator would set DATABASE_URL to per this binary's own
// doc comment) and an open connection to it.
func tenantDatabase(t *testing.T, withAssets bool) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_fapc_control")
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

	id, err := router.Create(ctx, "Backfill FAPC Smoke Test", "eu-west")
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
	if withAssets {
		if err := assets.Publish(ctx, tenantDB, actor); err != nil {
			t.Fatalf("assets.Publish: %v", err)
		}
		if err := assets.PublishStatuses(ctx, tenantDB, actor); err != nil {
			t.Fatalf("assets.PublishStatuses: %v", err)
		}
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

// createFixedAssetWithSchedule creates an in_service FixedAsset (via the
// engine, so GenerateDepreciationScheduleOnWrite auto-builds its
// DepreciationSchedule from usefulLifeMonths) and directly marks the
// first postedCount of its generated rows posted via raw SQL —
// deliberately bypassing incrementFixedAssetPostedPeriodCount, the same
// pre-Version-4/never-backfilled shape this tool exists to correct.
// posted_period_count on the returned FixedAsset stays at its Default
// (0) regardless of postedCount.
func createFixedAssetWithSchedule(t *testing.T, tenantDB *sql.DB, assetNumber string, usefulLifeMonths, postedCount int) string {
	t.Helper()
	ctx := context.Background()

	inServiceID := statusIDByCode(t, tenantDB, "fixed_asset_status", "in_service")

	// Through crud.Engine, deliberately not a raw INSERT — the
	// GenerateDepreciationScheduleOnWrite hook that auto-builds
	// DepreciationSchedule from useful_life_months only fires on the
	// engine's own write path (schedule_hook.go), not a bare SQL insert.
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	assetDef, err := publishedDef(ctx, entityDefs, "FixedAsset")
	if err != nil {
		t.Fatalf("look up FixedAsset definition: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	// GenerateDepreciationScheduleOnWrite only fires when explicitly
	// registered on this engine instance (cmd/universal-core/main.go's
	// own RegisterHook wiring does this for the real server; a bare
	// crud.NewEngine has no hooks by default) — see
	// internal/kernel/assets/schedule_hook_test.go's own fixture for the
	// same registration.
	engine.SetHook("FixedAsset", assets.GenerateDepreciationScheduleOnWrite)
	rec, err := engine.Create(ctx, assetDef, map[string]any{
		"asset_number": assetNumber, "name": map[string]any{"en": "Backfill Asset"},
		"acquisition_date": "2020-01-01", "cost": float64(usefulLifeMonths * 1000), "salvage_value": 0.0,
		"useful_life_months": float64(usefulLifeMonths), "depreciation_method": "straight_line",
		"status_id": inServiceID,
	}, audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"})
	if err != nil {
		t.Fatalf("create FixedAsset %s: %v", assetNumber, err)
	}
	assetID := rec.ID

	rows, err := tenantDB.QueryContext(ctx,
		`SELECT id FROM records WHERE entity_type = 'DepreciationSchedule' AND data->>'fixed_asset_id' = $1 ORDER BY data->>'sequence'`,
		assetID,
	)
	if err != nil {
		t.Fatalf("list generated DepreciationSchedule rows for %s: %v", assetID, err)
	}
	defer rows.Close()
	var rowIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan DepreciationSchedule id: %v", err)
		}
		rowIDs = append(rowIDs, id)
	}
	if len(rowIDs) != usefulLifeMonths {
		t.Fatalf("FixedAsset %s: expected %d generated DepreciationSchedule rows, got %d", assetID, usefulLifeMonths, len(rowIDs))
	}

	for i := 0; i < postedCount; i++ {
		if _, err := tenantDB.ExecContext(ctx,
			`UPDATE records SET data = jsonb_set(data, '{posted_at}', to_jsonb('2020-01-31'::text)) WHERE id = $1::uuid`,
			rowIDs[i],
		); err != nil {
			t.Fatalf("mark DepreciationSchedule %s posted: %v", rowIDs[i], err)
		}
	}
	return assetID
}

func statusIDByCode(t *testing.T, tenantDB *sql.DB, statusTypeCode, code string) string {
	t.Helper()
	var id string
	err := tenantDB.QueryRowContext(context.Background(), `
		SELECT s.id FROM records s
		JOIN records st ON st.id::text = s.data->>'status_type_id'
		WHERE st.entity_type = 'StatusType' AND st.data->>'code' = $1
		  AND s.entity_type = 'Status' AND s.data->>'code' = $2`,
		statusTypeCode, code,
	).Scan(&id)
	if err != nil {
		t.Fatalf("look up Status %s/%s: %v", statusTypeCode, code, err)
	}
	return id
}

func postedPeriodCount(t *testing.T, tenantDB *sql.DB, assetID string) (float64, bool) {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT data FROM records WHERE id = $1`, assetID).Scan(&raw); err != nil {
		t.Fatalf("read FixedAsset %s: %v", assetID, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal FixedAsset %s: %v", assetID, err)
	}
	v, ok := data["posted_period_count"].(float64)
	return v, ok
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
	dsn, _ := tenantDatabase(t, true)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn})
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

// TestBackfill_ActorTypeValidation mirrors
// cmd/backfill-purchase-order-status's own test of the same name —
// audit.Actor validation must run, and run correctly in both
// directions, before this tool touches the database at all (uc-infra#72
// class of bug: a hard-coded/unvalidated actor_type falsifies the audit
// trail an unattended pipeline backfill run writes).
func TestBackfill_ActorTypeValidation(t *testing.T) {
	dsn, _ := tenantDatabase(t, false)

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

func TestBackfill_NoAssetsPublished_FailsCleanly(t *testing.T) {
	dsn, _ := tenantDatabase(t, false) // assets.Publish never run
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit against a tenant with no published FixedAsset Definition")
	}
	if !strings.Contains(stderr, "look up FixedAsset definition") {
		t.Fatalf("expected a clear FixedAsset lookup error, got stderr: %q", stderr)
	}
}

// TestBackfill_SetsCorrectCountFromPostedChildren is this tool's own
// core regression test: a FixedAsset with 3 generated
// DepreciationSchedule rows, 2 marked posted OUTSIDE
// incrementFixedAssetPostedPeriodCount (the exact never-backfilled shape
// this binary exists to fix), must come out of a real run with
// posted_period_count = 2 — not 0 (its unmigrated default) and not 3
// (the total row count, which would overcount an asset that hasn't
// actually finished).
func TestBackfill_SetsCorrectCountFromPostedChildren(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	assetID := createFixedAssetWithSchedule(t, tenantDB, "FA-BACKFILL-1", 3, 2)

	if got, _ := postedPeriodCount(t, tenantDB, assetID); got != 0 {
		t.Fatalf("setup: posted_period_count = %v, want 0 (unmigrated)", got)
	}

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=ops", "-actor-type=ai_agent", "-model-version=claude-test-1")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 FixedAsset") {
		t.Fatalf("expected the migration to actually run, got stdout: %q", stdout)
	}

	got, ok := postedPeriodCount(t, tenantDB, assetID)
	if !ok || got != 2 {
		t.Errorf("posted_period_count = %v (present=%v), want 2", got, ok)
	}

	var actorType, actorID string
	var modelVersion sql.NullString
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT actor_type, actor_id, model_version FROM audit_log WHERE record_id = $1 ORDER BY id DESC LIMIT 1`, assetID,
	).Scan(&actorType, &actorID, &modelVersion); err != nil {
		t.Fatalf("read audit_log row for %s: %v", assetID, err)
	}
	if actorType != "ai_agent" || actorID != "ops" {
		t.Errorf("expected actor_type=ai_agent, actor_id=ops, got actor_type=%q, actor_id=%q", actorType, actorID)
	}
	if !modelVersion.Valid || modelVersion.String != "claude-test-1" {
		t.Errorf("expected model_version=claude-test-1, got %+v", modelVersion)
	}
}

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	assetID := createFixedAssetWithSchedule(t, tenantDB, "FA-BACKFILL-DRY", 3, 1)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=ops", "-dry-run")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 FixedAsset") {
		t.Fatalf("expected a dry-run report, got stdout: %q", stdout)
	}
	if got, _ := postedPeriodCount(t, tenantDB, assetID); got != 0 {
		t.Errorf("dry run must not write: posted_period_count = %v, want 0", got)
	}
}

// TestBackfill_AlreadyCorrectIsIdempotent confirms a FixedAsset whose
// stored posted_period_count already matches its true count is left
// untouched (recordmigrate's own "already-migrated rows are skipped,
// not rewritten" convention, hand-rolled here) — a SECOND run against a
// tenant the FIRST run already migrated does no further work. Runs the
// real binary twice (not a single run against a pre-zeroed fixture,
// which independent review pointed out would pass identically even
// with the update logic deleted entirely — that proved 0==0, not
// idempotency) and checks the counter's stored value is unchanged by
// the second run, not just that its own stdout claims so.
func TestBackfill_AlreadyCorrectIsIdempotent(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	assetID := createFixedAssetWithSchedule(t, tenantDB, "FA-BACKFILL-IDEMPOTENT", 3, 2)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=ops")
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 FixedAsset") {
		t.Fatalf("expected the first run to actually migrate, got stdout: %q", stdout)
	}
	firstCount, ok := postedPeriodCount(t, tenantDB, assetID)
	if !ok || firstCount != 2 {
		t.Fatalf("after first run: posted_period_count = %v (present=%v), want 2", firstCount, ok)
	}

	stdout, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=ops")
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 0 FixedAsset record(s); 1 already had the correct posted_period_count") {
		t.Fatalf("expected the second run to be a no-op, got stdout: %q", stdout)
	}
	secondCount, ok := postedPeriodCount(t, tenantDB, assetID)
	if !ok || secondCount != 2 {
		t.Fatalf("after second (no-op) run: posted_period_count = %v (present=%v), want unchanged at 2", secondCount, ok)
	}
}
