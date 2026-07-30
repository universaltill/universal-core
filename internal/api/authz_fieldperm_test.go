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
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// The HTTP half of ADR-0006's field-level commit: the same semantics
// internal/kernel/authz unit-tests, but asserted on what a browser or
// API client actually receives — JSON keys, rendered HTML, CSV columns,
// nav links, status codes.

// employeeEntityDef has one obviously-sensitive optional field, which is
// the realistic shape of a field rule (hiding an OPTIONAL field; hiding
// a required one makes creation impossible for that role — recorded in
// ADR-0006's limitations).
func employeeEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Employee",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "salary", Type: entity.FieldString},
		},
	}
}

func employeeFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Employee",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields: []form.FormField{
				{Name: "name", Label: "Name"},
				{Name: "salary", Label: "Salary"},
			},
		}},
	}
}

// widgetEntityDef is a second entity in the SAME module as Employee,
// left readable — so a test can tell "the module disappeared" apart from
// "one node inside it disappeared." Deliberately a fictional entity type
// ("Widget", not "Department") — this test predates foundation.Department
// (the real org-chart entity, `internal/kernel/foundation/foundation.go`)
// and originally used that name as its placeholder; renamed to avoid
// colliding with the real entity_type once it existed (same
// entity_definitions_entity_type_version_key collision a real
// foundation.Publish + this fixture's own publishEntityAndForm would hit
// if their names matched).
func widgetEntityDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Widget",
		Version:    1,
		Module:     "foundation",
		Fields:     []entity.Field{{Name: "name", Type: entity.FieldString, Required: true}},
	}
}

func widgetFormDef() *form.Definition {
	return &form.Definition{
		EntityType: "Widget",
		Version:    1,
		Sections: []form.Section{{
			Title:     "Details",
			Component: form.ComponentFields,
			Fields:    []form.FormField{{Name: "name", Label: "Name"}},
		}},
	}
}

// seedFieldRule hides entityType.fieldName from every holder of roleCode,
// creating the role and granting it to userID. Written through the raw
// engine — the "system setup" path authz carves out.
func seedFieldRule(t *testing.T, db *sql.DB, roleCode, userID, entityType, fieldName string) string {
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
	_, err = engine.Create(ctx, foundation.FieldPermission(), map[string]any{
		"role_id": role.ID, "entity_type": entityType, "field_name": fieldName, "hidden": true,
	}, actor)
	if err != nil {
		t.Fatalf("create FieldPermission: %v", err)
	}
	return role.ID
}

// fieldPermFixture sets up a tenant with Employee published, one record,
// and salary hidden from "user-clerk" (who holds the "clerk" role).
// "user-open" holds no roles and therefore sees everything.
func fieldPermFixture(t *testing.T) (tenantID string, db *sql.DB, mux *http.ServeMux, recordID string) {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db = newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, employeeEntityDef(), employeeFormDef())
	seedFieldRule(t, db, "clerk", "user-clerk", "Employee", "salary")

	rec, err := crud.NewEngine(db).Create(ctx, employeeEntityDef(),
		map[string]any{"name": "Dana", "salary": "120000"}, humanActor())
	if err != nil {
		t.Fatalf("seed Employee: %v", err)
	}

	mux = http.NewServeMux()
	testHandler(t, router).Routes(mux)
	return tenantID, db, mux, rec.ID
}

func getAs(t *testing.T, mux *http.ServeMux, target, tenantID, actorID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest("GET", target, tenantID, actorID, nil))
	return rec
}

// The JSON API strips the hidden key entirely — not blanked, not null,
// absent. A present-but-empty key would still confirm the field exists.
func TestAPI_FieldPermission_JSONStripsHiddenField(t *testing.T) {
	tenantID, _, mux, recordID := fieldPermFixture(t)

	for _, target := range []string{"/api/records/Employee", "/api/records/Employee/" + recordID} {
		rec := getAs(t, mux, target, tenantID, "user-clerk")
		if rec.Code != http.StatusOK {
			t.Fatalf("clerk GET %s: expected 200, got %d: %s", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "salary") || strings.Contains(body, "120000") {
			t.Fatalf("clerk GET %s leaked the hidden field: %s", target, body)
		}
		if !strings.Contains(body, "Dana") {
			t.Fatalf("clerk GET %s lost the visible field: %s", target, body)
		}
	}

	// A user with no field rules against them still sees it — proving the
	// stripping is per-actor, not a global schema change.
	rec := getAs(t, mux, "/api/records/Employee/"+recordID, tenantID, "user-open")
	if !strings.Contains(rec.Body.String(), "120000") {
		t.Fatalf("unrestricted user lost the field: %s", rec.Body.String())
	}
}

// The generated form must contain no trace of the field — not as a
// visible input, and not as one of formrender's hidden preservation
// inputs (which every other off-form field legitimately gets).
func TestAPI_FieldPermission_FormOmitsHiddenFieldEntirely(t *testing.T) {
	tenantID, _, mux, recordID := fieldPermFixture(t)

	rec := getAs(t, mux, "/forms/Employee/"+recordID, tenantID, "user-clerk")
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk form: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="salary"`) {
		t.Fatalf("form rendered an input for the hidden field:\n%s", body)
	}
	if strings.Contains(body, "120000") {
		t.Fatalf("form leaked the hidden value:\n%s", body)
	}
	if !strings.Contains(body, `name="name"`) {
		t.Fatalf("form lost the visible field:\n%s", body)
	}
}

// The list page drops the whole column, not just the cells: a header
// over a column of blanks names exactly what is being withheld and reads
// as broken data rather than as policy.
func TestAPI_FieldPermission_ListPageDropsColumn(t *testing.T) {
	tenantID, _, mux, _ := fieldPermFixture(t)

	rec := getAs(t, mux, "/records/Employee", tenantID, "user-clerk")
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk list page: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "salary") || strings.Contains(body, "120000") {
		t.Fatalf("list page leaked the hidden column:\n%s", body)
	}
	if !strings.Contains(body, "Dana") {
		t.Fatalf("list page lost the visible column:\n%s", body)
	}

	// Header and cells must stay aligned — one <th> per <td>. A column
	// dropped from only one of the two would silently shift every value
	// under the wrong heading.
	if got, want := strings.Count(body, "<th>"), strings.Count(body, "<td>"); got != want {
		t.Fatalf("header/cell count mismatch after dropping a column: %d <th> vs %d <td>\n%s", got, want, body)
	}
}

// CSV export drops the column too — ExportCSV builds its header from the
// Definition, which the guarded engine never sees, so this is a
// genuinely separate code path from the row redaction above.
func TestAPI_FieldPermission_CSVExportDropsColumn(t *testing.T) {
	tenantID, _, mux, _ := fieldPermFixture(t)

	rec := getAs(t, mux, "/export/Employee", tenantID, "user-clerk")
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk export: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "salary") || strings.Contains(body, "120000") {
		t.Fatalf("CSV export leaked the hidden column:\n%s", body)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		t.Fatalf("CSV export produced no data row:\n%s", body)
	}
	if header, want := strings.TrimSpace(lines[0]), "name"; header != want {
		t.Fatalf("CSV header = %q, want %q", header, want)
	}

	// Unrestricted user still gets both columns.
	open := getAs(t, mux, "/export/Employee", tenantID, "user-open")
	if !strings.Contains(open.Body.String(), "salary") {
		t.Fatalf("unrestricted export lost the column:\n%s", open.Body.String())
	}
}

// The load-bearing one: a save from a redacted form must not erase the
// field that form was never allowed to show. crud.Update replaces the
// record's whole data blob, so without server-side restoration "hide
// this field" would silently mean "let this user delete it."
func TestAPI_FieldPermission_SaveFromRedactedFormPreservesHiddenValue(t *testing.T) {
	tenantID, db, mux, recordID := fieldPermFixture(t)

	// Exactly what the redacted form submits: the visible field only.
	req := newRequest("POST", "/api/records/Employee/"+recordID, tenantID, "user-clerk",
		[]byte(`{"name":"Dana Renamed"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clerk save: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := crud.NewEngine(db).Get(context.Background(), employeeEntityDef(), recordID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if stored.Data["name"] != "Dana Renamed" {
		t.Fatalf("visible field not saved: %v", stored.Data)
	}
	if stored.Data["salary"] != "120000" {
		t.Fatalf("hidden field erased by a save that omitted it: %v", stored.Data)
	}
}

// Writing a field you cannot read is 403, on create and on update, and
// the refused update changes nothing.
func TestAPI_FieldPermission_WritingHiddenFieldIs403(t *testing.T) {
	tenantID, db, mux, recordID := fieldPermFixture(t)

	post := func(target string, body []byte) *httptest.ResponseRecorder {
		req := newRequest("POST", target, tenantID, "user-clerk", body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/api/records/Employee/"+recordID, []byte(`{"name":"Dana","salary":"999999"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("clerk update of hidden field: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/records/Employee", []byte(`{"name":"New Hire","salary":"888888"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("clerk create setting hidden field: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := crud.NewEngine(db).Get(context.Background(), employeeEntityDef(), recordID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if stored.Data["salary"] != "120000" || stored.Data["name"] != "Dana" {
		t.Fatalf("denied update still changed the record: %v", stored.Data)
	}
}

// A denied CSV import is also refused at the field level: the row's
// hidden-field value is a write the importer must not perform.
func TestAPI_FieldPermission_ImportSettingHiddenFieldWritesNothing(t *testing.T) {
	tenantID, db, mux, _ := fieldPermFixture(t)
	ctx := context.Background()

	before, err := crud.NewEngine(db).Count(ctx, employeeEntityDef())
	if err != nil {
		t.Fatalf("count before: %v", err)
	}

	csvContent := []byte("name,salary\nMallory,1\n")
	req := newMultipartRequest(t, "/import/Employee/commit", tenantID, "user-clerk", "staff.csv", csvContent,
		map[string]string{"mapping.name": "name", "mapping.salary": "salary"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "access denied") {
		t.Fatalf("expected per-row access-denied results, got:\n%s", rec.Body.String())
	}
	after, err := crud.NewEngine(db).Count(ctx, employeeEntityDef())
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("denied import wrote %d record(s)", after-before)
	}
}

// TestAPI_RBAC_DeniedPageIsLocalized covers the rendered-page denial
// surface: a real 403 page in the visitor's own language, inside the
// normal shell, rather than a bare JSON envelope as the whole document.
func TestAPI_RBAC_DeniedPageIsLocalized(t *testing.T) {
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
		[]map[string]any{{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false}},
	)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// The outsider is denied read on Vendor entirely.
	rec := getAs(t, mux, "/records/Vendor", tenantID, "user-outsider")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access denied") {
		t.Fatalf("denial page missing its localized title:\n%s", body)
	}
	// A real page, not a JSON body: the shell, the nav, and the
	// stylesheet all have to be there or this isn't a page a person can
	// read and navigate away from.
	for _, want := range []string{"<html", "uc-nav", "uc-denied", ".css"} {
		if !strings.Contains(body, want) {
			t.Fatalf("denial page is not a real shell-rendered page (missing %q):\n%s", want, body)
		}
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("denial page rendered the JSON envelope instead of a page:\n%s", body)
	}

	// Same page in Turkish — the whole point of it being a page.
	tr := getAs(t, mux, "/records/Vendor?lang=tr", tenantID, "user-outsider")
	if tr.Code != http.StatusForbidden {
		t.Fatalf("tr: expected 403, got %d", tr.Code)
	}
	if !strings.Contains(tr.Body.String(), "Erişim reddedildi") {
		t.Fatalf("denial page not localized to tr:\n%s", tr.Body.String())
	}
	// And in Arabic, which also has to flip the document direction.
	ar := getAs(t, mux, "/records/Vendor?lang=ar", tenantID, "user-outsider")
	if !strings.Contains(ar.Body.String(), `dir="rtl"`) {
		t.Fatalf("denial page lost RTL direction for ar:\n%s", ar.Body.String())
	}
}

// The two shells that reached a denied user without making a CRUD call
// of their own — ADR-0006 recorded both as accepted leaks for commit 1.
func TestAPI_RBAC_NewFormAndImportShellsAreGated(t *testing.T) {
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
		[]map[string]any{{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false}},
	)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Read-only clerk: may browse, may not reach a create surface.
	for _, target := range []string{"/forms/Vendor/new", "/import/Vendor"} {
		rec := getAs(t, mux, target, tenantID, "user-clerk")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-only clerk GET %s: expected 403, got %d: %s", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Access denied") {
			t.Fatalf("GET %s: not the rendered denial page:\n%s", target, rec.Body.String())
		}
	}

	// The list page they CAN read still works — the gate is on write, not
	// a blanket refusal of the whole entity type.
	if rec := getAs(t, mux, "/records/Vendor", tenantID, "user-clerk"); rec.Code != http.StatusOK {
		t.Fatalf("clerk list page: expected 200, got %d", rec.Code)
	}
}

// Nav and dashboard stop linking to entity types the user cannot read,
// and a module left with nothing visible disappears rather than becoming
// a tile that leads to a 403.
func TestAPI_RBAC_NavAndDashboardHideDeniedEntityTypes(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	// accessibleModules only lists entity types that have a published
	// FORM as well as a published entity Definition, and foundation's own
	// forms aren't published here — so the module contents under test are
	// exactly the ones published below. Employee and Widget both
	// declare Module "foundation" (one denied, one not, so that module
	// survives minus a node); Vendor declares no Module and so lands
	// alone in "general", which must disappear entirely once denied.
	publishEntityAndForm(t, db, employeeEntityDef(), employeeFormDef())
	publishEntityAndForm(t, db, widgetEntityDef(), widgetFormDef())
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}, authz.AdminRoleCode: {"user-admin"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Employee", "can_read": false, "can_write": false},
			{"role": "clerk", "entity_type": "Vendor", "can_read": false, "can_write": false},
		},
	)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// The dashboard/nav switch between MODULES, so a module emptied of
	// everything readable must stop appearing at all.
	clerk := getAs(t, mux, "/", tenantID, "user-clerk")
	if clerk.Code != http.StatusOK {
		t.Fatalf("clerk dashboard: expected 200, got %d: %s", clerk.Code, clerk.Body.String())
	}
	if strings.Contains(clerk.Body.String(), "/modules/general") {
		t.Fatalf("dashboard linked to a module whose only entity is denied:\n%s", clerk.Body.String())
	}
	admin := getAs(t, mux, "/", tenantID, "user-admin")
	if !strings.Contains(admin.Body.String(), "/modules/general") {
		t.Fatalf("admin dashboard lost a readable module — filtering is not per-actor:\n%s", admin.Body.String())
	}

	// ...and that module's own page is a 404 for them, matching how
	// renderModuleMenu already treats a module a tenant can't reach.
	if got := getAs(t, mux, "/modules/general", tenantID, "user-clerk"); got.Code != http.StatusNotFound {
		t.Fatalf("emptied module page: expected 404, got %d", got.Code)
	}

	// A module that survives drops only the denied entity's node.
	menu := getAs(t, mux, "/modules/foundation", tenantID, "user-clerk")
	if menu.Code != http.StatusOK {
		t.Fatalf("clerk module menu: expected 200, got %d: %s", menu.Code, menu.Body.String())
	}
	if strings.Contains(menu.Body.String(), "/records/Employee") {
		t.Fatalf("module menu linked to a denied entity type:\n%s", menu.Body.String())
	}
	if !strings.Contains(menu.Body.String(), "/records/Widget") {
		t.Fatalf("module menu lost an entity type the clerk CAN read:\n%s", menu.Body.String())
	}
	adminMenu := getAs(t, mux, "/modules/foundation", tenantID, "user-admin")
	if !strings.Contains(adminMenu.Body.String(), "/records/Employee") {
		t.Fatalf("admin module menu lost a readable entity type:\n%s", adminMenu.Body.String())
	}
}

// Backward compatibility, asserted rather than assumed: with zero
// FieldPermission rows, every surface returns exactly what it did before
// this mechanism existed. This is what makes the live tenant (which has
// the entities published and deliberately no rows) provably unaffected.
func TestAPI_FieldPermission_NoRulesChangesNothing(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, employeeEntityDef(), employeeFormDef())
	if _, err := crud.NewEngine(db).Create(ctx, employeeEntityDef(),
		map[string]any{"name": "Dana", "salary": "120000"}, humanActor()); err != nil {
		t.Fatalf("seed Employee: %v", err)
	}
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := getAs(t, mux, "/api/records/Employee", tenantID, "anyone")
	var envelope struct {
		Data []recordResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if len(envelope.Data) != 1 || envelope.Data[0].Data["salary"] != "120000" {
		t.Fatalf("a tenant with no rules lost data: %v", envelope.Data)
	}
	for _, target := range []string{"/records/Employee", "/forms/Employee/new", "/import/Employee", "/export/Employee"} {
		if got := getAs(t, mux, target, tenantID, "anyone"); got.Code != http.StatusOK {
			t.Fatalf("GET %s with no rules: expected 200, got %d: %s", target, got.Code, got.Body.String())
		}
	}
}

// TestAPI_FieldPermission_DeniedUserGetsNoExistenceOracle is the
// regression test for a blocker independent review found in this
// commit's first draft.
//
// updateRecord calls EffectiveWriteFields before the checks that run
// checkWrite. In the first draft that function resolved field rules and
// then did a RAW (unguarded) Get to fetch the values it preserves — so a
// user with no access to the entity type at all, who merely happened to
// hold a role carrying a FieldPermission rule for it, could tell an
// existing record id from a missing one by whether they got 403 or 404,
// and the server performed a full unauthorized read to decide. An
// authorization function must not itself be the leak.
func TestAPI_FieldPermission_DeniedUserGetsNoExistenceOracle(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	publishEntityAndForm(t, db, employeeEntityDef(), employeeFormDef())

	// The clerk holds a field rule on Employee.salary...
	seedFieldRule(t, db, "clerk", "user-clerk", "Employee", "salary")
	// ...while Employee is opted into RBAC granting only somebody else.
	seedRBAC(t, db,
		map[string][]string{"other": {"user-other"}},
		[]map[string]any{{"role": "other", "entity_type": "Employee", "can_read": true, "can_write": true}},
	)

	rec, err := crud.NewEngine(db).Create(ctx, employeeEntityDef(),
		map[string]any{"name": "Dana", "salary": "120000"}, humanActor())
	if err != nil {
		t.Fatalf("seed Employee: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	post := func(id string) int {
		req := newRequest("POST", "/api/records/Employee/"+id, tenantID, "user-clerk", []byte(`{"name":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	existing, missing := post(rec.ID), post("00000000-0000-0000-0000-00000000dead")
	if existing != http.StatusForbidden {
		t.Fatalf("denied user updating an existing record: got %d, want 403", existing)
	}
	if existing != missing {
		t.Fatalf("existence oracle: existing id -> %d, nonexistent id -> %d (must be indistinguishable)", existing, missing)
	}
}

// TestAPI_FieldPermission_ImportMappingHidesField is the regression test
// for the second blocker from the same review: the import wizard's
// mapping <select> enumerates the Definition's fields, so — like list
// columns and CSV export headers — it is a field-metadata surface that
// does not inherit row-level redaction. The first draft filtered every
// other such surface and missed this one, publishing the hidden field's
// name to a user who cannot see it anywhere else.
func TestAPI_FieldPermission_ImportMappingHidesField(t *testing.T) {
	tenantID, _, mux, _ := fieldPermFixture(t)

	req := newMultipartRequest(t, "/import/Employee/preview", tenantID, "user-clerk",
		"staff.csv", []byte("name\nDana\n"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import preview: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "salary") {
		t.Fatalf("import mapping page names the hidden field:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "name") {
		t.Fatalf("import mapping page lost the visible field:\n%s", rec.Body.String())
	}
}
