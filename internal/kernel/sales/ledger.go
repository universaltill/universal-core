package sales

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
	"github.com/universaltill/universal-core/internal/kernel/money"
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

	records := data.NewRecordRepo(nil)
	statusID, _ := rec.Data["status_id"].(string)
	if statusID == "" {
		return nil
	}
	status, err := records.GetTx(ctx, tx, "Status", statusID)
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

	// total is CustomerInvoice.total, still FieldNumber (major-unit float)
	// — converted here at this package's fixed money.Decimals scale, NOT
	// this invoice's own currency_id (uc-infra#163 investigated making
	// this currency-aware and independent review caught why that's
	// unsafe today: journal_lines (internal/db/migrations/tenant/
	// 0001_init.sql) has no per-line currency/scale column at all —
	// debit_minor/credit_minor are bare integers with no way to record
	// which decimal scale produced them — and this kernel's GL is a
	// single base currency (finance.ResolveBaseCurrency) with every
	// other reader of a posted minor-unit amount (internal/kernel/saft's
	// formatMinor, money.Money.Major/String) unconditionally dividing by
	// 100. Posting a real 0dp-JPY-scaled minor-unit count into that same
	// column would post a technically-correct minor-unit value that
	// every one of those readers then silently misinterprets by up to
	// 100x (a ¥1000 invoice would post as 1000, then read back and
	// export via SAF-T as "10.00" — a real financial misstatement, not a
	// display quirk). Fixing this for real needs the GL itself to become
	// currency-scale-aware (a schema change, likely journal_lines
	// gaining its own scale/currency column) — tracked as a new,
	// separately-scoped issue rather than attempted here. Same
	// conclusion, same reasoning, as purchasing's own
	// vendorInvoiceMatchDetail (internal/kernel/purchasing/ledger.go),
	// which independently hit the identical constraint from the
	// receivedValueForPurchaseOrder side.
	total, _ := rec.Data["total"].(float64)
	amountMinorMoney, err := money.FromMajorUnits(total, money.Decimals)
	if err != nil {
		// Unlike the old hardcoded-2dp ledger.ToMinorUnits (which never
		// returned an error), money.FromMajorUnits validates its input
		// and can reject a corrupt total (NaN/±Inf/overflow) — that must
		// fail the posting loudly, never silently post a zero or
		// fabricated amount.
		return fmt.Errorf("CustomerInvoice %s: convert total to minor units: %w", rec.ID, err)
	}
	amountMinor := int64(amountMinorMoney)
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
