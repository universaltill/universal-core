// Package ledger is a deterministic core (ADR-0017 §1, §16): hand-written,
// human-reviewed, never AI-authored without review, never configured by
// metadata. Nothing outside this package posts a journal entry directly.
package ledger

import (
	"errors"
	"fmt"
)

// Line is one debit or credit line of a journal entry. Exactly one of
// DebitMinor/CreditMinor must be non-zero — enforced both here and by the
// database CHECK constraint on journal_lines (defence in depth).
type Line struct {
	AccountID   string
	DebitMinor  int64
	CreditMinor int64
}

// Entry is a proposed journal entry: a date, a description, and its lines.
// It does not exist in the ledger until Post succeeds.
type Entry struct {
	Description string
	SourceType  string
	SourceID    string
	Lines       []Line
}

var (
	ErrUnbalanced  = errors.New("ledger: entry does not balance (sum of debits must equal sum of credits)")
	ErrNoLines     = errors.New("ledger: entry has no lines")
	ErrBadLine     = errors.New("ledger: a line must have exactly one of debit or credit set, both non-negative")
	ErrEmptyAcctID = errors.New("ledger: line has an empty account id")
)

// Validate checks the double-entry invariant without touching storage.
// This is the single function every code path — human or AI-drafted
// workflow — must pass through before a posting is accepted; there is no
// bypass.
func (e *Entry) Validate() error {
	if len(e.Lines) == 0 {
		return ErrNoLines
	}
	var totalDebit, totalCredit int64
	for i, l := range e.Lines {
		if l.AccountID == "" {
			return fmt.Errorf("line %d: %w", i, ErrEmptyAcctID)
		}
		if l.DebitMinor < 0 || l.CreditMinor < 0 {
			return fmt.Errorf("line %d: %w", i, ErrBadLine)
		}
		if (l.DebitMinor == 0) == (l.CreditMinor == 0) {
			// both zero, or both non-zero — neither is valid
			return fmt.Errorf("line %d: %w", i, ErrBadLine)
		}
		totalDebit += l.DebitMinor
		totalCredit += l.CreditMinor
	}
	if totalDebit != totalCredit {
		return fmt.Errorf("%w: debits=%d credits=%d", ErrUnbalanced, totalDebit, totalCredit)
	}
	return nil
}
