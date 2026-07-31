package data_test

import (
	"context"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
)

// TestRFQComparison_GridWithMissingQuotesAndExcludedMalformedRefs is the
// core proof for RFQComparison (#9): two RFQ lines, two invited vendors,
// quote lines covering some but not all (line, vendor) pairs — the
// returned grid must carry the real prices where quoted, leave the gap
// genuinely absent (not a fabricated zero) where a vendor never quoted a
// line, and exclude every malformed/dangling/wrong-RFQ reference row
// rather than aborting the whole report (uuidPattern's own doc comment,
// this file's mirror of reporting_test.go's established pattern).
func TestRFQComparison_GridWithMissingQuotesAndExcludedMalformedRefs(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	rfq, err := records.Create(ctx, "RequestForQuotation", map[string]any{
		"rfq_number": "RFQ-1", "due_date": "2026-08-15",
	})
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}
	// A second, unrelated RFQ — proves the comparison is scoped to the
	// requested rfqID, not every RequestForQuotationLine/Vendor/QuoteLine
	// in the tenant.
	otherRFQ, err := records.Create(ctx, "RequestForQuotation", map[string]any{
		"rfq_number": "RFQ-OTHER", "due_date": "2026-08-15",
	})
	if err != nil {
		t.Fatalf("create other RequestForQuotation: %v", err)
	}

	itemA, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-RFQ-A", "name": "Widget A", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item A: %v", err)
	}
	itemB, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-RFQ-B", "name": "Widget B", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item B: %v", err)
	}

	lineA, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemA.ID, "qty": 10.0,
	})
	if err != nil {
		t.Fatalf("create line A: %v", err)
	}
	lineB, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemB.ID, "qty": 5.0,
	})
	if err != nil {
		t.Fatalf("create line B: %v", err)
	}
	// A line on the OTHER RFQ — must never appear in this RFQ's grid.
	if _, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": otherRFQ.ID, "item_id": itemA.ID, "qty": 999.0,
	}); err != nil {
		t.Fatalf("create other RFQ's line: %v", err)
	}
	// A malformed item_id must exclude the line, not abort the query.
	if _, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": "not-a-uuid", "qty": 1.0,
	}); err != nil {
		t.Fatalf("create malformed line: %v", err)
	}

	vendorX, err := records.Create(ctx, "Party", map[string]any{"name": "Vendor X", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create vendor X: %v", err)
	}
	vendorY, err := records.Create(ctx, "Party", map[string]any{"name": "Vendor Y", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create vendor Y: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorX.ID,
	}); err != nil {
		t.Fatalf("invite vendor X: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorY.ID,
	}); err != nil {
		t.Fatalf("invite vendor Y: %v", err)
	}
	// A dangling vendor_id (well-formed UUID, no matching Party) must be
	// excluded from the vendor list.
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": "00000000-0000-0000-0000-000000000000",
	}); err != nil {
		t.Fatalf("invite dangling vendor: %v", err)
	}
	// An invited vendor on the OTHER RFQ — must never appear in this
	// RFQ's vendor column list.
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": otherRFQ.ID, "vendor_id": vendorX.ID,
	}); err != nil {
		t.Fatalf("invite vendor on other RFQ: %v", err)
	}

	// Quote coverage: lineA quoted by both vendors, lineB quoted only by
	// vendor X — the deliberate missing-quote gap this test pins.
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorX.ID, "unit_price": 9.5,
	}); err != nil {
		t.Fatalf("quote lineA/vendorX: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": lineA.ID, "vendor_id": vendorY.ID, "unit_price": 10.25,
	}); err != nil {
		t.Fatalf("quote lineA/vendorY: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": lineB.ID, "vendor_id": vendorX.ID, "unit_price": 4.0,
	}); err != nil {
		t.Fatalf("quote lineB/vendorX: %v", err)
	}
	// A quote against a malformed rfq_line_id must be excluded, not fatal.
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": "not-a-uuid", "vendor_id": vendorX.ID, "unit_price": 1.0,
	}); err != nil {
		t.Fatalf("create malformed quote line: %v", err)
	}

	lines, vendors, err := reporting.RFQComparison(ctx, rfq.ID)
	if err != nil {
		t.Fatalf("RFQComparison: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (malformed-item line and other-RFQ line excluded): %+v", len(lines), lines)
	}
	if lines[0].ID != lineA.ID || lines[0].ItemName != "Widget A" || lines[0].Qty != 10 {
		t.Errorf("line 0 = %+v, want lineA/Widget A/qty 10", lines[0])
	}
	if lines[1].ID != lineB.ID || lines[1].ItemName != "Widget B" || lines[1].Qty != 5 {
		t.Errorf("line 1 = %+v, want lineB/Widget B/qty 5", lines[1])
	}

	if len(vendors) != 2 {
		t.Fatalf("got %d vendors, want 2 (dangling ref and other-RFQ invite excluded): %+v", len(vendors), vendors)
	}
	if vendors[0].ID != vendorX.ID || vendors[0].Name != "Vendor X" {
		t.Errorf("vendor 0 = %+v, want Vendor X", vendors[0])
	}
	if vendors[1].ID != vendorY.ID || vendors[1].Name != "Vendor Y" {
		t.Errorf("vendor 1 = %+v, want Vendor Y", vendors[1])
	}

	if got := lines[0].QuotesByVendor[vendorX.ID]; got != 9.5 {
		t.Errorf("lineA/vendorX quote = %v, want 9.5", got)
	}
	if got := lines[0].QuotesByVendor[vendorY.ID]; got != 10.25 {
		t.Errorf("lineA/vendorY quote = %v, want 10.25", got)
	}
	if got := lines[1].QuotesByVendor[vendorX.ID]; got != 4.0 {
		t.Errorf("lineB/vendorX quote = %v, want 4.0", got)
	}
	if _, ok := lines[1].QuotesByVendor[vendorY.ID]; ok {
		t.Errorf("lineB/vendorY must have NO quote (the deliberate gap), got %v", lines[1].QuotesByVendor[vendorY.ID])
	}
}

// TestRFQComparison_NoLinesOrVendorsReturnsEmptySlices confirms an RFQ
// with nothing attached yet returns empty (not nil-panicking, not
// erroring) results — the handler's own empty-state rendering (#9)
// depends on being able to tell "zero lines/vendors" apart from a query
// failure.
func TestRFQComparison_NoLinesOrVendorsReturnsEmptySlices(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	rfq, err := records.Create(ctx, "RequestForQuotation", map[string]any{
		"rfq_number": "RFQ-EMPTY", "due_date": "2026-08-15",
	})
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}

	lines, vendors, err := reporting.RFQComparison(ctx, rfq.ID)
	if err != nil {
		t.Fatalf("RFQComparison: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines, got %d: %+v", len(lines), lines)
	}
	if len(vendors) != 0 {
		t.Errorf("expected 0 vendors, got %d: %+v", len(vendors), vendors)
	}
}

// TestRFQComparison_NonCanonicalUUIDReferencesStillResolve is the
// regression test for the independent review's 2026-07-31 finding: a
// quote line whose rfq_line_id/vendor_id is stored in a non-canonical
// but perfectly castable UUID spelling (uppercase hex here) used to join
// fine yet produce a map key that matched nothing, so the vendor's real
// quoted price silently vanished from the grid and the report showed
// them as never having quoted. entity.FieldReference validates only "is
// a string" — it never normalizes UUID spelling — so nothing upstream
// prevents this. RFQComparison now round-trips both map keys through
// ::uuid, matching the canonical form of the `id` column they're
// compared against.
func TestRFQComparison_NonCanonicalUUIDReferencesStillResolve(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	rfq, err := records.Create(ctx, "RequestForQuotation", map[string]any{
		"rfq_number": "RFQ-CASE", "due_date": "2026-08-15",
	})
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-CASE", "name": "Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	// item_id upper-cased too: the item name must still resolve.
	line, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": strings.ToUpper(item.ID), "qty": 3.0,
	})
	if err != nil {
		t.Fatalf("create line: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Vendor U", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": strings.ToUpper(vendor.ID),
	}); err != nil {
		t.Fatalf("invite vendor: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": strings.ToUpper(line.ID), "vendor_id": strings.ToUpper(vendor.ID), "unit_price": 7.25,
	}); err != nil {
		t.Fatalf("create quote line: %v", err)
	}
	// A quote line with a malformed vendor_id must be excluded, not abort
	// the query (the guard added alongside the cast).
	if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
		"rfq_line_id": line.ID, "vendor_id": "not-a-uuid", "unit_price": 1.0,
	}); err != nil {
		t.Fatalf("create malformed-vendor quote line: %v", err)
	}

	lines, vendors, err := reporting.RFQComparison(ctx, rfq.ID)
	if err != nil {
		t.Fatalf("RFQComparison: %v", err)
	}
	if len(lines) != 1 || len(vendors) != 1 {
		t.Fatalf("expected 1 line and 1 vendor, got %d/%d: %+v %+v", len(lines), len(vendors), lines, vendors)
	}
	if lines[0].ItemName != "Widget" {
		t.Errorf("item name = %q, want Widget (uppercase item_id must still join)", lines[0].ItemName)
	}
	got, ok := lines[0].QuotesByVendor[vendors[0].ID]
	if !ok {
		t.Fatalf("quote lost: QuotesByVendor = %+v, vendor column id = %s", lines[0].QuotesByVendor, vendors[0].ID)
	}
	if got != 7.25 {
		t.Errorf("quote = %v, want 7.25", got)
	}
	if len(lines[0].QuotesByVendor) != 1 {
		t.Errorf("expected exactly 1 quote (the malformed-vendor row excluded), got %+v", lines[0].QuotesByVendor)
	}
}

// TestRFQComparison_DuplicateQuoteRowsResolveToTheLatest pins the
// tie-break for a case nothing in the data model forbids: two
// RequestForQuotationQuoteLine rows for the same (line, vendor). The
// assembled map is last-write-wins, so the query orders by created_at/id
// to make "the most recently recorded quote wins" a stable rule rather
// than whatever order Postgres happened to scan in.
func TestRFQComparison_DuplicateQuoteRowsResolveToTheLatest(t *testing.T) {
	ctx := context.Background()
	tenantDB := freshTenantDB(t)
	records := data.NewRecordRepo(tenantDB)
	reporting := data.NewReportingRepo(tenantDB)

	rfq, err := records.Create(ctx, "RequestForQuotation", map[string]any{
		"rfq_number": "RFQ-DUP", "due_date": "2026-08-15",
	})
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}
	item, err := records.Create(ctx, "Item", map[string]any{"sku": "SKU-DUP", "name": "Widget", "item_type": "stock"})
	if err != nil {
		t.Fatalf("create Item: %v", err)
	}
	line, err := records.Create(ctx, "RequestForQuotationLine", map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": item.ID, "qty": 3.0,
	})
	if err != nil {
		t.Fatalf("create line: %v", err)
	}
	vendor, err := records.Create(ctx, "Party", map[string]any{"name": "Vendor D", "party_type": "organization"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	if _, err := records.Create(ctx, "RequestForQuotationVendor", map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendor.ID,
	}); err != nil {
		t.Fatalf("invite vendor: %v", err)
	}
	for _, price := range []float64{5.0, 8.0} {
		if _, err := records.Create(ctx, "RequestForQuotationQuoteLine", map[string]any{
			"rfq_line_id": line.ID, "vendor_id": vendor.ID, "unit_price": price,
		}); err != nil {
			t.Fatalf("create quote line %v: %v", price, err)
		}
	}

	lines, vendors, err := reporting.RFQComparison(ctx, rfq.ID)
	if err != nil {
		t.Fatalf("RFQComparison: %v", err)
	}
	if len(lines) != 1 || len(vendors) != 1 {
		t.Fatalf("expected 1 line and 1 vendor, got %d/%d", len(lines), len(vendors))
	}
	if got := lines[0].QuotesByVendor[vendors[0].ID]; got != 8.0 {
		t.Errorf("quote = %v, want 8 (the later-created of the two rows)", got)
	}
}
