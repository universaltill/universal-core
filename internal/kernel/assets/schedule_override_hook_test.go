package assets

import (
	"context"
	"testing"
)

// TestMarkDepreciationScheduleOverriddenOnWrite_NoOpSaveDoesNotMarkOverridden
// is uc-infra#236 independent review finding F2's regression pin: a
// plain Save with no real content change (open
// DepreciationScheduleForm, click Save, change nothing) must NOT flip
// overridden — the earlier "any Update marks it" design did, and could
// never clear it again either.
func TestMarkDepreciationScheduleOverriddenOnWrite_NoOpSaveDoesNotMarkOverridden(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-OV-1", "name": map[string]any{"en": "Override Test Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}
	row := rows[0]

	same := map[string]any{}
	for k, v := range row.Data {
		same[k] = v
	}
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID, same, nil, humanActor()); err != nil {
		t.Fatalf("no-op save of DepreciationSchedule row: %v", err)
	}

	after, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID)
	if err != nil {
		t.Fatalf("Get DepreciationSchedule after no-op save: %v", err)
	}
	if overridden, _ := after.Data["overridden"].(bool); overridden {
		t.Errorf("expected overridden=false after a no-op save (content unchanged, still matches Build), got true")
	}
	// Only the primary update audit row — the hook must not have
	// written (or audited) a no-change flip.
	if n := fx.auditCount(t, "DepreciationSchedule", row.ID, "update", "human"); n != 1 {
		t.Errorf("expected 1 update audit row (primary only, no hook write on a no-op), got %d", n)
	}
}

// TestMarkDepreciationScheduleOverriddenOnWrite_CorrectionMarksOverridden
// is the ordinary positive case: a real content change away from what
// Build computes marks the row overridden and audits that write
// separately from the primary one.
func TestMarkDepreciationScheduleOverriddenOnWrite_CorrectionMarksOverridden(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-OV-2", "name": map[string]any{"en": "Override Test Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}
	row := rows[0]

	corrected := map[string]any{}
	for k, v := range row.Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}

	after, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID)
	if err != nil {
		t.Fatalf("Get DepreciationSchedule after correction: %v", err)
	}
	if overridden, _ := after.Data["overridden"].(bool); !overridden {
		t.Errorf("expected overridden=true after a real correction, got false")
	}
	if after.Data["depreciation_amount"] != 950.00 {
		t.Errorf("expected the correction itself to be stored, got %v", after.Data["depreciation_amount"])
	}
	// Two update audit rows: crud.Engine.Update's own primary write, plus
	// this hook's own separate write marking overridden — same
	// "each write gets its own audit entry" convention
	// purchasing.MatchVendorInvoiceOnUpdate's redirect path already uses.
	if n := fx.auditCount(t, "DepreciationSchedule", row.ID, "update", "human"); n != 2 {
		t.Errorf("expected 2 update audit rows (primary + override marker), got %d", n)
	}
}

// TestMarkDepreciationScheduleOverriddenOnWrite_EditingBackClearsOverridden
// is the self-correcting property the recompute design is built on: a
// row edited back to exactly what Build would produce is no longer an
// override, automatically — not a one-way switch.
func TestMarkDepreciationScheduleOverriddenOnWrite_EditingBackClearsOverridden(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-OV-3", "name": map[string]any{"en": "Override Test Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}
	row := rows[0]
	originalAmount := row.Data["depreciation_amount"]

	corrected := map[string]any{}
	for k, v := range row.Data {
		corrected[k] = v
	}
	corrected["depreciation_amount"] = 950.00
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID, corrected, nil, humanActor()); err != nil {
		t.Fatalf("correct DepreciationSchedule row: %v", err)
	}
	mid, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID)
	if err != nil || func() bool { o, _ := mid.Data["overridden"].(bool); return !o }() {
		t.Fatalf("expected overridden=true after the correction — test setup invalid: err=%v", err)
	}

	revert := map[string]any{}
	for k, v := range mid.Data {
		revert[k] = v
	}
	revert["depreciation_amount"] = originalAmount
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID, revert, nil, humanActor()); err != nil {
		t.Fatalf("revert DepreciationSchedule row: %v", err)
	}

	after, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), row.ID)
	if err != nil {
		t.Fatalf("Get DepreciationSchedule after revert: %v", err)
	}
	if overridden, _ := after.Data["overridden"].(bool); overridden {
		t.Errorf("expected overridden=false after editing the row back to exactly what Build computes, got true")
	}
}

// TestMarkDepreciationScheduleOverriddenOnWrite_ClientSuppliedOverriddenIsIgnored
// is uc-infra#236 independent review finding F3's regression pin: the
// hook must not trust a caller-submitted `overridden` value — it's
// always recomputed from the row's actual content, so a client can't
// spoof it either direction.
func TestMarkDepreciationScheduleOverriddenOnWrite_ClientSuppliedOverriddenIsIgnored(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-OV-4", "name": map[string]any{"en": "Override Test Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
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
	if err != nil || len(rows) != 3 {
		t.Fatalf("list DepreciationSchedule: err=%v n=%d", err, len(rows))
	}

	// Row 0: content unchanged, but the client lies and claims
	// overridden=true. The hook must correct it back to false.
	spoofedTrue := map[string]any{}
	for k, v := range rows[0].Data {
		spoofedTrue[k] = v
	}
	spoofedTrue["overridden"] = true
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID, spoofedTrue, nil, humanActor()); err != nil {
		t.Fatalf("update DepreciationSchedule row 0: %v", err)
	}
	got0, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[0].ID)
	if err != nil {
		t.Fatalf("Get DepreciationSchedule row 0: %v", err)
	}
	if overridden, _ := got0.Data["overridden"].(bool); overridden {
		t.Errorf("expected a client-claimed overridden=true on an unchanged row to be corrected back to false, got true")
	}

	// Row 1: content genuinely corrected, but the client lies and claims
	// overridden=false. The hook must correct it back to true.
	spoofedFalse := map[string]any{}
	for k, v := range rows[1].Data {
		spoofedFalse[k] = v
	}
	spoofedFalse["depreciation_amount"] = 950.00
	spoofedFalse["overridden"] = false
	if _, err := fx.engine.Update(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[1].ID, spoofedFalse, nil, humanActor()); err != nil {
		t.Fatalf("update DepreciationSchedule row 1: %v", err)
	}
	got1, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), rows[1].ID)
	if err != nil {
		t.Fatalf("Get DepreciationSchedule row 1: %v", err)
	}
	if overridden, _ := got1.Data["overridden"].(bool); !overridden {
		t.Errorf("expected a client-claimed overridden=false on a genuinely-corrected row to be corrected back to true, got false")
	}
}

// TestMarkDepreciationScheduleOverriddenOnWrite_CreateRecomputesToo is
// uc-infra#236 independent review finding F5's regression pin: a
// DepreciationSchedule row CAN be created directly through the generic
// engine (a hand-add, CSV import, or any other non-generator path), not
// only via GenerateDepreciationScheduleOnWrite's own insertSchedule —
// this hook must give that row a correct, recomputed overridden value
// on Create too, not silently assume it's unreachable.
func TestMarkDepreciationScheduleOverriddenOnWrite_CreateRecomputesToo(t *testing.T) {
	fx := setUpScheduleHookFixture(t)
	ctx := context.Background()
	rec, err := fx.engine.Create(ctx, publishedDef(t, fx.tenantDB, "FixedAsset"), map[string]any{
		"asset_number": "FA-OV-5", "name": map[string]any{"en": "Override Test Asset"},
		"acquisition_date": "2026-01-15", "cost": 3000.0, "salvage_value": 0.0,
		"useful_life_months": 3.0, "depreciation_method": "straight_line",
		"currency_id":                         fx.currencyID,
		"asset_account_id":                    fx.assetAcct,
		"depreciation_expense_account_id":     fx.expAcct,
		"accumulated_depreciation_account_id": fx.accumAcct,
		"status_id":                           fx.statusID["in_service"],
	}, humanActor())
	if err != nil {
		t.Fatalf("create FixedAsset: %v", err)
	}
	// cost=3000, salvage=0, life=3, straight-line: sequence 1 is exactly
	// 1000.00/period_end 2026-02-28/book_value 2000.00 — read the real
	// generated row rather than hardcoding, so this test tracks Build's
	// own arithmetic instead of duplicating it.
	generated, err := fx.engine.ListByField(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), "fixed_asset_id", rec.ID)
	if err != nil || len(generated) != 3 {
		t.Fatalf("list generated DepreciationSchedule: err=%v n=%d", err, len(generated))
	}
	var seq1 map[string]any
	for _, r := range generated {
		if seq, _ := r.Data["sequence"].(float64); int(seq) == 1 {
			seq1 = r.Data
		}
	}
	if seq1 == nil {
		t.Fatal("no generated row at sequence 1")
	}

	// A directly-created row at a DUPLICATE sequence whose content
	// exactly matches what Build computes for sequence 1 — not an
	// override.
	matchingID := fx.createScheduleRow(t, rec.ID, 1,
		seq1["period_end"].(string), seq1["depreciation_amount"].(float64), seq1["book_value"].(float64))
	matching, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), matchingID)
	if err != nil {
		t.Fatalf("Get matching row: %v", err)
	}
	if overridden, _ := matching.Data["overridden"].(bool); overridden {
		t.Errorf("expected a directly-created row matching Build's own output to NOT be overridden, got true")
	}

	// A directly-created row at the same sequence whose amount does NOT
	// match — an override, exactly like a human correction would be.
	mismatchedID := fx.createScheduleRow(t, rec.ID, 1,
		seq1["period_end"].(string), 1.0, seq1["book_value"].(float64))
	mismatched, err := fx.engine.Get(ctx, publishedDef(t, fx.tenantDB, "DepreciationSchedule"), mismatchedID)
	if err != nil {
		t.Fatalf("Get mismatched row: %v", err)
	}
	if overridden, _ := mismatched.Data["overridden"].(bool); !overridden {
		t.Errorf("expected a directly-created row NOT matching Build's own output to be overridden, got false")
	}
}
