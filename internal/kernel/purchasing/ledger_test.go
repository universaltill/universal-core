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
// to receive against, plus a Facility (uc-infra#54: GoodsReceipt.facility_id
// is Required as of v2) every test's own GoodsReceipt create can point at.
type goodsReceiptFixture struct {
	tenantDB   *sql.DB
	engine     *crud.Engine
	itemID     string
	poID       string
	poLineID   string
	facilityID string
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

// createVendorParty creates a Party AND tags it with the vendor
// PartyRole — required (uc-infra#78) since PurchaseOrder.vendor_id now
// declares a TargetFilter requiring the referenced Party to actually
// hold that role; a bare Party (this file's fixtures all used to create
// before that change) is no longer a valid vendor_id.
func createVendorParty(t *testing.T, ctx context.Context, engine *crud.Engine, tenantDB *sql.DB, name string, actor audit.Actor) data.Record {
	t.Helper()
	party, err := engine.Create(ctx, defFor(t, tenantDB, "Party"), map[string]any{
		"party_type": "organization", "name": name, "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := engine.Create(ctx, defFor(t, tenantDB, "PartyRole"), map[string]any{
		"party_id": party.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("create vendor PartyRole: %v", err)
	}
	return party
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
	vendor := createVendorParty(t, ctx, engine, tenantDB, "Acme Textiles", actor)
	item, err := engine.Create(ctx, defFor(t, tenantDB, "Item"), map[string]any{
		"sku": "SKU-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	facility, err := engine.Create(ctx, defFor(t, tenantDB, "Facility"), map[string]any{
		"code": "MAIN", "name": "Main Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Facility: %v", err)
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

	return goodsReceiptFixture{
		tenantDB: tenantDB, engine: engine, itemID: item.ID, poID: po.ID, poLineID: line.ID,
		facilityID: facility.ID,
	}
}

// vendorInvoiceFixture bundles a real PurchaseOrder + POLine + one
// GoodsReceipt/GoodsReceiptLine already received against it — everything
// MatchVendorInvoiceOnUpdate's tests need to exercise the match itself,
// not just its plumbing (goodsReceiptFixture stops one step short: it
// never actually creates a GoodsReceiptLine, since PostGoodsReceiptLineToLedger's
// own tests create that themselves to control the moment the hook fires).
type vendorInvoiceFixture struct {
	tenantDB   *sql.DB
	engine     *crud.Engine
	vendorID   string
	poID       string
	facilityID string
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
	vendor := createVendorParty(t, ctx, engine, tenantDB, "Acme Textiles", actor)
	item, err := engine.Create(ctx, defFor(t, tenantDB, "Item"), map[string]any{
		"sku": "SKU-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	facility, err := engine.Create(ctx, defFor(t, tenantDB, "Facility"), map[string]any{
		"code": "MAIN", "name": "Main Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create Facility: %v", err)
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
			"purchase_order_id": po.ID, "received_date": "2026-01-10", "facility_id": facility.ID,
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

	return vendorInvoiceFixture{tenantDB: tenantDB, engine: engine, vendorID: vendor.ID, poID: po.ID, facilityID: facility.ID}
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
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
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

	// uc-infra#54: the same GoodsReceiptLine create must also have
	// credited a new InventoryItem row at the receiving facility.
	invDef := defFor(t, fx.tenantDB, "InventoryItem")
	invRecs, err := fx.engine.List(ctx, invDef)
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 1 {
		t.Fatalf("expected exactly 1 InventoryItem row credited by the receipt, got %d", len(invRecs))
	}
	inv := invRecs[0]
	if got, _ := inv.Data["item_id"].(string); got != fx.itemID {
		t.Errorf("InventoryItem.item_id = %q, want %q", got, fx.itemID)
	}
	if got, _ := inv.Data["facility_id"].(string); got != fx.facilityID {
		t.Errorf("InventoryItem.facility_id = %q, want the GoodsReceipt's own facility %q", got, fx.facilityID)
	}
	if got, _ := inv.Data["qty_on_hand"].(float64); got != 4 {
		t.Errorf("InventoryItem.qty_on_hand = %v, want 4 (the received qty)", got)
	}
	if got, _ := inv.Data["qty_available_to_promise"].(float64); got != 4 {
		t.Errorf("InventoryItem.qty_available_to_promise = %v, want 4 (credited alongside qty_on_hand)", got)
	}

	// The credit must have its own audit row, same discipline as every
	// other in-hook write (CLAUDE.md's ADR-0001 §14: never bolted on
	// after the fact).
	var auditCount int
	if err := fx.tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'InventoryItem' AND record_id = $1 AND action = 'create'`,
		inv.ID,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count InventoryItem audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 audit row for the credited InventoryItem, got %d", auditCount)
	}
}

// TestPostGoodsReceiptLineToLedger_ZeroValueLine_PostsNothing confirms a
// zero-price/zero-qty line (this hook's own doc comment: samples, a
// data-entry-in-progress POLine) is a legitimate no-op, not an error —
// but (uc-infra#54) also confirms the InventoryItem credit is NOT gated
// on the ledger posting: this fixture receives a real qty (4) of a
// zero-priced item (a free sample), which has nothing to post to the
// ledger but has still physically arrived and must still bump stock.
func TestPostGoodsReceiptLineToLedger_ZeroValueLine_PostsNothing(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 0)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
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

	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 1 {
		t.Fatalf("expected the InventoryItem credit to happen despite no ledger posting, got %d rows", len(invRecs))
	}
	if got, _ := invRecs[0].Data["qty_on_hand"].(float64); got != 4 {
		t.Errorf("InventoryItem.qty_on_hand = %v, want 4 even for a zero-priced line", got)
	}
}

// TestPostGoodsReceiptLineToLedger_ZeroQty_NoInventoryCredit confirms
// the reverse of the above: a zero qty_received line (a data-entry-in-
// progress line, not yet a real receipt) is a legitimate no-op on the
// InventoryItem side too — no phantom zero-quantity row.
func TestPostGoodsReceiptLineToLedger_ZeroQty_NoInventoryCredit(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(0),
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 0 {
		t.Fatalf("expected no InventoryItem row for a zero-qty line, got %d", len(invRecs))
	}
}

// TestPostGoodsReceiptLineToLedger_MultipleReceipts_InsertSeparateInventoryRows
// (uc-infra#54) pins the "insert a new row, don't upsert" design decision
// this hook's own doc comment explains: two GoodsReceiptLines crediting
// the same item at the same facility must each produce their own
// InventoryItem row, summing correctly through the reporting layer —
// exactly the "(item, facility) uniqueness is NOT enforced... every
// aggregate sums" convention ADR-0015 §2 established for this entity.
func TestPostGoodsReceiptLineToLedger_MultipleReceipts_InsertSeparateInventoryRows(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	for _, qty := range []float64{4, 6} {
		gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
			"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
		}, actor)
		if err != nil {
			t.Fatalf("create GoodsReceipt: %v", err)
		}
		if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
			"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
			"item_id": fx.itemID, "qty_received": qty,
		}, actor); err != nil {
			t.Fatalf("create GoodsReceiptLine: %v", err)
		}
	}

	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 2 {
		t.Fatalf("expected 2 separate InventoryItem rows (one per receipt), got %d", len(invRecs))
	}

	reporting := data.NewReportingRepo(fx.tenantDB)
	onHand, err := reporting.OnHandQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnHandQtyByItem: %v", err)
	}
	if got := onHand[fx.itemID]; got != 10 {
		t.Fatalf("on-hand for item = %v, want 10 (4 + 6 summed across the two credited rows)", got)
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
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
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

	// uc-infra#54: the InventoryItem credit runs earlier in the same
	// hook, in the same transaction — it must roll back too, not leave a
	// credited InventoryItem behind a receipt line that never committed.
	var invCount int
	if err := fx.tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'InventoryItem'`).Scan(&invCount); err != nil {
		t.Fatalf("count InventoryItem records: %v", err)
	}
	if invCount != 0 {
		t.Fatalf("expected the InventoryItem credit to roll back too, found %d", invCount)
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

// TestValidateStockTransfer_SameFacility_Rejected and its
// siblings below are direct unit-level calls — ValidateStockTransfer
// never touches tx or def (pure business-rule validation, no DB read/
// write of its own), the same "nil tx/def is safe" shape
// TestPostGoodsReceiptLineToLedger_UpdateAction_IsNoOp already
// establishes for a hook that guards on action before doing anything else.
// Every one of them runs the same assertion twice, once per action:
// these are invariants of what a StockTransfer is, so Update must reject
// exactly what Create rejects (the hook's own doc comment).
func TestValidateStockTransfer_SameFacility_Rejected(t *testing.T) {
	for _, action := range []audit.Action{audit.ActionCreate, audit.ActionUpdate} {
		rec := data.Record{ID: "x", Data: map[string]any{
			"from_facility_id": "f1", "to_facility_id": "f1", "qty": float64(5),
		}}
		err := ValidateStockTransfer(context.Background(), nil, nil, rec, action, humanActor())
		if err == nil {
			t.Fatalf("%s: expected an error when from_facility_id == to_facility_id", action)
		}
		if !errors.Is(err, crud.ErrHookRejected) {
			t.Errorf("%s: expected error to wrap crud.ErrHookRejected (so writeCrudError maps it to 400), got: %v", action, err)
		}
	}
}

func TestValidateStockTransfer_ZeroQty_Rejected(t *testing.T) {
	for _, action := range []audit.Action{audit.ActionCreate, audit.ActionUpdate} {
		rec := data.Record{ID: "x", Data: map[string]any{
			"from_facility_id": "f1", "to_facility_id": "f2", "qty": float64(0),
		}}
		err := ValidateStockTransfer(context.Background(), nil, nil, rec, action, humanActor())
		if err == nil {
			t.Fatalf("%s: expected an error when qty == 0", action)
		}
		if !errors.Is(err, crud.ErrHookRejected) {
			t.Errorf("%s: expected error to wrap crud.ErrHookRejected, got: %v", action, err)
		}
	}
}

func TestValidateStockTransfer_NegativeQty_Rejected(t *testing.T) {
	for _, action := range []audit.Action{audit.ActionCreate, audit.ActionUpdate} {
		rec := data.Record{ID: "x", Data: map[string]any{
			"from_facility_id": "f1", "to_facility_id": "f2", "qty": float64(-3),
		}}
		err := ValidateStockTransfer(context.Background(), nil, nil, rec, action, humanActor())
		if err == nil {
			t.Fatalf("%s: expected an error when qty < 0", action)
		}
		if !errors.Is(err, crud.ErrHookRejected) {
			t.Errorf("%s: expected error to wrap crud.ErrHookRejected, got: %v", action, err)
		}
	}
}

func TestValidateStockTransfer_ValidRecord_IsNoOp(t *testing.T) {
	for _, action := range []audit.Action{audit.ActionCreate, audit.ActionUpdate} {
		rec := data.Record{ID: "x", Data: map[string]any{
			"from_facility_id": "f1", "to_facility_id": "f2", "qty": float64(5),
		}}
		if err := ValidateStockTransfer(context.Background(), nil, nil, rec, action, humanActor()); err != nil {
			t.Fatalf("%s: expected a valid StockTransfer to pass, got: %v", action, err)
		}
	}
}

// TestValidateStockTransfer_AcceptsEveryNumericTypeEntityValidationDoes
// pins numberFieldValue against entity.validateFieldValue's own accepted
// set for FieldNumber (float64/int/int64). A plain float64 assertion
// reads an int-typed qty as 0 and rejects a valid transfer claiming
// "got 0" — reachable from any Go caller writing a literal, e.g.
// cmd/seed-demo-data, which already registers this hook.
func TestValidateStockTransfer_AcceptsEveryNumericTypeEntityValidationDoes(t *testing.T) {
	for _, qty := range []any{float64(5), int(5), int64(5)} {
		rec := data.Record{ID: "x", Data: map[string]any{
			"from_facility_id": "f1", "to_facility_id": "f2", "qty": qty,
		}}
		if err := ValidateStockTransfer(context.Background(), nil, nil, rec, audit.ActionCreate, humanActor()); err != nil {
			t.Errorf("qty %T(%v) is a valid FieldNumber value for entity.ValidateRecord, so this hook must accept it too, got: %v", qty, qty, err)
		}
	}
	// A non-numeric qty is rejected rather than silently read as 0 — the
	// error must say what actually arrived, not "got 0".
	rec := data.Record{ID: "x", Data: map[string]any{
		"from_facility_id": "f1", "to_facility_id": "f2", "qty": "5",
	}}
	err := ValidateStockTransfer(context.Background(), nil, nil, rec, audit.ActionCreate, humanActor())
	if err == nil || !errors.Is(err, crud.ErrHookRejected) {
		t.Fatalf("expected a non-numeric qty to be rejected as a hook rejection, got: %v", err)
	}
	if !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("expected the error to name the real problem (not a bogus zero-quantity), got: %v", err)
	}
}

// stockTransferFixture bundles a real tenant database with foundation +
// purchasing published, an Item, two Facility rows (so from_facility_id
// != to_facility_id is a real, distinct pair), and the stock_transfer_
// status "draft" Status id — everything a real end-to-end
// engine.Create("StockTransfer", ...) call needs.
type stockTransferFixture struct {
	tenantDB      *sql.DB
	engine        *crud.Engine
	itemID        string
	facilityAID   string
	facilityBID   string
	draftStatusID string
}

func setUpStockTransferFixture(t *testing.T) stockTransferFixture {
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
	item, err := engine.Create(ctx, defFor(t, tenantDB, "Item"), map[string]any{
		"sku": "SKU-ST-1", "name": "Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	facilityA, err := engine.Create(ctx, defFor(t, tenantDB, "Facility"), map[string]any{
		"code": "WH-A", "name": "Warehouse A", "facility_type": "warehouse",
	}, actor)
	if err != nil {
		t.Fatalf("create Facility A: %v", err)
	}
	facilityB, err := engine.Create(ctx, defFor(t, tenantDB, "Facility"), map[string]any{
		"code": "WH-B", "name": "Warehouse B", "facility_type": "warehouse",
	}, actor)
	if err != nil {
		t.Fatalf("create Facility B: %v", err)
	}
	draftStatusID := statusIDByCode(t, engine, tenantDB, "stock_transfer_status", "draft")

	return stockTransferFixture{
		tenantDB: tenantDB, engine: engine,
		itemID: item.ID, facilityAID: facilityA.ID, facilityBID: facilityB.ID,
		draftStatusID: draftStatusID,
	}
}

// TestValidateStockTransfer_ValidCreate_SucceedsEndToEnd confirms
// the hook is actually wired and firing through the real
// crud.Engine.Create path (not just callable directly, as the unit tests
// above already confirm) and that a valid StockTransfer is genuinely
// persisted.
func TestValidateStockTransfer_ValidCreate_SucceedsEndToEnd(t *testing.T) {
	fx := setUpStockTransferFixture(t)
	fx.engine.SetHook("StockTransfer", ValidateStockTransfer)
	ctx := context.Background()

	rec, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "StockTransfer"), map[string]any{
		"item_id": fx.itemID, "from_facility_id": fx.facilityAID, "to_facility_id": fx.facilityBID,
		"qty": float64(10), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, humanActor())
	if err != nil {
		t.Fatalf("expected a valid StockTransfer create to succeed, got: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("expected a real record ID back")
	}
}

// TestValidateStockTransfer_SameFacility_RollsBackTheWholeWrite
// confirms the hook rejection actually rolls back the write through the
// real engine — no orphaned StockTransfer record left behind — and that
// the rejection surfaces as crud.ErrHookRejected through the real
// Create path (Engine.runHook wraps it as "%s hook: %w", so this also
// confirms errors.Is still finds it through that extra wrapping layer).
func TestValidateStockTransfer_SameFacility_RollsBackTheWholeWrite(t *testing.T) {
	fx := setUpStockTransferFixture(t)
	fx.engine.SetHook("StockTransfer", ValidateStockTransfer)
	ctx := context.Background()

	_, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "StockTransfer"), map[string]any{
		"item_id": fx.itemID, "from_facility_id": fx.facilityAID, "to_facility_id": fx.facilityAID,
		"qty": float64(10), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, humanActor())
	if err == nil {
		t.Fatal("expected the create to fail when from_facility_id == to_facility_id")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected through the real Create path, got: %v", err)
	}

	var count int
	if err := fx.tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'StockTransfer'`).Scan(&count); err != nil {
		t.Fatalf("count StockTransfer records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the rejected StockTransfer write to roll back entirely, found %d", count)
	}
}

// TestValidateStockTransfer_ZeroQty_RollsBackTheWholeWrite is
// TestValidateStockTransfer_SameFacility_RollsBackTheWholeWrite's
// counterpart for the qty<=0 rejection.
func TestValidateStockTransfer_ZeroQty_RollsBackTheWholeWrite(t *testing.T) {
	fx := setUpStockTransferFixture(t)
	fx.engine.SetHook("StockTransfer", ValidateStockTransfer)
	ctx := context.Background()

	_, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "StockTransfer"), map[string]any{
		"item_id": fx.itemID, "from_facility_id": fx.facilityAID, "to_facility_id": fx.facilityBID,
		"qty": float64(0), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, humanActor())
	if err == nil {
		t.Fatal("expected the create to fail when qty == 0")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected through the real Create path, got: %v", err)
	}

	var count int
	if err := fx.tenantDB.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE entity_type = 'StockTransfer'`).Scan(&count); err != nil {
		t.Fatalf("count StockTransfer records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the rejected StockTransfer write to roll back entirely, found %d", count)
	}
}

// TestValidateStockTransfer_UpdateToSameFacility_RollsBackTheWholeWrite
// is the regression test for the hole an independent review measured on
// #13's first draft: the hook gated on audit.ActionCreate, so a
// StockTransfer created legitimately could be edited — through the
// ordinary PUT /api/records/StockTransfer/{id} route every entity has,
// and through the edit form a saved create form becomes — into exactly
// the same-facility, negative-quantity record the create path refuses.
// Reverting the hook to a create-only gate fails this test with the
// stored record holding from == to.
func TestValidateStockTransfer_UpdateToSameFacility_RollsBackTheWholeWrite(t *testing.T) {
	fx := setUpStockTransferFixture(t)
	fx.engine.SetHook("StockTransfer", ValidateStockTransfer)
	ctx := context.Background()
	def := defFor(t, fx.tenantDB, "StockTransfer")

	rec, err := fx.engine.Create(ctx, def, map[string]any{
		"item_id": fx.itemID, "from_facility_id": fx.facilityAID, "to_facility_id": fx.facilityBID,
		"qty": float64(10), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, humanActor())
	if err != nil {
		t.Fatalf("create a valid StockTransfer first: %v", err)
	}

	version := rec.Version
	_, err = fx.engine.Update(ctx, def, rec.ID, map[string]any{
		"item_id": fx.itemID, "from_facility_id": fx.facilityAID, "to_facility_id": fx.facilityAID,
		"qty": float64(-5), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, &version, humanActor())
	if err == nil {
		t.Fatal("expected an update to from == to (and qty < 0) to be rejected, not silently stored")
	}
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Errorf("expected error to wrap crud.ErrHookRejected through the real Update path, got: %v", err)
	}

	after, err := fx.engine.Get(ctx, def, rec.ID)
	if err != nil {
		t.Fatalf("re-read StockTransfer %s: %v", rec.ID, err)
	}
	if got, _ := after.Data["to_facility_id"].(string); got != fx.facilityBID {
		t.Errorf("expected the rejected update to roll back entirely, to_facility_id is now %q", got)
	}
	if got, _ := after.Data["qty"].(float64); got != 10 {
		t.Errorf("expected the rejected update to roll back entirely, qty is now %v", got)
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

// TestMatchVendorInvoiceOnUpdate_MismatchedTotal_RedirectsToMatchException
// confirms an invoice total that disagrees with what was actually
// received no longer rejects the transition (Phase 1's original,
// fail-closed behavior): the Update succeeds, but status_id lands on
// "match_exception" instead of "matched" and match_exception_reason
// carries why — the actual redirect this task adds.
func TestMatchVendorInvoiceOnUpdate_MismatchedTotal_RedirectsToMatchException(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 999.00) // received value is 125.00, invoice claims 999.00

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}

	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected the mismatched transition to land in match_exception, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, "999.00") || !strings.Contains(reason, "125.00") {
		t.Fatalf("expected match_exception_reason to name both totals, got %q", reason)
	}
}

// TestMatchVendorInvoiceOnUpdate_WrongVendor_RedirectsToMatchException
// confirms the PO leg of the match: an invoice whose vendor_id doesn't
// match its own PurchaseOrder's vendor_id redirects to match_exception
// even when the total agrees exactly with what was received — a
// value-only check would wrongly let this through (found by independent
// review of this hook's first version, which checked value only).
func TestMatchVendorInvoiceOnUpdate_WrongVendor_RedirectsToMatchException(t *testing.T) {
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
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": otherVendor.ID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected the wrong-vendor transition to land in match_exception, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, otherVendor.ID) {
		t.Fatalf("expected match_exception_reason to name the invoice's own vendor_id %q, got %q", otherVendor.ID, reason)
	}
}

// TestMatchVendorInvoiceOnUpdate_NothingReceived_RedirectsToMatchException
// confirms a PurchaseOrder with a real POLine but no GoodsReceipt at all
// redirects to match_exception — an invoice can't be "matched" against a
// receipt that never happened, regardless of what total it claims, but
// it's still a resolvable exception (wait for receipt, retry), not a
// hard failure.
func TestMatchVendorInvoiceOnUpdate_NothingReceived_RedirectsToMatchException(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 0) // receivedQty=0: no GoodsReceipt created at all
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00)

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected the nothing-received transition to land in match_exception, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, "nothing received") {
		t.Fatalf("expected match_exception_reason to say nothing was received, got %q", reason)
	}
}

// TestMatchVendorInvoiceOnUpdate_PartialReceipt_MatchesAgainstReceivedValue
// confirms the match keys off what was actually received (a partial
// delivery), not the PurchaseOrder's full ordered qty — an invoice for
// exactly the received portion must match; one for the full order must
// redirect to match_exception instead.
func TestMatchVendorInvoiceOnUpdate_PartialReceipt_MatchesAgainstReceivedValue(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 4) // ordered 10, only 4 received: received value = 50.00
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")

	fullOrderInv := createDraftVendorInvoice(t, fx, 125.00) // the full ordered value, not what was received
	version := fullOrderInv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), fullOrderInv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), fullOrderInv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected an invoice for the full ordered qty to redirect to match_exception against a partial receipt, got status_id=%v", got.Data["status_id"])
	}
}

// TestMatchVendorInvoiceOnUpdate_Retry_ClearsExceptionAndMatches confirms
// the actual resolution path: an invoice redirected to match_exception,
// then corrected (its total fixed to agree) and resubmitted for
// match_exception->matched, lands in "matched" with
// match_exception_reason cleared back to "".
func TestMatchVendorInvoiceOnUpdate_Retry_ClearsExceptionAndMatches(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 999.00) // wrong total: lands in match_exception

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the initial redirect to succeed, got: %v", err)
	}
	inException, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	previousReason, _ := inException.Data["match_exception_reason"].(string)
	if previousReason == "" {
		t.Fatal("fixture bug: expected the initial redirect to have set a match_exception_reason to retry against")
	}

	var auditCountBefore int
	if err := fx.tenantDB.QueryRow(`SELECT count(*) FROM audit_log WHERE record_id = $1`, inv.ID).Scan(&auditCountBefore); err != nil {
		t.Fatalf("count audit_log before retry: %v", err)
	}

	// Correct the total and retry match_exception->matched — echoing
	// match_exception_reason back in the submitted fields, the same way
	// a real edit form round-trips a hidden field it isn't changing
	// (formrender.buildHiddenFields): RecordRepo.UpdateTx is a full
	// replacement, so a caller that silently dropped this field would
	// already have cleared it via its OWN write, before this hook ever
	// runs — that's a systemic property of every field on every entity
	// in this kernel, not something specific to prove here. What this
	// test needs to prove is clearVendorInvoiceMatchException's own
	// behavior once a real previous reason is genuinely present.
	version = inException.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
		"match_exception_reason": previousReason,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the corrected retry to succeed, got: %v", err)
	}

	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != matchedStatusID {
		t.Fatalf("expected the retry to land in matched, got status_id=%v", got.Data["status_id"])
	}
	if reason, _ := got.Data["match_exception_reason"].(string); reason != "" {
		t.Fatalf("expected match_exception_reason cleared after a successful retry, got %q", reason)
	}

	// clearVendorInvoiceMatchException writes its own audit entry
	// (ledger.go's own doc comment on why) — confirm it actually ran,
	// not just that the field happened to end up empty because the
	// caller's own top-level Update already cleared it.
	var auditCountAfter int
	if err := fx.tenantDB.QueryRow(`SELECT count(*) FROM audit_log WHERE record_id = $1`, inv.ID).Scan(&auditCountAfter); err != nil {
		t.Fatalf("count audit_log after retry: %v", err)
	}
	// +1 for the caller's own Update, +1 for clearVendorInvoiceMatchException's.
	if auditCountAfter != auditCountBefore+2 {
		t.Fatalf("expected 2 new audit rows (the retry's own Update + clearVendorInvoiceMatchException's), got %d new (before=%d after=%d)",
			auditCountAfter-auditCountBefore, auditCountBefore, auditCountAfter)
	}
}

// TestMatchVendorInvoiceOnUpdate_DriftAfterMatch_RedirectsToMatchException
// confirms this hook's own "runs on every Update landing on matched, not
// just the literal transition" contract: an already-matched invoice whose
// total is edited into disagreement is redirected to match_exception with
// a reason, not silently left "matched" and not rolled back.
func TestMatchVendorInvoiceOnUpdate_DriftAfterMatch_RedirectsToMatchException(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00)

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the initial match to succeed, got: %v", err)
	}
	matched, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}

	// Edit the total on the already-matched invoice — same status_id
	// (matched->matched, a no-op transition per ValidateStatusTransition),
	// but the hook still re-checks because it reads the record's own
	// resolved status, not the requested edge.
	version = matched.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the drifted edit to succeed (redirected, not rolled back), got: %v", err)
	}

	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected the drift to redirect to match_exception, got status_id=%v", got.Data["status_id"])
	}
	if reason, _ := got.Data["match_exception_reason"].(string); reason == "" {
		t.Fatal("expected a non-empty match_exception_reason after drift")
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
		"purchase_order_id": fx.poID, "received_date": "2026-01-12", "facility_id": fx.facilityID,
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

// TestMatchException_PaidTransition_IsRejected confirms the actual
// "blocks payment release until resolved" mechanism structurally, not
// just via the seeded-graph shape TestPublishStatuses_SeedsVendorInvoiceGraph
// already checks: a VendorInvoice sitting in match_exception cannot be
// moved straight to "paid" — crud.Engine.ValidateStatusTransition (the
// same generic check internal/api runs before every Update) rejects it
// with ErrInvalidTransition, because match_exception->paid was never
// declared as an edge.
func TestMatchException_PaidTransition_IsRejected(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 999.00) // wrong total: lands in match_exception

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the initial redirect to succeed, got: %v", err)
	}
	inException, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}

	paidStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "paid")
	version = inException.Version
	err = fx.engine.ValidateStatusTransition(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"status_id": paidStatusID,
	}, false, &version)
	if !errors.Is(err, crud.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for match_exception->paid, got: %v", err)
	}
}

// TestMatchVendorInvoiceOnUpdate_DanglingPurchaseOrder_RedirectsToMatchException
// confirms a purchase_order_id that doesn't resolve to any real
// PurchaseOrder (a bad reference, or one that's been soft-deleted since)
// redirects to match_exception instead of hard-failing — independent
// review's own finding: fixing the reference and retrying is exactly as
// resolvable as a wrong vendor_id, so this shouldn't roll back any
// differently than that case does.
func TestMatchVendorInvoiceOnUpdate_DanglingPurchaseOrder_RedirectsToMatchException(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)

	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "draft")
	inv, err := fx.engine.Create(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": "00000000-0000-0000-0000-000000000000", "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": draftStatusID, "total": 125.00,
	}, humanActor())
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": "00000000-0000-0000-0000-000000000000", "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected a dangling purchase_order_id to redirect to match_exception, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, "does not exist") {
		t.Fatalf("expected match_exception_reason to say the PurchaseOrder doesn't exist, got %q", reason)
	}
}

// TestMatchVendorInvoiceOnUpdate_NoPOLines_RedirectsToMatchException
// confirms a PurchaseOrder header that exists but has no POLine at all
// yet (a real, ordinary in-progress state — POLine is its own separate
// CRUD-able entity, created after the header) redirects to
// match_exception rather than hard-failing.
func TestMatchVendorInvoiceOnUpdate_NoPOLines_RedirectsToMatchException(t *testing.T) {
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
	engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)

	vendor := createVendorParty(t, ctx, engine, tenantDB, "Acme Textiles", actor)
	draftPOStatusID := statusIDByCode(t, engine, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, defFor(t, tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftPOStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	// Deliberately no POLine created — a header can exist with none yet.

	draftInvStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "draft")
	inv, err := engine.Create(ctx, defFor(t, tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
		"invoice_date": "2026-01-15", "status_id": draftInvStatusID, "total": 125.00,
	}, actor)
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}

	matchedStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := engine.Update(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, actor); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := engine.Get(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected a PurchaseOrder with no POLines to redirect to match_exception, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, "no POLines") {
		t.Fatalf("expected match_exception_reason to say the PurchaseOrder has no lines, got %q", reason)
	}
}

// TestMatchVendorInvoiceOnUpdate_SecondConsecutiveFailure_UpdatesReason
// confirms match_exception_reason reflects the MOST RECENT failed
// attempt, not stuck on whatever the first one said — a retry that's
// still wrong (for a different reason) must overwrite it, not append or
// ignore it.
func TestMatchVendorInvoiceOnUpdate_SecondConsecutiveFailure_UpdatesReason(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00
	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 999.00) // wrong total #1

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 999.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the first redirect to succeed, got: %v", err)
	}
	first, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	firstReason, _ := first.Data["match_exception_reason"].(string)
	if !strings.Contains(firstReason, "999.00") {
		t.Fatalf("expected the first reason to mention 999.00, got %q", firstReason)
	}

	// Retry with a DIFFERENT wrong total — still disagrees, but not the
	// same way. match_exception->matched is a real declared edge; the
	// hook redirects back to match_exception again with a fresh reason.
	version = first.Version
	if _, err := fx.engine.Update(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 777.00,
		"match_exception_reason": firstReason,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the second redirect to succeed, got: %v", err)
	}
	second, err := fx.engine.Get(context.Background(), defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	secondReason, _ := second.Data["match_exception_reason"].(string)
	if !strings.Contains(secondReason, "777.00") {
		t.Fatalf("expected the second reason to mention the new total 777.00, got %q", secondReason)
	}
	if strings.Contains(secondReason, "999.00") {
		t.Fatalf("expected the reason to be replaced, not accumulated — still mentions the old 999.00: %q", secondReason)
	}
}

// TestMatchVendorInvoiceOnUpdate_NoPurchaseOrderID_HardFails confirms the
// one case vendorInvoiceMatchDetail still fails closed on: no
// purchase_order_id at all. Unreachable through the real API
// (purchase_order_id is Required — entity.ValidateRecord blocks this
// before crud.Engine.Update ever calls a hook), so this calls the hook
// directly, the same "direct unit-level call" shape
// TestPostGoodsReceiptLineToLedger_UpdateAction_IsNoOp already uses for
// a hook contract that can't be reached through the normal engine path.
func TestMatchVendorInvoiceOnUpdate_NoPurchaseOrderID_HardFails(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")

	tx, err := fx.tenantDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rec := data.Record{ID: "does-not-matter", Data: map[string]any{"status_id": matchedStatusID, "total": float64(1)}}
	err = MatchVendorInvoiceOnUpdate(context.Background(), tx, nil, rec, audit.ActionUpdate, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected ErrVendorInvoiceMatchFailed for a missing purchase_order_id, got: %v", err)
	}
}

// TestStatusIDByCodeTx_NotSeeded_ReturnsError confirms statusIDByCodeTx
// fails with a clear error (not a panic or a wrong id) when asked to
// resolve a code under a StatusType that has no such Status seeded —
// the "PublishStatuses not run for this tenant" case its own error
// message names.
func TestStatusIDByCodeTx_NotSeeded_ReturnsError(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	tx, err := fx.tenantDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	records := data.NewRecordRepo(nil)
	if _, err := statusIDByCodeTx(context.Background(), tx, records, "not-a-real-status-type-id", "match_exception"); err == nil {
		t.Fatal("expected an error for a status_type_id with no seeded Status rows")
	}
}

// TestMergedRecordData_OverlaysWithoutMutatingBase is a plain unit test
// (no DB) for the helper both the redirect and the clear paths in
// MatchVendorInvoiceOnUpdate rely on to avoid wiping a record's other
// fields — RecordRepo.UpdateTx is a full replacement (crud.Engine.Update's
// own doc comment, describing that same repo method: "Update validates
// and applies a full replacement of fields"), so a caller of it must
// always resend everything worth keeping.
func TestMergedRecordData_OverlaysWithoutMutatingBase(t *testing.T) {
	base := map[string]any{"a": float64(1), "b": "keep"}
	merged := mergedRecordData(base, map[string]any{"b": "changed", "c": true})

	if merged["a"] != float64(1) || merged["b"] != "changed" || merged["c"] != true {
		t.Fatalf("unexpected merged result: %+v", merged)
	}
	if base["b"] != "keep" {
		t.Fatalf("expected base map untouched, got %+v", base)
	}
	if _, ok := base["c"]; ok {
		t.Fatal("expected base map untouched — 'c' should not have leaked into it")
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
