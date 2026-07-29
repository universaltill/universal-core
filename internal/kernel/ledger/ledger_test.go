package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/kernel/audit"
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

func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_ledger_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func TestPost_WritesBalancedEntryAndAuditRow(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	accounts := data.NewGLAccountRepo(tenantDB)
	if _, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true); err != nil {
		t.Fatalf("upsert cash: %v", err)
	}
	if _, err := accounts.UpsertByCode(ctx, "4000", "Revenue", "income", "USD", true); err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}

	id, err := Post(ctx, tenantDB, Entry{
		Date:        "2026-01-15",
		Description: "Cash sale",
		SourceType:  "SalesOrder",
		SourceID:    "11111111-1111-1111-1111-111111111111",
		Lines: []Line{
			{AccountID: "1100", DebitMinor: 10000},
			{AccountID: "4000", CreditMinor: 10000},
		},
	}, humanActor())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty journal entry id")
	}

	entries := data.NewJournalEntryRepo(tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("expected exactly the posted entry to be listed, got %+v", list)
	}

	var auditCount int
	if err := tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'JournalEntry' AND record_id = $1 AND action = 'create' AND actor_type = 'human'`,
		id,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 audit row for the posted entry, got %d", auditCount)
	}
}

// TestPost_UnbalancedEntry_FailsBeforeAnyWrite confirms Validate runs
// before Post ever opens a transaction — no partial write, no orphaned
// journal_entries row, for an entry that should never have been posted
// at all.
func TestPost_UnbalancedEntry_FailsBeforeAnyWrite(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	accounts := data.NewGLAccountRepo(tenantDB)
	if _, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true); err != nil {
		t.Fatalf("upsert cash: %v", err)
	}

	_, err := Post(ctx, tenantDB, Entry{
		Description: "Unbalanced",
		Lines:       []Line{{AccountID: "1100", DebitMinor: 100}},
	}, humanActor())
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no journal_entries written, got %d", count)
	}
}

// TestPost_UnresolvableAccountCode_FailsBeforeAnyWrite confirms a typo'd
// or never-synced account code fails the whole Post before any write —
// this ledger can never end up with a journal entry referencing an
// account it doesn't actually recognize.
func TestPost_UnresolvableAccountCode_FailsBeforeAnyWrite(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	accounts := data.NewGLAccountRepo(tenantDB)
	if _, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true); err != nil {
		t.Fatalf("upsert cash: %v", err)
	}

	_, err := Post(ctx, tenantDB, Entry{
		Description: "Bad account",
		Lines: []Line{
			{AccountID: "1100", DebitMinor: 100},
			{AccountID: "9999-does-not-exist", CreditMinor: 100},
		},
	}, humanActor())
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected data.ErrNotFound (wrapped), got %v", err)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no journal_entries written, got %d", count)
	}
}

// TestPost_InactiveAccount_FailsBeforeAnyWrite confirms standard
// double-entry practice: posting to a deactivated account is rejected,
// not silently allowed just because the account still resolves. A real
// gap caught by independent review — Post's first version had no
// is_active check at all.
func TestPost_InactiveAccount_FailsBeforeAnyWrite(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	accounts := data.NewGLAccountRepo(tenantDB)
	if _, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true); err != nil {
		t.Fatalf("upsert cash: %v", err)
	}
	if _, err := accounts.UpsertByCode(ctx, "9000", "Retired Suspense", "asset", "USD", false); err != nil {
		t.Fatalf("upsert retired account: %v", err)
	}

	_, err := Post(ctx, tenantDB, Entry{
		Description: "Posting to a closed account",
		Lines: []Line{
			{AccountID: "1100", DebitMinor: 100},
			{AccountID: "9000", CreditMinor: 100},
		},
	}, humanActor())
	if !errors.Is(err, ErrInactiveAccount) {
		t.Fatalf("expected ErrInactiveAccount, got %v", err)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no journal_entries written, got %d", count)
	}
}
