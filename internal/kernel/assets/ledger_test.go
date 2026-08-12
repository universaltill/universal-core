package assets

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// depreciationFixture is a tenant with Assets + Finance published, one
// USD Currency, three GL accounts already synced (so ledger.PostTx can
// resolve them), and every fixed_asset_status id resolved by code —
// everything PostDueDepreciation's tests need to build a FixedAsset and
// its DepreciationSchedule rows directly, the same way
// TestFixedAsset_UsableEndToEnd (seed_test.go) already does for Publish
// itself.
type depreciationFixture struct {
	tenantDB   *sql.DB
	engine     *crud.Engine
	currencyID string
	assetAcct  string // asset_account_id — "1600"
	expAcct    string // depreciation_expense_account_id — "6100"
	accumAcct  string // accumulated_depreciation_account_id — "1601"
	statusID   map[string]string
}

func setUpDepreciationFixture(t *testing.T) depreciationFixture {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func(context.Context, *sql.DB, audit.Actor) error
	}{
		{"foundation", foundation.Publish},
		{"finance", finance.Publish},
		{"assets", Publish},
		{"assets forms", PublishForms},
		{"assets statuses", PublishStatuses},
	} {
		if err := step.fn(ctx, tenantDB, actor); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)

	currency, err := engine.Create(ctx, publishedDef(t, tenantDB, "Currency"), map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": float64(2),
	}, actor)
	if err != nil {
		t.Fatalf("create Currency: %v", err)
	}

	accountDef := publishedDef(t, tenantDB, "Account")
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

	statusTypes, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "StatusType"), "code", "fixed_asset_status")
	if err != nil || len(statusTypes) != 1 {
		t.Fatalf("fixed_asset_status StatusType: err=%v n=%d", err, len(statusTypes))
	}
	statuses, err := engine.ListByField(ctx, publishedDef(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	statusID := map[string]string{}
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code != "" {
			statusID[code] = s.ID
		}
	}
	for _, code := range []string{"draft", "in_service", "disposed", "written_off", "fully_depreciated"} {
		if statusID[code] == "" {
			t.Fatalf("missing fixed_asset_status %q", code)
		}
	}

	return depreciationFixture{
		tenantDB: tenantDB, engine: engine, currencyID: currency.ID,
		assetAcct: assetAcctID, expAcct: expAcctID, accumAcct: accumAcctID,
		statusID: statusID,
	}
}

// createAsset creates a FixedAsset with every account reference wired
// (fx.assetAcct/expAcct/accumAcct) unless withMissingAccounts is true,
// in which case depreciation_expense_account_id is left empty — the
// "misconfigured asset" case PostDueDepreciation must skip, not fail on.
// useful_life_months is always 3 — tests that need a schedule
// deliberately entered for FEWER than the asset's full life (the
// partial-schedule / "not actually finished" cases) create fewer than 3
// DepreciationSchedule rows against this same 3-month life, rather than
// varying the life itself.
func (fx depreciationFixture) createAsset(t *testing.T, statusCode string, withMissingAccounts bool) string {
	t.Helper()
	return fx.createAssetNumbered(t, "FA-"+statusCode, statusCode, withMissingAccounts)
}

// createAssetNumbered is createAsset with an explicit asset_number, for
// tests that create more than one asset in the same status (createAsset
// alone would give them all the identical "FA-<statusCode>" number).
func (fx depreciationFixture) createAssetNumbered(t *testing.T, assetNumber, statusCode string, withMissingAccounts bool) string {
	t.Helper()
	fields := map[string]any{
		"asset_number": assetNumber, "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID[statusCode],
	}
	if withMissingAccounts {
		delete(fields, "depreciation_expense_account_id")
	}
	rec, err := fx.engine.Create(context.Background(), publishedDef(t, fx.tenantDB, "FixedAsset"), fields, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	return rec.ID
}

// createAssetWithLife is createAssetNumbered's "in_service, fully wired"
// case with an explicit useful_life_months, for the one class of test
// createAssetNumbered's own fixed 3-month life can't express: a schedule
// long enough to span more than one PostDueDepreciationBatch call
// (TestPostDueDepreciationBatch_* below).
func (fx depreciationFixture) createAssetWithLife(t *testing.T, assetNumber string, usefulLifeMonths float64) string {
	t.Helper()
	rec, err := fx.engine.Create(context.Background(), publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": assetNumber, "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": 6000.0, "salvage_value": 0.0,
		"useful_life_months": usefulLifeMonths, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset %s: %v", assetNumber, err)
	}
	return rec.ID
}

// auditCount returns how many audit_log rows exist for entityType/id
// matching action and actorType — same raw-query pattern
// internal/kernel/ledger/ledger_test.go and
// internal/kernel/purchasing/ledger_test.go already use to assert audit
// rows directly rather than trust that a write "must have" produced one.
func (fx depreciationFixture) auditCount(t *testing.T, entityType, id, action, actorType string) int {
	t.Helper()
	var n int
	if err := fx.tenantDB.QueryRow(
		`SELECT count(*) FROM audit_log WHERE entity_type = $1 AND record_id = $2 AND action = $3 AND actor_type = $4`,
		entityType, id, action, actorType,
	).Scan(&n); err != nil {
		t.Fatalf("count audit_log for %s %s: %v", entityType, id, err)
	}
	return n
}

// createScheduleRow creates one DepreciationSchedule row directly (not
// through depreciation.Build — these tests exercise PostDueDepreciation's
// own branching, not the schedule arithmetic, which depreciation_test.go
// already covers exhaustively). amount/bookValue are plain currency
// units (2dp), matching how every other FieldNumber money value in this
// kernel is stored.
func (fx depreciationFixture) createScheduleRow(t *testing.T, assetID string, sequence int, periodEnd string, amount, bookValue float64) string {
	t.Helper()
	rec, err := fx.engine.Create(context.Background(), publishedDef(t, fx.tenantDB, "DepreciationSchedule"), map[string]any{
		"fixed_asset_id": assetID, "sequence": float64(sequence),
		"period_end": periodEnd, "depreciation_amount": amount, "book_value": bookValue,
	}, humanActor())
	if err != nil {
		t.Fatalf("create DepreciationSchedule row %d: %v", sequence, err)
	}
	return rec.ID
}

func (fx depreciationFixture) statusCode(t *testing.T, entityType, id string) string {
	t.Helper()
	rec, err := fx.engine.Get(context.Background(), publishedDef(t, fx.tenantDB, entityType), id)
	if err != nil {
		t.Fatalf("Get %s %s: %v", entityType, id, err)
	}
	statusID, _ := rec.Data["status_id"].(string)
	if statusID == "" {
		t.Fatalf("%s %s has no status_id", entityType, id)
	}
	status, err := fx.engine.Get(context.Background(), publishedDef(t, fx.tenantDB, "Status"), statusID)
	if err != nil {
		t.Fatalf("Get Status %s: %v", statusID, err)
	}
	code, _ := status.Data["code"].(string)
	return code
}

func TestPostDueDepreciation_PostsDueRowsAndLeavesFutureRowsUntouched(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false)
	dueID := fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)
	futureID := fx.createScheduleRow(t, assetID, 2, "2099-02-29", 1000.00, 1000.00)

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry, got %d", len(list))
	}
	entry := list[0]
	if entry.SourceType != "DepreciationSchedule" || entry.SourceID != dueID {
		t.Fatalf("unexpected source: type=%s id=%s", entry.SourceType, entry.SourceID)
	}
	byCode := map[string]data.JournalLine{}
	for _, l := range entry.Lines {
		byCode[l.AccountCode] = l
	}
	wantMinor := int64(1000 * 100)
	if exp := byCode["6100"]; exp.DebitMinor != wantMinor {
		t.Errorf("expected expense (6100) debit %d, got %+v", wantMinor, exp)
	}
	if accum := byCode["1601"]; accum.CreditMinor != wantMinor {
		t.Errorf("expected accumulated depreciation (1601) credit %d, got %+v", wantMinor, accum)
	}

	dueRow, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), dueID)
	if err != nil {
		t.Fatalf("Get due row: %v", err)
	}
	if postedAt, _ := dueRow.Data["posted_at"].(string); postedAt != "2020-01-31" {
		t.Errorf("due row posted_at = %q, want 2020-01-31", postedAt)
	}
	futureRow, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), futureID)
	if err != nil {
		t.Fatalf("Get future row: %v", err)
	}
	if postedAt, _ := futureRow.Data["posted_at"].(string); postedAt != "" {
		t.Errorf("future row posted_at = %q, want empty (not due yet)", postedAt)
	}

	// Schedule not exhausted (one row still unposted) — asset must stay
	// in_service.
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
		t.Errorf("asset status = %q, want in_service (schedule not exhausted)", code)
	}
}

func TestPostDueDepreciation_Idempotent_RerunDoesNotDoublePost(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false)
	fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)

	if _, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor()); err != nil {
		t.Fatalf("first PostDueDepreciation: %v", err)
	}
	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("second PostDueDepreciation: %v", err)
	}
	if posted != 0 {
		t.Errorf("second run posted = %d, want 0 (already posted)", posted)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry after two runs, got %d", len(list))
	}
}

func TestPostDueDepreciation_SkipsAssetNotInService(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	for _, statusCode := range []string{"draft", "disposed", "written_off"} {
		assetID := fx.createAsset(t, statusCode, false)
		fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)

		posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
		if err != nil {
			t.Fatalf("PostDueDepreciation (asset status %s): %v", statusCode, err)
		}
		if posted != 0 {
			t.Errorf("asset in status %q: posted = %d, want 0", statusCode, posted)
		}
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no journal entries for non-in_service assets, got %d", len(list))
	}
}

// TestPostDueDepreciation_MissingAccountRefs_SkipsThatAssetOnly is the
// per-asset failure-isolation guarantee: one misconfigured asset must
// not prevent every OTHER asset's due depreciation from posting in the
// same run.
func TestPostDueDepreciation_MissingAccountRefs_SkipsThatAssetOnly(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	badAssetID := fx.createAssetNumbered(t, "FA-bad", "in_service", true)
	fx.createScheduleRow(t, badAssetID, 1, "2020-01-31", 1000.00, 2000.00)

	goodAssetID := fx.createAssetNumbered(t, "FA-good", "in_service", false)
	goodRowID := fx.createScheduleRow(t, goodAssetID, 1, "2020-01-31", 500.00, 1000.00)

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1 (only the well-configured asset)", posted)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].SourceID != goodRowID {
		t.Fatalf("expected exactly 1 journal entry for the good asset's row, got %+v", list)
	}

	badRow, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"),
		func() string {
			rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", badAssetID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("list bad asset's schedule rows: err=%v n=%d", err, len(rows))
			}
			return rows[0].ID
		}(),
	)
	if err != nil {
		t.Fatalf("Get bad asset's row: %v", err)
	}
	if postedAt, _ := badRow.Data["posted_at"].(string); postedAt != "" {
		t.Errorf("bad asset's row posted_at = %q, want empty (never posted)", postedAt)
	}
}

// TestPostDueDepreciation_LastRowPosted_TransitionsToFullyDepreciated is
// the schedule-exhaustion case: once every schedule row an asset has is
// posted, the asset itself must advance out of in_service so it stops
// reading as "still depreciating."
func TestPostDueDepreciation_LastRowPosted_TransitionsToFullyDepreciated(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false)
	fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)
	fx.createScheduleRow(t, assetID, 2, "2020-02-29", 1000.00, 1000.00)
	fx.createScheduleRow(t, assetID, 3, "2020-03-31", 1000.00, 0.00)

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 3 {
		t.Fatalf("posted = %d, want 3", posted)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 journal entries, got %d", len(list))
	}

	if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
		t.Errorf("asset status = %q, want fully_depreciated once every row is posted", code)
	}

	// Rerun must be a true no-op: nothing left due, nothing left to
	// transition (already fully_depreciated, so even a due row appearing
	// later — e.g. a data fix — would correctly be skipped as
	// not-in_service rather than silently reactivating the asset).
	posted, err = PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("rerun PostDueDepreciation: %v", err)
	}
	if posted != 0 {
		t.Errorf("rerun posted = %d, want 0", posted)
	}
}

// TestPostDueDepreciation_NoSchedules_ReturnsZeroNoError is the
// unlicensed-tenant path: a tenant that never published Assets (or one
// that did but has no schedules yet) must not error — records.List
// returning an empty slice for an entity type nothing ever wrote is the
// ordinary case, not a fault.
func TestPostDueDepreciation_NoSchedules_ReturnsZeroNoError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	// Deliberately NOT publishing assets — the "module not licensed"
	// case.
	posted, err := PostDueDepreciation(ctx, tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation on a tenant without Assets published: %v", err)
	}
	if posted != 0 {
		t.Errorf("posted = %d, want 0", posted)
	}
}

// TestPostDueDepreciation_ZeroAmountRow_MarksPostedWithoutAJournalEntry
// guards the defensive zero-amount branch the same way
// PostCustomerInvoiceToLedger's own zero-total test guards its — even
// though depreciation.Build's own invariants mean a real schedule should
// never produce one.
func TestPostDueDepreciation_ZeroAmountRow_MarksPostedWithoutAJournalEntry(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false)
	zeroID := fx.createScheduleRow(t, assetID, 1, "2020-01-31", 0.00, 3000.00)

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1 (marked posted even though nothing was journaled)", posted)
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no journal entry for a zero-amount row, got %d", len(list))
	}
	row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), zeroID)
	if err != nil {
		t.Fatalf("Get row: %v", err)
	}
	if postedAt, _ := row.Data["posted_at"].(string); postedAt != "2020-01-31" {
		t.Errorf("posted_at = %q, want 2020-01-31 (still marked posted)", postedAt)
	}
}

// TestPostDueDepreciation_PartialSchedule_DoesNotTransitionUntilUsefulLifeIsFullyPosted
// is the direct regression test for independent review's finding: a
// FixedAsset with useful_life_months=3 but only ONE DepreciationSchedule
// row actually entered so far — exactly cmd/seed-demo-data's own
// deliberate pattern (it seeds only the first 12 of a 60-period
// schedule "to avoid burying the rest of the demo data"). Posting that
// one due row must NOT read "no unposted rows currently exist" as "this
// asset's life is complete": the asset must stay in_service, with 2
// periods of life still un-entered, not flip to fully_depreciated with
// 2/3 of its cost never depreciated and no way back (the status graph
// has no fully_depreciated -> in_service edge).
func TestPostDueDepreciation_PartialSchedule_DoesNotTransitionUntilUsefulLifeIsFullyPosted(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false) // useful_life_months: 3
	fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)
	// Deliberately NOT creating periods 2 and 3 yet — the partial-schedule
	// state.

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
		t.Fatalf("asset status = %q, want in_service — only 1 of 3 useful_life_months periods has ever been entered, let alone posted", code)
	}

	// Entering and posting the remaining two periods must still complete
	// it correctly.
	fx.createScheduleRow(t, assetID, 2, "2020-02-29", 1000.00, 1000.00)
	fx.createScheduleRow(t, assetID, 3, "2020-03-31", 1000.00, 0.00)
	if _, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor()); err != nil {
		t.Fatalf("second PostDueDepreciation: %v", err)
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
		t.Errorf("asset status = %q, want fully_depreciated once all 3 periods are entered and posted", code)
	}
}

// TestPostDueDepreciation_AlreadyFullyPostedButNotTransitioned_HealsOnNextRun
// is the direct regression test for independent review's other blocker:
// the completion check must not be gated on "did THIS call post the
// exhausting row" — a fire-once-or-never condition means an asset that
// somehow ends up with every row posted but the status transition
// itself never happened (a crashed process between the last row's
// commit and the transition, a version conflict, an overlapping second
// poller) is stuck in_service forever, since no LATER run would ever
// see posted > 0 again for it. Simulating that stuck state directly
// (marking every row posted without ever calling PostDueDepreciation, so
// this test doesn't depend on how such a state could arise) and
// confirming a run that posts NOTHING new still transitions it proves
// the check is retry-safe, not merely a repeat of the exhaustion test
// above.
func TestPostDueDepreciation_AlreadyFullyPostedButNotTransitioned_HealsOnNextRun(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false) // useful_life_months: 3
	scheduleDef := publishedDef(t, fx.tenantDB, "DepreciationSchedule")
	for seq, periodEnd := range map[int]string{1: "2020-01-31", 2: "2020-02-29", 3: "2020-03-31"} {
		id := fx.createScheduleRow(t, assetID, seq, periodEnd, 1000.00, 0.00)
		rec, err := fx.engine.Get(ctx, scheduleDef, id)
		if err != nil {
			t.Fatalf("Get row %d: %v", seq, err)
		}
		fields := map[string]any{}
		for k, v := range rec.Data {
			fields[k] = v
		}
		fields["posted_at"] = periodEnd
		version := rec.Version
		if _, err := fx.engine.Update(ctx, scheduleDef, id, fields, &version, humanActor()); err != nil {
			t.Fatalf("directly mark row %d posted: %v", seq, err)
		}
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
		t.Fatalf("setup: asset should still be in_service (never ran PostDueDepreciation), got %q", code)
	}

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 0 {
		t.Fatalf("posted = %d, want 0 — every row was already marked posted before this call", posted)
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
		t.Errorf("asset status = %q, want fully_depreciated — a call that posts nothing new must still complete the transition once the schedule is actually exhausted", code)
	}
}

// TestPostDueDepreciation_IdempotentRerun_AssetStaysInService is
// TestPostDueDepreciation_Idempotent_RerunDoesNotDoublePost's sibling,
// but scoped so the asset does NOT reach fully_depreciated on the first
// run (2 of 3 useful_life_months periods entered) — independent review's
// finding that the original idempotent-rerun test's asset always
// transitioned on the first run, so its rerun short-circuited at the
// !inService check and never actually reached either the posted_at
// pre-filter or the ExistsForSource guard inside postDepreciationRow.
// This test's rerun does reach them.
func TestPostDueDepreciation_IdempotentRerun_AssetStaysInService(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	assetID := fx.createAsset(t, "in_service", false) // useful_life_months: 3
	fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 2000.00)
	fx.createScheduleRow(t, assetID, 2, "2020-02-29", 1000.00, 1000.00)
	// period 3 deliberately not entered — asset must stay in_service.

	first, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("first PostDueDepreciation: %v", err)
	}
	if first != 2 {
		t.Fatalf("first run posted = %d, want 2", first)
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
		t.Fatalf("asset status = %q, want in_service (only 2 of 3 periods entered)", code)
	}

	second, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("second PostDueDepreciation: %v", err)
	}
	if second != 0 {
		t.Errorf("second run posted = %d, want 0 — both due rows were already posted, and the asset staying in_service means this run actually reached the idempotency guards instead of short-circuiting on status", second)
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected exactly 2 journal entries after two runs, got %d", len(list))
	}
}

// TestPostDueDepreciation_WritesAuditRows confirms all three mutating
// write paths (DepreciationSchedule.posted_at via a real posting,
// DepreciationSchedule.posted_at via the zero-amount markPosted path,
// and FixedAsset.status_id's transition) each write a real audit_log
// row with the system actor (ADR-0008) — not just that the code that
// builds and inserts them compiles and runs without erroring, which a
// green suite with no audit assertion at all cannot distinguish from
// "the insert silently no-ops."
func TestPostDueDepreciation_WritesAuditRows(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	// Real posting path.
	postedAssetID := fx.createAssetNumbered(t, "FA-posted", "in_service", false)
	postedRowID := fx.createScheduleRow(t, postedAssetID, 1, "2020-01-31", 1000.00, 2000.00)

	// Zero-amount markPosted path.
	zeroAssetID := fx.createAssetNumbered(t, "FA-zero", "in_service", false)
	zeroRowID := fx.createScheduleRow(t, zeroAssetID, 1, "2020-01-31", 0.00, 3000.00)

	// Full-life asset, to exercise the FixedAsset status-transition
	// write too.
	lifeAssetID := fx.createAssetNumbered(t, "FA-life", "in_service", false)
	fx.createScheduleRow(t, lifeAssetID, 1, "2020-01-31", 1000.00, 2000.00)
	fx.createScheduleRow(t, lifeAssetID, 2, "2020-02-29", 1000.00, 1000.00)
	fx.createScheduleRow(t, lifeAssetID, 3, "2020-03-31", 1000.00, 0.00)

	if _, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor()); err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}

	if n := fx.auditCount(t, "DepreciationSchedule", postedRowID, "update", "system"); n != 1 {
		t.Errorf("audit_log rows for the posted DepreciationSchedule row = %d, want 1", n)
	}
	if n := fx.auditCount(t, "DepreciationSchedule", zeroRowID, "update", "system"); n != 1 {
		t.Errorf("audit_log rows for the zero-amount DepreciationSchedule row = %d, want 1", n)
	}
	if n := fx.auditCount(t, "FixedAsset", lifeAssetID, "update", "system"); n != 1 {
		t.Errorf("audit_log rows for the FixedAsset status transition = %d, want 1", n)
	}
	if code := fx.statusCode(t, "FixedAsset", lifeAssetID); code != "fully_depreciated" {
		t.Fatalf("setup check: expected lifeAssetID to have transitioned, got %q", code)
	}
}

// TestPostDueDepreciation_MixedInServiceAndNotInService_OneRun proves
// per-asset status gating holds when both kinds of asset have due rows
// in the SAME tenant run, not just across separate runs the way
// TestPostDueDepreciation_SkipsAssetNotInService exercises it.
func TestPostDueDepreciation_MixedInServiceAndNotInService_OneRun(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	liveID := fx.createAssetNumbered(t, "FA-live", "in_service", false)
	liveRowID := fx.createScheduleRow(t, liveID, 1, "2020-01-31", 1000.00, 2000.00)

	disposedID := fx.createAssetNumbered(t, "FA-disposed", "disposed", false)
	fx.createScheduleRow(t, disposedID, 1, "2020-01-31", 1000.00, 2000.00)

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1 (only the in_service asset)", posted)
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].SourceID != liveRowID {
		t.Fatalf("expected exactly 1 journal entry, for the in_service asset's row, got %+v", list)
	}
}

// TestCurrencyMinorUnit_ResolvesNonDefaultScale is the direct regression
// test for independent review's finding that every other test's fixture
// uses a Currency with minor_unit=2 — identical to
// defaultCurrencyMinorUnit — so nothing distinguishes "currencyMinorUnit
// resolved the real value" from "the fallback silently applied and got
// lucky." This one uses 0 (JPY-style, no decimal places): a 1000.00
// depreciation_amount must convert to 1000 minor units, not 100000 (what
// the 2dp fallback would wrongly produce).
func TestCurrencyMinorUnit_ResolvesNonDefaultScale(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	actor := humanActor()

	jpy, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "Currency"), map[string]any{
		"code": "JPY", "name": "Japanese Yen", "minor_unit": float64(0),
	}, actor)
	if err != nil {
		t.Fatalf("create JPY Currency: %v", err)
	}
	fields := map[string]any{
		"asset_number": "FA-jpy", "name": map[string]any{"en": "Test Asset (JPY)"},
		"acquisition_date": "2020-01-01", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         jpy.ID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	asset, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), fields, actor)
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	fx.createScheduleRow(t, asset.ID, 1, "2020-01-31", 1000.00, 2000.00)

	if _, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor()); err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 journal entry, got %d", len(list))
	}
	for _, l := range list[0].Lines {
		if l.DebitMinor != 0 && l.DebitMinor != 1000 {
			t.Errorf("debit line = %d minor units, want 1000 (0dp JPY scale, not the 2dp fallback's 100000)", l.DebitMinor)
		}
		if l.CreditMinor != 0 && l.CreditMinor != 1000 {
			t.Errorf("credit line = %d minor units, want 1000 (0dp JPY scale, not the 2dp fallback's 100000)", l.CreditMinor)
		}
	}
}

// TestStatusIDByCodeTx_NotSeeded is a direct unit test of the
// not-seeded error path — reachable in practice when
// assets.PublishStatuses hasn't run for a tenant (or ran against a
// different status_type_id than the one passed in), not something the
// guarded engine's Required-field validation would prevent the way an
// empty status_id/fixed_asset_id/Account.code would (those three are
// all Required at the Definition level, so they're unreachable through
// the normal product write path and are left as documented,
// intentionally-deferred defensive code rather than tested here).
func TestStatusIDByCodeTx_NotSeeded(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	tx, err := fx.tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // test cleanup

	records := data.NewRecordRepo(fx.tenantDB)
	_, err = statusIDByCodeTx(ctx, tx, records, "00000000-0000-0000-0000-000000000000", statusCodeFullyDepreciated)
	if err == nil {
		t.Fatal("expected an error for a status type nothing was ever seeded against")
	}
	if !strings.Contains(err.Error(), "not seeded") {
		t.Errorf("error should say the status wasn't seeded, got: %v", err)
	}
}

// TestPostDueDepreciationBatch_CapsPerCallAndResumesAcrossCalls is the
// direct regression test for uc-infra#137/ADR-0025: a backlog bigger
// than one call's cap must post only up to the cap per call, must never
// flip the asset to fully_depreciated before its actual last row posts
// (a premature transition here would be silent, unrecoverable data
// corruption — the status graph has no fully_depreciated -> in_service
// edge, per transitionToFullyDepreciated's own doc comment), and must
// finish posting everything once enough calls have run — proving both
// the cap (acceptance criteria 2/5) and partial-batch correctness
// (acceptance criterion 3) in one realistic resumable-catch-up scenario.
func TestPostDueDepreciationBatch_CapsPerCallAndResumesAcrossCalls(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	const usefulLifeMonths = 6
	assetID := fx.createAssetWithLife(t, "FA-catchup", usefulLifeMonths)
	rowIDs := make([]string, usefulLifeMonths)
	for i := range usefulLifeMonths {
		seq := i + 1
		periodEnd := fmt.Sprintf("2020-%02d-28", seq) // all comfortably in the past — all due at once
		rowIDs[i] = fx.createScheduleRow(t, assetID, seq, periodEnd, 1000.00, float64(usefulLifeMonths-seq)*1000.00)
	}

	const cap = 2
	wantPerCall := []int{cap, cap, cap} // 6 due rows / cap 2 = exactly 3 calls
	for i, want := range wantPerCall {
		posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
		if err != nil {
			t.Fatalf("call %d: PostDueDepreciationBatch: %v", i+1, err)
		}
		if posted != want {
			t.Fatalf("call %d: posted = %d, want %d", i+1, posted, want)
		}
		// uc-infra#183: every one of these 3 calls reads exactly `cap`
		// due rows (6 rows / cap 2), so each reports truncated=true, even
		// the 3rd — the backlog landing exactly on a multiple of cap is
		// the one explicitly-accepted imprecision (see
		// PostDueDepreciationBatch's own doc comment); the "final call
		// has nothing left to do" check below is what proves it's
		// harmless (one extra attempt, not a correctness problem).
		if !truncated {
			t.Errorf("call %d: truncated = false, want true (this call's own read hit the cap)", i+1)
		}

		wantStatus := "in_service"
		if i == len(wantPerCall)-1 {
			wantStatus = "fully_depreciated"
		}
		if code := fx.statusCode(t, "FixedAsset", assetID); code != wantStatus {
			t.Fatalf("call %d: asset status = %q, want %q", i+1, code, wantStatus)
		}
	}

	// Every row now posted, in order — the batch cap must not have
	// skipped or reordered any of them.
	for i, rowID := range rowIDs {
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row %d: %v", i+1, err)
		}
		if postedAt, _ := row.Data["posted_at"].(string); postedAt == "" {
			t.Errorf("row %d (sequence %d) still unposted after enough calls to cover its whole schedule", i, i+1)
		}
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != usefulLifeMonths {
		t.Fatalf("journal entries = %d, want %d (one per schedule row, no double-posting across the capped calls)", len(list), usefulLifeMonths)
	}

	// A further call has nothing left to do — and, uc-infra#183, must
	// report truncated=false: this is what makes the 3rd call's own
	// truncated=true above just one wasted extra attempt, not a stuck
	// "always retries immediately" loop.
	posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
	if err != nil {
		t.Fatalf("final call: PostDueDepreciationBatch: %v", err)
	}
	if posted != 0 {
		t.Errorf("final call posted = %d, want 0 (nothing left due)", posted)
	}
	if truncated {
		t.Errorf("final call truncated = true, want false (nothing left due, so the read didn't hit the cap)")
	}
}

// TestPostDueDepreciationBatch_RejectsNonPositiveMaxRows confirms
// maxRows <= 0 is a caller error, not a silent "unlimited" — see
// PostDueDepreciationBatch's own doc comment on why that distinction
// matters (a config value left unset by accident must fail loudly, not
// reintroduce the unbounded-run hazard this function exists to avoid).
func TestPostDueDepreciationBatch_RejectsNonPositiveMaxRows(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()
	for _, maxRows := range []int{0, -1} {
		if _, _, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), maxRows); err == nil {
			t.Errorf("maxRows=%d: expected an error, got nil", maxRows)
		}
	}
}

// TestPostDueDepreciation_DelegatesToBatchUnbounded confirms
// PostDueDepreciation's own unbounded convenience form still posts an
// entire backlog in one call, regardless of size — the guarantee every
// other TestPostDueDepreciation_* test in this file already relies on
// implicitly; this test is the direct one, using a backlog bigger than
// PostDueDepreciationBatch's own realistic caps to make sure
// PostDueDepreciation's math.MaxInt delegation isn't accidentally a
// smaller hardcoded number that would just happen to cover every other
// test's small fixture sizes.
func TestPostDueDepreciation_DelegatesToBatchUnbounded(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	const usefulLifeMonths = 6
	assetID := fx.createAssetWithLife(t, "FA-unbounded", usefulLifeMonths)
	for i := range usefulLifeMonths {
		seq := i + 1
		fx.createScheduleRow(t, assetID, seq, fmt.Sprintf("2020-%02d-28", seq), 1000.00, float64(usefulLifeMonths-seq)*1000.00)
	}

	posted, err := PostDueDepreciation(ctx, fx.tenantDB, SchedulerActor())
	if err != nil {
		t.Fatalf("PostDueDepreciation: %v", err)
	}
	if posted != usefulLifeMonths {
		t.Fatalf("posted = %d, want %d (every due row in one call)", posted, usefulLifeMonths)
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
		t.Fatalf("asset status = %q, want fully_depreciated", code)
	}
}

// TestPostDueDepreciationBatch_OuterBudgetLeavesUnvisitedAssetUntouched is
// the direct regression test for independent review's coverage finding
// on uc-infra#137/ADR-0025: TestPostDueDepreciationBatch_
// CapsPerCallAndResumesAcrossCalls above uses a single asset, so the
// budget always runs out INSIDE postAssetDepreciation's own due-rows
// loop — the outer per-asset loop's own `remaining <= 0` break
// (PostDueDepreciationBatch) never actually fires in that test. This
// test uses two assets, each with more due rows than the whole cap, so
// the budget is guaranteed to be exhausted entirely within ONE asset,
// proving the OTHER asset — never even visited by postAssetDepreciation
// this call — is left completely untouched: no posted_at set on any of
// its rows, no journal entries for it, status still in_service. Map
// iteration order is unspecified, so this asserts the order-independent
// invariant (exactly one asset fully drained to the cap, the other
// entirely untouched) rather than which specific asset goes first.
func TestPostDueDepreciationBatch_OuterBudgetLeavesUnvisitedAssetUntouched(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	const rowsPerAsset = 3
	const cap = rowsPerAsset // exhausted entirely within whichever asset is visited first

	assetA := fx.createAssetWithLife(t, "FA-two-asset-a", rowsPerAsset)
	assetB := fx.createAssetWithLife(t, "FA-two-asset-b", rowsPerAsset)
	rowsFor := map[string][]string{}
	for _, assetID := range []string{assetA, assetB} {
		for i := 0; i < rowsPerAsset; i++ {
			seq := i + 1
			rowsFor[assetID] = append(rowsFor[assetID], fx.createScheduleRow(t, assetID, seq, fmt.Sprintf("2020-%02d-28", seq), 1000.00, 0))
		}
	}

	posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
	if err != nil {
		t.Fatalf("PostDueDepreciationBatch: %v", err)
	}
	if posted != cap {
		t.Fatalf("posted = %d, want %d (exactly one asset's worth, the budget for a second)", posted, cap)
	}
	// uc-infra#183: the worklist read itself hit the cap (6 due rows
	// across both assets, LIMITed to `cap`=3), so this call must report
	// truncated=true — the untouched asset's own rows are exactly the
	// real work this signal exists to let tickTenant retry sooner for.
	if !truncated {
		t.Errorf("truncated = false, want true (an entire asset's worth of due rows was left untouched)")
	}

	rowPosted := func(rowID string) bool {
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row %s: %v", rowID, err)
		}
		postedAt, _ := row.Data["posted_at"].(string)
		return postedAt != ""
	}
	assetFullyPosted := func(assetID string) bool {
		for _, rowID := range rowsFor[assetID] {
			if !rowPosted(rowID) {
				return false
			}
		}
		return true
	}
	assetUntouched := func(assetID string) bool {
		for _, rowID := range rowsFor[assetID] {
			if rowPosted(rowID) {
				return false
			}
		}
		return true
	}

	aFull, bFull := assetFullyPosted(assetA), assetFullyPosted(assetB)
	if aFull == bFull {
		t.Fatalf("expected exactly one of the two assets fully posted, got assetA fully posted=%v assetB fully posted=%v", aFull, bFull)
	}
	visited, untouched := assetA, assetB
	if bFull {
		visited, untouched = assetB, assetA
	}
	if !assetFullyPosted(visited) {
		t.Errorf("visited asset %s: expected all %d rows posted", visited, rowsPerAsset)
	}
	if !assetUntouched(untouched) {
		t.Errorf("untouched asset %s: expected zero rows posted (outer budget break must leave a not-yet-visited asset completely alone)", untouched)
	}
	if code := fx.statusCode(t, "FixedAsset", untouched); code != "in_service" {
		t.Errorf("untouched asset status = %q, want in_service", code)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != cap {
		t.Fatalf("journal entries = %d, want %d (only the visited asset's rows)", len(list), cap)
	}
}

// TestPostDueDepreciationBatch_TruncatedCallDoesNotTransitionEarly is the
// direct regression test for independent review's finding that capping
// introduced a NEW way to reach a premature fully_depreciated transition
// that the pre-existing (unbounded) completion check alone did not have
// to guard against: an asset whose useful_life_months was edited down
// AFTER its DepreciationSchedule already had more rows than that. Under
// the unbounded PostDueDepreciation, all such rows post in the very same
// call before the completion check ever runs, so the check's
// postedCount always reflects every one of them — never early. Under a
// cap small enough to stop this ONE asset's own due-rows loop partway
// through, postedCount could reach useful_life_months while due rows
// this same asset still has are simply unattempted yet — which, without
// the truncated-gate fix, would flip the asset to fully_depreciated with
// due rows unposted and no way back (the status graph has no
// fully_depreciated -> in_service edge).
func TestPostDueDepreciationBatch_TruncatedCallDoesNotTransitionEarly(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	// Schedule built (and, in a real product flow, mostly posted) against
	// a 12-month life; useful_life_months is then edited down to 6 by a
	// later data correction — deliberately more entered/due rows than the
	// asset's current useful_life_months, the exact precondition
	// independent review's finding needs.
	assetID := fx.createAssetWithLife(t, "FA-life-edited-down", 12)
	const totalRows = 12
	rowIDs := make([]string, totalRows)
	for i := 0; i < totalRows; i++ {
		seq := i + 1
		rowIDs[i] = fx.createScheduleRow(t, assetID, seq, fmt.Sprintf("2020-%02d-28", seq), 1000.00, 0)
	}
	asset, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), assetID)
	if err != nil {
		t.Fatalf("Get FixedAsset: %v", err)
	}
	newData := map[string]any{}
	for k, v := range asset.Data {
		newData[k] = v
	}
	newData["useful_life_months"] = 6.0
	expectedVersion := asset.Version
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), assetID, newData, &expectedVersion, humanActor()); err != nil {
		t.Fatalf("edit useful_life_months down to 6: %v", err)
	}

	// Cap of 6 is reached entirely within this one asset's own due-rows
	// loop (postAssetDepreciation's inner break, not the outer per-asset
	// one) — postedCount would read exactly 6, equal to the edited-down
	// life, on this very call.
	const cap = 6
	posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
	if err != nil {
		t.Fatalf("PostDueDepreciationBatch: %v", err)
	}
	if posted != cap {
		t.Fatalf("posted = %d, want %d", posted, cap)
	}
	// uc-infra#183: 12 due rows, capped at 6 — the worklist read hit the
	// cap, so this call must report truncated=true regardless of what
	// postedCount happens to equal.
	if !truncated {
		t.Errorf("truncated = false, want true (6 due rows were left unattempted by this call)")
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
		t.Fatalf("asset status = %q, want in_service — a truncated call must not transition the asset even though postedCount reached the (edited-down) useful_life_months, because 6 due rows are still genuinely unposted", code)
	}

	// The remaining 6 due rows must still be postable — the asset must
	// not have been locked out of ever posting them by an early
	// transition (there is no fully_depreciated -> in_service edge).
	posted2, truncated2, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), totalRows)
	if err != nil {
		t.Fatalf("second PostDueDepreciationBatch: %v", err)
	}
	if posted2 != cap {
		t.Fatalf("second call posted = %d, want %d (the remaining 6 rows)", posted2, cap)
	}
	// uc-infra#183: this call's cap (totalRows=12) comfortably exceeds
	// the 6 rows actually due, so the read didn't hit it.
	if truncated2 {
		t.Errorf("second call truncated = true, want false (nothing left due after this call)")
	}
	if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
		t.Fatalf("asset status = %q, want fully_depreciated once every due row is genuinely posted", code)
	}
	for i, rowID := range rowIDs {
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row %d: %v", i+1, err)
		}
		if postedAt, _ := row.Data["posted_at"].(string); postedAt == "" {
			t.Errorf("row %d still unposted after both calls", i+1)
		}
	}
}

// TestPostDueDepreciationBatch_ReportsTruncatedWhenHealingSweepHitsCap is
// the direct regression test for uc-infra#183's other truncation source:
// PostDueDepreciationBatch's due-rows read can find nothing to do (every
// row already posted) while its healing sweep still has more
// stuck-but-never-transitioned assets than this call's own cap can heal
// in one pass — see healStuckFullyDepreciatedAssets's own doc comment on
// why nothing about the due-rows worklist can ever surface these assets
// again. truncated must reflect that too, not just the due-rows read: a
// caller checking only the posting phase would silently wait out the
// full throttle interval between capped healing sweeps, the same bug
// this issue exists to fix, just via the other worklist.
func TestPostDueDepreciationBatch_ReportsTruncatedWhenHealingSweepHitsCap(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	const numStuckAssets = 3
	assetIDs := make([]string, numStuckAssets)
	for i := 0; i < numStuckAssets; i++ {
		assetID := fx.createAssetWithLife(t, fmt.Sprintf("FA-stuck-%d", i+1), 1)
		rowID := fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 0.00)
		// Mark posted directly (bypassing PostDueDepreciation, same
		// pattern TestPostDueDepreciation_
		// AlreadyFullyPostedButNotTransitioned_HealsOnNextRun uses) so
		// the schedule is genuinely exhausted but the fully_depreciated
		// transition never ran.
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row for asset %d: %v", i+1, err)
		}
		fields := map[string]any{}
		for k, v := range row.Data {
			fields[k] = v
		}
		fields["posted_at"] = "2020-01-31"
		version := row.Version
		if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID, fields, &version, humanActor()); err != nil {
			t.Fatalf("mark row posted for asset %d: %v", i+1, err)
		}
		assetIDs[i] = assetID
	}

	// cap smaller than numStuckAssets: the due-rows read finds nothing
	// (every row already posted), but the healing sweep's own
	// LifeCompleteGroupIDs read is capped too and can only heal `cap` of
	// the 3 stuck assets this call.
	const cap = 2
	posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
	if err != nil {
		t.Fatalf("PostDueDepreciationBatch: %v", err)
	}
	if posted != 0 {
		t.Fatalf("posted = %d, want 0 (every row was already marked posted before this call)", posted)
	}
	if !truncated {
		t.Errorf("truncated = false, want true (the healing sweep's own read hit its cap with a stuck asset left over)")
	}

	healedCount := 0
	for _, assetID := range assetIDs {
		if fx.statusCode(t, "FixedAsset", assetID) == "fully_depreciated" {
			healedCount++
		}
	}
	if healedCount != cap {
		t.Fatalf("healed %d of %d stuck assets, want exactly %d (the healing sweep's own cap)", healedCount, numStuckAssets, cap)
	}

	// A further call with room for the remainder finishes healing and
	// reports truncated=false.
	posted2, truncated2, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), numStuckAssets)
	if err != nil {
		t.Fatalf("second PostDueDepreciationBatch: %v", err)
	}
	if posted2 != 0 {
		t.Errorf("second call posted = %d, want 0 (healing never posts a journal entry)", posted2)
	}
	if truncated2 {
		t.Errorf("second call truncated = true, want false (every stuck asset healed)")
	}
	for _, assetID := range assetIDs {
		if code := fx.statusCode(t, "FixedAsset", assetID); code != "fully_depreciated" {
			t.Errorf("asset %s status = %q, want fully_depreciated after the second call", assetID, code)
		}
	}
}

// TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenHealingSweepIsPermanentlyStuck
// is the direct regression test for independent review's MAJOR finding
// on this function's first uc-infra#183 version: a healing-sweep window
// permanently full of assets this call can NEVER transition (here: the
// tenant's fixed_asset_status "fully_depreciated" Status was never
// seeded, so transitionToFullyDepreciated's own statusIDByCodeTx call
// fails identically every time) must NOT report truncated=true forever.
// The old (posted-window-fullness-only) version would have: every
// candidate lands back in the same LIMITed window on every future call,
// windowFull stays true, and tickTenant would clear its throttle
// unconditionally — a caller retrying at PollInterval cadence forever
// for zero possible gain, on the one query ADR-0026/uc-infra#202 already
// flags as the expensive one.
func TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenHealingSweepIsPermanentlyStuck(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	// Simulate a tenant whose fixed_asset_status Status set is missing
	// fully_depreciated (a corrupted/partial PublishStatuses run, or a
	// direct data edit) — transitionToFullyDepreciated's own
	// statusIDByCodeTx call will fail identically on every attempt.
	if _, err := fx.tenantDB.ExecContext(ctx, `DELETE FROM records WHERE entity_type = 'Status' AND data->>'code' = 'fully_depreciated'`); err != nil {
		t.Fatalf("delete fully_depreciated Status: %v", err)
	}

	const numStuckAssets = 3
	assetIDs := make([]string, numStuckAssets)
	for i := 0; i < numStuckAssets; i++ {
		assetID := fx.createAssetWithLife(t, fmt.Sprintf("FA-permastuck-%d", i+1), 1)
		rowID := fx.createScheduleRow(t, assetID, 1, "2020-01-31", 1000.00, 0.00)
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row for asset %d: %v", i+1, err)
		}
		fields := map[string]any{}
		for k, v := range row.Data {
			fields[k] = v
		}
		fields["posted_at"] = "2020-01-31"
		version := row.Version
		if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID, fields, &version, humanActor()); err != nil {
			t.Fatalf("mark row posted for asset %d: %v", i+1, err)
		}
		assetIDs[i] = assetID
	}

	const cap = 2 // < numStuckAssets, so the healing sweep's own window is exactly full every call
	for attempt := 1; attempt <= 3; attempt++ {
		posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
		if err != nil {
			t.Fatalf("attempt %d: PostDueDepreciationBatch: %v", attempt, err)
		}
		if posted != 0 {
			t.Errorf("attempt %d: posted = %d, want 0 (nothing due — this is a healing-only scenario)", attempt, posted)
		}
		if truncated {
			t.Errorf("attempt %d: truncated = true, want false — the healing window is permanently full of assets this call can never transition, so a caller must fall back to the ordinary throttle instead of retrying every poll tick forever", attempt)
		}
	}

	for _, assetID := range assetIDs {
		if code := fx.statusCode(t, "FixedAsset", assetID); code != "in_service" {
			t.Errorf("asset %s status = %q, want in_service — it must never have transitioned (the target status doesn't exist)", assetID, code)
		}
	}
}

// TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenDueRowsArePermanentlyUnpostable
// is the due-rows-side sibling of the healing-sweep test above:
// DepreciationSchedule rows whose asset's account references are all
// non-empty (so they pass records.ListDueUnposted's own gate) but point
// at Account records that don't exist stay due-and-unposted forever —
// postAssetDepreciation's accountCode lookup fails, and fails BEFORE
// spending any of its attempted budget, so the exact same rows sort
// into the exact same LIMITed window on every future call. Must not
// report truncated=true forever, for the same reason as the healing
// case above.
func TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenDueRowsArePermanentlyUnpostable(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx := context.Background()

	const noSuchAccount = "00000000-0000-0000-0000-000000000099" // well-formed UUID, no matching Account record
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-dangling-accounts", "name": map[string]any{"en": "Test Asset"},
		"acquisition_date": "2020-01-01", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    noSuchAccount,
		"depreciation_expense_account_id":     noSuchAccount,
		"accumulated_depreciation_account_id": noSuchAccount,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset with dangling account refs: %v", err)
	}
	const numRows = 3
	rowIDs := make([]string, numRows)
	for i := 0; i < numRows; i++ {
		seq := i + 1
		rowIDs[i] = fx.createScheduleRow(t, rec.ID, seq, fmt.Sprintf("2020-%02d-28", seq), 1000.00, 0.00)
	}

	const cap = 2 // < numRows, so the due-rows read is exactly full every call
	for attempt := 1; attempt <= 3; attempt++ {
		posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), cap)
		if err != nil {
			t.Fatalf("attempt %d: PostDueDepreciationBatch: %v", attempt, err)
		}
		if posted != 0 {
			t.Errorf("attempt %d: posted = %d, want 0 (every row's account refs are unresolvable)", attempt, posted)
		}
		if truncated {
			t.Errorf("attempt %d: truncated = true, want false — the due-rows window is permanently full of rows this call can never post, so a caller must fall back to the ordinary throttle instead of retrying every poll tick forever", attempt)
		}
	}

	for i, rowID := range rowIDs {
		row, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rowID)
		if err != nil {
			t.Fatalf("Get row %d: %v", i+1, err)
		}
		if postedAt, _ := row.Data["posted_at"].(string); postedAt != "" {
			t.Errorf("row %d: posted_at = %q, want empty — it must never have posted (its accounts don't resolve)", i+1, postedAt)
		}
	}
}

// TestPostDueDepreciationBatch_PropagatesContextCancellation confirms
// the earliest DB reads (depreciationInServiceStatusID's StatusType
// lookup, and the due-rows worklist read itself) surface a cancelled
// context as a real error rather than a misleading (0, false, nil) —
// the same "don't report a clean return for a call actually cut short"
// discipline this function's own doc comment already holds the healing
// sweep to.
func TestPostDueDepreciationBatch_PropagatesContextCancellation(t *testing.T) {
	fx := setUpDepreciationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call even starts

	posted, truncated, err := PostDueDepreciationBatch(ctx, fx.tenantDB, SchedulerActor(), 10)
	if err == nil {
		t.Fatal("expected an error from a call made with an already-cancelled context, got nil")
	}
	if posted != 0 {
		t.Errorf("posted = %d, want 0", posted)
	}
	if truncated {
		t.Errorf("truncated = true, want false on an error return")
	}
}
