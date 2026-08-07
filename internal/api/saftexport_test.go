package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/saft"
)

// setupSAFTTenant publishes the foundation + finance modules (the SAF-T
// export's entity-type surface) into an already-provisioned tenant, then
// seeds one customer and one supplier Party, a TaxCode, and a small
// posted ledger: two accounts and one balanced 2026-03-15 journal entry
// of 125.50.
func setupSAFTTenant(t *testing.T, tenantID string, db *sql.DB, mux *http.ServeMux) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := finance.Publish(ctx, db, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}
	if err := finance.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("finance.PublishForms: %v", err)
	}

	post := func(path string, body string) map[string]any {
		t.Helper()
		req := newRequest("POST", path, tenantID, "farshid", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return envelope.Data
	}

	customer := post("/api/records/Party", `{"party_type":"organization","name":"Customer GmbH","tax_id":"C-111"}`)
	supplier := post("/api/records/Party", `{"party_type":"organization","name":"Supplier LLC"}`)
	post("/api/records/PartyRole", `{"party_id":"`+customer["id"].(string)+`","role_type":"customer"}`)
	post("/api/records/PartyRole", `{"party_id":"`+supplier["id"].(string)+`","role_type":"vendor"}`)
	post("/api/records/TaxCode", `{"code":"VAT5","name":"VAT 5%","rate":5,"tax_type":"vat","jurisdiction":"NO"}`)

	// Ledger: dedicated typed tables, seeded via the repos (the same
	// write path finance.SyncGLAccounts/ledger.Post own in production).
	ctx = context.Background()
	accounts := data.NewGLAccountRepo(db)
	bankID, err := accounts.UpsertByCode(ctx, "1100", "Bank", "asset", "USD", true)
	if err != nil {
		t.Fatalf("upsert bank: %v", err)
	}
	revID, err := accounts.UpsertByCode(ctx, "3000", "Revenue", "income", "USD", true)
	if err != nil {
		t.Fatalf("upsert revenue: %v", err)
	}
	// Two entries, not one: a single-entry fixture is the exact shape
	// that masked the eager-load aliasing bug (#28's independent
	// review) — a multi-entry range is the representative case.
	for _, e := range []struct {
		date, desc string
		amt        int64
	}{
		{"2026-03-15", "Invoice INV-1", 125_50},
		{"2026-05-20", "Invoice INV-2", 74_50},
	} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := data.NewJournalEntryRepo(db).CreateTx(ctx, tx, e.date, e.desc, "CustomerInvoice", "",
			[]string{bankID, revID}, []int64{e.amt, 0}, []int64{0, e.amt}); err != nil {
			t.Fatalf("create journal entry %s: %v", e.date, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", e.date, err)
		}
	}
}

// validateSAFTAgainstXSD checks payload against the vendored Skatteetaten
// 1.30 schema (internal/kernel/saft/testdata) via xmllint — the same
// validation the saft package's own unit tests run, applied here to the
// HANDLER's real output: real UUIDs, real tenant names and seeded
// records exercise value shapes a hand-written serializer fixture never
// does. Skips without xmllint locally; CI installs libxml2-utils.
func validateSAFTAgainstXSD(t *testing.T, payload []byte) {
	t.Helper()
	if _, err := exec.LookPath("xmllint"); err != nil {
		t.Skip("xmllint not installed; XSD validation runs in CI")
	}
	path := filepath.Join(t.TempDir(), "audit.xml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write temp audit file: %v", err)
	}
	out, err := exec.Command("xmllint", "--noout",
		"--schema", "../kernel/saft/testdata/Norwegian_SAF-T_Financial_Schema_v_1.30.xsd", path).CombinedOutput()
	if err != nil {
		t.Fatalf("handler output does not validate against the SAF-T 1.30 XSD: %v\n%s\n---\n%s", err, out, payload)
	}
}

func TestSAFTExport_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

func TestSAFTExport_RejectsBadDates(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, _ := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	for _, q := range []string{
		"from=2026-01-01",               // missing to
		"from=01/01/2026&to=2026-12-31", // wrong format
		"from=2026-12-31&to=2026-01-01", // inverted
		"from=2026-13-40&to=2026-12-31", // impossible date
	} {
		req := newRequest("GET", "/export/saft?"+q, tenantID, "farshid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: expected 400, got %d: %s", q, rec.Code, rec.Body.String())
		}
	}
}

// TestSAFTExport_ProducesValidFileAndAuditRow is the core proof: real
// tenant, real published modules, real ledger rows → one XML document
// that parses back into the saft document model with the right master
// data, totals, and a recorded export audit row.
func TestSAFTExport_ProducesValidFileAndAuditRow(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("expected application/xml, got %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="saft_2026-01-01_2026-12-31.xml"` {
		t.Fatalf("unexpected Content-Disposition %q", cd)
	}

	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	if file.Header.DefaultCurrencyCode != "USD" {
		t.Errorf("DefaultCurrencyCode = %q, want USD (single-currency chart)", file.Header.DefaultCurrencyCode)
	}
	if file.MasterFiles == nil {
		t.Fatal("expected MasterFiles")
	}
	if n := len(file.MasterFiles.GeneralLedgerAccounts.Accounts); n != 2 {
		t.Errorf("expected 2 accounts, got %d", n)
	}
	if n := len(file.MasterFiles.Customers.Customers); n != 1 || file.MasterFiles.Customers.Customers[0].Name != "Customer GmbH" {
		t.Errorf("customers wrong: %+v", file.MasterFiles.Customers)
	}
	if file.MasterFiles.Customers.Customers[0].RegistrationNumber != "C-111" {
		t.Errorf("customer tax_id should map to RegistrationNumber, got %+v", file.MasterFiles.Customers.Customers[0])
	}
	if n := len(file.MasterFiles.Suppliers.Suppliers); n != 1 || file.MasterFiles.Suppliers.Suppliers[0].Name != "Supplier LLC" {
		t.Errorf("suppliers wrong: %+v", file.MasterFiles.Suppliers)
	}
	if file.MasterFiles.TaxTable == nil || len(file.MasterFiles.TaxTable.Entries[0].TaxCodeDetails) != 1 {
		t.Errorf("tax table wrong: %+v", file.MasterFiles.TaxTable)
	}
	gle := file.GeneralLedgerEntries
	// 125.50 + 74.50 across two transactions: the totals must sum BOTH
	// entries' lines, and every transaction must keep its own two lines
	// — the aliasing-bug regression assertions at the HTTP layer.
	if gle.NumberOfEntries != 2 || gle.TotalDebit != "200.00" || gle.TotalCredit != "200.00" {
		t.Errorf("GL totals wrong: %+v", gle)
	}
	if len(gle.Journals) != 1 || len(gle.Journals[0].Transactions) != 2 {
		t.Fatalf("expected 1 journal with 2 transactions, got %+v", gle.Journals)
	}
	for i, tx := range gle.Journals[0].Transactions {
		if len(tx.Lines) != 2 {
			t.Errorf("transaction %d (%s) lost its lines: got %d, want 2", i, tx.TransactionDate, len(tx.Lines))
		}
		if tx.VoucherType != "CustomerInvoice" {
			t.Errorf("transaction %d should carry its source as VoucherType, got %q", i, tx.VoucherType)
		}
	}

	// The real handler output — real UUIDs, real tenant name, real
	// seeded data, not a hand-written fixture — must itself validate
	// against the vendored Skatteetaten XSD. Skips without xmllint
	// locally; CI installs libxml2-utils so it always runs there.
	validateSAFTAgainstXSD(t, rec.Body.Bytes())

	// The export itself must be on the audit trail (ADR-0001 §14), with
	// the dev-auth human actor attributed.
	var action, actorType, actorID string
	var diff []byte
	if err := db.QueryRow(`SELECT action, actor_type, actor_id, diff FROM audit_log WHERE entity_type = 'SAFTExport'`).
		Scan(&action, &actorType, &actorID, &diff); err != nil {
		t.Fatalf("expected exactly one SAFTExport audit row: %v", err)
	}
	if action != "export" || actorType != "human" || actorID != "farshid" {
		t.Errorf("audit row wrong: action=%q actor=%s/%s", action, actorType, actorID)
	}
	var d map[string]any
	if err := json.Unmarshal(diff, &d); err != nil || d["from"] != "2026-01-01" || d["entries"] != float64(2) {
		t.Errorf("audit diff wrong: %s (err %v)", diff, err)
	}
}

// TestSAFTExport_EmptyTenantStillValidFile: a tenant with the modules
// published but no data at all still gets a well-formed file — the
// zero-entries shape the XSD explicitly allows.
func TestSAFTExport_EmptyTenantStillValidFile(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := finance.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if file.GeneralLedgerEntries.NumberOfEntries != 0 {
		t.Errorf("expected zero entries, got %d", file.GeneralLedgerEntries.NumberOfEntries)
	}
	validateSAFTAgainstXSD(t, rec.Body.Bytes())
}

// TestSAFTExport_DefaultCurrencyUsesTenantBaseCurrency is uc-infra#120's
// acceptance case for the export: with no ledger data at all (so
// glRepo.DistinctCurrencies returns zero currencies and the handler falls
// through to its configured-base-currency fallback, same shape as
// TestSAFTExport_EmptyTenantStillValidFile above), a tenant that has
// marked a Currency is_base=true gets that currency's code as the file's
// DefaultCurrencyCode instead of the hardcoded "USD".
func TestSAFTExport_DefaultCurrencyUsesTenantBaseCurrency(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := finance.Publish(ctx, db, actor); err != nil {
		t.Fatalf("finance.Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(db)
	currencyDefRaw, err := entityDefs.GetPublished(ctx, "Currency")
	if err != nil {
		t.Fatalf("GetPublished(Currency): %v", err)
	}
	currencyDef, err := entity.Unmarshal(currencyDefRaw.Definition)
	if err != nil {
		t.Fatalf("unmarshal Currency: %v", err)
	}
	if _, err := crud.NewEngine(db).Create(ctx, currencyDef, map[string]any{
		"code": "QAR", "name": "Qatari Riyal", "is_base": true,
	}, actor); err != nil {
		t.Fatalf("create base Currency: %v", err)
	}

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if file.Header.DefaultCurrencyCode != "QAR" {
		t.Fatalf("expected the tenant's base currency %q, got %q", "QAR", file.Header.DefaultCurrencyCode)
	}
	validateSAFTAgainstXSD(t, rec.Body.Bytes())
}

// TestSAFTExport_OwnOrganizationPopulatesCompanyProfile is uc-infra#63's
// core acceptance case: a Party holding the own_organization PartyRole,
// with its statutory fields set, flows through to the file's Company/
// Contact block instead of the spec's NA markers.
func TestSAFTExport_OwnOrganizationPopulatesCompanyProfile(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	post := func(path string, body string) map[string]any {
		t.Helper()
		req := newRequest("POST", path, tenantID, "farshid", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return envelope.Data
	}

	org := post("/api/records/Party", `{"party_type":"organization","name":"Demo Organization",`+
		`"tax_id":"TAX-98765","registration_number":"REG-12345",`+
		`"contact_first_name":"Jane","contact_last_name":"Doe"}`)
	post("/api/records/PartyRole", `{"party_id":"`+org["id"].(string)+`","role_type":"own_organization"}`)

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	c := file.Header.Company
	if c.RegistrationNumber != "REG-12345" {
		t.Errorf("RegistrationNumber = %q, want REG-12345 (not NA)", c.RegistrationNumber)
	}
	if len(c.TaxRegistration) != 1 || c.TaxRegistration[0].TaxRegistrationNumber != "TAX-98765" {
		t.Errorf("TaxRegistration = %+v, want [{TAX-98765}]", c.TaxRegistration)
	}
	if len(c.Contact) != 1 || c.Contact[0].ContactPerson.FirstName != "Jane" || c.Contact[0].ContactPerson.LastName != "Doe" {
		t.Errorf("Contact = %+v, want Jane/Doe", c.Contact)
	}
	validateSAFTAgainstXSD(t, rec.Body.Bytes())
}

// TestSAFTExport_NoOwnOrganizationFallsBackToNA is the regression case
// for every tenant that existed before uc-infra#63: no Party holds the
// own_organization role, so the file keeps the exact pre-#63 NA-marker
// behavior — setupSAFTTenant's fixture never creates one.
func TestSAFTExport_NoOwnOrganizationFallsBackToNA(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	c := file.Header.Company
	if c.RegistrationNumber != "NA" {
		t.Errorf("RegistrationNumber = %q, want NA (no own_organization Party configured)", c.RegistrationNumber)
	}
	if len(c.Contact) != 1 || c.Contact[0].ContactPerson.FirstName != "NA" || c.Contact[0].ContactPerson.LastName != "NA" {
		t.Errorf("Contact = %+v, want NA/NA", c.Contact)
	}
}

// TestSAFTExport_AmbiguousOwnOrganizationFallsBackToNA: two Parties both
// holding own_organization is a data-quality problem the export has no
// business guessing at (see foundation.PartyRole's own doc comment) — it
// must degrade to NA exactly like the zero-match case, never pick one.
func TestSAFTExport_AmbiguousOwnOrganizationFallsBackToNA(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	post := func(path string, body string) map[string]any {
		t.Helper()
		req := newRequest("POST", path, tenantID, "farshid", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return envelope.Data
	}

	for i, name := range []string{"Org One", "Org Two"} {
		org := post("/api/records/Party", `{"party_type":"organization","name":"`+name+`","registration_number":"REG-`+string(rune('A'+i))+`"}`)
		post("/api/records/PartyRole", `{"party_id":"`+org["id"].(string)+`","role_type":"own_organization"}`)
	}

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	if got := file.Header.Company.RegistrationNumber; got != "NA" {
		t.Errorf("RegistrationNumber = %q, want NA (ambiguous: two own_organization Parties)", got)
	}
}

// TestSAFTExport_OwnOrganizationPartyDeletedAfterRoleFallsBackToNA: the
// PartyRole row survives a Party delete (no cascade in this generic
// storage layer), so saftCompanyProfile's Get against the dangling
// party_id must degrade to NA like the zero-match case, not 500.
func TestSAFTExport_OwnOrganizationPartyDeletedAfterRoleFallsBackToNA(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	post := func(path string, body string) map[string]any {
		t.Helper()
		req := newRequest("POST", path, tenantID, "farshid", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return envelope.Data
	}

	org := post("/api/records/Party", `{"party_type":"organization","name":"Demo Organization","registration_number":"REG-12345"}`)
	post("/api/records/PartyRole", `{"party_id":"`+org["id"].(string)+`","role_type":"own_organization"}`)

	if err := data.NewRecordRepo(db).Delete(context.Background(), "Party", org["id"].(string)); err != nil {
		t.Fatalf("delete Party: %v", err)
	}

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (dangling role should degrade, not error), got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	if got := file.Header.Company.RegistrationNumber; got != "NA" {
		t.Errorf("RegistrationNumber = %q, want NA (own_organization role points at a deleted Party)", got)
	}
}

// TestSAFTExport_DuplicateOwnOrganizationRolesOnOneParty (independent
// review, uc-infra#63): nothing in the generic entity/crud layer makes
// PartyRole rows unique, so assigning the own_organization role twice to
// the SAME Party is a click away. Two rows naming one party is not the
// ambiguity the degrade-to-NA rule exists for — every row agrees on which
// Party is the tenant, so resolving it is not a guess. Counting ROWS
// rather than distinct parties would quietly swap a configured company
// profile back to NA (a statutory misstatement with no visible cause);
// saftParties already dedupes its own role→party join by party id for
// exactly this reason.
func TestSAFTExport_DuplicateOwnOrganizationRolesOnOneParty(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	post := func(path string, body string) map[string]any {
		t.Helper()
		req := newRequest("POST", path, tenantID, "farshid", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return envelope.Data
	}

	org := post("/api/records/Party", `{"party_type":"organization","name":"Demo Organization",`+
		`"registration_number":"REG-12345","contact_first_name":"Jane","contact_last_name":"Doe"}`)
	post("/api/records/PartyRole", `{"party_id":"`+org["id"].(string)+`","role_type":"own_organization"}`)
	post("/api/records/PartyRole", `{"party_id":"`+org["id"].(string)+`","role_type":"own_organization"}`)

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var file saft.AuditFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
		t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
	}
	c := file.Header.Company
	if c.RegistrationNumber != "REG-12345" {
		t.Errorf("RegistrationNumber = %q, want REG-12345 (two roles naming one Party is unambiguous)", c.RegistrationNumber)
	}
	if len(c.Contact) != 1 || c.Contact[0].ContactPerson.FirstName != "Jane" || c.Contact[0].ContactPerson.LastName != "Doe" {
		t.Errorf("Contact = %+v, want Jane/Doe", c.Contact)
	}
}

// TestSAFTExport_OwnOrganizationRoleWithUnresolvablePartyIDFallsBackToNA
// (independent review, uc-infra#63): PartyRole.party_id carries no
// TargetFilter/MustMatchParentField, so crud's own reference-target check
// skips it entirely (checkReferenceTargetConstraints returns early) and
// entity.ValidateRecord only asserts the value's shape, not that it
// resolves — a role row whose party_id is not UUID-shaped is therefore
// creatable through the ordinary API by any actor with PartyRole write
// access.
//
// The blank case is no longer creatable that way: uc-infra#86 made
// Required reject an empty string over the JSON API. It is still
// exercised here against a directly-seeded row, because that guard is
// write-path only and rows predating it still exist — see the subtest's
// own comment.
// records.id is a uuid column, so a Get on such a value returns a raw
// Postgres "invalid input syntax for type uuid" driver error rather than
// data.ErrNotFound: without the shape guard, ONE junk role row 500s the
// whole statutory export for the entire tenant. Must degrade to NA, the
// same tolerated-dangling-reference posture crud.looksLikeUUID's own doc
// comment and saftParties' missing-party skip already take.
func TestSAFTExport_OwnOrganizationRoleWithUnresolvablePartyIDFallsBackToNA(t *testing.T) {
	for _, partyID := range []string{"", "not-a-uuid"} {
		t.Run("party_id="+partyID, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)
			setupSAFTTenant(t, tenantID, db, mux)

			if partyID == "" {
				// The write-path guard this test's original comment
				// anticipated now EXISTS: uc-infra#86 made Required reject
				// an empty string over the JSON API, so this row is no
				// longer creatable through POST /api/records/PartyRole
				// (it 400s with "Party is required." — the better
				// outcome that comment asked for). Retiring the API half
				// of this case deliberately, as instructed, rather than
				// letting it pass silently.
				//
				// The EXPORT half is not retired, because the data shape
				// did not go away: #86 guards the write path only, and
				// nothing migrated the rows written before it landed. A
				// tenant provisioned earlier can still hold a blank
				// party_id today, and one such row must still not 500 the
				// whole statutory export. Seeded through the repository
				// directly (no validation layer) precisely because that
				// is how such a row got there — it predates the guard.
				if _, err := data.NewRecordRepo(db).Create(context.Background(), "PartyRole", map[string]any{
					"party_id": "", "role_type": "own_organization",
				}); err != nil {
					t.Fatalf("seed a legacy blank-party_id PartyRole directly: %v", err)
				}
			} else {
				req := newRequest("POST", "/api/records/PartyRole", tenantID, "farshid",
					[]byte(`{"party_id":"`+partyID+`","role_type":"own_organization"}`))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated {
					// Still not a skip: a non-UUID-but-non-empty party_id
					// remains creatable through the ordinary API (#86
					// rejects "" only), so if THIS shape ever starts being
					// rejected, retire it deliberately the same way the
					// empty case above was.
					t.Fatalf("POST PartyRole with party_id=%q: expected 201 (no write-path guard rejects this shape today), got %d: %s",
						partyID, rec.Code, rec.Body.String())
				}
			}

			req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 (unresolvable party_id should degrade, not error), got %d: %s", rec.Code, rec.Body.String())
			}
			var file saft.AuditFile
			if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
				t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
			}
			if got := file.Header.Company.RegistrationNumber; got != "NA" {
				t.Errorf("RegistrationNumber = %q, want NA (own_organization role points at an unresolvable party_id)", got)
			}
		})
	}
}

// TestSAFTExport_OwnOrganizationRoleWithNonCanonicalPartyIDStillResolves
// is the other half of the shape guard above, and the exact regression
// uc-infra#107's review caught in ITS first-draft fix: Postgres's uuid_in
// accepts brace-wrapped and unhyphenated spellings of the same id, so a
// guard built on a strict canonical-form regex would reject a party_id
// that really does resolve — silently turning a configured company
// profile into NA. ids.Canonical accepts what uuid_in accepts, so both
// spellings must still find the Party.
func TestSAFTExport_OwnOrganizationRoleWithNonCanonicalPartyIDStillResolves(t *testing.T) {
	for _, spelling := range []struct{ name, format string }{
		{"brace-wrapped", "{%s}"},
		{"unhyphenated uppercase", "%s"},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)
			setupSAFTTenant(t, tenantID, db, mux)

			post := func(path string, body string) map[string]any {
				t.Helper()
				req := newRequest("POST", path, tenantID, "farshid", []byte(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("POST %s: expected 201, got %d: %s", path, rec.Code, rec.Body.String())
				}
				var envelope struct {
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("decode %s response: %v", path, err)
				}
				return envelope.Data
			}

			org := post("/api/records/Party", `{"party_type":"organization","name":"Demo Organization","registration_number":"REG-12345"}`)
			id := org["id"].(string)
			if spelling.name == "unhyphenated uppercase" {
				id = strings.ToUpper(strings.ReplaceAll(id, "-", ""))
			}
			post("/api/records/PartyRole", `{"party_id":"`+fmt.Sprintf(spelling.format, id)+`","role_type":"own_organization"}`)

			req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "farshid", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var file saft.AuditFile
			if err := xml.Unmarshal(rec.Body.Bytes(), &file); err != nil {
				t.Fatalf("response is not parseable SAF-T XML: %v\n%s", err, rec.Body.String())
			}
			if got := file.Header.Company.RegistrationNumber; got != "REG-12345" {
				t.Errorf("RegistrationNumber = %q, want REG-12345 (a %s party_id is the same id to Postgres)", got, spelling.name)
			}
		})
	}
}

// TestSAFTExport_RBACDenied: once any Permission row exists for an
// entity type the file discloses, an actor without that grant gets a
// 403 — the whole-file gate, mirroring the purchasing report's.
func TestSAFTExport_RBACDenied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	// Close "Account" to a role the requesting clerk does not hold.
	seedRBAC(t, db,
		map[string][]string{"accountant": {"user-accountant"}},
		[]map[string]any{{"role": "accountant", "entity_type": "Account", "can_read": true}})

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor without Account read, got %d: %s", rec.Code, rec.Body.String())
	}
	// The form page runs the same gate (denyPageUnless → localized 403
	// page) — a denied actor must not see the form either.
	formReq := newRequest("GET", "/export/saft/form", tenantID, "user-clerk", nil)
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if formRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 from the form page for a denied actor, got %d: %s", formRec.Code, formRec.Body.String())
	}
	// The granted actor still gets the file.
	req = newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "user-accountant", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for the granted actor, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSAFTExport_FieldRedactionRefused403: read permission alone is not
// enough — a field-level hide on any field ts.crud reads for this file
// must refuse the whole export, the same hole #27's independent review
// found on the UBL export (TestUBLExport_FieldRedactionRefused403 is the
// shape this mirrors) and uc-infra#67 reported for SAF-T specifically.
// Covers every saftFieldReads entry, not just the ones that render as
// ""/0: PartyRole.party_id/role_type are the case an independent review
// pass on this fix's own first draft found missing — hiding either does
// not render blank, it makes saftParties' role→party join or
// customer/vendor switch fail silently, so the file would ship with
// EMPTY Customers/Suppliers master files (a false "no trading partners"
// statutory claim) instead of refusing. Party.registration_number/
// contact_first_name/contact_last_name (uc-infra#63) are the newest
// three: without a case here, a hidden statutory field would silently
// degrade to the file's own "NA" marker instead of refusing, indistinguishable
// from "no company profile configured" — exactly the kind of false-negative
// this whole test exists to catch. Every case must be reachable through
// the same clerk role with every OTHER field left visible, or the 403
// could be coming from something else entirely — the loop drops the
// case's own field from a fully-open grant to keep it isolated.
func TestSAFTExport_FieldRedactionRefused403(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entityType string
		field      string
	}{
		{"Party.name hidden", "Party", "name"},
		{"Party.tax_id hidden", "Party", "tax_id"},
		{"Party.registration_number hidden", "Party", "registration_number"},
		{"Party.contact_first_name hidden", "Party", "contact_first_name"},
		{"Party.contact_last_name hidden", "Party", "contact_last_name"},
		{"PartyRole.party_id hidden", "PartyRole", "party_id"},
		{"PartyRole.role_type hidden", "PartyRole", "role_type"},
		{"TaxCode.code hidden", "TaxCode", "code"},
		{"TaxCode.name hidden", "TaxCode", "name"},
		{"TaxCode.jurisdiction hidden", "TaxCode", "jurisdiction"},
		{"TaxCode.rate hidden", "TaxCode", "rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)
			setupSAFTTenant(t, tenantID, db, mux)

			roleIDs := seedRBAC(t, db,
				map[string][]string{"clerk": {"user-clerk"}},
				[]map[string]any{
					{"role": "clerk", "entity_type": "Account", "can_read": true},
					{"role": "clerk", "entity_type": "TaxCode", "can_read": true},
					{"role": "clerk", "entity_type": "Party", "can_read": true},
					{"role": "clerk", "entity_type": "PartyRole", "can_read": true},
				},
			)
			ctx := context.Background()
			engine := crud.NewEngine(db)
			actor := humanActor()
			if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
				"role_id": roleIDs["clerk"], "entity_type": tc.entityType, "field_name": tc.field, "hidden": true,
			}, actor); err != nil {
				t.Fatalf("create FieldPermission: %v", err)
			}

			req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "user-clerk", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for redacted %s.%s, got %d: %s", tc.entityType, tc.field, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.entityType+"."+tc.field) {
				t.Errorf("403 body should name the redacted field: %s", rec.Body.String())
			}

			// The form page runs the same gate (denyPageUnless → localized
			// 403 page) — a redacted actor must not see the form either,
			// same reasoning TestSAFTExport_RBACDenied already applies to
			// the entity-level case. Asserting the localized denial markup
			// (not just the status code) rules out a coincidental 403 from
			// something else on the page.
			formReq := newRequest("GET", "/export/saft/form", tenantID, "user-clerk", nil)
			formRec := httptest.NewRecorder()
			mux.ServeHTTP(formRec, formReq)
			if formRec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 from the form page for redacted %s.%s, got %d: %s", tc.entityType, tc.field, formRec.Code, formRec.Body.String())
			}
			if !strings.Contains(formRec.Body.String(), "uc-denied") {
				t.Errorf("form page 403 should be the rendered access-denied page for redacted %s.%s: %s", tc.entityType, tc.field, formRec.Body.String())
			}
		})
	}
}

// TestSAFTExport_NoFieldPermissionRowsStillSucceeds is the positive
// control for TestSAFTExport_FieldRedactionRefused403: the same clerk
// role, granted the same entity-level reads, but with no FieldPermission
// row at all (a tenant that has never touched field-level RBAC) must
// still get its file — proving the gate above refuses ONLY on an actual
// hide, not on the mere presence of the saftFieldReads/HiddenFields
// machinery. TestSAFTExport_RBACDenied already proves this shape for
// unrelated entity-level denials; this proves it for the field-level
// gate specifically.
func TestSAFTExport_NoFieldPermissionRowsStillSucceeds(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Account", "can_read": true},
			{"role": "clerk", "entity_type": "TaxCode", "can_read": true},
			{"role": "clerk", "entity_type": "Party", "can_read": true},
			{"role": "clerk", "entity_type": "PartyRole", "can_read": true},
		},
	)

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with entity-level read granted and no field hides, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSAFTExport_ModuleMenuHidesLinkWhenFieldRedacted is the module-menu
// analog of TestSAFTExport_FieldRedactionRefused403, mirroring
// TestAPI_PurchasingReport_ModuleMenuHidesReportLinkWhenDenied's shape
// for the field-level case: an independent review of this fix's own
// first draft found modulemenu.go's Finance link still offered the
// SAF-T link unconditionally to a field-redacted actor — the "tile that
// leads to a 403" outcome moduleReportLinks' RequiredRead exists to
// prevent, just one gate deeper (field-level, not just entity-level)
// than RequiredRead alone expresses. moduleReportLinks.ExtraDenied
// closes that gap.
func TestSAFTExport_ModuleMenuHidesLinkWhenFieldRedacted(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantID, db, mux)

	roleIDs := seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Account", "can_read": true},
			{"role": "clerk", "entity_type": "TaxCode", "can_read": true},
			{"role": "clerk", "entity_type": "Party", "can_read": true},
			{"role": "clerk", "entity_type": "PartyRole", "can_read": true},
		},
	)
	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()
	if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
		"role_id": roleIDs["clerk"], "entity_type": "Party", "field_name": "name", "hidden": true,
	}, actor); err != nil {
		t.Fatalf("create FieldPermission: %v", err)
	}

	rec := getAs(t, mux, "/modules/finance", tenantID, "user-clerk")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for the module menu itself (only a field, not an entity, is redacted), got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/export/saft/form") {
		t.Fatalf("field-redacted actor still saw the SAF-T link, which leads to a guaranteed 403:\n%s", rec.Body.String())
	}
}

// TestSAFTExport_TenantIsolation: tenant B's file never carries tenant
// A's ledger — two genuinely separate databases (ADR-0003), same proof
// shape as the CSV export's isolation test.
func TestSAFTExport_TenantIsolation(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantA, dbA := newTestTenant(t, router)
	tenantB, dbB := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	setupSAFTTenant(t, tenantA, dbA, mux)
	ctx := context.Background()
	if err := foundation.Publish(ctx, dbB, humanActor()); err != nil {
		t.Fatalf("foundation.Publish B: %v", err)
	}
	if err := finance.Publish(ctx, dbB, humanActor()); err != nil {
		t.Fatalf("finance.Publish B: %v", err)
	}

	req := newRequest("GET", "/export/saft?from=2026-01-01&to=2026-12-31", tenantB, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{"Customer GmbH", "Supplier LLC", "Invoice INV-1", "1100"} {
		if strings.Contains(body, leaked) {
			t.Errorf("tenant B's export leaked tenant A's %q:\n%s", leaked, body)
		}
	}
}

func TestSAFTExportForm_RendersDateInputs(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}

	req := newRequest("GET", "/export/saft/form", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/export/saft"`, `name="from"`, `name="to"`, `type="date"`, "SAF-T"} {
		if !strings.Contains(body, want) {
			t.Errorf("form page missing %q", want)
		}
	}
}
