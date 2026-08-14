package purchasing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/ledger"
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

// setUpGoodsReceiptFixture's unitPrice stays a human-typed major-unit
// dollar amount (uc-infra#136 kept every existing call site unchanged
// rather than converting each one to minor units by hand) — converted
// to POLine.unit_price's now-FieldMoney minor units here, at the one
// place that actually writes it.
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
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": float64(10), "unit_price": ledger.ToMinorUnits(unitPrice),
	}, actor)
	if err != nil {
		t.Fatalf("create POLine: %v", err)
	}

	return goodsReceiptFixture{
		tenantDB: tenantDB, engine: engine, itemID: item.ID, poID: po.ID, poLineID: line.ID,
		facilityID: facility.ID,
	}
}

// TestPurchaseOrder_DuplicatePONumberRejected is the real-Postgres
// end-to-end case for uc-infra#121's Unique declaration (the Definition-
// shape half is TestPurchaseOrder_UniqueOnPONumber in purchasing_test.go):
// a second PurchaseOrder created with an already-used po_number must be
// rejected by crud.Engine itself — record_unique_keys' real Postgres
// UNIQUE index (ADR-0018 §3(c)), not an application-level convention a
// caller could bypass.
func TestPurchaseOrder_DuplicatePONumberRejected(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	ctx := context.Background()
	actor := humanActor()

	vendor := createVendorParty(t, ctx, fx.engine, fx.tenantDB, "Beta Supplies", actor)
	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "purchase_order_status", "draft")

	_, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-02",
		"status_id": draftStatusID,
	}, actor)
	if err == nil {
		t.Fatal("expected the second PurchaseOrder with po_number \"PO-1\" to be rejected")
	}
	var uniqueErr *crud.UniqueConstraintError
	if !errors.As(err, &uniqueErr) {
		t.Fatalf("expected a *crud.UniqueConstraintError, got %T: %v", err, err)
	}
	if !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected errors.Is(err, crud.ErrUniqueConstraintViolation): %v", err)
	}
	if uniqueErr.EntityType != "PurchaseOrder" {
		t.Errorf("UniqueConstraintError.EntityType = %q, want %q", uniqueErr.EntityType, "PurchaseOrder")
	}
	if len(uniqueErr.Fields) != 1 || uniqueErr.Fields[0] != "po_number" {
		t.Errorf("UniqueConstraintError.Fields = %v, want [po_number]", uniqueErr.Fields)
	}

	// A DIFFERENT po_number for the same vendor must still succeed —
	// this isn't "at most one PO per vendor," only "po_number itself
	// must be distinct."
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-2", "vendor_id": vendor.ID, "order_date": "2026-01-02",
		"status_id": draftStatusID,
	}, actor); err != nil {
		t.Fatalf("expected a PurchaseOrder with a distinct po_number to succeed: %v", err)
	}
}

// TestItem_DuplicateSKURejected is the real-Postgres end-to-end case for
// uc-infra#181's Item.sku Unique declaration (the Definition-shape half
// is TestItem_UniqueOnSKU in purchasing_test.go): a second Item created
// with an already-used sku must be rejected by crud.Engine itself —
// record_unique_keys' real Postgres UNIQUE index (ADR-0018 §3(c)), not
// an application-level convention a caller could bypass. Mirrors
// TestPurchaseOrder_DuplicatePONumberRejected above exactly, one field
// over. setUpGoodsReceiptFixture already creates one Item with
// sku "SKU-1" as part of its own fixture setup, so this test only needs
// to attempt a second one.
func TestItem_DuplicateSKURejected(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	ctx := context.Background()
	actor := humanActor()

	_, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "Item"), map[string]any{
		"sku": "SKU-1", "name": "Duplicate Widget", "item_type": "stock",
	}, actor)
	if err == nil {
		t.Fatal("expected the second Item with sku \"SKU-1\" to be rejected")
	}
	var uniqueErr *crud.UniqueConstraintError
	if !errors.As(err, &uniqueErr) {
		t.Fatalf("expected a *crud.UniqueConstraintError, got %T: %v", err, err)
	}
	if !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected errors.Is(err, crud.ErrUniqueConstraintViolation): %v", err)
	}
	if uniqueErr.EntityType != "Item" {
		t.Errorf("UniqueConstraintError.EntityType = %q, want %q", uniqueErr.EntityType, "Item")
	}
	if len(uniqueErr.Fields) != 1 || uniqueErr.Fields[0] != "sku" {
		t.Errorf("UniqueConstraintError.Fields = %v, want [sku]", uniqueErr.Fields)
	}

	// A DIFFERENT sku must still succeed — this isn't "at most one Item,"
	// only "sku itself must be distinct."
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "Item"), map[string]any{
		"sku": "SKU-2", "name": "Another Widget", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("expected an Item with a distinct sku to succeed: %v", err)
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
// received, not the PO's ordered qty). unitPrice stays a human-typed
// major-unit dollar amount, same reasoning as setUpGoodsReceiptFixture's
// own doc comment (uc-infra#136) — converted to minor units at the one
// place it's actually written to POLine below.
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
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": qty, "unit_price": ledger.ToMinorUnits(unitPrice),
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

// TestPostGoodsReceiptLineToLedger_LegacyFractionalUnitPrice_RejectsReceipt
// (independent review of uc-infra#136's first pass) is the regression
// pin for a real bug the first version of this migration shipped: a
// legacy POLine.unit_price written before the FieldNumber->FieldMoney
// Version bump still holds a fractional major-unit decimal until
// cmd/backfill-poline-money converts it. The first draft discarded
// money.FromAny's error here, which fell through to unitPriceMinor==0
// and the amountMinor==0 "nothing to post" branch — silently crediting
// InventoryItem (a real physical receipt) while posting NO journal
// entry at all, an understated AP liability with no error anywhere.
// This pins the fix: the whole GoodsReceiptLine create must fail and
// roll back — including the InventoryItem credit, which must NOT have
// happened either — rather than silently under-post.
func TestPostGoodsReceiptLineToLedger_LegacyFractionalUnitPrice_RejectsReceipt(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	// Simulate a genuinely pre-migration row: entity.ValidateRecord would
	// reject this on any real write path now that unit_price is
	// FieldMoney, so the only way to reproduce it is a direct SQL
	// corruption of an already-created POLine, the same technique
	// cmd/backfill-poline-money's own tests use for "legacy" fixtures.
	if _, err := fx.tenantDB.ExecContext(context.Background(),
		`UPDATE records SET data = jsonb_set(data, '{unit_price}', '12.5') WHERE id = $1`, fx.poLineID,
	); err != nil {
		t.Fatalf("corrupt POLine unit_price to a legacy fractional value: %v", err)
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
		t.Fatal("expected GoodsReceiptLine create to fail against a legacy fractional POLine.unit_price")
	}
	if !strings.Contains(err.Error(), "invalid unit_price") {
		t.Fatalf("expected an invalid-unit_price error naming the cause, got: %v", err)
	}

	// Nothing must have been posted OR credited — the whole transaction
	// rolled back, not just the ledger half.
	entries, err := data.NewJournalEntryRepo(fx.tenantDB).List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected NO journal entry posted for a rejected receipt, got %d", len(entries))
	}
	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 0 {
		t.Fatalf("expected NO InventoryItem credited for a rejected receipt, got %d", len(invRecs))
	}
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

// TestPostGoodsReceiptLineToLedger_CreditValidatesAgainstPublishedNotCompiledIn
// is uc-infra#165's regression test: creditInventoryOnReceipt used to
// validate the InventoryItem credit against the binary's compiled-in
// InventoryItem() Definition, never the tenant's own PUBLISHED one. This
// publishes a newer InventoryItem version — adding a Required
// "lot_number" field the credit's own fields map never sets — on top of
// what setUpGoodsReceiptFixture already published via Publish(), then
// receives against it. Before the fix, the credit would validate against
// compiled-in v5 (no lot_number) and silently succeed despite the
// tenant's actual published Definition demanding the field; after the
// fix, it must fail with exactly the *entity.ValidationError this
// diverging published shape requires.
func TestPostGoodsReceiptLineToLedger_CreditValidatesAgainstPublishedNotCompiledIn(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	compiledIn := InventoryItem()
	diverged := *compiledIn
	diverged.Version = compiledIn.Version + 1
	diverged.Fields = append(append([]entity.Field{}, compiledIn.Fields...),
		entity.Field{Name: "lot_number", Type: entity.FieldString, Required: true},
	)
	raw, err := json.Marshal(&diverged)
	if err != nil {
		t.Fatalf("marshal diverged InventoryItem: %v", err)
	}
	repo := data.NewEntityDefinitionRepo(fx.tenantDB)
	if _, err := repo.CreateDraft(ctx, diverged.EntityType, diverged.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, diverged.EntityType, diverged.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, diverged.EntityType, diverged.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

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
		t.Fatal("expected the GoodsReceiptLine create to fail: the tenant's published InventoryItem now requires lot_number, which the credit never sets")
	}
	var verr *entity.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *entity.ValidationError, got %T: %v", err, err)
	}
	if verr.FieldName != "lot_number" {
		t.Fatalf("expected the validation failure to name lot_number, got %+v", verr)
	}

	// The whole write must have rolled back — no InventoryItem row, no
	// GoodsReceiptLine, no journal entry — same all-or-nothing posture
	// every other in-hook failure in this file already enforces.
	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 0 {
		t.Fatalf("expected no InventoryItem row after a rolled-back credit, got %d", len(invRecs))
	}
	lineRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"))
	if err != nil {
		t.Fatalf("list GoodsReceiptLine: %v", err)
	}
	if len(lineRecs) != 0 {
		t.Fatalf("expected no GoodsReceiptLine row after a rolled-back credit, got %d", len(lineRecs))
	}
	entries, err := data.NewJournalEntryRepo(fx.tenantDB).List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no journal entry after a rolled-back credit, got %d", len(entries))
	}
}

// TestPostGoodsReceiptLineToLedger_PublishedWithoutUniqueConstraint_FailsLoudly
// is the independent review's finding on uc-infra#165: routing the
// tenant's actual published InventoryItem Definition into
// Write/UpdateUniqueConstraintKeys means a tenant whose published version
// predates v5 (uc-infra#126, which is the version that first declared
// Unique(item_id, facility_id) — before that, nothing did) would
// otherwise silently lose this credit's concurrency-safety guarantee:
// WriteUniqueConstraintKeys/UpdateUniqueConstraintKeys are a no-op when
// Definition.Unique is empty, so no record_unique_keys row gets written
// and a concurrent create race becomes undetectable. This publishes a
// v4-shaped InventoryItem (all the same fields, deliberately no Unique)
// and asserts the credit now fails with a clear, actionable error
// instead of silently proceeding without the constraint it depends on.
func TestPostGoodsReceiptLineToLedger_PublishedWithoutUniqueConstraint_FailsLoudly(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	// getPublished/GetPublishedTx return the HIGHEST published version,
	// and setUpGoodsReceiptFixture's own Publish() already published the
	// compiled-in v5 (which DOES declare Unique) — so v5 must be rolled
	// back first, or the v4 draft below would just coexist unused and
	// this test would validate nothing.
	repoForRollback := data.NewEntityDefinitionRepo(fx.tenantDB)
	if err := repoForRollback.Rollback(ctx, "InventoryItem", InventoryItem().Version, actor); err != nil {
		t.Fatalf("Rollback compiled-in InventoryItem: %v", err)
	}

	preUniqueVersion := &entity.Definition{
		EntityType: "InventoryItem",
		Version:    4,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "facility_id", Type: entity.FieldReference, Required: true, Target: "Facility"},
			{Name: "qty_on_hand", Type: entity.FieldNumber, Required: true, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "qty_available_to_promise", Type: entity.FieldNumber, Required: true, Default: float64(0)},
		},
		// Deliberately no Unique — matching real pre-v5 InventoryItem.
	}
	raw, err := json.Marshal(preUniqueVersion)
	if err != nil {
		t.Fatalf("marshal pre-unique InventoryItem: %v", err)
	}
	repo := data.NewEntityDefinitionRepo(fx.tenantDB)
	if _, err := repo.CreateDraft(ctx, preUniqueVersion.EntityType, preUniqueVersion.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, preUniqueVersion.EntityType, preUniqueVersion.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, preUniqueVersion.EntityType, preUniqueVersion.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

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
		t.Fatal("expected the GoodsReceiptLine create to fail: the tenant's published InventoryItem (v4) doesn't declare Unique(item_id, facility_id)")
	}
	if !strings.Contains(err.Error(), "does not declare Unique(item_id, facility_id)") {
		t.Fatalf("expected a clear missing-Unique-constraint error, got %q", err.Error())
	}

	// Confirm this fails BEFORE any partial write, not mid-way through —
	// same all-or-nothing posture as every other rejection in this file.
	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 0 {
		t.Fatalf("expected no InventoryItem row, got %d", len(invRecs))
	}
}

// TestPostGoodsReceiptLineToLedger_InventoryItemNotPublished_FailsWithWrappedError
// covers the other side of uc-infra#165's fix: a tenant that somehow
// reaches this path with InventoryItem NOT published (its only published
// version explicitly rolled back here, since Publish() always publishes
// every module Definition together and nothing else can un-publish just
// one) must get a real, wrapped error — not a panic, not a silent
// fallback to the compiled-in Definition.
func TestPostGoodsReceiptLineToLedger_InventoryItemNotPublished_FailsWithWrappedError(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	repo := data.NewEntityDefinitionRepo(fx.tenantDB)
	if err := repo.Rollback(ctx, "InventoryItem", InventoryItem().Version, actor); err != nil {
		t.Fatalf("Rollback InventoryItem: %v", err)
	}

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
		t.Fatal("expected the GoodsReceiptLine create to fail: InventoryItem has no published Definition")
	}
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("expected errors.Is(err, data.ErrNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "look up published InventoryItem definition") {
		t.Fatalf("expected a clearly-wrapped lookup error, got %q", err.Error())
	}
}

// TestPostGoodsReceiptLineToLedger_InventoryItemCreditAppliesFieldDefaults
// is the regression test for uc-infra#218 (split from #212's independent
// review): creditInventoryOnReceipt creates the new InventoryItem row via
// the low-level records.CreateTx, not crud.Engine.Create, so it never got
// Engine.Create's own entity.ApplyDefaults call. Every field this hardcoded
// credit path sets (item_id/facility_id/qty_on_hand/qty_available_to_
// promise) happens to always be explicit, so this was latent, not an
// observed bug — this test proves it by publishing a v6 InventoryItem
// Definition that adds one more Required field carrying a Default this
// credit path does NOT set. Before uc-infra#218's fix, this would fail
// validation ("condition_code is required"); after it, ledger.go's own
// explicit entity.ApplyDefaults(def, fields) call (mirroring internal/
// api/handlers.go's identical pre-Engine.Create call) fills it in, same
// as every other write path into this Definition already does.
func TestPostGoodsReceiptLineToLedger_InventoryItemCreditAppliesFieldDefaults(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	withConditionCode := &entity.Definition{
		EntityType: "InventoryItem",
		Version:    6,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "facility_id", Type: entity.FieldReference, Required: true, Target: "Facility"},
			{Name: "qty_on_hand", Type: entity.FieldNumber, Required: true, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "qty_available_to_promise", Type: entity.FieldNumber, Required: true, Default: float64(0)},
			// Not set anywhere in creditInventoryOnReceipt's own fields
			// map — only entity.ApplyDefaults can supply it.
			{Name: "condition_code", Type: entity.FieldString, Required: true, Default: "good"},
		},
		// GetPublished/GetPublishedTx return the HIGHEST published
		// version (see the sibling test above), so publishing v6 makes
		// it authoritative without needing to roll back v5 first — and
		// keeps v5's Unique(item_id, facility_id), which this credit's
		// own upsert/retry design depends on.
		Unique: [][]string{{"item_id", "facility_id"}},
	}
	raw, err := json.Marshal(withConditionCode)
	if err != nil {
		t.Fatalf("marshal InventoryItem v6: %v", err)
	}
	repo := data.NewEntityDefinitionRepo(fx.tenantDB)
	if _, err := repo.CreateDraft(ctx, withConditionCode.EntityType, withConditionCode.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, withConditionCode.EntityType, withConditionCode.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, withConditionCode.EntityType, withConditionCode.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}

	if _, err = fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine: expected condition_code's Default to satisfy its own Required check via entity.ApplyDefaults, got: %v", err)
	}

	invRecs, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "InventoryItem"))
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 1 {
		t.Fatalf("expected exactly 1 InventoryItem row, got %d", len(invRecs))
	}
	if got, _ := invRecs[0].Data["condition_code"].(string); got != "good" {
		t.Fatalf("expected creditInventoryOnReceipt to apply condition_code's Default (\"good\") for the omitted field, got %q", got)
	}
}

// TestPostGoodsReceiptLineToLedger_PublishedDefinitionUnmarshalFails_WrapsTheError
// covers creditInventoryOnReceipt's entity.Unmarshal error branch —
// independent review, uc-infra#165: EntityDefinitionRepo.CreateDraft only
// json.Unmarshals a definition into a bare map[string]any to build its
// audit diff (definitions.go's createDraft); it never calls
// entity.Definition.Validate(), so structurally-valid-JSON-but-invalid-
// Definition content (here, a field with an unknown type — the exact
// "hand-edited in the database" case entity.Unmarshal's own doc comment
// exists to catch) can be drafted, approved, and published. This proves
// creditInventoryOnReceipt surfaces that as a real, wrapped error rather
// than panicking or silently falling back to compiled-in.
func TestPostGoodsReceiptLineToLedger_PublishedDefinitionUnmarshalFails_WrapsTheError(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	invalidRaw := []byte(`{
		"entity_type": "InventoryItem",
		"version": 6,
		"module": "purchasing",
		"fields": [{"name": "item_id", "type": "not_a_real_field_type"}]
	}`)
	repo := data.NewEntityDefinitionRepo(fx.tenantDB)
	if _, err := repo.CreateDraft(ctx, "InventoryItem", 6, invalidRaw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := repo.Approve(ctx, "InventoryItem", 6, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := repo.Publish(ctx, "InventoryItem", 6, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

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
		t.Fatal("expected the GoodsReceiptLine create to fail: the published InventoryItem definition is structurally invalid")
	}
	if !strings.Contains(err.Error(), "unmarshal published InventoryItem definition") {
		t.Fatalf("expected a clearly-wrapped unmarshal error, got %q", err.Error())
	}
}

// TestPostGoodsReceiptLineToLedger_SecondReceiptUpsertsInventoryItem is
// uc-infra#126's regression test: a second GoodsReceiptLine crediting
// the SAME (item_id, facility_id) pair must increment the existing
// InventoryItem row, not accumulate a second one. Before this fix,
// creditInventoryOnReceipt always CreateTx'd a new row (ledger.go's own
// former doc comment on that tradeoff) — this test fails against that
// unfixed behavior (2 rows, first row's qty left at 4 instead of 10) and
// passes once creditInventoryOnReceipt upserts.
func TestPostGoodsReceiptLineToLedger_SecondReceiptUpsertsInventoryItem(t *testing.T) {
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

	_, err = fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor)
	if err != nil {
		t.Fatalf("create first GoodsReceiptLine: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(6),
	}, actor); err != nil {
		t.Fatalf("create second GoodsReceiptLine: %v", err)
	}

	invDef := defFor(t, fx.tenantDB, "InventoryItem")
	invRecs, err := fx.engine.List(ctx, invDef)
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 1 {
		t.Fatalf("expected exactly 1 InventoryItem row after two receipts of the same (item, facility), got %d", len(invRecs))
	}
	inv := invRecs[0]
	if got, _ := inv.Data["qty_on_hand"].(float64); got != 10 {
		t.Errorf("InventoryItem.qty_on_hand = %v, want 10 (4 + 6, upserted)", got)
	}
	if got, _ := inv.Data["qty_available_to_promise"].(float64); got != 10 {
		t.Errorf("InventoryItem.qty_available_to_promise = %v, want 10 (4 + 6, upserted)", got)
	}
	if inv.Version != 2 {
		t.Errorf("InventoryItem.Version = %d, want 2 (one create + one update)", inv.Version)
	}

	// The second credit must have its own Update audit row — same
	// atomicity discipline the Create path already has, not silently
	// folded into (or skipped alongside) the first.
	var updateAuditCount int
	if err := fx.tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'InventoryItem' AND record_id = $1 AND action = 'update'`,
		inv.ID,
	).Scan(&updateAuditCount); err != nil {
		t.Fatalf("count InventoryItem update audit rows: %v", err)
	}
	if updateAuditCount != 1 {
		t.Fatalf("expected exactly 1 update audit row for the upserted InventoryItem, got %d", updateAuditCount)
	}

	// The first GoodsReceiptLine's credit must still have its own create
	// audit row — the fix must not remove or overwrite it.
	var createAuditCount int
	if err := fx.tenantDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'InventoryItem' AND record_id = $1 AND action = 'create'`,
		inv.ID,
	).Scan(&createAuditCount); err != nil {
		t.Fatalf("count InventoryItem create audit rows: %v", err)
	}
	if createAuditCount != 1 {
		t.Fatalf("expected exactly 1 create audit row for the InventoryItem, got %d", createAuditCount)
	}
}

// TestPostGoodsReceiptLineToLedger_ThirdFacilityReceiptCreatesNewRow
// confirms the upsert is scoped to (item_id, facility_id), not item_id
// alone — a receipt at a DIFFERENT facility for the same item must still
// create its own InventoryItem row, not merge into the first facility's.
func TestPostGoodsReceiptLineToLedger_ThirdFacilityReceiptCreatesNewRow(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 12.50)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	otherFacility, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "Facility"), map[string]any{
		"code": "SECOND", "name": "Second Warehouse", "facility_type": "warehouse", "is_active": true,
	}, actor)
	if err != nil {
		t.Fatalf("create second Facility: %v", err)
	}

	gr1, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt at first facility: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr1.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(4),
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine at first facility: %v", err)
	}

	gr2, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-11", "facility_id": otherFacility.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt at second facility: %v", err)
	}
	if _, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr2.ID, "po_line_id": fx.poLineID,
		"item_id": fx.itemID, "qty_received": float64(6),
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine at second facility: %v", err)
	}

	invDef := defFor(t, fx.tenantDB, "InventoryItem")
	invRecs, err := fx.engine.List(ctx, invDef)
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 2 {
		t.Fatalf("expected 2 InventoryItem rows (one per facility), got %d", len(invRecs))
	}
	byFacility := map[string]float64{}
	for _, rec := range invRecs {
		fid, _ := rec.Data["facility_id"].(string)
		qty, _ := rec.Data["qty_on_hand"].(float64)
		byFacility[fid] = qty
	}
	if byFacility[fx.facilityID] != 4 {
		t.Errorf("first facility qty_on_hand = %v, want 4", byFacility[fx.facilityID])
	}
	if byFacility[otherFacility.ID] != 6 {
		t.Errorf("second facility qty_on_hand = %v, want 6", byFacility[otherFacility.ID])
	}
}

// TestPostGoodsReceiptLineToLedger_ConcurrentReceiptsConverge is the
// adversarial case uc-infra#126 explicitly calls out: N GoodsReceiptLines
// against the SAME (item, facility) posted from concurrent transactions
// must still converge to exactly one InventoryItem row whose qty_on_hand
// is the exact sum — covering both retry paths creditInventoryOnReceipt
// needs (ErrVersionConflict on a losing UPDATE, and a losing CREATE
// racing the very first row into existence, only closable now that
// InventoryItem declares Unique on (item_id, facility_id), uc-infra#81).
func TestPostGoodsReceiptLineToLedger_ConcurrentReceiptsConverge(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 1.00)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	gr, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": fx.poID, "received_date": "2026-01-10", "facility_id": fx.facilityID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}

	const concurrency = 8
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			_, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
				"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID,
				"item_id": fx.itemID, "qty_received": float64(1),
			}, actor)
			errs <- err
		}()
	}
	for i := 0; i < concurrency; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent GoodsReceiptLine create: %v", err)
		}
	}

	invDef := defFor(t, fx.tenantDB, "InventoryItem")
	invRecs, err := fx.engine.List(ctx, invDef)
	if err != nil {
		t.Fatalf("list InventoryItem: %v", err)
	}
	if len(invRecs) != 1 {
		t.Fatalf("expected exactly 1 InventoryItem row after %d concurrent receipts of the same (item, facility), got %d", concurrency, len(invRecs))
	}
	if got, _ := invRecs[0].Data["qty_on_hand"].(float64); got != float64(concurrency) {
		t.Errorf("InventoryItem.qty_on_hand = %v, want %d (every concurrent receipt counted exactly once)", got, concurrency)
	}
}

// TestPostGoodsReceiptLineToLedger_DuplicateLegacyRows_ReturnsDiagnosableError
// covers creditInventoryOnReceipt's one deliberately-NOT-retried conflict
// (independent review, uc-infra#126): a pre-#126 tenant carrying two live
// InventoryItem rows for the same (item, facility), where only the newer
// one has ever been reconciled into record_unique_keys. GetByFieldsQ
// picks the older (unbackfilled) row; UpdateUniqueConstraintKeys' insert
// for it collides with the newer row's already-claimed key. Retrying
// would just pick the identical older row and hit the identical
// conflict again — not a transient race — so this must surface a clear,
// actionable error instead of looping or leaking an opaque wrapped one,
// and must leave nothing partially written.
func TestPostGoodsReceiptLineToLedger_DuplicateLegacyRows_ReturnsDiagnosableError(t *testing.T) {
	fx := setUpGoodsReceiptFixture(t, 10)
	fx.engine.SetHook("GoodsReceiptLine", PostGoodsReceiptLineToLedger)
	ctx := context.Background()
	actor := humanActor()

	invDef := defFor(t, fx.tenantDB, "InventoryItem")
	// The "backfilled" row: created normally through the engine, so it
	// gets its own record_unique_keys row automatically.
	newer, err := fx.engine.Create(ctx, invDef, map[string]any{
		"item_id": fx.itemID, "facility_id": fx.facilityID,
		"qty_on_hand": float64(5), "qty_available_to_promise": float64(5),
	}, actor)
	if err != nil {
		t.Fatalf("create backfilled InventoryItem: %v", err)
	}

	// The "pre-#126 legacy duplicate": inserted directly, bypassing the
	// engine (so no record_unique_keys row is ever written for it —
	// exactly the un-backfilled state a real tenant's old insert-only-
	// created duplicates would be in) and backdated so GetByFieldsQ's
	// (created_at, id) ordering deterministically picks THIS one first.
	var olderID string
	if err := fx.tenantDB.QueryRowContext(ctx,
		`INSERT INTO records (entity_type, data, created_at) VALUES (
		   'InventoryItem',
		   jsonb_build_object('item_id', $1::text, 'facility_id', $2::text, 'qty_on_hand', 3::numeric, 'qty_available_to_promise', 3::numeric),
		   now() - interval '1 day'
		 ) RETURNING id`,
		fx.itemID, fx.facilityID,
	).Scan(&olderID); err != nil {
		t.Fatalf("insert legacy duplicate InventoryItem: %v", err)
	}

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
		t.Fatal("expected the receipt to fail against duplicate legacy InventoryItem rows, got nil error")
	}
	if !strings.Contains(err.Error(), "duplicate live InventoryItem rows") {
		t.Fatalf("expected a diagnosable duplicate-rows error, got: %v", err)
	}
	if !strings.Contains(err.Error(), olderID) {
		t.Fatalf("expected the error to name the conflicting row %s, got: %v", olderID, err)
	}

	// The whole write must have rolled back cleanly: the untouched
	// (correctly-keyed) row is unchanged, and the GoodsReceiptLine that
	// triggered this never landed either — same transaction, no partial
	// credit anywhere.
	unchanged, err := fx.engine.Get(ctx, invDef, newer.ID)
	if err != nil {
		t.Fatalf("get newer InventoryItem: %v", err)
	}
	if got, _ := unchanged.Data["qty_on_hand"].(float64); got != 5 {
		t.Errorf("newer InventoryItem qty_on_hand = %v, want unchanged 5 (the failed credit must not have landed anywhere)", got)
	}
	lines, err := fx.engine.List(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"))
	if err != nil {
		t.Fatalf("list GoodsReceiptLine: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected the GoodsReceiptLine create to roll back entirely alongside the failed credit, got %d lines", len(lines))
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

// TestPostGoodsReceiptLineToLedger_MultipleReceipts_UpsertsOneInventoryRow
// (uc-infra#126, superseding #54's original "insert a new row, don't
// upsert" pin) confirms two GoodsReceiptLines crediting the same item at
// the same facility, via separate GoodsReceipts entirely — not just
// separate lines on one receipt, which
// TestPostGoodsReceiptLineToLedger_SecondReceiptUpsertsInventoryItem
// already covers — converge onto the SAME InventoryItem row, and that
// the reporting layer's aggregate still reports the correct total
// against that single row.
func TestPostGoodsReceiptLineToLedger_MultipleReceipts_UpsertsOneInventoryRow(t *testing.T) {
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
	if len(invRecs) != 1 {
		t.Fatalf("expected exactly 1 InventoryItem row (upserted across both receipts), got %d", len(invRecs))
	}

	reporting := data.NewReportingRepo(fx.tenantDB)
	onHand, err := reporting.OnHandQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnHandQtyByItem: %v", err)
	}
	if got := onHand[fx.itemID]; got != 10 {
		t.Fatalf("on-hand for item = %v, want 10 (4 + 6 summed in place on the one upserted row)", got)
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

// TestValidateGoodsReceiptLineQuality_NeitherFieldSet_IsNoOp (uc-infra#82)
// is the common case: a line with no quality data recorded at all, the
// same "additive optional field" state every GoodsReceiptLine written
// before this feature has.
func TestValidateGoodsReceiptLineQuality_NeitherFieldSet_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{"qty_received": float64(10)}}
	if err := validateGoodsReceiptLineQuality(rec); err != nil {
		t.Fatalf("expected no error with neither field set, got: %v", err)
	}
}

// TestValidateGoodsReceiptLineQuality_OnlyOneFieldSet_Rejected pins the
// required-together invariant: a line with only one of qty_accepted/
// qty_rejected is a partial, probably-still-being-entered record, not a
// real one.
func TestValidateGoodsReceiptLineQuality_OnlyOneFieldSet_Rejected(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
	}{
		{"only qty_accepted", map[string]any{"qty_received": float64(10), "qty_accepted": float64(8)}},
		{"only qty_rejected", map[string]any{"qty_received": float64(10), "qty_rejected": float64(2)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGoodsReceiptLineQuality(data.Record{ID: "x", Data: tc.data})
			if !errors.Is(err, crud.ErrHookRejected) {
				t.Fatalf("expected crud.ErrHookRejected, got: %v", err)
			}
			if !strings.Contains(err.Error(), "must both be set or both be omitted") {
				t.Fatalf("expected a required-together message, got: %v", err)
			}
		})
	}
}

// TestValidateGoodsReceiptLineQuality_NonNumericType_Rejected
// (independent review of uc-infra#82) is a direct unit-level call —
// entity.ValidateRecord already guarantees a real crud.Engine.Create/
// Update never reaches this hook with a non-numeric qty_accepted/
// qty_rejected (same reasoning ValidateStockTransfer's own
// AcceptsEveryNumericTypeEntityValidationDoes test documents), so this
// exercises the defensive branch directly, the same way
// TestPostGoodsReceiptLineToLedger_UpdateAction_IsNoOp already calls the
// hook function directly rather than through the engine.
func TestValidateGoodsReceiptLineQuality_NonNumericType_Rejected(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
	}{
		{"non-numeric qty_accepted", map[string]any{"qty_received": float64(10), "qty_accepted": "eight", "qty_rejected": float64(2)}},
		{"non-numeric qty_rejected", map[string]any{"qty_received": float64(10), "qty_accepted": float64(8), "qty_rejected": "two"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGoodsReceiptLineQuality(data.Record{ID: "x", Data: tc.data})
			if !errors.Is(err, crud.ErrHookRejected) {
				t.Fatalf("expected crud.ErrHookRejected, got: %v", err)
			}
			if !strings.Contains(err.Error(), "must be a number") {
				t.Fatalf("expected a not-a-number message, got: %v", err)
			}
		})
	}
}

// TestValidateGoodsReceiptLineQuality_NegativeQuantity_Rejected pins the
// non-negative invariant on both fields independently.
func TestValidateGoodsReceiptLineQuality_NegativeQuantity_Rejected(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
	}{
		{"negative qty_accepted", map[string]any{"qty_received": float64(10), "qty_accepted": float64(-1), "qty_rejected": float64(11)}},
		{"negative qty_rejected", map[string]any{"qty_received": float64(10), "qty_accepted": float64(11), "qty_rejected": float64(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGoodsReceiptLineQuality(data.Record{ID: "x", Data: tc.data})
			if !errors.Is(err, crud.ErrHookRejected) {
				t.Fatalf("expected crud.ErrHookRejected, got: %v", err)
			}
			if !strings.Contains(err.Error(), "must not be negative") {
				t.Fatalf("expected a non-negative message, got: %v", err)
			}
		})
	}
}

// TestValidateGoodsReceiptLineQuality_DoesNotNetToReceived_Rejected pins
// the core invariant: qty_accepted + qty_rejected must equal
// qty_received — a quality SPLIT of what arrived, not an independent
// count that can disagree with it.
func TestValidateGoodsReceiptLineQuality_DoesNotNetToReceived_Rejected(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{
		"qty_received": float64(10), "qty_accepted": float64(8), "qty_rejected": float64(1),
	}}
	err := validateGoodsReceiptLineQuality(rec)
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Fatalf("expected crud.ErrHookRejected, got: %v", err)
	}
	if !strings.Contains(err.Error(), "must equal qty_received") {
		t.Fatalf("expected a netting message, got: %v", err)
	}
}

// TestValidateGoodsReceiptLineQuality_NetsExactly_IsNoOp is the valid
// case: qty_accepted + qty_rejected == qty_received exactly.
func TestValidateGoodsReceiptLineQuality_NetsExactly_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{
		"qty_received": float64(10), "qty_accepted": float64(7), "qty_rejected": float64(3),
	}}
	if err := validateGoodsReceiptLineQuality(rec); err != nil {
		t.Fatalf("expected no error when quantities net exactly, got: %v", err)
	}
}

// TestValidateGoodsReceiptLineQuality_ZeroZeroWithBothSet_IsNoOp is the
// real, if unusual, 0/0 outcome (matching forecast.QualitySample's own
// HasData distinction): both fields explicitly present and zero,
// netting to a qty_received of zero.
func TestValidateGoodsReceiptLineQuality_ZeroZeroWithBothSet_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{
		"qty_received": float64(0), "qty_accepted": float64(0), "qty_rejected": float64(0),
	}}
	if err := validateGoodsReceiptLineQuality(rec); err != nil {
		t.Fatalf("expected no error for an explicit 0/0/0 record, got: %v", err)
	}
}

// TestValidateGoodsReceiptLineQuality_FloatRoundingWithinEpsilon_IsNoOp
// confirms qtyNetTolerance absorbs ordinary binary floating-point noise
// (0.1 + 0.2 != 0.3 in float64) without becoming a license to be off by
// a real unit — see qtyNetEpsilonAbs/qtyNetEpsilonRel's own doc comment.
func TestValidateGoodsReceiptLineQuality_FloatRoundingWithinEpsilon_IsNoOp(t *testing.T) {
	rec := data.Record{ID: "x", Data: map[string]any{
		"qty_received": float64(0.3), "qty_accepted": float64(0.1), "qty_rejected": float64(0.2),
	}}
	if err := validateGoodsReceiptLineQuality(rec); err != nil {
		t.Fatalf("expected qtyNetTolerance to absorb float64 rounding noise, got: %v", err)
	}
}

// TestQtyNetTolerance_ScalesWithMagnitude (independent review of
// uc-infra#82) pins the fix for a fixed absolute epsilon being too
// tight at scale: float64 has ~15-17 significant decimal digits, so the
// ABSOLUTE rounding noise on a large quantity is itself larger than a
// fixed 1e-9 — qtyNetTolerance must grow with qty_received's own
// magnitude rather than staying fixed.
func TestQtyNetTolerance_ScalesWithMagnitude(t *testing.T) {
	if got := qtyNetTolerance(0.3); got != qtyNetEpsilonAbs {
		t.Errorf("qtyNetTolerance(0.3) = %v, want the absolute floor %v", got, qtyNetEpsilonAbs)
	}
	if got := qtyNetTolerance(1e12); !approxEqualLedger(got, qtyNetEpsilonRel*1e12) {
		t.Errorf("qtyNetTolerance(1e12) = %v, want %v (relative, not the fixed absolute floor)", got, qtyNetEpsilonRel*1e12)
	}
	// Sign must not matter — a received value stored as negative
	// (shouldn't happen in practice, but this is a pure magnitude scale,
	// not a business validity check) scales the same as its absolute value.
	if got := qtyNetTolerance(-1e12); !approxEqualLedger(got, qtyNetEpsilonRel*1e12) {
		t.Errorf("qtyNetTolerance(-1e12) = %v, want %v", got, qtyNetEpsilonRel*1e12)
	}
}

func approxEqualLedger(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// TestValidateGoodsReceiptLineQuality_LargeQuantityRealMismatch_StillRejected
// is the negative control for the relative-tolerance fix above: scaling
// the allowance with magnitude must not turn into "anything goes" for a
// large quantity — a genuine, real-unit-sized mismatch is still rejected.
func TestValidateGoodsReceiptLineQuality_LargeQuantityRealMismatch_StillRejected(t *testing.T) {
	// At qty_received = 1e9, qtyNetTolerance is qtyNetEpsilonRel*1e9 = 1
	// (a full unit) — a mismatch of 100 is unambiguously outside that,
	// not a boundary case the relative scaling could accidentally absorb.
	rec := data.Record{ID: "x", Data: map[string]any{
		"qty_received": float64(1e9), "qty_accepted": float64(5e8), "qty_rejected": float64(5e8 - 100),
	}}
	err := validateGoodsReceiptLineQuality(rec)
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Fatalf("expected crud.ErrHookRejected for a real 100-unit mismatch at scale, got: %v", err)
	}
}

// TestPostGoodsReceiptLineToLedger_QualityMismatch_RollsBackTheWholeWrite
// is the end-to-end regression: a quality-invalid GoodsReceiptLine must
// never reach the database at all — not the record, not a ledger
// posting — same "hook rejection rolls back everything" contract
// TestPostGoodsReceiptLineToLedger_UnknownAccountCode_RollsBackTheLine
// already pins for the ledger-posting half of this same hook.
func TestPostGoodsReceiptLineToLedger_QualityMismatch_RollsBackTheWholeWrite(t *testing.T) {
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

	_, err = fx.engine.Create(ctx, defFor(t, fx.tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": fx.poLineID, "item_id": fx.itemID,
		"qty_received": float64(4), "qty_accepted": float64(1), "qty_rejected": float64(1), // 1+1 != 4
	}, actor)
	if !errors.Is(err, crud.ErrHookRejected) {
		t.Fatalf("expected crud.ErrHookRejected, got: %v", err)
	}

	records := data.NewRecordRepo(fx.tenantDB)
	list, err := records.List(ctx, "GoodsReceiptLine")
	if err != nil {
		t.Fatalf("List GoodsReceiptLine: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected the rejected GoodsReceiptLine write to be rolled back entirely, found %d", len(list))
	}
	entries := data.NewJournalEntryRepo(fx.tenantDB)
	entryList, err := entries.List(ctx)
	if err != nil {
		t.Fatalf("List journal entries: %v", err)
	}
	if len(entryList) != 0 {
		t.Fatalf("expected no journal entry to be posted for a rejected line, got %d", len(entryList))
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
// the same-facility record the create path refuses.
// Reverting the hook to a create-only gate fails this test with the
// stored record holding from == to.
//
// qty stays valid (10) throughout: qty gained a Min:0 bound of its own
// (uc-infra#80), enforced by entity.ValidateRecord — which crud.Engine.
// Update runs BEFORE any hook (crud.go:290) — so a negative qty here
// would be rejected at that earlier layer instead, with a plain
// validation error rather than one wrapping crud.ErrHookRejected. This
// test's job is specifically the hook's same-facility check surviving an
// UPDATE, not a re-test of the bound; TestValidateStockTransfer_
// NegativeQty_Rejected below already covers the hook's own qty<=0 logic
// directly, and TestValidateRecord_MinMax (internal/kernel/entity) covers
// the Min:0 bound itself.
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
		// A different (but still valid, Min:0-satisfying) qty than the
		// create above, so the rollback assertion on qty below still
		// distinguishes "rolled back" from "coincidentally the same".
		"qty": float64(20), "transfer_date": "2026-08-01", "status_id": fx.draftStatusID,
	}, &version, humanActor())
	if err == nil {
		t.Fatal("expected an update to from == to to be rejected, not silently stored")
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
	// %v, not a fixed 2dp format (independent review, uc-infra#193: a
	// hardcoded %.2f here would truncate a >2dp currency's real total —
	// see vendorInvoiceMatchDetail's own comment on its reason string),
	// so a whole-number amount like 999.00 prints as "999", not "999.00".
	want := fmt.Sprintf("total 999 (99900 minor units) does not match received value 125 (12500 minor units) for PurchaseOrder %s", fx.poID)
	if reason != want {
		t.Fatalf("expected match_exception_reason %q, got %q", want, reason)
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

// TestMatchVendorInvoiceOnUpdate_CurrencyMismatch_RedirectsToMatchException
// (uc-infra#193) confirms the new currency_id leg of the 3-way match:
// an invoice whose currency_id explicitly names a DIFFERENT currency
// than the PurchaseOrder it's billing against redirects to
// match_exception even though the plain numeric totals agree exactly —
// same "a value-only check isn't enough" shape as the existing
// wrong-vendor test above, for the field this task adds a check for.
// Before this task, nothing compared these two fields at all: this is a
// genuinely new check, not a rescale of an existing one.
func TestMatchVendorInvoiceOnUpdate_CurrencyMismatch_RedirectsToMatchException(t *testing.T) {
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

	usd, err := engine.Create(ctx, defFor(t, tenantDB, "Currency"), map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": float64(2),
	}, actor)
	if err != nil {
		t.Fatalf("create USD Currency: %v", err)
	}
	eur, err := engine.Create(ctx, defFor(t, tenantDB, "Currency"), map[string]any{
		"code": "EUR", "name": "Euro", "minor_unit": float64(2),
	}, actor)
	if err != nil {
		t.Fatalf("create EUR Currency: %v", err)
	}

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
	draftPOStatusID := statusIDByCode(t, engine, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, defFor(t, tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftPOStatusID, "currency_id": usd.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	line, err := engine.Create(ctx, defFor(t, tenantDB, "POLine"), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 10.0, "unit_price": ledger.ToMinorUnits(12.50),
	}, actor)
	if err != nil {
		t.Fatalf("create POLine: %v", err)
	}
	gr, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-01-10", "facility_id": facility.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": line.ID, "item_id": item.ID, "qty_received": 10.0,
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	draftInvStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "draft")
	inv, err := engine.Create(ctx, defFor(t, tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
		"invoice_date": "2026-01-15", "status_id": draftInvStatusID, "total": 125.00, // agrees numerically
		"currency_id": eur.ID, // but disagrees with the PO's own USD
	}, actor)
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}

	engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	matchedStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "match_exception")
	version := inv.Version
	if _, err := engine.Update(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00, "currency_id": eur.ID,
	}, &version, actor); err != nil {
		t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
	}
	got, err := engine.Get(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != exceptionStatusID {
		t.Fatalf("expected the currency mismatch to land in match_exception despite matching totals, got status_id=%v", got.Data["status_id"])
	}
	reason, _ := got.Data["match_exception_reason"].(string)
	if !strings.Contains(reason, eur.ID) || !strings.Contains(reason, usd.ID) {
		t.Fatalf("expected match_exception_reason to name both currency_ids (invoice %q, PO %q), got %q", eur.ID, usd.ID, reason)
	}
}

// TestVendorInvoiceMatch_NonDefaultCurrencyScale_ResolvesRealMinorUnits
// (uc-infra#193) is the direct regression test for the fix's core claim:
// resolving minorUnit from the PurchaseOrder's real currency_id, not the
// package's fixed money.Decimals=2. Uses a 0-decimal-place currency
// (JPY-style, same fixture shape as assets'
// TestCurrencyMinorUnit_ResolvesNonDefaultScale) so the two scales are
// unmistakably different (0 vs 2) rather than coincidentally producing
// the same minor-unit numbers.
//
// The mismatch case is the part that actually distinguishes old
// behavior from new: before this fix, receivedValueForPurchaseOrder
// summed POLine.unit_price's raw stored minor-unit integers directly
// (always effectively 2dp, since money.Money is fixed at that scale),
// and vendorInvoiceMatchDetail's own total conversion was hardcoded to
// money.Decimals too — so a mismatch here would have been reported as
// "13000 minor units" vs "12000 minor units" (both scaled ×100 too
// large for a real 0dp currency). After this fix, both sides resolve
// and convert at the PO's real 0dp scale: "130 minor units" vs "120
// minor units". Asserting on the SMALL numbers is what actually catches
// a regression back to the old fixed-2dp conversion — asserting only on
// the matched/match_exception verdict would not, since that verdict is
// scale-invariant when both sides are (mis)scaled identically, which is
// exactly what made the old code's bug easy to miss.
func TestVendorInvoiceMatch_NonDefaultCurrencyScale_ResolvesRealMinorUnits(t *testing.T) {
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

	jpy, err := engine.Create(ctx, defFor(t, tenantDB, "Currency"), map[string]any{
		"code": "JPY", "name": "Japanese Yen", "minor_unit": float64(0),
	}, actor)
	if err != nil {
		t.Fatalf("create JPY Currency: %v", err)
	}
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
	draftPOStatusID := statusIDByCode(t, engine, tenantDB, "purchase_order_status", "draft")
	po, err := engine.Create(ctx, defFor(t, tenantDB, "PurchaseOrder"), map[string]any{
		"po_number": "PO-1", "vendor_id": vendor.ID, "order_date": "2026-01-01",
		"status_id": draftPOStatusID, "currency_id": jpy.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	// unit_price is entered as a human-typed major-unit amount and
	// stored via ledger.ToMinorUnits, same convention every other
	// fixture in this file uses — qty 10 x 12.00 = a real received value
	// of 120.00 (major units), regardless of which currency it's in.
	line, err := engine.Create(ctx, defFor(t, tenantDB, "POLine"), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 10.0, "unit_price": ledger.ToMinorUnits(12.00),
	}, actor)
	if err != nil {
		t.Fatalf("create POLine: %v", err)
	}
	gr, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceipt"), map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-01-10", "facility_id": facility.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := engine.Create(ctx, defFor(t, tenantDB, "GoodsReceiptLine"), map[string]any{
		"goods_receipt_id": gr.ID, "po_line_id": line.ID, "item_id": item.ID, "qty_received": 10.0,
	}, actor); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	draftInvStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "draft")
	matchedStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "matched")
	exceptionStatusID := statusIDByCode(t, engine, tenantDB, "vendor_invoice_status", "match_exception")

	t.Run("agrees at the real 0dp scale", func(t *testing.T) {
		inv, err := engine.Create(ctx, defFor(t, tenantDB, "VendorInvoice"), map[string]any{
			"invoice_number": "VINV-OK", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
			"invoice_date": "2026-01-15", "status_id": draftInvStatusID, "total": 120.00, "currency_id": jpy.ID,
		}, actor)
		if err != nil {
			t.Fatalf("create VendorInvoice: %v", err)
		}
		version := inv.Version
		if _, err := engine.Update(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID, map[string]any{
			"invoice_number": "VINV-OK", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
			"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 120.00, "currency_id": jpy.ID,
		}, &version, actor); err != nil {
			t.Fatalf("expected the transition to succeed, got: %v", err)
		}
		got, err := engine.Get(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID)
		if err != nil {
			t.Fatalf("get VendorInvoice: %v", err)
		}
		if got.Data["status_id"] != matchedStatusID {
			t.Fatalf("expected a genuinely-agreeing 0dp-currency invoice to match, got status_id=%v", got.Data["status_id"])
		}
	})

	t.Run("mismatch reports the real 0dp minor-unit counts, not the 2dp-inflated ones", func(t *testing.T) {
		inv, err := engine.Create(ctx, defFor(t, tenantDB, "VendorInvoice"), map[string]any{
			"invoice_number": "VINV-BAD", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
			"invoice_date": "2026-01-15", "status_id": draftInvStatusID, "total": 130.00, "currency_id": jpy.ID,
		}, actor)
		if err != nil {
			t.Fatalf("create VendorInvoice: %v", err)
		}
		version := inv.Version
		if _, err := engine.Update(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID, map[string]any{
			"invoice_number": "VINV-BAD", "purchase_order_id": po.ID, "vendor_id": vendor.ID,
			"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 130.00, "currency_id": jpy.ID,
		}, &version, actor); err != nil {
			t.Fatalf("expected the redirect to succeed (no rollback), got: %v", err)
		}
		got, err := engine.Get(ctx, defFor(t, tenantDB, "VendorInvoice"), inv.ID)
		if err != nil {
			t.Fatalf("get VendorInvoice: %v", err)
		}
		if got.Data["status_id"] != exceptionStatusID {
			t.Fatalf("expected the real mismatch to land in match_exception, got status_id=%v", got.Data["status_id"])
		}
		reason, _ := got.Data["match_exception_reason"].(string)
		// Full expected string, not just the two "N minor units" substrings
		// (independent review, uc-infra#193's own first draft: a narrow
		// substring check like the old one here didn't catch that the
		// major-unit figures were wrong — money.Money(receivedMinor).Major()
		// misreported "120 minor units" as received value "1.20" instead
		// of "120", since it unconditionally divides by money's fixed 2dp
		// scale rather than the resolved 0dp one. Asserting the FULL
		// string is what actually catches that class of bug.)
		want := fmt.Sprintf("total 130 (130 minor units) does not match received value 120 (120 minor units) for PurchaseOrder %s", po.ID)
		if reason != want {
			t.Fatalf("expected match_exception_reason %q, got %q — a 2dp-inflated 13000/12000 or a wrong "+
				"major-unit figure here would mean minorUnit resolution or its display regressed", want, reason)
		}
	})
}

// TestVendorInvoiceMatch_UnresolvableCurrencyID_FallsBackToDefaultScale
// (uc-infra#193) confirms the "unresolvable currency degrades to
// money.Decimals, not a hard failure" fallback — same posture
// assets.PostDueDepreciation already established for the identical
// situation — using a currency_id that doesn't resolve to any real
// Currency row (a dangling reference, tolerated per ADR-0007) rather
// than an empty one, so this exercises the currencyMinorUnitTx error
// path specifically, not just the "no currency_id at all" path every
// other test in this file already covers.
func TestVendorInvoiceMatch_UnresolvableCurrencyID_FallsBackToDefaultScale(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00 at the default 2dp scale
	ctx := context.Background()

	// Give the PurchaseOrder a currency_id that doesn't resolve to any
	// real Currency row.
	poDef := defFor(t, fx.tenantDB, "PurchaseOrder")
	po, err := fx.engine.Get(ctx, poDef, fx.poID)
	if err != nil {
		t.Fatalf("get PurchaseOrder: %v", err)
	}
	poVersion := po.Version
	if _, err := fx.engine.Update(ctx, poDef, fx.poID, map[string]any{
		"po_number": "PO-1", "vendor_id": fx.vendorID, "order_date": "2026-01-01",
		"status_id": po.Data["status_id"], "currency_id": "00000000-0000-0000-0000-000000000000",
	}, &poVersion, humanActor()); err != nil {
		t.Fatalf("update PurchaseOrder currency_id: %v", err)
	}

	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00) // agrees at the 2dp fallback scale

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the transition to succeed despite the unresolvable currency_id, got: %v", err)
	}
	got, err := fx.engine.Get(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != matchedStatusID {
		t.Fatalf("expected an unresolvable PO currency_id to fall back to the 2dp default (not fail the match), got status_id=%v", got.Data["status_id"])
	}
}

// TestMatchVendorInvoiceOnUpdate_OutOfRangeCurrencyMinorUnit_FallsBackToDefaultScale
// (uc-infra#193, independent review) confirms currencyMinorUnitTx's
// range check: a Currency row whose minor_unit is OUTSIDE
// [0, money.MaxMinorUnitScale] — reachable only via a legacy row written
// before Currency's own [0,6] bound landed (Version 3, uc-infra#80), not
// through the ordinary entity.ValidateRecord-guarded Create path, so
// this writes it directly via data.RecordRepo, bypassing entity
// validation the same way this file's own
// TestPostGoodsReceiptLineToLedger_DuplicateLegacyRows_ReturnsDiagnosableError
// simulates a legacy row — degrades to the money.Decimals fallback, the
// same as an unresolvable/missing currency_id, rather than hard-failing
// the whole match via money.FromMajorUnits's own range check.
func TestMatchVendorInvoiceOnUpdate_OutOfRangeCurrencyMinorUnit_FallsBackToDefaultScale(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00 at the default 2dp scale
	ctx := context.Background()

	records := data.NewRecordRepo(fx.tenantDB)
	corrupt, err := records.Create(ctx, "Currency", map[string]any{
		"code": "XXX", "name": "Corrupt Legacy Currency", "minor_unit": float64(9), // outside [0,6]
	})
	if err != nil {
		t.Fatalf("create corrupt Currency row: %v", err)
	}

	poDef := defFor(t, fx.tenantDB, "PurchaseOrder")
	po, err := fx.engine.Get(ctx, poDef, fx.poID)
	if err != nil {
		t.Fatalf("get PurchaseOrder: %v", err)
	}
	poVersion := po.Version
	if _, err := fx.engine.Update(ctx, poDef, fx.poID, map[string]any{
		"po_number": "PO-1", "vendor_id": fx.vendorID, "order_date": "2026-01-01",
		"status_id": po.Data["status_id"], "currency_id": corrupt.ID,
	}, &poVersion, humanActor()); err != nil {
		t.Fatalf("update PurchaseOrder currency_id: %v", err)
	}

	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	inv := createDraftVendorInvoice(t, fx, 125.00) // agrees at the 2dp fallback scale

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the transition to succeed despite the corrupt minor_unit (fallback, not a hard failure), got: %v", err)
	}
	got, err := fx.engine.Get(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != matchedStatusID {
		t.Fatalf("expected an out-of-range Currency.minor_unit to fall back to the 2dp default (not fail the match), got status_id=%v", got.Data["status_id"])
	}
}

// TestMatchVendorInvoiceOnUpdate_InvoiceCurrencySetPOCurrencyBlank_NotAMismatch
// (uc-infra#193, independent review) confirms the currency_id mismatch
// check only fires when BOTH sides assert a currency — unlike vendor_id
// (where an empty invoice vendor_id IS itself a mismatch), a
// PurchaseOrder written before uc-infra#193 (or any tenant that hasn't
// adopted per-document currencies) has a blank currency_id, and that
// blank isn't a claim the invoice's own currency_id can disagree with.
// The regression this guards: an early version of this check compared
// invoiceCurrencyID != poCurrencyID without also requiring poCurrencyID
// non-empty, which would have redirected every such existing PO/invoice
// pairing to match_exception on its very next save.
func TestMatchVendorInvoiceOnUpdate_InvoiceCurrencySetPOCurrencyBlank_NotAMismatch(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10) // received value 125.00; PO has no currency_id
	ctx := context.Background()

	usd, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "Currency"), map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": float64(2),
	}, humanActor())
	if err != nil {
		t.Fatalf("create USD Currency: %v", err)
	}

	fx.engine.SetHook("VendorInvoice", MatchVendorInvoiceOnUpdate)
	draftStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "draft")
	inv, err := fx.engine.Create(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": draftStatusID, "total": 125.00, "currency_id": usd.ID,
	}, humanActor())
	if err != nil {
		t.Fatalf("create VendorInvoice: %v", err)
	}

	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")
	version := inv.Version
	if _, err := fx.engine.Update(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID, map[string]any{
		"invoice_number": "VINV-1", "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"invoice_date": "2026-01-15", "status_id": matchedStatusID, "total": 125.00, "currency_id": usd.ID,
	}, &version, humanActor()); err != nil {
		t.Fatalf("expected the transition to succeed (no currency mismatch), got: %v", err)
	}
	got, err := fx.engine.Get(ctx, defFor(t, fx.tenantDB, "VendorInvoice"), inv.ID)
	if err != nil {
		t.Fatalf("get VendorInvoice: %v", err)
	}
	if got.Data["status_id"] != matchedStatusID {
		t.Fatalf("expected an invoice currency_id with no PO currency_id to set to match (not flagged as a mismatch), got status_id=%v", got.Data["status_id"])
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
	// "999", not "999.00" — %v formatting, not a fixed 2dp format
	// (independent review, uc-infra#193, see the MismatchedTotal test
	// above for why).
	if !strings.Contains(firstReason, "999") {
		t.Fatalf("expected the first reason to mention 999, got %q", firstReason)
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
	if !strings.Contains(secondReason, "777") {
		t.Fatalf("expected the second reason to mention the new total 777, got %q", secondReason)
	}
	if strings.Contains(secondReason, "999") {
		t.Fatalf("expected the reason to be replaced, not accumulated — still mentions the old 999: %q", secondReason)
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

// TestMatchVendorInvoiceOnUpdate_CorruptTotal_HardFails (uc-infra#163):
// unlike the old hardcoded-2dp ledger.ToMinorUnits (which never returned
// an error), money.FromMajorUnits validates its input and can reject a
// corrupt total (NaN/±Inf/overflow). That must fail the whole match
// hook (ErrVendorInvoiceMatchFailed, same as a missing purchase_order_id
// or a genuine DB error) rather than being treated as an ordinary
// business disagreement routed to match_exception — a NaN total isn't
// something a human corrects by re-entering a number in the same field
// and retrying the way a wrong total or wrong vendor is.
func TestMatchVendorInvoiceOnUpdate_CorruptTotal_HardFails(t *testing.T) {
	fx := setUpVendorInvoiceFixture(t, 10, 12.50, 10)
	matchedStatusID := statusIDByCode(t, fx.engine, fx.tenantDB, "vendor_invoice_status", "matched")

	tx, err := fx.tenantDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rec := data.Record{ID: "does-not-matter", Data: map[string]any{
		"status_id": matchedStatusID, "purchase_order_id": fx.poID, "vendor_id": fx.vendorID,
		"total": math.NaN(),
	}}
	err = MatchVendorInvoiceOnUpdate(context.Background(), tx, nil, rec, audit.ActionUpdate, humanActor())
	if !errors.Is(err, ErrVendorInvoiceMatchFailed) {
		t.Fatalf("expected ErrVendorInvoiceMatchFailed for a NaN total, got: %v", err)
	}
}
