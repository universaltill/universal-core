// Smoke tests for the real compiled cmd/backfill-purchase-order-status
// binary, against real "legacy" PurchaseOrder rows (pre-Version-4:
// plain "status" string, no "status_id") inserted directly via SQL —
// exactly the shape a row written before the Version 3->4 bump actually
// has, which is the whole reason this binary exists (see
// purchasing.PurchaseOrder's own doc comment).
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

	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "backfill-purchase-order-status")
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
// tenantdb.Router.Create), with foundation always published and
// purchasing's entity Definition + Form published. Whether
// purchasing.PublishStatuses has also run is the caller's choice
// (withStatuses) — TestBackfill_NoStatusesPublished_FailsCleanly
// deliberately needs it not to have run. Returns the tenant's own DSN
// (what a real operator would set DATABASE_URL to per this binary's own
// doc comment — it talks to a tenant database directly, no router/
// control-plane wiring of its own) and an open connection to it.
func tenantDatabase(t *testing.T, withStatuses bool) (dsn string, tenantDB *sql.DB) {
	t.Helper()
	controlDSN := testexec.FreshDatabase(t, "uc_test_backfill_control")
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
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if withStatuses {
		if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
			t.Fatalf("purchasing.PublishStatuses: %v", err)
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

func insertLegacyPurchaseOrder(t *testing.T, tenantDB *sql.DB, poNumber string, data map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal legacy PurchaseOrder data for %s: %v", poNumber, err)
	}
	var id string
	err = tenantDB.QueryRowContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('PurchaseOrder', $1) RETURNING id`, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy PurchaseOrder %s: %v", poNumber, err)
	}
	return id
}

func recordStatusID(t *testing.T, tenantDB *sql.DB, id string) (statusID, oldStatus string) {
	t.Helper()
	var raw []byte
	if err := tenantDB.QueryRowContext(context.Background(), `SELECT data FROM records WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal record %s data: %v", id, err)
	}
	sid, _ := data["status_id"].(string)
	old, _ := data["status"].(string)
	return sid, old
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

func TestBackfill_NoStatusesPublished_FailsCleanly(t *testing.T) {
	dsn, _ := tenantDatabase(t, false) // purchasing.PublishStatuses never run
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit against a tenant with no purchase_order_status StatusType")
	}
	if !strings.Contains(stderr, "no purchase_order_status StatusType") {
		t.Fatalf("expected a clear StatusType error, got stderr: %q", stderr)
	}
}

func TestBackfill_DryRun_ReportsWithoutWriting(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)
	id := insertLegacyPurchaseOrder(t, tenantDB, "PO-DRY-1", map[string]any{
		"po_number": "PO-DRY-1", "vendor_id": "00000000-0000-0000-0000-000000000001",
		"order_date": "2026-01-01", "status": "draft",
	})

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would migrate 1 PurchaseOrder") {
		t.Fatalf("expected dry-run summary reporting 1 record, got stdout: %q", stdout)
	}

	statusID, oldStatus := recordStatusID(t, tenantDB, id)
	if statusID != "" {
		t.Fatalf("dry run must not write status_id, found %q", statusID)
	}
	if oldStatus != "draft" {
		t.Fatalf("dry run must not touch the old status field, got %q", oldStatus)
	}
}

func TestBackfill_MigratesLegacyRows_SkipsUnrecognized_IsIdempotent(t *testing.T) {
	dsn, tenantDB := tenantDatabase(t, true)

	draftID := insertLegacyPurchaseOrder(t, tenantDB, "PO-1", map[string]any{
		"po_number": "PO-1", "vendor_id": "00000000-0000-0000-0000-000000000001",
		"order_date": "2026-01-01", "status": "draft",
	})
	cancelledID := insertLegacyPurchaseOrder(t, tenantDB, "PO-2", map[string]any{
		"po_number": "PO-2", "vendor_id": "00000000-0000-0000-0000-000000000001",
		"order_date": "2026-01-02", "status": "cancelled",
	})
	unrecognizedID := insertLegacyPurchaseOrder(t, tenantDB, "PO-3", map[string]any{
		"po_number": "PO-3", "vendor_id": "00000000-0000-0000-0000-000000000001",
		"order_date": "2026-01-03", "status": "not-a-real-status",
	})

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 2 PurchaseOrder record(s); 0 already had status_id; 1 skipped for manual review.") {
		t.Fatalf("unexpected summary line: %q", stdout)
	}
	if !strings.Contains(stderr, `unrecognized old status "not-a-real-status"`) {
		t.Fatalf("expected a warning naming the unrecognized status, got stderr: %q", stderr)
	}

	for _, id := range []string{draftID, cancelledID} {
		statusID, oldStatus := recordStatusID(t, tenantDB, id)
		if statusID == "" {
			t.Fatalf("record %s: expected status_id to be set after migration", id)
		}
		if oldStatus != "" {
			t.Fatalf("record %s: expected the old status field to be dropped, still has %q", id, oldStatus)
		}
	}
	if statusID, _ := recordStatusID(t, tenantDB, unrecognizedID); statusID != "" {
		t.Fatalf("unrecognized-status record must be left untouched, got status_id %q", statusID)
	}

	// Re-run: the two already-migrated rows must be reported as
	// "already had status_id", not migrated a second time.
	stdout, stderr, code = run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Migrated 0 PurchaseOrder record(s); 2 already had status_id; 1 skipped for manual review.") {
		t.Fatalf("second run should be idempotent, got summary: %q", stdout)
	}
}
