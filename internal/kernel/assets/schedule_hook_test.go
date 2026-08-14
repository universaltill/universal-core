package assets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
)

// row is a scheduleMatches/allOverridden test fixture helper — a bare
// data.Record carrying just the fields those two pure functions
// actually read, without going through a real Create/Update.
func row(seq int, periodEnd string, amount, bookValue float64, overridden bool) data.Record {
	return data.Record{Data: map[string]any{
		"sequence": float64(seq), "period_end": periodEnd,
		"depreciation_amount": amount, "book_value": bookValue,
		"overridden": overridden,
	}}
}

func desiredRow(seq int, periodEnd string, amount, bookValue float64) map[string]any {
	return map[string]any{
		"sequence": float64(seq), "period_end": periodEnd,
		"depreciation_amount": amount, "book_value": bookValue,
	}
}

func TestScheduleMatches(t *testing.T) {
	tests := []struct {
		name     string
		existing []data.Record
		desired  []map[string]any
		want     bool
	}{
		{"both empty", nil, nil, true},
		{"different lengths", []data.Record{row(1, "2026-01-31", 100, 200, false)}, nil, false},
		{
			"exact match, nothing overridden",
			[]data.Record{row(1, "2026-01-31", 100, 200, false), row(2, "2026-02-28", 100, 100, false)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200), desiredRow(2, "2026-02-28", 100, 100)},
			true,
		},
		{
			"one non-overridden row's amount differs",
			[]data.Record{row(1, "2026-01-31", 999, 200, false)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200)},
			false,
		},
		{
			"one non-overridden row's period_end differs",
			[]data.Record{row(1, "2026-02-28", 100, 200, false)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200)},
			false,
		},
		{
			"one non-overridden row's book_value differs",
			[]data.Record{row(1, "2026-01-31", 100, 999, false)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200)},
			false,
		},
		{
			"mismatched row IS overridden — skipped, still matches",
			[]data.Record{row(1, "2026-01-31", 999, 999, true), row(2, "2026-02-28", 100, 100, false)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200), desiredRow(2, "2026-02-28", 100, 100)},
			true,
		},
		{
			"overridden row still counts toward the length/sequence check",
			[]data.Record{row(1, "2026-01-31", 999, 999, true)},
			[]map[string]any{desiredRow(1, "2026-01-31", 100, 200), desiredRow(2, "2026-02-28", 100, 100)},
			false, // lengths differ (1 vs 2) — overridden doesn't excuse a missing row
		},
		{
			"desired sequence missing from existing entirely",
			[]data.Record{row(1, "2026-01-31", 100, 200, false)},
			[]map[string]any{desiredRow(2, "2026-02-28", 100, 100)},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scheduleMatches(tt.existing, tt.desired); got != tt.want {
				t.Errorf("scheduleMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllOverridden(t *testing.T) {
	tests := []struct {
		name     string
		existing []data.Record
		want     bool
	}{
		{"empty — nothing to have overridden", nil, false},
		{"single overridden row", []data.Record{row(1, "2026-01-31", 100, 200, true)}, true},
		{"single non-overridden row", []data.Record{row(1, "2026-01-31", 100, 200, false)}, false},
		{
			"every row overridden",
			[]data.Record{row(1, "x", 1, 1, true), row(2, "y", 1, 1, true)},
			true,
		},
		{
			"mixed — one row not overridden",
			[]data.Record{row(1, "x", 1, 1, true), row(2, "y", 1, 1, false)},
			false,
		},
		{
			"non-bool overridden value treated as false",
			[]data.Record{{Data: map[string]any{"sequence": float64(1), "overridden": "true"}}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allOverridden(tt.existing); got != tt.want {
				t.Errorf("allOverridden() = %v, want %v", got, tt.want)
			}
		})
	}
}

// scheduleHookFixture is setUpDepreciationFixture's engine, with
// GenerateDepreciationScheduleOnWrite actually wired for "FixedAsset" and
// MarkDepreciationScheduleOverriddenOnWrite for "DepreciationSchedule" —
// the same pair cmd/universal-core/main.go and cmd/seed-demo-data/main.go
// both wire together (uc-infra#236). setUpDepreciationFixture's own
// engine deliberately does NOT register either (its tests build
// DepreciationSchedule rows by hand via createScheduleRow, exercising
// PostDueDepreciation's own branching, not this file's). Kept as a thin
// wrapper rather than changing setUpDepreciationFixture itself, so
// ledger_test.go's existing tests keep constructing their schedules
// exactly as before.
func setUpScheduleHookFixture(t *testing.T) depreciationFixture {
	t.Helper()
	fx := setUpDepreciationFixture(t)
	fx.engine.SetHook("FixedAsset", GenerateDepreciationScheduleOnWrite)
	fx.engine.SetHook("DepreciationSchedule", MarkDepreciationScheduleOverriddenOnWrite)
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
	// Simulate PostDueDepreciation having posted the first row — via the
	// raw records repo, matching production's own non-engine posting
	// path (markSchedulePosted's own doc comment, ledger_test.go).
	fx.markSchedulePosted(t, rows[0].ID, rows[0].Data["period_end"].(string))

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

// TestGenerateDepreciationScheduleOnWrite_ManualCorrectionSurvivesUnrelatedEdit
// is uc-infra#236's first acceptance criterion: a human's manual
// correction to an unposted DepreciationSchedule row must survive a
// later, unrelated FixedAsset save — not be silently regenerated away
// the next time the asset happens to be saved for any reason.
func TestGenerateDepreciationScheduleOnWrite_ManualCorrectionSurvivesUnrelatedEdit(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-7", "name": map[string]any{"en": "Hook Asset"},
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	// A human fixes a typo in the first period's amount.
	corrected := map[string]any{}
	for k, v := range rows[0].Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}

	// An unrelated FixedAsset save — Location only.
	unrelated := map[string]any{}
	for k, v := range base {
		unrelated[k] = v
	}
	unrelated["location"] = "Depot B"
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, unrelated, nil, humanActor()); err != nil {
		t.Fatalf("update FixedAsset location: %v", err)
	}

	after, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID)
	if err != nil {
		t.Fatalf("Get corrected row after unrelated edit — it must not have been deleted: %v", err)
	}
	if after.Data["depreciation_amount"] != 950.00 {
		t.Errorf("expected the manual correction (950.00) to survive the unrelated edit, got %v — silently reverted", after.Data["depreciation_amount"])
	}
	afterAll, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule after unrelated edit: %v", err)
	}
	if len(afterAll) != 3 {
		t.Fatalf("expected the schedule to still have 3 rows, got %d", len(afterAll))
	}
}

// TestGenerateDepreciationScheduleOnWrite_PriorCorrectionThenPosted_UnrelatedEditNotRejected
// is uc-infra#236's second acceptance criterion: once a manually-corrected
// row is later posted for real, an unrelated FixedAsset edit must not be
// rejected just because the stored schedule differs from a freshly
// computed one for the correction's own sake.
func TestGenerateDepreciationScheduleOnWrite_PriorCorrectionThenPosted_UnrelatedEditNotRejected(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-8", "name": map[string]any{"en": "Hook Asset"},
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	corrected := map[string]any{}
	for k, v := range rows[0].Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}
	// The corrected row is now the one that gets posted (a real posting
	// run would post it once due, at whatever amount is currently
	// stored — the correction itself, not the original Build value) —
	// via the raw records repo, matching production's own non-engine
	// posting path.
	fx.markSchedulePosted(t, rows[0].ID, rows[0].Data["period_end"].(string))

	unrelated := map[string]any{}
	for k, v := range base {
		unrelated[k] = v
	}
	unrelated["location"] = "Depot B"
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, unrelated, nil, humanActor()); err != nil {
		t.Fatalf("expected an unrelated edit to succeed even though a posted row was previously manually corrected, got: %v", err)
	}
}

// TestGenerateDepreciationScheduleOnWrite_GenuineChangeStillRegeneratesDespiteOverriddenRow
// guards the other direction: the override marker must not blind the
// hook to a REAL term change on some OTHER, non-overridden row — the fix
// only excuses an overridden row's own content from the comparison, not
// the schedule as a whole.
func TestGenerateDepreciationScheduleOnWrite_GenuineChangeStillRegeneratesDespiteOverriddenRow(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-9", "name": map[string]any{"en": "Hook Asset"},
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	corrected := map[string]any{}
	for k, v := range rows[0].Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}

	// A genuine term change — cost, not just an unrelated field — with
	// nothing posted yet, so it must still regenerate.
	changed := map[string]any{}
	for k, v := range base {
		changed[k] = v
	}
	changed["cost"] = 6000.0
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, changed, nil, humanActor()); err != nil {
		t.Fatalf("update FixedAsset cost: %v", err)
	}

	after, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil {
		t.Fatalf("list DepreciationSchedule after cost change: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("expected 3 regenerated rows, got %d", len(after))
	}
	for _, r := range after {
		if r.ID == rows[0].ID {
			t.Errorf("expected the overridden row to be regenerated (not preserved) once a genuine term change was detected, but its id survived")
		}
		if overridden, _ := r.Data["overridden"].(bool); overridden {
			t.Errorf("expected a freshly-regenerated row to not be overridden, got true for %s", r.ID)
		}
		if amount, _ := r.Data["depreciation_amount"].(float64); amount != 2000.0 {
			t.Errorf("expected the regenerated schedule to reflect cost=6000 (2000/period), got %v", amount)
		}
	}
}

// TestGenerateDepreciationScheduleOnWrite_GenuineChangeStillRejectedDespiteOverriddenRow
// is the posted-history-safety twin of the regenerate test above: an
// overridden row must not mask a genuine term change from the
// already-posted rejection path either.
func TestGenerateDepreciationScheduleOnWrite_GenuineChangeStillRejectedDespiteOverriddenRow(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-10", "name": map[string]any{"en": "Hook Asset"},
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	// Row 0 is manually corrected (overridden); row 1 — a DIFFERENT,
	// non-overridden row — is the one that gets posted.
	corrected := map[string]any{}
	for k, v := range rows[0].Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}
	fx.markSchedulePosted(t, rows[1].ID, rows[1].Data["period_end"].(string))

	changed := map[string]any{}
	for k, v := range base {
		changed[k] = v
	}
	changed["cost"] = 6000.0
	_, err = fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, changed, nil, humanActor())
	if err == nil {
		t.Fatal("expected the genuine cost change to be rejected — row 1 (not overridden) is already posted")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected, got %v", err)
	}

	got, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID)
	if err != nil {
		t.Fatalf("Get FixedAsset: %v", err)
	}
	if got.Data["cost"] != 3000.0 {
		t.Errorf("expected cost to remain 3000.0 after the rejected update, got %v", got.Data["cost"])
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
	fx.markSchedulePosted(t, rows[0].ID, rows[0].Data["period_end"].(string))

	unrelated := map[string]any{}
	for k, v := range base {
		unrelated[k] = v
	}
	unrelated["location"] = "Depot B"
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, unrelated, nil, humanActor()); err != nil {
		t.Fatalf("expected an edit that doesn't touch the schedule to succeed even after posting, got: %v", err)
	}
}

// TestGenerateDepreciationScheduleOnWrite_AllRowsOverriddenAndPosted_GenuineChangeStillRejected
// is uc-infra#236 independent review finding F1's regression pin: if
// EVERY row in the schedule happens to be overridden, scheduleMatches's
// per-row skip can never itself produce a mismatch — without the
// all-overridden guard in GenerateDepreciationScheduleOnWrite, a
// genuine term change on a FixedAsset with a posted row would slip
// through unrejected the moment every row had been touched by hand.
func TestGenerateDepreciationScheduleOnWrite_AllRowsOverriddenAndPosted_GenuineChangeStillRejected(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()

	base := map[string]any{
		"asset_number": "FA-HOOK-11", "name": map[string]any{"en": "Hook Asset"},
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	// Every row gets a real, deliberate correction — genuinely
	// overridden, not an artifact of a no-op save.
	for i, row := range rows {
		corrected := map[string]any{}
		for k, v := range row.Data {
			corrected[k] = v
		}
		corrected["depreciation_amount"] = 900.0 + float64(i)
		if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID, corrected, nil, humanActor()); err != nil {
			t.Fatalf("correct DepreciationSchedule row %d: %v", i, err)
		}
	}
	for _, row := range rows {
		got, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID)
		if err != nil {
			t.Fatalf("Get DepreciationSchedule %s: %v", row.ID, err)
		}
		if overridden, _ := got.Data["overridden"].(bool); !overridden {
			t.Fatalf("expected row %s to be overridden after its correction — test setup invalid", row.ID)
		}
	}

	// One row also gets posted — bypassing the override hook, matching
	// production's own posting path (does not disturb the overridden
	// flag the correction above already set).
	fx.markSchedulePosted(t, rows[0].ID, rows[0].Data["period_end"].(string))

	changed := map[string]any{}
	for k, v := range base {
		changed[k] = v
	}
	changed["cost"] = 6000.0
	_, err = fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID, changed, nil, humanActor())
	if err == nil {
		t.Fatal("expected the genuine cost change to be rejected even though every row is overridden — one of them is posted")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected, got %v", err)
	}

	got, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), rec.ID)
	if err != nil {
		t.Fatalf("Get FixedAsset: %v", err)
	}
	if got.Data["cost"] != 3000.0 {
		t.Errorf("expected cost to remain 3000.0 after the rejected update, got %v", got.Data["cost"])
	}
}
