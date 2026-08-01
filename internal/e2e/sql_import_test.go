// Real-browser E2E coverage for the External SQL sources settings page
// (internal/api/extsqlsource.go) and the "import from a SQL source" flow
// (internal/api/extsqlimport.go) — ADR-0019. The handler tests
// (internal/api/extsqlsource_test.go, extsqlimport_test.go) already prove
// the rendered HTML strings; this file proves the same flows through a
// real headless Chrome driving real form posts and real htmx swaps, on
// csv_import_test.go's harness.
//
// The "external" source is the trick the handler tests established: the
// tenant's own physical database, registered as an ExternalSQLSource and
// reached back over TCP like any legacy server would be — the import flow
// genuinely dials out, discovers relations via INFORMATION_SCHEMA, and
// fetches rows, with no fake driver anywhere. The import test's source is
// passwordless (a local trust-auth Postgres needs none), so the dial-out
// path never needs a SECRET_ENCRYPTION_KEY; the settings test stores a
// password anyway — through a real Cryptor wired into the server — because
// "the plaintext never renders" is only worth asserting when a password
// actually exists to leak.
package e2e

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/secretcrypt"
	"github.com/universaltill/universal-core/internal/testexec"
)

// e2eCryptorKey is a fixed test-only AES-256 key (base64 of 32 bytes) —
// the settings test needs the server to actually encrypt a submitted
// password, exactly as a SECRET_ENCRYPTION_KEY-configured deployment
// would; nothing here is a secret worth protecting.
var e2eCryptorKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// testServerWithSecretCryptor is csv_import_test.go's testServer with one
// difference: the Handler gets a real secretcrypt.Cryptor (testServer
// passes nil), so the SQL-sources settings page can store an encrypted
// password the way production does.
func testServerWithSecretCryptor(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	router := newTestRouter(t)
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "E2E Tenant", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}

	cryptor, err := secretcrypt.NewCryptor(e2eCryptorKey)
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	mux := http.NewServeMux()
	api.New(router, catalog, nil, nil, nil, nil, cryptor).Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, id, tenantDB
}

// extSQLPGParams derives connection parameters for the same Postgres
// server TEST_DATABASE_URL points at, with database set to tenantDB's own
// database — internal/api's testExtPGParams, re-derived here because test
// helpers don't cross packages. This is what makes the tenant's own
// database a real, reachable "external" source without assuming a second
// server exists.
func extSQLPGParams(t *testing.T, tenantDB *sql.DB) (host, port, user, pass, database, options string) {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	host = u.Hostname()
	port = u.Port()
	if u.User != nil {
		user = u.User.Username()
		// The password must travel too: locally a trust-auth Postgres
		// has none, but CI's service container requires one — dropping
		// it made the wizard's dial-out fail only in CI (relations list
		// never rendered, 30s deadline).
		pass, _ = u.User.Password()
	}
	options = u.RawQuery
	if err := tenantDB.QueryRow(`SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("resolve tenant database name: %v", err)
	}
	return host, port, user, pass, database, options
}

// registerTestPGSource registers the tenant's own database as an
// ExternalSQLSource straight over HTTP (the import test's setup — the
// settings page itself gets its browser coverage in
// TestSQLImportSourceSettings_RealBrowser) and returns the record id.
// The password is whatever TEST_DATABASE_URL carries: empty on a local
// trust-auth Postgres, real against CI's service container.
func registerTestPGSource(t *testing.T, srv *httptest.Server, tenantID, name string, tenantDB *sql.DB) string {
	t.Helper()
	host, port, user, pass, database, options := extSQLPGParams(t, tenantDB)
	vals := url.Values{}
	vals.Set("name", name)
	vals.Set("driver", "postgres")
	vals.Set("host", host)
	vals.Set("port", port)
	vals.Set("database", database)
	vals.Set("username", user)
	vals.Set("password", pass)
	vals.Set("options", options)

	req, err := http.NewRequest("POST", srv.URL+"/settings/sql-sources", strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatalf("build register request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Actor-ID", "00000000-0000-0000-0000-0000000000e2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering the source, got %d", resp.StatusCode)
	}
	return storedExtSQLSourceID(t, tenantDB, name)
}

// storedExtSQLSourceID reads one ExternalSQLSource record's id straight
// from the tenant database — the page renders names, not ids, so driving
// the source <select> by value needs to look underneath.
func storedExtSQLSourceID(t *testing.T, tenantDB *sql.DB, name string) string {
	t.Helper()
	var id string
	err := tenantDB.QueryRow(
		`SELECT id FROM records WHERE entity_type = 'ExternalSQLSource' AND data->>'name' = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&id)
	if err != nil {
		t.Fatalf("read stored ExternalSQLSource %q: %v", name, err)
	}
	return id
}

// TestSQLImportSourceSettings_RealBrowser drives the External SQL sources
// settings page (/settings/sql-sources) as a user would: type into the
// real create form (including a password), submit the real non-htmx form
// post, and read the resulting DOM. The load-bearing assertion is the
// secret discipline: the password reaches storage encrypted, and its
// plaintext never appears anywhere in the page source after save — not as
// an echoed <input value>, not in the summary, nowhere.
func TestSQLImportSourceSettings_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServerWithSecretCryptor(t)
	host, port, user, _, database, options := extSQLPGParams(t, tenantDB)
	ctx := browserCtx(t, tenantID)

	const plainPassword = "e2e-plain-password-must-never-render"
	const createForm = `form[action="/settings/sql-sources"]`

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/settings/sql-sources"),
		chromedp.WaitVisible(createForm, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open settings page: %v", err)
	}

	for _, fill := range []struct{ sel, value string }{
		{createForm + ` input[name="name"]`, "Legacy Postgres"},
		{createForm + ` select[name="driver"]`, "postgres"},
		{createForm + ` input[name="host"]`, host},
		{createForm + ` input[name="port"]`, port},
		{createForm + ` input[name="database"]`, database},
		{createForm + ` input[name="username"]`, user},
		{createForm + ` input[name="password"]`, plainPassword},
		{createForm + ` input[name="options"]`, options},
	} {
		if fill.value == "" {
			// A user doesn't type into fields they have no value for, and
			// the inputs start empty — TEST_DATABASE_URL commonly carries
			// no username against a local trust-auth Postgres. (Also found
			// the hard way: chromedp v0.16.0's SetValue fails its own
			// set-then-read-back verification on an empty string — "could
			// not set value on node N" — so an explicit blank fill wouldn't
			// work anyway.)
			continue
		}
		if err := chromedp.Run(ctx, chromedp.SetValue(fill.sel, fill.value, chromedp.ByQuery)); err != nil {
			t.Fatalf("fill %s with %q: %v", fill.sel, fill.value, err)
		}
	}

	var summaryText, pageHTML string
	if err := chromedp.Run(ctx,
		// A real form post: the browser navigates to the response document.
		chromedp.Click(createForm+` button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-extsql-source summary`, chromedp.ByQuery),
		chromedp.Text(`.uc-extsql-source summary`, &summaryText, chromedp.ByQuery),
		chromedp.OuterHTML(`html`, &pageHTML, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit create form: %v", err)
	}

	if !strings.Contains(summaryText, "Legacy Postgres") || !strings.Contains(summaryText, "postgres") {
		t.Fatalf("expected the saved source in the rendered list, got summary: %q", summaryText)
	}
	if !strings.Contains(summaryText, host+"/"+database) {
		t.Fatalf("expected the summary to name %s/%s, got: %q", host, database, summaryText)
	}

	// The secret-discipline proof, against the real rendered document.
	if strings.Contains(pageHTML, plainPassword) {
		t.Fatalf("plaintext password leaked into the page source after save:\n%s", pageHTML)
	}
	if !strings.Contains(pageHTML, "A password is already saved.") {
		t.Fatalf("expected the password-stored hint on the saved source, got:\n%s", pageHTML)
	}

	// The per-source test/delete forms render alongside the edit form.
	id := storedExtSQLSourceID(t, tenantDB, "Legacy Postgres")
	for _, action := range []string{
		`action="/settings/sql-sources/` + id + `/test"`,
		`action="/settings/sql-sources/` + id + `/delete"`,
	} {
		if !strings.Contains(pageHTML, action) {
			t.Fatalf("expected a per-source form with %s, got:\n%s", action, pageHTML)
		}
	}

	// And underneath: what landed in storage is ciphertext, never the
	// plaintext the form carried.
	var encrypted string
	if err := tenantDB.QueryRow(
		`SELECT COALESCE(data->>'password_encrypted', '') FROM records WHERE entity_type = 'ExternalSQLSource' AND data->>'name' = 'Legacy Postgres'`,
	).Scan(&encrypted); err != nil {
		t.Fatalf("read stored password ciphertext: %v", err)
	}
	if encrypted == "" || encrypted == plainPassword {
		t.Fatalf("expected an encrypted stored password, got %q", encrypted)
	}
}

// TestSQLImportWizard_NAVTemplate_RealBrowser drives the SQL-source
// import flow end to end for entity type Item in a real browser: the
// upload page's link into /import/Item/sql, the source picker, the real
// dial-out relation browse, the nav2009 template pre-fill (No_->sku,
// Description->name, plus the read-only item_type=stock constant), a
// re-preview through the real mapping <select>s, the commit, and finally
// the tenant database itself — the Item records must actually exist.
func TestSQLImportWizard_NAVTemplate_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServerWithSecretCryptor(t)

	// A NAV-2009-shaped table in the tenant's own database, which doubles
	// as the registered "external" source — see the file doc comment.
	const navTable = "CRONUS International Ltd_$Item"
	if _, err := tenantDB.Exec(`CREATE TABLE "` + navTable + `" ("No_" text, "Description" text)`); err != nil {
		t.Fatalf("create NAV-shaped table: %v", err)
	}
	if _, err := tenantDB.Exec(`INSERT INTO "` + navTable + `" VALUES ('1000', 'Bicycle'), ('1001', 'Touring Bicycle')`); err != nil {
		t.Fatalf("fill NAV-shaped table: %v", err)
	}

	sourceID := registerTestPGSource(t, srv, tenantID, "Legacy NAV", tenantDB)
	ctx := browserCtx(t, tenantID)

	// Step 1: the upload page links to the SQL flow; follow it and pick
	// the registered source in the real <select>.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/import/Item"),
		chromedp.WaitVisible(`.uc-import-sql-link a[href="/import/Item/sql"]`, chromedp.ByQuery),
		chromedp.Click(`.uc-import-sql-link a`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-extsql-import-form`, chromedp.ByQuery),
		chromedp.SetValue(`#uc-extsql-import-source`, sourceID, chromedp.ByQuery),
		clickAndSettle(`button[hx-post="/import/Item/sql/relations"]`),
	); err != nil {
		t.Fatalf("follow SQL link + browse relations: %v", err)
	}

	// Step 2: the relations list is the source database's real
	// INFORMATION_SCHEMA — the NAV-shaped table must be in it, alongside
	// the tenant's own kernel tables.
	var relationsText string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-extsql-relations`, &relationsText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read relations list: %v", err)
	}
	if !strings.Contains(relationsText, "public."+navTable) {
		t.Fatalf("expected the NAV-shaped table in the discovered relations, got:\n%s", relationsText)
	}

	// Step 3: pick the NAV table. Its Select button lives in the form
	// carrying the relation's hidden input.
	if err := chromedp.Run(ctx,
		clickAndSettle(`#uc-extsql-relations form:has(input[value="`+navTable+`"]) button`),
		chromedp.WaitVisible(`#uc-extsql-preview`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("select NAV relation: %v", err)
	}

	// The mapping form pre-fills from the nav2009 template — real
	// <select> values, not markup substrings.
	var skuValue, nameValue, templateNote, constantsText string
	var constantEditable bool
	if err := chromedp.Run(ctx,
		chromedp.Value(`select[name="mapping.No_"]`, &skuValue, chromedp.ByQuery),
		chromedp.Value(`select[name="mapping.Description"]`, &nameValue, chromedp.ByQuery),
		chromedp.Text(`.uc-extsql-template-note`, &templateNote, chromedp.ByQuery),
		chromedp.Text(`.uc-extsql-constants`, &constantsText, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('#uc-extsql-preview [name="mapping.item_type"], #uc-extsql-preview [name="item_type"]') !== null`, &constantEditable),
	); err != nil {
		t.Fatalf("read pre-filled mapping: %v", err)
	}
	if skuValue != "sku" || nameValue != "name" {
		t.Fatalf("expected the nav2009 template pre-fill (No_->sku, Description->name), got %q, %q", skuValue, nameValue)
	}
	if !strings.Contains(templateNote, "Microsoft Dynamics NAV 2009") {
		t.Fatalf("expected the template note to name the matched vendor template, got: %q", templateNote)
	}
	if !strings.Contains(constantsText, "item_type") || !strings.Contains(constantsText, "stock") || !strings.Contains(constantsText, "fixed value") {
		t.Fatalf("expected the item_type=stock constant rendered with the fixed-value label, got: %q", constantsText)
	}
	if constantEditable {
		t.Fatal("expected the template constant item_type to render read-only, but found an editable control for it")
	}

	// The first preview's rows are already real fetched data.
	var rowsHTML string
	if err := chromedp.Run(ctx, chromedp.InnerHTML(`.uc-import-rows`, &rowsHTML, chromedp.ByQuery)); err != nil {
		t.Fatalf("read preview rows: %v", err)
	}
	if !strings.Contains(rowsHTML, "Bicycle") {
		t.Fatalf("expected the preview to show fetched row data, got:\n%s", rowsHTML)
	}

	// Step 4: re-preview through the mapping form itself — the round trip
	// that resubmits the real mapping.* selects — then commit.
	var resultText string
	if err := chromedp.Run(ctx,
		clickAndSettle(`#uc-extsql-preview button[hx-post="/import/Item/sql/preview"]`),
		chromedp.WaitVisible(`.uc-import-rows`, chromedp.ByQuery),
		clickAndSettle(`button[hx-post="/import/Item/sql/commit"]`),
		chromedp.Text(`.uc-extsql-import-result`, &resultText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("re-preview + commit: %v", err)
	}
	if !strings.Contains(resultText, "2 succeeded") {
		t.Fatalf("expected the commit result to report 2 succeeded, got: %q", resultText)
	}

	// Underneath the rendering: the Item records genuinely exist in the
	// tenant database, mapped columns and template constant applied.
	var count int
	if err := tenantDB.QueryRow(`SELECT count(*) FROM records WHERE entity_type = 'Item' AND deleted_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count imported Items: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 imported Item records, got %d", count)
	}
	for sku, name := range map[string]string{"1000": "Bicycle", "1001": "Touring Bicycle"} {
		var gotName, gotType string
		if err := tenantDB.QueryRow(
			`SELECT data->>'name', data->>'item_type' FROM records WHERE entity_type = 'Item' AND data->>'sku' = $1 AND deleted_at IS NULL`,
			sku,
		).Scan(&gotName, &gotType); err != nil {
			t.Fatalf("read imported Item %s: %v", sku, err)
		}
		if gotName != name || gotType != "stock" {
			t.Fatalf("Item %s: expected name %q and item_type \"stock\", got %q, %q", sku, name, gotName, gotType)
		}
	}
}
