package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestGLAccountRepo_UpsertByCode_CreatesThenUpdates(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	id1, err := repo.UpsertByCode(ctx, "1000", "Assets", "asset", "USD", true)
	if err != nil {
		t.Fatalf("first UpsertByCode: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected a non-empty id")
	}

	// Second call with the same code but different name/currency must
	// update the existing row in place (ON CONFLICT DO UPDATE), not
	// create a second one — UNIQUE(code) would reject a duplicate insert
	// anyway, but this also confirms the update actually lands.
	id2, err := repo.UpsertByCode(ctx, "1000", "Total Assets", "asset", "GBP", false)
	if err != nil {
		t.Fatalf("second UpsertByCode: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("expected the same id on upsert, got %q then %q", id1, id2)
	}

	var name, currency string
	var isActive bool
	if err := db.QueryRowContext(ctx, `SELECT name, currency, is_active FROM gl_accounts WHERE id = $1`, id1).
		Scan(&name, &currency, &isActive); err != nil {
		t.Fatalf("read back gl_account: %v", err)
	}
	if name != "Total Assets" || currency != "GBP" || isActive {
		t.Fatalf("expected the update to land, got name=%q currency=%q is_active=%v", name, currency, isActive)
	}
}

// TestGLAccountRepo_UpsertBySourceRecord_CreatesThenUpdatesInPlace
// covers uc-infra#205's actual repository-level fix: two calls with the
// SAME sourceRecordID but a DIFFERENT code must update one row in
// place (same id), not create a second row — the mechanism that lets a
// renamed Account update its existing gl_accounts row instead of
// orphaning it.
func TestGLAccountRepo_UpsertBySourceRecord_CreatesThenUpdatesInPlace(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)
	const sourceRecordID = "11111111-1111-1111-1111-111111111111"

	id1, err := repo.UpsertBySourceRecord(ctx, sourceRecordID, "1000", "Assets", "asset", "USD", true)
	if err != nil {
		t.Fatalf("first UpsertBySourceRecord: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected a non-empty id")
	}

	// Same sourceRecordID, renamed code — the exact shape a real Account
	// rename produces.
	id2, err := repo.UpsertBySourceRecord(ctx, sourceRecordID, "1100", "Assets", "asset", "USD", true)
	if err != nil {
		t.Fatalf("second UpsertBySourceRecord (renamed code): %v", err)
	}
	if id2 != id1 {
		t.Fatalf("expected the rename to update the SAME row (id %q), got a different row (id %q)", id1, id2)
	}

	// The old code must no longer resolve to anything.
	if _, _, err := repo.IDByCode(ctx, "1000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected old code 1000 to no longer resolve after rename, got: %v", err)
	}
	gotID, isActive, err := repo.IDByCode(ctx, "1100")
	if err != nil {
		t.Fatalf("expected new code 1100 to resolve: %v", err)
	}
	if gotID != id1 || !isActive {
		t.Fatalf("expected new code to resolve to the same row (id=%q active=true), got id=%q active=%v", id1, gotID, isActive)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM gl_accounts WHERE source_record_id = $1`, sourceRecordID).
		Scan(&count); err != nil {
		t.Fatalf("count rows for source_record_id: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for this source_record_id, got %d", count)
	}
}

// TestGLAccountRepo_UpsertBySourceRecord_DoesNotCollideWithUpsertByCode
// confirms the two upsert paths coexist as designed: UpsertByCode
// (unrelated ledger-posting tests, no Account record to link to) keeps
// working unchanged alongside UpsertBySourceRecord (the real
// finance.Account sync paths) — different rows, both reachable by code.
func TestGLAccountRepo_UpsertBySourceRecord_DoesNotCollideWithUpsertByCode(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	codeOnlyID, err := repo.UpsertByCode(ctx, "2000", "Liabilities", "liability", "USD", true)
	if err != nil {
		t.Fatalf("UpsertByCode: %v", err)
	}

	linkedID, err := repo.UpsertBySourceRecord(ctx, "22222222-2222-2222-2222-222222222222", "3000", "Equity", "equity", "USD", true)
	if err != nil {
		t.Fatalf("UpsertBySourceRecord: %v", err)
	}
	if linkedID == codeOnlyID {
		t.Fatalf("expected two distinct rows, got the same id %q for both", linkedID)
	}

	var sourceRecordID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT source_record_id FROM gl_accounts WHERE id = $1`, codeOnlyID).
		Scan(&sourceRecordID); err != nil {
		t.Fatalf("read back UpsertByCode row's source_record_id: %v", err)
	}
	if sourceRecordID.Valid {
		t.Fatalf("expected UpsertByCode's row to have no source_record_id, got %q", sourceRecordID.String)
	}
}

// TestGLAccountRepo_UpsertBySourceRecord_CodeHeldByUnrelatedRow_FailsLoud
// covers uc-infra#205 review finding 4: ON CONFLICT (source_record_id)
// only arbitrates a collision on that column — it does nothing to stop
// the UPDATE it performs from itself violating the pre-existing
// UNIQUE(code) constraint against a DIFFERENT, unrelated row (the shape
// a pre-uc-infra#205 orphaned row, or any other row already sitting on
// that code, produces). This must fail loud with ErrGLAccountCodeConflict
// rather than silently reassigning the unrelated row's identity.
func TestGLAccountRepo_UpsertBySourceRecord_CodeHeldByUnrelatedRow_FailsLoud(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	// An unrelated, unlinked row already sitting on code "1100" — e.g.
	// a pre-fix orphan, or a row seeded via UpsertByCode.
	unrelatedID, err := repo.UpsertByCode(ctx, "1100", "Old Orphan", "asset", "USD", true)
	if err != nil {
		t.Fatalf("seed unrelated row: %v", err)
	}

	linkedID, err := repo.UpsertBySourceRecord(ctx, "33333333-3333-3333-3333-333333333333", "1000", "Assets", "asset", "USD", true)
	if err != nil {
		t.Fatalf("create linked row: %v", err)
	}

	// Renaming the linked row onto the code the unrelated row already
	// holds must fail, not silently succeed.
	if _, err := repo.UpsertBySourceRecord(ctx, "33333333-3333-3333-3333-333333333333", "1100", "Assets", "asset", "USD", true); !errors.Is(err, ErrGLAccountCodeConflict) {
		t.Fatalf("expected ErrGLAccountCodeConflict, got: %v", err)
	}

	// Neither row was touched by the failed attempt.
	var linkedCode, unrelatedCode string
	if err := db.QueryRowContext(ctx, `SELECT code FROM gl_accounts WHERE id = $1`, linkedID).Scan(&linkedCode); err != nil {
		t.Fatalf("read back linked row: %v", err)
	}
	if linkedCode != "1000" {
		t.Fatalf("expected the failed attempt to leave the linked row's code unchanged at 1000, got %q", linkedCode)
	}
	if err := db.QueryRowContext(ctx, `SELECT code FROM gl_accounts WHERE id = $1`, unrelatedID).Scan(&unrelatedCode); err != nil {
		t.Fatalf("read back unrelated row: %v", err)
	}
	if unrelatedCode != "1100" {
		t.Fatalf("expected the unrelated row's code to be untouched at 1100, got %q", unrelatedCode)
	}
}

func TestGLAccountRepo_IDByCode_NotFound(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	if _, _, err := repo.IDByCode(ctx, "9999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGLAccountRepo_IDByCode_ResolvesUpsertedAccount(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	id, err := repo.UpsertByCode(ctx, "2000", "Liabilities", "liability", "USD", true)
	if err != nil {
		t.Fatalf("UpsertByCode: %v", err)
	}
	got, isActive, err := repo.IDByCode(ctx, "2000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if got != id {
		t.Fatalf("expected IDByCode to resolve %q, got %q", id, got)
	}
	if !isActive {
		t.Fatal("expected is_active true, matching the upserted account")
	}
}

func TestGLAccountRepo_IDByCode_ReportsInactiveAccounts(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	if _, err := repo.UpsertByCode(ctx, "3000", "Retired Account", "asset", "USD", false); err != nil {
		t.Fatalf("UpsertByCode: %v", err)
	}
	_, isActive, err := repo.IDByCode(ctx, "3000")
	if err != nil {
		t.Fatalf("IDByCode: %v", err)
	}
	if isActive {
		t.Fatal("expected is_active false for a deactivated account")
	}
}

func TestJournalEntryRepo_CreateTx_WritesEntryAndLinesAtomically(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	accounts := NewGLAccountRepo(db)
	cashID, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert cash: %v", err)
	}
	revenueID, err := accounts.UpsertByCode(ctx, "4000", "Revenue", "income", "USD", true)
	if err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	entries := NewJournalEntryRepo(db)
	id, err := entries.CreateTx(ctx, tx, "2026-01-15", "Cash sale", "SalesOrder", "11111111-1111-1111-1111-111111111111",
		[]string{cashID, revenueID}, []int64{10000, 0}, []int64{0, 10000})
	if err != nil {
		t.Fatalf("CreateTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty journal entry id")
	}

	var lineCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM journal_lines WHERE journal_entry_id = $1`, id).Scan(&lineCount); err != nil {
		t.Fatalf("count journal_lines: %v", err)
	}
	if lineCount != 2 {
		t.Fatalf("expected 2 journal_lines, got %d", lineCount)
	}
}

func TestJournalEntryRepo_CreateTx_RollsBackOnFailure(t *testing.T) {
	// A bad account id (not a real UUID FK target) must fail the whole
	// insert batch — proving the entry never exists with only some of
	// its lines written, the atomicity CreateTx's own doc comment
	// promises.
	db := freshTenantDB(t)
	ctx := context.Background()
	accounts := NewGLAccountRepo(db)
	cashID, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert cash: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	entries := NewJournalEntryRepo(db)
	_, err = entries.CreateTx(ctx, tx, "2026-01-15", "Bad entry", "", "",
		[]string{cashID, "00000000-0000-0000-0000-000000000000"}, []int64{10000, 0}, []int64{0, 10000})
	if err == nil {
		t.Fatal("expected an error for a nonexistent account id")
	}
	_ = tx.Rollback()

	var entryCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries WHERE description = 'Bad entry'`).Scan(&entryCount); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if entryCount != 0 {
		t.Fatalf("expected the failed entry to not exist after rollback, found %d", entryCount)
	}
}

func TestJournalEntryRepo_List_ReturnsEntriesWithDenormalizedLines(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	accounts := NewGLAccountRepo(db)
	cashID, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert cash: %v", err)
	}
	revenueID, err := accounts.UpsertByCode(ctx, "4000", "Revenue", "income", "USD", true)
	if err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	entries := NewJournalEntryRepo(db)
	id, err := entries.CreateTx(ctx, tx, "2026-01-15", "Cash sale", "SalesOrder", "11111111-1111-1111-1111-111111111111",
		[]string{cashID, revenueID}, []int64{10000, 0}, []int64{0, 10000})
	if err != nil {
		t.Fatalf("CreateTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 journal entry, got %d", len(list))
	}
	got := list[0]
	if got.ID != id || got.Description != "Cash sale" || got.SourceType != "SalesOrder" || got.SourceID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("unexpected entry: %+v", got)
	}
	if len(got.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(got.Lines))
	}
	byCode := map[string]JournalLine{}
	for _, l := range got.Lines {
		byCode[l.AccountCode] = l
	}
	cashLine, ok := byCode["1100"]
	if !ok || cashLine.AccountName != "Cash" || cashLine.DebitMinor != 10000 || cashLine.CreditMinor != 0 {
		t.Fatalf("unexpected cash line: %+v (ok=%v)", cashLine, ok)
	}
	revenueLine, ok := byCode["4000"]
	if !ok || revenueLine.AccountName != "Revenue" || revenueLine.CreditMinor != 10000 || revenueLine.DebitMinor != 0 {
		t.Fatalf("unexpected revenue line: %+v (ok=%v)", revenueLine, ok)
	}
}

func TestJournalEntryRepo_List_EmptyWhenNoEntries(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	entries := NewJournalEntryRepo(db)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no entries, got %d", len(list))
	}
}

// seedSAFTLedger creates two accounts and three balanced journal entries
// (before, inside, and after a March-2026 reporting window) — the shared
// fixture for the ListRange/BalancesForRange tests below.
func seedSAFTLedger(t *testing.T, db *sql.DB) (bankID, revenueID string) {
	t.Helper()
	ctx := context.Background()
	accounts := NewGLAccountRepo(db)
	var err error
	bankID, err = accounts.UpsertByCode(ctx, "1100", "Bank", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert bank account: %v", err)
	}
	revenueID, err = accounts.UpsertByCode(ctx, "3000", "Revenue", "income", "USD", true)
	if err != nil {
		t.Fatalf("upsert revenue account: %v", err)
	}
	entries := NewJournalEntryRepo(db)
	for _, e := range []struct {
		date string
		amt  int64
	}{
		{"2026-02-10", 100_00}, // before the window
		{"2026-03-15", 25_50},  // inside it
		{"2026-04-01", 7_00},   // after it
	} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if _, err := entries.CreateTx(ctx, tx, e.date, "entry "+e.date, "", "",
			[]string{bankID, revenueID}, []int64{e.amt, 0}, []int64{0, e.amt}); err != nil {
			t.Fatalf("create journal entry %s: %v", e.date, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit journal entry %s: %v", e.date, err)
		}
	}
	return bankID, revenueID
}

func TestJournalEntryRepo_ListRange_FiltersAndOrdersOldestFirst(t *testing.T) {
	db := freshTenantDB(t)
	seedSAFTLedger(t, db)
	ctx := context.Background()

	got, err := NewJournalEntryRepo(db).ListRange(ctx, "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatalf("ListRange: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the in-window entry, got %d entries", len(got))
	}
	e := got[0]
	if e.EntryDate != "2026-03-15" || e.Description != "entry 2026-03-15" {
		t.Fatalf("wrong entry returned: %+v", e)
	}
	if e.PostedAt == "" {
		t.Fatal("ListRange must populate PostedAt (SystemEntryDate source)")
	}
	if len(e.Lines) != 2 {
		t.Fatalf("expected the entry's 2 lines eager-loaded, got %d", len(e.Lines))
	}
	if e.Lines[0].AccountCode != "1100" || e.Lines[0].DebitMinor != 25_50 {
		t.Fatalf("wrong first line: %+v", e.Lines[0])
	}

	// A wider window returns all three, oldest first (audit-file order).
	all, err := NewJournalEntryRepo(db).ListRange(ctx, "2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatalf("ListRange (wide): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}
	if all[0].EntryDate != "2026-02-10" || all[2].EntryDate != "2026-04-01" {
		t.Fatalf("expected oldest-first ordering, got %s ... %s", all[0].EntryDate, all[2].EntryDate)
	}
	// EVERY entry must keep its lines in a multi-entry result — the
	// regression assertion for the append-reallocation aliasing bug
	// (#28's independent review): with element pointers instead of
	// index-based attachment, all but the last entry came back
	// line-less, and a single-entry fixture can never notice.
	for i, e := range all {
		if len(e.Lines) != 2 {
			t.Fatalf("entry %d (%s) lost its lines in a multi-entry ListRange: got %d, want 2", i, e.EntryDate, len(e.Lines))
		}
	}

	// An empty window is an empty, non-error result.
	none, err := NewJournalEntryRepo(db).ListRange(ctx, "2020-01-01", "2020-12-31")
	if err != nil {
		t.Fatalf("ListRange (empty): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no entries, got %d", len(none))
	}
}

func TestGLAccountRepo_BalancesForRange_OpeningExcludesClosingIncludes(t *testing.T) {
	db := freshTenantDB(t)
	seedSAFTLedger(t, db)
	ctx := context.Background()
	// A third account with no activity at all must still appear (an
	// audit file lists the whole chart).
	if _, err := NewGLAccountRepo(db).UpsertByCode(ctx, "9999", "Dormant", "expense", "USD", true); err != nil {
		t.Fatalf("upsert dormant account: %v", err)
	}

	balances, err := NewGLAccountRepo(db).BalancesForRange(ctx, "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatalf("BalancesForRange: %v", err)
	}
	byCode := map[string]GLAccountBalance{}
	for _, b := range balances {
		byCode[b.Code] = b
	}
	if len(balances) != 3 {
		t.Fatalf("expected all 3 accounts, got %d", len(balances))
	}
	bank := byCode["1100"]
	// Opening: only the Feb entry (100.00 debit). Closing: Feb + Mar
	// (125.50 debit) — the April entry is outside the window on both.
	if bank.OpeningMinor != 100_00 || bank.ClosingMinor != 125_50 {
		t.Fatalf("bank balances wrong: %+v", bank)
	}
	rev := byCode["3000"]
	if rev.OpeningMinor != -100_00 || rev.ClosingMinor != -125_50 {
		t.Fatalf("revenue balances wrong (should be credit-side/negative nets): %+v", rev)
	}
	dormant := byCode["9999"]
	if dormant.OpeningMinor != 0 || dormant.ClosingMinor != 0 || dormant.AccountType != "expense" {
		t.Fatalf("dormant account wrong: %+v", dormant)
	}
}

func TestGLAccountRepo_DistinctCurrencies(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	repo := NewGLAccountRepo(db)

	got, err := repo.DistinctCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies (empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no currencies on an empty chart, got %v", got)
	}

	for i, c := range []string{"USD", "USD", "QAR"} {
		if _, err := repo.UpsertByCode(ctx, fmt.Sprintf("%d000", i+1), "Acct", "asset", c, true); err != nil {
			t.Fatalf("upsert account %d: %v", i, err)
		}
	}
	got, err = repo.DistinctCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies: %v", err)
	}
	if len(got) != 2 || got[0] != "QAR" || got[1] != "USD" {
		t.Fatalf("expected [QAR USD], got %v", got)
	}
}

// TestJournalEntryRepo_List_MultiEntryKeepsAllLines is List's own
// regression test for the same aliasing bug ListRange's wide-window
// assertion covers — List had the identical element-pointer pattern
// (pre-existing, caught in #28's independent review) and its original
// test seeded exactly one entry, the one shape that can't expose it.
func TestJournalEntryRepo_List_MultiEntryKeepsAllLines(t *testing.T) {
	db := freshTenantDB(t)
	seedSAFTLedger(t, db)

	entries, err := NewJournalEntryRepo(db).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	for i, e := range entries {
		if len(e.Lines) != 2 {
			t.Fatalf("entry %d (%s) lost its lines in a multi-entry List: got %d, want 2", i, e.EntryDate, len(e.Lines))
		}
	}
}
