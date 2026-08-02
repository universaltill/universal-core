package api

import (
	"strings"
	"testing"
)

// TestAPI_RecordForm_ShowsUBLDownloadLinkForExistingRecord is the
// internal/api integration layer for uc-infra#66 (independent review
// finding on #27: "the export is unreachable from the application").
// The formrender-package unit tests (internal/kernel/formrender) prove
// the {id}-substitution/i18n mechanism generically; this proves the
// real, published PurchaseOrder/SalesOrder/CustomerInvoice Form
// Definitions actually wire it up end to end, against a real record, via
// the real GET /forms/{entityType}/{id} route this pipeline serves.
func TestAPI_RecordForm_ShowsUBLDownloadLinkForExistingRecord(t *testing.T) {
	f := setupUBLTenant(t)

	cases := []struct {
		name       string
		entityType string
		id         string
	}{
		{"PurchaseOrder", "PurchaseOrder", f.poID},
		{"SalesOrder", "SalesOrder", f.soID},
		{"CustomerInvoice", "CustomerInvoice", f.invoiceID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, "/forms/"+tc.entityType+"/"+tc.id)
			if rec.Code != 200 {
				t.Fatalf("GET /forms/%s/%s: expected 200, got %d: %s", tc.entityType, tc.id, rec.Code, rec.Body.String())
			}
			wantHref := "/export/" + tc.entityType + "/" + tc.id + "/ubl"
			body := rec.Body.String()
			if !strings.Contains(body, `<a href="`+wantHref+`">`) {
				t.Fatalf("expected a download link to %q on the %s form page, got:\n%s", wantHref, tc.entityType, body)
			}
			if !strings.Contains(body, "Download UBL file") {
				t.Fatalf("expected the download link's English label to appear, got:\n%s", body)
			}
		})
	}
}

// TestAPI_RecordForm_OmitsUBLDownloadLinkForNewRecord is the negative
// case: a not-yet-saved PurchaseOrder/SalesOrder/CustomerInvoice has no
// id to export, so GET /forms/{entityType}/new must render no download
// link at all rather than one pointing at a literal "{id}" segment.
func TestAPI_RecordForm_OmitsUBLDownloadLinkForNewRecord(t *testing.T) {
	f := setupUBLTenant(t)

	for _, entityType := range []string{"PurchaseOrder", "SalesOrder", "CustomerInvoice"} {
		t.Run(entityType, func(t *testing.T) {
			rec := f.get(t, "/forms/"+entityType+"/new")
			if rec.Code != 200 {
				t.Fatalf("GET /forms/%s/new: expected 200, got %d: %s", entityType, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, "Download UBL file") || strings.Contains(body, "/ubl\"") {
				t.Fatalf("expected no UBL download link on the new/unsaved %s form, got:\n%s", entityType, body)
			}
		})
	}
}
