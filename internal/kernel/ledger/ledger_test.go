package ledger

import (
	"errors"
	"math/rand"
	"testing"
)

func TestEntryValidate_Balanced(t *testing.T) {
	e := Entry{
		Description: "Vendor payment",
		Lines: []Line{
			{AccountID: "AP", DebitMinor: 10000},
			{AccountID: "Cash", CreditMinor: 10000},
		},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("expected balanced entry to validate, got %v", err)
	}
}

func TestEntryValidate_MultiLineBalanced(t *testing.T) {
	// One debit split across two credit lines (e.g. partial cash + partial AP).
	e := Entry{
		Lines: []Line{
			{AccountID: "Inventory", DebitMinor: 15000},
			{AccountID: "Cash", CreditMinor: 5000},
			{AccountID: "AP", CreditMinor: 10000},
		},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("expected balanced multi-line entry to validate, got %v", err)
	}
}

func TestEntryValidate_Unbalanced(t *testing.T) {
	e := Entry{
		Lines: []Line{
			{AccountID: "AP", DebitMinor: 10000},
			{AccountID: "Cash", CreditMinor: 9999},
		},
	}
	err := e.Validate()
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}
}

func TestEntryValidate_NoLines(t *testing.T) {
	e := Entry{}
	if err := e.Validate(); !errors.Is(err, ErrNoLines) {
		t.Fatalf("expected ErrNoLines, got %v", err)
	}
}

func TestEntryValidate_LineWithBothDebitAndCredit(t *testing.T) {
	e := Entry{
		Lines: []Line{
			{AccountID: "AP", DebitMinor: 100, CreditMinor: 100},
			{AccountID: "Cash", CreditMinor: 100},
		},
	}
	if err := e.Validate(); !errors.Is(err, ErrBadLine) {
		t.Fatalf("expected ErrBadLine, got %v", err)
	}
}

func TestEntryValidate_LineWithNeitherDebitNorCredit(t *testing.T) {
	e := Entry{
		Lines: []Line{
			{AccountID: "AP"},
			{AccountID: "Cash", CreditMinor: 100},
		},
	}
	if err := e.Validate(); !errors.Is(err, ErrBadLine) {
		t.Fatalf("expected ErrBadLine, got %v", err)
	}
}

func TestEntryValidate_NegativeAmount(t *testing.T) {
	e := Entry{
		Lines: []Line{
			{AccountID: "AP", DebitMinor: -100},
			{AccountID: "Cash", CreditMinor: 100},
		},
	}
	if err := e.Validate(); !errors.Is(err, ErrBadLine) {
		t.Fatalf("expected ErrBadLine for negative amount, got %v", err)
	}
}

func TestEntryValidate_EmptyAccountID(t *testing.T) {
	e := Entry{
		Lines: []Line{
			{AccountID: "", DebitMinor: 100},
			{AccountID: "Cash", CreditMinor: 100},
		},
	}
	if err := e.Validate(); !errors.Is(err, ErrEmptyAcctID) {
		t.Fatalf("expected ErrEmptyAcctID, got %v", err)
	}
}

// Property test: for any randomly generated set of lines that we construct
// to balance by design, Validate must always accept them; if we then
// perturb one amount, it must always reject. This is the invariant the
// entire ledger's trustworthiness rests on.
func TestEntryValidate_BalancedRandomEntriesAlwaysPass(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for range 200 {
		numLines := 2 + rng.Intn(6)
		var total int64
		lines := make([]Line, 0, numLines)
		// all lines but the last are random debits
		for range numLines - 1 {
			amt := int64(1 + rng.Intn(1_000_000))
			total += amt
			lines = append(lines, Line{AccountID: "acct-debit", DebitMinor: amt})
		}
		// last line is a single credit balancing the total
		lines = append(lines, Line{AccountID: "acct-credit", CreditMinor: total})

		e := Entry{Lines: lines}
		if err := e.Validate(); err != nil {
			t.Fatalf("balanced random entry rejected: %v (lines=%+v)", err, lines)
		}

		// Now perturb: bump the credit line by 1 minor unit and confirm
		// it is always rejected as unbalanced.
		lines[len(lines)-1].CreditMinor++
		e2 := Entry{Lines: lines}
		if err := e2.Validate(); !errors.Is(err, ErrUnbalanced) {
			t.Fatalf("perturbed entry expected ErrUnbalanced, got %v (lines=%+v)", err, lines)
		}
	}
}
