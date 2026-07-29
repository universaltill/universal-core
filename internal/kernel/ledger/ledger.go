// Package ledger is a deterministic core (ADR-0017 §1, §16): hand-written,
// human-reviewed, never AI-authored without review, never configured by
// metadata. Nothing outside this package posts a journal entry directly.
//
// Post (added under ADR-0004) is the one function that actually writes a
// journal entry — internal/db/migrations/tenant/0001_init.sql's
// gl_accounts/journal_entries/journal_lines tables have existed since the
// kernel spike but had no writer until now. ADR-0004 also explains why
// these are dedicated typed tables, not generic Entity Definitions: a
// generic Definition is itself a form of metadata configuration, which
// would contradict this package's own "never configured by metadata"
// exception.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// Line is one debit or credit line of a journal entry. AccountID is a
// gl_accounts.code (e.g. "1100"), not a raw UUID — the human-meaningful
// identifier a caller assembling an Entry actually has on hand (a
// finance.Account's own code), not something it should need to look up
// via GLAccountRepo itself first. Post resolves every code to its real
// gl_accounts id before writing. Exactly one of DebitMinor/CreditMinor
// must be non-zero — enforced both by Validate and by the database CHECK
// constraint on journal_lines (defence in depth).
type Line struct {
	AccountID   string
	DebitMinor  int64
	CreditMinor int64
}

// Entry is a proposed journal entry: a date, a description, and its lines.
// It does not exist in the ledger until Post succeeds. Date is an
// ISO-8601 date string ("2026-01-15"), matching this codebase's other
// FieldDate values — not a time.Time, so a caller building an Entry from
// already-string-shaped record data (e.g. a CustomerInvoice's
// invoice_date) never needs to parse and reformat it.
type Entry struct {
	Date        string
	Description string
	// SourceType/SourceID identify the business document this entry
	// posts for (e.g. "CustomerInvoice"/that record's real records.id) —
	// journal_entries.source_id is a typed UUID column, so SourceID must
	// be a real UUID or empty, never an arbitrary string like an
	// entity's natural-key number.
	SourceType string
	SourceID   string
	Lines      []Line
}

var (
	ErrUnbalanced      = errors.New("ledger: entry does not balance (sum of debits must equal sum of credits)")
	ErrNoLines         = errors.New("ledger: entry has no lines")
	ErrBadLine         = errors.New("ledger: a line must have exactly one of debit or credit set, both non-negative")
	ErrEmptyAcctID     = errors.New("ledger: line has an empty account id")
	ErrInactiveAccount = errors.New("ledger: cannot post to a deactivated account")
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

// Post validates e, resolves every line's account code to a real
// gl_accounts id, and writes journal_entries + every journal_lines row
// atomically in one transaction along with the audit entry (ADR-0017
// §14/§16: audit commits in the same transaction as the mutation, never
// bolted on after — the same discipline crud.Engine.Create already
// follows for generic records). Returns the new journal entry's id.
//
// An unresolvable account code (finance.SyncGLAccounts hasn't run for
// it, or the caller passed a typo) fails the whole Post before any write
// happens — a journal entry can never exist referencing an account this
// ledger doesn't actually know about, exactly the referential integrity
// gl_accounts' own foreign key from journal_lines exists to guarantee.
// A resolvable but deactivated account (gl_accounts.is_active = false)
// also fails the whole Post, before any write, for the same reason —
// standard double-entry practice forbids posting to a closed/deactivated
// account, and this was a real gap caught by independent review before
// Post had ever been given a real caller.
func Post(ctx context.Context, db *sql.DB, e Entry, actor audit.Actor) (journalEntryID string, err error) {
	if err := e.Validate(); err != nil {
		return "", err
	}

	accounts := data.NewGLAccountRepo(db)
	accountIDs := make([]string, len(e.Lines))
	debits := make([]int64, len(e.Lines))
	credits := make([]int64, len(e.Lines))
	for i, l := range e.Lines {
		id, isActive, err := accounts.IDByCode(ctx, l.AccountID)
		if err != nil {
			return "", fmt.Errorf("resolve account %q: %w", l.AccountID, err)
		}
		if !isActive {
			return "", fmt.Errorf("%w: account %q", ErrInactiveAccount, l.AccountID)
		}
		accountIDs[i] = id
		debits[i] = l.DebitMinor
		credits[i] = l.CreditMinor
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	entries := data.NewJournalEntryRepo(db)
	id, err := entries.CreateTx(ctx, tx, e.Date, e.Description, e.SourceType, e.SourceID, accountIDs, debits, credits)
	if err != nil {
		return "", fmt.Errorf("create journal entry: %w", err)
	}

	auditEntry, err := audit.New("JournalEntry", id, audit.ActionCreate, actor, map[string]any{
		"date": e.Date, "description": e.Description,
		"source_type": e.SourceType, "source_id": e.SourceID,
	})
	if err != nil {
		return "", fmt.Errorf("build audit entry: %w", err)
	}
	if err := data.NewAuditRepo(db).Insert(ctx, tx, auditEntry); err != nil {
		return "", fmt.Errorf("write audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return id, nil
}
