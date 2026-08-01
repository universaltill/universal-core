package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// registerTestPGSource registers the test suite's own Postgres (the
// tenant's database itself, reached over TCP like any external server)
// as an ExternalSQLSource and returns its record id — the import flow
// then genuinely dials out, discovers relations via INFORMATION_SCHEMA,
// and fetches rows, with no fake driver anywhere.
func registerTestPGSource(t *testing.T, mux *http.ServeMux, tenantID string, db *sql.DB) string {
	t.Helper()
	return registerTestPGSourceNamed(t, mux, tenantID, db, "Legacy")
}

// registerTestPGSourceNamed is registerTestPGSource with a caller-chosen
// name — for tests that need TWO registered sources (both genuinely
// dialing the same test Postgres) distinguishable by name.
func registerTestPGSourceNamed(t *testing.T, mux *http.ServeMux, tenantID string, db *sql.DB, name string) string {
	t.Helper()
	host, port, user, pass, database, options := testExtPGParams(t, db)
	rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues(name, "postgres", host, port, database, user, pass, options))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 registering the test source %q, got %d: %s", name, rec.Code, rec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, name)
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
	if !strings.Contains(rec.Body.String(), "2 created, 0 updated") {
		t.Fatalf("expected the result to report 2 created, got:\n%s", rec.Body.String())
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
	if !strings.Contains(commitRec.Body.String(), "1 created, 0 updated") {
		t.Fatalf("expected 1 created, got:\n%s", commitRec.Body.String())
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
	if !strings.Contains(commitRec.Body.String(), "1 created, 0 updated") {
		t.Fatalf("expected 1 created, got:\n%s", commitRec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Party", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), `"party_type":"person"`) {
		t.Fatalf("expected the user-mapped column value (person), not the template constant (organization), got:\n%s", listRec.Body.String())
	}
}

// TestExtSQLImport_Preview_KeyColumnPrefilledFromTemplate (uc-infra#101):
// when the matched template names a KeyColumn the relation actually has,
// the mapping form's key-column select renders with it pre-selected,
// alongside the translated help text and the "matching rows will update"
// note.
func TestExtSQLImport_Preview_KeyColumnPrefilledFromTemplate(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	createScratchTable(t, db,
		`CREATE TABLE "CRONUS International Ltd_$Customer" ("No_" text, "Name" text, "VAT Registration No_" text)`,
		`INSERT INTO "CRONUS International Ltd_$Customer" VALUES ('C0001', 'Acme Corp', 'GB123456789')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Party/sql/preview", tenantID,
		url.Values{"source_id": {sourceID}, "schema": {"public"}, "relation": {"CRONUS International Ltd_$Customer"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="key_column"`) {
		t.Fatalf("expected the key-column select, got:\n%s", body)
	}
	if !strings.Contains(body, `value="No_" selected`) {
		t.Fatalf("expected the template's KeyColumn (No_) pre-selected, got:\n%s", body)
	}
	if !strings.Contains(body, "re-importing updates the rows imported earlier") {
		t.Fatalf("expected the translated key-column help text, got:\n%s", body)
	}
	if !strings.Contains(body, "A key column is selected") {
		t.Fatalf("expected the will-update note when a key column is selected, got:\n%s", body)
	}
}

// TestExtSQLImport_Commit_WithKeyColumnIsIdempotent is uc-infra#101's
// end-to-end proof: the same keyed commit run twice reports 2 created
// then 2 updated, and the live record count stays constant — no
// duplicates.
func TestExtSQLImport_Commit_WithKeyColumnIsIdempotent(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles'), ('V-2', 'Beta Supplies')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	vals := url.Values{
		"source_id":    {sourceID},
		"schema":       {"public"},
		"relation":     {"ext_vendors"},
		"mapping.name": {"name"},
		"key_column":   {"code"},
	}

	first := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, vals)
	if first.Code != http.StatusOK {
		t.Fatalf("first commit: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), "2 created, 0 updated, 0 failed") {
		t.Fatalf("first commit: expected 2 created, got:\n%s", first.Body.String())
	}

	second := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, vals)
	if second.Code != http.StatusOK {
		t.Fatalf("second commit: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "0 created, 2 updated, 0 failed") {
		t.Fatalf("second commit: expected 2 updated, got:\n%s", second.Body.String())
	}
	if !strings.Contains(second.Body.String(), ">Updated<") {
		t.Fatalf("second commit: expected per-row Updated status, got:\n%s", second.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'Vendor' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count Vendor records: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected the keyed re-run to leave exactly 2 Vendor records, found %d", count)
	}

	// The identity rows carry the schema-qualified relation — the scope
	// that keeps two relations importing into one entity type from
	// sharing a key namespace (sqlsource's package comment).
	var identities int
	if err := db.QueryRow(
		`SELECT count(*) FROM records WHERE entity_type = 'ExternalIdentity' AND deleted_at IS NULL AND data->>'source_relation' = 'public.ext_vendors'`,
	).Scan(&identities); err != nil {
		t.Fatalf("count ExternalIdentity records: %v", err)
	}
	if identities != 2 {
		t.Fatalf("expected 2 identity rows scoped to public.ext_vendors, found %d", identities)
	}
}

// TestExtSQLImport_Commit_WithoutKeyColumnDuplicatesOnRerun pins the
// create-only path's unchanged behavior: no key column means every run
// creates fresh records — exactly what the reworded row-cap note warns.
func TestExtSQLImport_Commit_WithoutKeyColumnDuplicatesOnRerun(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles'), ('V-2', 'Beta Supplies')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	vals := url.Values{
		"source_id":    {sourceID},
		"schema":       {"public"},
		"relation":     {"ext_vendors"},
		"mapping.name": {"name"},
	}
	for run := 1; run <= 2; run++ {
		rec := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, vals)
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d: expected 200, got %d: %s", run, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "2 created, 0 updated") {
			t.Fatalf("run %d: expected 2 created, got:\n%s", run, rec.Body.String())
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'Vendor' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count Vendor records: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected the keyless re-run to duplicate (4 records), found %d", count)
	}
}

// TestExtSQLImport_Commit_KeyColumnWithoutIdentityDefRendersTranslatedError:
// a tenant whose foundation set predates ExternalIdentity gets the clear
// translated explanation on a keyed commit, not a 500 — tenant sync owns
// publishing the definition, the handler only has to fail legibly.
func TestExtSQLImport_Commit_KeyColumnWithoutIdentityDefRendersTranslatedError(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	// Deliberately NOT publishFoundation: only the two definitions this
	// flow strictly needs, leaving ExternalIdentity unpublished.
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, db, foundation.ExternalSQLSource(), &form.Definition{
		EntityType: "ExternalSQLSource",
		Version:    1,
		Sections: []form.Section{{Title: "Details", Component: form.ComponentFields,
			Fields: []form.FormField{{Name: "name", Label: "Name"}}}},
	})
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID,
		url.Values{
			"source_id":    {sourceID},
			"schema":       {"public"},
			"relation":     {"ext_vendors"},
			"mapping.name": {"name"},
			"key_column":   {"code"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline translated error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Update-on-re-import is not available for this tenant yet") {
		t.Fatalf("expected the translated identity-unavailable error, got:\n%s", rec.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'Vendor' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count Vendor records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected nothing written when the keyed commit is refused, found %d", count)
	}
}

// TestExtSQLImport_KeyedImports_ScopedPerRelation is the review's NAV
// scenario made concrete: two relations with overlapping keys, both
// importing into the same entity type. The second relation's rows must
// CREATE — never silently overwrite the first relation's records just
// because a legacy number series repeats across tables.
func TestExtSQLImport_KeyedImports_ScopedPerRelation(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_customers ("code" text, "name" text)`,
		`INSERT INTO ext_customers VALUES ('10000', 'Customer Ten Thousand')`)
	createScratchTable(t, db,
		`CREATE TABLE ext_suppliers ("code" text, "name" text)`,
		`INSERT INTO ext_suppliers VALUES ('10000', 'Supplier Ten Thousand')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	commit := func(relation string) *httptest.ResponseRecorder {
		return postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, url.Values{
			"source_id":    {sourceID},
			"schema":       {"public"},
			"relation":     {relation},
			"mapping.name": {"name"},
			"key_column":   {"code"},
		})
	}

	if rec := commit("ext_customers"); !strings.Contains(rec.Body.String(), "1 created, 0 updated") {
		t.Fatalf("first relation: expected 1 created, got %d:\n%s", rec.Code, rec.Body.String())
	}
	// Same key "10000", different relation: a create, not an update of
	// the customer-relation record.
	if rec := commit("ext_suppliers"); !strings.Contains(rec.Body.String(), "1 created, 0 updated") {
		t.Fatalf("second relation: expected 1 created (no cross-relation overwrite), got %d:\n%s", rec.Code, rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/Vendor", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if !strings.Contains(listRec.Body.String(), "Customer Ten Thousand") || !strings.Contains(listRec.Body.String(), "Supplier Ten Thousand") {
		t.Fatalf("expected both relations' records to coexist, got:\n%s", listRec.Body.String())
	}
}

// TestExtSQLImport_IdentityWritesAreControlPlaneGated: ExternalIdentity
// is control-plane-gated in authz — once a tenant configures RBAC, a
// non-admin user's generic write to it is denied (an identity row
// re-points what the next import updates), while the keyed import
// itself, run by a user with ordinary write permission on the target
// entity, still works via the importer's raw-engine side-channel.
func TestExtSQLImport_IdentityWritesAreControlPlaneGated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles')`)

	seedRBAC(t, db,
		map[string][]string{"importer": {"user-importer"}},
		[]map[string]any{
			{"role": "importer", "entity_type": "Vendor", "can_read": true, "can_write": true},
		},
	)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	// The generic record API must refuse the identity write outright.
	denyReq := newRequest("POST", "/api/records/ExternalIdentity", tenantID, "user-importer",
		[]byte(`{"source_id":"`+sourceID+`","source_relation":"public.ext_vendors","entity_type":"Vendor","record_id":"11111111-1111-1111-1111-111111111111","external_key":"V-1"}`))
	denyRec := httptest.NewRecorder()
	mux.ServeHTTP(denyRec, denyReq)
	if denyRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a user's generic ExternalIdentity write, got %d: %s", denyRec.Code, denyRec.Body.String())
	}

	// The keyed import as that same ordinary user still works end to end.
	req := newRequest("POST", "/import/Vendor/sql/commit", tenantID, "user-importer",
		[]byte(url.Values{
			"source_id":    {sourceID},
			"schema":       {"public"},
			"relation":     {"ext_vendors"},
			"mapping.name": {"name"},
			"key_column":   {"code"},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for the keyed import, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1 created, 0 updated") {
		t.Fatalf("expected 1 created, got:\n%s", rec.Body.String())
	}

	var identities int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'ExternalIdentity' AND deleted_at IS NULL`).Scan(&identities); err != nil {
		t.Fatalf("count ExternalIdentity records: %v", err)
	}
	if identities != 1 {
		t.Fatalf("expected the import's own identity row to be written despite the gate, found %d", identities)
	}
}

// TestExtSQLImport_Preview_BlankKeyCellIsRowError: with a key column
// selected, a row whose key cell is blank shows as a row error at
// PREVIEW, exactly as commit would report it — the two stages must not
// disagree (sqlsource.MarkMissingKeys).
func TestExtSQLImport_Preview_BlankKeyCellIsRowError(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles'), ('', 'Keyless Supplies')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/preview", tenantID, url.Values{
		"source_id":      {sourceID},
		"schema":         {"public"},
		"relation":       {"ext_vendors"},
		"mapping.name":   {"name"},
		"key_column":     {"code"},
		"key_column_set": {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "uc-row-error") {
		t.Fatalf("expected the blank-key row marked as an error row at preview, got:\n%s", body)
	}
	if !strings.Contains(body, "missing key") {
		t.Fatalf("expected the missing-key row error text, got:\n%s", body)
	}
	if !strings.Contains(body, "uc-row-ok") {
		t.Fatalf("expected the keyed row to still preview OK, got:\n%s", body)
	}
}

// TestExtSQLImport_KeyColumnNotAColumnRendersTranslatedError: a
// hand-built/stale key_column that isn't one of the relation's columns
// gets the translated error fragment, not a hardcoded-English 400.
func TestExtSQLImport_KeyColumnNotAColumnRendersTranslatedError(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	rec := postExtSQLImport(mux, "/import/Vendor/sql/preview", tenantID, url.Values{
		"source_id":      {sourceID},
		"schema":         {"public"},
		"relation":       {"ext_vendors"},
		"mapping.name":   {"name"},
		"key_column":     {"no_such_column"},
		"key_column_set": {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline translated error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "The selected key column is not a column of this table.") {
		t.Fatalf("expected the translated key-column error, got:\n%s", rec.Body.String())
	}
}

// TestExtSQLImport_Preview_ExplicitNoneKeyColumnSurvivesResubmit pins
// the pre-fill gating fix: a user who resubmits with key_column
// explicitly set to none — even with every column unmapped — must not
// have the template's KeyColumn silently reapplied. Only the very first
// render (no key_column_set marker) gets the template pre-fill.
func TestExtSQLImport_Preview_ExplicitNoneKeyColumnSurvivesResubmit(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	createScratchTable(t, db,
		`CREATE TABLE "CRONUS International Ltd_$Customer" ("No_" text, "Name" text, "VAT Registration No_" text)`,
		`INSERT INTO "CRONUS International Ltd_$Customer" VALUES ('C0001', 'Acme Corp', 'GB123456789')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	// A resubmission that deliberately unmapped everything AND chose no
	// key column — the shape that used to be indistinguishable from a
	// first render.
	rec := postExtSQLImport(mux, "/import/Party/sql/preview", tenantID, url.Values{
		"source_id":      {sourceID},
		"schema":         {"public"},
		"relation":       {"CRONUS International Ltd_$Customer"},
		"key_column":     {""},
		"key_column_set": {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `value="No_" selected`) {
		t.Fatalf("expected the template KeyColumn NOT to override an explicit none, got:\n%s", body)
	}
	if strings.Contains(body, "A key column is selected") {
		t.Fatalf("expected no will-update note with key column explicitly none, got:\n%s", body)
	}
}

// TestExtSQLImport_KeyedReImportWorksUnderReadOnlySoR pins uc-infra#102's
// blocker fix end to end: with a read_only SystemOfRecord row naming the
// import's own source, the keyed re-import must still succeed — the
// import is the source's own pen (ForImportFrom's scoped bypass), and
// without it the SoR check blocked the very sync it protects, per-row.
func TestExtSQLImport_KeyedReImportWorksUnderReadOnlySoR(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles'), ('V-2', 'Beta Supplies')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	sourceID := registerTestPGSource(t, mux, tenantID, db)

	// The source is declared the read-only owner of Vendor — seeded via
	// the raw engine (system setup, not what this test is probing).
	if _, err := crud.NewEngine(db).Create(context.Background(), foundation.SystemOfRecord(), map[string]any{
		"entity_type": "Vendor", "source_id": sourceID, "mode": "read_only",
	}, humanActor()); err != nil {
		t.Fatalf("seed SystemOfRecord: %v", err)
	}

	vals := url.Values{
		"source_id":    {sourceID},
		"schema":       {"public"},
		"relation":     {"ext_vendors"},
		"mapping.name": {"name"},
		"key_column":   {"code"},
	}
	first := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, vals)
	if !strings.Contains(first.Body.String(), "2 created, 0 updated, 0 failed") {
		t.Fatalf("first keyed import under read_only SoR: expected 2 created, got %d:\n%s", first.Code, first.Body.String())
	}
	second := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, vals)
	if !strings.Contains(second.Body.String(), "0 created, 2 updated, 0 failed") {
		t.Fatalf("keyed RE-import under read_only SoR: expected 2 updated (the source's own pen), got %d:\n%s", second.Code, second.Body.String())
	}

	// The protection itself still stands for a human hand-edit.
	var recordID string
	if err := db.QueryRow(`SELECT id FROM records WHERE entity_type = 'Vendor' AND deleted_at IS NULL LIMIT 1`).Scan(&recordID); err != nil {
		t.Fatalf("pick an imported Vendor: %v", err)
	}
	editReq := newRequest("POST", "/api/records/Vendor/"+recordID, tenantID, "farshid", []byte(`{"name":"Hand Edited"}`))
	editRec := httptest.NewRecorder()
	mux.ServeHTTP(editRec, editReq)
	if editRec.Code != http.StatusConflict {
		t.Fatalf("expected the hand-edit to stay blocked (409), got %d: %s", editRec.Code, editRec.Body.String())
	}
}

// TestExtSQLImport_KeyedImportFromOtherSourceBlockedPerRowTranslated:
// ForImportFrom's bypass is scoped to the import's OWN source — a keyed
// import from a different registered source whose rows resolve to
// records the read_only source owns fails per-row, with the translated
// block message in the result rows (never authz's logs-only English
// Error() text, never a 500, never silent success).
func TestExtSQLImport_KeyedImportFromOtherSourceBlockedPerRowTranslated(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	createScratchTable(t, db,
		`CREATE TABLE ext_vendors ("code" text, "name" text)`,
		`INSERT INTO ext_vendors VALUES ('V-1', 'Acme Textiles'), ('V-2', 'Beta Supplies')`)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)
	ownerID := registerTestPGSourceNamed(t, mux, tenantID, db, "Legacy")
	otherID := registerTestPGSourceNamed(t, mux, tenantID, db, "Other")

	// Owner imports first and is declared read-only owner of Vendor.
	ownerVals := url.Values{
		"source_id":    {ownerID},
		"schema":       {"public"},
		"relation":     {"ext_vendors"},
		"mapping.name": {"name"},
		"key_column":   {"code"},
	}
	if rec := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, ownerVals); !strings.Contains(rec.Body.String(), "2 created") {
		t.Fatalf("owner import: expected 2 created, got %d:\n%s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	engine := crud.NewEngine(db)
	if _, err := engine.Create(ctx, foundation.SystemOfRecord(), map[string]any{
		"entity_type": "Vendor", "source_id": ownerID, "mode": "read_only",
	}, humanActor()); err != nil {
		t.Fatalf("seed SystemOfRecord: %v", err)
	}

	// The other source's identity rows resolve to the SAME records (a
	// dual-sourced mirror) — seeded via the raw engine by copying the
	// owner's identities under the other source's id.
	rows, err := db.Query(`SELECT data->>'record_id', data->>'external_key' FROM records WHERE entity_type = 'ExternalIdentity' AND deleted_at IS NULL AND data->>'source_id' = $1`, ownerID)
	if err != nil {
		t.Fatalf("read owner identities: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var recordID, key string
		if err := rows.Scan(&recordID, &key); err != nil {
			t.Fatalf("scan identity: %v", err)
		}
		if _, err := engine.Create(ctx, foundation.ExternalIdentity(), map[string]any{
			"source_id":       otherID,
			"source_relation": "public.ext_vendors",
			"entity_type":     "Vendor",
			"record_id":       recordID,
			"external_key":    key,
		}, humanActor()); err != nil {
			t.Fatalf("seed other-source identity: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate identities: %v", err)
	}

	otherVals := url.Values{
		"source_id":    {otherID},
		"schema":       {"public"},
		"relation":     {"ext_vendors"},
		"mapping.name": {"name"},
		"key_column":   {"code"},
	}
	rec := postExtSQLImport(mux, "/import/Vendor/sql/commit", tenantID, otherVals)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-row errors, not a request failure), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "0 created, 0 updated, 2 failed") {
		t.Fatalf("expected every row blocked, got:\n%s", body)
	}
	if !strings.Contains(body, "This record is owned by Legacy") {
		t.Fatalf("expected the translated per-row block message naming the owning source, got:\n%s", body)
	}
	if strings.Contains(body, "read-only external source") {
		t.Fatalf("authz's logs-only Error() text leaked into the result rows:\n%s", body)
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
