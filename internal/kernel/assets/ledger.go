package assets

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
// "Due" means period_end <= today and posted_at is still empty.
// Deliberately catch-up, not skip: a posting run that missed several
// periods (the worker was down, or a tenant licensed Assets after
// periods had already accrued) posts every one of them on the next run
// rather than silently understating expense — unlike
// workflow.Scheduler's "at least this often" semantics for a
// user-facing trigger, a financial posting that never happens is a real
// accounting error, not a merely-late notification.
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

	for assetID, rows := range rowsByAsset {
		n, err := postAssetDepreciation(ctx, db, records, assetID, rows, today, actor)
		if err != nil {
			log.Printf("assets: post depreciation for FixedAsset %s: %v", assetID, err)
			continue
		}
		posted += n
	}
	return posted, nil
}

// postAssetDepreciation posts every due, unposted row in rows (a
// snapshot of one FixedAsset's own DepreciationSchedule rows, taken
// before this run) and, if that exhausts the asset's schedule, advances
// its status to fully_depreciated. Returns how many rows it posted; a
// non-nil error means the WHOLE asset was skipped (bad status, missing
// account wiring, an unresolvable account) before any row was posted —
// PostDueDepreciation logs it and moves on to the next asset.
func postAssetDepreciation(ctx context.Context, db *sql.DB, records *data.RecordRepo, assetID string, rows []data.Record, today string, actor audit.Actor) (int, error) {
	asset, err := records.Get(ctx, "FixedAsset", assetID)
	if err != nil {
		return 0, fmt.Errorf("resolve FixedAsset %s: %w", assetID, err)
	}

	inService, err := assetStatusIs(ctx, records, asset, statusCodeInService)
	if err != nil {
		return 0, fmt.Errorf("resolve FixedAsset %s status: %w", assetID, err)
	}
	if !inService {
		// draft / disposed / written_off / fully_depreciated — an
		// ordinary steady state, not an error. See this function's own
		// doc comment on PostDueDepreciation.
		return 0, nil
	}

	expenseAccountID, _ := asset.Data["depreciation_expense_account_id"].(string)
	accumAccountID, _ := asset.Data["accumulated_depreciation_account_id"].(string)
	assetAccountID, _ := asset.Data["asset_account_id"].(string)
	if expenseAccountID == "" || accumAccountID == "" || assetAccountID == "" {
		return 0, fmt.Errorf("FixedAsset %s is in_service with due depreciation but missing one or more of asset_account_id/depreciation_expense_account_id/accumulated_depreciation_account_id", assetID)
	}
	expenseCode, err := accountCode(ctx, records, expenseAccountID)
	if err != nil {
		return 0, fmt.Errorf("resolve depreciation_expense_account_id: %w", err)
	}
	accumCode, err := accountCode(ctx, records, accumAccountID)
	if err != nil {
		return 0, fmt.Errorf("resolve accumulated_depreciation_account_id: %w", err)
	}
	// asset_account_id itself is validated present and resolvable above
	// but not otherwise used — see PostDueDepreciation's own doc comment.
	if _, err := accountCode(ctx, records, assetAccountID); err != nil {
		return 0, fmt.Errorf("resolve asset_account_id: %w", err)
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

	totalUnpostedBefore := 0
	for _, r := range rows {
		if postedAt, _ := r.Data["posted_at"].(string); postedAt == "" {
			totalUnpostedBefore++
		}
	}

	posted := 0
	for _, r := range rows {
		postedAt, _ := r.Data["posted_at"].(string)
		if postedAt != "" {
			continue
		}
		periodEnd, _ := r.Data["period_end"].(string)
		if periodEnd == "" || periodEnd > today {
			continue // not due yet
		}
		ok, err := postDepreciationRow(ctx, db, records, asset, r, expenseCode, accumCode, minorUnit, actor)
		if err != nil {
			return posted, fmt.Errorf("post DepreciationSchedule %s: %w", r.ID, err)
		}
		if ok {
			posted++
		}
	}

	if posted > 0 && posted == totalUnpostedBefore {
		// Every row this asset had left unposted just got posted — the
		// schedule is exhausted. Re-check status fresh rather than
		// trusting the inService read from the top of this function:
		// nothing else in this process changes it concurrently, but a
		// stale check here would be a silent latent bug the moment
		// something else does.
		if err := transitionToFullyDepreciated(ctx, db, records, assetID, actor); err != nil {
			return posted, fmt.Errorf("transition FixedAsset %s to fully_depreciated: %w", assetID, err)
		}
	}
	return posted, nil
}

// postDepreciationRow posts one schedule row's journal entry and marks
// it posted, both in one transaction — a posting failure must never
// leave a row marked posted_at with no corresponding journal entry, or
// vice versa. Returns (false, nil) if the row turned out to already be
// posted by the time this transaction opened (a concurrent run, or two
// worker pollers) — ExistsForSource is the authoritative idempotency
// check; the posted_at field read before calling this function is only
// a cheap pre-filter.
func postDepreciationRow(ctx context.Context, db *sql.DB, records *data.RecordRepo, asset, row data.Record, expenseCode, accumCode string, minorUnit int, actor audit.Actor) (bool, error) {
	amount, _ := row.Data["depreciation_amount"].(float64)
	amountMinor, err := MinorUnits(amount, minorUnit)
	if err != nil {
		return false, fmt.Errorf("convert depreciation_amount: %w", err)
	}
	if amountMinor == 0 {
		// depreciation.Build never emits a zero-amount period for a
		// positive depreciable base, but guard the way every other
		// posting hook in this kernel guards a zero-value source rather
		// than assume the invariant always holds by the time this runs.
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
// fully_depreciated once its schedule is exhausted. Re-reads the asset
// and its status fresh (not the caller's earlier snapshot) and is a
// no-op, not an error, if the asset is no longer in_service by the time
// this runs (e.g. disposed in between) — same "steady state, not a bug"
// reasoning PostDueDepreciation's own doc comment gives.
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
