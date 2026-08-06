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
	view := h.buildRFQReportView(context.Background(), tenantScope{}, data.Record{Data: map[string]any{}}, lines, vendors, "en")

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
