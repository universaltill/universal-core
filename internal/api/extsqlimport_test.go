package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// registerTestPGSource registers the test suite's own Postgres (the
// tenant's database itself, reached over TCP like any external server)
// as an ExternalSQLSource and returns its record id — the import flow
// then genuinely dials out, discovers relations via INFORMATION_SCHEMA,
// and fetches rows, with no fake driver anywhere.
func registerTestPGSource(t *testing.T, mux *http.ServeMux, tenantID string, db *sql.DB) string {
	t.Helper()
	host, port, user, pass, database, options := testExtPGParams(t, db)
	rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Legacy", "postgres", host, port, database, user, pass, options))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 registering the test source, got %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Legacy")
	return id
}

// createScratchTable creates (and fills) an importable table in the
// tenant database — raw DDL is fine in a test, same as handlers_test.go's
// own direct queries.
func createScratchTable(t *testing.T, db *sql.DB, ddl string, inserts ...string) {
	t.Helper()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	for _, ins := range inserts {
		if _, err := db.Exec(ins); err != nil {
			t.Fatalf("fill scratch table: %v", err)
		}
	}
}

func postExtSQLImport(mux *http.ServeMux, target, tenantID string, vals url.Values) *httptest.ResponseRecorder {
	req := newRequest("POST", target, tenantID, "farshid", []byte(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestImport_UploadPage_LinksToSQLImport(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/import/Vendor", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/import/Vendor/sql"`) {
		t.Fatalf("expected the upload page to link to the SQL-source flow, got:\n%s", rec.Body.String())
	}
}

func TestExtSQLImport_Page_ListsRegisteredSources(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	registerTestPGSource(t, mux, tenantID, db)

	req := newRequest("GET", "/import/Vendor/sql", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="source_id"`) || !strings.Contains(body, ">Legacy<") {
		t.Fatalf("expected a source picker listing the registered source, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/import/Vendor/sql/relations"`) {
		t.Fatalf("expected a browse button targeting the relations endpoint, got:\n%s", body)
	}
}

func TestExtSQLImport_Page_NoSourcesShowsConfigureLink(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/import/Vendor/sql", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No external SQL sources are registered yet.") {
		t.Fatalf("expected the no-sources note, got:\n%s", body)
	}
	if !strings.Contains(body, `href="/settings/sql-sources"`) {
		t.Fatalf("expected a link to the SQL sources settings page, got:\n%s", body)
	}
}

func TestExtSQLImport_Relations_ListsExternalTables(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("name" text, "note" text)`,
		`INSERT INTO ext_vendors VALUES ('Acme Textiles', 'x')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/relations", tenantID,
		url.Values{"source_id": {sourceID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "public.ext_vendors") {
		t.Fatalf("expected the scratch table in the relations list, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/import/Vendor/sql/preview"`) {
		t.Fatalf("expected each relation to offer the preview step, got:\n%s", body)
	}
}

// TestExtSQLImport_Preview_SuggestsMappingAndShowsRows is the SQL-source
// counterpart of TestImport_Preview_SuggestsMappingAndShowsRows: with no
// template match, SuggestMapping's name-match pre-fills the mapping and
// the preview shows real fetched rows plus a Commit button and the
// translated row-cap note.
func TestExtSQLImport_Preview_SuggestsMappingAndShowsRows(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("name" text, "note" text)`,
		`INSERT INTO ext_vendors VALUES ('Acme Textiles', 'x')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/preview", tenantID,
		url.Values{"source_id": {sourceID}, "schema": {"public"}, "relation": {"ext_vendors"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="mapping.name"`) {
		t.Fatalf("expected a mapping select for the 'name' column, got:\n%s", body)
	}
	if !strings.Contains(body, `value="name" selected`) {
		t.Fatalf("expected SuggestMapping's guess (name->name) pre-selected, got:\n%s", body)
	}
	if !strings.Contains(body, "Acme Textiles") {
		t.Fatalf("expected the preview to show the fetched row's data, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/import/Vendor/sql/commit"`) {
		t.Fatalf("expected a Commit button targeting the SQL commit endpoint, got:\n%s", body)
	}
	if !strings.Contains(body, "10000") {
		t.Fatalf("expected the translated row-cap note stating the %d-row limit, got:\n%s", 10000, body)
	}
}

// TestExtSQLImport_Commit_WritesRecordsViaGuardedEngine commits two
// fetched rows and confirms they actually landed as queryable records —
// written through the same RBAC-guarded engine and audit actor the CSV
// commit uses (csvimport.CommitRows with ts.crud + rc.Actor).
func TestExtSQLImport_Commit_WritesRecordsViaGuardedEngine(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("name" text, "note" text)`,
		`INSERT INTO ext_vendors VALUES ('Acme Textiles', 'x'), ('Beta Supplies', 'y')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID,
		url.Values{
			"source_id":    {sourceID},
			"schema":       {"public"},
			"relation":     {"ext_vendors"},
			"mapping.name": {"name"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "2 succeeded") {
		t.Fatalf("expected the result to report 2 succeeded, got:\n%s", rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Vendor", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), "Acme Textiles") || !strings.Contains(listRec.Body.String(), "Beta Supplies") {
		t.Fatalf("expected both committed rows to actually be queryable afterward, got:\n%s", listRec.Body.String())
	}

	// The commit went through the guarded engine with the request's own
	// actor — the audit trail must say so.
	var audited int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type = 'Vendor' AND actor_id = 'farshid'`).Scan(&audited); err != nil {
		t.Fatalf("count Vendor audit rows: %v", err)
	}
	if audited != 2 {
		t.Fatalf("expected 2 audited Vendor creates for actor farshid, got %d", audited)
	}
}

// TestExtSQLImport_NAVTemplate_PrefillsMappingAndConstants exercises the
// vendor-template path end to end against a NAV-2009-shaped relation
// name ("<Company>$Customer"): the mapping pre-fills from the template,
// the template's constants render read-only with the translated
// fixed-value label, and the commit applies them — every imported Party
// gets party_type=organization without any source column carrying it.
func TestExtSQLImport_NAVTemplate_PrefillsMappingAndConstants(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	createScratchTable(t, db,
		`CREATE TABLE "CRONUS International Ltd_$Customer" ("Name" text, "VAT Registration No_" text)`,
		`INSERT INTO "CRONUS International Ltd_$Customer" VALUES ('Acme Corp', 'GB123456789')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	previewRec := postExtSQLImport(mux, "/import/Party/sql/preview", tenantID,
		url.Values{"source_id": {sourceID}, "schema": {"public"}, "relation": {"CRONUS International Ltd_$Customer"}})
	if previewRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}
	body := previewRec.Body.String()
	if !strings.Contains(body, "Microsoft Dynamics NAV 2009") {
		t.Fatalf("expected the template note naming the matched vendor template, got:\n%s", body)
	}
	if !strings.Contains(body, `value="name" selected`) || !strings.Contains(body, `value="tax_id" selected`) {
		t.Fatalf("expected the template mapping (Name->name, VAT->tax_id) pre-selected, got:\n%s", body)
	}
	if !strings.Contains(body, "party_type") || !strings.Contains(body, "organization") {
		t.Fatalf("expected the template constant party_type=organization rendered, got:\n%s", body)
	}
	if !strings.Contains(body, "fixed value") {
		t.Fatalf("expected the translated fixed-value label on the constants, got:\n%s", body)
	}
	if !strings.Contains(body, "Acme Corp") {
		t.Fatalf("expected the preview to show the fetched NAV row, got:\n%s", body)
	}

	commitRec := postExtSQLImport(mux, "/import/Party/sql/commit", tenantID,
		url.Values{
			"source_id":                    {sourceID},
			"schema":                       {"public"},
			"relation":                     {"CRONUS International Ltd_$Customer"},
			"mapping.Name":                 {"name"},
			"mapping.VAT Registration No_": {"tax_id"},
		})
	if commitRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	if !strings.Contains(commitRec.Body.String(), "1 succeeded") {
		t.Fatalf("expected 1 succeeded, got:\n%s", commitRec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Party", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	listBody := listRec.Body.String()
	if !strings.Contains(listBody, "Acme Corp") || !strings.Contains(listBody, "GB123456789") {
		t.Fatalf("expected the imported Party's mapped columns, got:\n%s", listBody)
	}
	if !strings.Contains(listBody, `"party_type":"organization"`) {
		t.Fatalf("expected the template constant applied to the stored record, got:\n%s", listBody)
	}
}

// TestExtSQLImport_Fragments_DeniedWithoutWritePermission: the htmx
// fragment endpoints carry the same CanWrite gate the page has — a
// read-only user must not be able to browse or fetch external tables
// through them (review finding: the page gate alone was bypassable by
// posting to the fragments directly).
func TestExtSQLImport_Fragments_DeniedWithoutWritePermission(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Vendor", "can_read": true, "can_write": false},
		},
	)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	for _, target := range []string{
		"/import/Vendor/sql/relations",
		"/import/Vendor/sql/preview",
		"/import/Vendor/sql/commit",
	} {
		req := newRequest("POST", target, tenantID, "user-clerk",
			[]byte(url.Values{"schema": {"public"}, "relation": {"ext_vendors"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 for a read-only user, got %d: %s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestExtSQLImport_SystemCatalogProbeIsRefused: schema/relation arrive
// raw from the form, so preview/commit re-check them against the
// Relations() listing — a hand-built pg_catalog.pg_shadow probe must get
// the generic error fragment, never the catalog's contents (review
// finding: the discovery listing's system-schema exclusion was
// otherwise cosmetic).
func TestExtSQLImport_SystemCatalogProbeIsRefused(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	for _, target := range []string{
		"/import/Vendor/sql/preview",
		"/import/Vendor/sql/commit",
	} {
		rec := postExtSQLImport(mux, target, tenantID, url.Values{
			"source_id": {sourceID},
			"schema":    {"pg_catalog"},
			"relation":  {"pg_shadow"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 (inline error fragment), got %d: %s", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Could not read from the external source.") {
			t.Fatalf("%s: expected the generic refusal, got:\n%s", target, body)
		}
		for _, leak := range []string{"pg_shadow", "usename", "rolpassword", "passwd"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s: system catalog content leaked (%q):\n%s", target, leak, body)
			}
		}
	}
}

// TestExtSQLImport_UserMappingBeatsTemplateConstant: mapping a real
// source column onto a field the matched template supplies as a
// constant must let the user's mapping win — not force the constant
// alongside it into a duplicate-source ValidateMapping error exposing
// the internal __const: sentinel.
func TestExtSQLImport_UserMappingBeatsTemplateConstant(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	createScratchTable(t, db,
		`CREATE TABLE "CRONUS International Ltd_$Customer" ("Name" text, "VAT Registration No_" text, "Type_" text)`,
		`INSERT INTO "CRONUS International Ltd_$Customer" VALUES ('Jane Smith', 'GB999', 'person')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	// The user maps the source's own Type_ column to party_type — the
	// field the NAV template pins to "organization" as a constant.
	vals := url.Values{
		"source_id":     {sourceID},
		"schema":        {"public"},
		"relation":      {"CRONUS International Ltd_$Customer"},
		"mapping.Name":  {"name"},
		"mapping.Type_": {"party_type"},
	}

	previewRec := postExtSQLImport(mux, "/import/Party/sql/preview", tenantID, vals)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", previewRec.Code, previewRec.Body.String())
	}
	body := previewRec.Body.String()
	if strings.Contains(body, "__const:") {
		t.Fatalf("the internal constant-column sentinel leaked into the preview:\n%s", body)
	}
	if strings.Contains(body, "uc-import-mapping-error") {
		t.Fatalf("expected no mapping error when the user's mapping overrides the constant, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/import/Party/sql/commit"`) {
		t.Fatalf("expected a Commit button (valid mapping), got:\n%s", body)
	}

	commitRec := postExtSQLImport(mux, "/import/Party/sql/commit", tenantID, vals)
	if commitRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}
	if !strings.Contains(commitRec.Body.String(), "1 succeeded") {
		t.Fatalf("expected 1 succeeded, got:\n%s", commitRec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Party", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), `"party_type":"person"`) {
		t.Fatalf("expected the user-mapped column value (person), not the template constant (organization), got:\n%s", listRec.Body.String())
	}
}

// TestExtSQLImport_ConnectionError_ShowsGenericTranslatedMessage: a
// fetch against an unreachable source renders only the generic
// translated failure fragment — never the driver error, which embeds
// the DSN (host and credentials included).
func TestExtSQLImport_ConnectionError_ShowsGenericTranslatedMessage(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	if rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Broken", "postgres", "127.0.0.1", "1", "nowhere", "", "", "")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 registering the broken source, got %d: %s", rec.Code, rec.Body.String())
	}
	sourceID, _ := storedExtSQLSource(t, db, "Broken")

	for _, target := range []string{
		"/import/Vendor/sql/relations",
		"/import/Vendor/sql/preview",
	} {
		vals := url.Values{"source_id": {sourceID}, "schema": {"public"}, "relation": {"ext_vendors"}}
		rec := postExtSQLImport(mux, target, tenantID, vals)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 (inline error fragment), got %d: %s", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Could not read from the external source.") {
			t.Fatalf("%s: expected the generic translated failure, got:\n%s", target, body)
		}
		for _, leak := range []string{"postgres://", "dial tcp", "connection refused", "failed to connect", "127.0.0.1"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s: driver/DSN detail leaked into the response (%q):\n%s", target, leak, body)
			}
		}
	}
}
