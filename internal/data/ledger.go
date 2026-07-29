package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GLAccountRepo is the repository for gl_accounts — the ledger core's own
// typed chart of accounts (ADR-0004), a projection of
// internal/kernel/finance.Account kept in sync by finance.SyncGLAccounts,
// never written to directly by the generic entity engine.
type GLAccountRepo struct {
	db querier
}

func NewGLAccountRepo(db querier) *GLAccountRepo {
	return &GLAccountRepo{db: db}
}

// UpsertByCode inserts or updates a gl_accounts row keyed by code (the
// same natural key finance.Account already uses) — the one write path
// ADR-0004 allows into this table, called only from
// finance.SyncGLAccounts, never from the generic crud.Engine. Each call
// is its own auto-committed statement — a sync run needs no atomicity
// across different accounts' rows, the same reasoning
// moduleseed.PublishAll's own per-item idempotent upserts already rely on.
func (r *GLAccountRepo) UpsertByCode(ctx context.Context, code, name, accountType, currency string, isActive bool) (id string, err error) {
	err = r.db.QueryRowContext(ctx, `
		INSERT INTO gl_accounts (code, name, account_type, currency, is_active)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (code) DO UPDATE SET
			name = EXCLUDED.name,
			account_type = EXCLUDED.account_type,
			currency = EXCLUDED.currency,
			is_active = EXCLUDED.is_active
		RETURNING id`,
		code, name, accountType, currency, isActive,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert gl_account %s: %w", code, err)
	}
	return id, nil
}

// IDByCode resolves a gl_accounts.id (and its is_active flag) from its
// code — what internal/kernel/ledger.Post uses to turn a Line's
// human-meaningful AccountID (a finance.Account code, e.g. "1100") into
// the real gl_accounts foreign key journal_lines requires, and to reject
// posting to a deactivated account (Post's own job, this method just
// returns the flag rather than deciding the policy — a lookup helper
// shouldn't also be where "is this allowed" logic lives). Returns
// ErrNotFound if no account with that code has been synced yet
// (finance.SyncGLAccounts hasn't run, or the code is simply wrong).
func (r *GLAccountRepo) IDByCode(ctx context.Context, code string) (id string, isActive bool, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT id, is_active FROM gl_accounts WHERE code = $1`, code).Scan(&id, &isActive)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("look up gl_account by code %s: %w", code, err)
	}
	return id, isActive, nil
}

// JournalEntry is one posted, immutable ledger entry — the read shape
// JournalEntryRepo.List returns. Never constructed by hand and passed
// back for a write; internal/kernel/ledger.Post is the only writer.
type JournalEntry struct {
	ID          string
	EntryDate   string
	Description string
	SourceType  string
	SourceID    string
	Lines       []JournalLine
}

// JournalLine is one debit/credit line of a JournalEntry, with the
// account's code/name denormalized alongside AccountID — the same
// "carry the display value, not just the FK" convention
// GoodsReceiptLine/CustomerInvoice already follow elsewhere in this
// kernel, so a caller listing entries doesn't need a second join per row.
type JournalLine struct {
	ID          string
	AccountID   string
	AccountCode string
	AccountName string
	DebitMinor  int64
	CreditMinor int64
}

// JournalEntryRepo is the repository for journal_entries/journal_lines —
// the ledger core's own typed, foreign-keyed tables (ADR-0004). Nothing
// outside internal/kernel/ledger.Post calls CreateTx; List exists for
// read-only visibility (a future report page, per ADR-0004's own
// "explicitly deferred" note).
type JournalEntryRepo struct {
	db querier
}

func NewJournalEntryRepo(db querier) *JournalEntryRepo {
	return &JournalEntryRepo{db: db}
}

// CreateTx inserts journal_entries and every journal_lines row — one
// INSERT per line, in a loop, not a single batched statement, but always
// within the caller's transaction, which is what actually matters:
// internal/kernel/ledger.Post's own doc comment covers the atomicity
// contract this serves (a journal entry can never exist with only some
// of its lines written, since the whole tx rolls back on any failure).
// accountIDs is parallel to lines: accountIDs[i] is the already-resolved
// gl_accounts.id for lines[i].AccountID (a code) — Post resolves every
// code before calling this, so this method never needs GLAccountRepo
// itself.
func (r *JournalEntryRepo) CreateTx(ctx context.Context, tx *sql.Tx, entryDate, description, sourceType, sourceID string, lineAccountIDs []string, lineDebits, lineCredits []int64) (id string, err error) {
	if len(lineAccountIDs) == 0 {
		return "", fmt.Errorf("create journal entry: no lines")
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO journal_entries (entry_date, description, source_type, source_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		entryDate, description, nullableString(sourceType), nullableID(sourceID),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert journal_entry: %w", err)
	}

	for i, accountID := range lineAccountIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO journal_lines (journal_entry_id, account_id, debit_minor, credit_minor)
			VALUES ($1, $2, $3, $4)`,
			id, accountID, lineDebits[i], lineCredits[i],
		); err != nil {
			return "", fmt.Errorf("insert journal_line %d: %w", i, err)
		}
	}
	return id, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// List returns every posted journal entry, newest first, with lines
// eager-loaded and account code/name denormalized — no pagination yet,
// same "fine at today's scale, revisit when it isn't" convention
// TenantRepo.ListIDs's own doc comment already uses elsewhere in this
// package.
func (r *JournalEntryRepo) List(ctx context.Context) ([]JournalEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, entry_date, description, coalesce(source_type, ''), coalesce(source_id::text, '')
		FROM journal_entries ORDER BY posted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list journal_entries: %w", err)
	}
	defer rows.Close()

	var entries []JournalEntry
	byID := map[string]*JournalEntry{}
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Description, &e.SourceType, &e.SourceID); err != nil {
			return nil, fmt.Errorf("scan journal_entry: %w", err)
		}
		entries = append(entries, e)
		byID[e.ID] = &entries[len(entries)-1]
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return entries, nil
	}

	lineRows, err := r.db.QueryContext(ctx, `
		SELECT jl.id, jl.journal_entry_id, jl.account_id, ga.code, ga.name, jl.debit_minor, jl.credit_minor
		FROM journal_lines jl JOIN gl_accounts ga ON ga.id = jl.account_id
		ORDER BY jl.id`)
	if err != nil {
		return nil, fmt.Errorf("list journal_lines: %w", err)
	}
	defer lineRows.Close()

	for lineRows.Next() {
		var entryID string
		var l JournalLine
		if err := lineRows.Scan(&l.ID, &entryID, &l.AccountID, &l.AccountCode, &l.AccountName, &l.DebitMinor, &l.CreditMinor); err != nil {
			return nil, fmt.Errorf("scan journal_line: %w", err)
		}
		if e, ok := byID[entryID]; ok {
			e.Lines = append(e.Lines, l)
		}
	}
	return entries, lineRows.Err()
}
