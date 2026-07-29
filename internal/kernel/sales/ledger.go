package sales

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
)

// glAccountAR/glAccountRevenue are the chart-of-accounts codes this
// module posts issued CustomerInvoices to — matching internal/kernel/
// finance's default seeded chart ("1200 Accounts Receivable", "4100
// Sales Revenue"). Same documented limitation as purchasing's own
// glAccountInventory/glAccountAPAccrual constants: a customized chart of
// accounts needs matching codes for this to keep resolving.
const (
	glAccountAR      = "1200"
	glAccountRevenue = "4100"
)

// PostCustomerInvoiceToLedger is a crud.Hook (internal/kernel/crud.Hook)
// — register it for the "CustomerInvoice" entity type (Engine.SetHook)
// to post Dr Accounts Receivable / Cr Sales Revenue for total the moment
// an invoice's status transitions to "issued".
//
// Unlike purchasing's GoodsReceiptLine hook (which only ever fires
// once, on create), CustomerInvoice starts in draft (no posting yet —
// an unissued invoice has no financial reality) and only becomes real
// when issued, via Update. This runs on every Update, not just the
// first, and resolves the record's *current* status code itself rather
// than trying to diff against a "before" state (crud.Engine.Update
// doesn't fetch one — see its own doc comment) — an explicit
// data.JournalEntryRepo.ExistsForSource idempotency check guards against
// posting twice if this ever somehow gets called again while already
// issued (e.g. issued -> paid should never re-post the issued entry).
// Only the draft->issued transition posts anything here; issued->paid
// (cash debit / AR credit) is real future work, not attempted in this
// pass (see CustomerInvoice's own doc comment and erp/BACKLOG-TASKS.md).
func PostCustomerInvoiceToLedger(ctx context.Context, tx *sql.Tx, _ *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	if action != audit.ActionUpdate {
		// A brand-new CustomerInvoice is always created in "draft" (the
		// only is_initial status, sales.PublishStatuses) — never
		// financially real yet, nothing to post on create.
		return nil
	}

	statusID, _ := rec.Data["status_id"].(string)
	if statusID == "" {
		return nil
	}
	status, err := data.NewRecordRepo(nil).GetTx(ctx, tx, "Status", statusID)
	if err != nil {
		return fmt.Errorf("resolve Status %s: %w", statusID, err)
	}
	if code, _ := status.Data["code"].(string); code != "issued" {
		return nil
	}

	entries := data.NewJournalEntryRepo(tx)
	alreadyPosted, err := entries.ExistsForSource(ctx, "CustomerInvoice", rec.ID)
	if err != nil {
		return fmt.Errorf("check existing posting for CustomerInvoice %s: %w", rec.ID, err)
	}
	if alreadyPosted {
		return nil
	}

	total, _ := rec.Data["total"].(float64)
	amountMinor := ledger.ToMinorUnits(total)
	if amountMinor == 0 {
		// A zero-total invoice (data-entry in progress, a fully-
		// discounted order) has nothing to post — same reasoning
		// purchasing.PostGoodsReceiptLineToLedger gives for a zero-value
		// line, not a swallowed error.
		return nil
	}

	invoiceDate, _ := rec.Data["invoice_date"].(string)
	_, err = ledger.PostTx(ctx, tx, ledger.Entry{
		Date:        invoiceDate,
		Description: fmt.Sprintf("Customer invoice %s issued", rec.ID),
		SourceType:  "CustomerInvoice",
		SourceID:    rec.ID,
		Lines: []ledger.Line{
			{AccountID: glAccountAR, DebitMinor: amountMinor},
			{AccountID: glAccountRevenue, CreditMinor: amountMinor},
		},
	}, actor)
	if err != nil {
		return fmt.Errorf("post CustomerInvoice %s to ledger: %w", rec.ID, err)
	}
	return nil
}
