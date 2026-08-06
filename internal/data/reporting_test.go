package data_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
)

// This package's own tests (unlike every other repo, which is only ever
// exercised indirectly through internal/kernel/crud or a module's own
// seed tests) talk to RecordRepo/ReportingRepo directly — the aggregate
// queries in reporting.go are entity-specific by design (see that file's
// own doc comment on why that's fine here, unlike the generic engines),
// so there's no kernel package positioned to test them instead.

// freshTenantDB returns a connection to a brand-new, uniquely-named
// database with the tenant migration set applied (ADR-0003) — the
// per-tenant database every test in this file now runs against, in
// place of the old single shared database + tenant_id column. Two calls
// in the same test give two physically separate databases, which is how
// the former "within tenant"/"ignores other tenants" tests below now
// prove isolation — a stronger proof than a WHERE clause, since there is
// no shared table for a bug to accidentally query across.
func freshTenantDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_reporting_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	tenantDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open tenant database %s: %v", name, err)
	}
	t.Cleanup(func() { tenantDB.Close() })
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("ping tenant database %s: %v", name, err)
	}
	if _, err := tenantDB.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyTenant(context.Background(), tenantDB); err != nil {
		t.Fatalf("ApplyTenant: %v", err)
	}
	return tenantDB
}

func TestPurchaseOrderStatusBreakdown_GroupsByStatusAndIsolatedPerTenantDatabase(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	dbB := freshTenantDB(t)
	recordsA := data.NewRecordRepo(dbA)
	recordsB := data.NewRecordRepo(dbB)
	reportingA := data.NewReportingRepo(dbA)

	mustCreateStatus := func(records *data.RecordRepo, code string) string {
		t.Helper()
		rec, err := records.Create(ctx, "Status", map[string]any{"code": code, "name": code})
		if err != nil {
			t.Fatalf("create Status: %v", err)
		}
		return rec.ID
	}
	draftA := mustCreateStatus(recordsA, "draft")
	approvedA := mustCreateStatus(recordsA, "approved")
	draftB := mustCreateStatus(recordsB, "draft")

	mustCreate := func(records *data.RecordRepo, fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, "PurchaseOrder", fields); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreate(recordsA, map[string]any{"po_number": "PO-A1", "status_id": draftA, "total": 100.0})
	mustCreate(recordsA, map[string]any{"po_number": "PO-A2", "status_id": draftA, "total": 50.0})
	mustCreate(recordsA, map[string]any{"po_number": "PO-A3", "status_id": approvedA, "total": 200.0})
	// A different tenant's order, in a genuinely different database, must
	// never contaminate tenantA's totals — even though "draft" resolves to
	// a different Status row per tenant and this order's own row id space
	// otherwise overlaps freely with tenantA's.
	mustCreate(recordsB, map[string]any{"po_number": "PO-B1", "status_id": draftB, "total": 999.0})
	// A status_id that isn't even a well-formed UUID (e.g. a bad CSV
	// import mapping) must be excluded, not abort the whole query — see
	// reporting.go's uuidPattern doc comment.
	mustCreate(recordsA, map[string]any{"po_number": "PO-A4", "status_id": "not-a-uuid", "total": 1234.0})

	rows, err := reportingA.PurchaseOrderStatusBreakdown(ctx)
	if err != nil {
		t.Fatalf("PurchaseOrderStatusBreakdown: %v", err)
	}
	byStatus := map[string]data.PurchaseOrderStatusCount{}
	for _, r := range rows {
		byStatus[r.Status] = r
	}
	if got := byStatus["draft"]; got.Count != 2 || got.Value != 150.0 {
		t.Errorf("draft = %+v, want Count=2 Value=150", got)
	}
	if got := byStatus["approved"]; got.Count != 1 || got.Value != 200.0 {
		t.Errorf("approved = %+v, want Count=1 Value=200", got)
	}
	if _, ok := byStatus["submitted"]; ok {
		t.Error("submitted should not appear — no orders in that status")
	}
}

func TestTopVendorsBySpend_RanksDescendingAndExcludesMalformedRefs(t *testing.T) {
	ctx := context.Background()
	dbA := freshTenantDB(t)
	records := data.NewRecordRepo(dbA)
	reporting := data.NewReportingRepo(dbA)

	bigVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Big Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	smallVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Small Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	mustCreatePO := func(vendorID string, total float64) {
		t.Helper()
		if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": "PO-" + vendorID, "vendor_id": vendorID, "total": total,
		}); err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
	}
	mustCreatePO(bigVendor.ID, 1000.0)
	mustCreatePO(bigVendor.ID, 500.0)
	mustCreatePO(smallVendor.ID, 10.0)
	// A vendor_id that isn't even a well-formed UUID (e.g. a bad CSV
	// import mapping) must be excluded, not abort the whole query — see
	// reporting.go's uuidPattern doc comment.
	mustCreatePO("not-a-uuid", 50000.0)
	// A well-formed but dangling vendor_id (no matching Party in this
	// database — deleted, or, under the old shared-DB design, belonging
	// to a different tenant) must not resolve either.
	mustCreatePO("00000000-0000-0000-0000-000000000000", 999.0)

	got, err := reporting.TopVendorsBySpend(ctx, 10)
	if err != nil {
		t.Fatalf("TopVendorsBySpend: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vendors, want 2 (malformed/dangling refs excluded): %+v", len(got), got)
	}
	if got[0].VendorName != "Big Vendor" || got[0].Total != 1500.0 || got[0].OrderCount != 2 {
		t.Errorf("rank 0 = %+v, want Big Vendor/1500/2", got[0])
	}
	if got[1].VendorName != "Small Vendor" || got[1].Total != 10.0 {
		t.Errorf("rank 1 = %+v, want Small Vendor/10", got[1])
	}
}

// TestCompletedPOLeadTimes_ReceivedAtWithGoodsReceiptFallback covers
// #30's query-time receipt derivation: a PO's own received_at wins when
// present, the EARLIEST GoodsReceipt.received_date fills in when it
// isn't, and a PO with neither (in-flight/draft) is excluded entirely.
// A PO whose vendor_id is malformed or dangling is still RETURNED —
// with an empty VendorID — because its lead time is real evidence for
// the overall distribution even when it can't be attributed to a
// vendor (see CompletedPOLeadTimes' own doc comment).
func TestCompletedPOLeadTimes_ReceivedAtWithGoodsReceiptFallback(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Lead Time Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	mustCreatePO := func(fields map[string]any) string {
		t.Helper()
		rec, err := records.Create(ctx, "PurchaseOrder", fields)
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		return rec.ID
	}
	mustCreateGR := func(poID, receivedDate string) {
		t.Helper()
		if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
			"purchase_order_id": poID, "received_date": receivedDate,
		}); err != nil {
			t.Fatalf("create GoodsReceipt: %v", err)
		}
	}

	// PO with its own received_at AND a later GoodsReceipt — the stored
	// stage must win over the fallback.
	stamped := mustCreatePO(map[string]any{
		"po_number": "PO-STAMPED", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "sourced_at": "2026-07-03", "received_at": "2026-07-10",
	})
	mustCreateGR(stamped, "2026-07-12")

	// PO with NO received_at but two GoodsReceipts (partial deliveries) —
	// the EARLIEST received_date is the fallback receipt time.
	fallback := mustCreatePO(map[string]any{
		"po_number": "PO-FALLBACK", "vendor_id": vendor.ID, "order_date": "2026-07-05",
	})
	mustCreateGR(fallback, "2026-07-20")
	mustCreateGR(fallback, "2026-07-17")

	// In-flight PO: neither received_at nor any GoodsReceipt — excluded.
	mustCreatePO(map[string]any{
		"po_number": "PO-OPEN", "vendor_id": vendor.ID,
		"order_date": "2026-07-08", "sourced_at": "2026-07-09",
	})
	// Malformed vendor_id — still a completed PO, returned with an empty
	// VendorID (counts toward overall stats), not a query-aborting cast
	// error and not silently dropped.
	badVendor := mustCreatePO(map[string]any{
		"po_number": "PO-BADVENDOR", "vendor_id": "not-a-uuid",
		"order_date": "2026-07-02", "received_at": "2026-07-09",
	})
	// A GoodsReceipt whose purchase_order_id is malformed must neither
	// error the query nor attach anywhere.
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": "not-a-uuid", "received_date": "2026-07-01",
	}); err != nil {
		t.Fatalf("create malformed GoodsReceipt: %v", err)
	}

	got, err := reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		t.Fatalf("CompletedPOLeadTimes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d completed POs, want 3 (incl. the vendorless one): %+v", len(got), got)
	}
	// Ordered by order_date: PO-STAMPED (07-01), PO-BADVENDOR (07-02),
	// PO-FALLBACK (07-05).
	if got[0].POID != stamped || got[0].ReceivedDate != "2026-07-10" {
		t.Errorf("row 0 = %+v, want PO-STAMPED with its own received_at 2026-07-10", got[0])
	}
	if got[0].VendorID != vendor.ID || got[0].VendorName != "Lead Time Vendor" {
		t.Errorf("row 0 vendor = %s/%s, want the seeded vendor", got[0].VendorID, got[0].VendorName)
	}
	if got[0].OrderDate != "2026-07-01" || got[0].SourcedAt != "2026-07-03" {
		t.Errorf("row 0 dates = order %q sourced %q, want 2026-07-01/2026-07-03", got[0].OrderDate, got[0].SourcedAt)
	}
	if got[0].ShippedAt != "" {
		t.Errorf("row 0 shipped_at = %q, want empty for an unrecorded stage", got[0].ShippedAt)
	}
	if got[1].POID != badVendor || got[1].VendorID != "" || got[1].VendorName != "" {
		t.Errorf("row 1 = %+v, want PO-BADVENDOR with empty vendor id/name", got[1])
	}
	if got[1].ReceivedDate != "2026-07-09" {
		t.Errorf("row 1 received = %q, want 2026-07-09", got[1].ReceivedDate)
	}
	if got[2].POID != fallback || got[2].ReceivedDate != "2026-07-17" {
		t.Errorf("row 2 = %+v, want PO-FALLBACK with earliest GoodsReceipt date 2026-07-17", got[2])
	}
	// LastReceivedDate (#11) diverges from ReceivedDate exactly here: two
	// partial GoodsReceipts (07-17 and 07-20) means the LATEST one, not
	// the earliest, is when the order was actually fully satisfied.
	if got[2].LastReceivedDate != "2026-07-20" {
		t.Errorf("row 2 LastReceivedDate = %q, want the LATEST GoodsReceipt date 2026-07-20 (not the earliest)", got[2].LastReceivedDate)
	}
	// Rows with a stamped received_at (no partial-receipt ambiguity)
	// carry the same value in both fields — they only diverge across
	// multiple partial GoodsReceipt rows.
	if got[0].LastReceivedDate != got[0].ReceivedDate {
		t.Errorf("row 0 LastReceivedDate = %q, want it to equal ReceivedDate %q (single stamped receipt)", got[0].LastReceivedDate, got[0].ReceivedDate)
	}
}

// TestCompletedPOLeadTimes_PromisedDeliveryDate (#11) pins the
// pass-through of the new optional field: present when set, empty
// (never null-panics the scan) when the PO predates #11 or simply never
// had one pinned down.
func TestCompletedPOLeadTimes_PromisedDeliveryDate(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Promise Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}

	promised, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-PROMISED", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "promised_delivery_date": "2026-07-10", "received_at": "2026-07-09",
	})
	if err != nil {
		t.Fatalf("create PO with promised_delivery_date: %v", err)
	}
	unpromised, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-UNPROMISED", "vendor_id": vendor.ID,
		"order_date": "2026-07-02", "received_at": "2026-07-09",
	})
	if err != nil {
		t.Fatalf("create PO without promised_delivery_date: %v", err)
	}

	got, err := reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		t.Fatalf("CompletedPOLeadTimes: %v", err)
	}
	byID := make(map[string]data.CompletedPOLeadTime, len(got))
	for _, row := range got {
		byID[row.POID] = row
	}
	if got := byID[promised.ID].PromisedDeliveryDate; got != "2026-07-10" {
		t.Errorf("PO-PROMISED.PromisedDeliveryDate = %q, want 2026-07-10", got)
	}
	if got := byID[unpromised.ID].PromisedDeliveryDate; got != "" {
		t.Errorf("PO-UNPROMISED.PromisedDeliveryDate = %q, want empty", got)
	}
}

// TestCompletedPOLeadTimes_PartialReceiptLastReceivedDate (#11,
// independent review 2026-08-01) is the exact scenario that motivated
// LastReceivedDate: a PO promised for 2026-07-10, with a 1-unit partial
// GoodsReceipt one day EARLY (07-09) followed by the bulk of the order
// arriving 82 days LATE (09-30). ReceivedDate (MIN, #30's lead-time
// semantics) would say 07-09 — on time. LastReceivedDate (MAX, #11's
// on-time semantics) must say 09-30 — the actual completion date, and
// the one an on-time judgement has to be measured against.
func TestCompletedPOLeadTimes_PartialReceiptLastReceivedDate(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Partial Ship Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-PARTIAL", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "promised_delivery_date": "2026-07-10",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-09",
	}); err != nil {
		t.Fatalf("create early partial GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-09-30",
	}); err != nil {
		t.Fatalf("create late completing GoodsReceipt: %v", err)
	}

	got, err := reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		t.Fatalf("CompletedPOLeadTimes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ReceivedDate != "2026-07-09" {
		t.Errorf("ReceivedDate (first-arrival, #30 lead-time semantics) = %q, want the EARLY partial 2026-07-09", got[0].ReceivedDate)
	}
	if got[0].LastReceivedDate != "2026-09-30" {
		t.Errorf("LastReceivedDate (full-completion, #11 on-time semantics) = %q, want the LATE completing receipt 2026-09-30, not the early partial", got[0].LastReceivedDate)
	}
}

// TestOnOrderQtyByItem_SumsOpenStatusesOnly pins R3's on-order
// definition: submitted + approved POs count, drafts and terminal
// statuses (received, cancelled) don't, and quantities sum across lines
// and orders per item.
func TestOnOrderQtyByItem_SumsOpenStatusesOnly(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	statusIDs := map[string]string{}
	for _, code := range []string{"draft", "submitted", "approved", "received", "cancelled"} {
		rec, err := records.Create(ctx, "Status", map[string]any{"code": code, "name": code})
		if err != nil {
			t.Fatalf("create Status: %v", err)
		}
		statusIDs[code] = rec.ID
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OO", "name": "On Order Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	otherItem, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-NONE", "name": "Never Ordered", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreatePOWithLine := func(poNumber, status, itemID string, qty float64) {
		t.Helper()
		po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": poNumber, "order_date": "2026-07-01", "status_id": statusIDs[status],
		})
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		if _, err := records.Create(ctx, "POLine", map[string]any{
			"purchase_order_id": po.ID, "item_id": itemID, "qty": qty, "unit_price": 1.0,
		}); err != nil {
			t.Fatalf("create POLine: %v", err)
		}
	}
	mustCreatePOWithLine("PO-SUB", "submitted", item.ID, 100)
	mustCreatePOWithLine("PO-APP", "approved", item.ID, 40)
	mustCreatePOWithLine("PO-DRAFT", "draft", item.ID, 999)
	mustCreatePOWithLine("PO-RCVD", "received", item.ID, 999)
	mustCreatePOWithLine("PO-CANC", "cancelled", item.ID, 999)
	// Malformed refs are excluded, not fatal.
	if _, err := records.Create(ctx, "POLine", map[string]any{
		"purchase_order_id": "not-a-uuid", "item_id": item.ID, "qty": 999.0, "unit_price": 1.0,
	}); err != nil {
		t.Fatalf("create malformed POLine: %v", err)
	}

	got, err := reporting.OnOrderQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnOrderQtyByItem: %v", err)
	}
	if qty := got[item.ID]; qty != 140 {
		t.Errorf("on-order for item = %v, want 140 (submitted 100 + approved 40 only)", qty)
	}
	if _, ok := got[otherItem.ID]; ok {
		t.Errorf("item with no open PO must have no entry, got %v", got[otherItem.ID])
	}
}

// TestOnOrderQtyByItem_NetsAgainstReceivedQty (uc-infra#54) pins the
// per-line netting this function gained in the same commit as
// purchasing.PostGoodsReceiptLineToLedger's InventoryItem-crediting
// wiring: the moment receiving credits qty_on_hand, on-order must stop
// counting the delivered portion too, or on_hand + on_order double-counts
// the same units. Uses data.NewRecordRepo directly (bypassing
// entity.ValidateRecord, same as every other test in this file) so a
// GoodsReceipt fixture here does not need a facility_id — this function
// only ever reads GoodsReceiptLine.po_line_id/qty_received, never
// GoodsReceipt itself.
func TestOnOrderQtyByItem_NetsAgainstReceivedQty(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	statusIDs := map[string]string{}
	for _, code := range []string{"submitted", "approved"} {
		rec, err := records.Create(ctx, "Status", map[string]any{"code": code, "name": code})
		if err != nil {
			t.Fatalf("create Status: %v", err)
		}
		statusIDs[code] = rec.ID
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-NET", "name": "Netted Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	overItem, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OVER", "name": "Over-Received Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreatePOWithLine := func(poNumber, status, itemID string, qty float64) (poID, lineID string) {
		t.Helper()
		po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": poNumber, "order_date": "2026-07-01", "status_id": statusIDs[status],
		})
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		line, err := records.Create(ctx, "POLine", map[string]any{
			"purchase_order_id": po.ID, "item_id": itemID, "qty": qty, "unit_price": 1.0,
		})
		if err != nil {
			t.Fatalf("create POLine: %v", err)
		}
		return po.ID, line.ID
	}
	mustReceive := func(poID, lineID, itemID string, qtyReceived float64) {
		t.Helper()
		gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
			"purchase_order_id": poID, "received_date": "2026-07-05",
		})
		if err != nil {
			t.Fatalf("create GoodsReceipt: %v", err)
		}
		if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
			"goods_receipt_id": gr.ID, "po_line_id": lineID, "item_id": itemID, "qty_received": qtyReceived,
		}); err != nil {
			t.Fatalf("create GoodsReceiptLine: %v", err)
		}
	}

	// Partially received: ordered 100, received 30 in two GoodsReceiptLines
	// (20 + 10) against the same line — remaining must be 70, netted
	// against the SUM of both receipts, not just the last one.
	partialPO, partialLine := mustCreatePOWithLine("PO-PARTIAL", "submitted", item.ID, 100)
	mustReceive(partialPO, partialLine, item.ID, 20)
	mustReceive(partialPO, partialLine, item.ID, 10)

	// Fully received but still "approved" (not yet transitioned to
	// "received") — remaining must be 0, not negative, and must not
	// itself go missing from the map's arithmetic (it's summed into the
	// same item as the partial line above: 70 + 0 = 70).
	fullPO, fullLine := mustCreatePOWithLine("PO-FULL", "approved", item.ID, 50)
	mustReceive(fullPO, fullLine, item.ID, 50)

	// A second, unrelated submitted line for the same item with nothing
	// received yet — remaining is its full ordered qty, unaffected by
	// netting.
	mustCreatePOWithLine("PO-OPEN", "submitted", item.ID, 25)

	// Over-received (data inconsistency, e.g. a correction entered
	// twice): received MORE than ordered. Must floor at 0 for THIS LINE,
	// not go negative.
	overPO, overLine := mustCreatePOWithLine("PO-OVERRECEIVED", "submitted", overItem.ID, 10)
	mustReceive(overPO, overLine, overItem.ID, 15)

	// A second, genuinely under-received line for the SAME item
	// (independent review, uc-infra#54: the single over-received line
	// above cannot actually distinguish per-line flooring from
	// per-item flooring — flooring a lone negative-or-zero value gives
	// the same answer either way). With both lines present:
	//   - correct (floor PER LINE, then sum): max(10-15,0) + max(100-0,0)
	//     = 0 + 100 = 100.
	//   - the bug this guards against (sum raw remainders, THEN floor
	//     the item total): max((10-15)+(100-0), 0) = max(95,0) = 95.
	// The two disagree (100 vs 95), so this line is what actually pins
	// the per-line semantics — a regression to item-level flooring
	// would silently let the over-received line's negative remainder
	// mask 5 units of this genuinely still-open line.
	shortPO, shortLine := mustCreatePOWithLine("PO-UNDERRECEIVED", "submitted", overItem.ID, 100)

	// A negative qty_received (no Min concept exists yet, #80 — data
	// inconsistency, not a reachable normal path) must not REDUCE what
	// counts as received and inflate on-order past the ordered qty: the
	// per-line received total is floored at 0 before netting, same
	// discipline as the remaining-qty floor above.
	negPO, negLine := mustCreatePOWithLine("PO-NEGATIVE-RECEIPT", "submitted", item.ID, 5)
	mustReceive(negPO, negLine, item.ID, -3)

	// A GoodsReceiptLine with a malformed po_line_id must not abort the
	// query (uuidPattern guard) and must not attach to any line.
	strayGR, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": partialPO, "received_date": "2026-07-06",
	})
	if err != nil {
		t.Fatalf("create stray GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": strayGR.ID, "po_line_id": "not-a-uuid", "item_id": item.ID, "qty_received": 999.0,
	}); err != nil {
		t.Fatalf("create malformed GoodsReceiptLine: %v", err)
	}
	// A GoodsReceiptLine with a malformed goods_receipt_id (the OTHER
	// reference hop the netting subquery casts, uc-infra#54) must be
	// excluded the same way, not abort the query either.
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": "not-a-uuid", "po_line_id": partialLine, "item_id": item.ID, "qty_received": 999.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine with malformed goods_receipt_id: %v", err)
	}

	// A soft-deleted GoodsReceipt (crud.Engine.Delete does not cascade
	// to composition children, internal/kernel/crud/crud.go) must not
	// leave its still-live GoodsReceiptLine netting down on-order —
	// deleting the receipt "un-receives" it for this purpose. shortLine
	// (100 ordered, otherwise 0 received) is the target: if this
	// deleted receipt's 40 wrongly netted in, shortLine would read
	// 60 instead of 100.
	deletedGR, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": shortPO, "received_date": "2026-07-07",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt to be deleted: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": deletedGR.ID, "po_line_id": shortLine, "item_id": overItem.ID, "qty_received": 40.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine under the to-be-deleted GoodsReceipt: %v", err)
	}
	if err := records.Delete(ctx, "GoodsReceipt", deletedGR.ID); err != nil {
		t.Fatalf("soft-delete GoodsReceipt: %v", err)
	}

	got, err := reporting.OnOrderQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnOrderQtyByItem: %v", err)
	}
	if qty := got[item.ID]; qty != 100 {
		t.Errorf("on-order for netted item = %v, want 100 (70 partial-remaining + 0 fully-received + 25 untouched + 5 from the negative-receipt line, whose received qty must floor to 0, not -3)", qty)
	}
	if qty := got[overItem.ID]; qty != 100 {
		t.Errorf("on-order for the over/under-received item = %v, want 100 (0 floored-per-line + 100 under-received, NOT 95 — see the per-line-vs-per-item comment above; a soft-deleted receipt against the 100 line must not net in either)", qty)
	}
}

// TestOnHandQtyByItem sums qty_on_hand per item across every
// InventoryItem row, skipping rows with no item_id.
func TestOnHandQtyByItem(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	itemA, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OH-A", "name": "On Hand A", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	itemB, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OH-B", "name": "On Hand B", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	mustCreateInv := func(fields map[string]any) {
		t.Helper()
		if _, err := records.Create(ctx, "InventoryItem", fields); err != nil {
			t.Fatalf("create InventoryItem: %v", err)
		}
	}
	// Two rows for itemA — sums, matching StockSummary's treatment of
	// multiple rows per item.
	mustCreateInv(map[string]any{"item_id": itemA.ID, "qty_on_hand": 30, "qty_available_to_promise": 30})
	mustCreateInv(map[string]any{"item_id": itemA.ID, "qty_on_hand": 12, "qty_available_to_promise": 12})
	mustCreateInv(map[string]any{"item_id": itemB.ID, "qty_on_hand": 7, "qty_available_to_promise": 7})
	// No item_id — skipped, not an empty-string map key.
	mustCreateInv(map[string]any{"qty_on_hand": 99, "qty_available_to_promise": 99})

	got, err := reporting.OnHandQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnHandQtyByItem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(got), got)
	}
	if got[itemA.ID] != 42 {
		t.Errorf("itemA on-hand = %v, want 42 (30+12)", got[itemA.ID])
	}
	if got[itemB.ID] != 7 {
		t.Errorf("itemB on-hand = %v, want 7", got[itemB.ID])
	}
}

// TestLatestPOVendorByItem picks the vendor of the most recent PO per
// item, regardless of status — see the method's own doc comment.
func TestLatestPOVendorByItem(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	oldVendor, err := records.Create(ctx, "Party", map[string]any{"name": "Old Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	newVendor, err := records.Create(ctx, "Party", map[string]any{"name": "New Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-VND", "name": "Vendored Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreatePOWithLine := func(poNumber, vendorID, orderDate string) {
		t.Helper()
		po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
			"po_number": poNumber, "vendor_id": vendorID, "order_date": orderDate,
		})
		if err != nil {
			t.Fatalf("create PurchaseOrder: %v", err)
		}
		if _, err := records.Create(ctx, "POLine", map[string]any{
			"purchase_order_id": po.ID, "item_id": item.ID, "qty": 1.0, "unit_price": 1.0,
		}); err != nil {
			t.Fatalf("create POLine: %v", err)
		}
	}
	mustCreatePOWithLine("PO-OLD", oldVendor.ID, "2026-06-01")
	mustCreatePOWithLine("PO-NEW", newVendor.ID, "2026-07-15")

	got, err := reporting.LatestPOVendorByItem(ctx)
	if err != nil {
		t.Fatalf("LatestPOVendorByItem: %v", err)
	}
	if got[item.ID] != newVendor.ID {
		t.Errorf("latest vendor = %s, want the 2026-07-15 order's vendor %s", got[item.ID], newVendor.ID)
	}
}

func TestStockSummaryAndStockoutRiskItems(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	healthy, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-OK", "name": "Healthy Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	lowStock, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-LOW", "name": "Low Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	worseStock, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-WORSE", "name": "Worse Item", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}

	mustCreateInv := func(itemID string, onHand, atp float64) {
		t.Helper()
		if _, err := records.Create(ctx, "InventoryItem", map[string]any{
			"item_id": itemID, "qty_on_hand": onHand, "qty_available_to_promise": atp,
		}); err != nil {
			t.Fatalf("create InventoryItem: %v", err)
		}
	}
	mustCreateInv(healthy.ID, 100, 100)
	mustCreateInv(lowStock.ID, 10, 0)
	mustCreateInv(worseStock.ID, 5, -20)
	// A malformed item_id must be excluded from the stockout list, not
	// error the whole query.
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": "not-a-uuid", "qty_on_hand": 0, "qty_available_to_promise": -5,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	summary, err := reporting.StockSummary(ctx)
	if err != nil {
		t.Fatalf("StockSummary: %v", err)
	}
	if summary.ItemCount != 4 {
		t.Errorf("ItemCount = %d, want 4 (includes the malformed-ref row — it's still a real InventoryItem row)", summary.ItemCount)
	}
	if summary.TotalOnHand != 115 {
		t.Errorf("TotalOnHand = %v, want 115", summary.TotalOnHand)
	}
	if summary.StockoutCount != 3 {
		t.Errorf("StockoutCount = %d, want 3 (low, worse, and the malformed-ref row all have ATP <= 0)", summary.StockoutCount)
	}

	risk, err := reporting.StockoutRiskItems(ctx, 10)
	if err != nil {
		t.Fatalf("StockoutRiskItems: %v", err)
	}
	// Only 2 rows: the malformed item_id can't join to a real Item, so
	// it's excluded here even though StockSummary still counted it.
	if len(risk) != 2 {
		t.Fatalf("got %d stockout rows, want 2: %+v", len(risk), risk)
	}
	if risk[0].SKU != "SKU-WORSE" || risk[0].QtyATP != -20 {
		t.Errorf("rank 0 = %+v, want SKU-WORSE at -20 (worst first)", risk[0])
	}
	if risk[1].SKU != "SKU-LOW" || risk[1].QtyATP != 0 {
		t.Errorf("rank 1 = %+v, want SKU-LOW at 0", risk[1])
	}
}

// Since #12/ADR-0015 re-keyed InventoryItem by (item, facility), one
// item can hold several rows — and the stock reports are organization
// level, so they must aggregate to the item before counting.
//
// This test exists because the pre-existing fixtures could not catch a
// regression here: they give every item exactly one InventoryItem row,
// so per-row and per-item aggregation produce identical numbers and
// both the correct and the broken query pass. Every assertion below is
// chosen so that counting ROWS gives a different answer from counting
// ITEMS.
func TestStockReports_AggregateAcrossFacilities(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	mkItem := func(sku, name string) string {
		t.Helper()
		rec, err := records.Create(ctx, "Item", map[string]any{"sku": sku, "name": name, "item_type": "stock"})
		if err != nil {
			t.Fatalf("create Item %s: %v", sku, err)
		}
		return rec.ID
	}
	mkFacility := func(code string) string {
		t.Helper()
		rec, err := records.Create(ctx, "Facility", map[string]any{
			"code": code, "name": code, "facility_type": "warehouse", "is_active": true,
		})
		if err != nil {
			t.Fatalf("create Facility %s: %v", code, err)
		}
		return rec.ID
	}
	mkInv := func(itemID, facilityID string, onHand, atp float64) {
		t.Helper()
		if _, err := records.Create(ctx, "InventoryItem", map[string]any{
			"item_id": itemID, "facility_id": facilityID,
			"qty_on_hand": onHand, "qty_available_to_promise": atp,
		}); err != nil {
			t.Fatalf("create InventoryItem: %v", err)
		}
	}

	main := mkFacility("MAIN")
	store := mkFacility("STORE")

	// The discriminating case: empty in the store, plentiful in the
	// warehouse. Per row this looks like a stockout; per item it is
	// nothing of the kind.
	split := mkItem("SKU-SPLIT", "Split Across Facilities")
	mkInv(split, main, 100, 100)
	mkInv(split, store, 0, 0)

	// Genuinely exhausted everywhere — two rows, both empty. Must be
	// reported once, not twice.
	gone := mkItem("SKU-GONE", "Exhausted Everywhere")
	mkInv(gone, main, 0, 0)
	mkInv(gone, store, 0, -5)

	// Net negative only when summed: positive in one place, more
	// negative in another. Per row, neither is the worst; per item it
	// is the worst at -10.
	oversold := mkItem("SKU-OVERSOLD", "Oversold Overall")
	mkInv(oversold, main, 50, 20)
	mkInv(oversold, store, 10, -30)

	summary, err := reporting.StockSummary(ctx)
	if err != nil {
		t.Fatalf("StockSummary: %v", err)
	}
	// Six InventoryItem rows, three items. Counting rows gives 6.
	if summary.ItemCount != 3 {
		t.Errorf("ItemCount = %d, want 3 — it counts items, not item×facility rows (there are 6 rows)", summary.ItemCount)
	}
	if summary.TotalOnHand != 160 {
		t.Errorf("TotalOnHand = %v, want 160", summary.TotalOnHand)
	}
	if summary.TotalATP != 85 {
		t.Errorf("TotalATP = %v, want 85", summary.TotalATP)
	}
	// Counting rows with ATP <= 0 gives 3 (split@store, gone@main,
	// gone@store... plus oversold@store = 4). Per item it is 2.
	if summary.StockoutCount != 2 {
		t.Errorf("StockoutCount = %d, want 2 (SKU-GONE and SKU-OVERSOLD); SKU-SPLIT has stock in another facility", summary.StockoutCount)
	}

	risk, err := reporting.StockoutRiskItems(ctx, 10)
	if err != nil {
		t.Fatalf("StockoutRiskItems: %v", err)
	}
	if len(risk) != 2 {
		t.Fatalf("got %d stockout rows, want 2 (one per item, not one per facility): %+v", len(risk), risk)
	}
	bySKU := map[string]data.StockoutRiskItem{}
	for _, r := range risk {
		if _, dup := bySKU[r.SKU]; dup {
			t.Errorf("%s appears more than once — the report must be one row per item", r.SKU)
		}
		bySKU[r.SKU] = r
	}
	if _, ok := bySKU["SKU-SPLIT"]; ok {
		t.Error("SKU-SPLIT is empty in one facility but full in another — it is not an organization-level stockout")
	}
	if got := bySKU["SKU-OVERSOLD"]; got.QtyATP != -10 {
		t.Errorf("SKU-OVERSOLD ATP = %v, want -10 (20 + -30 summed across facilities)", got.QtyATP)
	}
	if got := bySKU["SKU-OVERSOLD"]; got.QtyOnHand != 60 {
		t.Errorf("SKU-OVERSOLD on-hand = %v, want 60 (50 + 10)", got.QtyOnHand)
	}
	if got := bySKU["SKU-GONE"]; got.QtyATP != -5 {
		t.Errorf("SKU-GONE ATP = %v, want -5 (0 + -5)", got.QtyATP)
	}
	// Worst first, by the summed figure.
	if risk[0].SKU != "SKU-OVERSOLD" {
		t.Errorf("rank 0 = %s, want SKU-OVERSOLD at -10 — ordering must use the summed ATP", risk[0].SKU)
	}

	// OnHandQtyByItem already summed before this change; pinned here so
	// the three stock aggregates are covered by one fixture.
	onHand, err := reporting.OnHandQtyByItem(ctx)
	if err != nil {
		t.Fatalf("OnHandQtyByItem: %v", err)
	}
	if onHand[split] != 100 {
		t.Errorf("OnHandQtyByItem[SKU-SPLIT] = %v, want 100", onHand[split])
	}
	if onHand[oversold] != 60 {
		t.Errorf("OnHandQtyByItem[SKU-OVERSOLD] = %v, want 60", onHand[oversold])
	}
}

// Two edge cases in StockSummary's per-item grouping, both of which
// looked fine and were wrong (independent review of #12).
func TestStockSummary_GroupingEdgeCases(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	real, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-REAL", "name": "Real", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": real.ID, "qty_on_hand": 10.0, "qty_available_to_promise": 10.0,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}
	// Two rows with no item_id at all. GROUP BY collapses them into one
	// NULL group, which count(*) would report as an "item" that joins to
	// no Item record — so the headline count and the risk table below it
	// would disagree about what exists.
	for i := 0; i < 2; i++ {
		if _, err := records.Create(ctx, "InventoryItem", map[string]any{
			"qty_on_hand": 3.0, "qty_available_to_promise": -1.0,
		}); err != nil {
			t.Fatalf("create item-less InventoryItem: %v", err)
		}
	}

	summary, err := reporting.StockSummary(ctx)
	if err != nil {
		t.Fatalf("StockSummary: %v", err)
	}
	if summary.ItemCount != 1 {
		t.Errorf("ItemCount = %d, want 1 — rows with no item_id must be excluded, not grouped into a phantom item", summary.ItemCount)
	}
	if summary.StockoutCount != 0 {
		t.Errorf("StockoutCount = %d, want 0 — the item-less rows must not raise an alert about an item that does not exist", summary.StockoutCount)
	}
	if summary.TotalOnHand != 10 {
		t.Errorf("TotalOnHand = %v, want 10 (the item-less rows are excluded entirely)", summary.TotalOnHand)
	}
}

// A row with no qty_available_to_promise means "unknown", not "zero
// available". Old per-row behaviour: NULL <= 0 is NULL, so uncounted.
// Naively wrapping the grouped sum in coalesce(...,0) turns every such
// item into a stockout alert.
func TestStockSummary_MissingATPIsNotAStockout(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-NOATP", "name": "No ATP", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	if _, err := records.Create(ctx, "InventoryItem", map[string]any{
		"item_id": item.ID, "qty_on_hand": 10.0,
	}); err != nil {
		t.Fatalf("create InventoryItem: %v", err)
	}

	summary, err := reporting.StockSummary(ctx)
	if err != nil {
		t.Fatalf("StockSummary: %v", err)
	}
	if summary.ItemCount != 1 {
		t.Errorf("ItemCount = %d, want 1", summary.ItemCount)
	}
	if summary.StockoutCount != 0 {
		t.Errorf("StockoutCount = %d, want 0 — an absent qty_available_to_promise is unknown, not exhausted", summary.StockoutCount)
	}
}

// TestGoodsReceiptLineQualities_ReturnsQuantitiesAndResolvesVendor
// (uc-infra#82) pins the basic read path: a line with quality data set
// resolves through GoodsReceipt.purchase_order_id to the PurchaseOrder's
// vendor, and QtyAccepted/QtyRejected pass through as the exact stored
// values.
func TestGoodsReceiptLineQualities_ReturnsQuantitiesAndResolvesVendor(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Quality Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-1", "vendor_id": vendor.ID, "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	line, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 10.0, "qty_accepted": 8.0, "qty_rejected": 2.0,
	})
	if err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	row := got[0]
	if row.LineID != line.ID {
		t.Errorf("LineID = %q, want %q", row.LineID, line.ID)
	}
	if row.VendorID != vendor.ID {
		t.Errorf("VendorID = %q, want %q", row.VendorID, vendor.ID)
	}
	if row.VendorName != "Quality Vendor" {
		t.Errorf("VendorName = %q, want %q", row.VendorName, "Quality Vendor")
	}
	if row.QtyAccepted != "8" {
		t.Errorf("QtyAccepted = %q, want %q", row.QtyAccepted, "8")
	}
	if row.QtyRejected != "2" {
		t.Errorf("QtyRejected = %q, want %q", row.QtyRejected, "2")
	}
}

// TestGoodsReceiptLineQualities_NoQualityDataIsEmptyNotZero (uc-infra#82)
// pins the "absent, not zero" distinction the forecast package's
// HasData flag depends on: a line written without qty_accepted/
// qty_rejected at all (every line before uc-infra#82, or a tenant that
// never uses the feature) must come back with EMPTY strings, not "0".
func TestGoodsReceiptLineQualities_NoQualityDataIsEmptyNotZero(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "No Quality Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-2", "vendor_id": vendor.ID, "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 10.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].QtyAccepted != "" || got[0].QtyRejected != "" {
		t.Errorf("QtyAccepted=%q QtyRejected=%q, want both empty (no quality data recorded)", got[0].QtyAccepted, got[0].QtyRejected)
	}
}

// TestGoodsReceiptLineQualities_MissingVendorStillReturnsRowWithEmptyVendorID
// (uc-infra#82) mirrors CompletedPOLeadTimes' own vendor-join reasoning:
// a line whose PurchaseOrder has no (or a dangling/malformed) vendor_id
// still holds real quality evidence for the OVERALL aggregate, so it
// must be returned with an empty VendorID rather than dropped.
func TestGoodsReceiptLineQualities_MissingVendorStillReturnsRowWithEmptyVendorID(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-NOVENDOR", "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 5.0, "qty_accepted": 5.0, "qty_rejected": 0.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].VendorID != "" {
		t.Errorf("VendorID = %q, want empty (PurchaseOrder has no vendor_id)", got[0].VendorID)
	}
	if got[0].QtyAccepted != "5" {
		t.Errorf("QtyAccepted = %q, want %q — quality data must still be returned despite no vendor", got[0].QtyAccepted, "5")
	}
}

// TestGoodsReceiptLineQualities_MalformedGoodsReceiptIDExcludedNotAborted
// (uc-infra#82) mirrors the uuidPattern guard's own purpose everywhere
// else in this file: one line with a malformed goods_receipt_id must not
// abort the whole query for every other, perfectly valid line.
func TestGoodsReceiptLineQualities_MalformedGoodsReceiptIDExcludedNotAborted(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Guard Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-GUARD", "vendor_id": vendor.ID, "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": "not-a-uuid", "qty_received": 3.0,
	}); err != nil {
		t.Fatalf("create malformed GoodsReceiptLine: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 5.0, "qty_accepted": 5.0, "qty_rejected": 0.0,
	}); err != nil {
		t.Fatalf("create valid GoodsReceiptLine: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (the malformed line must be excluded, not abort the query)", len(got))
	}
	if got[0].QtyAccepted != "5" {
		t.Errorf("QtyAccepted = %q, want %q", got[0].QtyAccepted, "5")
	}
}

// TestGoodsReceiptLineQualities_MalformedPurchaseOrderIDExcludedNotAborted
// (uc-infra#82, independent review) is the second half of the guard the
// previous test only covers one hop of: a GoodsReceipt whose
// purchase_order_id is malformed must also be excluded, not abort the
// whole query. Without the g.data->>'purchase_order_id' ~ $1 guard on
// that hop specifically, this scenario would 500 the entire report page
// for every tenant, not just fail to resolve one vendor.
func TestGoodsReceiptLineQualities_MalformedPurchaseOrderIDExcludedNotAborted(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Guard Vendor 2", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-GUARD-2", "vendor_id": vendor.ID, "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	validGR, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	// A GoodsReceipt whose OWN purchase_order_id is malformed — a
	// different hop than the previous test's malformed goods_receipt_id.
	malformedGR, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": "not-a-uuid", "received_date": "2026-07-06",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt with malformed purchase_order_id: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": malformedGR.ID, "qty_received": 3.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine against the malformed GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": validGR.ID, "qty_received": 5.0, "qty_accepted": 5.0, "qty_rejected": 0.0,
	}); err != nil {
		t.Fatalf("create valid GoodsReceiptLine: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (the line behind a malformed purchase_order_id must be excluded, not abort the query)", len(got))
	}
	if got[0].QtyAccepted != "5" {
		t.Errorf("QtyAccepted = %q, want %q", got[0].QtyAccepted, "5")
	}
}

// TestGoodsReceiptLineQualities_DeletedPurchaseOrderExcludesItsLines
// (uc-infra#82, independent review) pins the INNER-join behavior on the
// GoodsReceipt/PurchaseOrder hops: a soft-deleted PurchaseOrder's
// receipt lines are excluded from the quality aggregate entirely,
// matching CompletedPOLeadTimes' own precedent of requiring
// PurchaseOrder to be a live record. This is deliberately narrower than
// the vendor hop (which IS left-joined) — see the method's own doc
// comment for why the two hops behave differently.
func TestGoodsReceiptLineQualities_DeletedPurchaseOrderExcludesItsLines(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Deleted PO Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	po, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-QUALITY-DELETED", "vendor_id": vendor.ID, "order_date": "2026-07-01",
	})
	if err != nil {
		t.Fatalf("create PurchaseOrder: %v", err)
	}
	gr, err := records.Create(ctx, "GoodsReceipt", map[string]any{
		"purchase_order_id": po.ID, "received_date": "2026-07-05",
	})
	if err != nil {
		t.Fatalf("create GoodsReceipt: %v", err)
	}
	if _, err := records.Create(ctx, "GoodsReceiptLine", map[string]any{
		"goods_receipt_id": gr.ID, "qty_received": 5.0, "qty_accepted": 5.0, "qty_rejected": 0.0,
	}); err != nil {
		t.Fatalf("create GoodsReceiptLine: %v", err)
	}

	if err := records.Delete(ctx, "PurchaseOrder", po.ID); err != nil {
		t.Fatalf("soft-delete PurchaseOrder: %v", err)
	}

	got, err := reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		t.Fatalf("GoodsReceiptLineQualities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0 — a deleted PurchaseOrder's receipt lines must be excluded", len(got))
	}
}
