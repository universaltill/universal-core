// This file closes uc-infra#213: depreciation.Build (this package's own
// hand-written, property-tested schedule generator) had no caller
// anywhere in the shipped application — cmd/seed-demo-data was the only
// thing that ever invoked it. A FixedAsset created through the real app
// got an empty Depreciation Schedule section and had to have every
// period entered by hand for PostDueDepreciation/PostDueDepreciationBatch
// to have anything to post. GenerateDepreciationScheduleOnWrite is the
// crud.Hook that closes that gap, the same way purchasing.
// PostGoodsReceiptLineToLedger/finance.SyncGLAccountOnWrite already wire
// generic-engine writes into module-specific side effects without the
// generic engine itself knowing FixedAsset exists (CLAUDE.md's
// kernel-boundary rule).
package assets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// GenerateDepreciationScheduleOnWrite is registered for "FixedAsset" —
// see cmd/universal-core/main.go's RegisterHook wiring. Runs inside the
// FixedAsset write's own transaction (crud.Hook's own doc comment), so a
// rejection here rolls back the FixedAsset write itself rather than
// leaving a document with no usable schedule.
//
// On create: builds the full straight-line schedule (every period of
// useful_life_months, not a truncated preview — PostDueDepreciation's own
// doc comment already assumes Build "generates a schedule once, up
// front, for an asset's full useful life") and inserts it as ordinary
// DepreciationSchedule composition children, ready for
// PostDueDepreciationBatch to post as each period comes due.
//
// On update: reconciles rather than blindly regenerating. Recomputes
// what Build would produce for the record's current fields and compares
// it against the stored schedule (scheduleMatches) — an edit that never
// touched cost/salvage_value/useful_life_months/depreciation_method/
// acquisition_date/currency_id (editing FixedAsset.location, say)
// produces an identical desired schedule and this is a no-op, not a
// delete-and-reinsert on every save. When the desired schedule genuinely
// differs from what's stored:
//   - If none of the existing rows have posted_at set, the stored rows
//     are stale drafts with no financial consequence yet — delete and
//     replace them with the newly-computed schedule.
//   - If any row is already posted, regenerating would silently
//     invalidate journal entries this kernel already wrote
//     (internal/kernel/ledger, hand-reviewed, deterministic-core
//     territory) — the write is rejected instead
//     (crud.ErrHookRejected, surfaced as a 400) so a tenant that needs to
//     change a depreciating asset's terms mid-life has to do so through
//     an explicit accounting event (disposal/revaluation), not a plain
//     field edit. That's a deliberate, conservative default for a first
//     cut — a real partial-regeneration (rebuild only the still-unposted
//     tail, leave posted history untouched) is more permissive and
//     genuinely more correct, but is real design/implementation work of
//     its own; filed as follow-up (uc-infra#213's own "real design
//     question" note) rather than built speculatively here.
//
// Fixed limitation (uc-infra#236): this hook can only see the record's
// current (post-write) state, not what changed, so on its own it cannot
// tell "a relevant field genuinely changed" apart from "someone manually
// corrected a DepreciationSchedule row and the asset was saved again for
// an unrelated reason" — both look identical (stored schedule != freshly
// computed one) from here. Rather than extending crud.Hook's signature
// to hand every hook a pre-write record (a generic-engine change with a
// much wider blast radius, shared by five other modules' hooks — out of
// scope for this fix), scheduleMatches below skips an overridden row's
// own content entirely. MarkDepreciationScheduleOverriddenOnWrite (this
// file) is what keeps `overridden` trustworthy: it's not a flag a caller
// sets, it's RECOMPUTED on every DepreciationSchedule write by comparing
// the row against what buildDesiredSchedule would independently produce
// for its own FixedAsset — see that hook's own doc comment for why this
// (not a caller-trusted flag) is what closes uc-infra#236's independent
// review findings F2/F3.
//
// Guard (uc-infra#236 review, finding F1): scheduleMatches skipping every
// overridden row's content means a schedule where EVERY row happens to
// be overridden can never produce a content mismatch on its own — which
// would silently defeat the already-posted rejection below if the
// asset's terms genuinely changed at the same time. Handled below: if
// scheduleMatches reports a match SOLELY because every row was skipped
// as overridden, and at least one row is posted, this still rejects —
// there is no way from here to confirm the asset's terms didn't also
// change, so the conservative default (never silently risk posted
// history) applies exactly as it does for an ordinary detected mismatch.
func GenerateDepreciationScheduleOnWrite(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	records := data.NewRecordRepo(nil)

	desired, err := buildDesiredSchedule(ctx, tx, records, rec.ID, rec.Data)
	if err != nil {
		return err
	}

	// FOR UPDATE, not a plain read: this hook is about to decide, based on
	// whether any row here is already posted, whether it's safe to delete
	// and replace them — see ListByFieldForUpdateTx's own doc comment for
	// the concrete race a non-locking read would leave open against
	// internal/worker.Runner's depreciation-posting tick (PostDueDepreciation/
	// PostDueDepreciationBatch, this same package's ledger.go). Locking
	// here means a concurrent posting transaction either already committed
	// (so this read sees its posted_at) or is blocked behind this hook's
	// own transaction (so it waits, rather than racing this delete).
	existing, err := records.ListByFieldForUpdateTx(ctx, tx, "DepreciationSchedule", "fixed_asset_id", rec.ID)
	if err != nil {
		return fmt.Errorf("list existing DepreciationSchedule for FixedAsset %s: %w", rec.ID, err)
	}

	if action == audit.ActionCreate {
		if len(existing) > 0 {
			// Not reachable through any normal path — a brand-new record's
			// id cannot already have composition children. Left as a
			// defensive no-op (rather than an error) so a future caller
			// reusing this hook for a different write path fails safe
			// instead of duplicating a schedule.
			return nil
		}
		return insertSchedule(ctx, tx, records, desired, actor)
	}

	anyPosted := false
	for _, row := range existing {
		if postedAt, _ := row.Data["posted_at"].(string); postedAt != "" {
			anyPosted = true
			break
		}
	}

	if scheduleMatches(existing, desired) {
		if anyPosted && allOverridden(existing) {
			return fmt.Errorf("%w: FixedAsset %s's entire depreciation schedule has been manually corrected, and at least one period has already posted — this save is rejected because a correction on every row leaves no way to confirm the asset's own terms didn't also change. Restore at least one period to its computed value, or change the asset's terms through an explicit accounting event (disposal/revaluation) instead of a plain field edit", crud.ErrHookRejected, rec.ID)
		}
		return nil // this edit didn't change anything the schedule depends on
	}

	if anyPosted {
		// Deliberately doesn't assert WHICH field changed — this branch
		// is reached on any mismatch between the stored schedule and
		// what Build now computes, restricted (since uc-infra#236) to
		// non-overridden rows by scheduleMatches above, so by the time
		// this is reached a prior manual correction to a schedule row
		// has already been ruled out as the cause on any row that
		// wasn't itself skipped: what's left is a genuine edit to cost/
		// salvage_value/useful_life_months/depreciation_method/
		// acquisition_date/currency_id (or the all-overridden guard
		// above, which reaches the same conclusion by a different
		// route).
		return fmt.Errorf("%w: FixedAsset %s's stored depreciation schedule no longer matches what its current terms would compute, and at least one period has already posted — this save is rejected rather than silently regenerating posted history. Change the asset's terms through an explicit accounting event (disposal/revaluation) instead of a plain field edit", crud.ErrHookRejected, rec.ID)
	}

	for _, row := range existing {
		if err := records.DeleteTx(ctx, tx, "DepreciationSchedule", row.ID); err != nil {
			return fmt.Errorf("delete stale DepreciationSchedule %s for FixedAsset %s: %w", row.ID, rec.ID, err)
		}
		auditEntry, err := audit.New("DepreciationSchedule", row.ID, audit.ActionDelete, actor, nil)
		if err != nil {
			return fmt.Errorf("build audit entry for DepreciationSchedule %s delete: %w", row.ID, err)
		}
		if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
			return fmt.Errorf("write audit entry for DepreciationSchedule %s delete: %w", row.ID, err)
		}
	}
	return insertSchedule(ctx, tx, records, desired, actor)
}

// buildDesiredSchedule computes the full desired DepreciationSchedule row
// set for assetID's CURRENT fields — the same Method/AcquisitionDate/
// Cost/SalvageValue/UsefulLifeMonths extraction, currency-scale
// resolution, and Build/scheduleFields pipeline
// GenerateDepreciationScheduleOnWrite always ran inline before this was
// pulled out (uc-infra#236 review) so
// MarkDepreciationScheduleOverriddenOnWrite could reuse the identical
// computation — from the FixedAsset side there is exactly one
// "what should this schedule look like right now" answer, and both
// hooks needing their own, potentially-drifting copy of it would be
// exactly the kind of duplication CLAUDE.md's kernel-boundary discipline
// warns against, even though this is entity-specific (assets-module)
// code, not generic-engine code.
func buildDesiredSchedule(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, assetID string, assetData map[string]any) ([]map[string]any, error) {
	method, _ := assetData["depreciation_method"].(string)
	acquisitionDate, _ := assetData["acquisition_date"].(string)
	costMajor, _ := assetData["cost"].(float64)
	salvageMajor, _ := assetData["salvage_value"].(float64)
	lifeMonthsRaw, _ := assetData["useful_life_months"].(float64)

	// Same fallback-to-2dp posture as postAssetDepreciation (this
	// package's own posting path) — an unset or unresolvable currency_id
	// is a known, documented gap shared with internal/kernel/ledger.
	// ToMinorUnits, not a reason to fail the whole write.
	minorUnit := defaultCurrencyMinorUnit
	if currencyID, _ := assetData["currency_id"].(string); currencyID != "" {
		if v, err := currencyMinorUnitTx(ctx, tx, records, currencyID); err == nil {
			minorUnit = v
		}
	}

	costMinor, err := MinorUnits(costMajor, minorUnit)
	if err != nil {
		return nil, fmt.Errorf("%w: FixedAsset %s cost: %v", crud.ErrHookRejected, assetID, err)
	}
	salvageMinor, err := MinorUnits(salvageMajor, minorUnit)
	if err != nil {
		return nil, fmt.Errorf("%w: FixedAsset %s salvage_value: %v", crud.ErrHookRejected, assetID, err)
	}

	periods, err := Build(Input{
		Method:           Method(method),
		AcquisitionDate:  acquisitionDate,
		CostMinor:        costMinor,
		SalvageMinor:     salvageMinor,
		UsefulLifeMonths: int(math.Round(lifeMonthsRaw)),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: build depreciation schedule for FixedAsset %s: %v", crud.ErrHookRejected, assetID, err)
	}
	return scheduleFields(assetID, periods, minorUnit), nil
}

// currencyMinorUnitTx is currencyMinorUnit (ledger.go) against a
// caller-supplied transaction instead of records' own r.db — needed
// here, unlike every existing caller of currencyMinorUnit, because this
// hook is handed a records repo constructed with a nil db
// (data.NewRecordRepo(nil), the same "every read/write goes through the
// *Tx variant, r.db is never dereferenced" convention finance.
// SyncGLAccountOnWrite's own currency lookup already uses) — calling
// currencyMinorUnit's plain Get here would panic on a nil r.db, exactly
// as it did before this function existed. Real (non-fallback) resolution
// is exercised by this file's own
// TestGenerateDepreciationScheduleOnWrite_CreateWithNonDefaultCurrencyScale
// — TestGenerateDepreciationScheduleOnWrite_MissingCurrency_FallsBackToDefaultScale
// covers the sibling no-currency_id case, which never calls this
// function at all (an earlier version of this comment claimed otherwise
// — independent review caught that the claim didn't match what the test
// actually exercises).
func currencyMinorUnitTx(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, currencyID string) (int, error) {
	currency, err := records.GetTx(ctx, tx, "Currency", currencyID)
	if err != nil {
		return 0, fmt.Errorf("resolve Currency %s: %w", currencyID, err)
	}
	v, ok := currency.Data["minor_unit"].(float64)
	if !ok {
		return 0, fmt.Errorf("Currency %s has no minor_unit", currencyID)
	}
	return int(v), nil
}

// scheduleFields converts Build's output into the field maps
// DepreciationSchedule records are stored as — the same shape
// cmd/seed-demo-data's own per-period Create call already uses
// (fixed_asset_id/sequence/period_end/depreciation_amount/book_value),
// so a hook-generated row and a demo-seeded row are indistinguishable in
// storage.
func scheduleFields(fixedAssetID string, periods []Period, minorUnit int) []map[string]any {
	div := math.Pow(10, float64(minorUnit))
	out := make([]map[string]any, len(periods))
	for i, p := range periods {
		out[i] = map[string]any{
			"fixed_asset_id":      fixedAssetID,
			"sequence":            float64(p.Sequence),
			"period_end":          p.PeriodEnd,
			"depreciation_amount": float64(p.DepreciationMinor) / div,
			"book_value":          float64(p.BookValueMinor) / div,
		}
	}
	return out
}

// scheduleMatches reports whether existing (as read from storage, order
// not guaranteed) already holds exactly what desired describes — same
// count, and every desired row's period_end/depreciation_amount/
// book_value matching the stored row at that sequence exactly, EXCEPT a
// row marked overridden (uc-infra#236): its own content is deliberately
// never compared, since a manual correction is expected to differ from
// a freshly-Build-computed value — that's the whole point of the
// correction. Its presence at the right sequence still counts (the
// length check below still requires the same row count desired would
// produce), just not its content. Every non-overridden row still
// compares exactly — two independently-run Builds of the same inputs
// are bit-for-bit identical (depreciation.go's whole point), so any
// mismatch there still means the asset's own terms genuinely changed.
// See GenerateDepreciationScheduleOnWrite's own doc comment for the
// all-overridden guard this function's caller applies on top of a true
// result (uc-infra#236 review finding F1) — deliberately not built into
// this function itself, so scheduleMatches keeps its single, simple
// question ("does the schedule match") and the posted-safety policy
// decision stays with the caller that already owns every other posted-
// row decision.
func scheduleMatches(existing []data.Record, desired []map[string]any) bool {
	if len(existing) != len(desired) {
		return false
	}
	bySeq := make(map[int]data.Record, len(existing))
	for _, row := range existing {
		seq, _ := row.Data["sequence"].(float64)
		bySeq[int(seq)] = row
	}
	for _, d := range desired {
		seq := int(d["sequence"].(float64))
		row, ok := bySeq[seq]
		if !ok {
			return false
		}
		if overridden, _ := row.Data["overridden"].(bool); overridden {
			continue
		}
		if periodEnd, _ := row.Data["period_end"].(string); periodEnd != d["period_end"].(string) {
			return false
		}
		if amount, _ := row.Data["depreciation_amount"].(float64); amount != d["depreciation_amount"].(float64) {
			return false
		}
		if bookValue, _ := row.Data["book_value"].(float64); bookValue != d["book_value"].(float64) {
			return false
		}
	}
	return true
}

// allOverridden reports whether every row in existing is marked
// overridden — GenerateDepreciationScheduleOnWrite's own doc comment
// explains why its caller needs this alongside scheduleMatches. A nil
// or empty existing is deliberately NOT "all overridden" (there is
// nothing to have overridden anything), matching the same
// "nothing to protect" posture the rest of this file already takes for
// an empty schedule.
func allOverridden(existing []data.Record) bool {
	if len(existing) == 0 {
		return false
	}
	for _, row := range existing {
		if overridden, _ := row.Data["overridden"].(bool); !overridden {
			return false
		}
	}
	return true
}

// MarkDepreciationScheduleOverriddenOnWrite is the crud.Hook registered
// for "DepreciationSchedule" itself (see cmd/universal-core/main.go's
// and cmd/seed-demo-data/main.go's RegisterHook/SetHook wiring,
// alongside FixedAsset's own GenerateDepreciationScheduleOnWrite above).
// It runs on Create and Update alike — the caller-agnostic RECOMPUTE
// design below makes both safe (see uc-infra#236 review finding F5:
// DepreciationSchedule genuinely can be created directly, not only via
// GenerateDepreciationScheduleOnWrite's own insertSchedule, and this
// hook needs to give that row a correct overridden value too, not
// silently assume it can't happen).
//
// GenerateDepreciationScheduleOnWrite's own writes (insertSchedule,
// DeleteTx above) go straight through the low-level records repo,
// bypassing crud.Engine — and therefore this hook — entirely, so a
// freshly hook-generated row is never seen here at all; only a write
// that actually went through crud.Engine.Update/Create on
// "DepreciationSchedule" reaches this hook (DepreciationScheduleForm's
// Save action, the generic POST /api/records/DepreciationSchedule[/id]
// API, a CSV import, internal/kernel/recordmigrate, authz.GuardedEngine
// — anything that calls the generic engine on this entity type).
//
// `overridden` is deliberately NOT a flag this hook trusts from the
// caller (uc-infra#236 review finding F3: a caller-submitted
// `overridden: true` on an unchanged row would otherwise let anyone
// with ordinary write access spoof the marker and, combined with
// finding F1's all-overridden guard, quietly defeat the posted-history
// safety net). Instead it's RECOMPUTED from scratch on every write:
// resolve this row's own FixedAsset, ask buildDesiredSchedule what this
// row's sequence SHOULD contain right now, and compare. A row whose
// period_end/depreciation_amount/book_value already agrees with that
// answer is not an override, regardless of what the caller sent or what
// was stored before — including a plain no-op Save that changes
// nothing (uc-infra#236 review finding F2: with the old "any Update
// marks it" design, simply opening DepreciationScheduleForm and
// clicking Save with no changes permanently, un-clearably marked a row
// overridden). A row that DOES differ becomes overridden; one that's
// edited back to match becomes un-overridden again, automatically — the
// flag is a live fact about the row's current content, not a one-way
// switch.
func MarkDepreciationScheduleOverriddenOnWrite(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	assetID, _ := rec.Data["fixed_asset_id"].(string)
	if assetID == "" {
		return nil // malformed/dangling row — not this hook's concern (ADR-0007's tolerated-dangling-reference posture)
	}

	records := data.NewRecordRepo(nil)
	asset, err := records.GetTx(ctx, tx, "FixedAsset", assetID)
	if err != nil {
		if errors.Is(err, data.ErrNotFound) {
			return nil // dangling reference — not this hook's concern
		}
		return fmt.Errorf("resolve FixedAsset %s for DepreciationSchedule %s: %w", assetID, rec.ID, err)
	}

	desired, err := buildDesiredSchedule(ctx, tx, records, assetID, asset.Data)
	if err != nil {
		return fmt.Errorf("compute desired schedule for DepreciationSchedule %s's FixedAsset %s: %w", rec.ID, assetID, err)
	}

	seq, _ := rec.Data["sequence"].(float64)
	var want map[string]any
	for _, d := range desired {
		if int(d["sequence"].(float64)) == int(seq) {
			want = d
			break
		}
	}
	// want == nil means this row's own sequence isn't part of what the
	// asset's current terms would produce at all (e.g. the asset's
	// useful_life_months has since shrunk past this row's sequence) —
	// that's as much a divergence from "what the generator would write"
	// as a content mismatch, so it counts as overridden too rather than
	// silently defaulting to false.
	matchesGenerated := want != nil &&
		rec.Data["period_end"] == want["period_end"] &&
		rec.Data["depreciation_amount"] == want["depreciation_amount"] &&
		rec.Data["book_value"] == want["book_value"]
	newOverridden := !matchesGenerated

	if current, _ := rec.Data["overridden"].(bool); current == newOverridden {
		return nil // already correct — no redundant write on every later save
	}

	newData := make(map[string]any, len(rec.Data)+1)
	for k, v := range rec.Data {
		newData[k] = v
	}
	newData["overridden"] = newOverridden

	// expectedVersion = rec.Version, not nil: this row was already
	// write-locked by the caller's own UpdateTx/CreateTx earlier in this
	// same transaction (crud.Engine.Update/Create) — same reasoning as
	// purchasing.MatchVendorInvoiceOnUpdate's identical redirect-write
	// pattern (ledger.go). On Create, rec.Version is the freshly-inserted
	// row's own starting version (crud.Engine.Create's rec, same as
	// Update's), so this is correct for both actions this hook runs on.
	expectedVersion := rec.Version
	if _, err := records.UpdateTx(ctx, tx, "DepreciationSchedule", rec.ID, newData, &expectedVersion); err != nil {
		return fmt.Errorf("set DepreciationSchedule %s overridden=%v: %w", rec.ID, newOverridden, err)
	}
	auditEntry, err := audit.New("DepreciationSchedule", rec.ID, action, actor, map[string]any{"overridden": newOverridden})
	if err != nil {
		return fmt.Errorf("build audit entry setting DepreciationSchedule %s overridden=%v: %w", rec.ID, newOverridden, err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry setting DepreciationSchedule %s overridden=%v: %w", rec.ID, newOverridden, err)
	}
	return nil
}

// insertSchedule creates one DepreciationSchedule record per entry in
// desired, applying the tenant's actually-published Definition's own
// defaults/validation first — same reasoning and same pattern
// purchasing.creditInventoryOnReceipt's own doc comment gives for
// resolving the published Definition explicitly rather than trusting a
// compiled-in shape: this hook is handed a *sql.Tx by crud.Hook's own
// signature, so it creates via the low-level records.CreateTx, which
// (unlike crud.Engine.Create) never applies ApplyDefaults/ValidateRecord
// itself.
func insertSchedule(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, desired []map[string]any, actor audit.Actor) error {
	if len(desired) == 0 {
		return nil
	}
	publishedDef, err := data.NewEntityDefinitionRepo(nil).GetPublishedTx(ctx, tx, "DepreciationSchedule")
	if err != nil {
		return fmt.Errorf("look up published DepreciationSchedule definition: %w", err)
	}
	def, err := entity.Unmarshal(publishedDef.Definition)
	if err != nil {
		return fmt.Errorf("unmarshal published DepreciationSchedule definition: %w", err)
	}
	for _, fields := range desired {
		entity.ApplyDefaults(def, fields)
		if err := entity.ValidateRecord(def, fields); err != nil {
			return fmt.Errorf("build DepreciationSchedule row: %w", err)
		}
		rec, err := records.CreateTx(ctx, tx, "DepreciationSchedule", fields)
		if err != nil {
			return fmt.Errorf("create DepreciationSchedule row: %w", err)
		}
		auditEntry, err := audit.New("DepreciationSchedule", rec.ID, audit.ActionCreate, actor, fields)
		if err != nil {
			return fmt.Errorf("build audit entry for DepreciationSchedule %s: %w", rec.ID, err)
		}
		if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
			return fmt.Errorf("write audit entry for DepreciationSchedule %s: %w", rec.ID, err)
		}
	}
	return nil
}
