// Package ubl serializes single business documents into OASIS UBL 2.1
// XML: PurchaseOrder and SalesOrder become UBL Order documents (both map
// to UBL's one Order type — only the party roles flip), CustomerInvoice
// becomes a UBL Invoice. reference-data-model.md modeled these entities
// against their UBL counterparts from day one precisely so this export
// is a mapping exercise, not a redesign; this package is that mapping.
//
// Placement note (universal-core/CLAUDE.md "plugin-first"): unlike
// internal/kernel/saft — a jurisdiction-specific statutory format
// parked in core as a documented known gap until the plugin engine
// (uc-infra#57) exists — UBL 2.1 is the jurisdiction-INVARIANT OASIS
// vocabulary the data model itself is designed against, so this package
// is genuinely core, not a plugin-in-waiting. Country e-invoicing
// profiles layered on top of UBL (Peppol BIS, TR e-Fatura, any CIUS)
// remain plugins, always. Like saft, this package is deliberately
// entity-aware and is NOT part of the generic metadata kernel.
//
// The package is a pure serializer, same contract as saft: BuildOrder/
// BuildInvoice take an Input already read from storage and return the
// XML document model; they never touch the database. Callers
// (internal/api's UBL export handler) assemble inputs from the guarded
// CRUD engine.
//
// A CustomerInvoice deliberately has no line breakdown yet (see
// internal/kernel/sales.CustomerInvoice's doc comment), while UBL
// requires an Invoice to carry at least one InvoiceLine — BuildInvoice
// therefore emits ONE summary line carrying the invoice's own total.
// Deriving lines from the linked SalesOrder's SOLines instead would
// fabricate detail the invoice record doesn't assert (a partial invoice
// would be misstated); the summary line says exactly what we store and
// nothing more. When an InvoiceLine entity ships, this switches to real
// lines.
package ubl

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// UBL 2.1 namespace URIs. Each document's root declares its own maindoc
// namespace as the default plus the two component namespaces under the
// conventional cac/cbc prefixes the element tags below use literally
// (encoding/xml has no first-class multi-namespace prefix support; the
// literal-prefix-plus-xmlns-attribute form it emits is exactly the
// serialization the XSDs validate).
const (
	OrderNamespace   = "urn:oasis:names:specification:ubl:schema:xsd:Order-2"
	InvoiceNamespace = "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
	cacNamespace     = "urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"
	cbcNamespace     = "urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
)

// Party is one side of a document — a foundation Party record, or the
// exporting tenant itself (whose statutory profile doesn't exist yet;
// see the tenant statutory-profile card on the board — until it ships
// the tenant side carries its display name and no tax registration).
type Party struct {
	Name string
	// TaxID is the Party record's tax_id, when set — emitted as a
	// cac:PartyTaxScheme/cbc:CompanyID. Empty omits the whole element
	// (optional in UBL, unlike SAF-T's mandatory-with-"NA" convention).
	TaxID string
}

// Line is one PO/SO line. Amount fields are the float64 values the
// entity engine stores for FieldNumber fields; Build* formats them to
// the currency's minor-unit decimals, so formatting policy lives here,
// not in callers.
type Line struct {
	// ID identifies the line inside the document (LineItem/cbc:ID —
	// mandatory in UBL). Callers pass the line record's id.
	ID        string
	ItemSKU   string
	ItemName  string
	Qty       float64
	UnitPrice float64
	LineTotal float64
}

// OrderInput is everything BuildOrder needs for one UBL Order document.
type OrderInput struct {
	// ID is the document's natural key — po_number / so_number.
	ID string
	// IssueDate is the order_date (ISO-8601 date).
	IssueDate string
	// CurrencyCode is the ISO 4217 code every amount carries; UBL's
	// AmountType makes the currencyID attribute mandatory, which is why
	// a missing currency is a Build error here, not an omitted element.
	CurrencyCode string
	// MinorUnit is the currency's decimal places for amount formatting
	// (Currency.minor_unit; 2 for most currencies).
	MinorUnit int
	// Buyer/Seller map to BuyerCustomerParty/SellerSupplierParty: for a
	// PurchaseOrder the tenant buys from the vendor Party; for a
	// SalesOrder the customer Party buys from the tenant.
	Buyer  Party
	Seller Party
	// Lines must be non-empty — UBL requires at least one OrderLine.
	Lines []Line
	// Total is the order's total field → AnticipatedMonetaryTotal.
	Total float64
}

// InvoiceInput is everything BuildInvoice needs for one UBL Invoice.
type InvoiceInput struct {
	// ID is the invoice_number.
	ID string
	// IssueDate is the invoice_date (ISO-8601 date).
	IssueDate string
	// OrderRef is the linked SalesOrder's so_number, emitted as
	// cac:OrderReference. Empty omits the element.
	OrderRef     string
	CurrencyCode string
	MinorUnit    int
	// Supplier/Customer map to AccountingSupplierParty (the tenant —
	// the invoice issuer) and AccountingCustomerParty (the Party who
	// owes the money).
	Supplier Party
	Customer Party
	// Total is the invoice total → LegalMonetaryTotal/cbc:PayableAmount
	// and the single summary line (see the package comment).
	Total float64
}

// ---- XML document model ------------------------------------------------
// Field order in every struct below mirrors the XSD's sequence order —
// encoding/xml emits fields in declaration order and the schemas
// validate order strictly (same discipline as internal/kernel/saft).

// Order is the UBL Order document root.
type Order struct {
	XMLName  xml.Name `xml:"Order"`
	Xmlns    string   `xml:"xmlns,attr"`
	XmlnsCac string   `xml:"xmlns:cac,attr"`
	XmlnsCbc string   `xml:"xmlns:cbc,attr"`

	UBLVersionID         string        `xml:"cbc:UBLVersionID"`
	ID                   string        `xml:"cbc:ID"`
	IssueDate            string        `xml:"cbc:IssueDate"`
	DocumentCurrencyCode string        `xml:"cbc:DocumentCurrencyCode"`
	BuyerCustomerParty   CustomerParty `xml:"cac:BuyerCustomerParty"`
	SellerSupplierParty  SupplierParty `xml:"cac:SellerSupplierParty"`
	// AnticipatedMonetaryTotal is what the parties expect the order to
	// come to — the natural home for the order's total field.
	AnticipatedMonetaryTotal *MonetaryTotal `xml:"cac:AnticipatedMonetaryTotal,omitempty"`
	OrderLines               []OrderLine    `xml:"cac:OrderLine"`
}

// Invoice is the UBL Invoice document root.
type Invoice struct {
	XMLName  xml.Name `xml:"Invoice"`
	Xmlns    string   `xml:"xmlns,attr"`
	XmlnsCac string   `xml:"xmlns:cac,attr"`
	XmlnsCbc string   `xml:"xmlns:cbc,attr"`

	UBLVersionID            string          `xml:"cbc:UBLVersionID"`
	ID                      string          `xml:"cbc:ID"`
	IssueDate               string          `xml:"cbc:IssueDate"`
	DocumentCurrencyCode    string          `xml:"cbc:DocumentCurrencyCode"`
	OrderReference          *OrderReference `xml:"cac:OrderReference,omitempty"`
	AccountingSupplierParty SupplierParty   `xml:"cac:AccountingSupplierParty"`
	AccountingCustomerParty CustomerParty   `xml:"cac:AccountingCustomerParty"`
	LegalMonetaryTotal      MonetaryTotal   `xml:"cac:LegalMonetaryTotal"`
	InvoiceLines            []InvoiceLine   `xml:"cac:InvoiceLine"`
}

// CustomerParty wraps a Party in UBL's CustomerPartyType.
type CustomerParty struct {
	Party XMLParty `xml:"cac:Party"`
}

// SupplierParty wraps a Party in UBL's SupplierPartyType.
type SupplierParty struct {
	Party XMLParty `xml:"cac:Party"`
}

type XMLParty struct {
	PartyName      PartyName       `xml:"cac:PartyName"`
	PartyTaxScheme *PartyTaxScheme `xml:"cac:PartyTaxScheme,omitempty"`
}

type PartyName struct {
	Name string `xml:"cbc:Name"`
}

// PartyTaxScheme carries the party's tax identifier. The XSD makes the
// nested TaxScheme element mandatory; its generic "TAX" id says "the
// party's tax registration" without claiming a specific scheme — which
// scheme applies is a jurisdiction question, i.e. plugin territory.
type PartyTaxScheme struct {
	CompanyID string    `xml:"cbc:CompanyID"`
	TaxScheme TaxScheme `xml:"cac:TaxScheme"`
}

type TaxScheme struct {
	ID string `xml:"cbc:ID"`
}

type OrderReference struct {
	ID string `xml:"cbc:ID"`
}

// MonetaryTotal serves both Order/AnticipatedMonetaryTotal and
// Invoice/LegalMonetaryTotal (one XSD type, MonetaryTotalType).
type MonetaryTotal struct {
	PayableAmount Amount `xml:"cbc:PayableAmount"`
}

// Amount is a UBL amount — the currencyID attribute is mandatory in the
// unqualified data types the schemas build on.
type Amount struct {
	Value      string `xml:",chardata"`
	CurrencyID string `xml:"currencyID,attr"`
}

type OrderLine struct {
	LineItem LineItem `xml:"cac:LineItem"`
}

type LineItem struct {
	ID                  string    `xml:"cbc:ID"`
	Quantity            *Quantity `xml:"cbc:Quantity,omitempty"`
	LineExtensionAmount *Amount   `xml:"cbc:LineExtensionAmount,omitempty"`
	// Price precedes Item in LineItemType's sequence — the reverse of
	// InvoiceLineType, where Item comes first. Both orders are the
	// XSDs', not a choice.
	Price *Price  `xml:"cac:Price,omitempty"`
	Item  XMLItem `xml:"cac:Item"`
}

type InvoiceLine struct {
	ID                  string    `xml:"cbc:ID"`
	InvoicedQuantity    *Quantity `xml:"cbc:InvoicedQuantity,omitempty"`
	LineExtensionAmount Amount    `xml:"cbc:LineExtensionAmount"`
	Item                XMLItem   `xml:"cac:Item"`
}

// Quantity is a UBL quantity; the unitCode attribute is optional and
// this kernel has no UoM on order lines yet, so it's plain.
type Quantity struct {
	Value string `xml:",chardata"`
}

type Price struct {
	PriceAmount Amount `xml:"cbc:PriceAmount"`
}

type XMLItem struct {
	// Description precedes Name in ItemType's sequence.
	Description []string `xml:"cbc:Description,omitempty"`
	Name        string   `xml:"cbc:Name,omitempty"`
	// SellersItemIdentification carries the Item record's sku — the
	// identifier the selling side quotes, which is the closest match
	// for a single-company item master.
	SellersItemIdentification *ItemIdentification `xml:"cac:SellersItemIdentification,omitempty"`
}

type ItemIdentification struct {
	ID string `xml:"cbc:ID"`
}

// ---- building ----------------------------------------------------------

// BuildOrder assembles a UBL Order document model. It validates only
// what schema validity demands (document id present, date shape,
// currency shape, at least one line) — business validation happened at
// record-save time inside the guarded CRUD engine.
func BuildOrder(in OrderInput) (*Order, error) {
	currency, minor, err := validateCommon("order", in.ID, in.IssueDate, in.CurrencyCode, in.MinorUnit)
	if err != nil {
		return nil, err
	}
	if len(in.Lines) == 0 {
		return nil, fmt.Errorf("ubl: order %q has no lines; UBL requires at least one OrderLine", in.ID)
	}

	doc := &Order{
		Xmlns:                OrderNamespace,
		XmlnsCac:             cacNamespace,
		XmlnsCbc:             cbcNamespace,
		UBLVersionID:         "2.1",
		ID:                   in.ID,
		IssueDate:            in.IssueDate,
		DocumentCurrencyCode: currency,
		BuyerCustomerParty:   CustomerParty{Party: xmlParty(in.Buyer)},
		SellerSupplierParty:  SupplierParty{Party: xmlParty(in.Seller)},
		AnticipatedMonetaryTotal: &MonetaryTotal{
			PayableAmount: amount(in.Total, minor, currency),
		},
	}
	for _, l := range in.Lines {
		lineTotal := amount(l.LineTotal, minor, currency)
		doc.OrderLines = append(doc.OrderLines, OrderLine{LineItem: LineItem{
			ID:                  l.ID,
			Quantity:            &Quantity{Value: formatQty(l.Qty)},
			LineExtensionAmount: &lineTotal,
			Price:               &Price{PriceAmount: amount(l.UnitPrice, minor, currency)},
			Item:                xmlItem(l),
		}})
	}
	return doc, nil
}

// BuildInvoice assembles a UBL Invoice document model with the single
// summary line the package comment explains.
func BuildInvoice(in InvoiceInput) (*Invoice, error) {
	currency, minor, err := validateCommon("invoice", in.ID, in.IssueDate, in.CurrencyCode, in.MinorUnit)
	if err != nil {
		return nil, err
	}

	total := amount(in.Total, minor, currency)
	summaryDesc := "Invoice total"
	if in.OrderRef != "" {
		summaryDesc = "Invoice total for sales order " + in.OrderRef
	}
	doc := &Invoice{
		Xmlns:                   InvoiceNamespace,
		XmlnsCac:                cacNamespace,
		XmlnsCbc:                cbcNamespace,
		UBLVersionID:            "2.1",
		ID:                      in.ID,
		IssueDate:               in.IssueDate,
		DocumentCurrencyCode:    currency,
		AccountingSupplierParty: SupplierParty{Party: xmlParty(in.Supplier)},
		AccountingCustomerParty: CustomerParty{Party: xmlParty(in.Customer)},
		LegalMonetaryTotal:      MonetaryTotal{PayableAmount: total},
		InvoiceLines: []InvoiceLine{{
			ID:                  "1",
			InvoicedQuantity:    &Quantity{Value: "1"},
			LineExtensionAmount: total,
			Item:                XMLItem{Name: summaryDesc},
		}},
	}
	if in.OrderRef != "" {
		doc.OrderReference = &OrderReference{ID: in.OrderRef}
	}
	return doc, nil
}

// MarshalOrder / MarshalInvoice render a built document as the final
// XML payload, declaration included — same shape as saft.Marshal.
func MarshalOrder(doc *Order) ([]byte, error)     { return marshal(doc) }
func MarshalInvoice(doc *Invoice) ([]byte, error) { return marshal(doc) }

func marshal(doc any) ([]byte, error) {
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ubl: marshal document: %w", err)
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

func validateCommon(kind, id, issueDate, currencyCode string, minorUnit int) (currency string, minor int, err error) {
	if strings.TrimSpace(id) == "" {
		return "", 0, fmt.Errorf("ubl: %s document id is empty", kind)
	}
	if _, err := time.Parse("2006-01-02", issueDate); err != nil {
		return "", 0, fmt.Errorf("ubl: %s %q issue date is not an ISO-8601 date: %q", kind, id, issueDate)
	}
	currency = strings.ToUpper(strings.TrimSpace(currencyCode))
	if len(currency) != 3 {
		return "", 0, fmt.Errorf("ubl: %s %q currency %q is not a 3-letter ISO 4217 code", kind, id, currencyCode)
	}
	minor = minorUnit
	if minor < 0 {
		return "", 0, fmt.Errorf("ubl: %s %q has negative currency minor unit %d", kind, id, minorUnit)
	}
	return currency, minor, nil
}

func xmlParty(p Party) XMLParty {
	x := XMLParty{PartyName: PartyName{Name: p.Name}}
	if p.TaxID != "" {
		x.PartyTaxScheme = &PartyTaxScheme{
			CompanyID: p.TaxID,
			TaxScheme: TaxScheme{ID: "TAX"},
		}
	}
	return x
}

func xmlItem(l Line) XMLItem {
	x := XMLItem{Name: l.ItemName}
	if l.ItemSKU != "" {
		x.SellersItemIdentification = &ItemIdentification{ID: l.ItemSKU}
	}
	return x
}

// amount formats a float64 entity value to the currency's minor-unit
// decimals. Values come from JSONB numbers (float64 is what the entity
// engine stores for FieldNumber); fixed-decimal formatting here keeps
// every amount in a document consistently scaled.
//
// Known float artifact: a value whose decimal form sits on a half-unit
// boundary (2.675) is stored as the nearest binary double (2.67499…)
// and formats DOWN ("2.67"), where decimal half-up would give "2.68".
// This is the entity engine's float storage showing through, not a
// choice this package can fix — exact minor-unit money awaits a
// money.Money-equivalent integer type on FieldNumber amounts (the
// CLAUDE.md money convention; the ledger already stores integer minor
// units). Pinned by TestAmountFloatBoundaryArtifact so a future money
// type consciously changes it.
func amount(v float64, minor int, currency string) Amount {
	return Amount{Value: strconv.FormatFloat(v, 'f', minor, 64), CurrencyID: currency}
}

// formatQty renders a quantity without artificial trailing zeros (a
// count of 5 is "5", not "5.00" — quantities aren't money and have no
// minor unit).
func formatQty(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
