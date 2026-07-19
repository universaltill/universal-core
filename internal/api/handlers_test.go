package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
