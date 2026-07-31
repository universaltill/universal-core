package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createVendor is a small helper: POST one Vendor record through the real
// handler and return its id. Used to seed enough records that the cap and
// the type-ahead filter are actually exercised, not asserted against a
// handful.
func createVendor(t *testing.T, mux *http.ServeMux, tenantID, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create vendor %q: expected 201, got %d: %s", name, rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	return created.Data.ID
}

// decodeRefOptions decodes the reference-search endpoint's JSON body into
// the {id,label} shape the combobox consumes.
func decodeRefOptions(t *testing.T, body []byte) []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
} {
	t.Helper()
	var env struct {
		Data []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal reference options: %v (body=%s)", err, string(body))
	}
	if env.Error != nil {
		t.Fatalf("expected no error in envelope, got %q", *env.Error)
	}
	return env.Data
}

// TestReferenceSearch_ReturnsIDAndLabelForTargetEntity is the happy path:
// the endpoint returns [{id,label}] for the target entity, labelled by the
// `name` field, so the combobox has something to render.
func TestReferenceSearch_ReturnsIDAndLabelForTargetEntity(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	id := createVendor(t, mux, tenantID, "Acme Textiles")

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 {
		t.Fatalf("expected exactly one option, got %d: %+v", len(opts), opts)
	}
	if opts[0].ID != id || opts[0].Label != "Acme Textiles" {
		t.Fatalf("expected {%s, Acme Textiles}, got %+v", id, opts[0])
	}
}

// TestReferenceSearch_FiltersByTypeAhead confirms the ?q= type-ahead
// narrows by the label field — the whole reason the endpoint exists
// instead of shipping every record to the browser.
func TestReferenceSearch_FiltersByTypeAhead(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createVendor(t, mux, tenantID, "Acme Textiles")
	createVendor(t, mux, tenantID, "Beta Supplies")
	createVendor(t, mux, tenantID, "Acme Hardware")

	req := newRequest("GET", "/api/references/Vendor?q=acme", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 2 {
		t.Fatalf("expected the two Acme vendors, got %d: %+v", len(opts), opts)
	}
	for _, o := range opts {
		if o.Label != "Acme Textiles" && o.Label != "Acme Hardware" {
			t.Fatalf("type-ahead leaked a non-matching option: %+v", o)
		}
	}
}

// TestReferenceSearch_CapsResults proves the endpoint never returns more
// than referenceSearchLimit in one page, and that a second page continues
// where the first left off — the payload guarantee the picker relies on.
func TestReferenceSearch_CapsResults(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// One more than a full page, so page 2 has exactly the remainder.
	total := referenceSearchLimit + 5
	for i := 0; i < total; i++ {
		createVendor(t, mux, tenantID, fmt.Sprintf("Vendor %03d", i))
	}

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page 1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	page1 := decodeRefOptions(t, rec.Body.Bytes())
	if len(page1) != referenceSearchLimit {
		t.Fatalf("page 1 should be capped at %d, got %d", referenceSearchLimit, len(page1))
	}

	req2 := newRequest("GET", "/api/references/Vendor?page=2", tenantID, "farshid", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("page 2: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	page2 := decodeRefOptions(t, rec2.Body.Bytes())
	if len(page2) != total-referenceSearchLimit {
		t.Fatalf("page 2 should have the remainder %d, got %d", total-referenceSearchLimit, len(page2))
	}
	// The two pages must not overlap — a broken offset would repeat rows.
	seen := map[string]bool{}
	for _, o := range page1 {
		seen[o.ID] = true
	}
	for _, o := range page2 {
		if seen[o.ID] {
			t.Fatalf("page 2 repeated a page-1 id %s — offset is wrong", o.ID)
		}
	}
}

// TestReferenceSearch_UnknownEntityTypeIsNotFound confirms a bogus target
// is a clean lookup error, not a 500 or an empty 200 that would hide a
// mistyped Definition.
func TestReferenceSearch_UnknownEntityTypeIsNotFound(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/api/references/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected a non-200 for an unknown entity type, got 200: %s", rec.Body.String())
	}
}

// TestReferenceSearch_HiddenLabelFieldDegradesNot403 is the regression
// test for the review finding that a FieldPermission hiding the label
// field (name/title) from a role used to 403 the ENTIRE picker for that
// role: the endpoint sorted/filtered by the label field unconditionally,
// and the guarded engine's rejectHiddenSortFilter then refused the whole
// request. A role that can legitimately read the entity but not its name
// field must still get a usable (id-labelled) picker, degrading the same
// way the form's own current-value label resolution does — never failing.
func TestReferenceSearch_HiddenLabelFieldDegradesNot403(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	// Hide Vendor.name from holders of the "clerk" role (user "user-clerk").
	seedFieldRule(t, db, "clerk", "user-clerk", "Vendor", "name")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Create the vendor as an unrestricted user so the name is actually set.
	id := createVendor(t, mux, tenantID, "Acme Textiles")

	// The clerk searches: must be 200 (not 403), must return the record
	// labelled by its raw id (the hidden name is redacted), and must not
	// leak the hidden name value.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest("GET", "/api/references/Vendor", tenantID, "user-clerk", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk with hidden label field should still get 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Acme Textiles") {
		t.Fatalf("the hidden name value must not leak into the picker, got: %s", rec.Body.String())
	}
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].ID != id || opts[0].Label != id {
		t.Fatalf("expected exactly one option labelled by raw id %q, got %+v", id, opts)
	}
}

// TestReferenceSearch_RequiresAuth confirms the endpoint is behind the
// same auth gate as every other /api route — an unauthenticated request
// never reaches the data layer.
func TestReferenceSearch_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	// Deliberately NOT enabling dev auth, and sending no tenant/actor.
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/api/references/Vendor", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected an auth failure, got 200: %s", rec.Body.String())
	}
}
