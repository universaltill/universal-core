// Package saft serializes a tenant's general ledger into a SAF-T
// Financial audit file (OECD Standard Audit File — Taxation), targeting
// the Norwegian SAF-T Financial v1.30 schema — the best-documented
// implementation of the OECD standard (Skatteetaten publishes the XSD
// and validator publicly), which is exactly the selection criterion the
// backlog task set ("whichever launch market's SAF-T schema is
// best-documented first"; no current launch market mandates SAF-T
// proper, so the reference schema wins over an arbitrary local one).
//
// Placement note (universal-core/CLAUDE.md "plugin-first"): a statutory
// export format is jurisdiction-specific and therefore belongs in a
// plugin. The plugin engine doesn't exist yet (uc-infra#57), so this
// package follows the same documented known-gap precedent as the XLSX
// half of csvimport: built as a core capability now, expected to migrate
// behind the plugin boundary when the plugin engine lands. It is NOT
// part of the generic metadata kernel — like internal/kernel/purchasing
// or internal/data's ReportingRepo, it is deliberately entity-aware.
//
// The package is a pure serializer: Build takes an Input already read
// from storage and returns the XML document model; it never touches the
// database. Callers (internal/api's SAF-T export handler) assemble the
// Input from internal/data repositories and the guarded CRUD engine.
//
// Scope of this first slice: Header, MasterFiles (GeneralLedgerAccounts,
// Customers, Suppliers, TaxTable) and GeneralLedgerEntries. The XSD's
// SourceDocuments sections (invoices, payments, stock movement, assets)
// are deliberately not generated yet — the GL entries are the
// audit-file core and everything the current ledger actually posts.
package saft

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Namespace is the Norwegian SAF-T Financial target namespace — the
// xmlns every element in a generated file lives in.
const Namespace = "urn:StandardAuditFile-Taxation-Financial:NO"

// notAvailable is the Norwegian SAF-T documentation's convention for a
// mandatory text element whose value the producing system genuinely does
// not hold ("NA"). Since uc-infra#63 the kernel DOES model a tenant
// statutory profile (foundation.Party + the own_organization PartyRole),
// so this is now the fallback for a tenant that has not configured one —
// never a lazy default for data that does exist.
const notAvailable = "NA"

// Input is everything Build needs, already read from storage. Amounts
// are integer minor units (the ledger's own representation); Build does
// the 2-decimal formatting itself so no caller ever routes money through
// a float.
type Input struct {
	// Created is the file-generation date (ISO-8601 date). Passed in
	// rather than read from the clock so Build stays deterministic for
	// golden-master tests.
	Created string
	// From/To bound the exported period (ISO-8601 dates, inclusive).
	From, To string

	SoftwareVersion string

	// CompanyName is the exporting tenant's display name.
	CompanyName string
	// RegistrationNumber / TaxRegistrationNumber are the tenant's
	// statutory identifiers, sourced from the foundation.Party holding
	// the "own_organization" PartyRole (uc-infra#63) when exactly one
	// such Party exists for the tenant. Callers pass "" when it doesn't
	// (no statutory profile configured yet, or an ambiguous/deleted
	// one) and the file carries the spec's "NA" marker, same as before.
	RegistrationNumber    string
	TaxRegistrationNumber string
	// ContactFirstName / ContactLastName are the statutory contact
	// person's name, from the same own_organization Party (uc-infra#63).
	// Each falls back to the spec's "NA" marker independently when
	// blank — a party with a first name but no last name on file still
	// gets a real first name in the file rather than losing it to an
	// all-or-nothing check, same per-field fallback RegistrationNumber
	// and CompanyName already use via orNA.
	ContactFirstName string
	ContactLastName  string

	// DefaultCurrency is the ISO 4217 code for file amounts. The ledger
	// is single-currency today (see finance.SyncGLAccounts's
	// DefaultGLCurrency note); callers pass the distinct gl_accounts
	// currency when there is exactly one.
	DefaultCurrency string

	Accounts  []Account
	Customers []Party
	Suppliers []Party
	TaxCodes  []TaxCode
	Entries   []Entry
}

// Account is one GL account with its opening/closing balance for the
// selected period. Opening/ClosingMinor are signed nets (debits minus
// credits): positive lands on the file's debit-balance side, negative on
// the credit side.
type Account struct {
	Code string
	Name string
	// Type is the finance.Account/gl_accounts account_type enum:
	// asset | liability | equity | income | expense.
	Type         string
	OpeningMinor int64
	ClosingMinor int64
}

// Party is one customer or supplier row (a foundation Party holding the
// customer/vendor PartyRole).
type Party struct {
	// ID is the Party record id. UUIDs are 36 chars but the SAF-T
	// Customer/SupplierID element caps at 35 — Build strips the hyphens
	// (32 hex chars) rather than truncating, so the identifier stays
	// unique and reversible.
	ID   string
	Name string
	// RegistrationNumber is the Party's tax_id field, when set.
	RegistrationNumber string
}

// TaxCode is one finance.TaxCode record. Rate is the percentage as the
// entity stores it (5.0 == 5%). Country is the record's jurisdiction
// field when it already is an ISO 3166 alpha-2 code — anything else
// (empty, a free-text country name) falls back to the file's own
// country, see isoCountryOrFileCountry.
type TaxCode struct {
	Code    string
	Name    string
	Country string
	Rate    float64
	HasRate bool
}

// Entry is one posted journal entry.
type Entry struct {
	ID string
	// EntryDate is the accounting date (GLPostingDate/TransactionDate).
	EntryDate string
	// PostedDate is the date the system recorded the entry
	// (SystemEntryDate) — journal_entries.posted_at's date part.
	PostedDate  string
	Description string
	SourceType  string
	Lines       []Line
}

// Line is one debit-or-credit line of an Entry.
type Line struct {
	AccountCode string
	DebitMinor  int64
	CreditMinor int64
}

// ---- XML document model ------------------------------------------------
// Field order in every struct below mirrors the XSD's sequence order —
// encoding/xml emits fields in declaration order and the schema
// validates order strictly. Optional elements are pointers/omitempty.

// AuditFile is the document root.
type AuditFile struct {
	XMLName              xml.Name              `xml:"urn:StandardAuditFile-Taxation-Financial:NO AuditFile"`
	Header               Header                `xml:"Header"`
	MasterFiles          *MasterFiles          `xml:"MasterFiles,omitempty"`
	GeneralLedgerEntries *GeneralLedgerEntries `xml:"GeneralLedgerEntries"`
}

type Header struct {
	AuditFileVersion     string            `xml:"AuditFileVersion"`
	AuditFileCountry     string            `xml:"AuditFileCountry"`
	AuditFileDateCreated string            `xml:"AuditFileDateCreated"`
	SoftwareCompanyName  string            `xml:"SoftwareCompanyName"`
	SoftwareID           string            `xml:"SoftwareID"`
	SoftwareVersion      string            `xml:"SoftwareVersion"`
	Company              Company           `xml:"Company"`
	DefaultCurrencyCode  string            `xml:"DefaultCurrencyCode"`
	SelectionCriteria    SelectionCriteria `xml:"SelectionCriteria"`
	// TaxAccountingBasis is the extension element the Norwegian root
	// adds after HeaderStructure; "A" (regnskapspliktig) is the only
	// value the Financial schema admits.
	TaxAccountingBasis string `xml:"TaxAccountingBasis"`
}

// Company is the Header's company (CompanyHeaderStructure): unlike the
// base CompanyStructure, RegistrationNumber and at least one Contact are
// mandatory here.
type Company struct {
	RegistrationNumber string            `xml:"RegistrationNumber"`
	Name               string            `xml:"Name"`
	Contact            []Contact         `xml:"Contact"`
	TaxRegistration    []TaxRegistration `xml:"TaxRegistration,omitempty"`
}

type Contact struct {
	ContactPerson ContactPerson `xml:"ContactPerson"`
}

type ContactPerson struct {
	FirstName string `xml:"FirstName"`
	LastName  string `xml:"LastName"`
}

type TaxRegistration struct {
	TaxRegistrationNumber string `xml:"TaxRegistrationNumber"`
}

type SelectionCriteria struct {
	SelectionStartDate string `xml:"SelectionStartDate"`
	SelectionEndDate   string `xml:"SelectionEndDate"`
}

type MasterFiles struct {
	GeneralLedgerAccounts *GeneralLedgerAccounts `xml:"GeneralLedgerAccounts,omitempty"`
	Customers             *Customers             `xml:"Customers,omitempty"`
	Suppliers             *Suppliers             `xml:"Suppliers,omitempty"`
	TaxTable              *TaxTable              `xml:"TaxTable,omitempty"`
}

type GeneralLedgerAccounts struct {
	Accounts []XMLAccount `xml:"Account"`
}

// XMLAccount is the file-side account row. Exactly one of the
// Opening/Closing pairs is set per side (the XSD choice) — Build decides
// by the balance's sign.
type XMLAccount struct {
	AccountID          string `xml:"AccountID"`
	AccountDescription string `xml:"AccountDescription"`
	// GroupingCategory/GroupingCode map the account into the Norwegian
	// grouping code list. The XSD types these as free text (the official
	// list is documentation, not schema-enforced); Build maps this
	// kernel's five account types to the list's top-level terms — an
	// approximation a real Norwegian statutory filing would refine via
	// the official mapping tables (out of scope for this slice, the
	// account-type term keeps the file self-describing).
	GroupingCategory     string  `xml:"GroupingCategory"`
	GroupingCode         string  `xml:"GroupingCode"`
	AccountType          string  `xml:"AccountType"`
	OpeningDebitBalance  *string `xml:"OpeningDebitBalance,omitempty"`
	OpeningCreditBalance *string `xml:"OpeningCreditBalance,omitempty"`
	ClosingDebitBalance  *string `xml:"ClosingDebitBalance,omitempty"`
	ClosingCreditBalance *string `xml:"ClosingCreditBalance,omitempty"`
}

type Customers struct {
	Customers []Customer `xml:"Customer"`
}

// Customer extends CompanyStructure with CustomerID — the base's
// elements come first, in base order.
type Customer struct {
	RegistrationNumber string `xml:"RegistrationNumber,omitempty"`
	Name               string `xml:"Name"`
	CustomerID         string `xml:"CustomerID"`
}

type Suppliers struct {
	Suppliers []Supplier `xml:"Supplier"`
}

type Supplier struct {
	RegistrationNumber string `xml:"RegistrationNumber,omitempty"`
	Name               string `xml:"Name"`
	SupplierID         string `xml:"SupplierID"`
}

// TaxTable holds exactly one TaxTableEntry ("MVA" — the only TaxType the
// Norwegian TaxInformationStructure admits) with one TaxCodeDetails per
// tenant TaxCode: the XSD keys TaxTableEntry uniquely by TaxType, so
// one-entry-many-details is the only valid way to list several codes.
type TaxTable struct {
	Entries []TaxTableEntry `xml:"TaxTableEntry"`
}

type TaxTableEntry struct {
	TaxType        string           `xml:"TaxType"`
	Description    string           `xml:"Description"`
	TaxCodeDetails []TaxCodeDetails `xml:"TaxCodeDetails"`
}

type TaxCodeDetails struct {
	TaxCode       string  `xml:"TaxCode"`
	Description   string  `xml:"Description,omitempty"`
	TaxPercentage *string `xml:"TaxPercentage,omitempty"`
	// Country is mandatory in the XSD (ISO 3166 alpha-2). Fed from the
	// TaxCode record's jurisdiction field when that already is a real
	// alpha-2 code; anything else falls back to "NO" — the file's own
	// AuditFileCountry — because the spec's "NA" marker is also
	// Namibia's ISO code, and a validator can't tell the difference
	// (independent review finding, #28).
	Country string `xml:"Country"`
	// StandardTaxCode maps to the Norwegian standard VAT code list. No
	// mapping exists in this kernel yet, and the schema's own inline
	// documentation says to use "NA" exactly when mapping is not
	// possible (the pattern [0-9anAN]{1,2} admits it). Real per-code
	// mapping arrives with the tenant statutory profile follow-up.
	StandardTaxCode string `xml:"StandardTaxCode"`
	// BaseRate is the taxable proportion of the base (percent);
	// 100 — the whole base is taxable — is the standard value.
	BaseRate string `xml:"BaseRate"`
}

type GeneralLedgerEntries struct {
	NumberOfEntries int       `xml:"NumberOfEntries"`
	TotalDebit      string    `xml:"TotalDebit"`
	TotalCredit     string    `xml:"TotalCredit"`
	Journals        []Journal `xml:"Journal,omitempty"`
}

type Journal struct {
	JournalID    string        `xml:"JournalID"`
	Description  string        `xml:"Description"`
	Type         string        `xml:"Type"`
	Transactions []Transaction `xml:"Transaction,omitempty"`
}

type Transaction struct {
	TransactionID   string `xml:"TransactionID"`
	Period          int    `xml:"Period"`
	PeriodYear      int    `xml:"PeriodYear"`
	TransactionDate string `xml:"TransactionDate"`
	// VoucherType carries the posting's business-document source
	// (Entry.SourceType — "CustomerInvoice", "GoodsReceipt") when the
	// ledger recorded one; the XSD's optional voucher-type element is
	// exactly this concept.
	VoucherType     string    `xml:"VoucherType,omitempty"`
	Description     string    `xml:"Description"`
	SystemEntryDate string    `xml:"SystemEntryDate"`
	GLPostingDate   string    `xml:"GLPostingDate"`
	Lines           []XMLLine `xml:"Line"`
}

type XMLLine struct {
	RecordID     string  `xml:"RecordID"`
	AccountID    string  `xml:"AccountID"`
	Description  string  `xml:"Description"`
	DebitAmount  *Amount `xml:"DebitAmount,omitempty"`
	CreditAmount *Amount `xml:"CreditAmount,omitempty"`
}

type Amount struct {
	Amount string `xml:"Amount"`
}

// ---- building ----------------------------------------------------------

// Build assembles the document model from in. It validates only what it
// must to produce a schema-valid file (date shapes, currency shape) —
// business-level validation (does the ledger balance?) already happened
// at posting time, inside internal/kernel/ledger.
func Build(in Input) (*AuditFile, error) {
	for _, d := range []struct{ name, v string }{
		{"created", in.Created}, {"from", in.From}, {"to", in.To},
	} {
		if _, err := time.Parse("2006-01-02", d.v); err != nil {
			return nil, fmt.Errorf("saft: %s is not an ISO-8601 date: %q", d.name, d.v)
		}
	}
	if in.From > in.To {
		return nil, fmt.Errorf("saft: from %q is after to %q", in.From, in.To)
	}
	currency := strings.ToUpper(strings.TrimSpace(in.DefaultCurrency))
	if len(currency) != 3 {
		return nil, fmt.Errorf("saft: default currency %q is not a 3-letter ISO 4217 code", in.DefaultCurrency)
	}

	f := &AuditFile{
		Header: Header{
			AuditFileVersion:     "1.30",
			AuditFileCountry:     "NO",
			AuditFileDateCreated: in.Created,
			SoftwareCompanyName:  "Universal Till",
			SoftwareID:           "universal-core",
			SoftwareVersion:      orNA(truncate(in.SoftwareVersion, 18)),
			Company: Company{
				RegistrationNumber: orNA(truncate(in.RegistrationNumber, 35)),
				Name:               orNA(truncate(in.CompanyName, 256)),
				// The schema demands a contact person. 35/70 are the
				// Norwegian XSD's SAFmiddle1textType/SAFmiddle2textType
				// caps for FirstName/LastName (testdata/Norwegian_SAF-T_
				// Financial_Schema_v_1.30.xsd) — each falls back to the
				// spec's NA marker independently when blank (uc-infra#63).
				Contact: []Contact{{ContactPerson: ContactPerson{
					FirstName: orNA(truncate(in.ContactFirstName, 35)),
					LastName:  orNA(truncate(in.ContactLastName, 70)),
				}}},
			},
			DefaultCurrencyCode: currency,
			SelectionCriteria:   SelectionCriteria{SelectionStartDate: in.From, SelectionEndDate: in.To},
			TaxAccountingBasis:  "A",
		},
	}
	if in.TaxRegistrationNumber != "" {
		f.Header.Company.TaxRegistration = []TaxRegistration{{TaxRegistrationNumber: truncate(in.TaxRegistrationNumber, 35)}}
	}

	f.MasterFiles = buildMasterFiles(in)
	f.GeneralLedgerEntries = buildEntries(in)
	return f, nil
}

func buildMasterFiles(in Input) *MasterFiles {
	mf := &MasterFiles{}
	if len(in.Accounts) > 0 {
		accounts := make([]XMLAccount, len(in.Accounts))
		for i, a := range in.Accounts {
			x := XMLAccount{
				AccountID:          truncate(a.Code, 70),
				AccountDescription: orNA(truncate(a.Name, 256)),
				GroupingCategory:   groupingCategory(a.Type),
				GroupingCode:       truncate(a.Code, 35),
				AccountType:        "GL",
			}
			setBalance(a.OpeningMinor, &x.OpeningDebitBalance, &x.OpeningCreditBalance)
			setBalance(a.ClosingMinor, &x.ClosingDebitBalance, &x.ClosingCreditBalance)
			accounts[i] = x
		}
		mf.GeneralLedgerAccounts = &GeneralLedgerAccounts{Accounts: accounts}
	}
	if len(in.Customers) > 0 {
		c := &Customers{}
		for _, p := range in.Customers {
			c.Customers = append(c.Customers, Customer{
				RegistrationNumber: truncate(p.RegistrationNumber, 35),
				Name:               orNA(truncate(p.Name, 256)),
				CustomerID:         partyID(p.ID),
			})
		}
		mf.Customers = c
	}
	if len(in.Suppliers) > 0 {
		s := &Suppliers{}
		for _, p := range in.Suppliers {
			s.Suppliers = append(s.Suppliers, Supplier{
				RegistrationNumber: truncate(p.RegistrationNumber, 35),
				Name:               orNA(truncate(p.Name, 256)),
				SupplierID:         partyID(p.ID),
			})
		}
		mf.Suppliers = s
	}
	if len(in.TaxCodes) > 0 {
		// "MVA"/"Merverdiavgift" are the only TaxType/Description values
		// the Norwegian schema's enumerations admit for a tax table entry.
		entry := TaxTableEntry{TaxType: "MVA", Description: "Merverdiavgift"}
		seen := map[string]bool{}
		for _, tc := range in.TaxCodes {
			code := truncate(tc.Code, 70)
			// The XSD keys TaxCodeDetails uniquely by TaxCode — a
			// duplicate code (application-level uniqueness only, same as
			// every natural key in this kernel) would make the whole
			// file invalid, so later duplicates are dropped here.
			if code == "" || seen[code] {
				continue
			}
			seen[code] = true
			d := TaxCodeDetails{
				TaxCode:         code,
				Description:     truncate(tc.Name, 256),
				Country:         isoCountryOrFileCountry(tc.Country),
				StandardTaxCode: notAvailable,
				BaseRate:        "100",
			}
			if tc.HasRate {
				pct := strconv.FormatFloat(tc.Rate, 'f', -1, 64)
				d.TaxPercentage = &pct
			}
			entry.TaxCodeDetails = append(entry.TaxCodeDetails, d)
		}
		if len(entry.TaxCodeDetails) > 0 {
			mf.TaxTable = &TaxTable{Entries: []TaxTableEntry{entry}}
		}
	}
	if mf.GeneralLedgerAccounts == nil && mf.Customers == nil && mf.Suppliers == nil && mf.TaxTable == nil {
		return nil
	}
	return mf
}

func buildEntries(in Input) *GeneralLedgerEntries {
	gle := &GeneralLedgerEntries{
		NumberOfEntries: len(in.Entries),
		TotalDebit:      formatMinor(0),
		TotalCredit:     formatMinor(0),
	}
	if len(in.Entries) == 0 {
		return gle
	}
	var totalDebit, totalCredit int64
	journal := Journal{JournalID: "GL", Description: "General ledger", Type: "GL"}
	for _, e := range in.Entries {
		t := Transaction{
			TransactionID:   truncate(e.ID, 70),
			TransactionDate: e.EntryDate,
			VoucherType:     truncate(e.SourceType, 70),
			Description:     orNA(truncate(e.Description, 256)),
			SystemEntryDate: e.PostedDate,
			GLPostingDate:   e.EntryDate,
		}
		if d, err := time.Parse("2006-01-02", e.EntryDate); err == nil {
			t.Period = int(d.Month())
			t.PeriodYear = d.Year()
		}
		for i, l := range e.Lines {
			line := XMLLine{
				RecordID:    strconv.Itoa(i + 1),
				AccountID:   truncate(l.AccountCode, 70),
				Description: orNA(truncate(e.Description, 256)),
			}
			// Exactly-one-side is the ledger's own invariant (DB CHECK:
			// debit or credit is zero); a fully-zero line — which the
			// CHECK permits — renders as a zero debit, the conventional
			// side for "no amount".
			switch {
			case l.CreditMinor != 0:
				line.CreditAmount = &Amount{Amount: formatMinor(l.CreditMinor)}
				totalCredit += l.CreditMinor
			default:
				line.DebitAmount = &Amount{Amount: formatMinor(l.DebitMinor)}
				totalDebit += l.DebitMinor
			}
			t.Lines = append(t.Lines, line)
		}
		journal.Transactions = append(journal.Transactions, t)
	}
	gle.TotalDebit = formatMinor(totalDebit)
	gle.TotalCredit = formatMinor(totalCredit)
	gle.Journals = []Journal{journal}
	return gle
}

// Marshal renders f as an indented XML document with the standard
// declaration — the exact bytes the export endpoint streams.
func Marshal(f *AuditFile) ([]byte, error) {
	body, err := xml.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("saft: marshal audit file: %w", err)
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

// groupingCategories maps the kernel's five account types to the
// Norwegian grouping code list's top-level terms — see
// XMLAccount.GroupingCategory's doc comment for why this approximation
// is acceptable for this slice.
var groupingCategories = map[string]string{
	"asset":     "eiendeler",
	"liability": "gjeld",
	"equity":    "egenkapital",
	"income":    "inntekt",
	"expense":   "kostnad",
}

func groupingCategory(accountType string) string {
	if c, ok := groupingCategories[accountType]; ok {
		return c
	}
	return notAvailable
}

// setBalance puts a signed net (debits minus credits) on the correct
// side of an XSD debit/credit choice: non-negative nets are a debit
// balance, negative nets a credit balance, always as an absolute value.
func setBalance(netMinor int64, debit, credit **string) {
	v := formatMinor(abs64(netMinor))
	if netMinor >= 0 {
		*debit = &v
	} else {
		*credit = &v
	}
}

// formatMinor renders integer minor units as a 2-decimal amount string
// using integer math only — money never transits a float here.
func formatMinor(m int64) string {
	sign := ""
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// partyID renders a Party record id as a SAF-T Customer/SupplierID:
// UUIDs are 36 chars against the element's 35-char cap, so the hyphens
// go (32 hex chars, still unique and mechanically reversible).
func partyID(id string) string {
	return truncate(strings.ReplaceAll(id, "-", ""), 35)
}

// truncate caps s at max bytes-as-runes — the XSD length facets count
// characters, and silently generating an invalid file over one long
// description would fail the whole export.
func truncate(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max])
}

// isoCountryOrFileCountry passes s through uppercased when it already
// looks like an ISO 3166 alpha-2 code (exactly two ASCII letters) and
// falls back to the file's own AuditFileCountry ("NO") otherwise. The
// fallback is deliberately NOT the "NA" marker used for text elements:
// ISOCountryCode is a hard two-character facet, and "NA" is Namibia's
// real ISO code — an unset jurisdiction must not silently register a
// tax code in Namibia (independent review finding, #28).
func isoCountryOrFileCountry(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return "NO"
	}
	return s
}

// orNA substitutes the spec's NA marker for an empty mandatory value.
func orNA(s string) string {
	if s == "" {
		return notAvailable
	}
	return s
}

// SortAccounts orders accounts by code — deterministic file output for
// golden-master tests regardless of map/query iteration order upstream.
func SortAccounts(accounts []Account) {
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Code < accounts[j].Code })
}
