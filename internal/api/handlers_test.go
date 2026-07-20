package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

func seedTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO tenants (name, region) VALUES ($1, $2) RETURNING id`,
		"Test Tenant", "eu-west",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func humanActor() audit.Actor {
	return audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
}

func vendorEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

func vendorFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Vendor",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "name", Label: "Name"}}},
		},
	}
}

// itemWithFlagEntityDef/itemWithFlagFormDef are for the two form-submit
// regression tests below: a bool field (real HTML checkbox semantics)
// and a field the form deliberately doesn't show (a partial form —
// exactly what foundation.go's own doc comment encourages building).
func itemWithFlagEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "ItemWithFlag",
		Version:    1,
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString, Required: true},
			{Name: "is_urgent", Type: entity.FieldBool},
			{Name: "internal_note", Type: entity.FieldString},
		},
	}
}

// itemWithFlagFormDef deliberately shows only sku/is_urgent — not
// internal_note.
func itemWithFlagFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "ItemWithFlag",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{
				{Name: "sku", Label: "SKU"},
				{Name: "is_urgent", Label: "Urgent"},
			}},
		},
	}
}

// publishEntityAndForm drives both Definitions through
// CreateDraft -> Approve -> Publish, so handler tests can exercise a
// real registry lookup instead of constructing a Definition in Go and
// bypassing the registry entirely.
func publishEntityAndForm(t *testing.T, db *sql.DB, tenantID string, entDef *entity.Definition, formDef *form.Definition) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	entRaw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal entity def: %v", err)
	}
	entRepo := data.NewEntityDefinitionRepo(db)
	if _, err := entRepo.CreateDraft(ctx, tenantID, entDef.EntityType, entDef.Version, entRaw, actor); err != nil {
		t.Fatalf("CreateDraft entity: %v", err)
	}
	if err := entRepo.Approve(ctx, tenantID, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve entity: %v", err)
	}
	if err := entRepo.Publish(ctx, tenantID, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish entity: %v", err)
	}

	formRaw, err := json.Marshal(formDef)
	if err != nil {
		t.Fatalf("marshal form def: %v", err)
	}
	formRepo := data.NewFormDefinitionRepo(db)
	if _, err := formRepo.CreateDraft(ctx, tenantID, formDef.EntityType, formDef.Version, formRaw, actor); err != nil {
		t.Fatalf("CreateDraft form: %v", err)
	}
	if err := formRepo.Approve(ctx, tenantID, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Approve form: %v", err)
	}
	if err := formRepo.Publish(ctx, tenantID, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Publish form: %v", err)
	}
}

func testHandler(t *testing.T, db *sql.DB) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(db, catalog)
}

func newRequest(method, target, tenantID, actorID string, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if tenantID != "" {
		r.Header.Set("X-Tenant-ID", tenantID)
	}
	if actorID != "" {
		r.Header.Set("X-Actor-ID", actorID)
	}
	return r
}

func withDevAuthEnabled(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("INSECURE_DEV_AUTH")
	os.Setenv("INSECURE_DEV_AUTH", "true")
	t.Cleanup(func() {
		if had {
			os.Setenv("INSECURE_DEV_AUTH", prev)
		} else {
			os.Unsetenv("INSECURE_DEV_AUTH")
		}
	})
}

// TestAPI_CreateGetListRecord_FullLoop exercises registry -> crud -> HTTP
// end to end: publish a Definition through the real registry (not a
// hand-built Go value bypassing it), POST a record, GET it back, and
// confirm it shows up in the list — all through the actual HTTP
// handlers, not by calling crud.Engine directly.
func TestAPI_CreateGetListRecord_FullLoop(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	// Create.
	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.Error != nil {
		t.Fatalf("expected no error, got %v", *created.Error)
	}
	if created.Data.ID == "" {
		t.Fatal("expected a non-empty record id")
	}
	if created.Data.Data["name"] != "Acme Textiles" {
		t.Fatalf("expected name to round-trip, got %+v", created.Data.Data)
	}

	// Get.
	getReq := newRequest("GET", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "Acme Textiles") {
		t.Fatalf("expected the created record's data in the GET response, got %s", getRec.Body.String())
	}

	// List.
	listReq := newRequest("GET", "/api/records/Vendor", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), created.Data.ID) {
		t.Fatalf("expected the created record's id in the list response, got %s", listRec.Body.String())
	}
}

func TestAPI_CreateRecord_ValidationFailureIs400(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	// "name" is required; omit it.
	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a validation failure, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_CreateRecord_MalformedJSONIs400(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`not json`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_CreateRecord_FormURLEncodedBody is the regression test for the
// bug found by internal/e2e's real-browser testing: formrender's own
// <form> submits as application/x-www-form-urlencoded (htmx's default —
// no hx-encoding override on the form tag), which the old JSON-only
// decoder rejected outright with "invalid JSON body" before the request
// ever reached validation. Every real "Save" click was silently broken.
func TestAPI_CreateRecord_FormURLEncodedBody(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("POST", "/api/records/Vendor", strings.NewReader("name=Acme+Textiles"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Acme Textiles") {
		t.Fatalf("expected the form-encoded name to round-trip, got %s", rec.Body.String())
	}
}

// TestAPI_CreateRecord_HTMXRequest_ReturnsHTMLFragment confirms an
// htmx-issued create (HX-Request: true, set automatically by htmx on
// every request — see isHTMXRequest) gets back the re-rendered form as
// a bare HTML fragment, matching formrender's own
// hx-target="this" hx-swap="outerHTML" contract — not the JSON envelope
// a browser has nothing to do with once swapped into a <form> element's
// place. The returned form points at the new record's own id (a
// "create" form becomes an "edit" form for what it just created, the
// standard htmx pattern), and is NOT wrapped in the page shell (layout.go)
// — this is a swap response, not a page navigation.
func TestAPI_CreateRecord_HTMXRequest_ReturnsHTMLFragment(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("POST", "/api/records/Vendor", strings.NewReader("name=Acme+Textiles"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("expected a bare fragment (no page shell) for an htmx-swap response, got:\n%s", body)
	}
	if !strings.Contains(body, `value="Acme Textiles"`) {
		t.Fatalf("expected the saved value pre-filled in the returned form, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/api/records/Vendor/`) {
		t.Fatalf("expected the form to now target its own record id, got:\n%s", body)
	}
}

// TestAPI_UpdateRecord_FullLoop is the regression test for the second,
// more severe half of the same bug: POST /api/records/{entityType}/{id}
// had no route registered at all before this fix — saving an *existing*
// record's form 404'd outright, unconditionally, regardless of body
// format.
func TestAPI_UpdateRecord_FullLoop(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	updateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", []byte(`{"name":"Acme Textiles Ltd"}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), "Acme Textiles Ltd") {
		t.Fatalf("expected the updated name in the response, got %s", updateRec.Body.String())
	}

	getReq := newRequest("GET", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Acme Textiles Ltd") {
		t.Fatalf("expected the update to persist, got %s", getRec.Body.String())
	}
}

func TestAPI_UpdateRecord_HTMXRequest_ReturnsHTMLFragment(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/records/Vendor/"+created.Data.ID, strings.NewReader("name=Acme+Textiles+Ltd"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `value="Acme Textiles Ltd"`) {
		t.Fatalf("expected the updated value in the returned form, got:\n%s", rec.Body.String())
	}
}

func TestAPI_UpdateRecord_UnknownRecordIs404(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("POST", "/api/records/Vendor/99999999-9999-9999-9999-999999999999", tenantID, "farshid", []byte(`{"name":"X"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown record id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_UpdateRecord_ValidationFailureIs400(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	// "name" is required; omit it.
	updateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", []byte(`{}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a validation failure, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
}

// TestAPI_FormSubmit_CheckedBoolFieldSavesCorrectly is the end-to-end
// regression test (real HTTP handler, real Postgres) for the checkbox
// bug independent review found: a real browser checking a box and
// submitting the form used to 400 with "field is_urgent: \"on\" is not
// a bool", because formrender emitted a bare <input type="checkbox"> (no
// value attribute — browsers default a checked box's submitted value to
// "on") and csvimport.Coerce's strconv.ParseBool rejects "on" outright.
// Simulates exactly what a real browser now submits after formrender's
// fix: the hidden false-fallback, then the checkbox's own explicit
// value="true" — form-urlencoded body order matches DOM order, so this
// is "false" then "true" for the same key when checked.
func TestAPI_FormSubmit_CheckedBoolFieldSavesCorrectly(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("POST", "/api/records/ItemWithFlag", strings.NewReader("sku=STEEL-BAR-10&is_urgent=false&is_urgent=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Data.Data["is_urgent"] != true {
		t.Fatalf("expected is_urgent to save as true, got %+v", created.Data.Data)
	}
}

// TestAPI_FormSubmit_UncheckedBoolFieldSavesFalse is the unchecked-box
// counterpart: a real browser omits an unchecked checkbox from the
// submission entirely, sending only the hidden false-fallback.
func TestAPI_FormSubmit_UncheckedBoolFieldSavesFalse(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("POST", "/api/records/ItemWithFlag", strings.NewReader("sku=STEEL-BAR-10&is_urgent=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Data.Data["is_urgent"] != false {
		t.Fatalf("expected is_urgent to save as false, got %+v", created.Data.Data)
	}
}

// TestAPI_FormSubmit_PartialFormPreservesOffFormFields is the end-to-end
// regression test (real HTTP handler, real Postgres, real formrender
// output round-tripped back through parseRecordFields) for the more
// severe of the two bugs independent review found: updateRecord's
// underlying write is a full replacement, not a merge, so saving a
// deliberately partial form (itemWithFlagFormDef doesn't show
// internal_note) used to silently wipe internal_note from the stored
// record. This drives the ACTUAL rendered form's own HTML back through
// the update endpoint — not a hand-built body — so it fails if
// formrender's hidden-field fix and parseRecordFields' handling of it
// ever drift apart from each other.
func TestAPI_FormSubmit_PartialFormPreservesOffFormFields(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/ItemWithFlag", tenantID, "farshid",
		[]byte(`{"sku":"STEEL-BAR-10","is_urgent":false,"internal_note":"IMPORTANT, DO NOT LOSE"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	// Fetch the real rendered edit form — the actual HTML a browser
	// would get, hidden fields included — and parse the real
	// application/x-www-form-urlencoded body a submission of it would
	// produce, rather than hand-constructing one.
	formReq := newRequest("GET", "/forms/ItemWithFlag/"+created.Data.ID, tenantID, "farshid", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200 rendering the form, got %d: %s", formRec.Code, formRec.Body.String())
	}
	if !strings.Contains(formRec.Body.String(), `name="internal_note" value="IMPORTANT, DO NOT LOSE"`) {
		t.Fatalf("expected the rendered form to carry internal_note as a hidden field, got:\n%s", formRec.Body.String())
	}

	// Only the fields the form actually shows are changed — sku is
	// edited, internal_note is left exactly as the form rendered it
	// (its hidden fallback), matching what a real form submission does.
	body := "sku=" + url.QueryEscape("STEEL-BAR-10-REV2") +
		"&is_urgent=false" +
		"&internal_note=" + url.QueryEscape("IMPORTANT, DO NOT LOSE")
	updateReq := httptest.NewRequest("POST", "/api/records/ItemWithFlag/"+created.Data.ID, strings.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updateReq.Header.Set("X-Tenant-ID", tenantID)
	updateReq.Header.Set("X-Actor-ID", "farshid")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	getReq := newRequest("GET", "/api/records/ItemWithFlag/"+created.Data.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "IMPORTANT, DO NOT LOSE") {
		t.Fatalf("expected internal_note to survive a partial-form save, got %s", getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "STEEL-BAR-10-REV2") {
		t.Fatalf("expected the visibly-edited sku to have actually changed, got %s", getRec.Body.String())
	}
}

func TestAPI_UnknownEntityType_Is404NotInternalError(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	// Deliberately don't publish anything.

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("GET", "/api/records/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an entity type with no published definition, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_TenantIsolation confirms a record created under one tenant is
// invisible to another tenant's GET/list, through the actual HTTP
// handlers.
func TestAPI_TenantIsolation(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantA := seedTenant(t, db)
	tenantB := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantA, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, tenantB, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantA, "farshid", []byte(`{"name":"Tenant A Only"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Tenant B's GET for tenant A's record ID must not find it.
	getReq := newRequest("GET", "/api/records/Vendor/"+created.Data.ID, tenantB, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected tenant B to get 404 for tenant A's record, got %d: %s", getRec.Code, getRec.Body.String())
	}

	// Tenant B's list must not include it either.
	listReq := newRequest("GET", "/api/records/Vendor", tenantB, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), "Tenant A Only") {
		t.Fatalf("tenant B's list leaked tenant A's record: %s", listRec.Body.String())
	}
}

func TestAPI_NoAuthHeaders_Is401(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("GET", "/api/records/Vendor", nil) // no X-Tenant-ID/X-Actor-ID
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

// TestAPI_RenderNewForm_ProducesHTML is the first genuine end-to-end
// proof of the whole point of this increment: a Definition published
// through the real registry, rendered to real HTML through formrender,
// served over real HTTP.
func TestAPI_RenderNewForm_ProducesHTML(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("GET", "/forms/Vendor/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html content type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-entity-type="Vendor"`) {
		t.Fatalf("expected the rendered form to reference the Vendor entity type, got:\n%s", body)
	}
	if !strings.Contains(body, `name="name"`) {
		t.Fatalf("expected the rendered form to contain the name field, got:\n%s", body)
	}
	// Regression coverage for the gap internal/e2e's first real-browser
	// test caught: without this script tag, every hx-* attribute on the
	// page is inert markup — a browser has nothing to execute them with.
	if !strings.Contains(body, `<script src="/static/htmx.min.js"></script>`) {
		t.Fatalf("expected the page to load htmx.js, got:\n%s", body)
	}
}

// TestAPI_RenderRecordForm_ShowsRecordData confirms an existing record's
// data actually reaches the rendered HTML, not just an empty form shell.
func TestAPI_RenderRecordForm_ShowsRecordData(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Beta Supplies"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	formReq := newRequest("GET", "/forms/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)

	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", formRec.Code, formRec.Body.String())
	}
	if !strings.Contains(formRec.Body.String(), "Beta Supplies") {
		t.Fatalf("expected the record's own data in the rendered form, got:\n%s", formRec.Body.String())
	}
}

func purchaseOrderEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldString, Required: true},
		},
		Relationships: []entity.Relationship{
			{Name: "lines", Kind: entity.RelationComposition, Target: "POLine", ParentField: "purchase_order_id"},
		},
	}
}

func purchaseOrderFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}}},
			{Title: "Lines", Component: form.ComponentMasterDetail, Target: "POLine", RollUp: "line_total", RollUpTarget: "total"},
		},
	}
}

func poLineEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "POLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldString, Required: true},
			{Name: "line_total", Type: entity.FieldNumber, Required: true},
		},
	}
}

func poLineFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "POLine",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "line_total", Label: "Line Total"}}},
		},
	}
}

// TestAPI_RenderRecordForm_ShowsMasterDetailChildren is the regression
// test for a real gap found while dogfooding the purchasing module: a
// PurchaseOrder form's Lines section rendered as permanently empty even
// when POLine records referencing it actually existed, because
// renderForm never populated formrender.Data.Children (RecordRepo had no
// "list where field X == this id" query — see loadMasterDetailChildren's
// doc comment). This confirms a real child row now shows up, and that
// its line_total actually rolls up into the header.
func TestAPI_RenderRecordForm_ShowsMasterDetailChildren(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, tenantID, poLineEntityDef(), poLineFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	poReq := newRequest("POST", "/api/records/PurchaseOrder", tenantID, "farshid", []byte(`{"vendor_id":"v1"}`))
	poRec := httptest.NewRecorder()
	mux.ServeHTTP(poRec, poReq)
	var po struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(poRec.Body.Bytes(), &po); err != nil {
		t.Fatalf("unmarshal PO: %v", err)
	}

	lineBody := []byte(`{"purchase_order_id":"` + po.Data.ID + `","line_total":150.5}`)
	lineReq := newRequest("POST", "/api/records/POLine", tenantID, "farshid", lineBody)
	lineRecRec := httptest.NewRecorder()
	mux.ServeHTTP(lineRecRec, lineReq)
	if lineRecRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating POLine, got %d: %s", lineRecRec.Code, lineRecRec.Body.String())
	}

	formReq := newRequest("GET", "/forms/PurchaseOrder/"+po.Data.ID, tenantID, "farshid", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)

	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", formRec.Code, formRec.Body.String())
	}
	body := formRec.Body.String()
	if strings.Contains(body, "No lines yet") {
		t.Fatalf("expected the existing POLine to render as a child row, got:\n%s", body)
	}
	if !strings.Contains(body, "total: 150.5") {
		t.Fatalf("expected the roll-up to sum the child's line_total into the header total, got:\n%s", body)
	}
}

// TestAPI_ServesHTMXScript_Unauthenticated confirms /static/htmx.min.js
// is reachable without dev-auth headers — it has to be, since the page
// requesting it (a real browser navigating to a route DevAuth would
// otherwise gate) hasn't authenticated at the point it fetches its own
// <script> tag.
func TestAPI_ServesHTMXScript_Unauthenticated(t *testing.T) {
	db := testDB(t)
	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := httptest.NewRequest("GET", "/static/htmx.min.js", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no auth headers, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("expected a javascript content type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "htmx") {
		t.Fatalf("expected real htmx.js content, got %d bytes starting with: %.60s", rec.Body.Len(), rec.Body.String())
	}
}

func TestAPI_RenderForm_UnknownRecordIs404(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	req := newRequest("GET", "/forms/Vendor/99999999-9999-9999-9999-999999999999", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown record id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_MalformedRecordID_Is400NotRawSQLError is the regression test
// for the code-review finding that GET /api/records/{entityType}/{id}
// with a non-UUID id reached crud.Engine.Get, which reached Postgres,
// which returned "invalid input syntax for type uuid: ... (SQLSTATE
// 22P02)" as a raw, leaked 500. It's now caught before any query runs.
func TestAPI_MalformedRecordID_Is400NotRawSQLError(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)
	publishEntityAndForm(t, db, tenantID, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	for _, target := range []string{
		"/api/records/Vendor/not-a-uuid",
		"/forms/Vendor/not-a-uuid",
	} {
		req := newRequest("GET", target, tenantID, "farshid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 for a malformed record id, got %d: %s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "SQLSTATE") || strings.Contains(rec.Body.String(), "ERROR:") {
			t.Fatalf("%s: response leaked a raw driver error: %s", target, rec.Body.String())
		}
	}
}

// TestAPI_InternalErrors_NeverLeakRawDriverText is a broader regression
// test for the same finding: a malformed X-Tenant-ID (which used to
// reach the definition-lookup query and surface Postgres's raw error
// text with a 500) must now come back as a generic message. The tenant
// id shape is actually rejected one layer up by httpx.DevAuth (401,
// tested in internal/httpx), so this confirms the handler layer's own
// generic-500 behavior for a DB-reachable-but-still-invalid case: an
// entity type that collides with nothing (a plain lookup miss) stays a
// clean 404, never a raw error leak, across every route.
func TestAPI_InternalErrors_NeverLeakRawDriverText(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	tenantID := seedTenant(t, db)

	mux := http.NewServeMux()
	testHandler(t, db).Routes(mux)

	for _, target := range []string{
		"/api/records/DefinitelyNotDefined",
		"/forms/DefinitelyNotDefined/new",
	} {
		req := newRequest("GET", target, tenantID, "farshid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if strings.Contains(rec.Body.String(), "SQLSTATE") || strings.Contains(rec.Body.String(), "ERROR:") {
			t.Fatalf("%s: response leaked a raw driver error: %s", target, rec.Body.String())
		}
	}
}
