package purchasing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
)

// ErrVendorInvoiceMatchFailed is returned by MatchVendorInvoiceOnUpdate
// when a VendorInvoice's draft->matched transition doesn't agree with
// what was actually received against its PurchaseOrder — a control gate,
// not a bug: the caller (crud.Engine.Update) rolls back the whole
// transaction on any hook error (crud.go's own contract), so a failed
// match leaves the invoice exactly where it was, nothing written.
var ErrVendorInvoiceMatchFailed = errors.New("vendor invoice match failed")

// glAccountInventory/glAccountAPAccrual are the chart-of-accounts codes
// (finance.Account.code / gl_accounts.code) this module posts
// GoodsReceipt lines to — matching internal/kernel/finance's default
// seeded chart (cmd/seed-demo-data's seedFinance: "1300 Inventory",
// "2100 Accounts Payable"). A tenant that customizes its chart of
// accounts away from these exact codes needs matching ones for this
// posting to keep resolving — a real, documented limitation of this
// first slice (same category of known gap as finance.defaultGLCurrency),
// not a hidden assumption.
const (
	glAccountInventory = "1300"
	glAccountAPAccrual = "2100"
)

// PostGoodsReceiptLineToLedger is a crud.Hook (internal/kernel/crud.Hook)
// — register it for the "GoodsReceiptLine" entity type
// (Engine.SetHook) to post Dr Inventory / Cr AP Accrual for one received
// line's value the moment the line is created.
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
// Runs inside tx, the same transaction the GoodsReceiptLine write
// itself is in (crud.Hook's own contract) — a posting failure rolls back
// the line write too, never leaving an un-posted receipt line behind.
func PostGoodsReceiptLineToLedger(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	if action != audit.ActionCreate {
		// GoodsReceiptLine has no status/quantity-correction workflow
		// yet — nothing to do on Update (there's no real update path for
		// a received line today; if one lands later, it needs its own
		// reversing/adjusting entry, not a silent re-post here).
		return nil
	}

	qty, _ := rec.Data["qty_received"].(float64)
	poLineID, _ := rec.Data["po_line_id"].(string)
	if poLineID == "" {
		return fmt.Errorf("GoodsReceiptLine %s has no po_line_id", rec.ID)
	}
	goodsReceiptID, _ := rec.Data["goods_receipt_id"].(string)
	if goodsReceiptID == "" {
		return fmt.Errorf("GoodsReceiptLine %s has no goods_receipt_id", rec.ID)
	}

	records := data.NewRecordRepo(nil)
	poLine, err := records.GetTx(ctx, tx, "POLine", poLineID)
	if err != nil {
		return fmt.Errorf("resolve POLine %s: %w", poLineID, err)
	}
	unitPrice, _ := poLine.Data["unit_price"].(float64)

	goodsReceipt, err := records.GetTx(ctx, tx, "GoodsReceipt", goodsReceiptID)
	if err != nil {
		return fmt.Errorf("resolve GoodsReceipt %s: %w", goodsReceiptID, err)
	}
	receivedDate, _ := goodsReceipt.Data["received_date"].(string)

	amountMinor := ledger.ToMinorUnits(qty * unitPrice)
	if amountMinor == 0 {
		// A zero-value line (no price on the POLine yet, a free-of-
		// charge sample) has nothing to post — Entry.Validate would
		// reject a zero-amount line as ErrBadLine anyway, so skipping
		// here is a real, legitimate no-op, not a swallowed error.
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
// Deliberately posts nothing to the ledger. This kernel's
// PostGoodsReceiptLineToLedger (above) already books Dr Inventory / Cr
// Accounts Payable (glAccountAPAccrual, "2100") the moment goods are
// physically received — a real accrual-basis liability, not a
// suspense/clearing balance. A second Dr/Cr Accounts Payable posting
// here would double-book the same liability already on the books, not
// complete it. In textbook 3-way-match accounting, GoodsReceipt would
// post to a dedicated GR/IR clearing account instead, and *this* hook
// would be the one that clears it into the real Accounts Payable
// balance — but that means changing PostGoodsReceiptLineToLedger's own
// account target, a structural change to already-shipped, already-
// posted-in-production ledger behavior, not something to fold
// silently into this task. Flagged as real future work, not forgotten
// — erp/BACKLOG-TASKS.md Phase 3's "VendorInvoice entity + 3-way match"
// entry (match-exception workflow, payment-release gating) is exactly
// where that revisit belongs, once a real need for it shows up. Until
// then, this hook is purely a control gate: it either allows the
// draft->matched transition (nothing further to write — the liability
// is already correctly recorded) or blocks it outright.
//
// Runs on every Update where status resolves to "matched", not only the
// literal draft->matched transition — deliberately more conservative
// than a one-time check, so an invoice can't silently drift out of
// agreement with what's been received after the fact and never be
// re-validated. Re-running this is cheap (two List calls) and has no
// side effect to guard for idempotency against, unlike
// PostCustomerInvoiceToLedger's ExistsForSource guard — there's no
// journal entry here to post twice.
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

	poID, _ := rec.Data["purchase_order_id"].(string)
	if poID == "" {
		return fmt.Errorf("VendorInvoice %s has no purchase_order_id", rec.ID)
	}
	total, _ := rec.Data["total"].(float64)

	// The PO leg of the 3-way match: confirm this invoice's own vendor_id
	// actually agrees with the PurchaseOrder it claims to bill against —
	// found by independent review (this hook originally only checked
	// value, which would let an invoice carrying the wrong vendor_id but
	// a value that happens to match still pass). purchase_order_id is
	// FK-real (entity.FieldReference, Required) so a mismatch here is a
	// data problem, not a missing-link no-op like the "nothing received"
	// case above — fails closed the same way.
	po, err := records.GetTx(ctx, tx, "PurchaseOrder", poID)
	if err != nil {
		return fmt.Errorf("resolve PurchaseOrder %s: %w", poID, err)
	}
	vendorID, _ := rec.Data["vendor_id"].(string)
	poVendorID, _ := po.Data["vendor_id"].(string)
	if vendorID == "" || vendorID != poVendorID {
		return fmt.Errorf("%w: VendorInvoice %s vendor_id %q does not match PurchaseOrder %s's own vendor_id %q",
			ErrVendorInvoiceMatchFailed, rec.ID, vendorID, poID, poVendorID)
	}

	receivedValue, err := receivedValueForPurchaseOrder(ctx, tx, records, poID)
	if err != nil {
		return fmt.Errorf("compute received value for PurchaseOrder %s: %w", poID, err)
	}
	if receivedValue == 0 {
		return fmt.Errorf("%w: VendorInvoice %s: PurchaseOrder %s has nothing received against it yet",
			ErrVendorInvoiceMatchFailed, rec.ID, poID)
	}
	if ledger.ToMinorUnits(receivedValue) != ledger.ToMinorUnits(total) {
		return fmt.Errorf("%w: VendorInvoice %s total %.2f does not match received value %.2f for PurchaseOrder %s",
			ErrVendorInvoiceMatchFailed, rec.ID, total, receivedValue, poID)
	}
	return nil
}

// receivedValueForPurchaseOrder sums qty_received x unit_price across
// every GoodsReceiptLine posted against one of poID's own POLines — the
// "how much has actually been received, and at what value" side of the
// 3-way match. No SQL filter exists for this (this kernel's generic
// records table has no per-field query today, same known gap
// erp/BACKLOG-TASKS.md's list-page pagination item names) — both lists
// are read in full and filtered in Go, the same shape
// ledger.checkPeriodOpen already established for "no field-level query,
// filter every row of a bounded entity type in application code."
func receivedValueForPurchaseOrder(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, poID string) (float64, error) {
	poLines, err := records.ListTx(ctx, tx, "POLine")
	if err != nil {
		return 0, fmt.Errorf("list POLine: %w", err)
	}
	unitPriceByLineID := make(map[string]float64)
	for _, l := range poLines {
		if pid, _ := l.Data["purchase_order_id"].(string); pid == poID {
			price, _ := l.Data["unit_price"].(float64)
			unitPriceByLineID[l.ID] = price
		}
	}
	if len(unitPriceByLineID) == 0 {
		return 0, fmt.Errorf("%w: no POLine found for PurchaseOrder %s", ErrVendorInvoiceMatchFailed, poID)
	}

	grLines, err := records.ListTx(ctx, tx, "GoodsReceiptLine")
	if err != nil {
		return 0, fmt.Errorf("list GoodsReceiptLine: %w", err)
	}
	var total float64
	for _, l := range grLines {
		poLineID, _ := l.Data["po_line_id"].(string)
		price, ok := unitPriceByLineID[poLineID]
		if !ok {
			continue
		}
		qty, _ := l.Data["qty_received"].(float64)
		total += qty * price
	}
	return total, nil
}
