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

// seedRBAC writes Role/UserRole/Permission rows through the raw engine —
// the "system setup" path authz's package comment carves out — using the
// foundation Definitions directly. Returns the created role ids by code.
func seedRBAC(t *testing.T, db *sql.DB, grants map[string][]string, perms []map[string]any) map[string]string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()

	roleIDs := map[string]string{}
	for code, userIDs := range grants {
		role, err := engine.Create(ctx, foundation.Role(), map[string]any{"code": code, "name": code}, actor)
		if err != nil {
			t.Fatalf("create Role %s: %v", code, err)
		}
		roleIDs[code] = role.ID
		for _, uid := range userIDs {
			if _, err := engine.Create(ctx, foundation.UserRole(), map[string]any{"user_id": uid, "role_id": role.ID}, actor); err != nil {
				t.Fatalf("grant %s to %s: %v", code, uid, err)
			}
		}
	}
	for _, p := range perms {
		p["role_id"] = roleIDs[p["role"].(string)]
		delete(p, "role")
		if _, err := engine.Create(ctx, foundation.Permission(), p, actor); err != nil {
			t.Fatalf("create Permission: %v", err)
		}
	}
	return roleIDs
}

// TestAPI_RBAC_EntityLevel_Enforced403 is the HTTP half of
// internal/kernel/authz's own engine tests: the same deny/grant/admin
// semantics, but asserted as real status codes through Routes — the
// contract a browser or API client actually experiences (ADR-0006).
func TestAPI_RBAC_EntityLevel_Enforced403(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	seedRBAC(t, db,
		map[string][]string{
			"clerk":             {"user-clerk"},
			authz.AdminRoleCode: {"user-admin"},
		},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	do := func(method, target, actorID string, body []byte) *httptest.ResponseRecorder {
		req := newRequest(method, target, tenantID, actorID, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	payload := []byte(`{"name":"RBAC Vendor"}`)

	// Outsider (no roles): rules exist for Vendor, so read AND write 403.
	if rec := do("GET", "/api/records/Vendor", "user-outsider", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider list: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/api/records/Vendor", "user-outsider", payload); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider create: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Clerk: granted read-only — list 200, create 403.
	if rec := do("GET", "/api/records/Vendor", "user-clerk", nil); rec.Code != http.StatusOK {
		t.Fatalf("clerk list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/api/records/Vendor", "user-clerk", payload); rec.Code != http.StatusForbidden {
		t.Fatalf("clerk create: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// tenant_admin role code: full access despite no Permission row of
	// its own — the lockout-prevention bypass, end to end.
	rec := do("POST", "/api/records/Vendor", "user-admin", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// The privilege escalation the control-plane rule closes (found by
	// this commit's independent review): a non-admin authoring
	// themselves an admin grant, or widening their own permissions,
	// must be denied — RBAC is configured in this tenant, and the
	// control-plane types have no explicit rules opting anyone in.
	if rec := do("POST", "/api/records/Role", "user-clerk", []byte(`{"code":"tenant_admin","name":"evil"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("clerk create Role: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/api/records/Permission", "user-clerk", []byte(`{"role_id":"whatever","entity_type":"Vendor","can_write":true}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("clerk create Permission: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// An entity type with no Permission rows stays open to everyone —
	// the backward-compatibility guarantee, over HTTP.
	if rec := do("GET", "/api/records/Party", "user-outsider", nil); rec.Code != http.StatusOK {
		t.Fatalf("outsider list Party (no rules): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The denial body is the JSON error envelope, not an HTML error page
	// and not the "internal error" fallback.
	recDenied := do("GET", "/api/records/Vendor", "user-outsider", nil)
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recDenied.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("denial body is not the JSON error envelope: %v: %s", err, recDenied.Body.String())
	}
	if envelope.Error != "access denied" {
		t.Fatalf("denial error = %q, want %q", envelope.Error, "access denied")
	}
}

// TestAPI_RBAC_RenderedSurfaces_Denied403 covers the server-rendered
// routes (list page, record form) — same guarded engine underneath, and
// a denied user must get a 403, not a half-rendered page or a 500.
func TestAPI_RBAC_RenderedSurfaces_Denied403(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false},
		},
	)

	// A real record to load through the guarded form path. (The /new
	// form page itself makes no CRUD call for an entity without
	// reference fields, so it is deliberately NOT asserted here — form
	// and menu filtering for denied users is the field-level
	// enforcement commit's scope, per ADR-0006's two-commit plan.)
	vendorRec, err := crud.NewEngine(db).Create(ctx, vendorEntityDef(), map[string]any{"name": "Seeded Vendor"}, humanActor())
	if err != nil {
		t.Fatalf("seed Vendor record: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	for _, target := range []string{"/records/Vendor", "/forms/Vendor/" + vendorRec.ID} {
		req := newRequest("GET", target, tenantID, "user-outsider", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("outsider GET %s: expected 403, got %d: %s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "internal error") {
			t.Fatalf("outsider GET %s: denial leaked as internal error: %s", target, rec.Body.String())
		}
	}

	// The clerk's read grant renders the list page normally.
	req := newRequest("GET", "/records/Vendor", tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk GET /records/Vendor: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_RBAC_ImportCommit_DeniedWritesNothing covers the csvimport
// RecordCreator seam: a CSV import committed by a user without write
// permission fails closed — every row reports the denial and no record
// lands. (The response is the normal per-row result page, not a blanket
// 403 — importing is row-granular by design; ADR-0006 records this.)
func TestAPI_RBAC_ImportCommit_DeniedWritesNothing(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	csvContent := []byte("name\nAcme Textiles\nBeta Supplies\n")
	commitReq := newMultipartRequest(t, "/import/Vendor/commit", tenantID, "user-clerk", "vendors.csv", csvContent,
		map[string]string{"mapping.name": "name"})
	commitRec := httptest.NewRecorder()
	mux.ServeHTTP(commitRec, commitReq)

	if commitRec.Code != http.StatusOK {
		t.Fatalf("import commit page: expected 200 (per-row results), got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	if !strings.Contains(commitRec.Body.String(), "access denied") {
		t.Fatalf("expected per-row access-denied results, got:\n%s", commitRec.Body.String())
	}

	n, err := crud.NewEngine(db).Count(ctx, vendorEntityDef())
	if err != nil {
		t.Fatalf("count Vendor: %v", err)
	}
	if n != 0 {
		t.Fatalf("denied import still wrote %d Vendor record(s)", n)
	}
}
