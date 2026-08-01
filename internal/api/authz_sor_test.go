package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// seedSoRFixture writes, through the RAW engine (the system path the
// import engine itself uses — never the guarded engine these tests are
// probing), an ExternalSQLSource named sourceName, one imported Vendor
// record, its ExternalIdentity row, and a SystemOfRecord read_only row
// declaring that source the owner of Vendor. Returns the ids the tests
// assert against.
func seedSoRFixture(t *testing.T, db *sql.DB, sourceName string) (sourceID, recordID string) {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()

	src, err := engine.Create(ctx, foundation.ExternalSQLSource(), map[string]any{
		"name": sourceName, "driver": "postgres", "host": "legacy.internal", "database": "navdb",
	}, actor)
	if err != nil {
		t.Fatalf("seed ExternalSQLSource: %v", err)
	}
	rec, err := engine.Create(ctx, vendorEntityDef(), map[string]any{"name": "Imported Vendor"}, actor)
	if err != nil {
		t.Fatalf("seed Vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.ExternalIdentity(), map[string]any{
		"source_id":       src.ID,
		"source_relation": "public.vendor",
		"entity_type":     "Vendor",
		"record_id":       rec.ID,
		"external_key":    "V-1",
	}, actor); err != nil {
		t.Fatalf("seed ExternalIdentity: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.SystemOfRecord(), map[string]any{
		"entity_type": "Vendor", "source_id": src.ID, "mode": "read_only",
	}, actor); err != nil {
		t.Fatalf("seed SystemOfRecord: %v", err)
	}
	return src.ID, rec.ID
}

// TestAPI_SoR_UpdateOfImportedRecordBlockedTranslated (uc-infra#102):
// hand-editing a record imported from a read_only system of record is
// refused as a translated 409 naming the owning source — on the JSON API
// and on the form-save path alike (the envelope's error string is what
// the global toast shows a browser user), never the engine's logs-only
// English Error() text, never a 500.
func TestAPI_SoR_UpdateOfImportedRecordBlockedTranslated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	_, recordID := seedSoRFixture(t, db, "NAV Mirror")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// JSON API update → 409 with the translated, source-naming message.
	jsonReq := newRequest("POST", "/api/records/Vendor/"+recordID, tenantID, "farshid", []byte(`{"name":"Hand Edited"}`))
	jsonRec := httptest.NewRecorder()
	mux.ServeHTTP(jsonRec, jsonReq)
	if jsonRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a JSON update of an imported record, got %d: %s", jsonRec.Code, jsonRec.Body.String())
	}
	body := jsonRec.Body.String()
	if !strings.Contains(body, "This record is owned by NAV Mirror") {
		t.Fatalf("expected the translated message naming the source, got:\n%s", body)
	}
	if strings.Contains(body, "read-only external source") {
		t.Fatalf("the engine's logs-only Error() text leaked into the response:\n%s", body)
	}

	// Form-save path: the same endpoint as a real htmx form submission —
	// the translated envelope is what the error toast renders.
	formReq := httptest.NewRequest("POST", "/api/records/Vendor/"+recordID, strings.NewReader("name=Hand+Edited"))
	formReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formReq.Header.Set("HX-Request", "true")
	formReq.Header.Set("X-Tenant-ID", tenantID)
	formReq.Header.Set("X-Actor-ID", "farshid")
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a form save of an imported record, got %d: %s", formRec.Code, formRec.Body.String())
	}
	if !strings.Contains(formRec.Body.String(), "This record is owned by NAV Mirror") {
		t.Fatalf("expected the translated block message on the form-save path, got:\n%s", formRec.Body.String())
	}

	// The record survived both attempts untouched.
	getReq := newRequest("GET", "/api/records/Vendor/"+recordID, tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Imported Vendor") || strings.Contains(getRec.Body.String(), "Hand Edited") {
		t.Fatalf("expected the imported record to be unchanged, got:\n%s", getRec.Body.String())
	}
}

// TestAPI_SoR_UnidentifiedRecordOfSameTypeStillSaves: the read_only
// declaration protects IMPORTED records (those with an identity row) —
// a hand-created record of the same entity type stays the tenant's own
// and remains editable.
func TestAPI_SoR_UnidentifiedRecordOfSameTypeStillSaves(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	seedSoRFixture(t, db, "NAV Mirror")

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Home-Grown Vendor"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating a hand-made Vendor, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	updateReq := newRequest("POST", "/api/records/Vendor/"+created.Data.ID, tenantID, "farshid", []byte(`{"name":"Home-Grown Vendor Ltd"}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200 updating an un-identified Vendor, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
}

// TestAPI_SoR_DeleteBlockedTranslated: deleting an imported read-only
// record is refused the same translated way, not a 500. There is no
// generic record-delete route today, so this drives the one real delete
// surface a SoR rule can govern end to end — the SQL-sources settings
// delete — with the deleted-record fixture pointed at an
// ExternalSQLSource record that is itself identified as imported from a
// read_only master.
func TestAPI_SoR_DeleteBlockedTranslated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()
	owner, err := engine.Create(ctx, foundation.ExternalSQLSource(), map[string]any{
		"name": "NAV Master", "driver": "postgres", "host": "legacy.internal", "database": "navdb",
	}, actor)
	if err != nil {
		t.Fatalf("seed owner source: %v", err)
	}
	victim, err := engine.Create(ctx, foundation.ExternalSQLSource(), map[string]any{
		"name": "Mirrored Source", "driver": "postgres", "host": "mirror.internal", "database": "navdb",
	}, actor)
	if err != nil {
		t.Fatalf("seed mirrored source: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.ExternalIdentity(), map[string]any{
		"source_id":       owner.ID,
		"source_relation": "public.sources",
		"entity_type":     "ExternalSQLSource",
		"record_id":       victim.ID,
		"external_key":    "SRC-1",
	}, actor); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.SystemOfRecord(), map[string]any{
		"entity_type": "ExternalSQLSource", "source_id": owner.ID, "mode": "read_only",
	}, actor); err != nil {
		t.Fatalf("seed SystemOfRecord: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+victim.ID+"/delete", tenantID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 deleting an imported read-only record, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "This record is owned by NAV Master") {
		t.Fatalf("expected the translated block message naming the source, got:\n%s", rec.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE id = $1 AND deleted_at IS NULL`, victim.ID).Scan(&count); err != nil {
		t.Fatalf("count victim record: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the blocked delete to leave the record in place, found %d", count)
	}
}

// TestAPI_SoR_BidirectionalModeReservedTranslated: the "bidirectional"
// mode is declared in the enum but reserved for the sync engine —
// saving it (create or edit) through the generic form/API is a
// translated 400, not a silent accept or an untranslated engine string.
func TestAPI_SoR_BidirectionalModeReservedTranslated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Create with the reserved mode → 400 translated.
	createReq := newRequest("POST", "/api/records/SystemOfRecord", tenantID, "farshid",
		[]byte(`{"entity_type":"Vendor","mode":"bidirectional"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for the reserved mode on create, got %d: %s", createRec.Code, createRec.Body.String())
	}
	if !strings.Contains(createRec.Body.String(), "reserved for the upcoming sync engine") {
		t.Fatalf("expected the translated reserved-mode message, got:\n%s", createRec.Body.String())
	}

	// Editing an existing row INTO the reserved mode is refused the same
	// way.
	okReq := newRequest("POST", "/api/records/SystemOfRecord", tenantID, "farshid",
		[]byte(`{"entity_type":"Vendor","mode":"read_only"}`))
	okRec := httptest.NewRecorder()
	mux.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a read_only row, got %d: %s", okRec.Code, okRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	updateReq := newRequest("POST", "/api/records/SystemOfRecord/"+created.Data.ID, tenantID, "farshid",
		[]byte(`{"entity_type":"Vendor","mode":"bidirectional"}`))
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 editing into the reserved mode, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), "reserved for the upcoming sync engine") {
		t.Fatalf("expected the translated reserved-mode message on update, got:\n%s", updateRec.Body.String())
	}
}

// TestAPI_SoR_ConfiguredTenantPlainUserCannotAuthorSystemOfRecord:
// SystemOfRecord is control-plane-gated (authz.controlPlaneTypes) — a
// row decides whose edits get refused, so once a tenant configures RBAC
// a plain user may no longer author one (deny-unless-granted), while an
// admin still can. The other tests in this file run in UNCONFIGURED
// tenants (the bootstrap window, where control-plane types stay open)
// and seed their SoR rows through the raw engine — authoring permission
// is this test's subject, not theirs.
func TestAPI_SoR_ConfiguredTenantPlainUserCannotAuthorSystemOfRecord(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	seedRBAC(t, db,
		map[string][]string{
			"clerk":             {"user-clerk"},
			authz.AdminRoleCode: {"user-admin"},
		},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": true},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	payload := []byte(`{"entity_type":"Vendor","mode":"read_only"}`)

	denyReq := newRequest("POST", "/api/records/SystemOfRecord", tenantID, "user-clerk", payload)
	denyRec := httptest.NewRecorder()
	mux.ServeHTTP(denyRec, denyReq)
	if denyRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a plain user authoring SystemOfRecord in a configured tenant, got %d: %s", denyRec.Code, denyRec.Body.String())
	}
	if !strings.Contains(denyRec.Body.String(), "access denied") {
		t.Fatalf("expected the standard denial envelope, got:\n%s", denyRec.Body.String())
	}

	adminReq := newRequest("POST", "/api/records/SystemOfRecord", tenantID, "user-admin", payload)
	adminRec := httptest.NewRecorder()
	mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an admin authoring SystemOfRecord, got %d: %s", adminRec.Code, adminRec.Body.String())
	}
}

// TestAPI_SoR_PlatformOwnedRowSavesFine: the non-reserved modes save
// through the generic API without any SoR interference.
func TestAPI_SoR_PlatformOwnedRowSavesFine(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/api/records/SystemOfRecord", tenantID, "farshid",
		[]byte(`{"entity_type":"Vendor","mode":"platform_owned"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a platform_owned row, got %d: %s", rec.Code, rec.Body.String())
	}
}
