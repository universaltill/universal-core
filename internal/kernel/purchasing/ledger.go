package purchasing

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
	"github.com/universaltill/universal-core/internal/kernel/ledger"
	"github.com/universaltill/universal-core/internal/kernel/money"
)

// ErrVendorInvoiceMatchFailed wraps vendorInvoiceMatchDetail's two
// genuinely unresolvable-by-editing-this-invoice failures — a missing
// purchase_order_id (Required at the schema level, so this is a
// defensive check, not a reachable API path) and a real infrastructure
// error while resolving the PurchaseOrder or its received value (not
// "not found," an actual DB failure) — never for an ordinary business
// disagreement (wrong vendor, a dangling/missing PurchaseOrder, no
// POLines yet, nothing received yet, a value mismatch): those redirect
// the VendorInvoice to "match_exception" instead of erroring, see
// vendorInvoiceMatchDetail and MatchVendorInvoiceOnUpdate's own doc
// comments.
var ErrVendorInvoiceMatchFailed = errors.New("vendor invoice match failed")

// glAccountInventory/glAccountAPAccrual are the chart-of-accounts codes
// (finance.Account.code / gl_accounts.code) this module posts
// GoodsReceipt lines to — matching internal/kernel/finance's default
// seeded chart (cmd/seed-demo-data's seedFinance: "1300 Inventory",
// "2100 Accounts Payable"). A tenant that customizes its chart of
// accounts away from these exact codes needs matching ones for this
// posting to keep resolving — a real, documented limitation of this
// first slice (same category of known gap as finance.DefaultGLCurrency),
// not a hidden assumption.
const (
	glAccountInventory = "1300"
	glAccountAPAccrual = "2100"
)

// PostGoodsReceiptLineToLedger is a crud.Hook (internal/kernel/crud.Hook)
// — register it for the "GoodsReceiptLine" entity type
// (Engine.SetHook) to post Dr Inventory / Cr AP Accrual for one received
// line's value AND credit InventoryItem.qty_on_hand at the receiving
// facility, both the moment the line is created. It ALSO carries this
// entity type's quality validation (validateGoodsReceiptLineQuality,
// uc-infra#82) — crud.Engine.SetHook only supports one hook per entity
// type (see SetHook's own doc comment), so all three jobs share this
// function rather than one silently overwriting another's
// registration. They run under different action gates: quality
// validation on every action, ledger posting and the InventoryItem
// credit on Create only — see each section's own comment below for why.
//
// Posted per-line, not per-GoodsReceipt header, deliberately:
// GoodsReceipt and GoodsReceiptLine are saved as separate API calls (no
// single-transaction master-detail save exists in this kernel yet — see
// R11a's own "not yet built" note), so at GoodsReceipt-create time no
// lines exist yet to compute a total from. Each GoodsReceiptLine is
// itself a complete, immutable economic event (a quantity of one item
// received against one PO line) — posting it as soon as its value is
// known avoids needing to detect "the receipt is now fully entered,"
// which this kernel has no mechanism for anyway.
//
// The InventoryItem side (uc-infra#54, executing ADR-0015 §5's deferred
// decision) is a second, independent effect of the same event, gated
// only on qty > 0 below (a zero-or-negative qty_received credits
// nothing — #80 tracks the missing Min concept that would otherwise
// reject a negative one at the schema level) — NOT on the ledger
// posting: a free-of-charge sample has no value to post (amountMinor
// below is 0) but still physically arrived, so it still bumps stock.
// Only crud.Engine has exactly one Hook per entity type (SetHook
// overwrites), so both effects live in this one function rather than as
// separate hooks — there is nowhere else to put the second one.
//
// **Upserts the (item_id, facility_id) InventoryItem row (uc-infra#126)
// rather than always inserting a new one** — the original insert-only
// design (kept here for the reasoning, since the tradeoff it accepted is
// still worth understanding) chose an INSERT specifically because it
// needs no read-modify-write, so two GoodsReceiptLines crediting the
// same (item, facility) concurrently could never lose an update the way
// an upsert without its own retry loop would. That upside was real, but
// so was the cost independent review surfaced: a tenant receiving the
// same item at the same facility repeatedly accumulated one
// InventoryItem row per receipt forever (unbounded growth every report
// in internal/data/reporting.go scans), and the entity is also a
// first-class generic-CRUD screen — a user opening "the" InventoryItem
// row for an item edited one arbitrary sliver of a balance with no
// indication the rest existed.
//
// Now that InventoryItem declares Unique on (item_id, facility_id)
// (uc-infra#81), creditInventoryOnReceipt (below) closes both gaps with
// an upsert retry loop: find-and-UpdateTx with expectedVersion on a
// concurrent ErrVersionConflict, and — for the first-ever row at a given
// (item, facility) — a losing CreateTx (a concurrent receipt won the
// same Unique key first) retried as a find-and-update against the
// winner's row instead of failing outright. Idempotency-against-re-edits
// is unchanged: the action != Create guard below, since GoodsReceiptLine
// has no update path today.
//
// Runs inside tx, the same transaction the GoodsReceiptLine write
// itself is in (crud.Hook's own contract) — a posting failure rolls back
// the line write too, never leaving an un-posted receipt line behind.
func PostGoodsReceiptLineToLedger(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	// validateGoodsReceiptLineQuality runs on EVERY action, Create AND
	// Update, ahead of the ledger-posting action gate below — unlike the
	// posting itself, quality is not a one-time event tied to the
	// line's creation. GoodsReceiptLineForm (uc-infra#82) put
	// qty_accepted/qty_rejected on the ordinary generic edit form, and
	// that form's Save goes through the exact same
	// PUT /api/records/GoodsReceiptLine/{id} -> crud.Engine.Update path
	// every other entity's edit does (independent review of uc-infra#82:
	// the original draft validated on Create only, on the mistaken
	// belief — pinned by this file's OWN TestPostGoodsReceiptLineToLedger_
	// UpdateAction_IsNoOp test, which proves Update calls this hook —
	// that GoodsReceiptLine has no live update path). Skipping this on
	// Update would let an edit silently break the required-together and
	// netting invariants Create already enforces, corrupting
	// forecast.ComputeQuality's aggregate with no write-time guard at
	// all on that path.
	if err := validateGoodsReceiptLineQuality(rec); err != nil {
		return err
	}

	if action != audit.ActionCreate {
		// The LEDGER POSTING below is still Create-only: GoodsReceiptLine
		// has no status/quantity-correction workflow, so an Update never
		// re-posts or reverses a journal entry (if one lands later, it
		// needs its own reversing/adjusting entry, not a silent re-post
		// here). Quality validation above is independent of that and
		// must not be gated the same way — see this function's own
		// doc comment.
		return nil
	}

	// numberFieldValue, not a bare `.(float64)` assertion: this file's own
	// existing gap (numberFieldValue's doc comment already flags it for
	// the ledger-posting hooks) — an int-typed qty_received (a Go caller
	// writing a literal, e.g. cmd/seed-demo-data or a future importer)
	// would silently read as 0 through a bare assertion, silently
	// skipping the InventoryItem credit below on top of the pre-existing
	// silently-zero ledger posting.
	qty, _ := numberFieldValue(rec.Data["qty_received"])
	poLineID, _ := rec.Data["po_line_id"].(string)
	if poLineID == "" {
		return fmt.Errorf("GoodsReceiptLine %s has no po_line_id", rec.ID)
	}
	goodsReceiptID, _ := rec.Data["goods_receipt_id"].(string)
	if goodsReceiptID == "" {
		return fmt.Errorf("GoodsReceiptLine %s has no goods_receipt_id", rec.ID)
	}
	itemID, _ := rec.Data["item_id"].(string)
	if itemID == "" {
		return fmt.Errorf("GoodsReceiptLine %s has no item_id", rec.ID)
	}

	records := data.NewRecordRepo(nil)
	poLine, err := records.GetTx(ctx, tx, "POLine", poLineID)
	if err != nil {
		return fmt.Errorf("resolve POLine %s: %w", poLineID, err)
	}
	// money.FromAny, and its ERROR is NOT discarded (independent review
	// of uc-infra#136's first pass, which did `_, _ :=` here): a first
	// draft matched this file's existing "unreadable value silently reads
	// as zero" posture (numberFieldValue's own doc comment), but that
	// posture exists for a Go-*type* leniency (int vs int64 vs float64,
	// all genuinely valid), not for a value that fails FieldMoney's own
	// whole-number invariant. A legacy POLine written before the
	// unit_price FieldNumber->FieldMoney Version bump still holds a
	// fractional major-unit decimal (e.g. 12.5) until cmd/backfill-
	// poline-money converts it — money.FromAny correctly rejects that,
	// and discarding the error would silently fall through to
	// unitPriceMinor==0, which the amountMinor==0 branch below treats as
	// "nothing to post" — i.e. a real, physically-received line credits
	// inventory (numberFieldValue on qty, unaffected) but posts NO
	// journal entry at all: an understated AP liability with no error
	// and no log line (confirmed by independent review, reproduced
	// against real Postgres). The ledger is a deterministic core
	// (universal-core/CLAUDE.md) — failing this receipt closed and
	// telling the operator to run the backfill is the correct posture
	// here, not the generic-field-read leniency this file uses
	// elsewhere for read-only reporting hooks.
	unitPriceMinor, err := money.FromAny(poLine.Data["unit_price"])
	if err != nil {
		return fmt.Errorf("POLine %s has an invalid unit_price for ledger posting (%w) — if this PurchaseOrder predates uc-infra#136's FieldMoney migration, run cmd/backfill-poline-money against this tenant before receiving against it", poLineID, err)
	}

	goodsReceipt, err := records.GetTx(ctx, tx, "GoodsReceipt", goodsReceiptID)
	if err != nil {
		return fmt.Errorf("resolve GoodsReceipt %s: %w", goodsReceiptID, err)
	}
	receivedDate, _ := goodsReceipt.Data["received_date"].(string)
	facilityID, _ := goodsReceipt.Data["facility_id"].(string)
	if facilityID == "" {
		return fmt.Errorf("GoodsReceipt %s has no facility_id", goodsReceiptID)
	}

	if qty > 0 {
		if err := creditInventoryOnReceipt(ctx, tx, records, itemID, facilityID, qty, rec.ID, actor); err != nil {
			return err
		}
	}

	// NOT ledger.ToMinorUnits(qty * unitPrice) — that helper converts a
	// MAJOR-unit amount to minor units (×100); unitPriceMinor is already
	// in minor units (uc-infra#136), so running it through ToMinorUnits
	// too would double-convert and post 100x the real value. qty is a
	// plain FieldNumber float (a receiving clerk's typed, possibly
	// fractional, quantity), so the product itself is rounded to the
	// nearest whole minor unit — see ledger.ToMinorUnits's own identical
	// math.Round(...*100) shape, just without the extra ×100 here since
	// unitPriceMinor already carries that scale.
	amountMinor := int64(math.Round(qty * float64(unitPriceMinor)))
	if amountMinor == 0 {
		// A zero-value line (no price on the POLine yet, a free-of-
		// charge sample) has nothing to post — Entry.Validate would
		// reject a zero-amount line as ErrBadLine anyway, so skipping
		// here is a real, legitimate no-op, not a swallowed error. The
		// InventoryItem credit above already ran regardless — see this
		// function's own doc comment on why the two are not gated
		// together.
		return nil
	}

	_, err = ledger.PostTx(ctx, tx, ledger.Entry{
		Date:        receivedDate,
		Description: fmt.Sprintf("Goods receipt line %s", rec.ID),
		SourceType:  "GoodsReceiptLine",
		SourceID:    rec.ID,
		Lines: []ledger.Line{
			{AccountID: glAccountInventory, DebitMinor: amountMinor},
			{AccountID: glAccountAPAccrual, CreditMinor: amountMinor},
		},
	}, actor)
	if err != nil {
		return fmt.Errorf("post GoodsReceiptLine %s to ledger: %w", rec.ID, err)
	}
	return nil
}

// creditInventoryOnReceiptMaxRetries bounds creditInventoryOnReceipt's
// upsert retry loop — a defensive ceiling against a pathological
// scenario (a much larger concurrent burst than this kernel's own
// adversarial test exercises) livelocking forever, not a number expected
// to matter in practice: each retry only happens after losing a real
// race to another concurrently-committing transaction, so a long streak
// of consecutive losses is already an extremely unlikely tail, and this
// exists only so that tail fails loudly instead of hanging.
const creditInventoryOnReceiptMaxRetries = 20

// creditInventoryOnReceipt is PostGoodsReceiptLineToLedger's InventoryItem
// side, split out only for readability — see that function's own doc
// comment for the upsert-vs-insert tradeoff this implements. Called only
// when qty > 0 (that gate lives in the caller, not here — a
// zero-or-negative qty_received has nothing to credit), and independent
// of whether the ledger posting below it fires at all.
//
// qty_available_to_promise is credited by the same amount as
// qty_on_hand: nothing in this kernel today reserves stock against ATP
// separately (no SalesOrder-confirm path decrements it, no other writer
// of this field exists outside seed/backfill tooling), so on this
// kernel's current model the two move together. A real reservation
// model, if one lands later, changes this — not a reason to invent one
// here.
//
// Retry loop, not a single find-then-write: two GoodsReceiptLines
// crediting the same (item, facility) from concurrent transactions can
// both find "no existing row" (first-create race, closed by
// InventoryItem's Unique(item_id, facility_id) — uc-infra#81 — rejecting
// the loser's record_unique_keys insert) or both read the same existing
// row's version (find-then-update race, closed by expectedVersion
// rejecting the loser's UpdateTx with data.ErrVersionConflict). Either
// loss means "someone else just changed the fact this decision was based
// on" — re-reading and retrying is correct, not a bug being papered
// over, the same reasoning any optimistic-concurrency retry loop rests
// on. Bounded by creditInventoryOnReceiptMaxRetries so a pathological
// run fails loudly instead of hanging.
//
// One conflict this retry loop deliberately does NOT retry (independent
// review, uc-infra#126): a tenant carrying pre-#126 duplicate
// InventoryItem rows for the same (item, facility), created back when
// creditInventoryOnReceipt always inserted. If those duplicates predate
// the Unique constraint's record_unique_keys backfill, the row
// GetByFieldsQ picks (oldest) can lose UpdateUniqueConstraintKeys to a
// DIFFERENT still-live duplicate that already holds the key — retrying
// would just pick the same row and hit the same conflict again, since
// nothing about this transaction changes what GetByFieldsQ sees. That
// branch returns a clear, actionable error instead of looping.
//
// Still validates against the compiled-in InventoryItem() Definition,
// not the tenant's own published one (contrast every write in
// internal/kernel/crud.Engine, which always validates against
// data.EntityDefinitionRepo.GetPublished) — a real, disclosed gap, not
// an oversight, and NOT fixed by this function's upsert change:
// crud.Hook's signature hands this function a *sql.Tx and nothing else,
// and EntityDefinitionRepo has no Tx-taking read method to fetch the
// published Definition consistently within the same transaction (see
// data.EntityDefinitionRepo — every method takes only *sql.DB). Given
// #70 (existing tenants don't auto-adopt new Definitions), a tenant
// whose published InventoryItem Definition hasn't caught up, or one with
// a genuinely customized Definition, would have this credit validate
// against a shape that isn't what that tenant's own registry declares.
// Fixing this properly means either a Tx-capable GetPublished or
// threading the published Definition through crud.Hook's own signature
// — both real, separable changes, split out as uc-infra#165 rather than
// folded into this upsert fix.
func creditInventoryOnReceipt(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, itemID, facilityID string, qty float64, goodsReceiptLineID string, actor audit.Actor) error {
	def := InventoryItem()
	keys := data.NewRecordUniqueKeyRepo(nil)
	equals := map[string]string{"item_id": itemID, "facility_id": facilityID}

	for attempt := 0; ; attempt++ {
		existing, found, err := records.GetByFieldsQ(ctx, tx, "InventoryItem", equals)
		if err != nil {
			return fmt.Errorf("find existing InventoryItem for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
		}

		if found {
			existingOnHand, _ := numberFieldValue(existing.Data["qty_on_hand"])
			existingATP, _ := numberFieldValue(existing.Data["qty_available_to_promise"])
			fields := map[string]any{
				"item_id":                  itemID,
				"facility_id":              facilityID,
				"qty_on_hand":              existingOnHand + qty,
				"qty_available_to_promise": existingATP + qty,
			}
			if err := entity.ValidateRecord(def, fields); err != nil {
				return fmt.Errorf("build InventoryItem credit for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
			}
			expectedVersion := existing.Version
			if _, err := records.UpdateTx(ctx, tx, "InventoryItem", existing.ID, fields, &expectedVersion); err != nil {
				if errors.Is(err, data.ErrVersionConflict) {
					if attempt >= creditInventoryOnReceiptMaxRetries {
						return fmt.Errorf("credit InventoryItem for GoodsReceiptLine %s: exceeded %d retries on version conflict", goodsReceiptLineID, creditInventoryOnReceiptMaxRetries)
					}
					continue
				}
				return fmt.Errorf("credit InventoryItem for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
			}
			if err := crud.UpdateUniqueConstraintKeys(ctx, tx, keys, def, existing.ID, fields); err != nil {
				var uce *crud.UniqueConstraintError
				if errors.As(err, &uce) {
					// GetByFieldsQ picked `existing` as the oldest live
					// row for this (item, facility), but ANOTHER live
					// row already holds this pair's record_unique_keys
					// entry — only possible on a tenant carrying
					// pre-#126 duplicate InventoryItem rows that predate
					// the Unique(item_id, facility_id) constraint (v5)
					// and haven't been backfilled/merged yet (see
					// cmd/sync-tenant-modules' uniqueConstraintWarnings/
					// BackfillUniqueConstraintKeys for the operator-side
					// remediation). Not a transient race — retrying
					// would pick the same `existing` row and hit the
					// same conflict again — so this returns a clear,
					// actionable error instead of looping or leaking
					// UpdateUniqueConstraintKeys' own opaque wrapping.
					return fmt.Errorf("credit InventoryItem for GoodsReceiptLine %s: %s has duplicate live InventoryItem rows for (item_id, facility_id) — run cmd/sync-tenant-modules' backfill before this can upsert safely: %w", goodsReceiptLineID, existing.ID, err)
				}
				return fmt.Errorf("reconcile InventoryItem %s unique keys: %w", existing.ID, err)
			}
			auditEntry, err := audit.New("InventoryItem", existing.ID, audit.ActionUpdate, actor, fields)
			if err != nil {
				return fmt.Errorf("build audit entry for InventoryItem %s: %w", existing.ID, err)
			}
			if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
				return fmt.Errorf("write audit entry for InventoryItem %s: %w", existing.ID, err)
			}
			return nil
		}

		fields := map[string]any{
			"item_id":                  itemID,
			"facility_id":              facilityID,
			"qty_on_hand":              qty,
			"qty_available_to_promise": qty,
		}
		if err := entity.ValidateRecord(def, fields); err != nil {
			return fmt.Errorf("build InventoryItem credit for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
		}
		// SAVEPOINT around the create + unique-key write: a Postgres
		// UNIQUE index violation (the concurrent-create race
		// WriteUniqueConstraintKeys' own doc comment describes) aborts
		// the ENTIRE surrounding transaction at the SQL level, not just
		// the one failing statement — any further statement on this same
		// tx (including an attempt to soft-delete the losing row we just
		// inserted) would fail with "current transaction is aborted"
		// until the abort is cleared. ROLLBACK TO SAVEPOINT clears it
		// without discarding the rest of the transaction (the
		// GoodsReceiptLine write and its ledger posting), so this
		// function can retry in the SAME transaction instead of failing
		// the whole GoodsReceiptLine create over a benign, expected race.
		if _, err := tx.ExecContext(ctx, "SAVEPOINT credit_inventory_on_receipt"); err != nil {
			return fmt.Errorf("savepoint before InventoryItem create for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
		}
		invRec, err := records.CreateTx(ctx, tx, "InventoryItem", fields)
		if err != nil {
			return fmt.Errorf("credit InventoryItem for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
		}
		if err := crud.WriteUniqueConstraintKeys(ctx, tx, keys, def, invRec.ID, fields); err != nil {
			var uce *crud.UniqueConstraintError
			if errors.As(err, &uce) {
				// A concurrent transaction already committed the winning
				// row for this (item, facility) between our GetByFieldsQ
				// above and this insert. Roll back to the savepoint —
				// undoing our own now-unwanted row AND clearing the
				// transaction-level abort the conflict caused — then
				// retry: the next loop iteration's GetByFieldsQ will see
				// the winner (now committed) and upsert into it instead.
				if _, spErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT credit_inventory_on_receipt"); spErr != nil {
					return fmt.Errorf("roll back losing InventoryItem create for GoodsReceiptLine %s: %w", goodsReceiptLineID, spErr)
				}
				if attempt >= creditInventoryOnReceiptMaxRetries {
					return fmt.Errorf("credit InventoryItem for GoodsReceiptLine %s: exceeded %d retries on create race", goodsReceiptLineID, creditInventoryOnReceiptMaxRetries)
				}
				continue
			}
			return fmt.Errorf("write InventoryItem %s unique keys: %w", invRec.ID, err)
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT credit_inventory_on_receipt"); err != nil {
			return fmt.Errorf("release savepoint after InventoryItem create for GoodsReceiptLine %s: %w", goodsReceiptLineID, err)
		}
		auditEntry, err := audit.New("InventoryItem", invRec.ID, audit.ActionCreate, actor, fields)
		if err != nil {
			return fmt.Errorf("build audit entry for InventoryItem %s: %w", invRec.ID, err)
		}
		if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
			return fmt.Errorf("write audit entry for InventoryItem %s: %w", invRec.ID, err)
		}
		return nil
	}
}

// qtyNetEpsilonAbs/qtyNetEpsilonRel tolerate float64 rounding noise when
// comparing qty_accepted + qty_rejected against qty_received — the three
// values arrive as ordinary decimal input (a receiving clerk's typed
// quantity, or a CSV import), and while most real quantities net
// exactly, a fractional UoM (weight, length) can produce a sum that
// differs from the stored qty_received in its last binary digit despite
// being "equal" in every decimal sense a human typed. Not a business
// tolerance — an intentionally tiny guard against exactly this class of
// binary floating-point noise, not a license to be off by a real unit.
//
// Relative, not a single fixed absolute value (independent review of
// uc-infra#82): float64 has ~15-17 significant decimal digits, so the
// ABSOLUTE rounding noise on a large quantity is itself larger than a
// fixed 1e-9 — a fine-grained UoM receipt in the billions of units would
// spuriously fail netting on real, correct input. qtyNetTolerance scales
// the allowance with qty_received's own magnitude, falling back to the
// absolute floor for anything qty_received-sized-or-smaller (where 1e-9
// is already generous relative to the value).
const (
	qtyNetEpsilonAbs = 1e-9
	qtyNetEpsilonRel = 1e-9
)

// qtyNetTolerance returns the netting comparison's allowed slack for a
// given qty_received — see qtyNetEpsilonAbs/qtyNetEpsilonRel's own doc
// comment for why this scales rather than staying fixed.
func qtyNetTolerance(received float64) float64 {
	if abs := math.Abs(received); abs > 1 {
		return qtyNetEpsilonRel * abs
	}
	return qtyNetEpsilonAbs
}

// validateGoodsReceiptLineQuality is the uc-infra#82 half of
// PostGoodsReceiptLineToLedger's job: qty_accepted/qty_rejected
// (GoodsReceiptLine's own doc comment, Version 2) are declared OPTIONAL
// at the entity.Field level because entity.Field has no
// required-together concept (#80's same Min/Max gap), so this crud.Hook
// enforces the two invariants a plain field declaration can't:
//
//  1. Required together: a line records BOTH an accepted and a rejected
//     quantity or NEITHER — a line with only one set is a partial,
//     probably-still-being-entered quality record, not a real one, and
//     would silently corrupt forecast.ComputeQuality's sums (uncounted
//     "the other half" of a quantity) if let through.
//  2. Netting: when both are set, qty_accepted + qty_rejected must equal
//     qty_received (within qtyNetTolerance) — this is a SPLIT of what
//     arrived, not an independent count that could disagree with it.
//
// entity.ValidateRecord already guarantees that whichever of the two
// fields IS present in rec.Data is a well-formed FieldNumber before this
// hook ever runs (crud.Engine.Create/Update call it first) — this
// function only needs to decide presence (via the map's comma-ok, not
// numberFieldValue, which can't tell "absent" from "wrong type") and
// check the two business invariants above. Runs on EVERY action, Create
// AND Update — GoodsReceiptLine's generic edit form (forms.go) puts
// both fields on the ordinary PUT /api/records/{type}/{id} path, so an
// edit can violate either invariant exactly as easily as a create can
// (independent review of uc-infra#82 caught an earlier draft that
// validated on Create only).
func validateGoodsReceiptLineQuality(rec data.Record) error {
	acceptedRaw, hasAccepted := rec.Data["qty_accepted"]
	rejectedRaw, hasRejected := rec.Data["qty_rejected"]
	if !hasAccepted && !hasRejected {
		// No quality data recorded for this line at all — a valid,
		// expected state (GoodsReceiptLine's own doc comment): most
		// lines, until a tenant starts using this.
		return nil
	}
	if hasAccepted != hasRejected {
		return fmt.Errorf("%w: GoodsReceiptLine qty_accepted and qty_rejected must both be set or both be omitted", crud.ErrHookRejected)
	}

	accepted, ok := numberFieldValue(acceptedRaw)
	if !ok {
		return fmt.Errorf("%w: GoodsReceiptLine qty_accepted must be a number, got %T", crud.ErrHookRejected, acceptedRaw)
	}
	rejected, ok := numberFieldValue(rejectedRaw)
	if !ok {
		return fmt.Errorf("%w: GoodsReceiptLine qty_rejected must be a number, got %T", crud.ErrHookRejected, rejectedRaw)
	}
	if accepted < 0 {
		return fmt.Errorf("%w: GoodsReceiptLine qty_accepted must not be negative, got %v", crud.ErrHookRejected, accepted)
	}
	if rejected < 0 {
		return fmt.Errorf("%w: GoodsReceiptLine qty_rejected must not be negative, got %v", crud.ErrHookRejected, rejected)
	}

	received, _ := numberFieldValue(rec.Data["qty_received"])
	tolerance := qtyNetTolerance(received)
	if diff := accepted + rejected - received; diff > tolerance || diff < -tolerance {
		return fmt.Errorf("%w: GoodsReceiptLine qty_accepted (%v) + qty_rejected (%v) must equal qty_received (%v)",
			crud.ErrHookRejected, accepted, rejected, received)
	}
	return nil
}

// ValidateStockTransfer is a crud.Hook (internal/kernel/crud.Hook) —
// register it for the "StockTransfer" entity type (Engine.SetHook) to
// reject two business-rule violations entity.Field still has no way to
// express even with Min/Max (uc-infra#80): a transfer whose
// from_facility_id and to_facility_id are the same (a cross-field
// comparison — Min/Max is single-field only), and a transfer with
// qty <= 0 specifically (Min is inclusive, so qty's own declared Min:0
// already rejects every negative value; this hook's qty check narrows
// the remaining gap, qty == 0, that an inclusive bound structurally
// cannot). The two mechanisms layer rather than duplicate: qty's Min:0
// is what a form's min="0" attribute, csvimport's preview, and
// recordmigrate's dry run all see (none of those run crud.Hooks), and
// this hook is the strictly-greater-than-zero backstop for the one path
// that does.
//
// Validation-only, unlike PostGoodsReceiptLineToLedger: no ledger
// posting, no audit write of its own — a rejection returns
// crud.ErrHookRejected-wrapped errors, which crud.Engine's own hook
// wiring rolls the whole write back on (Hook's own doc comment), and
// internal/api's writeCrudError maps to a 400 with this hook's own
// message rather than the generic 500 an unwrapped error would produce
// (crud.ErrHookRejected's own doc comment explains why a generic kernel
// sentinel is needed here rather than an entity-specific one).
//
// Runs on BOTH Create and Update, deliberately — no action gate at all,
// unlike PostGoodsReceiptLineToLedger (whose posting must happen exactly
// once, so it genuinely only wants Create). These are invariants of what
// a StockTransfer *is*, not of how one comes into being: every
// status-managed entity in this product is editable through the generic
// PUT /api/records/{type}/{id} route and the rendered edit form the
// create form itself becomes after a save, so a create-only check is one
// ordinary edit away from a stored same-facility, negative-quantity
// transfer — measured, not theorised (independent review of #13). Both
// paths hand this hook the full replacement field set (Update is a full
// replacement — crud.Engine.Update), so the same two checks read the
// same way on either action.
//
// Every parameter but rec is deliberately unnamed: this hook reads
// nothing but the record's own incoming fields — no tx, so no database
// access at all, so no way for it to reach outside the tenant database
// crud.Engine already resolved (ADR-0003's per-tenant isolation).
func ValidateStockTransfer(_ context.Context, _ *sql.Tx, _ *entity.Definition, rec data.Record, _ audit.Action, _ audit.Actor) error {
	fromFacilityID, _ := rec.Data["from_facility_id"].(string)
	toFacilityID, _ := rec.Data["to_facility_id"].(string)
	if fromFacilityID != "" && fromFacilityID == toFacilityID {
		return fmt.Errorf("%w: StockTransfer from_facility_id and to_facility_id must differ (both %q)", crud.ErrHookRejected, fromFacilityID)
	}

	qty, ok := numberFieldValue(rec.Data["qty"])
	if !ok {
		return fmt.Errorf("%w: StockTransfer qty must be a number, got %T", crud.ErrHookRejected, rec.Data["qty"])
	}
	if qty <= 0 {
		return fmt.Errorf("%w: StockTransfer qty must be greater than zero, got %v", crud.ErrHookRejected, qty)
	}

	return nil
}

// numberFieldValue reads a FieldNumber value out of a record's data,
// accepting exactly the Go types entity.validateFieldValue itself blesses
// for FieldNumber — float64 (what every JSON, form and CSV path
// produces), plus int/int64 (what a Go caller writing a literal, e.g.
// cmd/seed-demo-data, produces). A plain `v.(float64)` assertion — the
// shape the ledger-posting hooks in this file use, where the consequence
// is a silently-zero posting rather than a rejection — would read an
// int-typed qty as 0 here and reject a perfectly valid transfer with
// "qty must be greater than zero, got 0", i.e. a validator disagreeing
// with the validator immediately upstream of it about what a number is.
func numberFieldValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// MatchVendorInvoiceOnUpdate is a crud.Hook (internal/kernel/crud.Hook)
// — register it for the "VendorInvoice" entity type (Engine.SetHook) to
// run the 3-way match the moment a VendorInvoice's status resolves to
// "matched": the PO leg confirms this invoice's own vendor_id actually
// agrees with the PurchaseOrder it claims to bill against (not just that
// purchase_order_id resolves to a real row — a value-only check would
// let a wrong-vendor invoice through as long as the total happened to
// match, a real gap independent review found in this hook's first
// version), and the Receipt <-> Invoice leg checks that this invoice's
// total agrees (to the cent, via ledger.ToMinorUnits) with the sum of
// everything actually received against its PurchaseOrder (every
// GoodsReceiptLine.qty_received × its matching POLine.unit_price).
//
// Deliberately posts nothing to the ledger on a successful match. This
// kernel's PostGoodsReceiptLineToLedger (above) already books Dr
// Inventory / Cr Accounts Payable (glAccountAPAccrual, "2100") the
// moment goods are physically received — a real accrual-basis liability,
// not a suspense/clearing balance. A second Dr/Cr Accounts Payable
// posting here would double-book the same liability already on the
// books, not complete it. In textbook 3-way-match accounting,
// GoodsReceipt would post to a dedicated GR/IR clearing account instead,
// and *this* hook would be the one that clears it into the real Accounts
// Payable balance — but that means changing PostGoodsReceiptLineToLedger's
// own account target, a structural change to already-shipped, already-
// posted-in-production ledger behavior, not something to fold silently
// into this task. Still flagged as real future work, not forgotten.
//
// A disagreement no longer rejects the transition outright (the
// behavior this hook shipped with first, "Phase 1"). Instead the write
// is redirected within the same transaction: status_id lands on
// "match_exception" instead of whatever the caller requested, and
// match_exception_reason on the record carries why. That redirect is
// itself a distinct mutation from the one the caller asked for — the
// caller's Update (and its own audit.Insert, in crud.Engine.Update)
// recorded an attempt to reach "matched"; what actually landed is
// different, so this hook writes its own audit entry for the override,
// the same "an in-hook write gets its own audit row, in the same tx" the
// ledger package already established (ledger.PostTx's own
// audit.Insert). On a later match that agrees, if match_exception_reason
// was set, it's cleared the same way, again with its own audit entry —
// but a match that agrees on the very first attempt writes nothing
// extra, matching this hook's original "purely a control gate, no
// side effect on success" shape.
//
// Runs on every Update where status resolves to "matched", not only the
// literal draft->matched or match_exception->matched transitions —
// deliberately more conservative than a one-time check, so an invoice
// can't silently drift out of agreement with what's been received after
// the fact and never be re-validated: editing a "matched" invoice's own
// total (a same-status "matched"->"matched" update, still a real Update
// this hook runs on — see crud/status.go's ValidateStatusTransition
// no-op-transition branch, which is a request-shape concern separate
// from this hook, which reads the record's own data, not the requested
// edge) into disagreement now correctly drifts it into match_exception
// with a reason instead of the whole edit being rolled back. Re-running
// this is cheap (a handful of List calls) and has no side effect to
// guard for idempotency against on the success path, unlike
// PostCustomerInvoiceToLedger's ExistsForSource guard.
func MatchVendorInvoiceOnUpdate(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	if action != audit.ActionUpdate {
		// A brand-new VendorInvoice is always created in "draft" (the
		// only is_initial status, purchasing.PublishStatuses) — never
		// matched yet, nothing to check on create.
		return nil
	}

	statusID, _ := rec.Data["status_id"].(string)
	if statusID == "" {
		return nil
	}
	records := data.NewRecordRepo(nil)
	status, err := records.GetTx(ctx, tx, "Status", statusID)
	if err != nil {
		return fmt.Errorf("resolve Status %s: %w", statusID, err)
	}
	if code, _ := status.Data["code"].(string); code != "matched" {
		return nil
	}
	statusTypeID, _ := status.Data["status_type_id"].(string)

	matchDetail, err := vendorInvoiceMatchDetail(ctx, tx, records, rec)
	if err != nil {
		// A hard/infra failure (missing purchase_order_id, a dangling
		// reference, a DB error) — not a business disagreement the
		// match-exception workflow is meant to hold open for correction.
		// Fails closed exactly as this hook always has: the whole
		// transaction rolls back, nothing written.
		return err
	}

	previousReason, _ := rec.Data["match_exception_reason"].(string)
	if matchDetail == "" {
		// Matched cleanly. Nothing to do on the common path (a first-
		// attempt match that just agrees) — only write when there's a
		// previous exception reason to clear, so a routine "->matched"
		// update stays the zero-extra-write control gate it always was.
		if previousReason == "" {
			return nil
		}
		return clearVendorInvoiceMatchException(ctx, tx, records, rec, actor)
	}

	exceptionStatusID, err := statusIDByCodeTx(ctx, tx, records, statusTypeID, statusCodeMatchException)
	if err != nil {
		return fmt.Errorf("resolve %s status: %w", statusCodeMatchException, err)
	}
	newData := mergedRecordData(rec.Data, map[string]any{
		"status_id":              exceptionStatusID,
		"match_exception_reason": matchDetail,
	})
	// expectedVersion = rec.Version, not nil: this row was already
	// write-locked by the caller's own UpdateTx earlier in this same
	// transaction (crud.Engine.Update), so no concurrent write can
	// interleave — nil would be equally safe here. Passing rec.Version
	// anyway costs nothing and catches a future bug where rec is stale by
	// the time this runs (e.g. a hook refactor that re-fetches rec
	// mid-function without updating this call).
	expectedVersion := rec.Version
	if _, err := records.UpdateTx(ctx, tx, "VendorInvoice", rec.ID, newData, &expectedVersion); err != nil {
		return fmt.Errorf("redirect VendorInvoice %s to %s: %w", rec.ID, statusCodeMatchException, err)
	}
	auditEntry, err := audit.New("VendorInvoice", rec.ID, audit.ActionUpdate, actor, map[string]any{
		"status_id":              exceptionStatusID,
		"match_exception_reason": matchDetail,
	})
	if err != nil {
		return fmt.Errorf("build audit entry for %s redirect: %w", statusCodeMatchException, err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry for %s redirect: %w", statusCodeMatchException, err)
	}
	return nil
}

// clearVendorInvoiceMatchException blanks a previously-set
// match_exception_reason once a later match agrees (a retry from
// match_exception->matched, or a matched invoice whose data was edited
// back into agreement) — its own write, its own audit entry, same
// reasoning as the redirect path in MatchVendorInvoiceOnUpdate above.
func clearVendorInvoiceMatchException(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, rec data.Record, actor audit.Actor) error {
	newData := mergedRecordData(rec.Data, map[string]any{"match_exception_reason": ""})
	// See MatchVendorInvoiceOnUpdate's own comment on why expectedVersion
	// is rec.Version here rather than nil.
	expectedVersion := rec.Version
	if _, err := records.UpdateTx(ctx, tx, "VendorInvoice", rec.ID, newData, &expectedVersion); err != nil {
		return fmt.Errorf("clear match_exception_reason on VendorInvoice %s: %w", rec.ID, err)
	}
	auditEntry, err := audit.New("VendorInvoice", rec.ID, audit.ActionUpdate, actor, map[string]any{"match_exception_reason": ""})
	if err != nil {
		return fmt.Errorf("build audit entry clearing match_exception_reason: %w", err)
	}
	if err := data.NewAuditRepo(nil).Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry clearing match_exception_reason: %w", err)
	}
	return nil
}

// mergedRecordData copies base and overlays overrides on top —
// RecordRepo.UpdateTx (internal/data/records.go) replaces a record's
// stored data wholesale (crud.Engine.Update's own doc comment, which
// calls the same repo method, describes the contract exactly: "Update
// validates and applies a full replacement of fields"), so a caller that
// wants to change two fields on an otherwise-unrelated record must
// resend every field or silently wipe the rest.
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

// statusCodeMatchException is the vendor_invoice_status code
// MatchVendorInvoiceOnUpdate redirects a disagreeing VendorInvoice to —
// see purchasing.PublishStatuses (seed.go) for the seeded Status/
// StatusTransition rows this code names.
const statusCodeMatchException = "match_exception"

// statusIDByCodeTx resolves one Status record's id within a specific
// StatusType by its code — the mirror of the "id -> code" lookup this
// hook already does when it reads the caller-requested status_id above,
// needed here because the hook decides to redirect *to* a status by code
// rather than only reading one the caller already resolved to an id.
// Scoped by statusTypeID, not code alone, for the same reason
// statusgraph.Seed's own doc comment gives for its identical scoping:
// two StatusTypes can share a Status code, and an unscoped lookup could
// silently resolve the wrong StatusType's row. No field-level query
// exists for this (same known gap receivedValueForPurchaseOrder below
// already documents) — every Status row is read and filtered in Go.
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
	return "", fmt.Errorf("status %q not seeded for status type %s (purchasing.PublishStatuses not run for this tenant?)", code, statusTypeID)
}

// vendorInvoiceMatchDetail runs the 3-way match against rec and reports
// the outcome: ("", nil) means it agrees, (reason, nil) means the match
// disagrees in some way a human can resolve by correcting data and
// retrying (the caller redirects rec to match_exception with reason —
// this is deliberately the outcome for EVERY ordinary business
// disagreement, including "the referenced PurchaseOrder doesn't exist"
// and "the PurchaseOrder has no lines yet": independent review found the
// first version of this split treated those two as hard/unresolvable,
// which was wrong — a dangling reference or a header entered before its
// lines are just as correctable as a wrong vendor_id or a bad total, and
// failing the whole transaction closed over them is strictly worse than
// this task's own "block payment release, not the write" goal). (_, err)
// is reserved for what's actually NOT resolvable by editing this
// invoice's own data: a missing purchase_order_id (Required at the
// schema level — entity.ValidateRecord should already block this before
// a hook ever runs; this is a defensive check for a hook invoked
// directly, bypassing that) and genuine infrastructure failures (a real
// DB error, not "not found") — those still fail closed, same as this
// hook's very first version did for every case.
func vendorInvoiceMatchDetail(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, rec data.Record) (string, error) {
	poID, _ := rec.Data["purchase_order_id"].(string)
	if poID == "" {
		return "", fmt.Errorf("%w: VendorInvoice %s has no purchase_order_id", ErrVendorInvoiceMatchFailed, rec.ID)
	}
	total, _ := rec.Data["total"].(float64)

	po, err := records.GetTx(ctx, tx, "PurchaseOrder", poID)
	if err != nil {
		if errors.Is(err, data.ErrNotFound) {
			return fmt.Sprintf("PurchaseOrder %s does not exist", poID), nil
		}
		return "", fmt.Errorf("%w: resolve PurchaseOrder %s: %w", ErrVendorInvoiceMatchFailed, poID, err)
	}
	// The PO leg of the 3-way match: confirm this invoice's own vendor_id
	// actually agrees with the PurchaseOrder it claims to bill against —
	// found by independent review (this hook originally only checked
	// value, which would let an invoice carrying the wrong vendor_id but
	// a value that happens to match still pass).
	vendorID, _ := rec.Data["vendor_id"].(string)
	poVendorID, _ := po.Data["vendor_id"].(string)
	if vendorID == "" || vendorID != poVendorID {
		return fmt.Sprintf("VendorInvoice vendor_id %q does not match PurchaseOrder %s's own vendor_id %q",
			vendorID, poID, poVendorID), nil
	}

	receivedMinor, hasLines, err := receivedValueForPurchaseOrder(ctx, tx, records, poID)
	if err != nil {
		return "", fmt.Errorf("%w: compute received value for PurchaseOrder %s: %w", ErrVendorInvoiceMatchFailed, poID, err)
	}
	if !hasLines {
		return fmt.Sprintf("PurchaseOrder %s has no POLines yet", poID), nil
	}
	if receivedMinor == 0 {
		return fmt.Sprintf("PurchaseOrder %s has nothing received against it yet", poID), nil
	}
	// total is VendorInvoice.total, still FieldNumber (major-unit float)
	// — that field is NOT part of uc-infra#136's migration, so it still
	// needs converting to minor units here.
	//
	// Deliberately NOT currency-aware via this invoice's own currency_id
	// (uc-infra#163 investigated this: unlike sales.PostCustomerInvoiceToLedger,
	// which posts a standalone Dr/Cr pair at whatever scale is chosen, this
	// comparison's OTHER side — receivedMinor, from receivedValueForPurchaseOrder
	// below — sums POLine.unit_price, which is money.Money and therefore
	// pinned at this package's fixed, currency-agnostic money.Decimals (2dp)
	// scale by design, not the PO's or invoice's own currency. Converting
	// totalMinor at a real per-currency scale while receivedMinor stays
	// pinned at 2dp would compare two different scales and silently break
	// this match for any non-2dp currency — worse than today's already-
	// documented fixed-2dp assumption, not better. A correct fix needs
	// receivedValueForPurchaseOrder itself to become currency-aware too
	// (recovering a major-unit amount from POLine.unit_price and
	// re-scaling), which is a bigger, separately-scoped change — tracked
	// as uc-infra#193 (filed this cycle) rather than ballooned into
	// this one. money.FromMajorUnits still buys this call site the
	// NaN/±Inf/overflow validation ledger.ToMinorUnits never had.
	totalMinorMoney, err := money.FromMajorUnits(total, money.Decimals)
	if err != nil {
		// A corrupt total (NaN/±Inf/overflow) is a data problem this
		// invoice's own record can't answer for — not an ordinary
		// business match disagreement, so this fails the hook outright
		// (ErrVendorInvoiceMatchFailed) rather than routing to
		// match_exception like every other mismatch below.
		return "", fmt.Errorf("%w: VendorInvoice %s: convert total to minor units: %w", ErrVendorInvoiceMatchFailed, rec.ID, err)
	}
	if totalMinor := int64(totalMinorMoney); receivedMinor != totalMinor {
		return fmt.Sprintf("total %.2f (%d minor units) does not match received value %.2f (%d minor units) for PurchaseOrder %s",
			total, totalMinor, money.Money(receivedMinor).Major(), receivedMinor, poID), nil
	}
	return "", nil
}

// receivedValueForPurchaseOrder sums qty_received x unit_price across
// every GoodsReceiptLine posted against one of poID's own POLines — the
// "how much has actually been received, and at what value" side of the
// 3-way match. Returns the sum in MINOR units (uc-infra#136:
// POLine.unit_price is FieldMoney now, so a per-line qty*price product
// is already minor-unit-scaled — see this function's own body for why
// that means plain int64(math.Round(...)) at the end, not
// ledger.ToMinorUnits, the same distinction PostGoodsReceiptLineToLedger's
// own amountMinor computation draws). hasLines reports whether poID has
// any POLine at all (a PO header can legitimately exist with none yet —
// POLine is its own separate CRUD-able entity, created after the
// header, per POLine's own doc comment — so "no lines" is a normal
// in-progress state for vendorInvoiceMatchDetail to redirect on, not
// this function's own concern to fail over). No SQL filter exists for
// this (this kernel's generic records table has no per-field query
// today, same known gap erp/BACKLOG-TASKS.md's list-page pagination
// item names) — both lists are read in full and filtered in Go, the
// same shape ledger.checkPeriodOpen already established for "no
// field-level query, filter every row of a bounded entity type in
// application code."
func receivedValueForPurchaseOrder(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, poID string) (receivedMinor int64, hasLines bool, err error) {
	poLines, err := records.ListTx(ctx, tx, "POLine")
	if err != nil {
		return 0, false, fmt.Errorf("list POLine: %w", err)
	}
	unitPriceMinorByLineID := make(map[string]money.Money)
	for _, l := range poLines {
		if pid, _ := l.Data["purchase_order_id"].(string); pid == poID {
			// money.FromAny, not a bare `.(float64)` assertion — same
			// permissive-on-error posture as PostGoodsReceiptLineToLedger's
			// own unitPriceMinor read above (an unreadable value degrades
			// to Money(0) rather than aborting the whole match).
			price, _ := money.FromAny(l.Data["unit_price"])
			unitPriceMinorByLineID[l.ID] = price
		}
	}
	if len(unitPriceMinorByLineID) == 0 {
		return 0, false, nil
	}

	grLines, err := records.ListTx(ctx, tx, "GoodsReceiptLine")
	if err != nil {
		return 0, false, fmt.Errorf("list GoodsReceiptLine: %w", err)
	}
	// Accumulated as float64 and rounded to int64 only ONCE at the end,
	// not per-line: qty is a fractional FieldNumber, so each line's own
	// qty*priceMinor product can itself be fractional (e.g. 2.5 units at
	// 1050 minor units = 2625.0, already exact, but a less round qty
	// wouldn't be), and rounding once after summing avoids compounding a
	// separate rounding error into every individual line before they're
	// added together.
	//
	// NOT the same strategy PostGoodsReceiptLineToLedger's own
	// amountMinor computation uses (independent review, uc-infra#136:
	// an earlier version of this comment claimed sameness — it isn't):
	// that function rounds PER LINE, once per GoodsReceiptLine, since
	// each line posts its own independent journal entry the moment it's
	// created. The two CAN disagree by a minor unit on genuinely
	// fractional quantities — two GoodsReceiptLines of qty 1.5 against a
	// 1-minor-unit price post round(1.5)+round(1.5)=4 to the ledger
	// across those two entries, while this function's own sum-then-round
	// computes round(3.0)=3 for the match — a real, pre-existing
	// (not introduced by this migration) rounding-strategy mismatch
	// between "what got posted" and "what this match compares against",
	// tracked as its own gap rather than papered over here.
	var sum float64
	for _, l := range grLines {
		poLineID, _ := l.Data["po_line_id"].(string)
		priceMinor, ok := unitPriceMinorByLineID[poLineID]
		if !ok {
			continue
		}
		qty, _ := l.Data["qty_received"].(float64)
		sum += qty * float64(priceMinor)
	}
	return int64(math.Round(sum)), true, nil
}
