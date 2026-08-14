package assets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
)

// scheduleHookFixture is setUpDepreciationFixture's engine, with
// GenerateDepreciationScheduleOnWrite actually wired for "FixedAsset" —
// setUpDepreciationFixture's own engine deliberately does NOT register
// this hook (its tests build DepreciationSchedule rows by hand via
// createScheduleRow, exercising PostDueDepreciation's own branching, not
// this file's). Kept as a thin wrapper rather than changing
// setUpDepreciationFixture itself, so ledger_test.go's existing tests
// keep constructing their schedules exactly as before.
func setUpScheduleHookFixture(t *testing.T) depreciationFixture {
	t.Helper()
	fx := setUpDepreciationFixture(t)
	fx.engine.SetHook("FixedAsset", GenerateDepreciationScheduleOnWrite)
	return fx
}

func TestGenerateDepreciationScheduleOnWrite_CreateGeneratesFullSchedule(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-1", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 12000.0, "salvage_value": 0.0,
		"useful_life_months": 12.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule: %v", err)
	}
	if len(rows) != 12 {
		t.Fatalf("expected 12 schedule rows (one per useful_life_months), got %d", len(rows))
	}

	wantPeriods, err := Build(Input{
		Method: MethodStraightLine, AcquisitionDate: "2026-01-15",
		CostMinor: 1200000, SalvageMinor: 0, UsefulLifeMonths: 12,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bySeq := map[int]map[string]any{}
	for _, r := range rows {
		seq, _ := r.Data["sequence"].(float64)
		bySeq[int(seq)] = r.Data
	}
	for _, p := range wantPeriods {
		got, ok := bySeq[p.Sequence]
		if !ok {
			t.Fatalf("missing schedule row for sequence %d", p.Sequence)
		}
		if got["period_end"] != p.PeriodEnd {
			t.Errorf("sequence %d: period_end = %v, want %s", p.Sequence, got["period_end"], p.PeriodEnd)
		}
		wantAmount := float64(p.DepreciationMinor) / 100
		if got["depreciation_amount"] != wantAmount {
			t.Errorf("sequence %d: depreciation_amount = %v, want %v", p.Sequence, got["depreciation_amount"], wantAmount)
		}
		wantBookValue := float64(p.BookValueMinor) / 100
		if got["book_value"] != wantBookValue {
			t.Errorf("sequence %d: book_value = %v, want %v", p.Sequence, got["book_value"], wantBookValue)
		}
		if got["posted_at"] != nil && got["posted_at"] != "" {
			t.Errorf("sequence %d: expected posted_at unset on a freshly-generated row, got %v", p.Sequence, got["posted_at"])
		}
	}
}

func TestGenerateDepreciationScheduleOnWrite_CreateWritesAuditRowsForEveryScheduleRow(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-2", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 6000.0, "salvage_value": 0.0,
		"useful_life_months": 6.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("expected 6 schedule rows, got %d", len(rows))
	}
	for _, r := range rows {
		if n := fx.auditCount(t, "DepreciationSchedule", r.ID, "create", "human"); n != 1 {
			t.Errorf("expected 1 create audit row for DepreciationSchedule %s, got %d", r.ID, n)
		}
	}
}

func TestGenerateDepreciationScheduleOnWrite_InvalidUsefulLifeMonths_RejectsWholeCreate(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	_, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-BAD-LIFE", "name": map[string]any{"en": "Bad Asset"},
		"acquisition_date": "2026-01-15", "cost": 1000.0, "salvage_value": 0.0,
		"useful_life_months": 0.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err == nil {
		t.Fatal("expected an error for useful_life_months=0, got nil")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected, got %v", err)
	}

	existing, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), "asset_number", "FA-HOOK-BAD-LIFE")
	if err != nil {
		t.Fatalf("list FixedAsset: %v", err)
	}
	if len(existing) != 0 {
		t.Fatalf("expected the whole FixedAsset write to roll back, but %d row(s) exist", len(existing))
	}
}

func TestGenerateDepreciationScheduleOnWrite_SalvageExceedsCost_RejectsWholeCreate(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	_, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-BAD-SALVAGE", "name": map[string]any{"en": "Bad Asset"},
		"acquisition_date": "2026-01-15", "cost": 1000.0, "salvage_value": 5000.0,
		"useful_life_months": 12.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err == nil {
		t.Fatal("expected an error for salvage_value > cost, got nil")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected, got %v", err)
	}
}

// TestGenerateDepreciationScheduleOnWrite_CreateWithNonDefaultCurrencyScale
// is this file's own instance of the same regression class
// TestCurrencyMinorUnit_ResolvesNonDefaultScale (ledger_test.go) pins:
// every other test in this file uses USD (minor_unit=2), identical to
// defaultCurrencyMinorUnit, so nothing distinguishes "currencyMinorUnitTx
// resolved the real value" from "the fallback silently applied and got
// lucky" (independent review of #213's own finding — the doc comment on
// currencyMinorUnitTx used to claim this was already covered by the
// missing-currency test above; it wasn't, since that test never sets
// currency_id at all and so never calls currencyMinorUnitTx in the first
// place). JPY-style minor_unit=0: a 1000.00 depreciation_amount must
// convert to 1000 minor units, not 100000 (what the 2dp fallback would
// wrongly produce).
func TestGenerateDepreciationScheduleOnWrite_CreateWithNonDefaultCurrencyScale(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	jpy, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "Currency"), map[string]any{
		"code": "JPY", "name": "Japanese Yen", "minor_unit": float64(0),
	}, humanActor())
	if err != nil {
		t.Fatalf("create JPY Currency: %v", err)
	}

	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-JPY", "name": map[string]any{"en": "Hook Asset (JPY)"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         jpy.ID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 schedule rows, got %d", len(rows))
	}
	bySeq := map[int]map[string]any{}
	for _, r := range rows {
		seq, _ := r.Data["sequence"].(float64)
		bySeq[int(seq)] = r.Data
	}
	wantAmounts := map[int]float64{1: 1000, 2: 1000, 3: 1000}
	wantBookValues := map[int]float64{1: 2000, 2: 1000, 3: 0}
	for seq, wantAmount := range wantAmounts {
		got := bySeq[seq]
		if got["depreciation_amount"] != wantAmount {
			t.Errorf("sequence %d: depreciation_amount = %v, want %v (at JPY's 0dp scale, not the 2dp fallback's wrongly-scaled 100x value)", seq, got["depreciation_amount"], wantAmount)
		}
		if got["book_value"] != wantBookValues[seq] {
			t.Errorf("sequence %d: book_value = %v, want %v", seq, got["book_value"], wantBookValues[seq])
		}
	}
}

func TestGenerateDepreciationScheduleOnWrite_MissingCurrency_FallsBackToDefaultScale(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	fields := map[string]any{
		"asset_number": "FA-HOOK-NOCUR", "name": map[string]any{"en": "No Currency Asset"},
		"acquisition_date": "2026-01-15", "cost": 100.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), fields, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset with no currency_id: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 schedule rows, got %d", len(rows))
	}
	// 100.00 / 3 months at 2dp (defaultCurrencyMinorUnit fallback) is
	// 33.34 + 33.33 + 33.33 — the same remainder-to-earliest-periods
	// distribution depreciation.go's own doc comment describes.
	total := 0.0
	for _, r := range rows {
		amt, _ := r.Data["depreciation_amount"].(float64)
		total += amt
	}
	if total != 100.0 {
		t.Errorf("expected the schedule to sum exactly to cost (100.0) at the 2dp fallback scale, got %v", total)
	}
}

func TestGenerateDepreciationScheduleOnWrite_UpdateUnrelatedField_DoesNotRegenerate(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-HOOK-3", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"location":                            "Depot A",
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	before, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule before update: %v", err)
	}
	beforeIDs := map[string]bool{}
	for _, r := range before {
		beforeIDs[r.ID] = true
	}

	// Full replacement, per crud.Engine.Update's own contract — every
	// field resent, only location actually different.
	newFields := map[string]any{
		"asset_number": "FA-HOOK-3", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"location":                            "Depot B",
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, newFields, nil, humanActor()); err != nil {
		t.Fatalf("update FixedAsset location: %v", err)
	}

	after, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule after update: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("expected the same %d schedule rows after an unrelated field edit, got %d", len(before), len(after))
	}
	for _, r := range after {
		if !beforeIDs[r.ID] {
			t.Errorf("schedule row %s was not part of the original schedule — the edit regenerated it unnecessarily", r.ID)
		}
	}
}

func TestGenerateDepreciationScheduleOnWrite_UpdateChangesUsefulLife_RegeneratesWhenUnposted(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-4", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), base, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	before, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil || len(before) != 3 {
		t.Fatalf("list DepreciationSchedule before update: err=%v n=%d", err, len(before))
	}

	extended := map[string]any{}
	for k, v := range base {
		extended[k] = v
	}
	extended["useful_life_months"] = 6.0
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, extended, nil, humanActor()); err != nil {
		t.Fatalf("update FixedAsset useful_life_months: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule after update: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("expected the schedule to regenerate to 6 rows, got %d", len(rows))
	}

	// The delete half of the audit trail, not just the create half
	// (already covered by TestGenerateDepreciationScheduleOnWrite_
	// CreateWritesAuditRowsForEveryScheduleRow) — every one of the 3
	// stale rows this regeneration replaced must have its own delete
	// audit entry, per CLAUDE.md's audit-actor-identity rule.
	for _, r := range before {
		if n := fx.auditCount(t, "DepreciationSchedule", r.ID, "delete", "human"); n != 1 {
			t.Errorf("expected 1 delete audit row for stale DepreciationSchedule %s, got %d", r.ID, n)
		}
	}
}

func TestGenerateDepreciationScheduleOnWrite_UpdateAfterPosting_Rejected(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-5", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), base, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}

	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected a generated schedule to mark a row posted against")
	}
	// Simulate PostDueDepreciation having posted the first row.
	posted := map[string]any{}
	for k, v := range rows[0].Data {
		posted[k] = v
	}
	posted["posted_at"] = rows[0].Data["period_end"]
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, posted, nil, humanActor()); err != nil {
		t.Fatalf("mark DepreciationSchedule row posted: %v", err)
	}

	changed := map[string]any{}
	for k, v := range base {
		changed[k] = v
	}
	changed["useful_life_months"] = 6.0
	_, err = fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, changed, nil, humanActor())
	if err == nil {
		t.Fatal("expected updating useful_life_months after posting to be rejected")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "already posted") {
		t.Errorf("expected a message explaining the rejection, got %v", err)
	}

	// The whole write rolled back — cost/useful_life_months on the
	// FixedAsset itself must be unchanged, not just the schedule.
	got, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID)
	if err != nil {
		t.Fatalf("Get FixedAsset: %v", err)
	}
	if got.Data["useful_life_months"] != 3.0 {
		t.Errorf("expected useful_life_months to remain 3.0 after the rejected update, got %v", got.Data["useful_life_months"])
	}
}

func TestGenerateDepreciationScheduleOnWrite_UpdateAfterPosting_UnrelatedFieldStillAllowed(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-6", "name": map[string]any{"en": "Hook Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"location":                            "Depot A",
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), base, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	rows, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("list DepreciationSchedule: %v (n=%d)", err, len(rows))
	}
	posted := map[string]any{}
	for k, v := range rows[0].Data {
		posted[k] = v
	}
	posted["posted_at"] = rows[0].Data["period_end"]
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, posted, nil, humanActor()); err != nil {
		t.Fatalf("mark DepreciationSchedule row posted: %v", err)
	}

	unrelated := map[string]any{}
	for k, v := range base {
		unrelated[k] = v
	}
	unrelated["location"] = "Depot B"
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, unrelated, nil, humanActor()); err != nil {
		t.Fatalf("expected an edit that doesn't touch the schedule to succeed even after posting, got: %v", err)
	}
}
