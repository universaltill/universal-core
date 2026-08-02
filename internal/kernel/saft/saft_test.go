package saft

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fullInput is a representative Input exercising every section Build
// generates: accounts on both balance sides, customers/suppliers
// (including a party holding both roles upstream — already split by the
// caller), tax codes with and without rates plus a duplicate code, and
// balanced journal entries.
func fullInput() Input {
	return Input{
		Created:               "2026-07-31",
		From:                  "2026-01-01",
		To:                    "2026-12-31",
		SoftwareVersion:       "v0.9.0",
		CompanyName:           "Demo Organization",
		RegistrationNumber:    "",
		TaxRegistrationNumber: "999999999",
		DefaultCurrency:       "USD",
		Accounts: []Account{
			{Code: "1100", Name: "Bank", Type: "asset", OpeningMinor: 500_00, ClosingMinor: 350_00},
			{Code: "2000", Name: "Accounts payable", Type: "liability", OpeningMinor: -200_00, ClosingMinor: -50_00},
			{Code: "3000", Name: "Sales revenue", Type: "income", OpeningMinor: 0, ClosingMinor: -300_00},
			{Code: "4000", Name: "Cost of goods", Type: "expense", OpeningMinor: 0, ClosingMinor: 0},
		},
		Customers: []Party{
			{ID: "6b3a4e6e-3e0f-4a3e-9a4e-000000000001", Name: "Customer One", RegistrationNumber: "C-123"},
		},
		Suppliers: []Party{
			{ID: "6b3a4e6e-3e0f-4a3e-9a4e-000000000002", Name: "Supplier One"},
		},
		TaxCodes: []TaxCode{
			{Code: "VAT-STD", Name: "Standard VAT", Rate: 25, HasRate: true},
			{Code: "VAT-0", Name: "Zero-rated"},
			{Code: "VAT-STD", Name: "Duplicate to drop", Rate: 10, HasRate: true},
		},
		Entries: []Entry{
			{
				ID:          "2b3a4e6e-3e0f-4a3e-9a4e-00000000000a",
				EntryDate:   "2026-03-15",
				PostedDate:  "2026-03-15",
				Description: "Invoice INV-1",
				SourceType:  "CustomerInvoice",
				Lines: []Line{
					{AccountCode: "1100", DebitMinor: 150_00},
					{AccountCode: "3000", CreditMinor: 150_00},
				},
			},
			{
				ID:          "2b3a4e6e-3e0f-4a3e-9a4e-00000000000b",
				EntryDate:   "2026-04-01",
				PostedDate:  "2026-04-02",
				Description: "",
				Lines: []Line{
					{AccountCode: "4000", DebitMinor: 75_50},
					{AccountCode: "2000", CreditMinor: 75_50},
				},
			},
		},
	}
}

// validateAgainstXSD runs xmllint against the vendored Norwegian 1.30
// schema. Skips when xmllint isn't installed locally; CI installs
// libxml2-utils so the validation always runs there.
func validateAgainstXSD(t *testing.T, payload []byte) {
	t.Helper()
	if _, err := exec.LookPath("xmllint"); err != nil {
		t.Skip("xmllint not installed; XSD validation runs in CI")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.xml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write temp audit file: %v", err)
	}
	out, err := exec.Command("xmllint", "--noout", "--schema", "testdata/Norwegian_SAF-T_Financial_Schema_v_1.30.xsd", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generated file does not validate against the SAF-T 1.30 XSD: %v\n%s\n---\n%s", err, out, payload)
	}
}

func TestBuild_FullFileValidatesAgainstXSD(t *testing.T) {
	f, err := Build(fullInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	payload, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	validateAgainstXSD(t, payload)
}

func TestBuild_EmptyLedgerValidatesAgainstXSD(t *testing.T) {
	in := Input{
		Created:         "2026-07-31",
		From:            "2026-01-01",
		To:              "2026-12-31",
		DefaultCurrency: "USD",
	}
	f, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if f.MasterFiles != nil {
		t.Fatalf("empty input should omit MasterFiles entirely, got %+v", f.MasterFiles)
	}
	if f.GeneralLedgerEntries.NumberOfEntries != 0 || f.GeneralLedgerEntries.TotalDebit != "0.00" || f.GeneralLedgerEntries.TotalCredit != "0.00" {
		t.Fatalf("empty ledger totals wrong: %+v", f.GeneralLedgerEntries)
	}
	payload, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	validateAgainstXSD(t, payload)
}

// TestBuild_CompanyContactFallback covers uc-infra#63's Company/Contact
// wiring: RegistrationNumber, TaxRegistrationNumber, and the Contact
// person's FirstName/LastName each fall back to the spec's "NA" marker
// independently when blank, and pass through untouched when set — never
// an all-or-nothing block.
func TestBuild_CompanyContactFallback(t *testing.T) {
	base := func() Input {
		return Input{Created: "2026-07-31", From: "2026-01-01", To: "2026-12-31", DefaultCurrency: "USD"}
	}

	t.Run("everything blank falls back to NA, byte-identical to before #63", func(t *testing.T) {
		f, err := Build(base())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		c := f.Header.Company
		if c.RegistrationNumber != "NA" || c.Name != "NA" {
			t.Errorf("RegistrationNumber/Name = %q/%q, want NA/NA", c.RegistrationNumber, c.Name)
		}
		if len(c.TaxRegistration) != 0 {
			t.Errorf("TaxRegistration = %+v, want omitted when blank", c.TaxRegistration)
		}
		if len(c.Contact) != 1 || c.Contact[0].ContactPerson.FirstName != "NA" || c.Contact[0].ContactPerson.LastName != "NA" {
			t.Errorf("Contact = %+v, want a single NA/NA ContactPerson", c.Contact)
		}
	})

	t.Run("contact first name set, last name blank: per-field fallback", func(t *testing.T) {
		in := base()
		in.ContactFirstName = "Jane"
		f, err := Build(in)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got := f.Header.Company.Contact[0].ContactPerson
		if got.FirstName != "Jane" || got.LastName != "NA" {
			t.Errorf("ContactPerson = %+v, want FirstName=Jane LastName=NA", got)
		}
	})

	t.Run("full statutory identity set: all real values, no NA", func(t *testing.T) {
		in := base()
		in.RegistrationNumber = "REG-12345"
		in.TaxRegistrationNumber = "TAX-98765"
		in.ContactFirstName = "Jane"
		in.ContactLastName = "Doe"
		f, err := Build(in)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		c := f.Header.Company
		if c.RegistrationNumber != "REG-12345" {
			t.Errorf("RegistrationNumber = %q, want REG-12345", c.RegistrationNumber)
		}
		if len(c.TaxRegistration) != 1 || c.TaxRegistration[0].TaxRegistrationNumber != "TAX-98765" {
			t.Errorf("TaxRegistration = %+v, want [{TAX-98765}]", c.TaxRegistration)
		}
		got := c.Contact[0].ContactPerson
		if got.FirstName != "Jane" || got.LastName != "Doe" {
			t.Errorf("ContactPerson = %+v, want Jane/Doe", got)
		}
		payload, err := Marshal(f)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		validateAgainstXSD(t, payload)
	})

	t.Run("contact names past the schema's 35/70 char caps are truncated, not rejected", func(t *testing.T) {
		in := base()
		in.ContactFirstName = strings.Repeat("A", 40)
		in.ContactLastName = strings.Repeat("B", 80)
		f, err := Build(in)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got := f.Header.Company.Contact[0].ContactPerson
		if len(got.FirstName) != 35 {
			t.Errorf("FirstName len = %d, want truncated to 35", len(got.FirstName))
		}
		if len(got.LastName) != 70 {
			t.Errorf("LastName len = %d, want truncated to 70", len(got.LastName))
		}
	})
}

func TestBuild_TotalsSumAllLines(t *testing.T) {
	f, err := Build(fullInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gle := f.GeneralLedgerEntries
	if gle.NumberOfEntries != 2 {
		t.Errorf("NumberOfEntries = %d, want 2", gle.NumberOfEntries)
	}
	if gle.TotalDebit != "225.50" || gle.TotalCredit != "225.50" {
		t.Errorf("totals = %s / %s, want 225.50 / 225.50", gle.TotalDebit, gle.TotalCredit)
	}
	if len(gle.Journals) != 1 || len(gle.Journals[0].Transactions) != 2 {
		t.Fatalf("expected 1 journal with 2 transactions, got %+v", gle.Journals)
	}
	tx := gle.Journals[0].Transactions[0]
	if tx.Period != 3 || tx.PeriodYear != 2026 {
		t.Errorf("first transaction period = %d/%d, want 3/2026", tx.Period, tx.PeriodYear)
	}
	// The second entry has an empty description — both the transaction
	// and its lines must carry the NA marker, never an empty mandatory
	// element.
	tx2 := gle.Journals[0].Transactions[1]
	if tx2.Description != "NA" || tx2.Lines[0].Description != "NA" {
		t.Errorf("empty descriptions should become NA, got %q / %q", tx2.Description, tx2.Lines[0].Description)
	}
}

func TestBuild_BalanceSides(t *testing.T) {
	f, err := Build(fullInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	accounts := f.MasterFiles.GeneralLedgerAccounts.Accounts
	byID := map[string]XMLAccount{}
	for _, a := range accounts {
		byID[a.AccountID] = a
	}
	bank := byID["1100"]
	if bank.OpeningDebitBalance == nil || *bank.OpeningDebitBalance != "500.00" || bank.ClosingDebitBalance == nil || *bank.ClosingDebitBalance != "350.00" {
		t.Errorf("asset balances landed wrong: %+v", bank)
	}
	if bank.OpeningCreditBalance != nil || bank.ClosingCreditBalance != nil {
		t.Errorf("asset account must not carry credit balances: %+v", bank)
	}
	ap := byID["2000"]
	if ap.OpeningCreditBalance == nil || *ap.OpeningCreditBalance != "200.00" || ap.ClosingCreditBalance == nil || *ap.ClosingCreditBalance != "50.00" {
		t.Errorf("liability balances landed wrong: %+v", ap)
	}
	// A zero net is a (zero) debit balance, per setBalance's >= 0 rule.
	cogs := byID["4000"]
	if cogs.OpeningDebitBalance == nil || *cogs.OpeningDebitBalance != "0.00" {
		t.Errorf("zero balance should be a 0.00 debit balance: %+v", cogs)
	}
}

func TestBuild_TaxTableDeduplicatesCodes(t *testing.T) {
	f, err := Build(fullInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := f.MasterFiles.TaxTable.Entries
	if len(entries) != 1 {
		t.Fatalf("tax table must hold exactly one MVA entry, got %d", len(entries))
	}
	details := entries[0].TaxCodeDetails
	if len(details) != 2 {
		t.Fatalf("duplicate tax code should be dropped: %+v", details)
	}
	if details[0].TaxCode != "VAT-STD" || details[0].TaxPercentage == nil || *details[0].TaxPercentage != "25" {
		t.Errorf("first tax detail wrong: %+v", details[0])
	}
	if details[1].TaxCode != "VAT-0" || details[1].TaxPercentage != nil {
		t.Errorf("rate-less tax code must omit TaxPercentage: %+v", details[1])
	}
}

func TestBuild_PartyIDsFitSchemaLength(t *testing.T) {
	f, err := Build(fullInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	id := f.MasterFiles.Customers.Customers[0].CustomerID
	if strings.Contains(id, "-") || len(id) != 32 {
		t.Errorf("CustomerID should be the 32-char hyphenless UUID, got %q (len %d)", id, len(id))
	}
}

func TestBuild_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"bad from", func(in *Input) { in.From = "01/01/2026" }},
		{"bad to", func(in *Input) { in.To = "yesterday" }},
		{"bad created", func(in *Input) { in.Created = "" }},
		{"from after to", func(in *Input) { in.From, in.To = "2026-12-31", "2026-01-01" }},
		{"bad currency", func(in *Input) { in.DefaultCurrency = "US" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fullInput()
			tc.mutate(&in)
			if _, err := Build(in); err == nil {
				t.Fatal("Build accepted invalid input")
			}
		})
	}
}

func TestFormatMinor(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{100, "1.00"},
		{123456, "1234.56"},
		{-7, "-0.07"},
		{-123456, "-1234.56"},
	}
	for _, tc := range cases {
		if got := formatMinor(tc.in); got != tc.want {
			t.Errorf("formatMinor(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  hello  ", 10); got != "hello" {
		t.Errorf("truncate should trim: %q", got)
	}
	// Multi-byte runes count as one character, matching the XSD's
	// character-based length facets.
	if got := truncate("åååå", 2); got != "åå" {
		t.Errorf("truncate should cut by runes: %q", got)
	}
}

func TestGroupingCategory_UnknownTypeFallsBack(t *testing.T) {
	if got := groupingCategory("weird"); got != "NA" {
		t.Errorf("unknown account type should map to NA, got %q", got)
	}
}
