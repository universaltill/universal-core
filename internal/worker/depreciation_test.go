package worker

import (
	"context"
	"database/sql"
	"fmt"
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

// shouldPostDepreciation/finishDepreciationPost are pure logic, no
// database needed — same "test the throttle math directly on a
// hand-built Runner" approach this package already uses for
// Config.withDefaults() elsewhere.
//
// Every shouldPostDepreciation(tenantID, ...) == true in these tests is
// paired with a finishDepreciationPost(tenantID, false) call immediately
// after (clearThrottle=false: these tests are exercising the ordinary
// "run finished, throttle stays" path unless a test specifically means
// to check the clearThrottle=true path), matching tickTenant's own real,
// unconditional pairing (see worker.go) — shouldPostDepreciation now
// also claims an in-flight slot (see depreciationInFlight's own doc
// comment), so a test that omitted the matching finish call would see
// every later check for that tenant return false regardless of elapsed
// time, which is exactly the intended behavior for a run that's still
// actually in flight, not a bug in these tests forgetting to release it.
func TestShouldPostDepreciation_ThrottlesPerTenantIndependently(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Minute}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check for tenant-a should be due (never run before)")
	}
	r.finishDepreciationPost("tenant-a", false)
	if r.shouldPostDepreciation("tenant-a", now.Add(10*time.Second)) {
		t.Fatal("tenant-a should be throttled: only 10s elapsed of a 1m interval")
	}
	// A different tenant has its own throttle clock.
	if !r.shouldPostDepreciation("tenant-b", now.Add(10*time.Second)) {
		t.Fatal("tenant-b's first check should be due regardless of tenant-a's throttle state")
	}
	r.finishDepreciationPost("tenant-b", false)
	if !r.shouldPostDepreciation("tenant-a", now.Add(61*time.Second)) {
		t.Fatal("tenant-a should be due again once the interval has elapsed")
	}
	r.finishDepreciationPost("tenant-a", false)
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

// TestFinishDepreciationPost_ClearThrottleRearmsImmediately is the
// direct regression test for finishDepreciationPost's clearThrottle
// parameter — tickTenant's own error and truncated (uc-infra#183) paths
// both pass true, so the tenant becomes due again on the very next poll
// tick instead of waiting out the full DepreciationPostInterval; the
// ordinary finished-and-nothing-left-due path passes false, and the
// throttle must actually stay in place (its own pure-logic sibling,
// TestFinishDepreciationPost_NoClearThrottleLeavesThrottleInPlace below,
// covers that half directly — TestRunner_DepreciationBatchCap_
// DoesNotClearThrottleWhenNotTruncated further down this file is the
// real end-to-end version of the same thing, through tickTenant and a
// real assets.PostDueDepreciationBatch call).
func TestFinishDepreciationPost_ClearThrottleRearmsImmediately(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Minute}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check should be due")
	}
	r.finishDepreciationPost("tenant-a", true)
	if !r.shouldPostDepreciation("tenant-a", now.Add(time.Second)) {
		t.Fatal("after finishDepreciationPost(id, clearThrottle=true), the very next check should be due again, not throttled")
	}
	r.finishDepreciationPost("tenant-a", false)
}

// TestFinishDepreciationPost_NoClearThrottleLeavesThrottleInPlace is
// TestFinishDepreciationPost_ClearThrottleRearmsImmediately's other
// half: clearThrottle=false (the ordinary finished-call path) must
// leave the throttle timestamp in place, so a later check within the
// same interval is still refused.
func TestFinishDepreciationPost_NoClearThrottleLeavesThrottleInPlace(t *testing.T) {
	r := &Runner{cfg: Config{DepreciationPostInterval: time.Minute}.withDefaults()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !r.shouldPostDepreciation("tenant-a", now) {
		t.Fatal("first check should be due")
	}
	r.finishDepreciationPost("tenant-a", false)
	if r.shouldPostDepreciation("tenant-a", now.Add(time.Second)) {
		t.Fatal("after finishDepreciationPost(id, clearThrottle=false), the very next check (1s into a 1m interval) should still be throttled")
	}
	if !r.shouldPostDepreciation("tenant-a", now.Add(61*time.Second)) {
		t.Fatal("once the interval genuinely elapses, the tenant should be due again regardless")
	}
	r.finishDepreciationPost("tenant-a", false)
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
	r.finishDepreciationPost("tenant-a", false)
	// ...and only now can the next run start.
	if !r.shouldPostDepreciation("tenant-a", now.Add(10*time.Second)) {
		t.Fatal("once the in-flight run finished, a new run should be allowed (interval already elapsed)")
	}
	r.finishDepreciationPost("tenant-a", false)
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

	depreciationBacklogFixture(t, tenantDB, 1)

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

// depreciationBacklogFixture publishes foundation+finance+assets against
// tenantDB and creates one in_service FixedAsset with rowCount due
// DepreciationSchedule rows, all comfortably in the past — the minimal
// real backlog the tests below need to exercise tickTenant's real (not
// stubbed) call into assets.PostDueDepreciationBatch. Shared with
// TestRunner_PostsDueDepreciationEndToEnd above (rowCount=1) rather than
// each test hand-rolling its own copy of the same publish/Currency/
// Account/SyncGLAccounts/StatusType/Status/FixedAsset sequence.
func depreciationBacklogFixture(t *testing.T, tenantDB *sql.DB, rowCount int) (assetID string) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"finance", finance.Publish},
		{"assets", assets.Publish},
		{"assets forms", assets.PublishForms},
		{"assets statuses", assets.PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
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
	assetAcctID := mustAccount("1600", "Vehicles", "asset")
	expAcctID := mustAccount("6100", "Depreciation Expense", "expense")
	accumAcctID := mustAccount("1601", "Accumulated Depreciation - Vehicles", "asset")
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
		t.Fatalf("missing fixed_asset_status in_service")
	}

	asset, err := engine.Create(ctx, depreciationDef(t, tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-catchup", "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": float64(rowCount) * 1000.0, "salvage_value": 0.0,
		"useful_life_months": float64(rowCount), "depreciation_method": "straight_line",
		"currency_id":                         currency.ID,
		"asset_account_id":                    assetAcctID,
		"depreciation_expense_account_id":     expAcctID,
		"accumulated_depreciation_account_id": accumAcctID,
		"status_id":                           inServiceID,
	}, actor)
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	scheduleDef := depreciationDef(t, tenantDB, "DepreciationSchedule")
	for i := 0; i < rowCount; i++ {
		seq := i + 1
		if _, err := engine.Create(ctx, scheduleDef, map[string]any{
			"fixed_asset_id": asset.ID, "sequence": float64(seq),
			"period_end":          fmt.Sprintf("2020-%02d-28", seq), // all comfortably in the past — all due at once
			"depreciation_amount": 1000.0, "book_value": float64(rowCount-seq) * 1000.0,
		}, actor); err != nil {
			t.Fatalf("create DepreciationSchedule row %d: %v", seq, err)
		}
	}
	return asset.ID
}

// depreciationStuckBacklogFixture is depreciationBacklogFixture's
// permanently-unpostable sibling: rowCount due DepreciationSchedule rows
// on a FixedAsset whose account references are all non-empty (so they
// pass records.ListDueUnposted's own gate) but point at Account records
// that don't exist — the same "stays due-and-unposted forever" shape
// TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenDueRowsAre
// PermanentlyUnpostable (internal/kernel/assets/ledger_test.go) proves
// at the assets-package level. This is that same scenario's worker-level
// sibling: the real end-to-end proof that tickTenant doesn't hot-loop on
// it either.
func depreciationStuckBacklogFixture(t *testing.T, tenantDB *sql.DB, rowCount int) (assetID string) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"finance", finance.Publish},
		{"assets", assets.Publish},
		{"assets forms", assets.PublishForms},
		{"assets statuses", assets.PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
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
		t.Fatalf("missing fixed_asset_status in_service")
	}

	const noSuchAccount = "00000000-0000-0000-0000-000000000099" // well-formed UUID, no matching Account record
	asset, err := engine.Create(ctx, depreciationDef(t, tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-dangling-accounts", "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": float64(rowCount) * 1000.0, "salvage_value": 0.0,
		"useful_life_months": float64(rowCount), "depreciation_method": "straight_line",
		"currency_id":                         "", // never resolved when the accounts already fail first
		"asset_account_id":                    noSuchAccount,
		"depreciation_expense_account_id":     noSuchAccount,
		"accumulated_depreciation_account_id": noSuchAccount,
		"status_id":                           inServiceID,
	}, actor)
	if err != nil {
		t.Fatalf("create FixedAsset with dangling account refs: %v", err)
	}

	scheduleDef := depreciationDef(t, tenantDB, "DepreciationSchedule")
	for i := 0; i < rowCount; i++ {
		seq := i + 1
		if _, err := engine.Create(ctx, scheduleDef, map[string]any{
			"fixed_asset_id": asset.ID, "sequence": float64(seq),
			"period_end":          fmt.Sprintf("2020-%02d-28", seq),
			"depreciation_amount": 1000.0, "book_value": float64(rowCount-seq) * 1000.0,
		}, actor); err != nil {
			t.Fatalf("create DepreciationSchedule row %d: %v", seq, err)
		}
	}
	return asset.ID
}

// TestRunner_DepreciationBatchCap_ResumesWithinPollIntervalNotThrottleInterval
// is the worker-level regression test for uc-infra#183: a backlog
// spanning more than one assets.PostDueDepreciationBatch call must
// resume on the very next poll tick, not wait out the full
// DepreciationPostInterval throttle between capped calls.
// DepreciationPostInterval is set much longer than PollInterval, and the
// completion deadline sits comfortably under DepreciationPostInterval —
// specifically so a tickTenant that only cleared the throttle on error
// (the pre-fix behavior, which treats a truncated-but-not-finished call
// exactly like a finished one) would miss this deadline: finishing this
// 2-call backlog would then take at least one full
// DepreciationPostInterval, not roughly two PollIntervals.
func TestRunner_DepreciationBatchCap_ResumesWithinPollIntervalNotThrottleInterval(t *testing.T) {
	router, control := newTestRouter(t)
	_, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	const rowCount = 4
	const batchCap = 2 // 4 due rows / cap 2 = exactly 2 calls needed
	assetID := depreciationBacklogFixture(t, tenantDB, rowCount)

	cfg := fastTestConfig()
	cfg.DepreciationPostInterval = 1500 * time.Millisecond // long relative to PollInterval (20ms)
	cfg.DepreciationPostBatchSize = batchCap
	r := New(router, control, nil, cfg)

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	engine := crud.NewEngine(tenantDB)
	scheduleDef := depreciationDef(t, tenantDB, "DepreciationSchedule")
	allPosted := func() bool {
		rows, err := engine.ListByField(ctx, scheduleDef, "fixed_asset_id", assetID)
		if err != nil {
			t.Fatalf("ListByField DepreciationSchedule: %v", err)
		}
		if len(rows) != rowCount {
			t.Fatalf("expected %d schedule rows, found %d", rowCount, len(rows))
		}
		for _, row := range rows {
			if postedAt, _ := row.Data["posted_at"].(string); postedAt == "" {
				return false
			}
		}
		return true
	}

	start := time.Now()
	const waitBudget = 700 * time.Millisecond // well under DepreciationPostInterval
	for !allPosted() {
		if time.Since(start) > waitBudget {
			t.Fatalf("timed out after %s waiting for the %d-row backlog (cap %d) to finish posting — "+
				"well under DepreciationPostInterval=%s, so a fixed tickTenant should have resumed on "+
				"the very next poll tick after each capped (truncated) call",
				time.Since(start), rowCount, batchCap, cfg.DepreciationPostInterval)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// No double-posting across the capped calls.
	entries := data.NewJournalEntryRepo(tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != rowCount {
		t.Fatalf("journal entries = %d, want %d (one per schedule row, no double-posting across the capped calls)", len(list), rowCount)
	}
}

// TestRunner_DepreciationBatchCap_DoesNotClearThrottleWhenNotTruncated is
// TestRunner_DepreciationBatchCap_ResumesWithinPollIntervalNotThrottleInterval's
// necessary other half: a call that finishes its whole backlog in one go
// (truncated=false) must NOT get the same immediate-retry treatment as a
// truncated one — tickTenant must leave the throttle in place. Without
// this test, a tickTenant that called finishDepreciationPost(id, true)
// UNCONDITIONALLY (ignoring truncated entirely, not just mishandling it)
// would still pass every other test in this file, because with nothing
// left due, extra premature attempts are silently harmless no-ops from
// the outside — the only way to catch that mutation is to inspect the
// throttle's own internal state directly (this test is in-package for
// exactly that reason), independent review's finding on the first
// version of this change.
func TestRunner_DepreciationBatchCap_DoesNotClearThrottleWhenNotTruncated(t *testing.T) {
	router, control := newTestRouter(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	const rowCount = 1
	const batchCap = 2 // strictly more room than the backlog needs — the due-rows read can never land exactly on the cap (PostDueDepreciationBatch's own doc comment on why that exact-match case is deliberately excluded from this test)
	assetID := depreciationBacklogFixture(t, tenantDB, rowCount)

	cfg := fastTestConfig()
	cfg.DepreciationPostInterval = 2 * time.Second // long enough that a wrongly-cleared throttle is easy to observe
	cfg.DepreciationPostBatchSize = batchCap
	r := New(router, control, nil, cfg)

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	engine := crud.NewEngine(tenantDB)
	scheduleDef := depreciationDef(t, tenantDB, "DepreciationSchedule")
	allPosted := func() bool {
		rows, err := engine.ListByField(ctx, scheduleDef, "fixed_asset_id", assetID)
		if err != nil {
			t.Fatalf("ListByField DepreciationSchedule: %v", err)
		}
		if len(rows) != rowCount {
			t.Fatalf("expected %d schedule rows, found %d", rowCount, len(rows))
		}
		for _, row := range rows {
			if postedAt, _ := row.Data["posted_at"].(string); postedAt == "" {
				return false
			}
		}
		return true
	}

	start := time.Now()
	const waitBudget = 700 * time.Millisecond // well under DepreciationPostInterval
	for !allPosted() {
		if time.Since(start) > waitBudget {
			t.Fatalf("timed out after %s waiting for the %d-row backlog (cap %d) to post in its one, non-truncated call",
				time.Since(start), rowCount, batchCap)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// allPosted() reads posted_at via a completely separate query from
	// the tick that set it — posted_at for both rows can go true WHILE
	// that same PostDueDepreciationBatch call is still inside its own
	// healing sweep (the transition-to-fully_depreciated step, which
	// runs after the due-rows loop but before the call returns), i.e.
	// before tickTenant's own finishDepreciationPost has released
	// depreciationInFlight. Poll briefly for in-flight to clear first —
	// same "don't assume synchronous-with-the-observed-write ordering"
	// caution startRunner's own doc comment already gives for job status.
	inFlightCleared := func() bool {
		r.depMu.Lock()
		defer r.depMu.Unlock()
		return !r.depreciationInFlight[tenantID]
	}
	settleStart := time.Now()
	for !inFlightCleared() {
		if time.Since(settleStart) > 500*time.Millisecond {
			t.Fatalf("tenant %s still marked depreciationInFlight 500ms after its call's posted rows became visible — finishDepreciationPost should have released it unconditionally", tenantID)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// White-box check (same package, deliberately): a non-truncated
	// finishing call must leave the throttle's own bookkeeping intact —
	// finishDepreciationPost must have been called with clearThrottle=
	// false (which leaves this map entry alone). If it had cleared it,
	// this tenant would be immediately eligible for
	// another attempt on the very next poll tick despite having nothing
	// left due, exactly the premature-retry behavior this throttle
	// exists to prevent.
	r.depMu.Lock()
	last, hasEntry := r.lastDepreciationPost[tenantID]
	r.depMu.Unlock()
	if !hasEntry {
		t.Fatalf("no lastDepreciationPost entry for tenant %s after its one finishing (non-truncated) call — "+
			"the throttle was cleared as if the call had been truncated, which would make tickTenant retry "+
			"on the very next poll tick even though nothing is left due", tenantID)
	}
	if age := time.Since(last); age > cfg.DepreciationPostInterval {
		t.Errorf("lastDepreciationPost is %s old, older than DepreciationPostInterval itself — expected it to have just been set by the finishing call", age)
	}
}

// TestRunner_DepreciationBatchCap_DoesNotHotLoopOnPermanentlyUnpostableBacklog
// is the real end-to-end proof for independent review's MAJOR finding
// (internal/kernel/assets/ledger_test.go's
// TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenDueRowsAre
// PermanentlyUnpostable proves the same thing one layer down, at the
// assets-package function itself): a tenant whose entire due-rows window
// is permanently unpostable (dangling account references —
// depreciationStuckBacklogFixture) must NOT make tickTenant retry it on
// every poll tick forever.
//
// Deliberately NOT implemented as "sample lastDepreciationPost
// periodically and count how many distinct values appear" — an earlier
// version of this test did that and was itself unreliable: under real
// CPU/goroutine-scheduling jitter, a polling loop sampling every ~15ms
// can go starved for a few hundred ms at a time and simply never
// observe most of the intermediate values a fast hot loop produces,
// silently under-counting regardless of how badly the throttle is
// actually behaving (confirmed directly: instrumented logging showed
// dozens of real per-tick attempts in the same window this test's
// sampling loop only ever detected 3-5 of). Instead: capture the
// timestamp of the FIRST real attempt, sleep a short, fixed,
// deterministic window comfortably longer than several PollIntervals
// but far short of DepreciationPostInterval, then read the timestamp
// ONCE more. A correctly-throttled tenant cannot have been re-armed at
// all in that window (DepreciationPostInterval hasn't elapsed); a
// hot-looping one (truncated pinned true) will have been re-armed many
// times over, so the second read is unambiguously later than the first.
func TestRunner_DepreciationBatchCap_DoesNotHotLoopOnPermanentlyUnpostableBacklog(t *testing.T) {
	router, control := newTestRouter(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx, cancel := context.WithCancel(context.Background())

	const rowCount = 3
	const batchCap = 2 // < rowCount, so the due-rows read is exactly full every attempt
	depreciationStuckBacklogFixture(t, tenantDB, rowCount)

	cfg := fastTestConfig() // PollInterval: 20ms
	cfg.DepreciationPostInterval = 2 * time.Second
	cfg.DepreciationPostBatchSize = batchCap
	r := New(router, control, nil, cfg)

	stop := startRunner(r, ctx)
	defer func() { cancel(); stop() }()

	readThrottleTimestamp := func() (time.Time, bool) {
		r.depMu.Lock()
		defer r.depMu.Unlock()
		ts, ok := r.lastDepreciationPost[tenantID]
		return ts, ok
	}

	// Wait for the first real attempt.
	firstDeadline := time.Now().Add(2 * time.Second)
	var t1 time.Time
	for {
		if ts, ok := readThrottleTimestamp(); ok {
			t1 = ts
			break
		}
		if time.Now().After(firstDeadline) {
			t.Fatal("tenant was never attempted at all — the fixture or the poll loop is broken, not exercising the throttle this test means to check")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A window comfortably longer than several PollIntervals (20ms each)
	// but a small fraction of DepreciationPostInterval (2s) — a
	// correctly-throttled tenant must not be re-armed at all in this
	// window; a hot-looping one would be re-armed many times over.
	const settleWindow = 300 * time.Millisecond
	time.Sleep(settleWindow)

	t2, ok := readThrottleTimestamp()
	if !ok {
		t.Fatal("lastDepreciationPost entry for the tenant disappeared entirely — a run should be either in flight or just-finished with an entry present")
	}
	if t2.After(t1) {
		t.Fatalf("tenant %s was re-armed (t1=%s -> t2=%s, %s apart) within a %s window even though DepreciationPostInterval=%s hasn't come close to elapsing — "+
			"this means truncated is pinned true for a permanently-unpostable backlog, turning the throttle into a no-op instead of the intended once-per-interval retry",
			tenantID, t1, t2, t2.Sub(t1), settleWindow, cfg.DepreciationPostInterval)
	}

	// And, as ever, nothing was actually posted — every row's account
	// references are unresolvable.
	entries := data.NewJournalEntryRepo(tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("journal entries = %d, want 0 (every row's account refs are unresolvable)", len(list))
	}
}
