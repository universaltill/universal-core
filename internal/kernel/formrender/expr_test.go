package formrender

import "testing"

func TestEvalVisibleIf_EmptyExpressionAlwaysVisible(t *testing.T) {
	visible, err := evalVisibleIf("", nil)
	if err != nil || !visible {
		t.Fatalf("expected empty expression to be visible with no error, got visible=%v err=%v", visible, err)
	}
}

func TestEvalVisibleIf_StringEquality(t *testing.T) {
	fields := map[string]any{"payment_method": "LC"}
	visible, err := evalVisibleIf("payment_method == 'LC'", fields)
	if err != nil || !visible {
		t.Fatalf("expected match to be visible, got visible=%v err=%v", visible, err)
	}
	visible, err = evalVisibleIf("payment_method == 'Wire'", fields)
	if err != nil || visible {
		t.Fatalf("expected mismatch to be hidden, got visible=%v err=%v", visible, err)
	}
}

func TestEvalVisibleIf_NotEqual(t *testing.T) {
	fields := map[string]any{"payment_method": "Wire"}
	visible, err := evalVisibleIf("payment_method != 'LC'", fields)
	if err != nil || !visible {
		t.Fatalf("expected != to be visible when values differ, got visible=%v err=%v", visible, err)
	}
}

func TestEvalVisibleIf_BoolLiteral(t *testing.T) {
	fields := map[string]any{"is_urgent": true}
	visible, err := evalVisibleIf("is_urgent == true", fields)
	if err != nil || !visible {
		t.Fatalf("expected bool literal match to be visible, got visible=%v err=%v", visible, err)
	}
}

func TestEvalVisibleIf_NumberLiteral(t *testing.T) {
	fields := map[string]any{"quantity": 5.0}
	visible, err := evalVisibleIf("quantity == 5", fields)
	if err != nil || !visible {
		t.Fatalf("expected number literal match to be visible, got visible=%v err=%v", visible, err)
	}
}

func TestEvalVisibleIf_MissingFieldIsNotEqual(t *testing.T) {
	visible, err := evalVisibleIf("payment_method == 'LC'", map[string]any{})
	if err != nil || visible {
		t.Fatalf("expected a field absent from the record to compare unequal, got visible=%v err=%v", visible, err)
	}
}

// TestEvalVisibleIf_NotEqualWithEqualsSignsInLiteral is the regression
// test for a parser bug: searching for "==" unconditionally before "!="
// misparsed "status != 'a==b'" by matching the "==" inside the quoted
// literal, corrupting the field name and the literal. The correct
// operator is whichever occurs first in the expression.
func TestEvalVisibleIf_NotEqualWithEqualsSignsInLiteral(t *testing.T) {
	fields := map[string]any{"status": "a==b"}
	visible, err := evalVisibleIf("status != 'a==b'", fields)
	if err != nil {
		t.Fatalf("evalVisibleIf: %v", err)
	}
	if visible {
		t.Fatalf("expected status == 'a==b' to make != false (hidden), got visible=true")
	}

	visible, err = evalVisibleIf("status != 'c==d'", fields)
	if err != nil {
		t.Fatalf("evalVisibleIf: %v", err)
	}
	if !visible {
		t.Fatalf("expected status != 'c==d' to be true (visible) when status is 'a==b'")
	}
}

func TestEvalVisibleIf_RejectsUnsupportedOperator(t *testing.T) {
	if _, err := evalVisibleIf("quantity > 5", nil); err == nil {
		t.Fatal("expected error: only == and != are supported, not a general expression language")
	}
}

func TestEvalVisibleIf_RejectsUnquotedLiteral(t *testing.T) {
	if _, err := evalVisibleIf("payment_method == LC", nil); err == nil {
		t.Fatal("expected error: bare word literal that isn't true/false must be rejected")
	}
}
