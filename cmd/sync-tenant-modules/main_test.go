// Smoke tests for the real compiled cmd/sync-tenant-modules binary
// against real, deliberately-stale tenant databases.
//
// "Stale" here is not a fixture invented for the test: it is the exact
// shape the live Demo Organization tenant was measured in on 2026-08-01
// (ADR-0017's Context) — purchasing published, but no `Facility` at all
// and `InventoryItem` still at the pre-#12 v2 shape, while the code in
// front of it already had both. Building that state needs
// moduleseed.PublishAll with a hand-built item list, because no code
// path publishes purchasing partially any more; publishing the current
// module and then "un-publishing" pieces of it would be a different
// state (rolled_back rows), not this one.
//
// DATABASE_URL is the CONTROL-PLANE database for this binary, unlike the
// backfill commands' single tenant database — every tenant here is
// reached through internal/tenantdb's router, so each test owns a whole
// control plane plus the tenant databases hanging off it.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/moduleseed"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/modules"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := testexec.Build(".", "sync-tenant-modules")
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

var setupActor = audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}

// controlPlane creates a fresh control-plane database with the real
// control schema applied, plus a router over it. The returned DSN is
// what a real operator sets DATABASE_URL to for this binary (see its own
// doc comment: the control plane, not a tenant).
func controlPlane(t *testing.T) (dsn string, control *sql.DB, router *tenantdb.Router) {
	t.Helper()
	dsn = testexec.FreshDatabase(t, "uc_test_sync_control")
	control = testexec.Open(t, dsn)
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	// Registered before any tenant's DropTenantDatabase, so (t.Cleanup
	// being LIFO) every tenant database is dropped before this closes the
	// pools that were still holding them open.
	t.Cleanup(func() { router.Close() })
	return dsn, control, router
}

// newTenant provisions one real tenant database (schema via
// tenantdb.Router.Create, nothing published yet) and registers its drop —
// the tenant's own database is a second physical database the control
// database's cleanup knows nothing about, so skipping this leaks it
// permanently (see testexec.DropTenantDatabase).
func newTenant(t *testing.T, control *sql.DB, router *tenantdb.Router, name string) (id string, tenantDB *sql.DB) {
	t.Helper()
	ctx := context.Background()
	id, err := router.Create(ctx, name, "eu-west")
	if err != nil {
		t.Fatalf("router.Create(%s): %v", name, err)
	}
	testexec.DropTenantDatabase(t, control, id)
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get(%s): %v", name, err)
	}
	return id, tenantDB
}

// staleTenant is the whole point of this binary: a tenant provisioned
// before #12 shipped. foundation is fully current, purchasing is present
// but two definitions behind — no Facility row in the registry at all,
// and InventoryItem still at v2.
func staleTenant(t *testing.T, control *sql.DB, router *tenantdb.Router, name string) (id string, tenantDB *sql.DB) {
	t.Helper()
	id, tenantDB = newTenant(t, control, router, name)
	ctx := context.Background()
	if err := modules.PublishFoundation(ctx, tenantDB, setupActor); err != nil {
		t.Fatalf("PublishFoundation: %v", err)
	}
	publishStalePurchasing(t, tenantDB)
	return id, tenantDB
}

// publishStalePurchasing publishes every purchasing Definition EXCEPT
// Facility, with InventoryItem pinned at v2 — the same hand-built
// moduleseed.Item list technique cmd/backfill-inventory-facility's
// publishInventoryItemOnly uses, for the same reason: purchasing.Publish
// is all-or-nothing at current code, and the state under test is a
// registry that is *behind* current code.
func publishStalePurchasing(t *testing.T, tenantDB *sql.DB) {
	t.Helper()
	var items []moduleseed.Item
	for _, def := range purchasing.All() {
		switch def.EntityType {
		case "Facility":
			continue
		case "InventoryItem":
			def = inventoryItemV2()
		}
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("marshal %s definition: %v", def.EntityType, err)
		}
		items = append(items, moduleseed.Item{Key: def.EntityType, Version: def.Version, Raw: raw})
	}
	err := moduleseed.PublishAll(context.Background(),
		data.NewEntityDefinitionRepo(tenantDB), items, setupActor)
	if err != nil {
		t.Fatalf("publish stale purchasing definitions: %v", err)
	}
}

// inventoryItemV2 is the pre-#12 InventoryItem: no facility_id, so no
// concept of *where* stock is. Reconstructed here rather than kept in
// the module (the module only ever carries current code) — v3 is what
// purchasing.InventoryItem() returns.
func inventoryItemV2() *entity.Definition {
	return &entity.Definition{
		EntityType: "InventoryItem",
		Version:    2,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty_on_hand", Type: entity.FieldNumber, Required: true, Default: float64(0)},
			{Name: "qty_available_to_promise", Type: entity.FieldNumber, Required: true, Default: float64(0)},
		},
	}
}

// publishedEntityVersions is the assertion counterpart of the binary's own
// version snapshot: entity type -> highest published version. An entity
// type with no published row is simply absent, which is exactly how
// "Facility does not exist for this tenant" reads.
func publishedEntityVersions(t *testing.T, tenantDB *sql.DB) map[string]int {
	t.Helper()
	rows, err := tenantDB.QueryContext(context.Background(),
		`SELECT entity_type, max(version) FROM entity_definitions
		 WHERE status = 'published' GROUP BY entity_type`)
	if err != nil {
		t.Fatalf("read published versions: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var et string
		var v int
		if err := rows.Scan(&et, &v); err != nil {
			t.Fatalf("scan published version: %v", err)
		}
		out[et] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate published versions: %v", err)
	}
	return out
}

// assertStale fails unless the tenant is still in the pre-sync state —
// used to prove a dry run wrote nothing and that -tenant-id left the
// other tenant alone.
func assertStale(t *testing.T, tenantDB *sql.DB, what string) {
	t.Helper()
	versions := publishedEntityVersions(t, tenantDB)
	if v, ok := versions["Facility"]; ok {
		t.Fatalf("%s: Facility must still be unpublished, found v%d", what, v)
	}
	if v := versions["InventoryItem"]; v != 2 {
		t.Fatalf("%s: InventoryItem must still be v2, got v%d", what, v)
	}
}

// insertLegacyInventoryItem writes a v2-shaped record (no facility_id)
// straight into records, bypassing crud.Engine — v3 makes facility_id
// Required, so the engine would reject the very row whose existence the
// sync has to warn about (same reasoning as
// cmd/backfill-inventory-facility's own smoke tests).
func insertLegacyInventoryItem(t *testing.T, tenantDB *sql.DB, itemID string, qty float64) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"item_id":                  itemID,
		"qty_on_hand":              qty,
		"qty_available_to_promise": qty,
	})
	if err != nil {
		t.Fatalf("marshal legacy InventoryItem: %v", err)
	}
	if _, err := tenantDB.ExecContext(context.Background(),
		`INSERT INTO records (entity_type, data) VALUES ('InventoryItem', $1)`, raw,
	); err != nil {
		t.Fatalf("insert legacy InventoryItem: %v", err)
	}
}

func TestSync_MissingDatabaseURL_FailsFast(t *testing.T) {
	_, stderr, code := run(t, []string{}, "-actor-id=smoke-test")
	if code == 0 {
		t.Fatal("expected non-zero exit with DATABASE_URL unset")
	}
	if !strings.Contains(stderr, "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got stderr: %q", stderr)
	}
}

func TestSync_MissingActorID_FailsFast(t *testing.T) {
	dsn, _, _ := controlPlane(t)
	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn})
	if code == 0 {
		t.Fatal("expected non-zero exit with -actor-id unset")
	}
	if !strings.Contains(stderr, "-actor-id is required") {
		t.Fatalf("expected -actor-id error, got stderr: %q", stderr)
	}
}

// TestSync_BringsAStaleTenantUpToDate is the core case: the drift
// measured on the live tenant, closed by one run.
func TestSync_BringsAStaleTenantUpToDate(t *testing.T) {
	dsn, control, router := controlPlane(t)
	id, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")
	assertStale(t, tenantDB, "before sync")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("sync: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Synced 1 tenant(s); 0 partially synced; 0 skipped.") {
		t.Fatalf("unexpected summary, got stdout: %q", stdout)
	}

	// The per-tenant progress line names exactly what moved, and the
	// module set it inferred (ADR-0017 §1).
	want := fmt.Sprintf("Sync Smoke Test (%s): Facility v1 (new), InventoryItem v2->v3 [foundation, purchasing]", id)
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected progress line %q, got stderr: %q", want, stderr)
	}

	versions := publishedEntityVersions(t, tenantDB)
	if v, ok := versions["Facility"]; !ok || v != 1 {
		t.Fatalf("expected Facility v1 published after sync, got v%d (present=%v)", v, ok)
	}
	if v := versions["InventoryItem"]; v != 3 {
		t.Fatalf("expected InventoryItem v3 after sync, got v%d", v)
	}
	// No records exist, so nothing was invalidated — the data-migration
	// section must stay silent rather than cry wolf.
	if strings.Contains(stdout, "data migration(s) now needed") {
		t.Fatalf("no records exist, so no data migration should be reported, got stdout: %q", stdout)
	}
}

// TestSync_NeverGrantsAModule is ADR-0017 §2, and the safety property
// that makes running this over the whole fleet acceptable: a tenant with
// purchasing must not come out of a sync with CRM.
func TestSync_NeverGrantsAModule(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("sync: exit %d, stderr: %s", code, stderr)
	}

	versions := publishedEntityVersions(t, tenantDB)
	for _, key := range []string{"crm", "hr", "projects", "assets"} {
		for _, def := range modules.Publishers[key].Definitions() {
			if v, ok := versions[def.EntityType]; ok {
				t.Fatalf("sync granted module %q: %s v%d appeared in a purchasing-only tenant",
					key, def.EntityType, v)
			}
		}
	}

	// And the tenant's inferred module set is unchanged by the sync.
	mods, err := data.NewEntityDefinitionRepo(tenantDB).PublishedModules(context.Background())
	if err != nil {
		t.Fatalf("PublishedModules: %v", err)
	}
	if strings.Join(mods, ",") != "foundation,purchasing" {
		t.Fatalf("expected modules [foundation purchasing] after sync, got %v", mods)
	}
}

func TestSync_IsIdempotent(t *testing.T) {
	dsn, control, router := controlPlane(t)
	id, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("first sync: exit %d, stderr: %s", code, stderr)
	}
	afterFirst := publishedEntityVersions(t, tenantDB)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("second sync: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Synced 1 tenant(s); 0 partially synced; 0 skipped.") {
		t.Fatalf("unexpected second-run summary, got stdout: %q", stdout)
	}
	want := fmt.Sprintf("Sync Smoke Test (%s): already current [foundation, purchasing]", id)
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected %q on the second run, got stderr: %q", want, stderr)
	}
	if strings.Contains(stdout, "data migration(s) now needed") {
		t.Fatalf("a no-op run must report no data migrations, got stdout: %q", stdout)
	}

	afterSecond := publishedEntityVersions(t, tenantDB)
	if len(afterFirst) != len(afterSecond) {
		t.Fatalf("second run changed the published set: %d types before, %d after", len(afterFirst), len(afterSecond))
	}
	for et, v := range afterFirst {
		if afterSecond[et] != v {
			t.Fatalf("second run moved %s from v%d to v%d", et, v, afterSecond[et])
		}
	}
}

func TestSync_DryRunReportsWithoutWriting(t *testing.T) {
	dsn, control, router := controlPlane(t)
	id, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would sync 1 tenant(s); 0 partially synced; 0 skipped.") {
		t.Fatalf("expected a would-sync summary, got stdout: %q", stdout)
	}
	// Exactly the changes the real run makes in
	// TestSync_BringsAStaleTenantUpToDate — a dry run whose report
	// differs from the run it predicts is worthless as a drift report.
	want := fmt.Sprintf("DRY RUN: Sync Smoke Test (%s): Facility v1 (new), InventoryItem v2->v3 [foundation, purchasing]", id)
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected planned-change line %q, got stderr: %q", want, stderr)
	}

	assertStale(t, tenantDB, "after dry run")
}

func TestSync_WarnsAboutRecordsMadeInvalid(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")
	insertLegacyInventoryItem(t, tenantDB, "00000000-0000-0000-0000-000000000001", 5)

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("sync: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "1 data migration(s) now needed:") {
		t.Fatalf("expected a data-migration section, got stdout: %q", stdout)
	}
	want := `Sync Smoke Test: InventoryItem — 1 record(s) lack the now-required "facility_id" and will fail validation on next edit`
	if !strings.Contains(stdout, want) {
		t.Fatalf("expected warning %q, got stdout: %q", want, stdout)
	}
	if !strings.Contains(stdout, "Run the matching backfill command") {
		t.Fatalf("expected a pointer to the backfill command, got stdout: %q", stdout)
	}
	// The sync reports; it must not have repaired anything itself.
	var missing int
	if err := tenantDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM records WHERE entity_type = 'InventoryItem'
		   AND deleted_at IS NULL AND coalesce(data->>'facility_id', '') = ''`,
	).Scan(&missing); err != nil {
		t.Fatalf("count records missing facility_id: %v", err)
	}
	if missing != 1 {
		t.Fatalf("sync must not migrate records itself, %d row(s) still lack facility_id", missing)
	}
}

func TestSync_SkipsATenantWithNoDefinitions(t *testing.T) {
	dsn, control, router := controlPlane(t)
	id, tenantDB := newTenant(t, control, router, "Sync Smoke Test Empty")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("sync: exit %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stderr, "panic:") {
		t.Fatalf("a definition-less tenant must be skipped cleanly, not panic; stderr: %q", stderr)
	}
	want := fmt.Sprintf("WARNING: Sync Smoke Test Empty (%s): no published definitions carrying a module key", id)
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected skip warning %q, got stderr: %q", want, stderr)
	}
	if !strings.Contains(stdout, "Synced 0 tenant(s); 0 partially synced; 1 skipped.") {
		t.Fatalf("expected the tenant counted as skipped, got stdout: %q", stdout)
	}
	if v := publishedEntityVersions(t, tenantDB); len(v) != 0 {
		t.Fatalf("a skipped tenant must gain nothing, got %v", v)
	}
}

func TestSync_TenantIDTargetsOneTenant(t *testing.T) {
	dsn, control, router := controlPlane(t)
	targetID, target := staleTenant(t, control, router, "Sync Smoke Test One")
	_, bystander := staleTenant(t, control, router, "Sync Smoke Test Two")

	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn},
		"-actor-id=smoke-test", "-tenant-id="+targetID)
	if code != 0 {
		t.Fatalf("sync: exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Synced 1 tenant(s); 0 partially synced; 0 skipped.") {
		t.Fatalf("expected exactly one tenant synced, got stdout: %q", stdout)
	}
	if strings.Contains(stderr, "Sync Smoke Test Two") {
		t.Fatalf("the untargeted tenant must not even be visited, got stderr: %q", stderr)
	}

	versions := publishedEntityVersions(t, target)
	if v, ok := versions["Facility"]; !ok || v != 1 {
		t.Fatalf("targeted tenant: expected Facility v1, got v%d (present=%v)", v, ok)
	}
	if v := versions["InventoryItem"]; v != 3 {
		t.Fatalf("targeted tenant: expected InventoryItem v3, got v%d", v)
	}
	assertStale(t, bystander, "untargeted tenant")
}

// A dry run must predict the data migrations a real run would create,
// not just the version bumps. An earlier version returned before
// computing them, so -dry-run reported "InventoryItem v2->v3" and said
// nothing about the ten records that bump would strand — the exact
// obligation ADR-0017 §5 calls the load-bearing part of this command,
// missing from the mode most likely to be run first.
func TestSync_DryRunPredictsTheMigrationsItWouldCreate(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")
	insertLegacyInventoryItem(t, tenantDB, "00000000-0000-0000-0000-000000000001", 5)

	dryOut, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d", code)
	}
	for _, want := range []string{
		"data migration(s) now needed",
		"InventoryItem",
		"facility_id",
	} {
		if !strings.Contains(dryOut, want) {
			t.Errorf("dry-run stdout is missing %q — a preview that hides the backfill obligation is the failure this test exists for:\n%s", want, dryOut)
		}
	}
	// Still a dry run: nothing published, nothing repaired.
	assertStale(t, tenantDB, "after a dry run")

	// And the prediction must match what the real run actually reports.
	realOut, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("real run: exit %d", code)
	}
	dryWarn := warningSection(dryOut)
	realWarn := warningSection(realOut)
	if dryWarn == "" {
		t.Fatal("dry run produced no warning section to compare")
	}
	if dryWarn != realWarn {
		t.Errorf("dry run predicted a different migration set than the real run produced:\ndry:  %q\nreal: %q", dryWarn, realWarn)
	}
}

// warningSection returns everything from the data-migration header
// onward, so a dry run's prediction can be compared to a real run's
// report verbatim.
func warningSection(out string) string {
	i := strings.Index(out, "data migration(s) now needed")
	if i < 0 {
		return ""
	}
	return out[i:]
}

// Naming one tenant that then gets skipped must fail loudly. Otherwise
// a mistyped -tenant-id is indistinguishable from success: the tenant is
// logged as skipped and the process exits 0 saying "Synced 0 tenant(s);
// 1 skipped."
func TestSync_UnknownTenantIDFailsLoudly(t *testing.T) {
	dsn, control, router := controlPlane(t)
	staleTenant(t, control, router, "Sync Smoke Test")

	_, stderr, code := run(t, []string{"DATABASE_URL=" + dsn},
		"-actor-id=smoke-test", "-tenant-id=00000000-0000-0000-0000-0000000000ff")
	if code == 0 {
		t.Errorf("a -tenant-id that resolves to nothing must exit non-zero, got 0; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "was not synced") {
		t.Errorf("stderr should say plainly that the named tenant was not synced:\n%s", stderr)
	}
}

// The fleet-wide case is deliberately the opposite: one unreachable
// tenant must not fail the run, because with database-per-tenant
// (ADR-0003) it says nothing about the others, and a fleet sync held
// hostage to one sick tenant is worse than a reported skip.
func TestSync_OneBadTenantDoesNotFailAFleetRun(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, healthy := staleTenant(t, control, router, "Sync Smoke Test")
	newTenant(t, control, router, "Sync Smoke Test Empty") // no definitions -> skipped

	stdout, _, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("a fleet run must not fail because one tenant was skipped, got exit %d", code)
	}
	if !strings.Contains(stdout, "Synced 1 tenant(s); 0 partially synced; 1 skipped.") {
		t.Errorf("expected 1 synced / 1 skipped, got:\n%s", stdout)
	}
	versions := publishedEntityVersions(t, healthy)
	if versions["Facility"] == 0 {
		t.Error("the healthy tenant must still have been synced despite the other being skipped")
	}
}

// A tenant whose registry has ROLLED BACK the version the code wants
// can never be advanced: moduleseed.publishOne deliberately leaves a
// rolled_back row alone. The first implementation predicted that change
// in its dry run — inventing a backfill obligation that could never
// arrive — and said nothing on the real path, so a permanently-stuck
// tenant looked exactly like a healthy one (independent review).
func TestSync_RolledBackTargetVersionIsReportedNotPredicted(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, tenantDB := staleTenant(t, control, router, "Sync Smoke Test")
	insertLegacyInventoryItem(t, tenantDB, "00000000-0000-0000-0000-000000000001", 5)

	// Publish InventoryItem v3, then roll it back: the exact state that
	// makes the sync unable to advance this type.
	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(tenantDB)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	raw, err := json.Marshal(purchasing.InventoryItem())
	if err != nil {
		t.Fatalf("marshal InventoryItem: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, "InventoryItem", 3, raw, actor); err != nil {
		t.Fatalf("draft v3: %v", err)
	}
	if err := repo.Approve(ctx, "InventoryItem", 3, actor); err != nil {
		t.Fatalf("approve v3: %v", err)
	}
	if err := repo.Publish(ctx, "InventoryItem", 3, actor); err != nil {
		t.Fatalf("publish v3: %v", err)
	}
	if err := repo.Rollback(ctx, "InventoryItem", 3, actor); err != nil {
		t.Fatalf("rollback v3: %v", err)
	}

	dryOut, dryErr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d", code)
	}
	if strings.Contains(dryOut, "data migration(s) now needed") {
		t.Errorf("the dry run invented a backfill obligation for a change that can never happen:\n%s", dryOut)
	}
	if !strings.Contains(dryErr, "rolled_back") {
		t.Errorf("the dry run must say the tenant is stuck, got stderr:\n%s", dryErr)
	}

	_, realErr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("real run: exit %d", code)
	}
	if !strings.Contains(realErr, "rolled_back") || !strings.Contains(realErr, "InventoryItem") {
		t.Errorf("a real run must name the type it cannot advance — otherwise a stuck tenant looks healthy:\n%s", realErr)
	}
	if v := publishedEntityVersions(t, tenantDB)["InventoryItem"]; v != 2 {
		t.Errorf("InventoryItem is at v%d; the rolled-back v3 must not have been published", v)
	}
}

// "Never grants a module" rested on foundation being universal. It was
// an assumption the code acted on without checking, and a tenant
// holding only a bundle-installed definition gained 22 foundation
// entity types from a sync (independent review).
func TestSync_DoesNotGrantFoundationToATenantWithoutIt(t *testing.T) {
	dsn, control, router := controlPlane(t)
	_, tenantDB := newTenant(t, control, router, "Sync Smoke Test Bundle Only")

	ctx := context.Background()
	repo := data.NewEntityDefinitionRepo(tenantDB)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "smoke-test-setup"}
	raw, err := json.Marshal(&entity.Definition{
		EntityType: "AcmeWidget", Version: 1, Module: "acme",
		Fields: []entity.Field{{Name: "name", Type: entity.FieldString, Required: true}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := repo.CreateDraft(ctx, "AcmeWidget", 1, raw, actor); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := repo.Approve(ctx, "AcmeWidget", 1, actor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := repo.Publish(ctx, "AcmeWidget", 1, actor); err != nil {
		t.Fatalf("publish: %v", err)
	}

	before := len(publishedEntityVersions(t, tenantDB))
	stdout, stderr, code := run(t, []string{"DATABASE_URL=" + dsn}, "-actor-id=smoke-test")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	after := len(publishedEntityVersions(t, tenantDB))
	if after != before {
		t.Errorf("the sync GRANTED %d entity types to a tenant with no foundation — ADR-0017 §2 says it never grants a module", after-before)
	}
	if !strings.Contains(stderr, "no foundation definitions published") {
		t.Errorf("the skip must say why, got stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "0 partially synced; 1 skipped.") {
		t.Errorf("expected the tenant counted as skipped, got:\n%s", stdout)
	}
}
