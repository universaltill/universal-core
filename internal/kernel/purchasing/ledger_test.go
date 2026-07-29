package purchasing

import (
	"context"
	"database/sql"
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

	statusTypes, err := engine.ListByField(ctx, defFor(t, tenantDB, "StatusType"), "code", "purchase_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list purchase_order_status StatusType: %v", err)
	}
	statuses, err := engine.ListByField(ctx, defFor(t, tenantDB, "Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var draftStatusID string
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			draftStatusID = s.ID
		}
	}
	if draftStatusID == "" {
		t.Fatal("expected a draft Status")
	}

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
