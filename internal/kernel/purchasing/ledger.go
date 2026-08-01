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

	receivedValue, hasLines, err := receivedValueForPurchaseOrder(ctx, tx, records, poID)
	if err != nil {
		return "", fmt.Errorf("%w: compute received value for PurchaseOrder %s: %w", ErrVendorInvoiceMatchFailed, poID, err)
	}
	if !hasLines {
		return fmt.Sprintf("PurchaseOrder %s has no POLines yet", poID), nil
	}
	if receivedValue == 0 {
		return fmt.Sprintf("PurchaseOrder %s has nothing received against it yet", poID), nil
	}
	if receivedMinor, totalMinor := ledger.ToMinorUnits(receivedValue), ledger.ToMinorUnits(total); receivedMinor != totalMinor {
		return fmt.Sprintf("total %.2f (%d minor units) does not match received value %.2f (%d minor units) for PurchaseOrder %s",
			total, totalMinor, receivedValue, receivedMinor, poID), nil
	}
	return "", nil
}

// receivedValueForPurchaseOrder sums qty_received x unit_price across
// every GoodsReceiptLine posted against one of poID's own POLines — the
// "how much has actually been received, and at what value" side of the
// 3-way match. hasLines reports whether poID has any POLine at all (a PO
// header can legitimately exist with none yet — POLine is its own
// separate CRUD-able entity, created after the header, per POLine's own
// doc comment — so "no lines" is a normal in-progress state for
// vendorInvoiceMatchDetail to redirect on, not this function's own
// concern to fail over). No SQL filter exists for this (this kernel's
// generic records table has no per-field query today, same known gap
// erp/BACKLOG-TASKS.md's list-page pagination item names) — both lists
// are read in full and filtered in Go, the same shape
// ledger.checkPeriodOpen already established for "no field-level query,
// filter every row of a bounded entity type in application code."
func receivedValueForPurchaseOrder(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, poID string) (total float64, hasLines bool, err error) {
	poLines, err := records.ListTx(ctx, tx, "POLine")
	if err != nil {
		return 0, false, fmt.Errorf("list POLine: %w", err)
	}
	unitPriceByLineID := make(map[string]float64)
	for _, l := range poLines {
		if pid, _ := l.Data["purchase_order_id"].(string); pid == poID {
			price, _ := l.Data["unit_price"].(float64)
			unitPriceByLineID[l.ID] = price
		}
	}
	if len(unitPriceByLineID) == 0 {
		return 0, false, nil
	}

	grLines, err := records.ListTx(ctx, tx, "GoodsReceiptLine")
	if err != nil {
		return 0, false, fmt.Errorf("list GoodsReceiptLine: %w", err)
	}
	var sum float64
	for _, l := range grLines {
		poLineID, _ := l.Data["po_line_id"].(string)
		price, ok := unitPriceByLineID[poLineID]
		if !ok {
			continue
		}
		qty, _ := l.Data["qty_received"].(float64)
		sum += qty * price
	}
	return sum, true, nil
}
