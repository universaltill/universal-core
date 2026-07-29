package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/universaltill/universal-core/internal/data"
)

// ErrPeriodClosed is returned when Post/PostTx's entry date falls inside
// a finance.Period whose status is "closed" or "locked" — the minimal
// close-checklist enforcement erp/BACKLOG-TASKS.md's Phase 1 "Period
// close" task asks for, not the full month-end feature set.
var ErrPeriodClosed = errors.New("ledger: cannot post into a closed or locked period")

// ErrInvalidDate is returned when an Entry's Date isn't a well-formed
// ISO-8601 "YYYY-MM-DD" string. A financial control that can't parse the
// date it's supposed to gate on must fail closed (reject), not silently
// treat an unparseable value as "outside every period" — found by
// independent review: the original string-comparison implementation
// (date < start || date > end) failed *open* for a malformed date
// (single-digit month, an RFC3339 timestamp suffix, etc.), which is
// exactly backwards for a control meant to block postings.
var ErrInvalidDate = errors.New("ledger: entry date is not a valid ISO-8601 date (YYYY-MM-DD)")

const isoDateLayout = "2006-01-02"

// checkPeriodOpen looks up every finance.Period record and, if any
// period's [start_date, end_date] range covers date, rejects the post
// when that period's status is "closed" or "locked". No matching period
// for date is deliberately NOT an error — periods are opt-in bookkeeping
// a tenant grows into (cmd/seed-demo-data seeds only a couple of
// months' worth today), not a prerequisite every posting must have one
// to satisfy; only an *explicitly* closed/locked period actually blocks
// anything. Every covering period is checked, not just the first
// match — two overlapping periods (nothing today prevents a tenant from
// having them) with different statuses must still block if *any* of
// them is closed/locked, regardless of iteration order.
//
// date must be a well-formed ISO-8601 "YYYY-MM-DD" string or empty
// (checked via time.Parse, not string comparison — see ErrInvalidDate's
// own doc comment for why this isn't just a style preference). A
// Period record whose own start_date/end_date fails to parse is skipped
// (treated as not covering anything) rather than failing the whole
// check — that's a data-quality problem with *that one record*, not a
// reason to block every posting tenant-wide; only the Entry's own date
// gets the fail-closed treatment, since that's the value actually being
// posted and this function's one job is deciding whether it's safe.
//
// Runs inside tx (the same transaction Post/PostTx already writes the
// journal entry in) via RecordRepo.ListTx — a read, not a write of
// finance.Period, a generic Entity Definition. See this package's own
// ADR-0004 addendum (uc-infra/docs/adr/0004-ledger-owns-its-own-
// account-table.md) for why this is a deliberately scoped exception to
// that ADR's "never read the generic entity directly" pattern, not an
// oversight: unlike Account (whose code is embedded into every posted
// journal_line via a permanent FK, so a stale/wrong read would corrupt
// posted data forever), a Period's status is re-evaluated fresh on
// every single post and never becomes part of what's actually
// recorded — the blast radius of reading it directly is "this specific
// post is wrongly allowed or blocked," never permanent data corruption.
func checkPeriodOpen(ctx context.Context, tx *sql.Tx, date string) error {
	if date == "" {
		return nil
	}
	entryDate, err := time.Parse(isoDateLayout, date)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidDate, date)
	}

	periods, err := data.NewRecordRepo(nil).ListTx(ctx, tx, "Period")
	if err != nil {
		return fmt.Errorf("list periods: %w", err)
	}
	for _, p := range periods {
		startStr, _ := p.Data["start_date"].(string)
		endStr, _ := p.Data["end_date"].(string)
		if startStr == "" || endStr == "" {
			continue
		}
		start, err := time.Parse(isoDateLayout, startStr)
		if err != nil {
			continue
		}
		end, err := time.Parse(isoDateLayout, endStr)
		if err != nil {
			continue
		}
		if entryDate.Before(start) || entryDate.After(end) {
			continue
		}
		status, _ := p.Data["status"].(string)
		if status == "closed" || status == "locked" {
			return fmt.Errorf("%w: period %q covering %s is %s", ErrPeriodClosed, p.ID, date, status)
		}
	}
	return nil
}
