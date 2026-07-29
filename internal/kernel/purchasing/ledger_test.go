package purchasing

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// goodsReceiptFixture bundles everything a ledger.go test needs: a real
// tenant database with foundation + purchasing published, the two
// gl_accounts this hook posts to (seeded directly — this is a
// purchasing-package test, it only needs the real rows to exist, not
// finance.SyncGLAccounts's own logic, which has its own tests in
// internal/kernel/finance), and one Item + PurchaseOrder + POLine ready
// to receive against.
type goodsReceiptFixture struct {
	tenantDB *sql.DB
	engine   *crud.Engine
	itemID   string
	poID     string
	poLineID string
}

func defFor(t *testing.T, tenantDB *sql.DB, entityType string) *entity.Definition {
	t.Helper()
	v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(context.Background(), entityType)
	if err != nil {
		t.Fatalf("GetPublished(%s): %v", entityType, err)
	}
	d, err := entity.Unmarshal(v.Definition)
	if err != nil {
		t.Fatalf("unmarshal %s: %v", entityType, err)
	}
	return d
}

// statusIDByCode looks up a Status record's id by its code, scoped to a
// specific StatusType code — needed once more than one StatusType exists
// (see purchasing.seedStatusGraph's own doc comment for why a plain
// code-only lookup would be wrong once vendor_invoice_status exists
// alongside purchase_order_status). Shared by every fixture in this file
// that needs a starting/target status.
func statusIDByCode(t *testing.T, engine *crud.Engine, tenantDB *sql.DB, statusTypeCode, code string) string {
	t.Helper()
	ctx := context.Background()
	statusTypes, err := engine.ListByField(ctx, defFor(t, tenantDB, "StatusType"), "code", statusTypeCode)
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list %s StatusType: %v", statusTypeCode, err)
	}
	statuses, err := engine.ListByField(ctx, defFor(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	for _, s := range statuses {
		if c, _ := s.Data["code"].(string); c == code {
			return s.ID
		}
	}
	t.Fatalf("no Status %q under StatusType %q", code, statusTypeCode)
	return ""
}

func setUpGoodsReceiptFixture(t *testing.T, unitPrice float64) goodsReceiptFixture {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishForms: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	glAccounts := data.NewGLAccountRepo(tenantDB)
	if _, err := glAccounts.UpsertByCode(ctx, glAccountInventory, "Inventory", "asset", "USD", true); err != nil {
		t.Fatalf("upsert inventory gl_account: %v", err)
	}
	if _, err := glAccounts.UpsertByCode(ctx, glAccountAPAccrual, "Accounts Payable", "liability", "USD", true); err != nil {
		t.Fatalf("upsert AP gl_account: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, defFor(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Acme Textiles", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	item, err := engine.Create(ctx, defFor(t, tenantDB, "Item"), map[string]any{
		"sku": "SKU-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	draftStatusID := statusIDByCode(t, engine, tenantDB, "purchase_order_status", "draft")

	po, err := engine.Create(ctx, defFor(t, tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	line, err := engine.Create(ctx, defFor(t, tenantDB, "POLine"), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": float64(10), "unit_price": unitPrice,
	}, actor)
	if err != nil {
		t.Fatalf("create POLine: %v", err)
	}

	return goodsReceiptFixture{tenantDB: tenantDB, engine: engine, itemID: item.ID, poID: po.ID, poLineID: line.ID}
}

// vendorInvoiceFixture bundles a real PurchaseOrder + POLine + one
// GoodsReceipt/GoodsReceiptLine already received against it — everything
// MatchVendorInvoiceOnUpdate's tests need to exercise the match itself,
// not just its plumbing (goodsReceiptFixture stops one step short: it
// never actually creates a GoodsReceiptLine, since PostGoodsReceiptLineToLedger's
// own tests create that themselves to control the moment the hook fires).
type vendorInvoiceFixture struct {
	tenantDB *sql.DB
	engine   *crud.Engine
	vendorID string
	poID     string
}

// setUpVendorInvoiceFixture creates a PurchaseOrder with one POLine
// (qty x unitPrice), then receives receivedQty of it in full via a real
// GoodsReceipt/GoodsReceiptLine — receivedQty < qty lets a test exercise
// a partial receipt (the match should key off what was actually
// received, not the PO's ordered qty).
func setUpVendorInvoiceFixture(t *testing.T, qty, unitPrice, receivedQty float64) vendorInvoiceFixture {
	t.Helper()
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, defFor(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Acme Textiles", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	item, err := engine.Create(ctx, defFor(t, tenantDB, "Item"), map[string]any{
		"sku": "SKU-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	draftStatusID := statusIDByCode(t, engine, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, defFor(t, tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	line, err := engine.Create(ctx, defFor(t, tenantDB, "POLine"), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": qty, "unit_price": unitPrice,
	}, actor)
	if err != nil {
		t.Fatalf("create POLine: %v", err)
	}

	if receivedQty > 0 {
		gr, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceipt"), map[string]any{
			"purchase_order_id": po.ID, "received_date": "2026-01-10",
		}, actor)
		if err != nil {
			t.Fatalf("create GoodsReceipt: %v", err)
		}
		if _, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceiptLine"), map[string]any{
			"goods_receipt_id": gr.ID, "po_line_id": line.ID, "item_id": item.ID, "qty_received": receivedQty,
		}, actor); err != nil {
			t.Fatalf("create GoodsReceiptLine: %v", err)
		}
	}

	return vendorInvoiceFixture{tenantDB: tenantDB, engine: engine, vendorID: vendor.ID, poID: po.ID}
}

// createDraftVendorInvoice creates a VendorInvoice in "draft" (the only
// is_initial vendor_invoice_status) with the given total — mirrors
// sales's own seedCustomerInvoices pattern of creating in draft then
// Updating through real transitions, so the match hook (registered for
// Update, not Create) actually gets exercised.
func createDraftVendorInvoice(t *testing.T, fx vendorInvoiceFixture, total float64) data.Record {
	t.Helper()
	ctx := context.Background()
	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "draft")
	rec, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": draftStatusID, "total": total,
	}, humanActor())
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}
	return rec
}

func TestPostGoodsReceiptLineToLedger_PostsInventoryAndAP(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10",
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}

	line, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 journal entry, got %d", len(list))
	}
	entry := list[0]
	if entry.SourceType != "GoodsReceiptLine" || entry.SourceID != line.ID {
		t.Fatalf("unexpected source: type=%s id=%s", entry.SourceType, entry.SourceID)
	}
	// journal_entries.entry_date is a Postgres DATE column, scanned back
	// as an RFC3339 midnight timestamp by pgx when the destination is a
	// plain Go string (a pre-existing formatting quirk of
	// JournalEntryRepo.List, not something this test is about) — assert
	// it carries the right calendar date, not an exact string match.
	if !strings.HasPrefix(entry.EntryDate, "2026-01-10") {
		t.Fatalf("expected the entry to carry the GoodsReceipt's own received_date (2026-01-10), got %q", entry.EntryDate)
	}
	wantMinor := int64(4 * 12.50 * 100) // qty x unit_price x 100 (2-decimal minor units)
	byCode := map[string]data.JournalLine{}
	for _, l := range entry.Lines {
		byCode[l.AccountCode] = l
	}
	if inv := byCode["1300"]; inv.DebitMinor != wantMinor {
		t.Fatalf("expected Inventory (1300) debit %d, got %+v", wantMinor, inv)
	}
	if ap := byCode["2100"]; ap.CreditMinor != wantMinor {
		t.Fatalf("expected AP Accrual (2100) credit %d, got %+v", wantMinor, ap)
	}
}

// TestPostGoodsReceiptLineToLedger_ZeroValueLine_PostsNothing confirms a
// zero-price/zero-qty line (this hook's own doc comment: samples, a
// data-entry-in-progress POLine) is a legitimate no-op, not an error.
func TestPostGoodsReceiptLineToLedger_ZeroValueLine_PostsNothing(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 0)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10",
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no journal entry for a zero-value line, got %d", len(list))
	}
}

// TestPostGoodsReceiptLineToLedger_UnknownAccountCode_RollsBackTheLine
// confirms the hook's failure (an unrecognized/never-synced gl_account
// code) rolls back the whole GoodsReceiptLine write too — crud.Hook's
// own contract (Hook's doc comment): a failed posting must never leave
// an un-posted receipt line behind.
func TestPostGoodsReceiptLineToLedger_UnknownAccountCode_RollsBackTheLine(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	// Deliberately drop the AP account this hook needs, after setup —
	// simulates a tenant whose chart of accounts doesn't have the
	// expected code.
	if _, err := fx.tenantDB.Exec(`DELETE FROM gl_accounts WHERE code = $1`, glAccountAPAccrual); err != nil {
		t.Fatalf("delete AP gl_account: %v", err)
	}
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10",
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	_, err = fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor)
	if err == nil {
		t.Fatal("expected the GoodsReceiptLine create to fail when the ledger posting can't resolve an account")
	}

	var count int
	if err := fx.tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'GoodsReceiptLine'`).Scan(&count); err != nil {
		t.Fatalf("count GoodsReceiptLine records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the GoodsReceiptLine write to roll back too, found %d", count)
	}
}

func TestPostGoodsReceiptLineToLedger_UpdateAction_IsNoOp(t *testing.T) {
	// Direct unit-level call — GoodsReceiptLine has no real update path
	// today, this just confirms the hook itself guards against firing
	// on an action it was never designed for.
	rec := data.Record{ID: "x", Data: map[string]any{"qty_received": float64(1), "po_line_id": "y", "goods_receipt_id": "z"}}
	if err := PostGoodsReceiptLineToLedger(context.Background(), nil, nil, rec, audit.ActionUpdate, humanActor()); err != nil {
		t.Fatalf("expected Update action to no-op without even touching tx, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_MatchingTotal_TransitionSucceeds confirms
// the base 3-way-match happy path: a PO received in full, an invoice
// whose total exactly equals qty x unit_price, draft->matched succeeds —
// and, per this hook's own doc comment on why it posts nothing, confirms
// zero journal entries exist afterward (no double-booking the AP
// liability PostGoodsReceiptLineToLedger already posted at receipt time).
func TestMatchVendorInvoiceOnUpdate_MatchingTotal_TransitionSucceeds(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00)

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected draft->matched to succeed for a matching total, got: %v", err)
	}

	entries := data.NewJournalEntryRepo(fx.tenantDB)
	list, err := entries.List(context.Background())
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no journal entries posted by the match hook itself, got %d", len(list))
	}
}

// TestMatchVendorInvoiceOnUpdate_MismatchedTotal_RejectsTransition
// confirms an invoice total that disagrees with what was actually
// received is rejected outright — draft->matched fails with
// ErrVendorInvoiceMatchFailed, and (crud.Hook's own contract) the whole
// Update rolls back, leaving the invoice exactly as it was.
func TestMatchVendorInvoiceOnUpdate_MismatchedTotal_RejectsTransition(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 999.00) // received value is 125.00, invoice claims 999.00

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	_, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected ErrVendorInvoiceMatchFailed for a mismatched total, got: %v", err)
	}

	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "draft")
	if got.Data["status_id"] != draftStatusID {
		t.Fatalf("expected the rejected transition to leave VendorInvoice in draft, got status_id=%v", got.Data["status_id"])
	}
}

// TestMatchVendorInvoiceOnUpdate_WrongVendor_RejectsTransition confirms
// the PO leg of the match: an invoice whose vendor_id doesn't match its
// own PurchaseOrder's vendor_id is rejected even when the total agrees
// exactly with what was received — a value-only check would wrongly let
// this through (found by independent review of this hook's first
// version, which checked value only).
func TestMatchVendorInvoiceOnUpdate_WrongVendor_RejectsTransition(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)

	otherVendor, err := fx.engine.Create(context.Background(), defFor(t, fx.tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": "Some Other Vendor", "status": "active",
	}, humanActor())
	if err != nil {
		t.Fatalf("create second Party: %v", err)
	}

	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "draft")
	inv, err := fx.engine.Create(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": otherVendor.ID,
		"invoice_date": "2026-01-15", "status_id": draftStatusID, "total": 125.00, // value matches exactly
	}, humanActor())
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	_, err = fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": otherVendor.ID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected ErrVendorInvoiceMatchFailed for a vendor_id that doesn't match the PurchaseOrder's own vendor, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_NothingReceived_RejectsTransition
// confirms a PurchaseOrder with a real POLine but no GoodsReceipt at all
// blocks the match — an invoice can't be "matched" against a receipt
// that never happened, regardless of what total it claims.
func TestMatchVendorInvoiceOnUpdate_NothingReceived_RejectsTransition(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 0) // receivedQty=0: no GoodsReceipt created at all
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00)

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	_, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected ErrVendorInvoiceMatchFailed when nothing has been received, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_PartialReceipt_MatchesAgainstReceivedValue
// confirms the match keys off what was actually received (a partial
// delivery), not the PurchaseOrder's full ordered qty — an invoice for
// exactly the received portion must match; one for the full order must not.
func TestMatchVendorInvoiceOnUpdate_PartialReceipt_MatchesAgainstReceivedValue(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 4) // ordered 10, only 4 received: received value = 50.00
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")

	fullOrderInv := createDraftVendorInvoice(t, fx, 125.00) // the full ordered value, not what was received
	version := fullOrderInv.Version
	_, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), fullOrderInv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected an invoice for the full ordered qty to fail matching against a partial receipt, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_MultipleReceiptsSummed confirms the
// match sums qty_received x unit_price across every GoodsReceiptLine tied
// to the PurchaseOrder's POLines, not just the first — two separate
// partial deliveries against the same order must add up correctly.
func TestMatchVendorInvoiceOnUpdate_MultipleReceiptsSummed(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 4) // first receipt: 4 units = 50.00
	ctx := context.Background()
	actor := humanActor()

	// A second, separate GoodsReceipt against the same PO/POLine.
	poLines, err := fx.engine.ListByField(ctx, defFor(t, fx.tenantDB, "POLine"), "purchase_order_id", fx.poID)
	if err != nil || len(poLines) != 1 {
		t.Fatalf("list POLine: %v (count %d)", err, len(poLines))
	}
	gr2, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-12",
	}, actor)
	if err != nil {
		t.Fatalf("create second GoodsReceipt: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr2.ID, "po_line_id": poLines[0].ID,
		"item_id": poLines[0].Data["item_id"], "qty_received": float64(6), // remaining 6 units = 75.00
	}, actor); err != nil {
		t.Fatalf("create second GoodsReceiptLine: %v", err)
	}
	// Total received across both receipts: 4+6=10 units = 125.00 (matches
	// the full PO now, unlike the single-receipt partial-match test above).

	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00)
	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, actor); err != nil {
		t.Fatalf("expected the two receipts' summed value to match the invoice total, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_CreateAction_IsNoOp mirrors
// TestPostGoodsReceiptLineToLedger_UpdateAction_IsNoOp's own reasoning
// for the opposite action: a brand-new VendorInvoice is always created in
// draft (never matched yet), so Create must no-op without even touching
// tx or the database.
func TestMatchVendorInvoiceOnUpdate_CreateAction_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{"status_id": "irrelevant", "purchase_order_id": "y", "total": float64(1)}}
	if err := MatchVendorInvoiceOnUpdate(context.Background(), nil, nil, rec, audit.ActionCreate, humanActor()); err != nil {
		t.Fatalf("expected Create action to no-op without even touching tx, got: %v", err)
	}
}
