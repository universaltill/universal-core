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
// Known limitation (uc-infra#236, filed by this change's own independent
// review, not fixed here): this hook can only see the record's current
// (post-write) state, not what changed, so it cannot tell "a relevant
// field genuinely changed" apart from "someone manually corrected a
// DepreciationSchedule row and the asset was saved again for an
// unrelated reason" — both look identical (stored schedule != freshly
// computed one) from here. Concretely: a manual correction to an
// unposted row can be silently overwritten by the next FixedAsset save,
// and once posted, an unrelated FixedAsset edit can be rejected even
// though the asset's own terms never changed. See uc-infra#236 for why
// the real fix (either handing this hook the pre-write record, or a
// tracked marker distinguishing hook-written rows from corrected ones)
// is its own follow-up rather than built speculatively here.
func GenerateDepreciationScheduleOnWrite(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	records := data.NewRecordRepo(nil)

	method, _ := rec.Data["depreciation_method"].(string)
	acquisitionDate, _ := rec.Data["acquisition_date"].(string)
	costMajor, _ := rec.Data["cost"].(float64)
	salvageMajor, _ := rec.Data["salvage_value"].(float64)
	lifeMonthsRaw, _ := rec.Data["useful_life_months"].(float64)

	// Same fallback-to-2dp posture as postAssetDepreciation (this
	// package's own posting path) — an unset or unresolvable currency_id
	// is a known, documented gap shared with internal/kernel/ledger.
	// ToMinorUnits, not a reason to fail the whole FixedAsset write.
	minorUnit := defaultCurrencyMinorUnit
	if currencyID, _ := rec.Data["currency_id"].(string); currencyID != "" {
		if v, err := currencyMinorUnitTx(ctx, tx, records, currencyID); err == nil {
			minorUnit = v
		}
	}

	costMinor, err := MinorUnits(costMajor, minorUnit)
	if err != nil {
		return fmt.Errorf("%w: FixedAsset %s cost: %v", crud.ErrHookRejected, rec.ID, err)
	}
	salvageMinor, err := MinorUnits(salvageMajor, minorUnit)
	if err != nil {
		return fmt.Errorf("%w: FixedAsset %s salvage_value: %v", crud.ErrHookRejected, rec.ID, err)
	}

	periods, err := Build(Input{
		Method:           Method(method),
		AcquisitionDate:  acquisitionDate,
		CostMinor:        costMinor,
		SalvageMinor:     salvageMinor,
		UsefulLifeMonths: int(math.Round(lifeMonthsRaw)),
	})
	if err != nil {
		return fmt.Errorf("%w: build depreciation schedule for FixedAsset %s: %v", crud.ErrHookRejected, rec.ID, err)
	}
	desired := scheduleFields(rec.ID, periods, minorUnit)

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

	if scheduleMatches(existing, desired) {
		return nil // this edit didn't change anything the schedule depends on
	}

	for _, row := range existing {
		if postedAt, _ := row.Data["posted_at"].(string); postedAt != "" {
			// Deliberately doesn't assert WHICH field changed, or that a
			// depreciation-relevant field changed at all — this branch is
			// reached on ANY mismatch between the stored schedule and what
			// Build now computes, which can also mean a prior manual
			// correction to a DepreciationSchedule row (see uc-infra#236,
			// filed by this same review), not necessarily an edit to cost/
			// salvage_value/useful_life_months/depreciation_method/
			// acquisition_date/currency_id. An earlier draft's message
			// claimed the specific cause; independent review caught that
			// it can be wrong.
			return fmt.Errorf("%w: FixedAsset %s's stored depreciation schedule no longer matches what its current terms would compute, and at least one period has already posted — this save is rejected rather than silently regenerating posted history. If the asset's cost, salvage value, useful life, method, acquisition date or currency didn't actually change, a prior manual correction to a schedule row is the more likely cause (uc-infra#236)", crud.ErrHookRejected, rec.ID)
		}
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
// book_value matching the stored row at that sequence exactly. Both
// sides are produced by the identical MinorUnits/Build/scheduleFields
// code path, so this is a plain equality check, not a tolerance
// comparison — two independently-run Builds of the same inputs are
// bit-for-bit identical (depreciation.go's whole point).
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
