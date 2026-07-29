package data

import (
	"context"
	"errors"
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
