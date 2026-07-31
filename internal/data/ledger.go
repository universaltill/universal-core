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
	// PostedAt is posted_at's date part (ISO-8601 date string) — only
	// populated by ListRange, which needs it for the SAF-T export's
	// SystemEntryDate; List predates the field and doesn't select it.
	PostedAt string
	Lines    []JournalLine
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

// ExistsForSource reports whether a journal entry has already been
// posted for (sourceType, sourceID) — the idempotency guard a hook
// posting on a status transition needs (e.g. CustomerInvoice draft->
// issued only ever posts once, even if the same transition is somehow
// driven twice), since crud.Engine's Update hook has no other way to
// know "have I already done this" without re-deriving it here.
func (r *JournalEntryRepo) ExistsForSource(ctx context.Context, sourceType, sourceID string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM journal_entries WHERE source_type = $1 AND source_id = $2)`,
		sourceType, sourceID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check existing journal entry for %s %s: %w", sourceType, sourceID, err)
	}
	return exists, nil
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
	// Index by position, never by &entries[i]: append reallocates the
	// backing array as it grows, so element pointers taken mid-loop end
	// up pointing into orphaned arrays and every line attached through
	// them is silently lost (real bug, caught by #28's independent
	// review: with 2+ entries all but the last came back line-less).
	byID := map[string]int{}
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Description, &e.SourceType, &e.SourceID); err != nil {
			return nil, fmt.Errorf("scan journal_entry: %w", err)
		}
		entries = append(entries, e)
		byID[e.ID] = len(entries) - 1
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
		if i, ok := byID[entryID]; ok {
			entries[i].Lines = append(entries[i].Lines, l)
		}
	}
	return entries, lineRows.Err()
}

// GLAccountBalance is one chart-of-accounts row with its opening and
// closing balance for a date range — the read shape the SAF-T export
// (internal/kernel/saft) builds its GeneralLedgerAccounts section from.
// OpeningMinor/ClosingMinor are signed nets (debits minus credits):
// opening sums every line dated strictly before the range, closing sums
// through its last day, so closing == opening + in-range activity by
// construction.
type GLAccountBalance struct {
	Code         string
	Name         string
	AccountType  string
	OpeningMinor int64
	ClosingMinor int64
}

// BalancesForRange returns every gl_accounts row (zero-activity accounts
// included — an audit file's account list is the whole chart, not just
// the accounts that moved) with signed opening/closing nets for the
// inclusive [from, to] date range, ordered by code. from/to are ISO-8601
// date strings, matching entry_date's own DATE column.
func (r *GLAccountRepo) BalancesForRange(ctx context.Context, from, to string) ([]GLAccountBalance, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT ga.code, ga.name, ga.account_type,
		       COALESCE(SUM(CASE WHEN je.entry_date < $1::date THEN jl.debit_minor - jl.credit_minor ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN je.entry_date <= $2::date THEN jl.debit_minor - jl.credit_minor ELSE 0 END), 0)
		FROM gl_accounts ga
		LEFT JOIN journal_lines jl ON jl.account_id = ga.id
		LEFT JOIN journal_entries je ON je.id = jl.journal_entry_id
		GROUP BY ga.code, ga.name, ga.account_type
		ORDER BY ga.code`,
		from, to)
	if err != nil {
		return nil, fmt.Errorf("query gl account balances: %w", err)
	}
	defer rows.Close()

	var balances []GLAccountBalance
	for rows.Next() {
		var b GLAccountBalance
		if err := rows.Scan(&b.Code, &b.Name, &b.AccountType, &b.OpeningMinor, &b.ClosingMinor); err != nil {
			return nil, fmt.Errorf("scan gl account balance: %w", err)
		}
		balances = append(balances, b)
	}
	return balances, rows.Err()
}

// DistinctCurrencies returns the distinct gl_accounts.currency values,
// ordered — how the SAF-T export decides whether the ledger is
// single-currency (exactly one value: that is the file's
// DefaultCurrencyCode) or needs the documented fallback.
func (r *GLAccountRepo) DistinctCurrencies(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT currency FROM gl_accounts ORDER BY currency`)
	if err != nil {
		return nil, fmt.Errorf("query distinct gl currencies: %w", err)
	}
	defer rows.Close()
	var currencies []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan gl currency: %w", err)
		}
		currencies = append(currencies, c)
	}
	return currencies, rows.Err()
}

// ListRange returns every journal entry whose entry_date falls in the
// inclusive [from, to] range, oldest first (an audit file reads
// chronologically, unlike List's newest-first report ordering), with
// lines eager-loaded and posted_at's date part carried as PostedAt —
// the SAF-T export's SystemEntryDate. Same two-query eager-load shape
// as List; the lines query is scoped to the same range so an entry
// outside it never contributes rows.
func (r *JournalEntryRepo) ListRange(ctx context.Context, from, to string) ([]JournalEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, entry_date::text, description, coalesce(source_type, ''), coalesce(source_id::text, ''),
		       to_char(posted_at, 'YYYY-MM-DD')
		FROM journal_entries
		WHERE entry_date >= $1::date AND entry_date <= $2::date
		ORDER BY entry_date, posted_at, id`,
		from, to)
	if err != nil {
		return nil, fmt.Errorf("list journal_entries in range: %w", err)
	}
	defer rows.Close()

	var entries []JournalEntry
	// Position index, not element pointers — see List's own comment on
	// the append-reallocation aliasing bug.
	byID := map[string]int{}
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Description, &e.SourceType, &e.SourceID, &e.PostedAt); err != nil {
			return nil, fmt.Errorf("scan journal_entry: %w", err)
		}
		entries = append(entries, e)
		byID[e.ID] = len(entries) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return entries, nil
	}

	lineRows, err := r.db.QueryContext(ctx, `
		SELECT jl.id, jl.journal_entry_id, jl.account_id, ga.code, ga.name, jl.debit_minor, jl.credit_minor
		FROM journal_lines jl
		JOIN gl_accounts ga ON ga.id = jl.account_id
		JOIN journal_entries je ON je.id = jl.journal_entry_id
		WHERE je.entry_date >= $1::date AND je.entry_date <= $2::date
		ORDER BY ga.code, jl.id`,
		from, to)
	if err != nil {
		return nil, fmt.Errorf("list journal_lines in range: %w", err)
	}
	defer lineRows.Close()

	for lineRows.Next() {
		var entryID string
		var l JournalLine
		if err := lineRows.Scan(&l.ID, &entryID, &l.AccountID, &l.AccountCode, &l.AccountName, &l.DebitMinor, &l.CreditMinor); err != nil {
			return nil, fmt.Errorf("scan journal_line: %w", err)
		}
		if i, ok := byID[entryID]; ok {
			entries[i].Lines = append(entries[i].Lines, l)
		}
	}
	return entries, lineRows.Err()
}
