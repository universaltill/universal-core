package assets

import (
	"context"
	"database/sql"
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
func (fx depreciationFixture) createAsset(t *testing.T, statusCode string, withMissingAccounts bool) string {
	t.Helper()
	fields := map[string]any{
		"asset_number": "FA-" + statusCode, "name": map[string]any{"en": "Test Asset"},
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

	badAssetID := fx.createAsset(t, "in_service", true)
	fx.createScheduleRow(t, badAssetID, 1, "2020-01-31", 1000.00, 2000.00)

	goodAssetID := fx.createAsset(t, "in_service", false)
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
