package assets

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
)

// statusCodeInService / statusCodeFullyDepreciated are the
// fixed_asset_status codes PostDueDepreciation reads and writes — see
// PublishStatuses (seed.go) for the seeded Status/StatusTransition rows
// these name.
const (
	statusCodeInService        = "in_service"
	statusCodeFullyDepreciated = "fully_depreciated"
)

// defaultCurrencyMinorUnit mirrors foundation.Currency's own field
// default (Default: float64(2)) — used when a FixedAsset's currency_id
// is unset or its Currency record can't be resolved. Same documented
// simplification internal/kernel/ledger.ToMinorUnits already makes for
// every other posting module in this kernel; this one at least reads
// the real value when it's available (assets.MinorUnits takes an
// explicit scale, unlike ToMinorUnits' hardcoded 2dp).
const defaultCurrencyMinorUnit = 2

// depreciationSchedulerActorID is the audit actor id PostDueDepreciation
// runs as — no human clicked anything and no model drafted this
// posting, the kernel acted because a schedule came due (ADR-0008's
// `system` actor, same reasoning as internal/worker's own
// schedulerActor()).
const depreciationSchedulerActorID = "assets-depreciation-scheduler"

// SchedulerActor is the audit.Actor internal/worker passes to
// PostDueDepreciation — exported so the wiring layer doesn't have to
// duplicate ADR-0008's reasoning to construct one.
func SchedulerActor() audit.Actor {
	return audit.Actor{Type: audit.ActorSystem, ID: depreciationSchedulerActorID}
}

// PostDueDepreciation posts every due, unposted DepreciationSchedule row
// to the ledger (Dr depreciation expense / Cr accumulated depreciation)
// and returns how many rows it posted. See ADR-0022
// (uc-infra/docs/adr/0022-...) for why this is a plain function called
// from internal/worker.Runner.tickTenant rather than a workflow
// StepKind, and uc-infra#76 for the originating requirement.
//
// This is the unbounded convenience form — every due row, one call,
// no resume needed — for tests and one-off/administrative use.
// internal/worker.Runner itself calls PostDueDepreciationBatch instead,
// capped by Config.DepreciationPostBatchSize: see that function's own
// doc comment and ADR-0025 (uc-infra/docs/adr/0025-...) for why an
// unbounded per-tenant run is a hazard in the worker's own sequential
// per-tenant tick loop specifically, not here.
//
// "Due" means period_end <= today and posted_at is still empty.
// Deliberately catch-up, not skip: a posting run that missed several
// periods (the worker was down, or a tenant licensed Assets after
// periods had already accrued) posts every one of them (across as many
// calls as it takes — see PostDueDepreciationBatch) rather than silently
// understating expense — unlike workflow.Scheduler's "at least this
// often" semantics for a user-facing trigger, a financial posting that
// never happens is a real accounting error, not a merely-late
// notification.
//
// Only posts for a FixedAsset currently "in_service": draft (not yet
// depreciating, per PublishStatuses' own doc comment), disposed and
// written_off (already exited service — continuing to post depreciation
// past disposal would overstate accumulated depreciation for an asset no
// longer owned) and fully_depreciated (nothing left to post) are all
// silently skipped, not logged as errors — each is an ordinary steady
// state, not a bug. This is a deliberate refinement over the raw ticket
// text: depreciation.Build generates a schedule once, up front, for an
// asset's full useful life, so without this check a disposed asset's
// future periods would still post.
//
// A FixedAsset missing any of its three account references, or whose
// referenced Account can't be resolved to a live gl_accounts code, is
// logged and skipped — that asset's remaining due rows this run, not the
// whole tenant's run: one misconfigured asset must not block every other
// asset's depreciation from posting on schedule. asset_account_id is
// required here even though depreciation itself never posts to it (that
// account holds the asset's original cost, posted at acquisition —
// separate, future work): the ticket that scoped this card is explicit
// that an asset's GL wiring becomes an error, not an optional field, the
// moment posting exists, and a partially-wired asset (expense/
// accumulated set but not asset_account_id) is far more likely a data
// entry mistake than an intentional state.
func PostDueDepreciation(ctx context.Context, db *sql.DB, actor audit.Actor) (posted int, err error) {
	return PostDueDepreciationBatch(ctx, db, actor, math.MaxInt)
}

// PostDueDepreciationBatch is PostDueDepreciation, bounded to attempting
// at most maxRows schedule rows in this one call (spent on rows
// attempted, not rows successfully posted — a row a concurrent run
// already got to first still costs a real transaction here, and
// budgeting only on successes would let that case blow through the cap;
// see postAssetDepreciation's own doc comment). maxRows must be positive
// (PostDueDepreciation's own unbounded convenience form is the way to
// say "no cap" — this function treats maxRows <= 0 as a caller error,
// not a synonym for unlimited, so a config value that was accidentally
// left unset fails loudly instead of silently reintroducing the
// unbounded-run hazard this function exists to prevent).
//
// uc-infra#137 / ADR-0025 (uc-infra/docs/adr/0025-...): internal/worker.
// Runner.tick() drains tenants sequentially, so one tenant's
// PostDueDepreciation call running long (a large catch-up backlog —
// exactly the scenario this function's own "catch-up, not skip" design
// intentionally supports) delays every OTHER tenant's tick, including
// the queued-job work a user is actually waiting on. Capping how much
// posting work one call can do bounds the transactional/lock-holding
// portion of that delay; the tenant's own remaining backlog simply
// resumes on its next throttle-interval tick, the same "a period that
// came due mid-interval waits at most this long to post" latency
// ADR-0022 already accepts for the sync-vs-fire split — this just
// extends that tolerance to cover one run's own posting duration, not
// only how often a run starts. This does NOT bound the initial
// records.List scan below, which still reads every DepreciationSchedule
// row in the tenant on every call regardless of maxRows — tracked as
// uc-infra#182, not fixed here.
//
// Once maxRows rows have been attempted, remaining assets (and
// remaining due rows within the asset a cap boundary fell inside) are
// left completely untouched this call — same "steady state, not a bug"
// treatment already given to every other skip case in this file. A
// FixedAsset whose due rows straddle more than one capped call is safe:
// postAssetDepreciation tracks whether ITS OWN due-rows loop was cut
// short by the cap and, if so, skips its fully_depreciated eligibility
// check entirely for that call — see that function's own doc comment
// for why the check alone (counting posted_at rows against
// useful_life_months) isn't sufficient once a call can stop mid-asset.
func PostDueDepreciationBatch(ctx context.Context, db *sql.DB, actor audit.Actor, maxRows int) (posted int, err error) {
	if maxRows <= 0 {
		return 0, fmt.Errorf("assets: PostDueDepreciationBatch: maxRows must be positive, got %d", maxRows)
	}

	records := data.NewRecordRepo(db)
	schedules, err := records.List(ctx, "DepreciationSchedule")
	if err != nil {
		return 0, fmt.Errorf("list DepreciationSchedule: %w", err)
	}
	if len(schedules) == 0 {
		// Assets not licensed for this tenant, or no schedules generated
		// yet — not an error, the common case for most tenants.
		return 0, nil
	}

	today := time.Now().UTC().Format("2006-01-02")

	// Grouped by asset so a bad asset (missing accounts, wrong status)
	// is diagnosed and skipped once, not once per row, and so "any
	// unposted rows left for this asset" can be answered from the
	// in-memory snapshot already taken above rather than re-listing
	// every schedule row in the tenant per asset.
	rowsByAsset := map[string][]data.Record{}
	for _, s := range schedules {
		assetID, _ := s.Data["fixed_asset_id"].(string)
		if assetID == "" {
			log.Printf("assets: DepreciationSchedule %s has no fixed_asset_id, skipping", s.ID)
			continue
		}
		rowsByAsset[assetID] = append(rowsByAsset[assetID], s)
	}

	remaining := maxRows
	for assetID, rows := range rowsByAsset {
		if ctx.Err() != nil {
			return posted, ctx.Err()
		}
		if remaining <= 0 {
			// Budget spent — this asset, and every other not-yet-visited
			// asset in this run, is left untouched and resumes on the
			// tenant's next throttle-interval tick. Break rather than
			// continue: map iteration order is unspecified, but every
			// remaining asset is equally "not due yet this call" once the
			// budget is gone, so there is nothing left this call can do.
			break
		}
		// posted += n unconditionally, even on error: postAssetDepreciation
		// returns real partial progress (it posts each due row in its own
		// transaction, so an error on row 6 of 8 still leaves rows 1-5
		// truly posted) — discarding n on the error path would undercount
		// this run's actual work and could suppress the "posted N rows"
		// log line below even though N rows really were posted (independent
		// review's finding: this was previously discarded here).
		//
		// remaining is spent by attempted, not n (posted): a row a
		// concurrent run already posted first costs a real BeginTx +
		// ExistsForSource round trip here (postDepreciationRow) but
		// contributes 0 to n — spending budget only on n would let a
		// tenant with a large already-posted-by-someone-else backlog
		// blow straight through the cap re-discovering that, defeating
		// the whole point of this function (independent review's
		// finding, uc-infra#137).
		n, attempted, err := postAssetDepreciation(ctx, db, records, assetID, rows, today, actor, remaining)
		posted += n
		remaining -= attempted
		if err != nil {
			log.Printf("assets: post depreciation for FixedAsset %s: %v", assetID, err)
		}
	}
	return posted, nil
}

// postAssetDepreciation attempts up to maxRows of the due, unposted rows
// in rows (a snapshot of one FixedAsset's own DepreciationSchedule rows,
// taken at the top of this run; maxRows is always >= 1 — the caller
// never invokes this once its own budget is exhausted) and, if the
// asset's full useful life has now been posted, advances its status to
// fully_depreciated. Returns (posted, attempted, err): posted is how
// many rows this call actually wrote a journal entry for; attempted is
// how many due rows this call spent its budget on, including ones that
// turned out to already be posted by a concurrent run (postDepreciationRow
// returning ok=false, nil) — see PostDueDepreciationBatch's own doc
// comment on why the budget is spent on attempts, not successes. A
// non-nil error means the rest of the asset's due rows this run were
// skipped (bad status read, missing account wiring, an unresolvable
// account, or a mid-loop posting failure) — PostDueDepreciationBatch
// logs it, keeps whatever was posted before the error, and moves on to
// the next asset. Due rows left over because maxRows was reached (not an
// error) are left unposted the same way and simply picked up on a later
// call — see PostDueDepreciationBatch's own doc comment.
func postAssetDepreciation(ctx context.Context, db *sql.DB, records *data.RecordRepo, assetID string, rows []data.Record, today string, actor audit.Actor, maxRows int) (posted, attempted int, err error) {
	asset, err := records.Get(ctx, "FixedAsset", assetID)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve FixedAsset %s: %w", assetID, err)
	}

	inService, err := assetStatusIs(ctx, records, asset, statusCodeInService)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve FixedAsset %s status: %w", assetID, err)
	}
	if !inService {
		// draft / disposed / written_off / fully_depreciated — an
		// ordinary steady state, not an error. See this function's own
		// doc comment on PostDueDepreciation.
		return 0, 0, nil
	}

	// Filter to due rows BEFORE resolving/validating account wiring.
	// An in_service asset can legitimately have no due rows yet (its
	// first period hasn't come round) even with its GL accounts still
	// unset — that's an ordinary in-progress data-entry state per
	// assets.go's own doc comment, not a fault. Checking wiring first
	// (the original order) meant every such asset logged a false
	// "missing accounts" error, every throttle interval, forever, until
	// someone actually finished configuring it — independent review's
	// finding.
	var due []data.Record
	for _, r := range rows {
		if postedAt, _ := r.Data["posted_at"].(string); postedAt != "" {
			continue
		}
		periodEnd, _ := r.Data["period_end"].(string)
		if periodEnd == "" || periodEnd > today {
			continue // not due yet
		}
		due = append(due, r)
	}

	// truncated marks whether this asset's OWN due-row loop below stopped
	// because the budget ran out mid-asset, as opposed to running every
	// due row to completion. Gates the completion check further down:
	// see that check's own doc comment for why a truncated asset must
	// never be allowed to attempt the fully_depreciated transition, even
	// though postedCount alone can no longer distinguish "genuinely
	// finished" from "just happened to reach the life count with rows
	// still left unattempted" once a cap can stop this loop early.
	truncated := false
	if len(due) > 0 {
		expenseAccountID, _ := asset.Data["depreciation_expense_account_id"].(string)
		accumAccountID, _ := asset.Data["accumulated_depreciation_account_id"].(string)
		assetAccountID, _ := asset.Data["asset_account_id"].(string)
		if expenseAccountID == "" || accumAccountID == "" || assetAccountID == "" {
			return 0, 0, fmt.Errorf("FixedAsset %s is in_service with due depreciation but missing one or more of asset_account_id/depreciation_expense_account_id/accumulated_depreciation_account_id", assetID)
		}
		expenseCode, err := accountCode(ctx, records, expenseAccountID)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve depreciation_expense_account_id: %w", err)
		}
		accumCode, err := accountCode(ctx, records, accumAccountID)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve accumulated_depreciation_account_id: %w", err)
		}
		// asset_account_id itself is validated present and resolvable
		// above but not otherwise used — see PostDueDepreciation's own
		// doc comment.
		if _, err := accountCode(ctx, records, assetAccountID); err != nil {
			return 0, 0, fmt.Errorf("resolve asset_account_id: %w", err)
		}

		minorUnit := defaultCurrencyMinorUnit
		if currencyID, _ := asset.Data["currency_id"].(string); currencyID != "" {
			if v, err := currencyMinorUnit(ctx, records, currencyID); err == nil {
				minorUnit = v
			}
			// An unresolvable currency_id falls back to the default rather
			// than skipping the whole asset — same "known gap, documented,
			// not a hard failure" treatment ledger.ToMinorUnits' own doc
			// comment already gives every other module's fixed-2dp
			// assumption.
		}

		for _, r := range due {
			if ctx.Err() != nil {
				return posted, attempted, ctx.Err()
			}
			if attempted >= maxRows {
				// Budget reached — the rest of this asset's due rows stay
				// unposted (posted_at still empty) and resume on a later
				// call, same as an untouched asset below the outer loop's
				// own budget cutoff. Not an error: see
				// PostDueDepreciationBatch's own doc comment. Spent on
				// attempts, not successful posts — see this function's own
				// doc comment on why.
				truncated = true
				break
			}
			attempted++
			ok, err := postDepreciationRow(ctx, db, records, asset, r, expenseCode, accumCode, minorUnit, actor)
			if err != nil {
				return posted, attempted, fmt.Errorf("post DepreciationSchedule %s: %w", r.ID, err)
			}
			if ok {
				posted++
			}
		}
	}

	// Retry-safe, life-aware completion check. Runs every call that
	// wasn't truncated by the budget (see truncated's own doc comment
	// just above), whether or not this call itself posted anything —
	// NOT gated on "this run posted the exhausting row" (the previous
	// shape: `posted > 0 && posted == totalUnpostedBefore`), which
	// independent review showed is a fire-once-or-never condition: once
	// every entered row is already posted, `posted` is 0 on every later
	// call and the transition can never be retried — a transient error,
	// a version conflict from a concurrent edit, or an overlapping
	// second poller (worker.go's own throttle records a start time, not
	// an exclusive lock across the run) all leave the asset stuck
	// reading in_service forever. Recomputing "is this asset's schedule
	// actually finished" from scratch on every untruncated call,
	// independent of what (if anything) this specific call posted,
	// makes a previously-stuck asset heal itself on a later tick instead.
	//
	// Gating on !truncated (uc-infra#137/ADR-0025's own review): once a
	// budget cap can stop this asset's own due-row loop partway through,
	// postedCount alone can no longer tell "every due row is genuinely
	// posted" apart from "the life-month count happened to be reached
	// while rows this asset still has due were merely never attempted
	// this call" — e.g. an asset whose useful_life_months was edited
	// down after more schedule rows than that already existed. Skipping
	// the check on a truncated call costs nothing (it simply re-checks,
	// cheaply, on whichever later call finally finishes this asset's
	// due rows without being cut off) and closes what would otherwise be
	// a silent, unrecoverable early transition — the status graph has no
	// fully_depreciated -> in_service edge, so posting the remaining due
	// rows after a premature transition would be impossible, not just
	// late.
	//
	// "Finished" is also life-aware, not just "every entered row is
	// posted": rows is compared against the asset's own
	// useful_life_months, not merely its own length. cmd/seed-demo-data
	// deliberately seeds only the first 12 of a 60-period schedule ("60
	// rows would bury the rest of the demo data") — reading "no unposted
	// rows currently exist" as "life complete" would flip that asset to
	// fully_depreciated after 12 months with 48 unbooked periods and
	// ~$39,600 of undepreciated cost, a state with no way back. This
	// count also folds in `posted` — the rows this very call just
	// posted, which the stale `rows` snapshot's own posted_at fields
	// don't yet reflect — without a second database round trip, since
	// `rows` was already loaded fresh (this run's own top-level List) by
	// the caller.
	usefulLifeMonths, _ := asset.Data["useful_life_months"].(float64)
	if usefulLifeMonths > 0 && !truncated {
		postedCount := posted
		for _, r := range rows {
			if postedAt, _ := r.Data["posted_at"].(string); postedAt != "" {
				postedCount++
			}
		}
		if postedCount >= int(usefulLifeMonths) {
			if err := transitionToFullyDepreciated(ctx, db, records, assetID, actor); err != nil {
				return posted, attempted, fmt.Errorf("transition FixedAsset %s to fully_depreciated: %w", assetID, err)
			}
		}
	}
	return posted, attempted, nil
}

// postDepreciationRow posts one schedule row's journal entry and marks
// it posted, both in one transaction — a posting failure must never
// leave a row marked posted_at with no corresponding journal entry, or
// vice versa. Returns (false, nil) if the row turned out to already be
// posted by the time this transaction opened (a concurrent run, or two
// worker pollers).
//
// What actually PREVENTS a double post under READ COMMITTED (this
// package's transactions all use the default isolation level) is the
// records.UpdateTx call below with expectedVersion set: two concurrent
// transactions can both read ExistsForSource as false (it's a plain,
// non-locking read), but only one of them can win the row-version check
// — the loser gets ErrVersionConflict and rolls back its whole
// transaction, including the journal entry it just wrote. ExistsForSource
// is still worth keeping as an early, cheap exit (skip the ledger.PostTx
// call entirely for the common already-posted case), but it is not by
// itself what makes this safe — don't read its presence here as license
// to pass expectedVersion=nil the way purchasing/ledger.go's own
// same-shaped call does in a context where that really is safe.
func postDepreciationRow(ctx context.Context, db *sql.DB, records *data.RecordRepo, asset, row data.Record, expenseCode, accumCode string, minorUnit int, actor audit.Actor) (bool, error) {
	amount, _ := row.Data["depreciation_amount"].(float64)
	amountMinor, err := MinorUnits(amount, minorUnit)
	if err != nil {
		return false, fmt.Errorf("convert depreciation_amount: %w", err)
	}
	if amountMinor == 0 {
		// A genuinely zero depreciable base (cost == salvage_value) is a
		// legitimate schedule depreciation.Build can produce — every
		// period really is $0, and marking it posted with no journal
		// entry is correct, not a guard against an invariant violation.
		// But a malformed row (e.g. depreciation_amount missing entirely,
		// which the map type assertion above silently reads as 0.0) looks
		// IDENTICAL at this point, and closing it out with no journal
		// entry and no trace is exactly how a real period's expense goes
		// permanently unbooked without anyone noticing (independent
		// review's finding: the prior version of this branch was
		// completely silent here, unlike sales/purchasing's own zero-guard,
		// which is a pure no-op that changes no state and has nothing to
		// go silently wrong). Logging doesn't distinguish the two cases
		// either, but it at least makes a zero-amount posting visible
		// rather than indistinguishable from an ordinary one.
		log.Printf("assets: DepreciationSchedule %s has a zero depreciation_amount (period_end %s) — marking posted with no journal entry",
			row.ID, row.Data["period_end"])
		if err := markPosted(ctx, db, records, row, actor); err != nil {
			return false, err
		}
		return true, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	entries := data.NewJournalEntryRepo(tx)
	alreadyPosted, err := entries.ExistsForSource(ctx, "DepreciationSchedule", row.ID)
	if err != nil {
		return false, fmt.Errorf("check existing posting for DepreciationSchedule %s: %w", row.ID, err)
	}
	if alreadyPosted {
		return false, nil
	}

	periodEnd, _ := row.Data["period_end"].(string)
	if _, err := ledger.PostTx(ctx, tx, ledger.Entry{
		Date:        periodEnd,
		Description: fmt.Sprintf("Depreciation for FixedAsset %s, period ending %s", asset.ID, periodEnd),
		SourceType:  "DepreciationSchedule",
		SourceID:    row.ID,
		Lines: []ledger.Line{
			{AccountID: expenseCode, DebitMinor: amountMinor},
			{AccountID: accumCode, CreditMinor: amountMinor},
		},
	}, actor); err != nil {
		return false, fmt.Errorf("post to ledger: %w", err)
	}

	newData := mergedRecordData(row.Data, map[string]any{"posted_at": periodEnd})
	expectedVersion := row.Version
	if _, err := records.UpdateTx(ctx, tx, "DepreciationSchedule", row.ID, newData, &expectedVersion); err != nil {
		return false, fmt.Errorf("set posted_at on DepreciationSchedule %s: %w", row.ID, err)
	}
	auditEntry, err := audit.New("DepreciationSchedule", row.ID, audit.ActionUpdate, actor, map[string]any{"posted_at": periodEnd})
	if err != nil {
		return false, fmt.Errorf("build audit entry for DepreciationSchedule %s posted_at: %w", row.ID, err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return false, fmt.Errorf("write audit entry for DepreciationSchedule %s posted_at: %w", row.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit tx: %w", err)
	}
	return true, nil
}

// markPosted is postDepreciationRow's zero-amount path: no journal entry
// to post, but posted_at still needs to advance so this row is never
// reconsidered "due" again — its own transaction and audit entry, same
// discipline as every other write in this file.
func markPosted(ctx context.Context, db *sql.DB, records *data.RecordRepo, row data.Record, actor audit.Actor) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	periodEnd, _ := row.Data["period_end"].(string)
	newData := mergedRecordData(row.Data, map[string]any{"posted_at": periodEnd})
	expectedVersion := row.Version
	if _, err := records.UpdateTx(ctx, tx, "DepreciationSchedule", row.ID, newData, &expectedVersion); err != nil {
		return fmt.Errorf("set posted_at on DepreciationSchedule %s: %w", row.ID, err)
	}
	auditEntry, err := audit.New("DepreciationSchedule", row.ID, audit.ActionUpdate, actor, map[string]any{"posted_at": periodEnd})
	if err != nil {
		return fmt.Errorf("build audit entry for DepreciationSchedule %s posted_at: %w", row.ID, err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry for DepreciationSchedule %s posted_at: %w", row.ID, err)
	}
	return tx.Commit()
}

// transitionToFullyDepreciated advances a FixedAsset from in_service to
// fully_depreciated — the caller (postAssetDepreciation) has already
// decided the asset's full useful life is posted; this function's own
// job is just the atomic, safe write. Re-reads the asset and its status
// fresh (not the caller's earlier snapshot) and is a no-op, not an
// error, if the asset is no longer in_service by the time this runs
// (e.g. disposed in between, or a concurrent call already transitioned
// it) — same "steady state, not a bug" reasoning PostDueDepreciation's
// own doc comment gives. Being called repeatedly for an
// already-fully_depreciated asset is expected and cheap (one no-op
// transaction), not guarded against separately — the caller's own
// life-aware check already only calls this when it currently believes
// there's something to do, and this function is the authority on
// whether that's still true.
func transitionToFullyDepreciated(ctx context.Context, db *sql.DB, records *data.RecordRepo, assetID string, actor audit.Actor) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	asset, err := records.GetTx(ctx, tx, "FixedAsset", assetID)
	if err != nil {
		return fmt.Errorf("resolve FixedAsset %s: %w", assetID, err)
	}
	statusID, _ := asset.Data["status_id"].(string)
	if statusID == "" {
		return nil
	}
	status, err := records.GetTx(ctx, tx, "Status", statusID)
	if err != nil {
		return fmt.Errorf("resolve Status %s: %w", statusID, err)
	}
	if code, _ := status.Data["code"].(string); code != statusCodeInService {
		return nil
	}
	statusTypeID, _ := status.Data["status_type_id"].(string)

	newStatusID, err := statusIDByCodeTx(ctx, tx, records, statusTypeID, statusCodeFullyDepreciated)
	if err != nil {
		return err
	}
	newData := mergedRecordData(asset.Data, map[string]any{"status_id": newStatusID})
	expectedVersion := asset.Version
	if _, err := records.UpdateTx(ctx, tx, "FixedAsset", assetID, newData, &expectedVersion); err != nil {
		return fmt.Errorf("transition FixedAsset %s to %s: %w", assetID, statusCodeFullyDepreciated, err)
	}
	auditEntry, err := audit.New("FixedAsset", assetID, audit.ActionUpdate, actor, map[string]any{"status_id": newStatusID})
	if err != nil {
		return fmt.Errorf("build audit entry for FixedAsset %s status transition: %w", assetID, err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry for FixedAsset %s status transition: %w", assetID, err)
	}
	return tx.Commit()
}

// assetStatusIs reports whether asset's current status_id resolves to
// code — a plain (non-tx) read since it only gates whether
// postAssetDepreciation proceeds at all, not something posted alongside
// a write.
func assetStatusIs(ctx context.Context, records *data.RecordRepo, asset data.Record, code string) (bool, error) {
	statusID, _ := asset.Data["status_id"].(string)
	if statusID == "" {
		return false, nil
	}
	status, err := records.Get(ctx, "Status", statusID)
	if err != nil {
		return false, fmt.Errorf("resolve Status %s: %w", statusID, err)
	}
	c, _ := status.Data["code"].(string)
	return c == code, nil
}

// accountCode resolves a finance.Account record id to its code (the
// human-meaningful gl_accounts.code ledger.Entry.Line.AccountID wants —
// see internal/kernel/ledger.Line's own doc comment). Unlike
// sales/purchasing's hardcoded chart-of-accounts constants, FixedAsset
// carries its own account references, so this resolves the real record
// rather than assuming a fixed code.
func accountCode(ctx context.Context, records *data.RecordRepo, accountID string) (string, error) {
	account, err := records.Get(ctx, "Account", accountID)
	if err != nil {
		return "", fmt.Errorf("resolve Account %s: %w", accountID, err)
	}
	code, _ := account.Data["code"].(string)
	if code == "" {
		return "", fmt.Errorf("Account %s has no code", accountID)
	}
	return code, nil
}

// currencyMinorUnit resolves a Currency record id to its minor_unit
// scale (see foundation.Currency's own Default: float64(2)).
func currencyMinorUnit(ctx context.Context, records *data.RecordRepo, currencyID string) (int, error) {
	currency, err := records.Get(ctx, "Currency", currencyID)
	if err != nil {
		return 0, fmt.Errorf("resolve Currency %s: %w", currencyID, err)
	}
	v, ok := currency.Data["minor_unit"].(float64)
	if !ok {
		return 0, fmt.Errorf("Currency %s has no minor_unit", currencyID)
	}
	return int(v), nil
}

// mergedRecordData copies base and overlays overrides on top —
// RecordRepo.UpdateTx replaces a record's stored data wholesale, so a
// caller changing one field must resend every field or silently wipe
// the rest. Same helper, same reasoning, as
// internal/kernel/purchasing/ledger.go's own — duplicated rather than
// shared because both are small, unexported, and package-private by the
// same convention every module here already follows (compare
// internal/kernel/sales, internal/kernel/purchasing).
func mergedRecordData(base map[string]any, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// statusIDByCodeTx resolves one Status record's id within a specific
// StatusType by its code. Scoped by statusTypeID, not code alone — two
// StatusTypes can share a Status code (e.g. another module's own
// "cancelled"), and an unscoped lookup could silently resolve the wrong
// StatusType's row. Same helper, same reasoning, as
// internal/kernel/purchasing/ledger.go's own.
func statusIDByCodeTx(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, statusTypeID, code string) (string, error) {
	statuses, err := records.ListTx(ctx, tx, "Status")
	if err != nil {
		return "", fmt.Errorf("list Status: %w", err)
	}
	for _, s := range statuses {
		if typeID, _ := s.Data["status_type_id"].(string); typeID != statusTypeID {
			continue
		}
		if c, _ := s.Data["code"].(string); c == code {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("status %q not seeded for status type %s (assets.PublishStatuses not run for this tenant?)", code, statusTypeID)
}
