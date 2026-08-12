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
	posted, _, err = PostDueDepreciationBatch(ctx, db, actor, math.MaxInt)
	return posted, err
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
// only how often a run starts.
//
// The initial worklist read (records.ListDueUnposted below) is ALSO
// bounded by maxRows at the query level — uc-infra#182: the previous
// shape read and decoded every DepreciationSchedule row in the tenant
// on every call, capped or not, before applying this cap in Go. The
// worklist query is gated to rows whose FixedAsset is currently
// in_service with complete GL wiring (data.ListDueUnpostedGate) — a row
// belonging to a disposed/written_off/misconfigured asset never
// mutates, so without the gate it would occupy the SAME LIMITed window
// on every future call forever (independent review caught this in this
// function's first uc-infra#182 shape: enough disposed assets, which
// `depreciation.Build` deliberately leaves with their remaining periods
// permanently due-and-unposted per this file's own "catch-up, not
// skip"/disposal doc comments, sorting earlier than any postable row,
// silently stops posting for the WHOLE tenant).
//
// Once maxRows rows have been attempted, remaining assets (and
// remaining due rows within the asset a cap boundary fell inside) are
// left completely untouched this call — same "steady state, not a bug"
// treatment already given to every other skip case in this file. Which
// asset(s) a capped call's fully_depreciated eligibility applies to is
// NOT decided here or in postAssetDepreciation any more (an asset whose
// own due rows straddle more than one capped call must not be checked
// against a partial, in-memory view of its schedule) — see
// healStuckFullyDepreciatedAssets's own doc comment for the query-level
// check that replaced it.
//
// truncated (uc-infra#183) reports whether tickTenant should retry this
// tenant on the very next poll tick instead of waiting out the full
// DepreciationPostInterval throttle. It requires BOTH of:
//
//  1. A window hit its own limit — either the due-rows worklist read
//     (records.ListDueUnposted's own LIMIT, so there may be more due rows
//     beyond this window) or the healing sweep (records.
//     LifeCompleteGroupIDs, so there may be more stuck assets beyond that
//     window).
//  2. This call actually made progress — posted at least one row, or
//     healed (actually transitioned) at least one stuck asset.
//
// Both conditions matter, not just the first (independent review's
// finding on this function's first version): a window can be
// PERMANENTLY full of items this call can never act on — a
// DepreciationSchedule row whose asset's account references point at a
// since-deleted Account (accountCode below errors every call, and
// postAssetDepreciation returns before spending any of its attempted
// budget, so the same unpostable rows sort into the same LIMITed window
// forever), or a life-complete FixedAsset whose tenant never seeded a
// fully_depreciated Status (statusIDByCodeTx below errors every call,
// same permanent-residency effect on LifeCompleteGroupIDs' own window).
// Reporting truncated=true for either case — as this function's first
// version did, checking only condition 1 — would make tickTenant clear
// the throttle unconditionally, turning DepreciationPostInterval into a
// permanent no-op for that tenant: the exact per-tenant-delays-every-
// other-tenant's-tick hazard ADR-0025 exists to bound (uc-infra#137),
// reintroduced at up to 15x the query rate for the one query
// uc-infra#202 already flags as the expensive one. Gating on progress
// too means a permanently-stuck window instead falls back to the plain
// pre-uc-infra#183 behavior: one attempt per DepreciationPostInterval,
// forever, same as an ordinary error — see
// TestPostDueDepreciationBatch_DoesNotReportTruncatedWhenHealingSweepIsPermanentlyStuck
// and its due-rows sibling in ledger_test.go for the direct regression
// coverage.
//
// Deliberately NOT "posted == maxRows" for condition 1: posted only
// counts rows this call actually wrote a journal entry for, but the
// budget is spent on rows ATTEMPTED (see the remaining/attempted
// accounting above) — a backlog dominated by rows a concurrent run
// already posted first can exhaust the whole cap while posted stays low
// or even zero, which would make a posted-based check under-report
// truncation exactly in the pathological case (uc-infra#137's own
// motivating scenario) where noticing it matters most. Checking the
// worklist read length directly has only the same narrow,
// explicitly-accepted imprecision the simpler heuristic would have had
// (a backlog that lands EXACTLY on maxRows AND makes real progress this
// call still reads as truncated after the call that actually finished
// it — one wasted extra attempt next tick, not a correctness problem).
func PostDueDepreciationBatch(ctx context.Context, db *sql.DB, actor audit.Actor, maxRows int) (posted int, truncated bool, err error) {
	if maxRows <= 0 {
		return 0, false, fmt.Errorf("assets: PostDueDepreciationBatch: maxRows must be positive, got %d", maxRows)
	}

	records := data.NewRecordRepo(db)
	today := time.Now().UTC().Format("2006-01-02")

	// Resolved once up front (not per-asset, not per-call inside the
	// worklist query) — both the worklist's own postability gate below
	// and the completion sweep further down need it.
	statusID, err := depreciationInServiceStatusID(ctx, records)
	if err != nil {
		return 0, false, fmt.Errorf("resolve in_service status: %w", err)
	}
	if statusID == "" {
		// Assets not published for this tenant — not an error, the
		// common case for most tenants (mirrors the pre-uc-infra#182
		// "no schedules yet" early-out this replaced).
		return 0, false, nil
	}

	due, err := records.ListDueUnposted(ctx, "DepreciationSchedule", "period_end", today, "posted_at", data.ListDueUnpostedGate{
		ParentType:       "FixedAsset",
		JoinField:        "fixed_asset_id",
		ParentMatchField: "status_id",
		ParentMatchValue: statusID,
		ParentRequiredNonEmpty: []string{
			"asset_account_id", "depreciation_expense_account_id", "accumulated_depreciation_account_id",
		},
	}, maxRows)
	if err != nil {
		return 0, false, fmt.Errorf("list due DepreciationSchedule: %w", err)
	}
	// The worklist read itself hit its LIMIT — there may be more due rows
	// than this window covers, regardless of how many of THESE rows the
	// posting loop below actually gets to (see this function's own doc
	// comment on why this is checked here, not via posted==maxRows). Only
	// half of the truncated signal (see this function's own doc comment)
	// — windowFull alone says nothing about whether this call could act
	// on any of it.
	windowFull := len(due) == maxRows

	if len(due) > 0 {
		// Grouped by asset so a bad asset (missing accounts, wrong
		// status) is diagnosed and skipped once, not once per row.
		rowsByAsset := map[string][]data.Record{}
		for _, s := range due {
			assetID, _ := s.Data["fixed_asset_id"].(string)
			if assetID == "" {
				log.Printf("assets: DepreciationSchedule %s has no fixed_asset_id, skipping", s.ID)
				continue
			}
			rowsByAsset[assetID] = append(rowsByAsset[assetID], s)
		}

		// remaining can never run out mid-asset any more: due is already
		// bounded to maxRows total by the query above, so the SUM of
		// every asset's own slice in rowsByAsset is <= maxRows, meaning
		// remaining is always >= the slice length of whichever asset is
		// visited next. postAssetDepreciation's own inner budget check
		// is kept as a belt-and-braces guard (see its own doc comment),
		// not because this loop still relies on it to bound total work.
		remaining := maxRows
		for assetID, rows := range rowsByAsset {
			if ctx.Err() != nil {
				// truncated is deliberately not computed here (its
				// progress-gating needs this whole call to have finished
				// running) — irrelevant anyway, since tickTenant only
				// reads truncated when err == nil.
				return posted, false, ctx.Err()
			}
			if remaining <= 0 {
				break
			}
			// posted += n unconditionally, even on error:
			// postAssetDepreciation returns real partial progress (it
			// posts each due row in its own transaction, so an error on
			// row 6 of 8 still leaves rows 1-5 truly posted) — discarding
			// n on the error path would undercount this run's actual
			// work (independent review's finding, predates uc-infra#182).
			//
			// remaining is spent by attempted, not n (posted): a row a
			// concurrent run already posted first costs a real BeginTx +
			// ExistsForSource round trip here (postDepreciationRow) but
			// contributes 0 to n — spending budget only on n would let a
			// tenant with a large already-posted-by-someone-else backlog
			// blow straight through the cap re-discovering that (uc-infra#137).
			n, attempted, err := postAssetDepreciation(ctx, db, records, assetID, rows, actor, remaining)
			posted += n
			remaining -= attempted
			if err != nil {
				log.Printf("assets: post depreciation for FixedAsset %s: %v", assetID, err)
			}
		}
	}

	// Runs every call, independent of whether Phase 1 above found (or
	// posted) anything this time — see healStuckFullyDepreciatedAssets's
	// own doc comment for why this can't be folded into the due-rows
	// loop above. A real error here (including ctx cancellation) is
	// propagated, not merely logged: PostDueDepreciationBatch's own
	// posting loop above already treats ctx.Err() as a real returned
	// error rather than a swallowed one, and this sweep should behave
	// the same way rather than reporting a clean (posted, nil) return
	// for a call that was actually cut short mid-sweep by a shutdown.
	healed, healWindowFull, err := healStuckFullyDepreciatedAssets(ctx, db, records, actor, statusID, today, maxRows)
	if err != nil {
		return posted, false, fmt.Errorf("heal stuck fully-depreciated assets: %w", err)
	}
	// Either phase hitting its own window is a candidate signal — the two
	// phases are independent worklists (due-unposted rows vs.
	// stuck-but-never-transitioned assets), and a caller retrying sooner
	// because of one doesn't need to know which. But a candidate alone
	// isn't enough (this function's own doc comment): only report
	// truncated when this call actually made progress somewhere — posted
	// a row, or actually transitioned a stuck asset — so a window
	// permanently full of items neither phase can ever act on falls back
	// to the ordinary once-per-DepreciationPostInterval cadence instead
	// of retrying every poll tick forever for no possible gain.
	windowFull = windowFull || healWindowFull
	progressed := posted > 0 || healed > 0
	truncated = windowFull && progressed

	return posted, truncated, nil
}

// healStuckFullyDepreciatedAssets is PostDueDepreciationBatch's
// completion/healing sweep — the query-level replacement for what used
// to be postAssetDepreciation's own per-call, life-aware completion
// check (uc-infra#182).
//
// Why this can't just be "check the asset(s) this call posted for":
// uc-infra#137/ADR-0025's own retry-safety finding (see
// TestPostDueDepreciation_AlreadyFullyPostedButNotTransitioned_HealsOnNextRun)
// is that an asset can end up with every DepreciationSchedule row
// posted but the fully_depreciated transition itself never happened (a
// crash, a version conflict, an overlapping poller) — and once every
// row is posted, that asset has ZERO due-or-unposted rows left, so
// records.ListDueUnposted's worklist (which only surfaces rows still
// needing posting) will never surface it again for ANY future call to
// notice. Something has to keep checking "is this in_service asset's
// schedule actually finished" independent of whether it has due work
// this call.
//
// records.LifeCompleteGroupIDs answers this as ONE SQL query instead of
// a Go-side loop, and its RESULT is empty in the ordinary, healthy
// steady state (nothing stuck). Its own per-call COST is NOT fully
// proportional to how many assets are actually stuck, though —
// independent review measured this directly (ADR-0026's own note): the
// correlated count(*) subquery still has to read every in_service
// candidate asset's own posted rows to know it ISN'T stuck, which sums
// to roughly the same order as the tenant's total schedule-row volume
// for in_service assets, the same asymptotic shape as the bug this
// whole change exists to fix, just executed in Postgres instead of Go.
// A real fix (a denormalized per-asset posted-count, or a bounded/
// rotating candidate batch instead of every in_service asset every
// call) is real follow-up work, filed as uc-infra#202 rather than
// designed into this same change — see ADR-0026's Consequences section.
// What this DOES still fix, unconditionally: ListDueUnposted's own
// worklist-read cost (the dominant, measured cost in uc-infra#182's own
// motivating example) is genuinely bounded now; this sweep's residual
// cost is a smaller, separate, and honestly-documented gap, not a
// regression from before this change.
// The returned windowFull (uc-infra#183) reports whether this sweep's
// own candidate read hit limit — same "the LIMIT was actually the
// limiting factor" signal PostDueDepreciationBatch's due-rows read uses,
// and the same narrow imprecision (a stuck count that lands exactly on
// limit still reads as windowFull after a sweep that actually caught
// every stuck asset). transitioned counts only the candidates this call
// ACTUALLY moved to fully_depreciated — not len(stuck), which also
// includes any candidate transitionToFullyDepreciated failed for (logged
// below, not returned as an error: one bad asset must not stop the rest
// from healing). PostDueDepreciationBatch needs this distinction to tell
// "the window is full of assets this call is genuinely making progress
// on" apart from "the window is full of assets permanently stuck for a
// reason this sweep can never resolve" — see its own doc comment.
func healStuckFullyDepreciatedAssets(ctx context.Context, db *sql.DB, records *data.RecordRepo, actor audit.Actor, statusID, today string, limit int) (transitioned int, windowFull bool, err error) {
	stuck, err := records.LifeCompleteGroupIDs(ctx, data.LifeCompleteGroupOptions{
		ParentType:       "FixedAsset",
		ParentMatchField: "status_id",
		ParentMatchValue: statusID,
		LifeField:        "useful_life_months",
		ChildType:        "DepreciationSchedule",
		ChildJoinField:   "fixed_asset_id",
		ChildPostedField: "posted_at",
		ChildDueField:    "period_end",
		Cutoff:           today,
	}, limit)
	if err != nil {
		return 0, false, fmt.Errorf("find stuck fully-depreciated FixedAssets: %w", err)
	}
	for _, assetID := range stuck {
		if err := transitionToFullyDepreciated(ctx, db, records, assetID, actor); err != nil {
			log.Printf("assets: heal stuck FixedAsset %s: %v", assetID, err)
			continue
		}
		transitioned++
	}
	return transitioned, len(stuck) == limit, nil
}

// depreciationInServiceStatusID resolves the fixed_asset_status
// StatusType's "in_service" Status id — the same StatusType/Status
// lookup assetStatusIs does per-asset, just needed once here since
// healStuckFullyDepreciatedAssets checks many candidate assets against
// the SAME status id in one query rather than resolving it per asset.
// Returns ("", nil), not an error, when the fixed_asset_status
// StatusType itself doesn't exist — Assets isn't published for this
// tenant, so there is nothing to heal, mirroring the "not licensed" case
// PostDueDepreciationBatch's own doc comment already treats as ordinary
// rather than a fault.
func depreciationInServiceStatusID(ctx context.Context, records *data.RecordRepo) (string, error) {
	statusTypes, err := records.ListByField(ctx, "StatusType", "code", "fixed_asset_status")
	if err != nil {
		return "", fmt.Errorf("resolve fixed_asset_status StatusType: %w", err)
	}
	if len(statusTypes) == 0 {
		return "", nil
	}
	if len(statusTypes) > 1 {
		return "", fmt.Errorf("expected at most 1 fixed_asset_status StatusType, found %d", len(statusTypes))
	}
	statuses, err := records.ListByField(ctx, "Status", "status_type_id", statusTypes[0].ID)
	if err != nil {
		return "", fmt.Errorf("list fixed_asset_status Status rows: %w", err)
	}
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == statusCodeInService {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("status %q not seeded for fixed_asset_status (assets.PublishStatuses not run for this tenant?)", statusCodeInService)
}

// postAssetDepreciation attempts up to maxRows of the due-and-unposted
// DepreciationSchedule rows in due (already filtered at the query level
// by records.ListDueUnposted — see PostDueDepreciationBatch's own doc
// comment, uc-infra#182). maxRows is always >= 1 — the caller
// never invokes this once its own budget is exhausted. Returns (posted,
// attempted, err): posted is how many rows this call actually wrote a
// journal entry for; attempted is how many due rows this call spent its
// budget on, including ones that turned out to already be posted by a
// concurrent run (postDepreciationRow returning ok=false, nil) — see
// PostDueDepreciationBatch's own doc comment on why the budget is spent
// on attempts, not successes. A non-nil error means the rest of the
// asset's due rows this run were skipped (bad status read, missing
// account wiring, an unresolvable account, or a mid-loop posting
// failure) — PostDueDepreciationBatch logs it, keeps whatever was
// posted before the error, and moves on to the next asset. Due rows
// left over because maxRows was reached (not an error) are left
// unposted the same way and simply picked up on a later call.
//
// Does NOT decide whether the asset's fully_depreciated transition is
// now due — that used to live here (comparing this call's own,
// possibly-partial view of the asset's schedule against
// useful_life_months) but moved to healStuckFullyDepreciatedAssets's
// query-level check (uc-infra#182): once this function is only ever
// handed the DUE rows a bounded, tenant-wide fetch happened to return
// for this one call, it no longer has enough information in hand to
// tell "this asset's schedule is genuinely finished" apart from "this
// call's own bounded worklist just happened to stop here" — a
// distinction uc-infra#137/ADR-0025's own review already showed matters
// (a premature transition is silent, unrecoverable data corruption: the
// status graph has no fully_depreciated -> in_service edge).
func postAssetDepreciation(ctx context.Context, db *sql.DB, records *data.RecordRepo, assetID string, due []data.Record, actor audit.Actor, maxRows int) (posted, attempted int, err error) {
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

	if len(due) == 0 {
		return 0, 0, nil
	}

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
	// asset_account_id itself is validated present and resolvable above
	// but not otherwise used — see PostDueDepreciation's own doc
	// comment.
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
			// Belt-and-braces only: PostDueDepreciationBatch's own
			// records.ListDueUnposted fetch already bounds the total
			// candidate set across every asset to maxRows, so the sum of
			// every asset's own due slice this call can never exceed the
			// budget handed to this one asset — see that function's own
			// doc comment. Kept in case that invariant ever changes.
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
// fully_depreciated — the caller (healStuckFullyDepreciatedAssets) has
// already decided the asset's full useful life is posted; this
// function's own job is just the atomic, safe write. Re-reads the asset
// and its status fresh (not the caller's earlier snapshot) and is a
// no-op, not an error, if the asset is no longer in_service by the time
// this runs (e.g. disposed in between, or a concurrent call already
// transitioned it) — same "steady state, not a bug" reasoning
// PostDueDepreciation's own doc comment gives. Being called repeatedly
// for an already-fully_depreciated asset is expected and cheap (one
// no-op transaction), not guarded against separately — the caller's own
// completion sweep already only calls this when it currently believes
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
