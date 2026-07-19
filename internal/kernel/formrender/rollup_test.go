package formrender

import "testing"

func TestComputeRollUp_SumsAcrossChildren(t *testing.T) {
	children := []map[string]any{{"line_total": 10.0}, {"line_total": 20.5}}
	total, err := computeRollUp(children, "line_total")
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 30.5 {
		t.Fatalf("expected 30.5, got %v", total)
	}
}

func TestComputeRollUp_SkipsChildrenMissingTheField(t *testing.T) {
	children := []map[string]any{{"line_total": 10.0}, {"other": 5.0}}
	total, err := computeRollUp(children, "line_total")
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 10.0 {
		t.Fatalf("expected 10.0, got %v", total)
	}
}

func TestComputeRollUp_ErrorsOnNonNumericField(t *testing.T) {
	children := []map[string]any{{"line_total": "not a number"}}
	if _, err := computeRollUp(children, "line_total"); err == nil {
		t.Fatal("expected error for a non-numeric roll_up field")
	}
}

func TestComputeRollUp_EmptyChildrenIsZero(t *testing.T) {
	total, err := computeRollUp(nil, "line_total")
	if err != nil {
		t.Fatalf("computeRollUp: %v", err)
	}
	if total != 0 {
		t.Fatalf("expected 0, got %v", total)
	}
}
