package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestAPI_MasterDetailMoneyChildCellIsRegionallyFormatted (uc-infra#166) is
// this card's headline behaviour at the HTTP layer: a FieldMoney column in
// a master-detail child row (PurchaseOrder's Lines section, POLine's
// line_total) now renders through the viewer's regional number format,
// matching internal/api/listview.go's own top-level FieldMoney cell
// (TestListPage_MoneyFieldDisplaysMajorUnitDecimal) instead of the
// ungrouped m.String() every prior release showed here. Reuses
// purchaseOrderWithMoneyTotalEntityDef/poLineMoneyEntityDef
// (masterdetail_rollup_roundtrip_test.go) rather than inventing a new
// money-typed master-detail fixture pair.
func TestAPI_MasterDetailMoneyChildCellIsRegionallyFormatted(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, tenantDB, purchaseOrderWithMoneyTotalEntityDef(), purchaseOrderWithMoneyTotalFormDef())
	publishEntityAndForm(t, tenantDB, poLineMoneyEntityDef(), poLineMoneyFormDef())

	// 123456789 minor units == $1,234,567.89 — large enough to require
	// grouping (distinguishing this test from one that happens to pass
	// under both grouped and ungrouped formatting), and the same worked
	// value TestListPage_MoneyFieldDisplaysMajorUnitDecimal already pins
	// for the top-level list page, so a mismatch here would mean the two
	// surfaces actually disagree, not just that this test's own math is
	// wrong.
	const lineTotalMinorUnits = 123456789
	engine := crud.NewEngine(tenantDB)
	po, err := engine.Create(ctx, purchaseOrderWithMoneyTotalEntityDef(), map[string]any{
		"vendor_id": "v1", "total": lineTotalMinorUnits,
	}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineMoneyEntityDef(), map[string]any{
		"purchase_order_id": po.ID, "line_total": lineTotalMinorUnits,
	}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	for name, tc := range map[string]struct {
		query   string
		wantAmt string
	}{
		"british default (no ?region=)": {"", "1,234,567.89"},
		"turkish":                       {"?lang=tr&region=tr-TR", "1.234.567,89"},
		"farsi":                         {"?lang=fa&region=fa-IR", "۱٬۲۳۴٬۵۶۷٫۸۹"},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest("GET", "/forms/PurchaseOrder/"+po.ID+tc.query, tenantID, "farshid", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /forms/PurchaseOrder/%s%s: expected 200, got %d: %s", po.ID, tc.query, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `data-field="line_total">`+tc.wantAmt+`<`) {
				t.Fatalf("expected the child cell to show the regionally formatted amount %q, got:\n%s", tc.wantAmt, excerpt(body))
			}
			if strings.Contains(body, `data-field="line_total">1234567.89<`) {
				t.Fatalf("ungrouped amount leaked into the child cell, got:\n%s", excerpt(body))
			}
			if strings.Contains(body, `data-field="line_total">123456789<`) {
				t.Fatalf("raw minor-units integer leaked into the child cell, got:\n%s", excerpt(body))
			}
			// The header's own money-typed roll-up total (this PO's single
			// line IS the whole total, purchaseOrderWithMoneyTotalFormDef's
			// RollUp/RollUpTarget) must show the SAME regionally formatted
			// amount as the child cell above it — the exact cells-vs-total
			// agreement uc-infra#166's independent review folded in
			// (formerly filed separately as uc-infra#189). Checked via
			// occurrence count rather than the full `<p class="uc-rollup">`
			// tag, since the roll-up LABEL text itself is locale-translated
			// ("Total"/"Toplam"/etc) and this assertion cares about the
			// AMOUNT, not the label.
			if got := strings.Count(body, tc.wantAmt); got < 2 {
				t.Fatalf("expected the regionally formatted amount %q to appear at least twice (child cell AND roll-up total), got %d occurrence(s) in:\n%s", tc.wantAmt, got, excerpt(body))
			}
		})
	}
}

// TestAPI_MasterDetailMoneyChildCellWholeAmountUsesFixedTwoDecimalPrecision
// (uc-infra#166 independent review, finding 1): the test above only uses
// amounts with a non-zero cents digit, under which a WRONG "natural
// precision" (-1) FormatNumber call would happen to print identically to
// the correct fixed-money.Decimals (2) call. A whole-dollar amount over
// 1000 is the case that actually distinguishes them: fixed precision
// always prints "1,234,567.00"; natural precision prints the shorter
// "1,234,567" with no trailing zeros.
func TestAPI_MasterDetailMoneyChildCellWholeAmountUsesFixedTwoDecimalPrecision(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, tenantDB, purchaseOrderWithMoneyTotalEntityDef(), purchaseOrderWithMoneyTotalFormDef())
	publishEntityAndForm(t, tenantDB, poLineMoneyEntityDef(), poLineMoneyFormDef())

	const lineTotalMinorUnits = 123456700 // $1,234,567.00 exactly.
	engine := crud.NewEngine(tenantDB)
	po, err := engine.Create(ctx, purchaseOrderWithMoneyTotalEntityDef(), map[string]any{
		"vendor_id": "v1", "total": lineTotalMinorUnits,
	}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineMoneyEntityDef(), map[string]any{
		"purchase_order_id": po.ID, "line_total": lineTotalMinorUnits,
	}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	req := newRequest("GET", "/forms/PurchaseOrder/"+po.ID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forms/PurchaseOrder/%s: expected 200, got %d: %s", po.ID, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-field="line_total">1,234,567.00<`) {
		t.Fatalf("expected the fixed-precision amount 1,234,567.00, got:\n%s", excerpt(body))
	}
	if strings.Contains(body, `data-field="line_total">1,234,567<`) {
		t.Fatalf("natural-precision amount (missing .00) leaked into the child cell, got:\n%s", excerpt(body))
	}
}
