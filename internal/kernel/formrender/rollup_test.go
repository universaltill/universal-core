package formrender

import "testing"

func TestComputeRollUp_SumsAcrossChildren(t *testing.T) {
	children := []map[string]any{{"line_total": 10.0}, {"line_total": 20.5}}
	total, err := computeRollUp(children, "line_total", false)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 30.5 {
		t.Fatalf("expected 30.5, got %v", total)
	}
}

func TestComputeRollUp_SkipsChildrenMissingTheField(t *testing.T) {
	children := []map[string]any{{"line_total": 10.0}, {"other": 5.0}}
	total, err := computeRollUp(children, "line_total", false)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 10.0 {
		t.Fatalf("expected 10.0, got %v", total)
	}
}

func TestComputeRollUp_ErrorsOnNonNumericField(t *testing.T) {
	children := []map[string]any{{"line_total": "not a number"}}
	if _, err := computeRollUp(children, "line_total", false); err == nil {
		t.Fatal("expected error for a non-numeric roll_up field")
	}
}

func TestComputeRollUp_EmptyChildrenIsZero(t *testing.T) {
	total, err := computeRollUp(nil, "line_total", false)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 0 {
		t.Fatalf("expected 0, got %v", total)
	}
}

// TestComputeRollUp_Money_SumsExactMinorUnits (uc-infra#136): the
// isMoney branch must sum via money.Money (int64), not float64 — pinning
// this against a case that would still happen to be exact under plain
// float64 addition (since realistic minor-unit amounts are small whole
// numbers) is weaker than it looks, but the real point is that the
// VALUE parsing goes through money.FromAny, so a fractional (legacy,
// un-backfilled) child value is rejected here rather than silently
// summed as if it were already-correct minor units.
func TestComputeRollUp_Money_SumsExactMinorUnits(t *testing.T) {
	children := []map[string]any{{"line_total": 1050.0}, {"line_total": 950.0}}
	total, err := computeRollUp(children, "line_total", true)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 2000.0 {
		t.Fatalf("expected 2000 (minor units), got %v", total)
	}
}

func TestComputeRollUp_Money_SkipsChildrenMissingTheField(t *testing.T) {
	children := []map[string]any{{"line_total": 1050.0}, {"other": 5.0}}
	total, err := computeRollUp(children, "line_total", true)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 1050.0 {
		t.Fatalf("expected 1050, got %v", total)
	}
}

// TestComputeRollUp_Money_SkipsFractionalValueRatherThanErroring
// (independent review of uc-infra#136's first pass): a fractional value
// is never a valid FieldMoney amount (money.FromAny's own whole-number
// gate) — but a legacy, not-yet-backfilled child row is ORDINARY,
// EXPECTED mid-migration data, not corruption, so it must be SKIPPED
// (excluded from the sum) rather than failing the whole render. Erroring
// here would turn GET /forms/PurchaseOrder/{id} into a permanent 500 for
// any tenant with an un-backfilled POLine (the first version of this fix
// did exactly that, caught by independent review): see this function's
// own doc comment for why that's a materially worse regression than an
// under-counted total.
func TestComputeRollUp_Money_SkipsFractionalValueRatherThanErroring(t *testing.T) {
	children := []map[string]any{{"line_total": 10.5}, {"line_total": 1050.0}}
	total, err := computeRollUp(children, "line_total", true)
	if err != nil {
		t.Fatalf("computeRollUp must not error on a legacy fractional child value: %v", err)
	}
	if total != 1050.0 {
		t.Fatalf("expected the fractional child excluded and the valid one summed (1050), got %v", total)
	}
}

// TestComputeRollUp_Money_SkipsNonNumericFieldRatherThanErroring is
// TestComputeRollUp_Money_SkipsFractionalValueRatherThanErroring's
// counterpart for a value that isn't even a number at all.
func TestComputeRollUp_Money_SkipsNonNumericFieldRatherThanErroring(t *testing.T) {
	children := []map[string]any{{"line_total": "not a number"}, {"line_total": 1050.0}}
	total, err := computeRollUp(children, "line_total", true)
	if err != nil {
		t.Fatalf("computeRollUp must not error on a non-numeric money roll_up field: %v", err)
	}
	if total != 1050.0 {
		t.Fatalf("expected the non-numeric child excluded and the valid one summed (1050), got %v", total)
	}
}

func TestComputeRollUp_Money_EmptyChildrenIsZero(t *testing.T) {
	total, err := computeRollUp(nil, "line_total", true)
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 0 {
		t.Fatalf("expected 0, got %v", total)
	}
}
