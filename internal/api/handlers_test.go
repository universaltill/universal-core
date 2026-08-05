package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/db"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/testexec"
)

// freshControlDB returns a connection to a brand-new, uniquely-named
// control-plane database (ADR-0003) with the control migration set
// applied. Skips (not fails) if TEST_DATABASE_URL isn't set.
func freshControlDB(t *testing.T) (controlDB *sql.DB, controlDSN string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("uc_test_api_control_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	dsn := u.String()
	control, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open control database %s: %v", name, err)
	}
	t.Cleanup(func() { control.Close() })
	if err := control.Ping(); err != nil {
		t.Fatalf("ping control database %s: %v", name, err)
	}
	if _, err := control.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto extension: %v", err)
	}
	if err := db.ApplyControl(context.Background(), control); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	return control, dsn
}

// newTestRouter builds a Router against a fresh control database — every
// test's Handler is built against this, exactly as cmd/universal-core
// wires it in production.
func newTestRouter(t *testing.T) *tenantdb.Router {
	t.Helper()
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	return router
}

// newTestTenant provisions a brand-new tenant via router.Create and
// returns its id and own *sql.DB — the real provisioning path, so these
// tests exercise the same route cmd/provision-tenant does. No
// workflow_jobs cleanup needed the way the old shared-database version
// of this helper required: each tenant is a physically separate
// database, so a job left behind by one test can never be claimed by
// another test's ProcessOne call.
func newTestTenant(t *testing.T, router *tenantdb.Router) (tenantID string, tenantDB *sql.DB) {
	t.Helper()
	ctx := context.Background()
	tenantID, err := router.Create(ctx, "Test Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	// Router.Create provisions a SECOND physical database (ADR-0003) that
	// freshControlDB's own cleanup knows nothing about — without this,
	// every test in this package leaked one permanently (~4,100 orphans
	// on the dev machine before this was found).
	testexec.DropConnectedDatabase(t, tenantDB)
	return tenantID, tenantDB
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

// quickCreatableVendorEntityDef is vendorEntityDef with QuickCreatable
// set — a separate fixture rather than flipping the flag on the shared
// vendorEntityDef, so every OTHER existing test that publishes a plain
// Vendor keeps exercising the (more common) non-quick-creatable case
// unchanged. Used only by the two TestAPI_RenderForm_
// ReferenceCreateButton* tests below (part 2 of #24,
// universaltill/uc-infra#51), which are specifically about the
// QuickCreatable + CanWrite pair of gates.
func quickCreatableVendorEntityDef() *entity.Definition {
	def := vendorEntityDef()
	def.QuickCreatable = true
	return def
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

// nodeEntityDef/nodeFormDef are a throwaway self-referencing entity
// (same shape Account.parent_account_id/Department.parent_department_id
// take — see internal/kernel/crud/cycle_test.go's categoryDef, which
// exercises the same guard at the crud.Engine layer directly) — used
// here to prove the guard's HTTP-level wiring: crud.ErrReferenceCycle
// actually reaches the client as a 400 through writeCrudError, not just
// through crud.Engine's own package tests.
func nodeEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Node",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "parent_node_id", Type: entity.FieldReference, Target: "Node"},
		},
	}
}

func nodeFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Node",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "name", Label: "Name"}}},
		},
	}
}

// groupedItemEntityDef/groupedItemFormDef are a throwaway self-referencing
// entity carrying MustMatchParentField (uc-infra#78) — the HTTP-level
// counterpart of internal/kernel/crud/target_constraints_test.go's own
// groupedItemDef, used here to prove crud.ErrTargetConstraintViolation's
// mapping to a translated 400 (independent review, finding #6/#7: this
// had zero coverage at the HTTP layer, and the message shipped
// untranslated before that fix).
func groupedItemEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "GroupedItem",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "group_id", Type: entity.FieldString, Required: true},
			{Name: "parent_item_id", Type: entity.FieldReference, Target: "GroupedItem", MustMatchParentField: "group_id"},
		},
	}
}

func groupedItemFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "GroupedItem",
		Version:    1,
		Sections: []form.Section{
			{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{
				{Name: "name", Label: "Name"},
				{Name: "group_id", Label: "Group"},
				{Name: "parent_item_id", Label: "Parent Item"},
			}},
		},
	}
}

// publishEntityAndForm drives both Definitions through
// CreateDraft -> Approve -> Publish, so handler tests can exercise a
// real registry lookup instead of constructing a Definition in Go and
// bypassing the registry entirely.
func publishEntityAndForm(t *testing.T, db *sql.DB, entDef *entity.Definition, formDef *form.Definition) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	entRaw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal entity def: %v", err)
	}
	entRepo := data.NewEntityDefinitionRepo(db)
	if _, err := entRepo.CreateDraft(ctx, entDef.EntityType, entDef.Version, entRaw, actor); err != nil {
		t.Fatalf("CreateDraft entity: %v", err)
	}
	if err := entRepo.Approve(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve entity: %v", err)
	}
	if err := entRepo.Publish(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish entity: %v", err)
	}

	formRaw, err := json.Marshal(formDef)
	if err != nil {
		t.Fatalf("marshal form def: %v", err)
	}
	formRepo := data.NewFormDefinitionRepo(db)
	if _, err := formRepo.CreateDraft(ctx, formDef.EntityType, formDef.Version, formRaw, actor); err != nil {
		t.Fatalf("CreateDraft form: %v", err)
	}
	if err := formRepo.Approve(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Approve form: %v", err)
	}
	if err := formRepo.Publish(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Publish form: %v", err)
	}
}

// publishWorkflow drives def through CreateDraft -> Approve -> Publish —
// the real registry-backed lifecycle a trigger-matching test needs
// (triggerWorkflows reads published workflow_definitions rows directly,
// not a stub).
func publishWorkflow(t *testing.T, db *sql.DB, def *workflow.Definition) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal workflow def: %v", err)
	}
	repo := data.NewWorkflowDefinitionRepo(db)
	if _, err := repo.CreateDraft(ctx, def.Name, def.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft workflow: %v", err)
	}
	if err := repo.Approve(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Approve workflow: %v", err)
	}
	if err := repo.Publish(ctx, def.Name, def.Version, actor); err != nil {
		t.Fatalf("Publish workflow: %v", err)
	}
}

// testHandler builds a Handler with webauth disabled (nil Authenticator)
// — every existing test in this file authenticates via DevAuth's
// X-Tenant-ID/X-Actor-ID headers, and a nil Authenticator's Guard is a
// pure pass-through straight to DevAuth (see webauth.Authenticator.Guard
// and httpx.DevAuth's own doc comments on how the two compose).
// internal/webauth's own tests cover the real-login path.
func testHandler(t *testing.T, router *tenantdb.Router) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(router, catalog, nil, nil, nil, nil, nil)
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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// "name" is required; omit it.
	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a validation failure, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_CreateRecord_MalformedJSONIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/api/records/Vendor/99999999-9999-9999-9999-999999999999", tenantID, "farshid", []byte(`{"name":"X"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown record id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_UpdateRecord_ValidationFailureIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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

// TestAPI_UpdateRecord_SelfReferenceCycleIs400 is the HTTP-level proof
// that crud.ErrReferenceCycle (internal/kernel/crud/cycle_test.go proves
// the guard itself) actually reaches a real client as 400 through
// writeCrudError — before this test, nothing exercised
// internal/api/handlers.go's own mapping line at all.
func TestAPI_UpdateRecord_SelfReferenceCycleIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, nodeEntityDef(), nodeFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createRoot := newRequest("POST", "/api/records/Node", tenantID, "farshid", []byte(`{"name":"Root"}`))
	rootRec := httptest.NewRecorder()
	mux.ServeHTTP(rootRec, createRoot)
	var root struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rootRec.Body.Bytes(), &root); err != nil {
		t.Fatalf("unmarshal root create response: %v", err)
	}

	// A record pointing at itself is the direct-cycle case.
	updateReq := newRequest("POST", "/api/records/Node/"+root.Data.ID, tenantID, "farshid",
		[]byte(`{"name":"Root","parent_node_id":"`+root.Data.ID+`","version":1}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a self-reference cycle, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), "crud:") {
		t.Fatalf("expected no raw Go package prefix in the client-facing error body, got: %s", updateRec.Body.String())
	}
}

// TestAPI_CreateRecord_HookRejectionIs400 is
// TestAPI_UpdateRecord_SelfReferenceCycleIs400's counterpart for
// crud.ErrHookRejected (added for uc-infra#13's StockTransfer
// validation hook, internal/kernel/purchasing/ledger.go's
// ValidateStockTransfer): a generic proof that ANY hook wrapping
// its rejection in crud.ErrHookRejected reaches the client as a 400 with
// its own message through writeCrudError, not the generic 500 an
// unwrapped hook error would fall through to. Uses a throwaway hook on
// the plain Vendor entity rather than the real purchasing package,
// keeping this file's own "internal/api stays entity-agnostic" rule
// intact (RegisterHook's own doc comment) — this is a property of the
// generic dispatch/error-mapping mechanism, not of StockTransfer
// specifically, and purchasing.ValidateStockTransfer's own
// package tests already cover the hook's actual business rules.
func TestAPI_CreateRecord_HookRejectionIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	handler := testHandler(t, router)
	handler.RegisterHook("Vendor", func(ctx context.Context, tx *sql.Tx, def *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
		return fmt.Errorf("%w: Vendor name %q is not allowed", crud.ErrHookRejected, rec.Data["name"])
	})
	mux := http.NewServeMux()
	handler.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a hook rejection, got %d: %s", createRec.Code, createRec.Body.String())
	}
	if !strings.Contains(createRec.Body.String(), "Acme Textiles") {
		t.Errorf("expected the hook's own message to reach the client, got: %s", createRec.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'Vendor'`).Scan(&count); err != nil {
		t.Fatalf("count Vendor records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the rejected create to roll back entirely, found %d Vendor records", count)
	}
}

// TestAPI_UpdateRecord_TargetConstraintViolationIs400Localized is the
// HTTP-level proof that crud.ErrTargetConstraintViolation (a
// *crud.TargetConstraintError, uc-infra#78) reaches the client as a 400
// with a TRANSLATED per-field message — independent review findings #6
// (the message shipped untranslated, contradicting CLAUDE.md's "no
// hardcoded user-facing strings") and #7 (zero coverage at this layer)
// together. Uses groupedItemEntityDef's MustMatchParentField shape
// rather than TargetFilter's entity-join shape — either exercises the
// same writeCrudErrorLocalized mapping, and this one needs no second
// entity type/PartyRole fixture to set up.
func TestAPI_UpdateRecord_TargetConstraintViolationIs400Localized(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, groupedItemEntityDef(), groupedItemFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createParent := newRequest("POST", "/api/records/GroupedItem", tenantID, "farshid",
		[]byte(`{"name":"A-root","group_id":"group-a"}`))
	parentRec := httptest.NewRecorder()
	mux.ServeHTTP(parentRec, createParent)
	var parent struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(parentRec.Body.Bytes(), &parent); err != nil {
		t.Fatalf("unmarshal parent create response: %v", err)
	}

	createChild := newRequest("POST", "/api/records/GroupedItem", tenantID, "farshid",
		[]byte(`{"name":"B-child","group_id":"group-b"}`))
	childRec := httptest.NewRecorder()
	mux.ServeHTTP(childRec, createChild)
	var child struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(childRec.Body.Bytes(), &child); err != nil {
		t.Fatalf("unmarshal child create response: %v", err)
	}

	// Assigning the group-a parent to the group-b child violates
	// MustMatchParentField.
	updateReq := newRequest("POST", "/api/records/GroupedItem/"+child.Data.ID, tenantID, "farshid",
		[]byte(`{"name":"B-child","group_id":"group-b","parent_item_id":"`+parent.Data.ID+`","version":1}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a target constraint violation, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	// The translated envelope message (crud.error.target_constraint_violation,
	// {field} resolved via the field-label catalog, falling back to the raw
	// field name — no field.GroupedItem.parent_item_id key exists), NOT the
	// untranslated Detail text (entity/field/value-naming, e.g. "expected").
	want := `The selected value for parent_item_id does not meet this field's required constraint.`
	if !strings.Contains(updateRec.Body.String(), want) {
		t.Fatalf("expected the translated envelope message %q, got: %s", want, updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), "expected \"group-b\"") {
		t.Fatalf("expected the raw untranslated Detail text NOT to reach the client, got: %s", updateRec.Body.String())
	}
}

// TestAPI_RenderForm_IncludesVersionHiddenField confirms an existing
// record's edit form actually carries the "_version" hidden field a real
// browser needs to round-trip for optimistic-locking protection — a new/
// unsaved record's form must NOT have one (nothing to check yet).
func TestAPI_RenderForm_IncludesVersionHiddenField(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	newFormReq := newRequest("GET", "/forms/Vendor/new", tenantID, "farshid", nil)
	newFormRec := httptest.NewRecorder()
	mux.ServeHTTP(newFormRec, newFormReq)
	if strings.Contains(newFormRec.Body.String(), `name="_version"`) {
		t.Fatalf("expected no _version field on a new/unsaved record's form, got:\n%s", newFormRec.Body.String())
	}

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

	editFormReq := newRequest("GET", "/forms/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	editFormRec := httptest.NewRecorder()
	mux.ServeHTTP(editFormRec, editFormReq)
	if !strings.Contains(editFormRec.Body.String(), `<input type="hidden" name="_version" value="1">`) {
		t.Fatalf("expected a freshly created record's edit form to carry _version=1, got:\n%s", editFormRec.Body.String())
	}
}

// TestAPI_UpdateRecord_StaleVersionReturns409JSON is optimistic locking's
// real-world scenario over the JSON API: two clients both read the
// record, one saves first (moving its version on), and the second's save
// — built against the version it originally read — must be rejected
// rather than silently winning and erasing the first save.
func TestAPI_UpdateRecord_StaleVersionReturns409JSON(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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

	// The first save (against version 1, the record's version at create)
	// succeeds and moves the record to version 2.
	firstUpdateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid",
		[]byte(`{"name":"Editor A's change","_version":1}`))
	firstUpdateRec := httptest.NewRecorder()
	mux.ServeHTTP(firstUpdateRec, firstUpdateReq)
	if firstUpdateRec.Code != http.StatusOK {
		t.Fatalf("expected the first update to succeed with 200, got %d: %s", firstUpdateRec.Code, firstUpdateRec.Body.String())
	}

	// The second save was built against the same version 1 (it read the
	// record before the first save happened) — must be rejected, not
	// silently applied on top of Editor A's change.
	secondUpdateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid",
		[]byte(`{"name":"Editor B's change","_version":1}`))
	secondUpdateRec := httptest.NewRecorder()
	mux.ServeHTTP(secondUpdateRec, secondUpdateReq)
	if secondUpdateRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a stale _version, got %d: %s", secondUpdateRec.Code, secondUpdateRec.Body.String())
	}

	getReq := newRequest("GET", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Editor A's change") {
		t.Fatalf("expected Editor A's change to have survived, got:\n%s", getRec.Body.String())
	}
	if strings.Contains(getRec.Body.String(), "Editor B's change") {
		t.Fatalf("expected Editor B's rejected change to NOT be persisted, got:\n%s", getRec.Body.String())
	}
}

// TestAPI_UpdateRecord_MissingVersionSkipsCheck confirms backward
// compatibility explicitly: a JSON caller that never sends "_version" (as
// every API client/test predating this feature does) keeps updating
// unconditionally — optimistic locking is opt-in per request, not a
// breaking change to the existing contract.
func TestAPI_UpdateRecord_MissingVersionSkipsCheck(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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

	// Two consecutive updates, neither sending _version — both must
	// succeed even though the record's real version moved on between them.
	for i, name := range []string{"First Edit", "Second Edit"} {
		body := fmt.Appendf(nil, `{"name":%q}`, name)
		req := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update %d (no _version): expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, itemWithFlagEntityDef(), itemWithFlagFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)
	// Deliberately don't publish anything.

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	tenantB, dbB := newTestTenant(t, router)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, dbB, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	_, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/api/records/Vendor", nil) // no X-Tenant-ID/X-Actor-ID
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

// TestAPI_Dashboard_ListsEntityTypesWithPublishedForms confirms the
// root page ("/") lists an entity type only when it has BOTH a
// published entity Definition and a published Form Definition — a link
// to an entity with no form would just 404.
// TestAPI_Dashboard_ShowsHubNodePerModule is the regression test for the
// hub-and-spoke home page (see dashboard.go's hubLayout): one connected,
// clickable node per module the tenant has access to, not a flat list
// of entity types. vendorEntityDef has no Module set, so it falls into
// the "general" bucket — accessibleModules' documented degrade path for
// an entity Definition that never declared one.
func TestAPI_Dashboard_ShowsHubNodePerModule(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<script src="/static/htmx.min.js"></script>`) {
		t.Fatalf("expected the dashboard to load htmx.js like every other page navigation, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-hub-node uc-hub-node-0" data-search="" href="/modules/general"`) {
		t.Fatalf("expected a hub node linking to the general module, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-hub-lines"`) {
		t.Fatalf("expected the connecting-line svg, got:\n%s", body)
	}
	if strings.Contains(body, `href="/forms/Vendor/new"`) {
		t.Fatalf("expected no direct entity links on the hub itself — that's the module menu's job, got:\n%s", body)
	}
}

// TestAPI_Dashboard_ShowsPlaceholderModulesWithIcons is the regression
// test for "add all ERP modules, coming soon, colorful with icons":
// every standard ERP domain this kernel doesn't have a real module for
// yet still gets a hub node — muted, non-clickable, badged "Coming
// soon" — rather than being left off the hub entirely just because
// there's no real module behind it.
func TestAPI_Dashboard_ShowsPlaceholderModulesWithIcons(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `uc-hub-node-placeholder`) {
		t.Fatalf("expected at least one placeholder module node, got:\n%s", body)
	}
	if !strings.Contains(body, "Coming soon") {
		t.Fatalf("expected the coming-soon badge text, got:\n%s", body)
	}
	if !strings.Contains(body, `<span class="uc-hub-node-icon">💰</span>Finance`) {
		t.Fatalf("expected a Finance placeholder node with its icon, got:\n%s", body)
	}
	if strings.Contains(body, `href="/modules/finance"`) {
		t.Fatalf("expected the Finance placeholder to be non-clickable (no real module yet), got:\n%s", body)
	}
}

// TestAPI_Dashboard_RealModuleTakesOverPlaceholderSlot confirms a real
// module never shows up twice — once as itself, once as its own
// "coming soon" placeholder — when its key happens to match one of
// plannedModuleKeys.
func TestAPI_Dashboard_RealModuleTakesOverPlaceholderSlot(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	entDef := vendorEntityDef()
	entDef.Module = "finance"
	entRaw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	entRepo := data.NewEntityDefinitionRepo(db)
	if _, err := entRepo.CreateDraft(ctx, entDef.EntityType, entDef.Version, entRaw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := entRepo.Approve(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := entRepo.Publish(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	formDef := vendorFormDef()
	formRaw, err := json.Marshal(formDef)
	if err != nil {
		t.Fatalf("marshal form: %v", err)
	}
	formRepo := data.NewFormDefinitionRepo(db)
	if _, err := formRepo.CreateDraft(ctx, formDef.EntityType, formDef.Version, formRaw, actor); err != nil {
		t.Fatalf("CreateDraft form: %v", err)
	}
	if err := formRepo.Approve(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Approve form: %v", err)
	}
	if err := formRepo.Publish(ctx, formDef.EntityType, formDef.Version, actor); err != nil {
		t.Fatalf("Publish form: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="/modules/finance"`) {
		t.Fatalf("expected Finance to be a real, clickable module, got:\n%s", body)
	}
	// Finance's icon (💰) should render exactly once on the hub — twice
	// would mean it's showing up both as the real module AND as its own
	// "coming soon" placeholder.
	if n := strings.Count(body, "💰"); n != 1 {
		t.Fatalf("expected Finance's icon exactly once (real module, not also a placeholder), got %d occurrences in:\n%s", n, body)
	}
}

// TestAPI_ModuleMenu_ShowsEntitiesAsSearchableHubNodes confirms the
// graphical module menu (modulemenu.go, changed 2026-07-26 from a flat
// <ul> to the same hub-and-spoke graphic the dashboard's own module
// switcher uses — Farshid: "purchasing is only a list of menu... put
// them in graphical mode and searchable"): each entity type renders as
// its own hub node (icon, translated name, technical code sub-label),
// still filterable by the same search box, still linking to the
// entity's own list page. New/Import are no longer shown inline here
// (dropped along with the flat list) — reachable one click further, via
// that list page's own toolbar (listview.go), same distance the
// dashboard's own module nodes already put every entity-level action
// behind.
func TestAPI_ModuleMenu_ShowsEntitiesAsSearchableHubNodes(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/modules/general", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="uc-menu-search"`) {
		t.Fatalf("expected a search box, got:\n%s", body)
	}
	if !strings.Contains(body, `data-search="vendor vendor"`) {
		t.Fatalf("expected a lowercased searchable key combining Vendor's display name and code, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-hub-node uc-hub-node-0" data-search="vendor vendor" href="/records/Vendor"`) {
		t.Fatalf("expected Vendor rendered as its own hub node linking to its list page, got:\n%s", body)
	}
	if !strings.Contains(body, `<span class="uc-hub-node-code">Vendor</span>`) {
		t.Fatalf("expected Vendor's technical code shown as the node's sub-label, got:\n%s", body)
	}
}

func TestAPI_ModuleMenu_UnknownKeyIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/modules/no-such-module", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_ModuleMenu_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/modules/general", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

// TestAPI_Dashboard_OmitsEntityWithoutPublishedForm is the regression
// test for the "would just 404" reasoning above: an entity published
// with no matching form must not appear at all.
func TestAPI_Dashboard_OmitsEntityWithoutPublishedForm(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	// Publish only the entity Definition — deliberately no form.
	entDef := vendorEntityDef()
	entRaw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	entRepo := data.NewEntityDefinitionRepo(db)
	if _, err := entRepo.CreateDraft(ctx, entDef.EntityType, entDef.Version, entRaw, actor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := entRepo.Approve(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := entRepo.Publish(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Vendor") {
		t.Fatalf("expected Vendor to be omitted (no published form), got:\n%s", rec.Body.String())
	}
}

// isJSONEnvelope reports whether body is the API's {"data":...,"error":...}
// JSON envelope (CLAUDE.md's API convention) — as opposed to a rendered
// HTML page that merely happens to contain the substring "error" somewhere
// (an inline <script>, a CSS class, user-facing copy), which a bare
// strings.Contains(body, `"error"`) check can't tell apart.
func isJSONEnvelope(body string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	_, hasData := m["data"]
	_, hasError := m["error"]
	return hasData && hasError
}

func TestIsJSONEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty body", "", false},
		{"whitespace only", "   \n", false},
		{"bare null", "null", false},
		{"empty object", "{}", false},
		{"real envelope", `{"data":null,"error":null}`, true},
		{"real envelope with trailing newline", "{\"data\":{\"id\":\"1\"},\"error\":null}\n", true},
		{"envelope plus an extra key", `{"data":null,"error":null,"meta":{"page":1}}`, true},
		{"html containing the word error", `<html><body class="error-banner">"error" is not JSON</body></html>`, false},
		{"html with a trailing json-looking blob", "<!doctype html>\n{\"data\":null,\"error\":\"x\"}", false},
		{"json array, not an object", `[{"data":null,"error":null}]`, false},
		{"data only, no error key", `{"data":null}`, false},
		{"error only, no data key", `{"error":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJSONEnvelope(tc.body); got != tc.want {
				t.Fatalf("isJSONEnvelope(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestAPI_Dashboard_AnonymousShowsWelcomePage confirms "/" never returns
// the raw {"data":null,"error":...} JSON blob every other route does on
// a 401 — a browser landing on the site with no session gets a real HTML
// welcome page instead, even on a deployment where dev-auth is enabled
// but this particular request just didn't carry the headers.
func TestAPI_Dashboard_AnonymousShowsWelcomePage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with a welcome page, got %d: %s", rec.Code, body)
	}
	if !strings.Contains(body, "<html") {
		t.Fatalf("expected a real HTML shell, got:\n%s", body)
	}
	if !strings.Contains(body, "developer auth enabled") {
		t.Fatalf("expected the dev-auth welcome hint, got:\n%s", body)
	}
	if isJSONEnvelope(body) {
		t.Fatalf("expected HTML, not a JSON error body, got:\n%s", body)
	}
}

// TestAPI_Dashboard_NoAuthBackendConfigured_ShowsWelcomeNotJSON is the
// regression test for the exact bug report that motivated renderRoot:
// on a deployment with neither webauth nor dev-auth configured (the
// public erp.universaltill.com state before webauth's Terraform is
// applied), "/" used to hard-401 with a raw JSON error body — a browser
// visitor should see an explanatory HTML page instead.
func TestAPI_Dashboard_NoAuthBackendConfigured_ShowsWelcomeNotJSON(t *testing.T) {
	router := newTestRouter(t)
	// Deliberately not calling withDevAuthEnabled(t): neither auth
	// backend is configured, matching the public deployment's actual
	// state until webauth's Zitadel Terraform is applied.

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with a welcome page, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if isJSONEnvelope(body) {
		t.Fatalf("expected HTML, not a JSON error body, got:\n%s", body)
	}
	if !strings.Contains(body, "does not have sign-in configured") {
		t.Fatalf("expected the no-auth-backend explanation, got:\n%s", body)
	}
}

// TestAPI_UnknownPathStill404s is the regression test for the real
// footgun in registering "GET /{$}": a plain "GET /" pattern in Go's
// net/http.ServeMux acts as a subtree catch-all and would have silently
// swallowed every unmatched path into the dashboard instead of a real
// 404 — "{$}" is the exact-match-only wildcard that avoids that.
func TestAPI_UnknownPathStill404s(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/this/path/does/not/exist", "", "", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected a genuine 404 for an unmatched path, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_RenderNewForm_ProducesHTML is the first genuine end-to-end
// proof of the whole point of this increment: a Definition published
// through the real registry, rendered to real HTML through formrender,
// served over real HTTP.
func TestAPI_RenderNewForm_ProducesHTML(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
func orderEntityDefWithVendorReference() *entity.Definition {
	return &entity.Definition{
		EntityType: "Order",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldReference, Target: "Vendor"},
		},
	}
}

func orderFormDefWithVendorReference() *form.Definition {
	return &form.Definition{
		EntityType: "Order",
		Version:    1,
		Sections: []form.Section{
			{Title: "Header", Component: form.ComponentFields, Fields: []form.FormField{{Name: "vendor_id", Label: "Vendor"}}},
		},
	}
}

// orderEntityDefWithJoinFilteredVendorReference is
// orderEntityDefWithVendorReference plus an entity-join TargetFilter on
// vendor_id (the TimeEntry.employee_id/PurchaseOrder.vendor_id shape) —
// used by TestAPI_RenderForm_ReferenceCreateButtonHiddenForJoinTargetFilter
// (finding #10) below.
func orderEntityDefWithJoinFilteredVendorReference() *entity.Definition {
	return &entity.Definition{
		EntityType: "Order",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldReference, Target: "Vendor", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "VendorRole", EntityField: "vendor_id", Field: "role_type", Value: "vendor"},
			}},
		},
	}
}

// TestAPI_RenderForm_ReferenceFieldRendersComboboxNotFullList is the
// end-to-end regression test for #24's actual scaling fix (formrender's
// own tests cover the template logic in isolation): a reference field
// renders as a searchable combobox targeting the referenced entity, and
// the new-record form must NOT pre-load every target record — that
// full-<select> behaviour is exactly what fell over at real
// customer-list scale. The candidate records are instead served on
// demand by /api/references/{target} (covered by reference_search_test.go).
func TestAPI_RenderForm_ReferenceFieldRendersComboboxNotFullList(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithVendorReference(), orderFormDefWithVendorReference())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating the Vendor, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/forms/Order/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="uc-ref" data-target="Vendor" data-field="vendor_id"`) {
		t.Fatalf("expected vendor_id to render as a combobox targeting Vendor, got:\n%s", body)
	}
	if strings.Contains(body, `<select id="vendor_id"`) {
		t.Fatalf("reference field must no longer render as a full <select>, got:\n%s", body)
	}
	// The scaling guarantee: a new form must NOT ship the target records'
	// data. If "Acme Textiles" appears in the new-form HTML, the old
	// preload-everything path is back.
	if strings.Contains(body, "Acme Textiles") {
		t.Fatalf("new form must not pre-load target records (found a vendor name in the body), got:\n%s", body)
	}
}

// TestAPI_RenderForm_HXRequestGetsFragmentNotFullPage is the direct,
// isolated regression test for the collateral change renderForm gained
// as part of #24's part 2 (universaltill/uc-infra#51): an HX-Request:
// true GET now gets a bare form fragment instead of the full shelled
// page. This diff's own independent review found the real-browser e2e
// test (internal/e2e/reference_picker_quick_create_test.go) does NOT
// actually prove this — chromedp's `body.innerHTML = html` on a full
// <!doctype html> document silently drops the <html>/<head>/<body> tags
// and never executes <script>, so every assertion there still passes
// even with this branch reverted to always returning the full page.
// This test asserts on the raw response body directly, at the layer
// that's actually authoritative: no <!doctype>, no <nav> (the shell's
// own chrome), and a Vary: HX-Request header so an intermediate cache
// can't serve the wrong representation to the wrong kind of request.
func TestAPI_RenderForm_HXRequestGetsFragmentNotFullPage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// A real browser navigation (no HX-Request) still gets the full
	// shelled page — the pre-existing, unchanged behaviour.
	pageReq := newRequest("GET", "/forms/Vendor/new", tenantID, "farshid", nil)
	pageRec := httptest.NewRecorder()
	mux.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", pageRec.Code, pageRec.Body.String())
	}
	pageBody := pageRec.Body.String()
	if !strings.Contains(pageBody, "<!doctype html>") || !strings.Contains(pageBody, `class="uc-nav"`) {
		t.Fatalf("expected a real browser navigation to still get the full shelled page, got:\n%s", pageBody)
	}
	if got := pageRec.Header().Get("Vary"); got != "HX-Request" {
		t.Fatalf("expected Vary: HX-Request on the full-page response, got %q", got)
	}

	// An htmx-issued GET (what the quick-create modal's fetch sends,
	// layout.go) gets a bare fragment: the form itself, and nothing of
	// the page shell around it — no <!doctype>, no <nav>. A regression
	// that started nesting the full page into the modal (exactly the
	// independent review's concrete worry) would fail this assertion
	// immediately, unlike the e2e test.
	fragReq := newRequest("GET", "/forms/Vendor/new", tenantID, "farshid", nil)
	fragReq.Header.Set("HX-Request", "true")
	fragRec := httptest.NewRecorder()
	mux.ServeHTTP(fragRec, fragReq)
	if fragRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", fragRec.Code, fragRec.Body.String())
	}
	fragBody := fragRec.Body.String()
	if strings.Contains(fragBody, "<!doctype html>") || strings.Contains(fragBody, `class="uc-nav"`) {
		t.Fatalf("expected HX-Request to get a bare fragment with no page shell, got:\n%s", fragBody)
	}
	if !strings.HasPrefix(strings.TrimSpace(fragBody), `<form class="uc-form"`) {
		t.Fatalf("expected the fragment to start with the form tag itself, got:\n%s", fragBody)
	}
	if got := fragRec.Header().Get("Vary"); got != "HX-Request" {
		t.Fatalf("expected Vary: HX-Request on the fragment response, got %q", got)
	}

	// The SAME change applies to the existing-record route
	// (/forms/{entityType}/{id}), not just /new — confirm it isn't
	// accidentally scoped to only the new-record path.
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
	editFragReq := newRequest("GET", "/forms/Vendor/"+created.Data.ID, tenantID, "farshid", nil)
	editFragReq.Header.Set("HX-Request", "true")
	editFragRec := httptest.NewRecorder()
	mux.ServeHTTP(editFragRec, editFragReq)
	editFragBody := editFragRec.Body.String()
	if strings.Contains(editFragBody, "<!doctype html>") || strings.Contains(editFragBody, `class="uc-nav"`) {
		t.Fatalf("expected an existing-record HX-Request GET to also get a bare fragment, got:\n%s", editFragBody)
	}
}

// TestAPI_RenderForm_ReferenceCreateButtonRendersWithPermission is the
// HTTP-level test for part 2 of #24 (universaltill/uc-infra#51): a viewer
// who can write the referenced entity (here, Vendor is not opted into
// RBAC at all — the default-open behaviour TestAPI_RBAC_EntityLevel_
// Enforced403 already covers) sees the "+ Create new {Entity}"
// affordance on the picker. formrender's own tests (render_test.go)
// cover the template logic for CreateNewLabel in isolation; this proves
// the real handler wiring — CanWrite resolution, entityDisplayName,
// i18n — actually reaches the rendered page.
func TestAPI_RenderForm_ReferenceCreateButtonRendersWithPermission(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, quickCreatableVendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithVendorReference(), orderFormDefWithVendorReference())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/Order/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<button type="button" class="uc-ref-create" data-target="Vendor">`) {
		t.Fatalf("expected a quick-create button targeting Vendor, got:\n%s", body)
	}
}

// TestAPI_RenderForm_ReferenceCreateButtonHiddenWithoutWritePermission is
// the negative counterpart, over real RBAC (not a hand-built formrender.
// Data): Order itself stays open to everyone (so the page renders at
// all) but Vendor is opted into RBAC with can_write=false for the
// requesting actor's role — the quick-create button for vendor_id must
// not render, the same CanWrite gate renderForm's own "new record" page
// already applies to itself (denyPageUnless), just scoped to the
// referenced entity instead of the page's own entity.
func TestAPI_RenderForm_ReferenceCreateButtonHiddenWithoutWritePermission(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	// QuickCreatable — this test's job is to prove CanWrite is enforced
	// independently of that flag, not merely that the flag itself would
	// already have hidden the button.
	publishEntityAndForm(t, db, quickCreatableVendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithVendorReference(), orderFormDefWithVendorReference())

	seedRBAC(t, db,
		map[string][]string{"order_clerk": {"user-order-clerk"}},
		[]map[string]any{
			{"role": "order_clerk", "entity_type": "Vendor", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/Order/new", tenantID, "user-order-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (Order itself is unrestricted), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The literal opening tag, not a bare "uc-ref-create" substring check
	// — the page's own shell script (layout.go) always contains that
	// class name in its click-delegation JS regardless of whether any
	// button actually rendered, so a bare substring match would pass
	// even with the fix reverted.
	if strings.Contains(body, `<button type="button" class="uc-ref-create"`) {
		t.Fatalf("expected no quick-create button without Vendor write permission, got:\n%s", body)
	}
	// The search/select half of the picker must still work — this actor
	// can READ Vendor, just not create one.
	if !strings.Contains(body, `class="uc-ref" data-target="Vendor" data-field="vendor_id"`) {
		t.Fatalf("expected the vendor_id combobox to still render, got:\n%s", body)
	}
}

// TestAPI_RenderForm_ReferenceCreateButtonHiddenWithoutQuickCreatable is
// the regression test for the independent review's finding on this
// feature (part 2 of #24, universaltill/uc-infra#51): CanWrite alone is
// NOT sufficient to offer quick-create. Vendor here is plain
// vendorEntityDef() — QuickCreatable defaults false, and RBAC is left
// entirely unconfigured (default-open, full CanWrite) — the exact shape
// of foundation's real Status entity, which is a wide-open-by-default
// FieldReference target with no Permission row denying it, but whose
// fields (is_initial, is_terminal, status_type_id) are graph-shaping and
// were never meant to be one click away from an unrelated order form.
// Before this fix, this exact scenario rendered a quick-create button.
func TestAPI_RenderForm_ReferenceCreateButtonHiddenWithoutQuickCreatable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithVendorReference(), orderFormDefWithVendorReference())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/Order/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `<button type="button" class="uc-ref-create"`) {
		t.Fatalf("expected no quick-create button for a non-QuickCreatable target despite full CanWrite, got:\n%s", body)
	}
}

// TestAPI_RenderForm_ReferenceCreateButtonHiddenForJoinTargetFilter is
// the regression test for independent review finding #10: quick-create
// from a picker can only POST a bare new record of the target type — it
// has no way to also create the separate joined-entity row (e.g. a
// PartyRole/VendorRole) an entity-join TargetFilter condition requires,
// which would guarantee the very next save 400s with no way to fix it
// from that dialog. Vendor here IS QuickCreatable and this actor DOES
// hold CanWrite on it (both other gates pass), isolating that the
// suppression is specifically about the join TargetFilter shape.
func TestAPI_RenderForm_ReferenceCreateButtonHiddenForJoinTargetFilter(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, quickCreatableVendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithJoinFilteredVendorReference(), orderFormDefWithVendorReference())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/forms/Order/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `<button type="button" class="uc-ref-create"`) {
		t.Fatalf("expected no quick-create button for a field with a join TargetFilter, got:\n%s", body)
	}
	// The search/select half of the picker must still work.
	if !strings.Contains(body, `class="uc-ref" data-target="Vendor" data-field="vendor_id"`) {
		t.Fatalf("expected the vendor_id combobox to still render, got:\n%s", body)
	}
}

// TestAPI_ReferenceSearch_WithoutNameFieldFallsBackToID confirms the
// search endpoint labels a target entity with no "name" field by its raw
// id, rather than an error or an empty label. The label-fallback chain
// (name -> title -> id) now lives in searchReferenceOptions.
func TestAPI_ReferenceSearch_WithoutNameFieldFallsBackToID(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	noNameEntDef := &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields:     []entity.Field{{Name: "code", Type: entity.FieldString, Required: true}},
	}
	noNameFormDef := &form.Definition{
		EntityType: "Vendor",
		Version:    1,
		Sections:   []form.Section{{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "code", Label: "Code"}}}},
	}
	publishEntityAndForm(t, db, noNameEntDef, noNameFormDef)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"code":"V-001"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != created.Data.ID {
		t.Fatalf("expected the option labelled by id when the target has no name field, got %+v", opts)
	}
}

// TestAPI_ReferenceSearch_FallsBackToTitleField is the regression test
// for the Position.reports_to_position_id picker (and any other entity
// with a "title" field but no "name" field, e.g. IssueReport): the
// label-field fallback chain (name -> title -> id) must resolve to
// "title", since Position has no "name" field per reference-data-model.md
// §7's own spec (title is its label). Deliberately does NOT extend to a
// "code" fallback (see referenceLabelFieldCandidates' own doc comment) —
// its sibling TestAPI_ReferenceSearch_WithoutNameFieldFallsBackToID pins
// that a code-only entity still falls back to id.
func TestAPI_ReferenceSearch_FallsBackToTitleField(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	titleOnlyEntDef := &entity.Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields:     []entity.Field{{Name: "title", Type: entity.FieldString, Required: true}},
	}
	titleOnlyFormDef := &form.Definition{
		EntityType: "Vendor",
		Version:    1,
		Sections:   []form.Section{{Title: "Details", Component: form.ComponentFields, Fields: []form.FormField{{Name: "title", Label: "Title"}}}},
	}
	publishEntityAndForm(t, db, titleOnlyEntDef, titleOnlyFormDef)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"title":"Regional Manager"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/api/references/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	opts := decodeRefOptions(t, rec.Body.Bytes())
	if len(opts) != 1 || opts[0].Label != "Regional Manager" {
		t.Fatalf("expected the option labelled by its title field (not its raw id), got %+v", opts)
	}
}

func TestAPI_RenderRecordForm_ShowsMasterDetailChildren(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, poLineEntityDef(), poLineFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	// "Total", not "total": the label resolves through
	// field.PurchaseOrder.total (#99), not the raw RollUpTarget field
	// name — see formrender's own TestRender_MasterDetailRollUp.
	if !strings.Contains(body, "Total: 150.5") {
		t.Fatalf("expected the roll-up to sum the child's line_total into the header total, got:\n%s", body)
	}
}

// itemEntityDef/itemFormDef and poLineWithItemRefEntityDef/
// poLineWithItemRefFormDef are dedicated fixtures for
// TestAPI_RenderRecordForm_MasterDetail* below (uc-infra#85) rather
// than extending poLineEntityDef/poLineFormDef: those two are shared by
// TestAPI_RenderRecordForm_ShowsMasterDetailChildren and other tests
// above, and poLineEntityDef deliberately has no FieldReference column
// (formrender's own unit tests already cover that shape in isolation —
// this file's gap, per independent review, was that NO internal/api
// integration test exercised a master_detail child row with a
// FieldReference column against real Postgres/real RBAC at all).
func itemEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Item",
		Version:    1,
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldString, Required: true}},
	}
}

func itemFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Item",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields:    []form.FormField{{Name: "name", Label: "Name"}},
		}},
	}
}

func poLineWithItemRefEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "POLine",
		Version:    1,
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldString, Required: true},
			{Name: "item_id", Type: entity.FieldReference, Target: "Item"},
			{Name: "line_total", Type: entity.FieldNumber, Required: true},
		},
	}
}

func poLineWithItemRefFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "POLine",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields: []form.FormField{
				{Name: "item_id", Label: "Item"},
				{Name: "line_total", Label: "Line Total"},
			},
		}},
	}
}

// seedEntityPermission grants roleCode entity-level RBAC rules for
// entityType (foundation.Permission — can_read/can_write), creates the
// role, and grants it to userID — the master_detail-column analogue of
// seedFieldRule above, at the entity level (authz.Resolver.CanRead)
// rather than the field level (authz.Resolver.HiddenFields). Mirrors
// internal/kernel/authz's own test fixture (authz_test.go's
// fixture.permit), just seeded through the real HTTP-facing crud.Engine
// path rather than authz's internal test harness.
func seedEntityPermission(t *testing.T, db *sql.DB, roleCode, userID, entityType string, canRead, canWrite bool) string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()

	role, err := engine.Create(ctx, foundation.Role(), map[string]any{"code": roleCode, "name": roleCode}, actor)
	if err != nil {
		t.Fatalf("create Role %s: %v", roleCode, err)
	}
	if _, err := engine.Create(ctx, foundation.UserRole(), map[string]any{"user_id": userID, "role_id": role.ID}, actor); err != nil {
		t.Fatalf("grant %s to %s: %v", roleCode, userID, err)
	}
	if _, err := engine.Create(ctx, foundation.Permission(), map[string]any{
		"role_id": role.ID, "entity_type": entityType, "can_read": canRead, "can_write": canWrite,
	}, actor); err != nil {
		t.Fatalf("create Permission for %s on %s: %v", roleCode, entityType, err)
	}
	return role.ID
}

// TestAPI_RenderRecordForm_MasterDetailReferenceColumnResolvesToLabel
// (uc-infra#85): the internal/api-layer counterpart to formrender's own
// unit tests — a master_detail child row's FieldReference column
// resolves to the target's label against a REAL published Item and a
// REAL Postgres round trip, not a synthetic ChildReferenceLabels map
// handed to the renderer directly. This is the path formrender's tests
// can't reach: whether loadMasterDetailChildren/pageReferenceLabels
// actually wire the lookup up end to end.
func TestAPI_RenderRecordForm_MasterDetailReferenceColumnResolvesToLabel(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, poLineWithItemRefEntityDef(), poLineWithItemRefFormDef())
	publishEntityAndForm(t, db, itemEntityDef(), itemFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	itemReq := newRequest("POST", "/api/records/Item", tenantID, "farshid", []byte(`{"name":"Acme Widget"}`))
	itemRec := httptest.NewRecorder()
	mux.ServeHTTP(itemRec, itemReq)
	var item struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(itemRec.Body.Bytes(), &item); err != nil {
		t.Fatalf("unmarshal Item: %v", err)
	}

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

	lineBody := []byte(`{"purchase_order_id":"` + po.Data.ID + `","item_id":"` + item.Data.ID + `","line_total":150.5}`)
	lineReq := newRequest("POST", "/api/records/POLine", tenantID, "farshid", lineBody)
	lineRec := httptest.NewRecorder()
	mux.ServeHTTP(lineRec, lineReq)
	if lineRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating POLine, got %d: %s", lineRec.Code, lineRec.Body.String())
	}

	formReq := newRequest("GET", "/forms/PurchaseOrder/"+po.Data.ID, tenantID, "farshid", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", formRec.Code, formRec.Body.String())
	}
	body := formRec.Body.String()
	if !strings.Contains(body, "Acme Widget") {
		t.Errorf("expected the master-detail item_id column resolved to the item's name, got:\n%s", body)
	}
	if strings.Contains(body, item.Data.ID) {
		t.Errorf("expected the raw item id NOT to leak into the rendered form once a label was available, got:\n%s", body)
	}
}

// TestAPI_RenderRecordForm_MasterDetailReferenceColumnFallsBackToRawIDWhenTargetUnreadable
// (uc-infra#85): the RBAC-degrade path CLAUDE.md's multi-tenancy
// discipline treats as highest-stakes — a viewer who can read
// PurchaseOrder/POLine but NOT Item must still get a working form (200,
// not an error, not a hidden section), with the unreadable reference's
// cell falling back to the raw id exactly like an unresolvable/dangling
// one already does (childCellValue's documented fallback), never a
// label the viewer has no permission to see. formrender's own unit
// tests already prove the RENDERER's fallback given a map that's
// missing an entry; this proves the missing entry is what a real
// CanRead denial actually produces end to end.
func TestAPI_RenderRecordForm_MasterDetailReferenceColumnFallsBackToRawIDWhenTargetUnreadable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, purchaseOrderEntityDef(), purchaseOrderFormDef())
	publishEntityAndForm(t, db, poLineWithItemRefEntityDef(), poLineWithItemRefFormDef())
	publishEntityAndForm(t, db, itemEntityDef(), itemFormDef())
	// Entity-level RBAC opts in the moment ANY Permission row exists for
	// ANY entity type in the tenant (authz.Resolver.resolve's
	// rulesExist), so PurchaseOrder/POLine need their own explicit
	// can_read=true grant for "restricted" once Item's row exists —
	// otherwise this test would 403 on the form itself instead of
	// isolating the one column under test.
	seedEntityPermission(t, db, "restricted", "user-restricted", "Item", false, false)
	seedEntityPermission(t, db, "restricted", "user-restricted", "PurchaseOrder", true, true)
	seedEntityPermission(t, db, "restricted", "user-restricted", "POLine", true, true)

	engine := crud.NewEngine(db)
	item, err := engine.Create(ctx, itemEntityDef(), map[string]any{"name": "Acme Widget"}, humanActor())
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	po, err := engine.Create(ctx, purchaseOrderEntityDef(), map[string]any{"vendor_id": "v1"}, humanActor())
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	if _, err := engine.Create(ctx, poLineWithItemRefEntityDef(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "line_total": 150.5,
	}, humanActor()); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	formReq := newRequest("GET", "/forms/PurchaseOrder/"+po.ID, tenantID, "user-restricted", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusOK {
		t.Fatalf("expected 200 (the form itself must still render even though one child column is unresolvable), got %d: %s", formRec.Code, formRec.Body.String())
	}
	body := formRec.Body.String()
	if strings.Contains(body, "Acme Widget") {
		t.Errorf("expected the item's name NOT to leak to a viewer without read access to Item, got:\n%s", body)
	}
	if !strings.Contains(body, item.ID) {
		t.Errorf("expected the item_id column to fall back to the raw id when the target is unreadable, got:\n%s", body)
	}
}

// TestAPI_ServesHTMXScript_Unauthenticated confirms /static/htmx.min.js
// is reachable without dev-auth headers — it has to be, since the page
// requesting it (a real browser navigating to a route DevAuth would
// otherwise gate) hasn't authenticated at the point it fetches its own
// <script> tag.
func TestAPI_ServesHTMXScript_Unauthenticated(t *testing.T) {
	router := newTestRouter(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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

// TestAPI_ServesCSS_Unauthenticated confirms app.css serves at its
// actual content-hashed path (see layout.go's appCSSPath) — not a
// fixed "/static/app.css", which is exactly the stale-cache bug this
// hashing fixed (a browser that had ever loaded the app before kept
// serving a year-old immutable-cached stylesheet).
func TestAPI_ServesCSS_Unauthenticated(t *testing.T) {
	router := newTestRouter(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", appCSSPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no auth headers, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Fatalf("expected a css content type, got %q", ct)
	}
}

// TestAPI_Shell_LinksToHashedCSSPath confirms every rendered page
// actually links to the same hashed path serveCSS answers at — the
// two must never drift, or every page silently 404s its own stylesheet.
func TestAPI_Shell_LinksToHashedCSSPath(t *testing.T) {
	router := newTestRouter(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `href="`+appCSSPath+`"`) {
		t.Fatalf("expected the page to link to %s, got:\n%s", appCSSPath, rec.Body.String())
	}
}

// TestAPI_RecordList_ShowsExistingRecords is the regression test for
// the actual gap Farshid found logging in for the first time: the
// dashboard only ever linked to "New" (a blank form) and "Import" —
// there was nowhere to go look at records that already existed short of
// the JSON-only GET /api/records/{entityType}.
func TestAPI_RecordList_ShowsExistingRecords(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/records/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Acme Textiles") {
		t.Fatalf("expected the record's data in the list page, got:\n%s", body)
	}
	if !strings.Contains(body, `<script src="/static/htmx.min.js"></script>`) {
		t.Fatalf("expected the list page to load htmx.js like every other page navigation, got:\n%s", body)
	}
	if !strings.Contains(body, `href="/forms/Vendor/new"`) {
		t.Fatalf("expected a link to the Vendor new-record form, got:\n%s", body)
	}
}

// TestAPI_RecordList_ReferenceColumnShowsLabelNotRawID is the
// regression test for Farshid pointing out the list page showed "long
// guid numbers which is not useful" — the reference-dropdown fix
// (2026-07-20) only fixed the form view; list rows still showed a
// reference field's raw stored id. Now resolves to the target record's
// own label, the same lookup the form's dropdown already uses.
func TestAPI_RecordList_ReferenceColumnShowsLabelNotRawID(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, orderEntityDefWithVendorReference(), orderFormDefWithVendorReference())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createVendor := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createVendorRec := httptest.NewRecorder()
	mux.ServeHTTP(createVendorRec, createVendor)
	var vendor struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createVendorRec.Body.Bytes(), &vendor); err != nil {
		t.Fatalf("unmarshal vendor create response: %v", err)
	}

	createOrder := newRequest("POST", "/api/records/Order", tenantID, "farshid",
		[]byte(`{"vendor_id":"`+vendor.Data.ID+`"}`))
	createOrderRec := httptest.NewRecorder()
	mux.ServeHTTP(createOrderRec, createOrder)
	if createOrderRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating the Order, got %d: %s", createOrderRec.Code, createOrderRec.Body.String())
	}

	req := newRequest("GET", "/records/Order", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, ">Acme Textiles</a></td>") {
		t.Fatalf("expected the vendor's name resolved in the list cell, got:\n%s", body)
	}
	if strings.Contains(body, ">"+vendor.Data.ID+"</a></td>") {
		t.Fatalf("expected no raw vendor id shown as a cell value, got:\n%s", body)
	}
}

// TestAPI_RecordList_EnumColumnShowsTranslatedLabel confirms an enum
// field's list-page cell shows its translated label ("field.Item.
// item_type.stock" -> "Stock"), not the raw stored value — the same
// "field data like status should be multilingual" gap Farshid pointed
// out, on the list page rather than just the form.
func TestAPI_RecordList_EnumColumnShowsTranslatedLabel(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	itemDef := &entity.Definition{
		EntityType: "Item",
		Version:    1,
		Fields: []entity.Field{
			{Name: "item_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"stock", "service", "non_stock"}},
		},
	}
	itemFormDef := &form.Definition{
		EntityType: "Item",
		Version:    1,
		Sections: []form.Section{{
			Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "item_type", Label: "Type"}},
		}},
	}
	publishEntityAndForm(t, db, itemDef, itemFormDef)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Item", tenantID, "farshid", []byte(`{"item_type":"stock"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/records/Item", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, ">Stock</a></td>") {
		t.Fatalf("expected the translated label \"Stock\", got:\n%s", body)
	}
	if strings.Contains(body, ">stock</a></td>") {
		t.Fatalf("expected no raw untranslated \"stock\" value shown, got:\n%s", body)
	}
}

// TestAPI_RecordList_ColumnHeaderIsTranslated confirms list-page column
// headers resolve through "field.{EntityType}.{FieldName}" rather than
// showing the raw snake_case field name regardless of locale — the other
// half of the gap QUEUE.md flagged (per-field labels worked on forms via
// the enum-translation branch, but list columns were never wired up).
func TestAPI_RecordList_ColumnHeaderIsTranslated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	poDef := &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Fields:     []entity.Field{{Name: "po_number", Type: entity.FieldString, Required: true}},
	}
	poFormDef := &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Sections: []form.Section{{
			Title: "Header", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "po_number", Label: "PO Number"}},
		}},
	}
	publishEntityAndForm(t, db, poDef, poFormDef)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Column headers only render alongside a non-empty table (the empty
	// state is a plain "no records yet" message, no <thead> at all) —
	// same reason TestAPI_RecordList_EnumColumnShowsTranslatedLabel above
	// creates a record first.
	createReq := newRequest("POST", "/api/records/PurchaseOrder", tenantID, "farshid", []byte(`{"po_number":"PO-1"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/records/PurchaseOrder?lang=ar", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "رقم أمر الشراء") {
		t.Fatalf("expected the Arabic column header \"رقم أمر الشراء\" for po_number, got:\n%s", body)
	}
	if strings.Contains(body, "<th>po_number</th>") {
		t.Fatalf("expected no raw untranslated \"po_number\" column header, got:\n%s", body)
	}
}

// TestAPI_RecordList_PaginatesBeyondPageSize confirms the list page
// actually bounds its query (listPageSize rows per page, not "every
// record, unpaginated" — QUEUE.md's flagged gap) and that Prev/Next
// correctly appear/disappear at the boundaries: page 1 of a two-page set
// has a Next but no Previous, page 2 has a Previous but no Next, and the
// records shown are the right slice, not duplicated or dropped.
func TestAPI_RecordList_PaginatesBeyondPageSize(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	total := listPageSize + 5
	for i := range total {
		body := fmt.Appendf(nil, `{"name":"Vendor-%02d"}`, i)
		createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", body)
		createRec := httptest.NewRecorder()
		mux.ServeHTTP(createRec, createReq)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("create Vendor-%02d: expected 201, got %d: %s", i, createRec.Code, createRec.Body.String())
		}
	}

	page1Req := newRequest("GET", "/records/Vendor", tenantID, "farshid", nil)
	page1Rec := httptest.NewRecorder()
	mux.ServeHTTP(page1Rec, page1Req)
	page1Body := page1Rec.Body.String()

	if got := strings.Count(page1Body, "<tr onclick"); got != listPageSize {
		t.Fatalf("expected %d rows on page 1, got %d:\n%s", listPageSize, got, page1Body)
	}
	if !strings.Contains(page1Body, `href="/records/Vendor?page=2"`) {
		t.Fatalf("expected a link to page 2 on page 1, got:\n%s", page1Body)
	}
	if strings.Contains(page1Body, "Previous") {
		t.Fatalf("expected no Previous link on page 1, got:\n%s", page1Body)
	}
	if !strings.Contains(page1Body, "Page 1 of 2") {
		t.Fatalf("expected the page label \"Page 1 of 2\", got:\n%s", page1Body)
	}

	page2Req := newRequest("GET", "/records/Vendor?page=2", tenantID, "farshid", nil)
	page2Rec := httptest.NewRecorder()
	mux.ServeHTTP(page2Rec, page2Req)
	page2Body := page2Rec.Body.String()

	if got := strings.Count(page2Body, "<tr onclick"); got != total-listPageSize {
		t.Fatalf("expected %d rows on page 2, got %d:\n%s", total-listPageSize, got, page2Body)
	}
	if !strings.Contains(page2Body, `href="/records/Vendor?page=1"`) {
		t.Fatalf("expected a link back to page 1 on page 2, got:\n%s", page2Body)
	}
	if strings.Contains(page2Body, ">Next<") {
		t.Fatalf("expected no Next link on the last page, got:\n%s", page2Body)
	}

	// A page number past the end clamps to the last page rather than
	// showing an empty table a user could reach by editing the URL.
	pastEndReq := newRequest("GET", "/records/Vendor?page=99", tenantID, "farshid", nil)
	pastEndRec := httptest.NewRecorder()
	mux.ServeHTTP(pastEndRec, pastEndReq)
	if got := strings.Count(pastEndRec.Body.String(), "<tr onclick"); got != total-listPageSize {
		t.Fatalf("expected page=99 to clamp to the last page (%d rows), got %d", total-listPageSize, got)
	}
}

// TestAPI_RecordList_NoPagerWhenSinglePage confirms the pager itself is
// absent (not just disabled) when everything fits on one page — no
// "Page 1 of 1" noise for the common case.
func TestAPI_RecordList_NoPagerWhenSinglePage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Only Vendor"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	req := newRequest("GET", "/records/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "uc-list-pager") {
		t.Fatalf("expected no pager for a single page of results, got:\n%s", rec.Body.String())
	}
}

func TestAPI_RecordList_EmptyShowsEmptyMessage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/records/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No records yet") {
		t.Fatalf("expected the empty-state message, got:\n%s", rec.Body.String())
	}
}

func TestAPI_RecordList_UnknownEntityTypeIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/records/NoSuchEntity", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_RecordList_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/records/Vendor", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

// TestAPI_Nav_LinksToPublishedModules confirms the shared top nav (see
// nav.go) shows up on an authenticated page and links to each module's
// list page — the actual "go to a separate system" switcher Farshid
// asked about, not just a per-page New/Import link.
func TestAPI_Nav_LinksToPublishedModules(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="uc-nav"`) {
		t.Fatalf("expected a nav bar, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-nav-link" href="/modules/general"`) {
		t.Fatalf("expected a nav link to the general module's menu, got:\n%s", body)
	}
}

// TestAPI_Nav_AnonymousIsBrandOnly confirms the welcome page (no
// session) never tries to list modules for a tenant it doesn't have —
// nav degrades to brand-only rather than erroring or leaking anything.
func TestAPI_Nav_AnonymousIsBrandOnly(t *testing.T) {
	router := newTestRouter(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="uc-nav-brand"`) {
		t.Fatalf("expected a brand link, got:\n%s", body)
	}
	if strings.Contains(body, `uc-nav-link`) {
		t.Fatalf("expected no module links on the anonymous welcome page, got:\n%s", body)
	}
}

// TestAPI_Locale_QueryParamSetsCookieAndRTLDir is the regression test
// for the actual multilingual gap Farshid flagged: the i18n catalog
// existing server-side isn't the same as a visitor being able to use
// the app in Arabic. ?lang=ar must (1) actually switch rendered text,
// (2) flip the document to dir="rtl" (translated text in a
// left-to-right layout is still wrong), and (3) persist via a cookie so
// the very next click — a plain <a href> with no ?lang= of its own —
// doesn't silently revert to English.
func TestAPI_Locale_QueryParamSetsCookieAndRTLDir(t *testing.T) {
	router := newTestRouter(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/?lang=ar", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="ar" dir="rtl">`) {
		t.Fatalf("expected an RTL document for ar, got:\n%s", body)
	}
	if !strings.Contains(body, "يونيفرسال كور") {
		t.Fatalf("expected the Arabic brand string, got:\n%s", body)
	}

	var localeCookieSet bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "uc_locale" && c.Value == "ar" {
			localeCookieSet = true
		}
	}
	if !localeCookieSet {
		t.Fatalf("expected ?lang=ar to persist a uc_locale=ar cookie, got: %v", rec.Result().Cookies())
	}

	// The next request — a plain click with no ?lang= — must still be
	// Arabic, via the cookie alone.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: "uc_locale", Value: "ar"})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), `<html lang="ar" dir="rtl">`) {
		t.Fatalf("expected the locale cookie alone to keep the page in Arabic, got:\n%s", rec2.Body.String())
	}
}

func TestAPI_Locale_UnsupportedLangIgnored(t *testing.T) {
	router := newTestRouter(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/?lang=zz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `<html lang="en" dir="ltr">`) {
		t.Fatalf("expected an unsupported locale to fall back to English, got:\n%s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "uc_locale" {
			t.Fatalf("expected an unsupported locale to never be persisted into a cookie, got: %+v", c)
		}
	}
}

func TestAPI_Nav_ShowsLanguageSwitcher(t *testing.T) {
	router := newTestRouter(t)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="uc-nav-lang uc-nav-lang-active" href="/?lang=en"`) {
		t.Fatalf("expected an active English switcher link, got:\n%s", body)
	}
	if !strings.Contains(body, `class="uc-nav-lang" href="/?lang=ar"`) {
		t.Fatalf("expected an Arabic switcher link, got:\n%s", body)
	}
}

// TestAPI_Nav_ShowsLogoutOnlyWithRealLogin confirms the logout link
// never appears when webauth is disabled — /ui/logout isn't even
// registered on that deployment (see webauth.Authenticator.Register),
// so linking to it would be a dead link to a 404, not a working control.
// testHandler's Handler always has a nil *webauth.Authenticator
// (Enabled() == false), matching every dev-auth-only deployment.
func TestAPI_Nav_ShowsLogoutOnlyWithRealLogin(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `href="/ui/logout"`) {
		t.Fatalf("expected no logout link when webauth is disabled, got:\n%s", rec.Body.String())
	}
}

// TestNavTmpl_RendersLogoutLinkWhenShown is the positive-case sibling
// of TestAPI_Nav_ShowsLogoutOnlyWithRealLogin: that test only ever
// exercises ShowLogout=false (testHandler's Handler always has a nil
// *webauth.Authenticator, so Enabled() is always false — there's no way
// to construct a real "enabled" Authenticator from this package, since
// webauth.New requires live OIDC discovery). Exercises navTmpl directly
// with ShowLogout=true instead, confirming the template itself actually
// renders a working /ui/logout link when the view says to — the half
// of the gating logic the other test structurally cannot reach.
func TestNavTmpl_RendersLogoutLinkWhenShown(t *testing.T) {
	var buf bytes.Buffer
	if err := navTmpl.Execute(&buf, navView{
		Brand:       "Universal Core",
		Locale:      "en",
		CurrentPath: "/",
		Locales:     []string{"en"},
		ShowLogout:  true,
		LogoutLabel: "Log out",
	}); err != nil {
		t.Fatalf("execute navTmpl: %v", err)
	}
	if !strings.Contains(buf.String(), `<a class="uc-nav-link" href="/ui/logout">Log out</a>`) {
		t.Fatalf("expected a rendered logout link, got:\n%s", buf.String())
	}
}

// TestAPI_ModuleMenu_ShowsTranslatedEntityNames confirms a real shipped
// entity (not a test fixture) gets its actual translated display name,
// not just its raw technical EntityType — the same "backend i18n
// existing isn't the same as it being visible" gap the language
// switcher itself fixes, applied to entity labels.
func TestAPI_ModuleMenu_ShowsTranslatedEntityNames(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/modules/foundation?lang=ar", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "طرف") {
		t.Fatalf("expected Party's Arabic display name, got:\n%s", body)
	}
	if !strings.Contains(body, `<span class="uc-hub-node-code">Party</span>`) {
		t.Fatalf("expected Party's technical code shown alongside its name, got:\n%s", body)
	}
	// Department/Position are new alongside Party in this same module —
	// confirming their entity.{Type}.name keys actually landed (a wholly
	// missing key from every locale isn't caught by
	// i18n's TestLocales_HaveIdenticalKeySets, which only compares locale
	// files against each other) rather than silently falling back to the
	// raw "Department"/"Position" identifier catalog.TOrDefault would use.
	if !strings.Contains(body, "القسم") {
		t.Fatalf("expected Department's Arabic display name, got:\n%s", body)
	}
	if !strings.Contains(body, "المنصب") {
		t.Fatalf("expected Position's Arabic display name, got:\n%s", body)
	}
}

func TestAPI_RenderForm_UnknownRecordIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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

// workflowJobStatuses queries a tenant's workflow_jobs rows for
// entityType directly — the trigger-wiring tests below need to observe
// what triggerWorkflows actually enqueued without a worker process
// running to process it (this test file builds a bare Handler, not
// internal/worker.Runner), so "did it get enqueued at all, with the
// right shape" is checked at the DB level.
func workflowJobStatuses(t *testing.T, db *sql.DB, entityType string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT status FROM workflow_jobs WHERE entity_type = $1 ORDER BY created_at`, entityType)
	if err != nil {
		t.Fatalf("query workflow_jobs: %v", err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan status: %v", err)
		}
		statuses = append(statuses, s)
	}
	return statuses
}

// TestAPI_CreateRecord_TriggersOnCreateWorkflow is the point of wiring
// triggerWorkflows into createRecord at all: before this, workflow.
// Queue.Enqueue was reachable only from tests, since nothing in a real
// deployment ever called it — creating a record silently never started
// any workflow, no matter what on_create Definition was published.
func TestAPI_CreateRecord_TriggersOnCreateWorkflow(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishWorkflow(t, db, &workflow.Definition{
		Name: "vendor_onboarding", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	statuses := workflowJobStatuses(t, db, "Vendor")
	if len(statuses) != 1 || statuses[0] != "queued" {
		t.Fatalf("expected exactly one queued workflow job for the new Vendor, got %v", statuses)
	}
}

// TestAPI_CreateRecord_NoMatchingWorkflow_NoJobEnqueued confirms
// triggerWorkflows doesn't fire indiscriminately — a published workflow
// for a DIFFERENT entity type must not enqueue anything when an
// unrelated entity type is created.
func TestAPI_CreateRecord_NoMatchingWorkflow_NoJobEnqueued(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishWorkflow(t, db, &workflow.Definition{
		Name: "item_onboarding", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Item"},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	if statuses := workflowJobStatuses(t, db, "Vendor"); len(statuses) != 0 {
		t.Fatalf("expected no workflow job for an entity type with no matching trigger, got %v", statuses)
	}
}

// TestAPI_UpdateRecord_TriggersOnUpdateWorkflow is on_create's sibling
// case — an on_update-triggered workflow must fire on updateRecord, not
// just createRecord.
func TestAPI_UpdateRecord_TriggersOnUpdateWorkflow(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishWorkflow(t, db, &workflow.Definition{
		Name: "vendor_change_review", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnUpdate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepNotify}},
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

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
	// on_create doesn't match this workflow's trigger, so create alone
	// must not have enqueued anything yet.
	if statuses := workflowJobStatuses(t, db, "Vendor"); len(statuses) != 0 {
		t.Fatalf("expected no workflow job from create (trigger is on_update), got %v", statuses)
	}

	updateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", []byte(`{"name":"Acme Textiles Ltd"}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	statuses := workflowJobStatuses(t, db, "Vendor")
	if len(statuses) != 1 || statuses[0] != "queued" {
		t.Fatalf("expected exactly one queued workflow job after the update, got %v", statuses)
	}
}

// TestAPI_ApproveWorkflowJob_ResumesWaitingApproval exercises the HTTP
// endpoint ResumeAfterApproval's own doc comment says didn't exist yet.
// Drives a job to waiting_approval directly via workflow.Queue (standing
// in for the real worker, which isn't running in this test) to isolate
// what's actually under test: the HTTP layer correctly calling
// ResumeAfterApproval, not the worker's own poll loop (internal/worker
// already covers that).
func TestAPI_ApproveWorkflowJob_ResumesWaitingApproval(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval},
			{Kind: workflow.StepNotify},
		},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	// Stand in for the worker: claim and run the job to its
	// require_approval halt.
	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(context.Background(), workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}

	jobRepo := data.NewWorkflowJobRepo(db)
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}
	got, err := jobRepo.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "waiting_approval" {
		t.Fatalf("expected the job to be halted at waiting_approval before testing the approve endpoint, got %q", got.Status)
	}

	approveReq := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantID, "farshid", nil)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", approveRec.Code, approveRec.Body.String())
	}

	got, err = jobRepo.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("Get after approve: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected the job back to queued (ready for a worker to pick up the remaining steps) after approval, got %q", got.Status)
	}
	if got.StepIndex != 1 {
		t.Fatalf("expected step_index advanced past the require_approval step (0) to 1, got %d", got.StepIndex)
	}
}

// TestAPI_ApproveWorkflowJob_DeniesCallerWithoutNamedRole confirms the
// require_approval step's `role` param, previously decorative (see
// uc-infra ADR-0006's addendum), now actually gates who may resume the
// job: a caller who doesn't hold the named Role gets 403, and the job is
// left exactly as it was — still waiting_approval, not silently resumed.
func TestAPI_ApproveWorkflowJob_DeniesCallerWithoutNamedRole(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "cfo"}},
			{Kind: workflow.StepNotify},
		},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	// "farshid" holds no Role at all, let alone "cfo".
	approveReq := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantID, "farshid", nil)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a caller without the required role, got %d: %s", approveRec.Code, approveRec.Body.String())
	}

	jobRepo := data.NewWorkflowJobRepo(db)
	got, err := jobRepo.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("Get after denied approve: %v", err)
	}
	if got.Status != "waiting_approval" {
		t.Fatalf("expected the job to remain waiting_approval after a denied approve attempt, got %q", got.Status)
	}
}

// TestAPI_ApproveWorkflowJob_RoleHolderCanApprove is
// TestAPI_ApproveWorkflowJob_DeniesCallerWithoutNamedRole's positive
// sibling: once "farshid" actually holds the "cfo" Role via UserRole, the
// same require_approval step lets the approve endpoint through.
func TestAPI_ApproveWorkflowJob_RoleHolderCanApprove(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "cfo"}},
			{Kind: workflow.StepNotify},
		},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	roleRec := httptest.NewRecorder()
	mux.ServeHTTP(roleRec, newRequest("POST", "/api/records/Role", tenantID, "farshid", []byte(`{"code":"cfo","name":"Chief Financial Officer"}`)))
	if roleRec.Code != http.StatusCreated {
		t.Fatalf("create Role: expected 201, got %d: %s", roleRec.Code, roleRec.Body.String())
	}
	var role struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(roleRec.Body.Bytes(), &role); err != nil {
		t.Fatalf("unmarshal Role response: %v", err)
	}

	userRoleRec := httptest.NewRecorder()
	mux.ServeHTTP(userRoleRec, newRequest("POST", "/api/records/UserRole", tenantID, "farshid",
		[]byte(`{"user_id":"farshid","role_id":"`+role.Data.ID+`"}`)))
	if userRoleRec.Code != http.StatusCreated {
		t.Fatalf("create UserRole: expected 201, got %d: %s", userRoleRec.Code, userRoleRec.Body.String())
	}

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	approveReq := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantID, "farshid", nil)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a caller holding the required role, got %d: %s", approveRec.Code, approveRec.Body.String())
	}

	jobRepo := data.NewWorkflowJobRepo(db)
	got, err := jobRepo.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("Get after approve: %v", err)
	}
	if got.Status != "queued" {
		t.Fatalf("expected the job back to queued after a role holder's approval, got %q", got.Status)
	}
}

// TestAPI_ApproveWorkflowJob_MachineActorBypassesRoleCheck confirms the
// `!rc.Machine` carve-out in approveWorkflowJob actually works: a service
// token holds no Role/UserRole grants at all (RBAC's own ADR-0006
// posture — machine actors are coarse-gated by Zitadel's
// tenant_integration role instead), so without this bypass no service
// integration could ever resume a role-gated job. Builds the request's
// RequestContext directly (Machine: true) rather than through a header,
// the same "already-authenticated request passes through unchanged"
// pattern internal/httpx's own DevAuth tests use — this repo has no
// lighter-weight way to simulate a verified service-token request in this
// package.
func TestAPI_ApproveWorkflowJob_MachineActorBypassesRoleCheck(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "cfo"}},
			{Kind: workflow.StepNotify},
		},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	// "svc-integration" holds no Role at all — a human caller with no
	// grants would get 403 (proven by
	// TestAPI_ApproveWorkflowJob_DeniesCallerWithoutNamedRole); a machine
	// actor must bypass the check entirely.
	approveReq := httptest.NewRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", nil)
	approveReq.Header.Set("X-Tenant-ID", tenantID)
	approveReq = approveReq.WithContext(httpx.WithRequestContext(approveReq.Context(), httpx.RequestContext{
		TenantID: tenantID,
		Actor:    audit.Actor{Type: audit.ActorHuman, ID: "svc-integration"},
		Machine:  true,
	}))
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a machine actor bypassing the role check, got %d: %s", approveRec.Code, approveRec.Body.String())
	}
}

// TestAPI_ApproveWorkflowJob_RoleGateIsTenantScoped proves the new role
// lookup respects ADR-0003's database-per-tenant isolation: "farshid"
// holds "cfo" in tenant A, but tenant B is a genuinely separate database
// with no such grant — the same actor id must still be denied there.
func TestAPI_ApproveWorkflowJob_RoleGateIsTenantScoped(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	tenantB, dbB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()
	for _, db := range []*sql.DB{dbA, dbB} {
		publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
		if err := foundation.Publish(ctx, db, actor); err != nil {
			t.Fatalf("foundation.Publish: %v", err)
		}
	}
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps: []workflow.Step{
			{Kind: workflow.StepRequireApproval, Params: map[string]any{"role": "cfo"}},
			{Kind: workflow.StepNotify},
		},
	}
	publishWorkflow(t, dbA, def)
	publishWorkflow(t, dbB, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Grant "farshid" the cfo role in tenant A only.
	roleRec := httptest.NewRecorder()
	mux.ServeHTTP(roleRec, newRequest("POST", "/api/records/Role", tenantA, "farshid", []byte(`{"code":"cfo","name":"Chief Financial Officer"}`)))
	if roleRec.Code != http.StatusCreated {
		t.Fatalf("create Role in tenant A: expected 201, got %d: %s", roleRec.Code, roleRec.Body.String())
	}
	var role struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(roleRec.Body.Bytes(), &role); err != nil {
		t.Fatalf("unmarshal Role response: %v", err)
	}
	userRoleRec := httptest.NewRecorder()
	mux.ServeHTTP(userRoleRec, newRequest("POST", "/api/records/UserRole", tenantA, "farshid",
		[]byte(`{"user_id":"farshid","role_id":"`+role.Data.ID+`"}`)))
	if userRoleRec.Code != http.StatusCreated {
		t.Fatalf("create UserRole in tenant A: expected 201, got %d: %s", userRoleRec.Code, userRoleRec.Body.String())
	}

	// Halt a job in tenant B, where "farshid" holds no such grant.
	createReq := newRequest("POST", "/api/records/Vendor", tenantB, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	q, err := workflow.NewQueue(dbB, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(ctx, workflow.RegistryDefinitionLookup(dbB)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := dbB.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	approveReq := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantB, "farshid", nil)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 — tenant A's cfo grant must not leak into tenant B, got %d: %s", approveRec.Code, approveRec.Body.String())
	}
}

func TestAPI_ApproveWorkflowJob_UnknownJobIs404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/api/workflow-jobs/99999999-9999-9999-9999-999999999999/approve", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown job id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_ApproveWorkflowJob_TenantIsolation confirms a tenant can't
// resume another tenant's job by guessing/reusing its id — the same
// isolation internal/kernel/workflow's own
// TestWorkflowJobRepo_TenantIsolation already proves at the repo layer,
// checked again here at the HTTP layer where a caller-supplied id
// actually originates.
func TestAPI_ApproveWorkflowJob_TenantIsolation(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	tenantB, _ := newTestTenant(t, router)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval}},
	}
	publishWorkflow(t, dbA, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantA, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(dbA, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(context.Background(), workflow.RegistryDefinitionLookup(dbA)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := dbA.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	approveReq := newRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", tenantB, "farshid", nil)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusNotFound {
		t.Fatalf("expected tenant B approving tenant A's job to 404, got %d: %s", approveRec.Code, approveRec.Body.String())
	}
}

// TestAPI_ListWorkflowJobs_ReturnsMatchingStatusOnly is the read side of
// the approval loop's own HTTP surface: GET /api/workflow-jobs?
// status=waiting_approval must return exactly the jobs actually waiting,
// not jobs in other statuses and not another tenant's jobs.
func TestAPI_ListWorkflowJobs_ReturnsMatchingStatusOnly(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	tenantB, dbB := newTestTenant(t, router)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, dbB, vendorEntityDef(), vendorFormDef())
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval}},
	}
	publishWorkflow(t, dbA, def)
	publishWorkflow(t, dbB, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Tenant A: one Vendor whose workflow will be driven to
	// waiting_approval. Tenant B: same setup, must not leak into A's list.
	for _, tid := range []string{tenantA, tenantB} {
		req := newRequest("POST", "/api/records/Vendor", tid, "farshid", []byte(`{"name":"Acme Textiles"}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create Vendor for tenant %s: expected 201, got %d: %s", tid, rec.Code, rec.Body.String())
		}
	}

	// Each tenant's own job lives in its own database now — process both.
	for _, tdb := range []*sql.DB{dbA, dbB} {
		q, err := workflow.NewQueue(tdb, nil)
		if err != nil {
			t.Fatalf("NewQueue: %v", err)
		}
		if _, err := q.ProcessOne(context.Background(), workflow.RegistryDefinitionLookup(tdb)); err != nil {
			t.Fatalf("ProcessOne: %v", err)
		}
	}

	var tenantAJobID string
	if err := dbA.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&tenantAJobID); err != nil {
		t.Fatalf("find tenant A's job: %v", err)
	}

	listReq := newRequest("GET", "/api/workflow-jobs?status=waiting_approval", tenantA, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}

	var got struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].ID != tenantAJobID || got.Data[0].Status != "waiting_approval" {
		t.Fatalf("expected exactly tenant A's one waiting_approval job, got %+v", got.Data)
	}

	// A status with nothing matching returns an empty list, not an error.
	doneReq := newRequest("GET", "/api/workflow-jobs?status=done", tenantA, "farshid", nil)
	doneRec := httptest.NewRecorder()
	mux.ServeHTTP(doneRec, doneReq)
	var gotDone struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(doneRec.Body.Bytes(), &gotDone); err != nil {
		t.Fatalf("unmarshal done-status response: %v", err)
	}
	if len(gotDone.Data) != 0 {
		t.Fatalf("expected no done jobs, got %+v", gotDone.Data)
	}
}

func TestAPI_ListWorkflowJobs_MissingStatusIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/api/workflow-jobs", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing status query param, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_ListWorkflowJobs_UnknownStatusIs400 confirms a typo'd status
// (e.g. "waitng_approval") comes back as a clear 400, not a silent empty
// list a caller could easily mistake for "nothing is actually waiting."
func TestAPI_ListWorkflowJobs_UnknownStatusIs400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/api/workflow-jobs?status=waitng_approval", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown status value, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_WorkflowInbox_ShowsWaitingJobAndApproveButton confirms the
// human-facing page actually renders a waiting job with a working
// Approve control pointed at the real approve endpoint — the page
// listWorkflowJobs alone doesn't give anyone without a JSON client.
func TestAPI_WorkflowInbox_ShowsWaitingJobAndApproveButton(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval}},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(context.Background(), workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	req := newRequest("GET", "/workflow-jobs", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "vendor_approval") {
		t.Fatalf("expected the waiting workflow's name in the inbox, got:\n%s", body)
	}
	if !strings.Contains(body, `id="workflow-job-`+jobID+`"`) {
		t.Fatalf("expected a row for the waiting job, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/api/workflow-jobs/`+jobID+`/approve"`) {
		t.Fatalf("expected the Approve button to hx-post the real approve endpoint, got:\n%s", body)
	}
}

func TestAPI_WorkflowInbox_EmptyShowsEmptyMessage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/workflow-jobs", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Nothing waiting for your approval.") {
		t.Fatalf("expected the empty-state message, got:\n%s", rec.Body.String())
	}
}

// TestAPI_ApproveWorkflowJob_HTMXRequestGetsEmptyBody confirms the
// htmx-specific response shape approveWorkflowJob's doc comment
// describes: an empty 200, not the JSON envelope a non-htmx caller gets,
// so hx-swap="outerHTML" removes the row cleanly instead of rendering a
// JSON blob inside the table.
func TestAPI_ApproveWorkflowJob_HTMXRequestGetsEmptyBody(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	def := &workflow.Definition{
		Name: "vendor_approval", Version: 1,
		Trigger: workflow.Trigger{Type: workflow.TriggerOnCreate, EntityType: "Vendor"},
		Steps:   []workflow.Step{{Kind: workflow.StepRequireApproval}},
	}
	publishWorkflow(t, db, def)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	q, err := workflow.NewQueue(db, nil)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if _, err := q.ProcessOne(context.Background(), workflow.RegistryDefinitionLookup(db)); err != nil {
		t.Fatalf("ProcessOne (halt at approval): %v", err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT id FROM workflow_jobs WHERE workflow_name = $1`, def.Name).Scan(&jobID); err != nil {
		t.Fatalf("find enqueued job: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/workflow-jobs/"+jobID+"/approve", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "farshid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body for an htmx approve request (so hx-swap removes the row cleanly), got:\n%s", rec.Body.String())
	}
}

// TestWriteInternalError confirms the client-facing response never
// leaks the real error's own text (which can carry SQLSTATE codes,
// table/column names, or query fragments) while still writing a
// generic 500 envelope.
func TestWriteInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInternalError(rec, "test context", fmt.Errorf("pq: relation \"secret_table\" does not exist"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	var env struct {
		Data  any     `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if env.Error == nil || *env.Error != "internal error" {
		t.Fatalf("expected the generic \"internal error\" message, got %v", env.Error)
	}
	if strings.Contains(*env.Error, "secret_table") {
		t.Fatal("the real error text must never reach the client")
	}
}

// TestAPI_PurchaseOrder_StagedLeadTimeTimestamps drives #29's staged
// lead-time chain through the real HTTP stack against the real published
// purchasing module (not a throwaway test Definition): an in-order set
// of all six stages creates cleanly and round-trips on GET, and an
// out-of-order pair is refused with a 400 whose body names both fields
// involved — the same registry -> crud -> handler path the generated
// form's own Save posts through.
func TestAPI_PurchaseOrder_StagedLeadTimeTimestamps(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, tenantDB := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, def("Party"), map[string]any{
		"party_type": "organization", "name": "Staged Lead-Time Vendor", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("create Party: %v", err)
	}
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := engine.Create(ctx, def("PartyRole"), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("create vendor PartyRole: %v", err)
	}
	statusTypes, err := engine.ListByField(ctx, def("StatusType"), "code", "purchase_order_status")
	if err != nil || len(statusTypes) == 0 {
		t.Fatalf("list purchase_order_status StatusType: %v (n=%d)", err, len(statusTypes))
	}
	poStatuses, err := engine.ListByField(ctx, def("Status"), "status_type_id", statusTypes[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	var draftID string
	for _, s := range poStatuses {
		if code, _ := s.Data["code"].(string); code == "draft" {
			draftID = s.ID
		}
	}
	if draftID == "" {
		t.Fatal("no draft Status seeded for purchase_order_status")
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	stages := map[string]string{
		"sourced_at":          "2026-07-04",
		"production_start_at": "2026-07-08",
		"production_ready_at": "2026-07-18",
		"shipped_at":          "2026-07-22",
		"customs_cleared_at":  "2026-07-26",
		"received_at":         "2026-07-27",
	}
	fields := map[string]any{
		"po_number": "PO-STAGED-1", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "status_id": draftID,
	}
	for k, v := range stages {
		fields[k] = v
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createReq := newRequest("POST", "/api/records/PurchaseOrder", tenantID, "farshid", body)
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a fully in-order stage chain, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	// GET it back: every stage date round-trips exactly (ISO-8601
	// strings in JSONB, no truncation/reformatting anywhere in between).
	getReq := newRequest("GET", "/api/records/PurchaseOrder/"+created.Data.ID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var fetched struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("unmarshal get response: %v", err)
	}
	for name, want := range stages {
		if got, _ := fetched.Data.Data[name].(string); got != want {
			t.Errorf("stage %q: expected %q to round-trip, got %q", name, want, got)
		}
	}

	// Out-of-order: shipped_at before production_ready_at -> 400, and
	// the error body names both fields so the caller knows which pair to
	// fix (entity.validateNotBefore's message, surfaced verbatim).
	bad := map[string]any{
		"po_number": "PO-STAGED-2", "vendor_id": vendor.ID,
		"order_date": "2026-07-01", "status_id": draftID,
		"sourced_at": "2026-07-04", "production_start_at": "2026-07-08",
		"production_ready_at": "2026-07-18", "shipped_at": "2026-07-10",
	}
	badBody, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad body: %v", err)
	}
	badReq := newRequest("POST", "/api/records/PurchaseOrder", tenantID, "farshid", badBody)
	badRec := httptest.NewRecorder()
	mux.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shipped_at before production_ready_at, got %d: %s", badRec.Code, badRec.Body.String())
	}
	if b := badRec.Body.String(); !strings.Contains(b, "shipped_at") || !strings.Contains(b, "production_ready_at") {
		t.Fatalf("expected the error body to name both fields of the violated chain, got: %s", b)
	}
}
