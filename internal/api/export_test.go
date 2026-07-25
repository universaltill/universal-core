package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExport_RequiresAuth(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("GET", "/export/Order", nil) // no auth headers
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExport_UnknownEntityTypeIs404(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("GET", "/export/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unpublished entity type, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestExport_WritesCSVAttachmentWithTenantsOwnRecords is the core
// proof: a real record created through the generic API is exported as a
// CSV row with the right headers, Content-Type, and a
// Content-Disposition that names a real, downloadable filename.
func TestExport_WritesCSVAttachmentWithTenantsOwnRecords(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, orderEntityDef(), orderFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Order", tenantID, "farshid",
		[]byte(`{"vendor_id":"Acme Textiles","order_date":"2026-07-01"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating the Order record, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/export/Order", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("expected a text/csv Content-Type, got %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="Order.csv"` {
		t.Fatalf("expected a well-formed Content-Disposition, got %q", cd)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Acme Textiles") {
		t.Fatalf("expected the created record's data in the export, got:\n%s", body)
	}
	if !strings.Contains(body, "2026-07-01") {
		t.Fatalf("expected the created record's order_date in the export, got:\n%s", body)
	}
}

// TestExport_TenantIsolation confirms one tenant's export never includes
// another tenant's records — the same cross-tenant-leak class of bug
// every other list/read path in this kernel is held to.
func TestExport_TenantIsolation(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantA, orderEntityDef(), orderFormDef())
	publishEntityAndForm(t, db, tenantB, orderEntityDef(), orderFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Order", tenantA, "farshid",
		[]byte(`{"vendor_id":"Tenant A Only Vendor","order_date":"2026-07-01"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating tenant A's Order record, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/export/Order", tenantB, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Tenant A Only Vendor") {
		t.Fatalf("expected tenant B's export to never contain tenant A's data, got:\n%s", rec.Body.String())
	}
}

// TestSafeFilenameComponent_StripsHeaderInjectionCharacters guards
// exportRecordsCSV's own Content-Disposition sink directly, independent
// of whether anything in this kernel could currently get an entity type
// containing a quote or CR/LF published — see export.go's own doc
// comment on why this is closed regardless.
func TestSafeFilenameComponent_StripsHeaderInjectionCharacters(t *testing.T) {
	got := safeFilenameComponent(`Evil"; X-Injected: yes` + "\r\nSet-Cookie: a=b")
	if strings.ContainsAny(got, "\"\r\n:;") {
		t.Fatalf("expected all header-unsafe characters stripped, got %q", got)
	}
}
