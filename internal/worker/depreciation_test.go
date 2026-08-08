package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// shouldPostDepreciation/finishDepreciationPost/forgetDepreciationPost
// are pure logic, no database needed — same "test the throttle math
// directly on a hand-built Runner" approach this package already uses
// for Config.withDefaults() elsewhere.
//
// Every shouldPostDepreciation(tenantID, ...) == true in these tests is
// paired with a finishDepreciationPost(tenantID) call immediately after,
// matching tickTenant's own real, unconditional pairing (see
// worker.go) — shouldPostDepreciation now also claims an in-flight slot
// (see depreciationInFlight's own doc comment), so a test that omitted
// the matching finish call would see every later check for that tenant
// return false regardless of elapsed time, which is exactly the
// intended behavior for a run that's still actually in flight, not a
// bug in these tests forgetting to release it.
func TestShouldPostDepreciation_ThrottlesPerTenantIndependently(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Minute}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check for tenant-a should be due (never run before)")
	}
	r.finishDepreciationPost("tenant-a")
	if r.shouldPostDepreciation("tenant-a", now.Add(10*time.Second)) {
		t.Fatal("tenant-a should be throttled: only 10s elapsed of a 1m interval")
	}
	// A different tenant has its own throttle clock.
	if !r.shouldPostDepreciation("tenant-b", now.Add(10*time.Second)) {
		t.Fatal("tenant-b's first check should be due regardless of tenant-a's throttle state")
	}
	r.finishDepreciationPost("tenant-b")
	if !r.shouldPostDepreciation("tenant-a", now.Add(61*time.Second)) {
		t.Fatal("tenant-a should be due again once the interval has elapsed")
	}
	r.finishDepreciationPost("tenant-a")
}

func TestShouldPostDepreciation_ZeroIntervalUsesDefault(t *testing.T) {
	r := &Runner{cfg: Config{}.withDefaults()}
	if r.cfg.DepreciationPostInterval != defaultDepreciationPostInterval {
		t.Fatalf("withDefaults() left DepreciationPostInterval = %v, want the %v default", r.cfg.DepreciationPostInterval, defaultDepreciationPostInterval)
	}
}

// TestWithDefaults_ZeroDepreciationPostBatchSizeUsesDefault is
// uc-infra#137/ADR-0025's direct regression test for the new batch-cap
// Config field defaulting the same way every other zero-value Config
// field here already does — same shape as
// TestShouldPostDepreciation_ZeroIntervalUsesDefault above.
func TestWithDefaults_ZeroDepreciationPostBatchSizeUsesDefault(t *testing.T) {
	r := &Runner{cfg: Config{}.withDefaults()}
	if r.cfg.DepreciationPostBatchSize != defaultDepreciationPostBatchSize {
		t.Fatalf("withDefaults() left DepreciationPostBatchSize = %d, want the %d default", r.cfg.DepreciationPostBatchSize, defaultDepreciationPostBatchSize)
	}
}

// TestWithDefaults_PositiveDepreciationPostBatchSizePreserved confirms
// an explicitly-configured cap is not clobbered by withDefaults() — the
// same "only zero/negative falls back" guarantee every other Config
// field's own default guard already gives.
func TestWithDefaults_PositiveDepreciationPostBatchSizePreserved(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostBatchSize: 5}.withDefaults()}
	if r.cfg.DepreciationPostBatchSize != 5 {
		t.Fatalf("withDefaults() overwrote an explicit DepreciationPostBatchSize: got %d, want 5", r.cfg.DepreciationPostBatchSize)
	}
}

func TestForgetDepreciationPost_ClearsThrottleForRetry(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Minute}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check should be due")
	}
	// forgetDepreciationPost alone (no finishDepreciationPost) is exactly
	// what tickTenant does on a run that ends in error — see worker.go.
	r.forgetDepreciationPost("tenant-a")
	if r.shouldPostDepreciation("tenant-a", now.Add(time.Second)) {
		t.Fatal("still in flight (finishDepreciationPost never called): forgetDepreciationPost alone must not be enough to become due again")
	}
	r.finishDepreciationPost("tenant-a")
	if !r.shouldPostDepreciation("tenant-a", now.Add(time.Second)) {
		t.Fatal("after both forgetDepreciationPost and finishDepreciationPost, the very next check should be due again, not throttled")
	}
	r.finishDepreciationPost("tenant-a")
}

// TestShouldPostDepreciation_InFlightGuard_BlocksAConcurrentSecondRun is
// the direct regression test for independent review's blocker: two
// RunConcurrent pollers (Config.Concurrency, default 2) must never both
// be allowed to call assets.PostDueDepreciation for the SAME tenant at
// once, even if the run is slow enough that DepreciationPostInterval
// elapses while it's still in progress — an overlapping run is what
// reproduced a permanently-stuck in_service asset (two runs racing to
// post the same rows and transition the same asset).
func TestShouldPostDepreciation_InFlightGuard_BlocksAConcurrentSecondRun(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Second}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check should be due")
	}
	// A slow run that hasn't called finishDepreciationPost yet — even
	// well past the (short, 1s) interval, a concurrent poller's check
	// for the SAME tenant must still be refused.
	if r.shouldPostDepreciation("tenant-a", now.Add(10*time.Second)) {
		t.Fatal("a concurrent poller must not be able to start a second run for a tenant whose run is still in flight")
	}
	// The first run finishes...
	r.finishDepreciationPost("tenant-a")
	// ...and only now can the next run start.
	if !r.shouldPostDepreciation("tenant-a", now.Add(10*time.Second)) {
		t.Fatal("once the in-flight run finished, a new run should be allowed (interval already elapsed)")
	}
	r.finishDepreciationPost("tenant-a")
}

// depreciationDef resolves a published entity Definition — same helper
// shape as sales/purchasing/assets' own test-only defFor/publishedDef,
// duplicated here (unexported, package-private) rather than shared,
// matching this repo's established per-package convention for small
// test helpers.
func depreciationDef(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

// TestRunner_PostsDueDepreciationEndToEnd proves tickTenant actually
// calls assets.PostDueDepreciation (uc-infra#76, ADR-0022) against a
// real tenant provisioned the same way cmd/provision-tenant does —
// internal/kernel/assets/ledger_test.go already proves
// PostDueDepreciation's own logic exhaustively; this test proves the
// WIRING, the same division of labor
// TestRunner_FiresScheduledWorkflowEndToEnd already uses for the
// schedule-firing path.
func TestRunner_PostsDueDepreciationEndToEnd(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	actor := humanActor()
	for _, fn := range []func(context.Context, *sql.DB, audit.Actor) error{
		foundation.Publish, finance.Publish, assets.Publish, assets.PublishForms, assets.PublishStatuses,
	} {
		if err := fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	currency, err := engine.Create(ctx, depreciationDef(t, tenantDB, "Currency"), map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": float64(2),
	}, actor)
	if err != nil {
		t.Fatalf("create Currency: %v", err)
	}
	accountDef := depreciationDef(t, tenantDB, "Account")
	mustAccount := func(code, name, accountType string) string {
		t.Helper()
		rec, err := engine.Create(ctx, accountDef, map[string]any{
			"code": code, "name": name, "type": accountType,
			"currency_id": currency.ID, "is_active": true,
		}, actor)
		if err != nil {
			t.Fatalf("create Account %s: %v", code, err)
		}
		return rec.ID
	}
	expAcctID := mustAccount("6100", "Depreciation Expense", "expense")
	accumAcctID := mustAccount("1601", "Accumulated Depreciation", "asset")
	assetAcctID := mustAccount("1600", "Vehicles", "asset")
	if err := finance.SyncGLAccounts(ctx, tenantDB, actor); err != nil {
		t.Fatalf("SyncGLAccounts: %v", err)
	}

	statusTypes, err := engine.ListByField(ctx, depreciationDef(t, tenantDB, "StatusType"), "code", "fixed_asset_status")
	if err != nil || len(statusTypes) != 1 {
		t.Fatalf("fixed_asset_status StatusType: err=%v n=%d", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, depreciationDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var inServiceID string
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == "in_service" {
			inServiceID = s.ID
		}
	}
	if inServiceID == "" {
		t.Fatal("missing in_service status")
	}

	asset, err := engine.Create(ctx, depreciationDef(t, tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-1", "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": 1000.0, "salvage_value": 0.0,
		"useful_life_months": 12.0, "depreciation_method": "straight_line",
		"currency_id":                         currency.ID,
		"asset_account_id":                    assetAcctID,
		"depreciation_expense_account_id":     expAcctID,
		"accumulated_depreciation_account_id": accumAcctID,
		"status_id":                           inServiceID,
	}, actor)
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	if _, err := engine.Create(ctx, depreciationDef(t, tenantDB, "DepreciationSchedule"), map[string]any{
		"fixed_asset_id": asset.ID, "sequence": float64(1),
		"period_end": "2020-01-31", "depreciation_amount": 100.00, "book_value": 900.00,
	}, actor); err != nil {
		t.Fatalf("create DepreciationSchedule row: %v", err)
	}

	cfg := fastTestConfig()
	cfg.DepreciationPostInterval = 20 * time.Millisecond
	r := New(router, control, nil, cfg)
	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	entries := data.NewJournalEntryRepo(tenantDB)
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := entries.List(ctx)
		if err != nil {
			t.Fatalf("List journal entries: %v", err)
		}
		if len(list) == 1 {
			if list[0].SourceType != "DepreciationSchedule" {
				t.Fatalf("unexpected journal entry source: %+v", list[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker never posted the due depreciation row — PostDueDepreciation is not wired into tickTenant (found %d entries)", len(list))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
