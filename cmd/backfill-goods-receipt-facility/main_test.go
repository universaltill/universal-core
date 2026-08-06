// Smoke tests for the real compiled cmd/backfill-goods-receipt-facility
// binary, against real "legacy" GoodsReceipt rows (pre-Version-2: no
// facility_id at all) inserted directly via SQL — exactly the shape a
// row written before the Version 1->2 bump actually has, which is the
// whole reason this binary exists (see purchasing.GoodsReceipt's own
// doc comment). Raw SQL is the only way to produce them: v2 makes
// facility_id Required, so crud.Engine.Create would reject the very
// rows under test. Deliberately mirrors
// cmd/backfill-inventory-facility/main_test.go closely — same binary
// shape, same purchasing.GetOrCreateFacility underneath, same test
// coverage bar.
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
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-goods-receipt-facility")
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
// tenantdb.Router.Create), with foundation always published. Whether
// purchasing.Publish has also run is the caller's choice (withPurchasing)
// — TestBackfill_NoFacilityDefinitionPublished_FailsCleanly deliberately
// needs it not to have run, since BOTH entity Definitions this binary
// depends on (GoodsReceipt and Facility) live in the purchasing module.
// Returns the tenant's own DSN (what a real operator would set
// DATABASE_URL to per this binary's own doc comment — it talks to a
// tenant database directly, no router/control-plane wiring of its own)
// and an open connection to it.
func tenantDatabase(t *testing.T, withPurchasing bool) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_gr_fac_control")
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

	id, err := router.Create(ctx, "Backfill GoodsReceipt Facility Smoke Test", "eu-west")
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
	if withPurchasing {
		if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
			t.Fatalf("purchasing.Publish: %v", err)
		}
		if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
			t.Fatalf("purchasing.PublishForms: %v", err)
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
	// Same host/user/password/sslmode as base — only dbname differs
	// (internal/tenantdb.Router's own doc comment describes this exact
	// shape for every tenant database on the same Postgres server).
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// publishGoodsReceiptOnly puts the GoodsReceipt v2 Definition into the
// registry WITHOUT the rest of purchasing — the only way to reach this
// binary's Facility-definition lookup, which is otherwise shadowed by
// the GoodsReceipt lookup that runs first.
func publishGoodsReceiptOnly(t *testing.T, tenantDB *sql.DB) {
	t.Helper()
	def := purchasing.GoodsReceipt()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal GoodsReceipt definition: %v", err)
	}
	err = moduleseed.PublishAll(context.Background(),
		data.NewEntityDefinitionRepo(tenantDB),
		[]moduleseed.Item{{Key: def.EntityType, Version: def.Version, Raw: raw}},
		audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"},
	)
	if err != nil {
		t.Fatalf("publish GoodsReceipt definition alone: %v", err)
	}
}

// insertLegacyGoodsReceipt writes a records row directly, bypassing
// crud.Engine (and therefore entity.ValidateRecord) — see the package
// comment: a v1-shaped GoodsReceipt with no facility_id cannot be
// created through the engine once v2 is published, which is precisely
// the state this binary repairs.
func insertLegacyGoodsReceipt(t *testing.T, tenantDB *sql.DB, label string, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal legacy GoodsReceipt data for %s: %v", label, err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('GoodsReceipt', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy GoodsReceipt %s: %v", label, err)
	}
	return id
}

// insertFacility writes a Facility records row directly — used to
// pre-seed a facility the binary must find rather than create.
func insertFacility(t *testing.T, tenantDB *sql.DB, code, name string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"code": code, "name": name, "facility_type": "warehouse", "is_active": true,
	})
	if err != nil {
		t.Fatalf("marshal Facility %s: %v", code, err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('Facility', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert Facility %s: %v", code, err)
	}
	return id
}

// recordFacility reads back one record's facility_id and its row
// version. The version is what proves an already-stamped record was
// genuinely left alone rather than rewritten with the same value — a
// no-op Update would still bump it.
func recordFacility(t *testing.T, tenantDB *sql.DB, id string) (facilityID string, version int) {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT data, version FROM records WHERE id = $1`, id,
	).Scan(&raw, &version); err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal record %s data: %v", id, err)
	}
	fid, _ := fields["facility_id"].(string)
	return fid, version
}

type facilityRow struct {
	id   string
	code string
	name string
}

// facilities returns every live Facility record, so a test can assert on
// how many exist as well as on their content ("created it once" is a
// claim about the whole table, not about one row).
func facilities(t *testing.T, tenantDB *sql.DB) []facilityRow {
	t.Helper()
	rows, err := tenantDB.QueryContext(context.Background(),
		`SELECT id, data FROM records WHERE entity_type = 'Facility' AND deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("list Facility records: %v", err)
	}
	defer rows.Close()
	var out []facilityRow
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatalf("scan Facility record: %v", err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal Facility %s: %v", id, err)
		}
		code, _ := fields["code"].(string)
		name, _ := fields["name"].(string)
		out = append(out, facilityRow{id: id, code: code, name: name})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Facility records: %v", err)
	}
	return out
}

func legacyFields(purchaseOrderID, receivedDate string) map[string]any {
	return map[string]any{
		"purchase_order_id": purchaseOrderID,
		"received_date":     receivedDate,
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
	dsn, _ := tenantDatabase(t, true)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn})
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

func TestBackfill_NoFacilityDefinitionPublished_FailsCleanly(t *testing.T) {
	// Facility and GoodsReceipt both live in purchasing, so "no Facility
	// definition" has two real shapes: the module was never published at
	// all, and (the narrower one this binary's own error message
	// addresses) GoodsReceipt is published but Facility is not.
	t.Run("purchasing never published", func(t *testing.T) {
		dsn, _ := tenantDatabase(t, false)
		_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
		if code == 0 {
			t.Fatal("expected non-zero exit against a tenant with no purchasing Definitions")
		}
		if strings.Contains(stderr, "panic:") {
			t.Fatalf("must fail cleanly, not panic; stderr: %q", stderr)
		}
		if !strings.Contains(stderr, "look up GoodsReceipt definition") || !strings.Contains(stderr, "not found") {
			t.Fatalf("expected a clear missing-definition error, got stderr: %q", stderr)
		}
	})

	t.Run("GoodsReceipt published but Facility is not", func(t *testing.T) {
		dsn, tenantDB := tenantDatabase(t, false)
		publishGoodsReceiptOnly(t, tenantDB)
		_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
		if code == 0 {
			t.Fatal("expected non-zero exit against a tenant with no published Facility definition")
		}
		if strings.Contains(stderr, "panic:") {
			t.Fatalf("must fail cleanly, not panic; stderr: %q", stderr)
		}
		if !strings.Contains(stderr, "look up Facility definition") {
			t.Fatalf("expected a clear Facility-definition error, got stderr: %q", stderr)
		}
	})
}

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	id := insertLegacyGoodsReceipt(t, tenantDB, "GR-DRY-1",
		legacyFields("00000000-0000-0000-0000-000000000001", "2026-01-10"))

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 GoodsReceipt record(s); 0 already had facility_id; 0 skipped for manual review.") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}
	// The facility the real run would create has to be announced as a
	// would-be, not reported as created.
	if !strings.Contains(stderr, `DRY RUN: would create Facility "MAIN" (Main Warehouse)`) {
		t.Fatalf("expected a dry-run notice about the facility, got stderr: %q", stderr)
	}
	if strings.Contains(stderr, `created Facility "MAIN"`) {
		t.Fatalf("dry run must not report having created a facility, got stderr: %q", stderr)
	}

	// Nothing written: no Facility created, and the record is untouched
	// (including its version — a dry run must not write at all).
	if got := facilities(t, tenantDB); len(got) != 0 {
		t.Fatalf("dry run must not create a Facility, found %d: %+v", len(got), got)
	}
	facilityID, version := recordFacility(t, tenantDB, id)
	if facilityID != "" {
		t.Fatalf("dry run must not write facility_id, found %q", facilityID)
	}
	if version != 1 {
		t.Fatalf("dry run must not rewrite the record, version is now %d", version)
	}
}

func TestBackfill_StampsFacility_CreatesItOnce_IsIdempotent(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)

	bareA := insertLegacyGoodsReceipt(t, tenantDB, "GR-1",
		legacyFields("00000000-0000-0000-0000-000000000001", "2026-01-10"))
	bareB := insertLegacyGoodsReceipt(t, tenantDB, "GR-2",
		legacyFields("00000000-0000-0000-0000-000000000002", "2026-01-11"))

	// Already carries a facility_id, and a different one — it must be
	// left exactly as-is, not re-pointed at MAIN.
	const otherFacilityID = "11111111-1111-1111-1111-111111111111"
	stampedFields := legacyFields("00000000-0000-0000-0000-000000000003", "2026-01-12")
	stampedFields["facility_id"] = otherFacilityID
	stamped := insertLegacyGoodsReceipt(t, tenantDB, "GR-3", stampedFields)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 2 GoodsReceipt record(s); 1 already had facility_id; 0 skipped for manual review.") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}

	created := facilities(t, tenantDB)
	if len(created) != 1 {
		t.Fatalf("expected exactly one Facility, got %d: %+v", len(created), created)
	}
	if created[0].code != "MAIN" {
		t.Fatalf("expected the default facility code MAIN, got %q", created[0].code)
	}
	if !strings.Contains(stderr, `created Facility "MAIN"`) {
		t.Fatalf("expected the run to report creating the facility, got stderr: %q", stderr)
	}
	mainID := created[0].id

	for _, id := range []string{bareA, bareB} {
		facilityID, _ := recordFacility(t, tenantDB, id)
		if facilityID != mainID {
			t.Fatalf("record %s: expected facility_id %s, got %q", id, mainID, facilityID)
		}
	}
	stampedID, stampedVersion := recordFacility(t, tenantDB, stamped)
	if stampedID != otherFacilityID {
		t.Fatalf("already-stamped record must keep its own facility_id %s, got %q", otherFacilityID, stampedID)
	}
	if stampedVersion != 1 {
		t.Fatalf("already-stamped record must not be rewritten, version is now %d", stampedVersion)
	}

	// Re-run: every row now has a facility_id, so nothing is migrated and
	// no second facility is created.
	stdout, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 0 GoodsReceipt record(s); 3 already had facility_id; 0 skipped for manual review.") {
		t.Fatalf("second run should be idempotent, got summary: %q", stdout)
	}
	if !strings.Contains(stderr, `using existing Facility "MAIN"`) {
		t.Fatalf("second run should reuse the facility, got stderr: %q", stderr)
	}
	if after := facilities(t, tenantDB); len(after) != 1 || after[0].id != mainID {
		t.Fatalf("second run must not create another Facility, got %+v", after)
	}
}

func TestBackfill_RespectsFacilityCodeFlag(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	id := insertLegacyGoodsReceipt(t, tenantDB, "GR-DEPOT-1",
		legacyFields("00000000-0000-0000-0000-000000000001", "2026-01-10"))

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn},
		"-actor-id=smoke-test", "-facility-code=DEPOT-2", "-facility-name=Second Depot")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 GoodsReceipt record(s); 0 already had facility_id; 0 skipped for manual review.") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}

	created := facilities(t, tenantDB)
	if len(created) != 1 {
		t.Fatalf("expected exactly one Facility, got %d: %+v", len(created), created)
	}
	if created[0].code != "DEPOT-2" || created[0].name != "Second Depot" {
		t.Fatalf("expected Facility DEPOT-2/\"Second Depot\", got %+v", created[0])
	}
	if facilityID, _ := recordFacility(t, tenantDB, id); facilityID != created[0].id {
		t.Fatalf("record %s: expected facility_id %s, got %q", id, created[0].id, facilityID)
	}
}

func TestBackfill_UsesExistingFacility(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	existingID := insertFacility(t, tenantDB, "MAIN", "Pre-existing Warehouse")
	id := insertLegacyGoodsReceipt(t, tenantDB, "GR-EXISTING-1",
		legacyFields("00000000-0000-0000-0000-000000000001", "2026-01-10"))

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 1 GoodsReceipt record(s); 0 already had facility_id; 0 skipped for manual review.") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, `using existing Facility "MAIN": `+existingID) {
		t.Fatalf("expected the run to reuse the pre-existing facility, got stderr: %q", stderr)
	}

	all := facilities(t, tenantDB)
	if len(all) != 1 {
		t.Fatalf("expected no second MAIN Facility, got %d: %+v", len(all), all)
	}
	if all[0].id != existingID || all[0].name != "Pre-existing Warehouse" {
		t.Fatalf("the pre-existing Facility must be untouched, got %+v", all[0])
	}
	if facilityID, _ := recordFacility(t, tenantDB, id); facilityID != existingID {
		t.Fatalf("record %s: expected facility_id %s, got %q", id, existingID, facilityID)
	}
}
