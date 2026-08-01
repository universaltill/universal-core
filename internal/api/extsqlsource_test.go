package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// extSQLSourceValues builds the settings form's urlencoded field set.
func extSQLSourceValues(name, driver, host, port, database, username, password, options string) url.Values {
	v := url.Values{}
	v.Set("name", name)
	v.Set("driver", driver)
	v.Set("host", host)
	v.Set("port", port)
	v.Set("database", database)
	v.Set("username", username)
	v.Set("password", password)
	v.Set("options", options)
	return v
}

func postExtSQLForm(mux *http.ServeMux, target, tenantID string, vals url.Values) *httptest.ResponseRecorder {
	req := newRequest("POST", target, tenantID, "farshid", []byte(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// storedExtSQLSource reads one ExternalSQLSource record's id and stored
// password ciphertext straight from the tenant database — the settings
// page never renders either, so proving what actually landed in storage
// has to look underneath it.
func storedExtSQLSource(t *testing.T, db *sql.DB, name string) (id, passwordEncrypted string) {
	t.Helper()
	err := db.QueryRow(
		`SELECT id, COALESCE(data->>'password_encrypted', '') FROM records WHERE entity_type = 'ExternalSQLSource' AND data->>'name' = $1`,
		name,
	).Scan(&id, &passwordEncrypted)
	if err != nil {
		t.Fatalf("read stored ExternalSQLSource %q: %v", name, err)
	}
	return id, passwordEncrypted
}

// testExtPGParams derives connection parameters for the same Postgres
// server TEST_DATABASE_URL points at, with database set to tenantDB's
// own database — a real, reachable external source for tests, without
// assuming any second server exists.
func testExtPGParams(t *testing.T, tenantDB *sql.DB) (host, port, user, pass, database, options string) {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	host = u.Hostname()
	port = u.Port()
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	options = u.RawQuery
	if err := tenantDB.QueryRow(`SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("resolve tenant database name: %v", err)
	}
	return host, port, user, pass, database, options
}

func TestExtSQLSources_Page_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/settings/sql-sources", nil) // no auth headers
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExtSQLSources_Page_RendersEmptyList(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/settings/sql-sources", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No external SQL sources are registered yet.") {
		t.Fatalf("expected the empty-list note, got:\n%s", body)
	}
	if !strings.Contains(body, `action="/settings/sql-sources"`) {
		t.Fatalf("expected the create form, got:\n%s", body)
	}
}

// TestExtSQLSources_Create_EncryptsPasswordAndNeverEchoesIt is the core
// secret-discipline proof, mirroring
// TestAIProviderSettings_Save_AnthropicEncryptsKeyAndNeverEchoesIt: the
// password reaches storage encrypted (round-trippable through the
// handler's own Cryptor, and never ciphertext == plaintext), and neither
// the settings response nor the generic record API ever carries the
// plaintext.
func TestExtSQLSources_Create_EncryptsPasswordAndNeverEchoesIt(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	cryptor := testCryptor(t)
	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, cryptor).Routes(mux)

	const plainPassword = "sa-password-should-never-appear"
	rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("NAV", "mssql", "nav.example.internal", "1433", "NAVDB", "reader", plainPassword, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plainPassword) {
		t.Fatalf("expected the plaintext password to never appear in the response, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "A password is already saved.") {
		t.Fatalf("expected the page to report a password is stored, got:\n%s", rec.Body.String())
	}

	_, encrypted := storedExtSQLSource(t, db, "NAV")
	if encrypted == "" || encrypted == plainPassword {
		t.Fatalf("expected an encrypted stored password, got %q", encrypted)
	}
	decrypted, err := cryptor.Decrypt(encrypted)
	if err != nil || decrypted != plainPassword {
		t.Fatalf("expected the stored ciphertext to decrypt back to the plaintext, got %q, %v", decrypted, err)
	}

	listReq := newRequest("GET", "/api/records/ExternalSQLSource", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), plainPassword) {
		t.Fatalf("expected the stored record to never contain the plaintext password, got:\n%s", listRec.Body.String())
	}
}

// TestExtSQLSources_Create_PasswordWithoutCryptorFails confirms the page
// refuses to store a password at all when no SECRET_ENCRYPTION_KEY is
// configured, with a translated inline error — never silently storing
// it unencrypted (aiprovidersettings' exact convention).
func TestExtSQLSources_Create_PasswordWithoutCryptorFails(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux) // no secret cryptor

	rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("NAV", "mssql", "nav.example.internal", "", "NAVDB", "reader", "secret-pw", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SECRET_ENCRYPTION_KEY") {
		t.Fatalf("expected an inline error naming the missing configuration, got:\n%s", rec.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'ExternalSQLSource'`).Scan(&count); err != nil {
		t.Fatalf("count ExternalSQLSource records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected nothing stored when encryption is unavailable, found %d records", count)
	}
}

// TestExtSQLSources_Update_BlankPasswordKeepsStoredSecret is the
// blank-means-unchanged proof: editing a source without retyping the
// password must carry the stored ciphertext forward untouched, while the
// visibly-edited field actually changes.
func TestExtSQLSources_Update_BlankPasswordKeepsStoredSecret(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)

	if rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("NAV", "mssql", "old-host.internal", "1433", "NAVDB", "reader", "secret-pw", "")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on create, got %d: %s", rec.Code, rec.Body.String())
	}
	id, encryptedBefore := storedExtSQLSource(t, db, "NAV")
	if encryptedBefore == "" {
		t.Fatal("expected a stored encrypted password after create")
	}

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id, tenantID,
		extSQLSourceValues("NAV", "mssql", "new-host.internal", "1433", "NAVDB", "reader", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on edit, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new-host.internal") {
		t.Fatalf("expected the edited host to be saved, got:\n%s", rec.Body.String())
	}

	_, encryptedAfter := storedExtSQLSource(t, db, "NAV")
	if encryptedAfter != encryptedBefore {
		t.Fatalf("expected a blank password on edit to keep the stored ciphertext unchanged, got %q -> %q", encryptedBefore, encryptedAfter)
	}
}

func TestExtSQLSources_Delete_RemovesSource(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	if rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Mirror", "postgres", "mirror.internal", "", "reporting", "", "", "")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on create, got %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Mirror")

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id+"/delete", tenantID, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No external SQL sources are registered yet.") {
		t.Fatalf("expected the empty-list note after delete, got:\n%s", rec.Body.String())
	}

	// crud.Delete is a soft delete (records.deleted_at) — "gone" means
	// gone from every live query, not the row physically vanishing.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'ExternalSQLSource' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count ExternalSQLSource records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the record to actually be gone, found %d", count)
	}
}

// TestExtSQLSources_TestConnection_Succeeds points a registered source
// at the test suite's own Postgres — a genuinely reachable external
// database — and confirms the success message renders.
func TestExtSQLSources_TestConnection_Succeeds(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)

	host, port, user, pass, database, options := testExtPGParams(t, db)
	if rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Self", "postgres", host, port, database, user, pass, options)); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on create, got %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Self")

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id+"/test", tenantID, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Connection succeeded.") {
		t.Fatalf("expected the success message, got:\n%s", rec.Body.String())
	}
}

// TestExtSQLSources_TestConnection_FailureIsGenericAndNeverLeaksDSN
// registers an unreachable source and confirms the failure renders only
// the generic translated message — never the driver error's text, which
// routinely embeds the composed DSN (scheme, credentials and all).
func TestExtSQLSources_TestConnection_FailureIsGenericAndNeverLeaksDSN(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// Port 1 on loopback: refused immediately, no long dial timeout.
	if rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Broken", "postgres", "127.0.0.1", "1", "nowhere", "", "", "")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on create, got %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Broken")

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id+"/test", tenantID, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Connection failed.") {
		t.Fatalf("expected the generic translated failure message, got:\n%s", body)
	}
	// The driver error for a refused dial reads like `dial tcp
	// 127.0.0.1:1: connect: connection refused` and pgx's wrapper embeds
	// the DSN — none of that may reach the browser.
	for _, leak := range []string{"postgres://", "dial tcp", "connection refused", "failed to connect"} {
		if strings.Contains(body, leak) {
			t.Fatalf("driver error text leaked into the response (%q):\n%s", leak, body)
		}
	}
}

// TestExtSQLSources_Create_LinkLocalHostRejected: the settings form
// refuses a link-local host outright with the translated inline error —
// the same cloud-metadata SSRF defense validateOllamaBaseURL applies to
// the Ollama base_url (see validateExtSQLHost).
func TestExtSQLSources_Create_LinkLocalHostRejected(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postExtSQLForm(mux, "/settings/sql-sources", tenantID,
		extSQLSourceValues("Metadata", "postgres", "169.254.169.254", "", "latest", "", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "link-local") {
		t.Fatalf("expected the translated link-local error, got:\n%s", rec.Body.String())
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'ExternalSQLSource' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count ExternalSQLSource records: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no record stored for a link-local host, found %d", count)
	}
}

// TestExtSQLSources_TestConnection_BypassStoredLinkLocalHostRefused
// covers the funnel behind the form: a record written through the
// generic JSON API never went through buildExtSQLSourceFields, so the
// dial-time check in extSQLSourceFromFields must refuse it — reaching
// the user only as the generic failure, never a dial to the metadata
// endpoint.
func TestExtSQLSources_TestConnection_BypassStoredLinkLocalHostRefused(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/ExternalSQLSource", tenantID, "farshid",
		[]byte(`{"name":"Sneaky","driver":"postgres","host":"169.254.169.254","database":"latest"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 via the generic API, got %d: %s", createRec.Code, createRec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Sneaky")

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id+"/test", tenantID, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Connection failed.") {
		t.Fatalf("expected the generic failure for a stored link-local host, got:\n%s", rec.Body.String())
	}
}

// TestExtSQLSources_TestConnection_HugeStoredPortFailsGenerically: a
// port far outside the valid range, stored via the generic JSON API
// (the form path already rejects it), must be refused at dial time
// rather than silently truncated by int(float64) — surfacing as the
// generic failure message.
func TestExtSQLSources_TestConnection_HugeStoredPortFailsGenerically(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	createReq := newRequest("POST", "/api/records/ExternalSQLSource", tenantID, "farshid",
		[]byte(`{"name":"Huge","driver":"postgres","host":"127.0.0.1","port":1000000000000,"database":"x"}`))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 via the generic API, got %d: %s", createRec.Code, createRec.Body.String())
	}
	id, _ := storedExtSQLSource(t, db, "Huge")

	rec := postExtSQLForm(mux, "/settings/sql-sources/"+id+"/test", tenantID, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Connection failed.") {
		t.Fatalf("expected the generic failure for an out-of-range stored port, got:\n%s", rec.Body.String())
	}
}
