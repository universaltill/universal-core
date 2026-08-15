package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/money"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// rfqStatusID resolves a rfq_status Status record's id by code — the
// same two-step status_type_id-scoped lookup internal/e2e's
// publishedStatusID performs (status codes are unique per StatusType,
// not globally), duplicated here rather than imported since
// internal/e2e and internal/api are independent test packages with no
// shared test-helper package between them.
func rfqStatusID(t *testing.T, db *sql.DB, statusCode string) string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	types, err := engine.ListByField(ctx, foundation.StatusType(), "code", "rfq_status")
	if err != nil || len(types) != 1 {
		t.Fatalf("lookup StatusType rfq_status: %v (n=%d)", err, len(types))
	}
	statuses, err := engine.ListByField(ctx, foundation.Status(), "status_type_id", types[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	for _, s := range statuses {
		if code, _ := s.Data["code"].(string); code == statusCode {
			return s.ID
		}
	}
	t.Fatalf("no rfq_status Status with code %q", statusCode)
	return ""
}

// setupRFQTenant publishes foundation + purchasing (entities, forms,
// statuses) into an already-provisioned tenant and seeds one RFQ with
// two lines (Widget A qty 10, Widget B qty 5) and two invited vendors
// (Vendor X, Vendor Y). Returns the RFQ id and the two line/vendor id
// pairs so a caller can layer quote lines on top.
func setupRFQTenant(t *testing.T, db *sql.DB) (rfqID string, lineAID, lineBID, vendorXID, vendorYID string) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(db)
	draftID := rfqStatusID(t, db, "draft")

	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-API-1", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}

	itemA, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-API-A", "name": "Widget A", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item A: %v", err)
	}
	itemB, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-API-B", "name": "Widget B", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("create Item B: %v", err)
	}

	lineA, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemA.ID, "qty": 10.0,
	}, actor)
	if err != nil {
		t.Fatalf("create line A: %v", err)
	}
	lineB, err := engine.Create(ctx, purchasing.RequestForQuotationLine(), map[string]any{
		"request_for_quotation_id": rfq.ID, "item_id": itemB.ID, "qty": 5.0,
	}, actor)
	if err != nil {
		t.Fatalf("create line B: %v", err)
	}

	vendorX, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Vendor X", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create vendor X: %v", err)
	}
	vendorY, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Vendor Y", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create vendor Y: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorX.ID,
	}, actor); err != nil {
		t.Fatalf("invite vendor X: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationVendor(), map[string]any{
		"request_for_quotation_id": rfq.ID, "vendor_id": vendorY.ID,
	}, actor); err != nil {
		t.Fatalf("invite vendor Y: %v", err)
	}

	return rfq.ID, lineA.ID, lineB.ID, vendorX.ID, vendorY.ID
}

// TestRFQComparisonReport_RendersGridWithCheapestMarked (#9) is the core
// proof: two vendors quote the same line at different prices, and the
// rendered page must carry both prices, both vendor names, and the
// cheapest one marked with the .uc-rfq-lowest class this test asserts
// against directly (a real headless-browser DOM/style assertion is
// internal/e2e/rfq_report_test.go's job — CLAUDE.md is explicit that a
// rendered-HTML-string test never substitutes for that at the page
// level, but asserting the handler emitted the right markup at all is
// still this layer's job, same division as every other report test in
// this package).
func TestRFQComparisonReport_RendersGridWithCheapestMarked(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, lineAID, lineBID, vendorXID, vendorYID := setupRFQTenant(t, db)
	ctx := context.Background()
	actor := humanActor()
	engine := crud.NewEngine(db)

	// lineA: vendor X cheaper (9.50 < 10.25). lineB: only vendor X quoted
	// — the deliberate missing-quote gap for vendor Y. unit_price is
	// FieldMoney now (minor units, uc-infra#68): 950 == $9.50.
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineAID, "vendor_id": vendorXID, "unit_price": 950,
	}, actor); err != nil {
		t.Fatalf("quote lineA/vendorX: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineAID, "vendor_id": vendorYID, "unit_price": 1025,
	}, actor); err != nil {
		t.Fatalf("quote lineA/vendorY: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineBID, "vendor_id": vendorXID, "unit_price": 400,
	}, actor); err != nil {
		t.Fatalf("quote lineB/vendorX: %v", err)
	}

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Widget A", "Widget B", "Vendor X", "Vendor Y",
		"9.50", "10.25", "4.00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q:\n%s", want, body)
		}
	}
	// The cheapest price on lineA (9.50, vendor X) must be marked; the
	// more expensive one (10.25, vendor Y) must not carry that class on
	// its own cell. A crude but adequate proxy at this layer: the marker
	// class appears exactly once (lineA's one cheapest cell — lineB has
	// only one quote at all, so nothing to be "cheaper" than there is
	// still marked lowest by the same rule, giving two occurrences
	// total).
	if got := strings.Count(body, `class="uc-rfq-lowest"`); got != 2 {
		t.Errorf("expected 2 uc-rfq-lowest markers (lineA's cheapest cell + lineB's sole quote), got %d:\n%s", got, body)
	}
	// vendor X's footer total: 9.50 + 4.00 = 13.50. vendor Y's footer
	// total: 10.25 only (never quoted lineB). Exact int64 minor-unit
	// addition (uc-infra#68) — no float ever enters this sum.
	if !strings.Contains(body, "13.50") {
		t.Errorf("report body missing vendor X's footer total 13.50:\n%s", body)
	}
}

// TestRFQComparisonReport_NoQuotesYetRendersBlanksNotErrors: an RFQ with
// real lines and invited vendors but zero recorded quotes still renders
// 200 with every cell blank ("—"), never an error — the whole point of
// QuotesByVendor being a sparse map (data.RFQComparison's own doc
// comment).
func TestRFQComparisonReport_NoQuotesYetRendersBlanksNotErrors(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, _, _, _, _ := setupRFQTenant(t, db)

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Widget A", "Widget B", "Vendor X", "Vendor Y"} {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q:\n%s", want, body)
		}
	}
	// Every one of the 4 (line, vendor) cells is unquoted, plus both
	// footer cells — 6 missing-quote placeholders total, no marked
	// "lowest" cell anywhere (nothing to compare).
	if got := strings.Count(body, ">—<"); got != 6 {
		t.Errorf("expected 6 missing-quote placeholders, got %d:\n%s", got, body)
	}
	if strings.Contains(body, `class="uc-rfq-lowest"`) {
		t.Errorf("expected no lowest-price marker with zero quotes:\n%s", body)
	}
}

// TestRFQComparisonReport_RBACDenied mirrors the purchasing report's own
// RBAC test (authz_rbac_test.go/saftexport_test.go): once any Permission
// row exists for one of the entity types this report gates on
// (rfqReportEntityTypes), an actor without that specific grant gets a
// 403 for the whole page, not a partial/degraded render.
func TestRFQComparisonReport_RBACDenied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, _, _, _, _ := setupRFQTenant(t, db)

	// Close "Party" (one of rfqReportEntityTypes' join targets, for the
	// vendor names) to a role the requesting clerk does not hold.
	seedRBAC(t, db,
		map[string][]string{"procurement": {"user-procurement"}},
		[]map[string]any{{"role": "procurement", "entity_type": "Party", "can_read": true}})

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor without Party read, got %d: %s", rec.Code, rec.Body.String())
	}

	// The granted actor still gets the page.
	req = newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "user-procurement", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for the granted actor, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRFQComparisonReport_EmptyStateWhenNoLinesOrVendors confirms the
// empty-state message renders (not an empty <table>) for an RFQ with
// neither lines nor invited vendors yet.
func TestRFQComparisonReport_EmptyStateWhenNoLinesOrVendors(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(db)
	draftID := rfqStatusID(t, db, "draft")
	rfq, err := engine.Create(ctx, purchasing.RequestForQuotation(), map[string]any{
		"rfq_number": "RFQ-EMPTY-API", "due_date": "2026-08-20", "status_id": draftID,
	}, actor)
	if err != nil {
		t.Fatalf("create RequestForQuotation: %v", err)
	}

	req := newRequest("GET", "/reports/rfq/"+rfq.ID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<table") {
		t.Errorf("expected no table for an RFQ with no lines/vendors, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-empty"`) {
		t.Errorf("expected the empty-state message, got:\n%s", body)
	}
}

// TestRFQComparisonReport_NotFound confirms an unknown RFQ id 404s
// rather than rendering a broken/empty page.
func TestRFQComparisonReport_NotFound(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}

	req := newRequest("GET", "/reports/rfq/00000000-0000-0000-0000-000000000000", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown RFQ id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRFQComparisonReport_TiedLowestPriceMarksBoth (independent review,
// 2026-07-31): two vendors quoting the SAME lowest price on a line is a
// real, ordinary outcome, and the "cheapest" mark must go on both — not
// on whichever vendor's column happens to be iterated first, and
// certainly not on neither. buildRFQReportView's two-pass shape (find
// the minimum, then mark every cell equal to it) is what makes that
// true; this pins it so a future "single winner" refactor can't silently
// start picking an arbitrary one.
func TestRFQComparisonReport_TiedLowestPriceMarksBoth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, lineAID, lineBID, vendorXID, vendorYID := setupRFQTenant(t, db)
	ctx := context.Background()
	actor := humanActor()
	engine := crud.NewEngine(db)

	// lineA: both vendors quote exactly 750 ($7.50) — the tie. lineB:
	// nobody quoted, so no mark there at all.
	for _, vendorID := range []string{vendorXID, vendorYID} {
		if _, err := engine.Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
			"rfq_line_id": lineAID, "vendor_id": vendorID, "unit_price": 750,
		}, actor); err != nil {
			t.Fatalf("quote lineA/%s: %v", vendorID, err)
		}
	}
	_ = lineBID

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, `class="uc-rfq-lowest"`); got != 2 {
		t.Errorf("expected BOTH tied cells marked lowest (2 markers), got %d:\n%s", got, body)
	}
}

// TestBuildRFQReportView_EdgeCasePrices covers the arithmetic edges the
// HTTP-level tests can't cheaply reach: a legitimately quoted 0.00 (a
// free/waived line — Present, and genuinely the lowest, unlike a MISSING
// quote which is neither), a negative price (a credit/rebate line), and
// a row nobody quoted at all (no mark, no footer contribution).
//
// QuotesByVendor is money.Money (minor units, uc-infra#68) — cell.Value
// is the major-unit decimal string money.Money.String() produces, not
// the raw minor-units integer.
func TestBuildRFQReportView_EdgeCasePrices(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}
	vendors := []data.RFQComparisonVendor{{ID: "v1", Name: "A"}, {ID: "v2", Name: "B"}}
	lines := []data.RFQComparisonLine{
		{ID: "l1", ItemName: "Free", Qty: 1, QuotesByVendor: map[string]money.Money{"v1": 0, "v2": 300}},
		{ID: "l2", ItemName: "Rebate", Qty: 1, QuotesByVendor: map[string]money.Money{"v1": -200, "v2": 300}},
		{ID: "l3", ItemName: "Unquoted", Qty: 1, QuotesByVendor: nil},
	}
	view := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en", false, false, false, false)

	if len(view.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(view.Rows))
	}
	// A real 0.00 quote is Present and lowest — it must not be confused
	// with the missing-quote blank.
	if c := view.Rows[0].Cells[0]; !c.Present || !c.Lowest || c.Value != "0.00" {
		t.Errorf("row 0 vendor A = %+v, want a present, lowest 0.00", c)
	}
	if c := view.Rows[1].Cells[0]; !c.Present || !c.Lowest || c.Value != "-2.00" {
		t.Errorf("row 1 vendor A = %+v, want a present, lowest -2.00", c)
	}
	for i, c := range view.Rows[2].Cells {
		if c.Present || c.Lowest {
			t.Errorf("row 2 cell %d = %+v, want absent and unmarked (nobody quoted)", i, c)
		}
	}
	// Footer: A = 0 + -200 = -200 ($-2.00, both real quotes), B = 300 +
	// 300 = 600 ($6.00) — exact int64 minor-unit addition.
	if view.FooterCells[0].Value != "-2.00" || !view.FooterCells[0].Present {
		t.Errorf("vendor A footer = %+v, want -2.00", view.FooterCells[0])
	}
	if view.FooterCells[1].Value != "6.00" || !view.FooterCells[1].Present {
		t.Errorf("vendor B footer = %+v, want 6.00", view.FooterCells[1])
	}
}

// TestBuildRFQReportView_ItemAndVendorNameRedaction (uc-infra#234,
// independent review) pins itemNameHidden/vendorNameHidden's effect
// directly at the buildRFQReportView layer, not just transitively
// through rendered HTML — the api-level HTTP test
// (TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable)
// already proves the real values never reach the page; this proves the
// view MODEL itself carries the redaction (ItemAvailable/NameAvailable
// false, the string fields left blank, never just "hidden by the
// template"), and that price/lowest-mark/footer data is completely
// unaffected by either flag.
func TestBuildRFQReportView_ItemAndVendorNameRedaction(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}
	vendors := []data.RFQComparisonVendor{{ID: "v1", Name: "Vendor A"}}
	lines := []data.RFQComparisonLine{
		{ID: "l1", ItemName: "Widget", Qty: 1, QuotesByVendor: map[string]money.Money{"v1": 500}},
	}

	view := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en", true, true, false, false)

	if view.Rows[0].ItemAvailable || view.Rows[0].Item != "" {
		t.Errorf("itemNameHidden=true: row = %+v, want ItemAvailable=false and Item=\"\"", view.Rows[0])
	}
	if view.Vendors[0].NameAvailable || view.Vendors[0].Name != "" {
		t.Errorf("vendorNameHidden=true: vendor col = %+v, want NameAvailable=false and Name=\"\"", view.Vendors[0])
	}
	// A different field (unit_price) — completely unaffected by either
	// redaction flag.
	if c := view.Rows[0].Cells[0]; !c.Present || !c.Lowest || c.Value != "5.00" {
		t.Errorf("price cell = %+v, want a present, lowest 5.00 regardless of name redaction", c)
	}
	if view.FooterCells[0].Value != "5.00" || !view.FooterCells[0].Present {
		t.Errorf("footer = %+v, want 5.00 regardless of name redaction", view.FooterCells[0])
	}

	// Unrestricted: both real values carry through.
	openView := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en", false, false, false, false)
	if !openView.Rows[0].ItemAvailable || openView.Rows[0].Item != "Widget" {
		t.Errorf("itemNameHidden=false: row = %+v, want ItemAvailable=true and Item=\"Widget\"", openView.Rows[0])
	}
	if !openView.Vendors[0].NameAvailable || openView.Vendors[0].Name != "Vendor A" {
		t.Errorf("vendorNameHidden=false: vendor col = %+v, want NameAvailable=true and Name=\"Vendor A\"", openView.Vendors[0])
	}
}

// TestBuildRFQReportView_QtyAndUnitPriceRedaction (uc-infra#235) is this
// file's own version of TestBuildRFQReportView_ItemAndVendorNameRedaction
// for the two fields #234 deliberately left open: qtyRedacted must blank
// only Qty/QtyAvailable, and unitPriceRedacted must blank every price
// cell AND the derived .uc-rfq-lowest mark AND the footer total — not
// just the cell text — while leaving row/column membership and the
// item/vendor names untouched.
func TestBuildRFQReportView_QtyAndUnitPriceRedaction(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}
	vendors := []data.RFQComparisonVendor{{ID: "v1", Name: "Vendor A"}, {ID: "v2", Name: "Vendor B"}}
	lines := []data.RFQComparisonLine{
		{ID: "l1", ItemName: "Widget", Qty: 3, QuotesByVendor: map[string]money.Money{"v1": 500, "v2": 300}},
	}

	// Both redacted: no real qty, no real price, no lowest mark, no
	// footer total — but the row/column shape (1 row x 2 vendors) is
	// untouched, and each redacted cell is Hidden (a real quote exists),
	// never Present, never a bare Missing.
	view := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en", false, false, true, true)
	if len(view.Rows) != 1 || len(view.Rows[0].Cells) != 2 || len(view.Vendors) != 2 {
		t.Fatalf("expected 1 row x 2 vendors regardless of redaction, got %d rows, %d vendors", len(view.Rows), len(view.Vendors))
	}
	if view.Rows[0].QtyAvailable || view.Rows[0].Qty != "" {
		t.Errorf("qtyRedacted=true: row = %+v, want QtyAvailable=false and Qty=\"\"", view.Rows[0])
	}
	for i, c := range view.Rows[0].Cells {
		if c.Present || !c.Hidden || c.Lowest || c.Value != "" {
			t.Errorf("unitPriceRedacted=true: cell %d = %+v, want Hidden=true, Present=false, Lowest=false, Value=\"\"", i, c)
		}
	}
	for i, c := range view.FooterCells {
		if c.Present || !c.Hidden || c.Value != "" {
			t.Errorf("unitPriceRedacted=true: footer cell %d = %+v, want Hidden=true, Present=false, Value=\"\"", i, c)
		}
	}
	// Name fields are a different field entirely — unaffected.
	if !view.Rows[0].ItemAvailable || view.Rows[0].Item != "Widget" {
		t.Errorf("qty/price redaction must not touch Item: row = %+v", view.Rows[0])
	}
	if !view.Vendors[0].NameAvailable || view.Vendors[0].Name != "Vendor A" {
		t.Errorf("qty/price redaction must not touch vendor Name: vendor = %+v", view.Vendors[0])
	}

	// Unrestricted: real qty, real prices, the cheaper cell (v2, 3.00)
	// marked lowest, real footer totals.
	openView := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en", false, false, false, false)
	if !openView.Rows[0].QtyAvailable || openView.Rows[0].Qty != "3" {
		t.Errorf("qtyRedacted=false: row = %+v, want QtyAvailable=true and Qty=\"3\"", openView.Rows[0])
	}
	if c := openView.Rows[0].Cells[1]; !c.Present || c.Hidden || !c.Lowest || c.Value != "3.00" {
		t.Errorf("unitPriceRedacted=false: cheaper cell = %+v, want Present=true, Hidden=false, Lowest=true, Value=\"3.00\"", c)
	}
	if c := openView.FooterCells[0]; !c.Present || c.Hidden || c.Value != "5.00" {
		t.Errorf("unitPriceRedacted=false: vendor A footer = %+v, want Present=true, Value=\"5.00\"", c)
	}

	// A row nobody quoted at all stays a genuine Missing ("—"), never
	// Hidden, even when unitPriceRedacted — Hidden means "a real quote
	// exists but you can't see it", which is false here.
	unquoted := []data.RFQComparisonLine{{ID: "l2", ItemName: "Never Quoted", Qty: 1, QuotesByVendor: nil}}
	unquotedView := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, unquoted, vendors, "en", false, false, false, true)
	for i, c := range unquotedView.Rows[0].Cells {
		if c.Hidden || c.Present {
			t.Errorf("nobody quoted: cell %d = %+v, want neither Hidden nor Present (a plain Missing)", i, c)
		}
	}
	for i, c := range unquotedView.FooterCells {
		if c.Hidden || c.Present {
			t.Errorf("nobody quoted: footer cell %d = %+v, want neither Hidden nor Present", i, c)
		}
	}
}

// TestRFQLineQtyRedacted_TestRFQQuoteLineUnitPriceRedacted (uc-infra#235,
// independent review) pins both predicates' full truth tables directly,
// at the smallest possible unit — mirroring reporting.go's own
// TestPurchaseOrderTotalRedacted_.../TestInventoryItemOnHandRedacted_...
// tests for the equivalent predicates on other entities. This is the
// guard against a typo in either field-name literal silently disabling
// the whole fix: a bare inline map lookup at each call site would have
// no unit-level test of its own, only the slower, coarser HTTP-level
// regression test to catch a mistake here — and that HTTP test seeds its
// FieldPermission rows against the exact same literal, so a typo shared
// by both sides would go green there too.
func TestRFQLineQtyRedacted_TestRFQQuoteLineUnitPriceRedacted(t *testing.T) {
	cases := []struct {
		name          string
		hidden        map[string]bool
		wantQty       bool
		wantUnitPrice bool
	}{
		{"nothing hidden", map[string]bool{}, false, false},
		{"nil map", nil, false, false},
		{"qty hidden", map[string]bool{"qty": true}, true, false},
		{"unit_price hidden", map[string]bool{"unit_price": true}, false, true},
		{"both hidden", map[string]bool{"qty": true, "unit_price": true}, true, true},
		{"unrelated field hidden", map[string]bool{"item_id": true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rfqLineQtyRedacted(tc.hidden); got != tc.wantQty {
				t.Errorf("rfqLineQtyRedacted(%v) = %v, want %v", tc.hidden, got, tc.wantQty)
			}
			if got := rfqQuoteLineUnitPriceRedacted(tc.hidden); got != tc.wantUnitPrice {
				t.Errorf("rfqQuoteLineUnitPriceRedacted(%v) = %v, want %v", tc.hidden, got, tc.wantUnitPrice)
			}
		})
	}
}

// TestRFQComparisonReport_RBACDeniedOnQuoteLineAlone is the sharpest
// version of the gate test (independent review, 2026-07-31): the
// existing RBAC test closes "Party", which the report needs for vendor
// NAMES — but the genuinely sensitive data on this page is the quoted
// PRICES, which live only on RequestForQuotationQuoteLine. An actor
// granted read on everything else and denied exactly that one type must
// still be refused the whole page; if rfqReportEntityTypes ever drifts
// out of sync with what RFQComparison actually queries, this is the test
// that catches it.
func TestRFQComparisonReport_RBACDeniedOnQuoteLineAlone(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, _, _, _, _ := setupRFQTenant(t, db)

	// The clerk gets read on every rfqReportEntityTypes member EXCEPT
	// RequestForQuotationQuoteLine; the procurement role gets that one.
	perms := []map[string]any{}
	for _, entityType := range []string{
		"RequestForQuotation", "RequestForQuotationLine",
		"RequestForQuotationVendor", "Item", "Party", "Status",
	} {
		perms = append(perms, map[string]any{"role": "clerk", "entity_type": entityType, "can_read": true})
	}
	perms = append(perms, map[string]any{
		"role": "procurement", "entity_type": "RequestForQuotationQuoteLine", "can_read": true,
	})
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}, "procurement": {"user-procurement"}},
		perms)

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor denied RequestForQuotationQuoteLine read (quoted prices are exactly what this report exposes), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable
// (uc-infra#234) is this report's own version of reporting.go's
// TestAPI_PurchasingReport_HiddenPurchaseOrderPartyItemFieldsRenderNotAvailable
// (uc-infra#230/#233): Item.name (the grid's row label) and Party.name
// (the vendor column header) are both read straight out of
// data.ReportingRepo.RFQComparison's raw SQL, entirely outside the
// ts.crud-redacted path a form/CRUD page would otherwise apply — the
// same FieldPermission-bypass shape, now confirmed on a third report
// family. Neither field drives row/column MEMBERSHIP here: every
// requested line still gets a row and every invited vendor still gets a
// column, redacted or not — only the label text changes, mirroring the
// "gate structure only when the hidden value itself drives which rows
// appear" distinction uc-infra#230's own fix had to learn (Item.name/
// Party.name are pure display fields here, unlike qty_on_hand there).
func TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, lineAID, _, vendorXID, _ := setupRFQTenant(t, db)
	// A real quote line (uc-infra#234 independent review): redacting
	// Item.name/Party.name must leave the price grid itself untouched —
	// with setupRFQTenant's fixture alone (zero quotes) every cell would
	// render the missing-quote blank for every actor regardless of
	// redaction, which would prove nothing about that claim.
	if _, err := crud.NewEngine(db).Create(context.Background(), purchasing.RequestForQuotationQuoteLine(),
		map[string]any{"rfq_line_id": lineAID, "vendor_id": vendorXID, "unit_price": 950},
		humanActor()); err != nil {
		t.Fatalf("seed quote line: %v", err)
	}

	seedFieldRule(t, db, "itemname-restricted", "user-itemname-hidden", "Item", "name")
	seedFieldRule(t, db, "partyname-restricted", "user-partyname-hidden", "Party", "name")
	// One role with FieldPermission rows on BOTH fields, not two
	// single-field roles granted to the same user: authz.Resolver.
	// HiddenFields' own contract is a field is hidden only when EVERY
	// role the actor holds has a hidden=true row for it — see
	// reporting_test.go's own identical comment on this exact pitfall.
	allRoleID := seedFieldRule(t, db, "all-restricted", "user-all-hidden", "Item", "name")
	if _, err := crud.NewEngine(db).Create(context.Background(), foundation.FieldPermission(),
		map[string]any{"role_id": allRoleID, "entity_type": "Party", "field_name": "name", "hidden": true},
		humanActor()); err != nil {
		t.Fatalf("create second FieldPermission: %v", err)
	}

	get := func(actorID string) string {
		t.Helper()
		req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, actorID, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("actor %s: expected 200, got %d: %s", actorID, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Unrestricted actor: both real labels render, NotAvailable appears
	// nowhere.
	openBody := get("farshid")
	for _, want := range []string{"Widget A", "Widget B", "Vendor X", "Vendor Y"} {
		if !strings.Contains(openBody, want) {
			t.Errorf("unrestricted actor: expected %q in body:\n%s", want, openBody)
		}
	}
	if strings.Contains(openBody, "Not available") {
		t.Errorf("unrestricted actor: NotAvailable must not appear:\n%s", openBody)
	}
	if !strings.Contains(openBody, "9.50") {
		t.Errorf("unrestricted actor: expected the real quoted price 9.50:\n%s", openBody)
	}

	// Item.name hidden: both row labels blank (2 rows -> 2 NotAvailable
	// cells); vendor column headers stay real.
	itemBody := get("user-itemname-hidden")
	for _, leaked := range []string{"Widget A", "Widget B"} {
		if strings.Contains(itemBody, leaked) {
			t.Errorf("item-name-restricted actor: %q must not appear anywhere:\n%s", leaked, itemBody)
		}
	}
	for _, want := range []string{"Vendor X", "Vendor Y"} {
		if !strings.Contains(itemBody, want) {
			t.Errorf("item-name-restricted actor: expected %q in body:\n%s", want, itemBody)
		}
	}
	if got := strings.Count(itemBody, "Not available"); got != 2 {
		t.Errorf("item-name-restricted actor: expected 2 NotAvailable cells (one per row), got %d:\n%s", got, itemBody)
	}
	if !strings.Contains(itemBody, "9.50") {
		t.Errorf("item-name-restricted actor: expected the real quoted price 9.50 (a different field) to still render:\n%s", itemBody)
	}

	// Party.name hidden: both vendor column headers blank (2 vendors ->
	// 2 NotAvailable cells); row labels stay real.
	vendorBody := get("user-partyname-hidden")
	for _, leaked := range []string{"Vendor X", "Vendor Y"} {
		if strings.Contains(vendorBody, leaked) {
			t.Errorf("party-name-restricted actor: %q must not appear anywhere:\n%s", leaked, vendorBody)
		}
	}
	for _, want := range []string{"Widget A", "Widget B"} {
		if !strings.Contains(vendorBody, want) {
			t.Errorf("party-name-restricted actor: expected %q in body:\n%s", want, vendorBody)
		}
	}
	if got := strings.Count(vendorBody, "Not available"); got != 2 {
		t.Errorf("party-name-restricted actor: expected 2 NotAvailable cells (one per vendor), got %d:\n%s", got, vendorBody)
	}
	if !strings.Contains(vendorBody, "9.50") {
		t.Errorf("party-name-restricted actor: expected the real quoted price 9.50 (a different field) to still render:\n%s", vendorBody)
	}

	// Both hidden: neither real value appears anywhere, and the grid
	// keeps its full 2-row x 2-vendor shape (row/column membership never
	// depended on either field) -> 4 NotAvailable cells total.
	allBody := get("user-all-hidden")
	for _, leaked := range []string{"Widget A", "Widget B", "Vendor X", "Vendor Y"} {
		if strings.Contains(allBody, leaked) {
			t.Errorf("all-restricted actor: %q must not appear anywhere:\n%s", leaked, allBody)
		}
	}
	if !strings.Contains(allBody, "9.50") {
		t.Errorf("all-restricted actor: expected the real quoted price 9.50 (a different field) to still render:\n%s", allBody)
	}
	if got := strings.Count(allBody, "Not available"); got != 4 {
		t.Errorf("all-restricted actor: expected 4 NotAvailable cells (2 rows + 2 vendor headers), got %d:\n%s", got, allBody)
	}
}

// TestRFQComparisonReport_HiddenQtyUnitPriceFieldsRenderNotAvailable
// (uc-infra#235) is this report's own version of
// TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable for the
// two fields #234 deliberately left open: RequestForQuotationLine.qty and
// RequestForQuotationQuoteLine.unit_price. unit_price is the sharper
// case — it also has to take the .uc-rfq-lowest mark and the footer
// totals down with it, not just its own cell text, since both are
// derived from the same hidden value.
func TestRFQComparisonReport_HiddenQtyUnitPriceFieldsRenderNotAvailable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, lineAID, lineBID, vendorXID, vendorYID := setupRFQTenant(t, db)
	// lineA: BOTH vendors quote (vendorX cheaper at 9.50) — the symmetric
	// case, and where the lowest-price mark lives. lineB: only vendorX
	// quotes (4.00) — the ASYMMETRIC case (independent review,
	// uc-infra#235): vendorY never quoted lineB at all, so vendorY's
	// lineB cell must render a plain Missing ("—") under every actor
	// below, redacted or not, while vendorX's lineB cell — a real quote
	// that exists — must render Hidden (NotAvailable) for a price-
	// restricted actor. Without this asymmetric fixture, an
	// implementation that hoisted the Hidden decision to per-LINE rather
	// than per-CELL (redacting every vendor's cell on any line ANY vendor
	// quoted) would pass every assertion here by coincidence.
	engine := crud.NewEngine(db)
	if _, err := engine.Create(context.Background(), purchasing.RequestForQuotationQuoteLine(),
		map[string]any{"rfq_line_id": lineAID, "vendor_id": vendorXID, "unit_price": 950}, humanActor()); err != nil {
		t.Fatalf("seed quote lineA/vendorX: %v", err)
	}
	if _, err := engine.Create(context.Background(), purchasing.RequestForQuotationQuoteLine(),
		map[string]any{"rfq_line_id": lineAID, "vendor_id": vendorYID, "unit_price": 1200}, humanActor()); err != nil {
		t.Fatalf("seed quote lineA/vendorY: %v", err)
	}
	if _, err := engine.Create(context.Background(), purchasing.RequestForQuotationQuoteLine(),
		map[string]any{"rfq_line_id": lineBID, "vendor_id": vendorXID, "unit_price": 400}, humanActor()); err != nil {
		t.Fatalf("seed quote lineB/vendorX: %v", err)
	}

	seedFieldRule(t, db, "qty-restricted", "user-qty-hidden", "RequestForQuotationLine", "qty")
	seedFieldRule(t, db, "price-restricted", "user-price-hidden", "RequestForQuotationQuoteLine", "unit_price")
	// One role with FieldPermission rows on BOTH fields, same
	// "HiddenFields is a union of per-role agreement, not per-row" pitfall
	// TestRFQComparisonReport_HiddenItemPartyFieldsRenderNotAvailable's own
	// comment already documents for Item.name/Party.name.
	allRoleID := seedFieldRule(t, db, "all-qty-price-restricted", "user-all-qty-price-hidden", "RequestForQuotationLine", "qty")
	if _, err := engine.Create(context.Background(), foundation.FieldPermission(),
		map[string]any{"role_id": allRoleID, "entity_type": "RequestForQuotationQuoteLine", "field_name": "unit_price", "hidden": true},
		humanActor()); err != nil {
		t.Fatalf("create second FieldPermission: %v", err)
	}

	get := func(actorID string) string {
		t.Helper()
		req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, actorID, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("actor %s: expected 200, got %d: %s", actorID, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Unrestricted actor: real qty (10/5), real prices (including lineB's
	// sole 4.00 quote), vendorX's 9.50 AND 4.00 both marked cheapest (each
	// is the only/lowest present quote on its own row), real footer totals
	// — NotAvailable appears nowhere.
	openBody := get("farshid")
	for _, want := range []string{">10<", ">5<", "9.50", "12.00", "4.00", "13.50"} {
		if !strings.Contains(openBody, want) {
			t.Errorf("unrestricted actor: expected %q in body:\n%s", want, openBody)
		}
	}
	if got := strings.Count(openBody, `class="uc-rfq-lowest"`); got != 2 {
		t.Errorf("unrestricted actor: expected 2 lowest-price marks (lineA/vendorX, lineB/vendorX), got %d:\n%s", got, openBody)
	}
	if strings.Contains(openBody, "Not available") {
		t.Errorf("unrestricted actor: NotAvailable must not appear:\n%s", openBody)
	}

	// qty hidden: both rows' Qty cell blanked (2 NotAvailable); prices,
	// lowest marks, and footer totals are a different field entirely and
	// stay real.
	qtyBody := get("user-qty-hidden")
	for _, leaked := range []string{">10<", ">5<"} {
		if strings.Contains(qtyBody, leaked) {
			t.Errorf("qty-restricted actor: %q must not appear anywhere:\n%s", leaked, qtyBody)
		}
	}
	if got := strings.Count(qtyBody, "Not available"); got != 2 {
		t.Errorf("qty-restricted actor: expected 2 NotAvailable cells (one per row), got %d:\n%s", got, qtyBody)
	}
	for _, want := range []string{"9.50", "12.00", "4.00"} {
		if !strings.Contains(qtyBody, want) {
			t.Errorf("qty-restricted actor: expected %q (a different field) to still render:\n%s", want, qtyBody)
		}
	}
	if got := strings.Count(qtyBody, `class="uc-rfq-lowest"`); got != 2 {
		t.Errorf("qty-restricted actor: expected 2 lowest-price marks (a different field), got %d:\n%s", got, qtyBody)
	}

	// unit_price hidden: lineA's 2 quoted cells blanked, lineB's ONE
	// quoted cell (vendorX) blanked too — 3 Hidden cells — plus BOTH
	// vendors' footer totals (each quoted at least once) — 5 NotAvailable
	// total. lineB's vendorY cell, who genuinely never quoted THIS line
	// (independent review, uc-infra#235 — the asymmetric case), still
	// renders its plain Missing ("—"), proving Hidden is decided per
	// CELL, not per line/vendor. No lowest mark anywhere. Qty is a
	// different field and stays real.
	priceBody := get("user-price-hidden")
	for _, leaked := range []string{"9.50", "12.00", "4.00"} {
		if strings.Contains(priceBody, leaked) {
			t.Errorf("price-restricted actor: %q must not appear anywhere:\n%s", leaked, priceBody)
		}
	}
	if got := strings.Count(priceBody, "Not available"); got != 5 {
		t.Errorf("price-restricted actor: expected 5 NotAvailable cells (2 lineA + 1 lineB/vendorX + 2 footer), got %d:\n%s", got, priceBody)
	}
	if got := strings.Count(priceBody, "—"); got != 1 {
		t.Errorf("price-restricted actor: expected exactly 1 plain Missing cell (lineB/vendorY, who never quoted at all), got %d:\n%s", got, priceBody)
	}
	if strings.Contains(priceBody, `class="uc-rfq-lowest"`) {
		t.Errorf("price-restricted actor: no lowest-price mark must render (it's derived from the hidden price):\n%s", priceBody)
	}
	for _, want := range []string{">10<", ">5<"} {
		if !strings.Contains(priceBody, want) {
			t.Errorf("price-restricted actor: expected %q (a different field) to still render:\n%s", want, priceBody)
		}
	}

	// Both hidden: 2 (qty) + 5 (unit_price, same breakdown as above) = 7
	// NotAvailable, still exactly 1 plain Missing (lineB/vendorY), no
	// lowest mark, neither qty nor price value anywhere.
	allBody := get("user-all-qty-price-hidden")
	for _, leaked := range []string{">10<", ">5<", "9.50", "12.00", "4.00"} {
		if strings.Contains(allBody, leaked) {
			t.Errorf("all-restricted actor: %q must not appear anywhere:\n%s", leaked, allBody)
		}
	}
	if got := strings.Count(allBody, "Not available"); got != 7 {
		t.Errorf("all-restricted actor: expected 7 NotAvailable cells, got %d:\n%s", got, allBody)
	}
	if got := strings.Count(allBody, "—"); got != 1 {
		t.Errorf("all-restricted actor: expected exactly 1 plain Missing cell (lineB/vendorY), got %d:\n%s", got, allBody)
	}
	if strings.Contains(allBody, `class="uc-rfq-lowest"`) {
		t.Errorf("all-restricted actor: no lowest-price mark must render:\n%s", allBody)
	}
}

// TestRFQComparisonReport_WritesNothing pins the "#9 is informational
// only" non-goal as an executable fact rather than a doc comment: GET
// /reports/rfq/{id} must not add a single audit_log row (CLAUDE.md's
// audit rule is "every mutation writes an audit row", so a report that
// wrote anything would show up here) nor a single record.
func TestRFQComparisonReport_WritesNothing(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()

	rfqID, lineAID, _, vendorXID, _ := setupRFQTenant(t, db)
	if _, err := crud.NewEngine(db).Create(ctx, purchasing.RequestForQuotationQuoteLine(), map[string]any{
		"rfq_line_id": lineAID, "vendor_id": vendorXID, "unit_price": 950,
	}, humanActor()); err != nil {
		t.Fatalf("quote lineA/vendorX: %v", err)
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	auditBefore, recordsBefore := count("audit_log"), count("records")

	req := newRequest("GET", "/reports/rfq/"+rfqID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := count("audit_log"); got != auditBefore {
		t.Errorf("the report route wrote %d audit_log rows — it must be read-only", got-auditBefore)
	}
	if got := count("records"); got != recordsBefore {
		t.Errorf("the report route wrote %d records rows — it must be read-only", got-recordsBefore)
	}
}
