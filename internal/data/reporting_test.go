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
	"github.com/universaltill/universal-core/internal/kernel/projects"
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

// TestPurchaseOrderStatusBreakdown_LegacyFractionalTotalExcludedNotFatal
// (uc-infra#136) mirrors rfq_reporting_test.go's own
// TestRFQComparison_LegacyFractionalUnitPriceExcludedNotFatal: a
// PurchaseOrder written before total's FieldNumber->FieldMoney Version
// bump still carries a fractional major-unit decimal until
// cmd/backfill-purchase-order-total converts it. The moneyMinorUnitsPattern
// guard must exclude that row from the SUM without erroring the whole
// query or dropping the row from count(*) — it's still a real
// PurchaseOrder in this status, just one whose value can't be trusted
// yet.
func TestPurchaseOrderStatusBreakdown_LegacyFractionalTotalExcludedNotFatal(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	draft, err := records.Create(ctx, "Status", map[string]any{"code": "draft", "name": "Draft"})
	if err != nil {
		t.Fatalf("create Status: %v", err)
	}
	// The pre-migration shape: a real major-unit decimal, exactly what
	// total held before the Version bump.
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-LEGACY", "status_id": draft.ID, "total": 9.5,
	}); err != nil {
		t.Fatalf("create legacy PurchaseOrder: %v", err)
	}
	// A real, already-migrated (post-backfill) minor-units row in the
	// same status — must still sum normally alongside the excluded
	// legacy row, proving the guard excludes only the bad row's VALUE,
	// not the row itself.
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-MIGRATED", "status_id": draft.ID, "total": 1025,
	}); err != nil {
		t.Fatalf("create migrated PurchaseOrder: %v", err)
	}

	rows, err := reporting.PurchaseOrderStatusBreakdown(ctx)
	if err != nil {
		t.Fatalf("PurchaseOrderStatusBreakdown must not error on a legacy fractional total: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 status row, got %d: %+v", len(rows), rows)
	}
	got := rows[0]
	// Count includes BOTH POs — the legacy row still really exists and
	// is still really in this status; only its contribution to Value is
	// excluded.
	if got.Count != 2 {
		t.Errorf("expected Count=2 (legacy row still counted), got %d", got.Count)
	}
	if got.Value != 1025 {
		t.Errorf("expected Value=1025 (only the migrated row's total summed), got %v", got.Value)
	}
}

// TestTopVendorsBySpend_LegacyFractionalTotalExcludedNotFatal is
// TestPurchaseOrderStatusBreakdown_LegacyFractionalTotalExcludedNotFatal's
// counterpart for the vendor-spend query.
func TestTopVendorsBySpend_LegacyFractionalTotalExcludedNotFatal(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Legacy Vendor", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-LEGACY", "vendor_id": vendor.ID, "total": 9.5,
	}); err != nil {
		t.Fatalf("create legacy PurchaseOrder: %v", err)
	}
	if _, err := records.Create(ctx, "PurchaseOrder", map[string]any{
		"po_number": "PO-MIGRATED", "vendor_id": vendor.ID, "total": 1025,
	}); err != nil {
		t.Fatalf("create migrated PurchaseOrder: %v", err)
	}

	got, err := reporting.TopVendorsBySpend(ctx, 10)
	if err != nil {
		t.Fatalf("TopVendorsBySpend must not error on a legacy fractional total: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 vendor row, got %d: %+v", len(got), got)
	}
	if got[0].OrderCount != 2 {
		t.Errorf("expected OrderCount=2 (legacy row still counted), got %d", got[0].OrderCount)
	}
	if got[0].Total != 1025 {
		t.Errorf("expected Total=1025 (only the migrated row's total summed), got %v", got[0].Total)
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

// --- ProjectBudgetActuals (uc-infra#134) ---
//
// projectActualsFixture is the shared setup every test below builds on:
// a Project, and helpers for the Task/TimeEntry/Employee/Party rows a
// "labour" actual is computed from. Kept minimal — only the fields
// ProjectBudgetActuals' own query actually reads — since RecordRepo.Create
// (like every other fixture in this file) bypasses entity.ValidateRecord
// entirely, so it never enforces Required/Min/Max on what's written here.
type projectActualsFixture struct {
	t         *testing.T
	ctx       context.Context
	records   *data.RecordRepo
	reporting *data.ReportingRepo
	projectID string
}

func newProjectActualsFixture(t *testing.T, projectCode string) *projectActualsFixture {
	t.Helper()
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)
	proj, err := records.Create(ctx, "Project", map[string]any{
		"project_code": projectCode, "name": projectCode, "start_date": "2026-01-01",
	})
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}
	return &projectActualsFixture{t: t, ctx: ctx, records: records, reporting: reporting, projectID: proj.ID}
}

func (f *projectActualsFixture) budgetLine(category string) {
	f.t.Helper()
	if _, err := f.records.Create(f.ctx, "ProjectBudgetLine", map[string]any{
		"project_id": f.projectID, "category": category, "planned_amount": 0.0,
	}); err != nil {
		f.t.Fatalf("create ProjectBudgetLine: %v", err)
	}
}

func (f *projectActualsFixture) task() string {
	f.t.Helper()
	task, err := f.records.Create(f.ctx, "Task", map[string]any{
		"project_id": f.projectID, "title": "work",
	})
	if err != nil {
		f.t.Fatalf("create Task: %v", err)
	}
	return task.ID
}

// party creates a bare Party row — TimeEntry.employee_id targets Party
// (ADR-0013 rule 4), and RecordRepo.Create never enforces the
// employee-PartyRole TargetFilter (that's a crud.Engine-time check, not
// exercised by this repo-level test), so an empty Party record is
// sufficient to exist as the referenced id.
func (f *projectActualsFixture) party(name string) string {
	f.t.Helper()
	p, err := f.records.Create(f.ctx, "Party", map[string]any{"name": name, "party_type": "person"})
	if err != nil {
		f.t.Fatalf("create Party: %v", err)
	}
	return p.ID
}

// employee creates an Employee record for partyID. costRateMinor < 0
// means "leave cost_rate unset" (Go has no natural "absent" sentinel for
// an int literal in a table-driven caller, and a real cost_rate can
// never legitimately be negative per its own Min:0 bound, so it's a safe
// sentinel here).
func (f *projectActualsFixture) employee(partyID, hireDate string, costRateMinor int64) string {
	f.t.Helper()
	fields := map[string]any{"employee_number": "E-" + partyID, "party_id": partyID, "hire_date": hireDate}
	if costRateMinor >= 0 {
		fields["cost_rate"] = costRateMinor
	}
	emp, err := f.records.Create(f.ctx, "Employee", fields)
	if err != nil {
		f.t.Fatalf("create Employee: %v", err)
	}
	return emp.ID
}

func (f *projectActualsFixture) timeEntry(taskID, employeeID string, hours float64) {
	f.t.Helper()
	if _, err := f.records.Create(f.ctx, "TimeEntry", map[string]any{
		"task_id": taskID, "employee_id": employeeID, "entry_date": "2026-02-01", "hours": hours,
	}); err != nil {
		f.t.Fatalf("create TimeEntry: %v", err)
	}
}

func (f *projectActualsFixture) byCategory(rows []data.ProjectCategoryActual) map[string]data.ProjectCategoryActual {
	out := map[string]data.ProjectCategoryActual{}
	for _, r := range rows {
		out[r.Category] = r
	}
	return out
}

// TestProjectBudgetActuals_LabourCategoryAllPriced (uc-infra#134) is the
// simple happy path: every TimeEntry's employee has cost_rate set, so
// Actual is the exact sum and nothing is Unpriced.
func TestProjectBudgetActuals_LabourCategoryAllPriced(t *testing.T) {
	f := newProjectActualsFixture(t, "P-LABOUR-ALL-PRICED")
	f.budgetLine("labour")
	task := f.task()

	partyA := f.party("Alice")
	f.employee(partyA, "2020-01-01", 2500) // $25.00/hr
	partyB := f.party("Bob")
	f.employee(partyB, "2021-01-01", 1000) // $10.00/hr

	f.timeEntry(task, partyA, 2) // 2h * $25.00 = $50.00 = 5000 minor
	f.timeEntry(task, partyB, 3) // 3h * $10.00 = $30.00 = 3000 minor

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	byCat := f.byCategory(got)
	labour, ok := byCat["labour"]
	if !ok {
		t.Fatal("expected a labour row")
	}
	if labour.Actual == nil {
		t.Fatal("expected a non-nil Actual — real TimeEntry data was logged and fully priced")
	}
	if *labour.Actual != 8000 {
		t.Errorf("Actual = %d, want 8000 (5000 + 3000 minor units)", *labour.Actual)
	}
	if labour.UnpricedHours != 0 || labour.UnpricedEntries != 0 {
		t.Errorf("expected no unpriced hours/entries, got %v/%v", labour.UnpricedHours, labour.UnpricedEntries)
	}
}

// TestProjectBudgetActuals_LabourCategoryPartiallyPriced (uc-infra#134)
// is the case the whole Unpriced* pair exists for: some TimeEntry rows'
// employees have no cost_rate set at all, so those hours/entries must be
// excluded from Actual and counted separately — Actual must reflect only
// the priced subset, never the full total and never a fabricated $0.
func TestProjectBudgetActuals_LabourCategoryPartiallyPriced(t *testing.T) {
	f := newProjectActualsFixture(t, "P-LABOUR-PARTIAL")
	f.budgetLine("labour")
	task := f.task()

	pricedParty := f.party("Priced Employee")
	f.employee(pricedParty, "2020-01-01", 2000) // $20.00/hr

	unpricedParty := f.party("Unpriced Employee")
	f.employee(unpricedParty, "2020-01-01", -1) // no cost_rate set

	f.timeEntry(task, pricedParty, 4)     // 4h * $20.00 = $80.00 = 8000 minor
	f.timeEntry(task, unpricedParty, 6)   // unpriced: no cost_rate
	f.timeEntry(task, unpricedParty, 1.5) // unpriced: another entry, same party

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 8000 {
		t.Fatalf("Actual = %v, want a real 8000 (only the priced entry) — not $0, not the full total", labour.Actual)
	}
	if labour.UnpricedHours != 7.5 {
		t.Errorf("UnpricedHours = %v, want 7.5 (6 + 1.5)", labour.UnpricedHours)
	}
	if labour.UnpricedEntries != 2 {
		t.Errorf("UnpricedEntries = %v, want 2", labour.UnpricedEntries)
	}
}

// TestProjectBudgetActuals_NonLabourCategoryIsNilNotZero (uc-infra#134)
// pins the headline invariant of this whole design: a category with no
// expense source at all (materials, here) must report Actual == nil,
// distinguishable from a genuine zero, even when the project has zero
// labour activity either.
func TestProjectBudgetActuals_NonLabourCategoryIsNilNotZero(t *testing.T) {
	f := newProjectActualsFixture(t, "P-MATERIALS-ONLY")
	f.budgetLine("materials")

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Category != "materials" {
		t.Errorf("Category = %q, want materials", got[0].Category)
	}
	if got[0].Actual != nil {
		t.Errorf("Actual = %v, want nil — materials has no expense source to compute one from", *got[0].Actual)
	}
	if got[0].UnpricedHours != 0 || got[0].UnpricedEntries != 0 {
		t.Errorf("expected zero Unpriced* for a non-labour category, got %+v", got[0])
	}
}

// TestProjectBudgetActuals_LabourCategoryZeroTimeEntriesIsComputedZero
// (uc-infra#134) is the deliberate counterpart to the nil case above: a
// "labour" budget line with genuinely nothing logged against it gets a
// REAL computed 0, not nil — there is a labour cost source, it just has
// no activity yet, which is a known fact, not an unknown one.
func TestProjectBudgetActuals_LabourCategoryZeroTimeEntriesIsComputedZero(t *testing.T) {
	f := newProjectActualsFixture(t, "P-LABOUR-NO-ACTIVITY")
	f.budgetLine("labour")
	// No Task, no TimeEntry at all.

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil {
		t.Fatal("expected a non-nil, computed-zero Actual for labour with zero logged activity — not nil")
	}
	if *labour.Actual != 0 {
		t.Errorf("Actual = %d, want 0", *labour.Actual)
	}
	if labour.UnpricedHours != 0 || labour.UnpricedEntries != 0 {
		t.Errorf("expected zero Unpriced* with zero TimeEntry rows, got %+v", labour)
	}
}

// TestProjectBudgetActuals_RehireUsesLatestHireDateWithCostRateSet
// (uc-infra#134) pins the doc comment's employee-resolution rule: a
// Party with two Employee records (a rehire) uses whichever record WITH
// cost_rate SET has the latest hire_date, not simply the latest
// Employee record overall.
func TestProjectBudgetActuals_RehireUsesLatestHireDateWithCostRateSet(t *testing.T) {
	f := newProjectActualsFixture(t, "P-REHIRE")
	f.budgetLine("labour")
	task := f.task()

	party := f.party("Rehired Employee")
	// Earlier employment: cost_rate set.
	f.employee(party, "2018-01-01", 1500) // $15.00/hr
	// Later employment (the rehire): NO cost_rate set. If resolution
	// picked "latest hire_date, full stop" this would make the whole
	// party unpriced instead of using the earlier, priced employment.
	f.employee(party, "2026-01-01", -1)

	f.timeEntry(task, party, 4) // should price at $15.00/hr = 6000 minor

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 6000 {
		t.Fatalf("Actual = %v, want 6000 — must fall back to the earlier employment's cost_rate, since the latest employment has none set", labour.Actual)
	}
	if labour.UnpricedHours != 0 || labour.UnpricedEntries != 0 {
		t.Errorf("expected the entry to be priced via the earlier employment, got Unpriced* = %+v", labour)
	}
}

// TestProjectBudgetActuals_NoProjectBudgetLinesReturnsEmptySlice
// (uc-infra#134): a Project with no ProjectBudgetLine rows at all
// returns an empty slice, not an error and not a fabricated row.
func TestProjectBudgetActuals_NoProjectBudgetLinesReturnsEmptySlice(t *testing.T) {
	f := newProjectActualsFixture(t, "P-NO-BUDGET-LINES")

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0", len(got))
	}
}

// TestProjectBudgetActuals_MalformedTaskIDExcludedNotAborted
// (uc-infra#134) is this method's version of TopVendorsBySpend's own
// malformed-reference guard test: a TimeEntry whose task_id isn't even
// a well-formed UUID must be excluded from the aggregate, not abort the
// whole query with a Postgres ::uuid cast error.
func TestProjectBudgetActuals_MalformedTaskIDExcludedNotAborted(t *testing.T) {
	f := newProjectActualsFixture(t, "P-MALFORMED-TASK-ID")
	f.budgetLine("labour")
	task := f.task()

	party := f.party("Guard Employee")
	f.employee(party, "2020-01-01", 4000) // $40.00/hr

	f.timeEntry(task, party, 2) // valid: 2h * $40.00 = 8000 minor
	// A TimeEntry with a task_id that isn't a well-formed UUID at all —
	// same "bad CSV import mapping" scenario uuidPattern's own doc
	// comment (reporting.go) describes for every other join in this file.
	if _, err := f.records.Create(f.ctx, "TimeEntry", map[string]any{
		"task_id": "not-a-uuid", "employee_id": party, "entry_date": "2026-02-01", "hours": 99.0,
	}); err != nil {
		f.t.Fatalf("create malformed TimeEntry: %v", err)
	}

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 8000 {
		t.Fatalf("Actual = %v, want 8000 — the malformed-task_id row must be excluded, not counted or aborting the query", labour.Actual)
	}
	if labour.UnpricedHours != 0 || labour.UnpricedEntries != 0 {
		t.Errorf("the malformed row must be excluded entirely, not counted as unpriced either, got %+v", labour)
	}
}

// TestProjectBudgetActuals_MissingHireDateDoesNotOutrankARealDate
// (uc-infra#134, independent review finding) pins a regression: Postgres
// sorts NULL FIRST on a bare `ORDER BY ... DESC`, so an Employee record
// missing hire_date entirely (hire_date is Required, so this can only
// happen via data that bypassed validation — the same threat model
// uuidPattern's own doc comment defends against elsewhere in this file)
// would silently outrank a real, validly-dated rate under a naive
// `DESC` sort. The employee_rates CTE's `DESC NULLS LAST, id` must push
// the missing-hire_date row to the back, not the front, so the
// genuinely-dated employment's cost_rate is the one actually used.
func TestProjectBudgetActuals_MissingHireDateDoesNotOutrankARealDate(t *testing.T) {
	f := newProjectActualsFixture(t, "P-MISSING-HIRE-DATE")
	f.budgetLine("labour")
	task := f.task()

	party := f.party("Party With A Malformed Employee Row")
	// The real, validly-dated employment.
	f.employee(party, "2020-01-01", 3000) // $30.00/hr
	// A malformed row for the SAME party: cost_rate set, but no
	// hire_date key at all — bypasses the fixture's employee() helper,
	// which always sets one, the same way the malformed-task_id test
	// above bypasses timeEntry() to construct an invalid row directly.
	if _, err := f.records.Create(f.ctx, "Employee", map[string]any{
		"employee_number": "E-NO-HIRE-DATE", "party_id": party, "cost_rate": int64(100),
	}); err != nil {
		t.Fatalf("create malformed Employee (no hire_date): %v", err)
	}

	f.timeEntry(task, party, 2)

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 6000 {
		t.Fatalf("Actual = %v, want 6000 (2h * $30.00) — the real, dated employment's rate must win over a malformed record with no hire_date at all, not the other way around", labour.Actual)
	}
}

// TestProjectBudgetActuals_TieBreakOnHireDateIsDeterministic
// (uc-infra#134, independent review finding) pins the other half of the
// same fix: two Employee records for one Party sharing the exact same
// hire_date (an accepted-but-unprevented case — hr.go's own doc comment
// names simultaneous employments as "accepted") must resolve to the
// SAME record every time, not whatever a heap scan happens to return
// first. The employee_rates CTE breaks the tie by `id` — the smaller id
// wins, since DISTINCT ON's default ORDER BY direction on the trailing
// key is ascending.
func TestProjectBudgetActuals_TieBreakOnHireDateIsDeterministic(t *testing.T) {
	f := newProjectActualsFixture(t, "P-HIRE-DATE-TIE")
	f.budgetLine("labour")
	task := f.task()

	party := f.party("Simultaneous Employments")
	empA, err := f.records.Create(f.ctx, "Employee", map[string]any{
		"employee_number": "E-TIE-A", "party_id": party, "hire_date": "2022-06-01", "cost_rate": int64(1111),
	})
	if err != nil {
		t.Fatalf("create Employee A: %v", err)
	}
	empB, err := f.records.Create(f.ctx, "Employee", map[string]any{
		"employee_number": "E-TIE-B", "party_id": party, "hire_date": "2022-06-01", "cost_rate": int64(2222),
	})
	if err != nil {
		t.Fatalf("create Employee B: %v", err)
	}
	wantRate := int64(1111)
	if empB.ID < empA.ID {
		wantRate = 2222
	}

	f.timeEntry(task, party, 1)

	// Run it twice — a nondeterministic tie-break would be the whole
	// point of this test, and a single run can't distinguish "always
	// resolves this way" from "happened to resolve this way once."
	for i := 0; i < 2; i++ {
		got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
		if err != nil {
			t.Fatalf("ProjectBudgetActuals (run %d): %v", i, err)
		}
		labour := f.byCategory(got)["labour"]
		if labour.Actual == nil || int64(*labour.Actual) != wantRate {
			t.Fatalf("run %d: Actual = %v, want %d — the tie-break must deterministically pick the same Employee record (lowest id) every time", i, labour.Actual, wantRate)
		}
	}
}

// TestProjectBudgetActuals_ExcludesCrossProjectTimeEntries (uc-infra#134,
// independent review finding) is the one filter every shipped scenario
// so far left completely unexercised: every fixture above builds exactly
// one Project, so `AND t.data->>'project_id' = $1` never had a second
// project's data around to actually exclude. A bug that leaked another
// project's hours into this project's actual would have passed all of
// them.
func TestProjectBudgetActuals_ExcludesCrossProjectTimeEntries(t *testing.T) {
	f := newProjectActualsFixture(t, "P-ISOLATION-TARGET")
	f.budgetLine("labour")
	task := f.task()
	party := f.party("Target Project Employee")
	f.employee(party, "2020-01-01", 1000) // $10.00/hr
	f.timeEntry(task, party, 2)           // 2h * $10.00 = 2000 minor — the only entry that should count

	// A second, unrelated Project in the SAME tenant database, with its
	// own much larger labour activity that must not leak into the
	// target project's actual.
	other, err := f.records.Create(f.ctx, "Project", map[string]any{
		"project_code": "P-ISOLATION-OTHER", "name": "P-ISOLATION-OTHER", "start_date": "2026-01-01",
	})
	if err != nil {
		t.Fatalf("create other Project: %v", err)
	}
	otherTask, err := f.records.Create(f.ctx, "Task", map[string]any{
		"project_id": other.ID, "title": "unrelated work",
	})
	if err != nil {
		t.Fatalf("create other Project's Task: %v", err)
	}
	if _, err := f.records.Create(f.ctx, "TimeEntry", map[string]any{
		"task_id": otherTask.ID, "employee_id": party, "entry_date": "2026-02-01", "hours": 100.0,
	}); err != nil {
		t.Fatalf("create other Project's TimeEntry: %v", err)
	}

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 2000 {
		t.Fatalf("Actual = %v, want 2000 — a different Project's 100h must not leak into this one's actual", labour.Actual)
	}
}

// TestProjectBudgetActuals_AllUnpricedIsComputedZeroWithUnpricedCount
// (uc-infra#134) is the most dangerous untested reading of this API a
// caller could get wrong: when EVERY TimeEntry on a labour-active
// project is unpriced, Actual is still a real, non-nil, computed 0 (real
// labour activity exists, none of it happens to be priced) — a caller
// that reads Actual alone without checking UnpricedEntries would see
// "confirmed zero spend," which is exactly wrong. This differs from
// TestProjectBudgetActuals_LabourCategoryZeroTimeEntriesIsComputedZero
// (which has zero TimeEntry rows at all, so UnpricedEntries is also 0)
// by having real, unpriced activity.
func TestProjectBudgetActuals_AllUnpricedIsComputedZeroWithUnpricedCount(t *testing.T) {
	f := newProjectActualsFixture(t, "P-ALL-UNPRICED")
	f.budgetLine("labour")
	task := f.task()

	party := f.party("Entirely Unpriced Employee")
	f.employee(party, "2020-01-01", -1) // no cost_rate set at all
	f.timeEntry(task, party, 5)
	f.timeEntry(task, party, 2.5)

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil {
		t.Fatal("expected a non-nil, computed-zero Actual — real activity exists, it's just entirely unpriced")
	}
	if *labour.Actual != 0 {
		t.Errorf("Actual = %d, want 0 (nothing priced)", *labour.Actual)
	}
	if labour.UnpricedEntries != 2 {
		t.Errorf("UnpricedEntries = %d, want 2 — a caller must be able to tell this apart from genuine zero activity", labour.UnpricedEntries)
	}
	if labour.UnpricedHours != 7.5 {
		t.Errorf("UnpricedHours = %v, want 7.5", labour.UnpricedHours)
	}
}

// TestProjectBudgetActuals_MalformedHoursExcludedEntirely (uc-infra#134)
// is this method's own doc-comment promise ("A TimeEntry whose own
// `hours` value isn't a well-formed number is excluded from the
// aggregate entirely") — untested until now. A malformed hours value
// must not count as priced, unpriced, or abort the query.
func TestProjectBudgetActuals_MalformedHoursExcludedEntirely(t *testing.T) {
	f := newProjectActualsFixture(t, "P-MALFORMED-HOURS")
	f.budgetLine("labour")
	task := f.task()
	party := f.party("Malformed Hours Employee")
	f.employee(party, "2020-01-01", 4000) // $40.00/hr

	f.timeEntry(task, party, 2) // valid: 2h * $40.00 = 8000 minor
	if _, err := f.records.Create(f.ctx, "TimeEntry", map[string]any{
		"task_id": task, "employee_id": party, "entry_date": "2026-02-01", "hours": "not-a-number",
	}); err != nil {
		t.Fatalf("create malformed-hours TimeEntry: %v", err)
	}

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 8000 {
		t.Fatalf("Actual = %v, want 8000 — the malformed-hours row must be excluded, not counted at any rate", labour.Actual)
	}
	if labour.UnpricedHours != 0 || labour.UnpricedEntries != 0 {
		t.Errorf("the malformed-hours row must be excluded entirely, not counted as unpriced either (its hours can't be trusted enough to count as either), got %+v", labour)
	}
}

// TestProjectBudgetActuals_MalformedCostRateTreatedAsUnpriced
// (uc-infra#134) is this method's moneyMinorUnitsPattern guard — an
// Employee record whose cost_rate isn't a well-formed minor-units
// integer must not resolve as that employee's rate; the employee_rates
// CTE's own `~ $4` filter should simply exclude that Employee row,
// leaving the party unpriced exactly as if no cost_rate were set at all.
func TestProjectBudgetActuals_MalformedCostRateTreatedAsUnpriced(t *testing.T) {
	f := newProjectActualsFixture(t, "P-MALFORMED-COST-RATE")
	f.budgetLine("labour")
	task := f.task()
	party := f.party("Malformed Cost Rate Employee")
	if _, err := f.records.Create(f.ctx, "Employee", map[string]any{
		"employee_number": "E-MALFORMED-RATE", "party_id": party, "hire_date": "2020-01-01", "cost_rate": "not-a-rate",
	}); err != nil {
		t.Fatalf("create Employee with malformed cost_rate: %v", err)
	}
	f.timeEntry(task, party, 3)

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 0 {
		t.Fatalf("Actual = %v, want a computed 0 — a malformed cost_rate must not be usable as a rate", labour.Actual)
	}
	if labour.UnpricedEntries != 1 || labour.UnpricedHours != 3 {
		t.Errorf("expected the entry counted as unpriced (malformed cost_rate == no usable rate), got %+v", labour)
	}
}

// TestProjectBudgetActuals_ExcludesSoftDeletedRows (uc-infra#134) checks
// all three deleted_at IS NULL guards this query relies on: a
// soft-deleted TimeEntry, a soft-deleted Task (which takes its
// TimeEntries out of the join entirely), and a soft-deleted Employee
// (whose cost_rate must stop being usable, same effect as never having
// been set) must each be excluded — matching this file's own house
// precedent, TestGoodsReceiptLineQualities_DeletedPurchaseOrderExcludesItsLines.
func TestProjectBudgetActuals_ExcludesSoftDeletedRows(t *testing.T) {
	f := newProjectActualsFixture(t, "P-SOFT-DELETES")
	f.budgetLine("labour")

	liveTask := f.task()
	liveParty := f.party("Live Employee")
	f.employee(liveParty, "2020-01-01", 1000) // $10.00/hr
	f.timeEntry(liveTask, liveParty, 2)       // 2h * $10.00 = 2000 minor — the only entry that should survive

	// A soft-deleted TimeEntry, otherwise identical to a valid one.
	deletedEntry, err := f.records.Create(f.ctx, "TimeEntry", map[string]any{
		"task_id": liveTask, "employee_id": liveParty, "entry_date": "2026-02-01", "hours": 50.0,
	})
	if err != nil {
		t.Fatalf("create TimeEntry to delete: %v", err)
	}
	if err := f.records.Delete(f.ctx, "TimeEntry", deletedEntry.ID); err != nil {
		t.Fatalf("soft-delete TimeEntry: %v", err)
	}

	// A soft-deleted Task with a real, otherwise-valid TimeEntry under it.
	deletedTask := f.task()
	f.timeEntry(deletedTask, liveParty, 75)
	if err := f.records.Delete(f.ctx, "Task", deletedTask); err != nil {
		t.Fatalf("soft-delete Task: %v", err)
	}

	// A soft-deleted Employee: cost_rate must stop being usable.
	deletedRateParty := f.party("Soft-Deleted Employee")
	deletedEmp, err := f.records.Create(f.ctx, "Employee", map[string]any{
		"employee_number": "E-DELETED", "party_id": deletedRateParty, "hire_date": "2020-01-01", "cost_rate": int64(9999),
	})
	if err != nil {
		t.Fatalf("create Employee to delete: %v", err)
	}
	if err := f.records.Delete(f.ctx, "Employee", deletedEmp.ID); err != nil {
		t.Fatalf("soft-delete Employee: %v", err)
	}
	f.timeEntry(liveTask, deletedRateParty, 10)

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 2000 {
		t.Fatalf("Actual = %v, want 2000 — soft-deleted TimeEntry/Task rows must not contribute at all", labour.Actual)
	}
	if labour.UnpricedEntries != 1 || labour.UnpricedHours != 10 {
		t.Errorf("expected exactly the soft-deleted-Employee's TimeEntry counted as unpriced (10h), got %+v", labour)
	}
}

// TestProjectBudgetActuals_TwoLabourBudgetLinesShareOneCombinedRow
// (uc-infra#134) pins this method's own doc comment claim: a Project
// with two "labour" ProjectBudgetLine rows (not prevented by the
// entity's own schema — nothing enforces one row per category) still
// gets exactly one combined "labour" row back, not two, and not an
// error.
func TestProjectBudgetActuals_TwoLabourBudgetLinesShareOneCombinedRow(t *testing.T) {
	f := newProjectActualsFixture(t, "P-TWO-LABOUR-LINES")
	f.budgetLine("labour")
	f.budgetLine("labour") // a second labour line — not prevented today
	task := f.task()
	party := f.party("Combined Row Employee")
	f.employee(party, "2020-01-01", 500) // $5.00/hr
	f.timeEntry(task, party, 4)          // 4h * $5.00 = 2000 minor

	got, err := f.reporting.ProjectBudgetActuals(f.ctx, f.projectID)
	if err != nil {
		t.Fatalf("ProjectBudgetActuals: %v", err)
	}
	count := 0
	for _, r := range got {
		if r.Category == "labour" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d labour rows, want exactly 1 combined row even with two ProjectBudgetLine rows sharing that category", count)
	}
	labour := f.byCategory(got)["labour"]
	if labour.Actual == nil || *labour.Actual != 2000 {
		t.Fatalf("Actual = %v, want 2000", labour.Actual)
	}
}

// TestProjectBudgetActuals_LabourEnumValueStillMatchesProjectsPackage
// (uc-infra#134, independent review finding) guards against
// ProjectBudgetLine's "labour" EnumValues entry being renamed in
// projects.go without anyone noticing that ProjectBudgetActuals' own
// `cat != "labour"` string comparison (reporting.go) would then silently
// treat EVERY category as non-priceable, with no build or test failure
// anywhere else to catch it.
func TestProjectBudgetActuals_LabourEnumValueStillMatchesProjectsPackage(t *testing.T) {
	def := projects.ProjectBudgetLine()
	f, ok := def.FieldByName("category")
	if !ok {
		t.Fatal("expected a category field on ProjectBudgetLine")
	}
	found := false
	for _, v := range f.EnumValues {
		if v == "labour" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ProjectBudgetLine.category's EnumValues = %v, want \"labour\" present — ProjectBudgetActuals' string comparison depends on this exact value", f.EnumValues)
	}
}
