package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestAPI_RFQForm_ShowsCompareQuotesLinkForExistingRecord is the
// internal/api integration layer for uc-infra#69 (independent review
// finding on #9: "the vendor quote comparison report is unreachable from
// in-app navigation"). The formrender-package unit tests
// (internal/kernel/formrender) already prove the generic {id}-
// substitution/i18n navigate mechanism; this proves the real, published
// RequestForQuotation Form Definition actually wires it up end to end,
// against a real record, via the real GET /forms/{entityType}/{id} route
// this pipeline serves — same pattern as
// TestAPI_RecordForm_ShowsUBLDownloadLinkForExistingRecord (uc-infra#66).
func TestAPI_RFQForm_ShowsCompareQuotesLinkForExistingRecord(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rfqID, _, _, _, _ := setupRFQTenant(t, db)

	req := newRequest("GET", "/forms/RequestForQuotation/"+rfqID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forms/RequestForQuotation/%s: expected 200, got %d: %s", rfqID, rec.Code, rec.Body.String())
	}
	wantHref := "/reports/rfq/" + rfqID
	body := rec.Body.String()
	if !strings.Contains(body, `<a href="`+wantHref+`">`) {
		t.Fatalf("expected a Compare Quotes link to %q on the RequestForQuotation form page, got:\n%s", wantHref, body)
	}
	if !strings.Contains(body, "Compare Quotes") {
		t.Fatalf("expected the link's English label to appear, got:\n%s", body)
	}
}

// TestAPI_RFQForm_OmitsCompareQuotesLinkForNewRecord is the negative
// case: a not-yet-saved RequestForQuotation has no id to compare quotes
// for, so GET /forms/RequestForQuotation/new must render no link at all
// rather than one pointing at a literal "{id}" segment.
func TestAPI_RFQForm_OmitsCompareQuotesLinkForNewRecord(t *testing.T) {
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
	if err := purchasing.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}

	req := newRequest("GET", "/forms/RequestForQuotation/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forms/RequestForQuotation/new: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Compare Quotes") || strings.Contains(body, `href="/reports/rfq/`) {
		t.Fatalf("expected no Compare Quotes link on the new/unsaved RequestForQuotation form, got:\n%s", body)
	}
}
