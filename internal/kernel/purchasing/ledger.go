package purchasing

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
)

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
