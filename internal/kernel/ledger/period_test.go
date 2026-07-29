package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
)

// setUpPeriodFixture publishes finance (Period/FiscalYear/Account live
// there), seeds the two gl_accounts a test entry posts to, and creates a
// FiscalYear + one Period with the given status covering
// [startDate, endDate]. Returns the tenant db (what Post/PostTx need).
func setUpPeriodFixture(t *testing.T, status, startDate, endDate string) *sql.DB {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := finance.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}

	accounts := data.NewGLAccountRepo(tenantDB)
	if _, err := accounts.UpsertByCode(ctx, "1100", "Cash", "asset", "USD", true); err != nil {
		t.Fatalf("upsert cash: %v", err)
	}
	if _, err := accounts.UpsertByCode(ctx, "4000", "Revenue", "income", "USD", true); err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	fy, err := engine.Create(ctx, def("FiscalYear"), map[string]any{
		"name": "FY2026", "start_date": "2026-01-01", "end_date": "2026-12-31", "status": "open",
	}, actor)
	if err != nil {
		t.Fatalf("create FiscalYear: %v", err)
	}
	if _, err := engine.Create(ctx, def("Period"), map[string]any{
		"fiscal_year_id": fy.ID, "name": "test-period",
		"start_date": startDate, "end_date": endDate, "status": status,
	}, actor); err != nil {
		t.Fatalf("create Period: %v", err)
	}
	return tenantDB
}

func postTestEntry(date string) Entry {
	return Entry{
		Date:        date,
		Description: "Test entry",
		Lines: []Line{
			{AccountID: "1100", DebitMinor: 1000},
			{AccountID: "4000", CreditMinor: 1000},
		},
	}
}

func TestPost_ClosedPeriod_RejectsBeforeAnyWrite(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	_, err := Post(ctx, tenantDB, postTestEntry("2026-01-15"), humanActor())
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("expected ErrPeriodClosed, got %v", err)
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no journal_entries written, got %d", count)
	}
}

func TestPost_LockedPeriod_Rejects(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "locked", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	_, err := Post(ctx, tenantDB, postTestEntry("2026-01-15"), humanActor())
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("expected ErrPeriodClosed for a locked period, got %v", err)
	}
}

func TestPost_OpenPeriod_Succeeds(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "open", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	id, err := Post(ctx, tenantDB, postTestEntry("2026-01-15"), humanActor())
	if err != nil {
		t.Fatalf("expected posting into an open period to succeed, got %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty journal entry id")
	}
}

// TestPost_DateOutsideAnyPeriod_Succeeds confirms periods are opt-in
// bookkeeping — a date that falls entirely outside every defined Period
// (even though a closed one exists elsewhere) is not restricted.
func TestPost_DateOutsideAnyPeriod_Succeeds(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	id, err := Post(ctx, tenantDB, postTestEntry("2026-03-01"), humanActor())
	if err != nil {
		t.Fatalf("expected posting outside any defined period to succeed, got %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty journal entry id")
	}
}

// TestPost_ClosedPeriod_BoundaryDatesInclusive confirms the [start_date,
// end_date] range check is inclusive on both ends, not off-by-one.
func TestPost_ClosedPeriod_BoundaryDatesInclusive(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	for _, date := range []string{"2026-01-01", "2026-01-31"} {
		_, err := Post(ctx, tenantDB, postTestEntry(date), humanActor())
		if !errors.Is(err, ErrPeriodClosed) {
			t.Fatalf("expected ErrPeriodClosed for boundary date %s, got %v", date, err)
		}
	}

	// One day past the closed period's end must not be restricted.
	id, err := Post(ctx, tenantDB, postTestEntry("2026-02-01"), humanActor())
	if err != nil {
		t.Fatalf("expected the day after a closed period to succeed, got %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty journal entry id")
	}
}

// TestCheckPeriodOpen_NoDate_IsANoOp tests checkPeriodOpen directly
// (this file is `package ledger`, not `ledger_test`) — journal_entries.
// entry_date is itself a NOT NULL DB column, so an empty date was never
// a valid full Post/PostTx call regardless of period logic; this
// isolates the one thing actually new here: the period check itself
// treats an empty date as "nothing to check," not an error, matching
// its own doc comment.
func TestCheckPeriodOpen_NoDate_IsANoOp(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	tx, err := tenantDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := checkPeriodOpen(ctx, tx, ""); err != nil {
		t.Fatalf("expected an empty date to skip the period check, got %v", err)
	}
}

// TestPost_MalformedDate_FailsClosed confirms an unparseable date fails
// the post rather than silently being treated as "outside every
// period" — a real gap found by independent review: the original
// string-comparison implementation failed *open* for exactly these
// inputs (a single-digit month, or an RFC3339 suffix pushing the string
// lexicographically past a period's end_date).
func TestPost_MalformedDate_FailsClosed(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	for _, badDate := range []string{"2026-1-15", "2026-01-31T10:00:00Z", "not-a-date"} {
		_, err := Post(ctx, tenantDB, postTestEntry(badDate), humanActor())
		if !errors.Is(err, ErrInvalidDate) {
			t.Fatalf("date %q: expected ErrInvalidDate, got %v", badDate, err)
		}
	}

	var count int
	if err := tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil {
		t.Fatalf("count journal_entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no journal_entries written for any malformed date, got %d", count)
	}
}

// TestPost_OverlappingPeriods_AnyClosedBlocks confirms two overlapping
// Period records (nothing prevents this today) covering the same date,
// one open and one closed, still block the post — checkPeriodOpen's own
// doc comment describes checking every covering period, not stopping at
// the first match.
func TestPost_OverlappingPeriods_AnyClosedBlocks(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "open", "2026-01-01", "2026-01-31")
	ctx := context.Background()
	actor := humanActor()

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	v, err := entityDefs.GetPublished(ctx, "Period")
	if err != nil {
		t.Fatalf("GetPublished(Period): %v", err)
	}
	periodDef, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal Period: %v", err)
	}
	fyRepo := data.NewEntityDefinitionRepo(tenantDB)
	fyV, err := fyRepo.GetPublished(ctx, "FiscalYear")
	if err != nil {
		t.Fatalf("GetPublished(FiscalYear): %v", err)
	}
	fyDef, err := entity.Unmarshal(fyV.Definition)
	if err != nil {
		t.Fatalf("unmarshal FiscalYear: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	fys, err := engine.List(ctx, fyDef)
	if err != nil || len(fys) == 0 {
		t.Fatalf("expected a FiscalYear from setUpPeriodFixture: %v", err)
	}
	// A second, overlapping period covering the same date range as the
	// fixture's open one, but closed.
	if _, err := engine.Create(ctx, periodDef, map[string]any{
		"fiscal_year_id": fys[0].ID, "name": "overlap-closed",
		"start_date": "2026-01-10", "end_date": "2026-01-20", "status": "closed",
	}, actor); err != nil {
		t.Fatalf("create overlapping Period: %v", err)
	}

	_, err = Post(ctx, tenantDB, postTestEntry("2026-01-15"), actor)
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("expected the overlapping closed period to block posting, got %v", err)
	}
}

// TestPost_ClosedPeriod_ChecksBeforeAccountResolution confirms the
// period gate runs strictly before any account-code resolution — a
// closed period AND a bogus account code together must fail with
// ErrPeriodClosed, not an account-resolution error, proving the "no
// partial work" ordering PostTx's own code establishes (period check,
// then account resolution, then the write) actually holds.
func TestPost_ClosedPeriod_ChecksBeforeAccountResolution(t *testing.T) {
	tenantDB := setUpPeriodFixture(t, "closed", "2026-01-01", "2026-01-31")
	ctx := context.Background()

	entry := Entry{
		Date:        "2026-01-15",
		Description: "Bad account, closed period",
		Lines: []Line{
			{AccountID: "9999-does-not-exist", DebitMinor: 1000},
			{AccountID: "1100", CreditMinor: 1000},
		},
	}
	_, err := Post(ctx, tenantDB, entry, humanActor())
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("expected ErrPeriodClosed (checked before account resolution), got %v", err)
	}
}
