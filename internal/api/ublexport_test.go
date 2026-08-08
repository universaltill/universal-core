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
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// ublFixture is everything the UBL export tests create: a tenant with
// foundation+purchasing+sales published, one vendor and one customer
// Party, a USD Currency, two Items, a two-line PurchaseOrder, a
// one-line SalesOrder, and a CustomerInvoice against that SalesOrder.
type ublFixture struct {
	tenantID string
	db       *sql.DB
	mux      *http.ServeMux
	router   *tenantdb.Router

	vendorID, customerID, currencyID string
	widgetID                         string
	poID, soID, invoiceID            string
	// noCurrencySOID / noLinesPOID are the degenerate records the 400
	// tests exercise.
	noCurrencySOID string
	noLinesPOID    string
}

func setupUBLTenant(t *testing.T) *ublFixture {
	t.Helper()
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := foundation.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	if err := purchasing.Publish(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}
	if err := purchasing.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("purchasing.PublishStatuses: %v", err)
	}
	if err := sales.Publish(ctx, db, actor); err != nil {
		t.Fatalf("sales.Publish: %v", err)
	}
	if err := sales.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("sales.PublishForms: %v", err)
	}
	if err := sales.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("sales.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(db)
	defs := entityDefLookup(t, db)
	create := func(entityType string, fields map[string]any) string {
		t.Helper()
		rec, err := engine.Create(ctx, defs(entityType), fields, actor)
		if err != nil {
			t.Fatalf("create %s: %v", entityType, err)
		}
		return rec.ID
	}
	statusID := func(statusTypeCode, statusCode string) string {
		t.Helper()
		statusTypes, err := engine.ListByField(ctx, defs("StatusType"), "code", statusTypeCode)
		if err != nil || len(statusTypes) == 0 {
			t.Fatalf("list StatusType %s: %v (n=%d)", statusTypeCode, err, len(statusTypes))
		}
		statuses, err := engine.ListByField(ctx, defs("Status"), "status_type_id", statusTypes[0].ID)
		if err != nil {
			t.Fatalf("list Status for %s: %v", statusTypeCode, err)
		}
		for _, s := range statuses {
			if code, _ := s.Data["code"].(string); code == statusCode {
				return s.ID
			}
		}
		t.Fatalf("no %q Status seeded for %s", statusCode, statusTypeCode)
		return ""
	}

	f := &ublFixture{tenantID: tenantID, db: db, router: router}
	f.vendorID = create("Party", map[string]any{
		"party_type": "organization", "name": "Supplier & Sons <LLC>", "tax_id": "S-999", "status": "active",
	})
	// uc-infra#78: PurchaseOrder.vendor_id/SalesOrder.customer_id now
	// declare a TargetFilter requiring the referenced Party to actually
	// hold the matching PartyRole.
	create("PartyRole", map[string]any{"party_id": f.vendorID, "role_type": "vendor"})
	f.customerID = create("Party", map[string]any{
		"party_type": "organization", "name": "Customer GmbH", "tax_id": "C-111", "status": "active",
	})
	create("PartyRole", map[string]any{"party_id": f.customerID, "role_type": "customer"})
	f.currencyID = create("Currency", map[string]any{
		"code": "USD", "name": "US Dollar", "minor_unit": 2.0,
	})
	widgetID := create("Item", map[string]any{
		"sku": "SKU-1", "name": "Widget", "item_type": "stock",
	})
	f.widgetID = widgetID
	gadgetID := create("Item", map[string]any{
		"sku": "SKU-2", "name": "Gadget", "item_type": "stock",
	})

	poDraft := statusID("purchase_order_status", "draft")
	f.poID = create("PurchaseOrder", map[string]any{
		"po_number": "PO-1042", "vendor_id": f.vendorID, "order_date": "2026-07-01",
		"currency_id": f.currencyID, "status_id": poDraft, "total": 145.0,
	})
	create("POLine", map[string]any{
		"purchase_order_id": f.poID, "item_id": widgetID, "qty": 10.0, "unit_price": 12.5, "line_total": 125.0,
	})
	create("POLine", map[string]any{
		"purchase_order_id": f.poID, "item_id": gadgetID, "qty": 2.5, "unit_price": 8.0, "line_total": 20.0,
	})
	f.noLinesPOID = create("PurchaseOrder", map[string]any{
		"po_number": "PO-EMPTY", "vendor_id": f.vendorID, "order_date": "2026-07-02",
		"currency_id": f.currencyID, "status_id": poDraft, "total": 0.0,
	})

	soDraft := statusID("sales_order_status", "draft")
	f.soID = create("SalesOrder", map[string]any{
		"so_number": "SO-2001", "customer_id": f.customerID, "order_date": "2026-07-10",
		"currency_id": f.currencyID, "status_id": soDraft, "total": 500.0,
	})
	create("SOLine", map[string]any{
		"sales_order_id": f.soID, "item_id": widgetID, "qty": 40.0, "unit_price": 12.5, "line_total": 500.0,
	})
	f.noCurrencySOID = create("SalesOrder", map[string]any{
		"so_number": "SO-NOCURR", "customer_id": f.customerID, "order_date": "2026-07-11",
		"status_id": soDraft, "total": 10.0,
	})
	create("SOLine", map[string]any{
		"sales_order_id": f.noCurrencySOID, "item_id": widgetID, "qty": 1.0, "unit_price": 10.0, "line_total": 10.0,
	})

	f.invoiceID = create("CustomerInvoice", map[string]any{
		"invoice_number": "INV-77", "sales_order_id": f.soID, "customer_id": f.customerID,
		"invoice_date": "2026-07-15", "currency_id": f.currencyID,
		"status_id": statusID("customer_invoice_status", "draft"), "total": 500.0,
	})

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	f.mux = mux
	return f
}

// entityDefLookup returns a published-Definition getter bound to db —
// the same unmarshal dance ledger_hooks_test does, shared here.
func entityDefLookup(t *testing.T, db *sql.DB) func(string) *entity.Definition {
	t.Helper()
	repo := data.NewEntityDefinitionRepo(db)
	return func(entityType string) *entity.Definition {
		v, err := repo.GetPublished(context.Background(), entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s definition: %v", entityType, err)
		}
		return d
	}
}

func (f *ublFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("GET", path, f.tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

// validateUBLAgainstXSD validates real handler output against the
// vendored OASIS UBL 2.1 maindoc schema (skips without xmllint locally;
// CI installs libxml2-utils — same convention as SAF-T's twin helper).
func validateUBLAgainstXSD(t *testing.T, schema string, payload []byte) {
	t.Helper()
	if _, err := exec.LookPath("xmllint"); err != nil {
		t.Skip("xmllint not installed; XSD validation runs in CI")
	}
	path := filepath.Join(t.TempDir(), "doc.xml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write temp document: %v", err)
	}
	out, err := exec.Command("xmllint", "--noout",
		"--schema", filepath.Join("..", "kernel", "ubl", "testdata", "xsd", "maindoc", schema), path).CombinedOutput()
	if err != nil {
		t.Fatalf("handler output does not validate against %s: %v\n%s\n---\n%s", schema, err, out, payload)
	}
}

// The ubl package's marshal-side structs use literal cbc:/cac: prefixed
// tags (see its doc comment), which Go's unmarshaler does not match
// back against namespaced elements — so the tests read documents
// through these parse-side twins, which match by local name in any
// namespace. That asymmetry is confined to tests; clients consume the
// XML with real namespace-aware parsers (as xmllint's XSD pass proves).
type ublTestParty struct {
	Name  string `xml:"PartyName>Name"`
	TaxID string `xml:"PartyTaxScheme>CompanyID"`
}

type ublTestAmount struct {
	Value      string `xml:",chardata"`
	CurrencyID string `xml:"currencyID,attr"`
}

type ublTestOrderLine struct {
	ID    string        `xml:"LineItem>ID"`
	Qty   string        `xml:"LineItem>Quantity"`
	Total ublTestAmount `xml:"LineItem>LineExtensionAmount"`
	Price ublTestAmount `xml:"LineItem>Price>PriceAmount"`
	Name  string        `xml:"LineItem>Item>Name"`
	SKU   string        `xml:"LineItem>Item>SellersItemIdentification>ID"`
}

type ublTestOrder struct {
	ID                   string             `xml:"ID"`
	IssueDate            string             `xml:"IssueDate"`
	DocumentCurrencyCode string             `xml:"DocumentCurrencyCode"`
	Buyer                ublTestParty       `xml:"BuyerCustomerParty>Party"`
	Seller               ublTestParty       `xml:"SellerSupplierParty>Party"`
	Total                ublTestAmount      `xml:"AnticipatedMonetaryTotal>PayableAmount"`
	Lines                []ublTestOrderLine `xml:"OrderLine"`
}

type ublTestInvoiceLine struct {
	Amount ublTestAmount `xml:"LineExtensionAmount"`
	Name   string        `xml:"Item>Name"`
}

type ublTestInvoice struct {
	ID                   string               `xml:"ID"`
	IssueDate            string               `xml:"IssueDate"`
	DocumentCurrencyCode string               `xml:"DocumentCurrencyCode"`
	OrderRef             string               `xml:"OrderReference>ID"`
	Supplier             ublTestParty         `xml:"AccountingSupplierParty>Party"`
	Customer             ublTestParty         `xml:"AccountingCustomerParty>Party"`
	Total                ublTestAmount        `xml:"LegalMonetaryTotal>PayableAmount"`
	Lines                []ublTestInvoiceLine `xml:"InvoiceLine"`
}

func TestUBLExport_PurchaseOrder(t *testing.T) {
	f := setupUBLTenant(t)
	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("Content-Type = %q, want application/xml", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="PO-1042.xml"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	var doc ublTestOrder
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not parseable UBL XML: %v\n%s", err, rec.Body.String())
	}
	if doc.ID != "PO-1042" || doc.IssueDate != "2026-07-01" || doc.DocumentCurrencyCode != "USD" {
		t.Errorf("document header wrong: %+v", doc)
	}
	// On a purchase the tenant buys and the vendor sells — including the
	// vendor's XML-metacharacter name surviving the round trip.
	if doc.Seller.Name != "Supplier & Sons <LLC>" {
		t.Errorf("seller = %q, want the vendor Party", doc.Seller.Name)
	}
	if doc.Seller.TaxID != "S-999" {
		t.Errorf("seller tax id = %q, want S-999", doc.Seller.TaxID)
	}
	if doc.Buyer.Name == "" || doc.Buyer.Name == "Supplier & Sons <LLC>" {
		t.Errorf("buyer = %q, want the tenant", doc.Buyer.Name)
	}
	if len(doc.Lines) != 2 {
		t.Fatalf("expected 2 order lines, got %d", len(doc.Lines))
	}
	// Sequence, not set membership: lines must come out in the entry
	// order the operator keyed them (created_at, id) — an earlier draft
	// sorted by record UUID and shuffled them half the time, which an
	// order-insensitive map assertion here failed to catch (independent
	// review finding).
	if doc.Lines[0].SKU != "SKU-1" || doc.Lines[0].Total.Value != "125.00" ||
		doc.Lines[1].SKU != "SKU-2" || doc.Lines[1].Total.Value != "20.00" {
		t.Errorf("line sequence wrong: %+v", doc.Lines)
	}
	if doc.Total.Value != "145.00" || doc.Total.CurrencyID != "USD" {
		t.Errorf("total = %+v, want 145.00 USD", doc.Total)
	}

	validateUBLAgainstXSD(t, "UBL-Order-2.1.xsd", rec.Body.Bytes())

	// The export is a disclosure and must land on the audit trail
	// (ADR-0001 §14), attributed to the dev-auth human actor.
	var action, actorType, actorID string
	var diff []byte
	if err := f.db.QueryRow(`SELECT action, actor_type, actor_id, diff FROM audit_log WHERE entity_type = 'PurchaseOrder' AND action = 'export'`).
		Scan(&action, &actorType, &actorID, &diff); err != nil {
		t.Fatalf("expected a PurchaseOrder export audit row: %v", err)
	}
	if actorType != "human" || actorID != "farshid" {
		t.Errorf("audit actor wrong: %s/%s", actorType, actorID)
	}
	var d map[string]any
	if err := json.Unmarshal(diff, &d); err != nil || d["format"] != "ubl-2.1-order" || d["number"] != "PO-1042" {
		t.Errorf("audit diff wrong: %s (err %v)", diff, err)
	}
}

func TestUBLExport_SalesOrder_RolesFlip(t *testing.T) {
	f := setupUBLTenant(t)
	rec := f.get(t, "/export/SalesOrder/"+f.soID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc ublTestOrder
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// On a sale the customer buys and the tenant sells — the exact
	// mirror of the PurchaseOrder assertions.
	if doc.Buyer.Name != "Customer GmbH" {
		t.Errorf("buyer = %q, want Customer GmbH", doc.Buyer.Name)
	}
	if doc.Seller.Name == "" || doc.Seller.Name == "Customer GmbH" {
		t.Errorf("seller = %q, want the tenant", doc.Seller.Name)
	}
	if doc.ID != "SO-2001" || len(doc.Lines) != 1 {
		t.Errorf("document wrong: id=%q lines=%d", doc.ID, len(doc.Lines))
	}
	validateUBLAgainstXSD(t, "UBL-Order-2.1.xsd", rec.Body.Bytes())
}

func TestUBLExport_CustomerInvoice(t *testing.T) {
	f := setupUBLTenant(t)
	rec := f.get(t, "/export/CustomerInvoice/"+f.invoiceID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="INV-77.xml"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	var doc ublTestInvoice
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.ID != "INV-77" || doc.IssueDate != "2026-07-15" || doc.DocumentCurrencyCode != "USD" {
		t.Errorf("header wrong: %+v", doc)
	}
	if doc.OrderRef != "SO-2001" {
		t.Errorf("OrderReference = %q, want SO-2001", doc.OrderRef)
	}
	if doc.Customer.Name != "Customer GmbH" {
		t.Errorf("customer = %q", doc.Customer.Name)
	}
	if doc.Total.Value != "500.00" || doc.Total.CurrencyID != "USD" {
		t.Errorf("total = %+v", doc.Total)
	}
	// The single summary line (CustomerInvoice has no line entity yet —
	// see the ubl package comment).
	if len(doc.Lines) != 1 || doc.Lines[0].Amount.Value != "500.00" {
		t.Errorf("summary line wrong: %+v", doc.Lines)
	}
	if !strings.Contains(doc.Lines[0].Name, "SO-2001") {
		t.Errorf("summary line %q should reference the sales order", doc.Lines[0].Name)
	}
	validateUBLAgainstXSD(t, "UBL-Invoice-2.1.xsd", rec.Body.Bytes())
}

func TestUBLExport_RequiresAuth(t *testing.T) {
	f := setupUBLTenant(t)
	req := httptest.NewRequest("GET", "/export/PurchaseOrder/"+f.poID+"/ubl", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", rec.Code)
	}
}

func TestUBLExport_ErrorPaths(t *testing.T) {
	f := setupUBLTenant(t)
	for name, tc := range map[string]struct {
		path string
		code int
		want string
	}{
		"unsupported entity type": {"/export/Party/" + f.vendorID + "/ubl", http.StatusNotFound, "not available"},
		"unknown record":          {"/export/PurchaseOrder/00000000-0000-0000-0000-000000000000/ubl", http.StatusNotFound, "not found"},
		"malformed record id":     {"/export/PurchaseOrder/not-a-uuid/ubl", http.StatusBadRequest, "invalid record id"},
		"order without lines":     {"/export/PurchaseOrder/" + f.noLinesPOID + "/ubl", http.StatusBadRequest, "no lines"},
		"missing currency":        {"/export/SalesOrder/" + f.noCurrencySOID + "/ubl", http.StatusBadRequest, "no currency"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.get(t, tc.path)
			if rec.Code != tc.code {
				t.Fatalf("expected %d, got %d: %s", tc.code, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("error body %q should mention %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// TestUBLExport_DanglingReferences: a counterparty or currency deleted
// after the document was created is the record's own defect — a clean
// 400 naming the missing reference, never a 500 or invalid XML. A
// deleted Item, by contrast, only degrades the line's name: the line's
// own quantity and amounts are still true (ublLines's documented
// choice).
func TestUBLExport_DanglingReferences(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)

	// Deleting the vendor leaves the PO's vendor_id dangling.
	if err := engine.Delete(ctx, defs("Party"), f.vendorID, humanActor()); err != nil {
		t.Fatalf("delete vendor Party: %v", err)
	}
	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Fatalf("dangling vendor: expected 400 naming the missing Party, got %d: %s", rec.Code, rec.Body.String())
	}

	// Deleting the currency: same contract on the SalesOrder.
	if err := engine.Delete(ctx, defs("Currency"), f.currencyID, humanActor()); err != nil {
		t.Fatalf("delete Currency: %v", err)
	}
	rec = f.get(t, "/export/SalesOrder/"+f.soID+"/ubl")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Currency") {
		t.Fatalf("dangling currency: expected 400 naming the Currency, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUBLExport_DanglingItemDegrades: a deleted Item only degrades the
// line's name/sku — the line's own quantity and amounts are still true
// — and the degraded document still validates (ItemType's children are
// all optional in the XSD).
func TestUBLExport_DanglingItemDegrades(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)
	if err := engine.Delete(ctx, defs("Item"), f.widgetID, humanActor()); err != nil {
		t.Fatalf("delete widget Item: %v", err)
	}

	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite dangling Item, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc ublTestOrder
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc.Lines) != 2 {
		t.Fatalf("expected both lines, got %d", len(doc.Lines))
	}
	if doc.Lines[0].Name != "" || doc.Lines[0].SKU != "" {
		t.Errorf("deleted item's line should have no name/sku, got %+v", doc.Lines[0])
	}
	if doc.Lines[0].Total.Value != "125.00" || doc.Lines[0].Qty != "10" {
		t.Errorf("deleted item's line must keep its own amounts: %+v", doc.Lines[0])
	}
	if doc.Lines[1].Name != "Gadget" {
		t.Errorf("intact item's line lost its name: %+v", doc.Lines[1])
	}
	validateUBLAgainstXSD(t, "UBL-Order-2.1.xsd", rec.Body.Bytes())
}

// TestUBLExport_InvoiceDanglingSalesOrderOmitsRef: the linked
// SalesOrder contributes only its number; a dangling reference omits
// the OrderReference rather than failing (the invoice's own data is
// complete without it).
func TestUBLExport_InvoiceDanglingSalesOrderOmitsRef(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)
	if err := engine.Delete(ctx, defs("SalesOrder"), f.soID, humanActor()); err != nil {
		t.Fatalf("delete SalesOrder: %v", err)
	}

	rec := f.get(t, "/export/CustomerInvoice/"+f.invoiceID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite dangling SalesOrder, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc ublTestInvoice
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.OrderRef != "" {
		t.Errorf("OrderReference should be omitted for a dangling sales order, got %q", doc.OrderRef)
	}
	if len(doc.Lines) != 1 || strings.Contains(doc.Lines[0].Name, "SO-") {
		t.Errorf("summary line should not name a sales order: %+v", doc.Lines)
	}
	validateUBLAgainstXSD(t, "UBL-Invoice-2.1.xsd", rec.Body.Bytes())
}

// TestUBLExport_InvoiceDanglingPartyAndCurrency: the invoice-side twins
// of TestUBLExport_DanglingReferences (buildUBLInvoice resolves the
// customer before the currency, so the order of deletions below
// exercises each branch separately).
func TestUBLExport_InvoiceDanglingPartyAndCurrency(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)

	if err := engine.Delete(ctx, defs("Currency"), f.currencyID, humanActor()); err != nil {
		t.Fatalf("delete Currency: %v", err)
	}
	rec := f.get(t, "/export/CustomerInvoice/"+f.invoiceID+"/ubl")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Currency") {
		t.Fatalf("dangling currency: expected 400 naming the Currency, got %d: %s", rec.Code, rec.Body.String())
	}

	if err := engine.Delete(ctx, defs("Party"), f.customerID, humanActor()); err != nil {
		t.Fatalf("delete customer Party: %v", err)
	}
	rec = f.get(t, "/export/CustomerInvoice/"+f.invoiceID+"/ubl")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Fatalf("dangling customer: expected 400 naming the missing Party, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUBLExport_EmptyCounterparty: a record whose party reference is
// simply absent (not dangling) gets the dedicated "no counterparty"
// 400. vendor_id is Required at create time, so the state is
// manufactured by stripping the key directly in SQL — defensive-branch
// coverage, not a reachable create path today.
func TestUBLExport_EmptyCounterparty(t *testing.T) {
	f := setupUBLTenant(t)
	if _, err := f.db.Exec(`UPDATE records SET data = data - 'vendor_id' WHERE id = $1`, f.poID); err != nil {
		t.Fatalf("strip vendor_id: %v", err)
	}
	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no counterparty") {
		t.Fatalf("expected 400 no-counterparty, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUBLExport_FieldRedactionRefused403: read permission alone is not
// enough — a field-level hide on any field the document reads must
// refuse the whole export. The guarded engine deletes hidden fields
// from records, so without this gate the document would render them as
// ""/0: a schema-valid file with silently false amounts (the
// independent review's blocker finding — it demonstrated a nameless
// supplier, zero-priced lines, and an intact 145.00 header total from
// exactly this hole).
func TestUBLExport_FieldRedactionRefused403(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	actor := humanActor()

	roleIDs := seedRBAC(t, f.db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "POLine", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "Item", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "Party", "can_read": true, "can_write": false},
			{"role": "clerk", "entity_type": "Currency", "can_read": true, "can_write": false},
		},
	)
	if _, err := engine.Create(ctx, foundation.FieldPermission(), map[string]any{
		"role_id": roleIDs["clerk"], "entity_type": "POLine", "field_name": "line_total", "hidden": true,
	}, actor); err != nil {
		t.Fatalf("create FieldPermission: %v", err)
	}

	req := newRequest("GET", "/export/PurchaseOrder/"+f.poID+"/ubl", f.tenantID, "user-clerk", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for redacted line_total, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "POLine.line_total") {
		t.Errorf("403 body should name the redacted field: %s", rec.Body.String())
	}
}

// TestUBLExport_TenantIsolation: tenant B asking for tenant A's record
// id must get a plain 404 from its own empty database — never tenant
// A's document (two genuinely separate tenant databases, ADR-0003; the
// same cross-tenant-leak proof TestExport_TenantIsolation established
// for the CSV export).
func TestUBLExport_TenantIsolation(t *testing.T) {
	f := setupUBLTenant(t)
	tenantB, dbB := newTestTenant(t, f.router)
	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, dbB, actor); err != nil {
		t.Fatalf("foundation.Publish tenant B: %v", err)
	}
	if err := purchasing.Publish(ctx, dbB, actor); err != nil {
		t.Fatalf("purchasing.Publish tenant B: %v", err)
	}

	req := newRequest("GET", "/export/PurchaseOrder/"+f.poID+"/ubl", tenantB, "farshid", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant A's id in tenant B, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Supplier") || strings.Contains(rec.Body.String(), "PO-1042") {
		t.Fatalf("tenant B's response leaked tenant A's data: %s", rec.Body.String())
	}
}

// TestUBLExport_RBAC_WholeDocumentGate: the document disclose more
// entity types than the one in the URL — a user who can read
// PurchaseOrder but not Item must NOT get a document that embeds item
// names (same whole-file gate as the SAF-T export).
func TestUBLExport_RBAC_WholeDocumentGate(t *testing.T) {
	f := setupUBLTenant(t)
	seedRBAC(t, f.db,
		map[string][]string{
			"po-only":           {"user-po-only"},
			authz.AdminRoleCode: {"user-admin"},
		},
		[]map[string]any{
			{"role": "po-only", "entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
			{"role": "po-only", "entity_type": "POLine", "can_read": true, "can_write": false},
			{"role": "po-only", "entity_type": "Party", "can_read": true, "can_write": false},
			{"role": "po-only", "entity_type": "Currency", "can_read": true, "can_write": false},
			// No Item permission: with rules present for the type, absence
			// of a grant is a deny (ADR-0006).
			{"role": "po-only", "entity_type": "Item", "can_read": false, "can_write": false},
		},
	)

	req := newRequest("GET", "/export/PurchaseOrder/"+f.poID+"/ubl", f.tenantID, "user-po-only", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user without Item read, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Item") {
		t.Errorf("403 body should name the missing entity type: %s", rec.Body.String())
	}

	// The admin bypass still exports fine — proves the gate is the
	// permission, not the route.
	adminReq := newRequest("GET", "/export/PurchaseOrder/"+f.poID+"/ubl", f.tenantID, "user-admin", nil)
	adminRec := httptest.NewRecorder()
	f.mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin export: expected 200, got %d: %s", adminRec.Code, adminRec.Body.String())
	}
}

// TestUBLInputError_Error: ublInputError's Error() method carries the
// record-state message verbatim. writeUBLError now calls Error()
// rather than reaching into the unexported field directly (a one-line
// tidy alongside this fix — same behavior, but it means the method is
// genuinely load-bearing instead of dead code a coverage test merely
// pins). TestUBLExport_ErrorPaths's "order without lines"/"missing
// currency" cases already exercise that call site end-to-end; this
// test isolates the type itself, plus the %v path a log line would
// take, which no existing test reaches (uc-infra#110).
func TestUBLInputError_Error(t *testing.T) {
	err := ublInputError{msg: "missing currency"}
	if got := err.Error(); got != "missing currency" {
		t.Fatalf("Error() = %q, want %q", got, "missing currency")
	}
	if got := fmt.Sprintf("%v", err); got != "missing currency" {
		t.Fatalf("%%v of ublInputError = %q, want %q", got, "missing currency")
	}
}

// TestUBLExport_OwnOrganizationSetsTenantTaxID is uc-infra#119's core
// acceptance case: a Party holding the own_organization PartyRole
// (uc-infra#63) flows its tax_id into the tenant's own side of every
// document type — the buyer on a PurchaseOrder, the seller on a
// SalesOrder, and the supplier on a CustomerInvoice — the same
// role→Party resolution saftCompanyProfile already proved for SAF-T,
// now shared via ownOrganizationParty. Name resolution is deliberately
// untouched: the org Party's own "name" never overrides the tenant
// display name (see ublTenantParty's doc comment), so this only
// asserts TaxID. Also asserts each document's counterparty TaxID is
// unchanged (still the vendor's/customer's own S-999/C-111, not the
// tenant's TENANT-TAX-1) — this fixture is the first one where BOTH
// sides of a document carry a PartyTaxScheme, so a tax-id cross-wire
// between the two sides would otherwise go uncaught (independent
// review finding).
func TestUBLExport_OwnOrganizationSetsTenantTaxID(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)
	actor := humanActor()

	org, err := engine.Create(ctx, defs("Party"), map[string]any{
		"party_type": "organization", "name": "Demo Organization", "tax_id": "TENANT-TAX-1",
	}, actor)
	if err != nil {
		t.Fatalf("create own-organization Party: %v", err)
	}
	if _, err := engine.Create(ctx, defs("PartyRole"), map[string]any{
		"party_id": org.ID, "role_type": "own_organization",
	}, actor); err != nil {
		t.Fatalf("create own_organization PartyRole: %v", err)
	}

	poRec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if poRec.Code != http.StatusOK {
		t.Fatalf("PurchaseOrder export: expected 200, got %d: %s", poRec.Code, poRec.Body.String())
	}
	var po ublTestOrder
	if err := xml.Unmarshal(poRec.Body.Bytes(), &po); err != nil {
		t.Fatalf("PurchaseOrder response is not parseable UBL XML: %v\n%s", err, poRec.Body.String())
	}
	if po.Buyer.TaxID != "TENANT-TAX-1" {
		t.Errorf("PurchaseOrder buyer (tenant) TaxID = %q, want TENANT-TAX-1", po.Buyer.TaxID)
	}
	if po.Seller.TaxID != "S-999" {
		t.Errorf("PurchaseOrder seller (vendor) TaxID = %q, want S-999 (unchanged by the tenant's own profile)", po.Seller.TaxID)
	}

	soRec := f.get(t, "/export/SalesOrder/"+f.soID+"/ubl")
	if soRec.Code != http.StatusOK {
		t.Fatalf("SalesOrder export: expected 200, got %d: %s", soRec.Code, soRec.Body.String())
	}
	var so ublTestOrder
	if err := xml.Unmarshal(soRec.Body.Bytes(), &so); err != nil {
		t.Fatalf("SalesOrder response is not parseable UBL XML: %v\n%s", err, soRec.Body.String())
	}
	if so.Seller.TaxID != "TENANT-TAX-1" {
		t.Errorf("SalesOrder seller (tenant) TaxID = %q, want TENANT-TAX-1", so.Seller.TaxID)
	}
	if so.Buyer.TaxID != "C-111" {
		t.Errorf("SalesOrder buyer (customer) TaxID = %q, want C-111 (unchanged by the tenant's own profile)", so.Buyer.TaxID)
	}

	invRec := f.get(t, "/export/CustomerInvoice/"+f.invoiceID+"/ubl")
	if invRec.Code != http.StatusOK {
		t.Fatalf("CustomerInvoice export: expected 200, got %d: %s", invRec.Code, invRec.Body.String())
	}
	var inv ublTestInvoice
	if err := xml.Unmarshal(invRec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("CustomerInvoice response is not parseable UBL XML: %v\n%s", err, invRec.Body.String())
	}
	if inv.Supplier.TaxID != "TENANT-TAX-1" {
		t.Errorf("CustomerInvoice supplier (tenant) TaxID = %q, want TENANT-TAX-1", inv.Supplier.TaxID)
	}
	if inv.Customer.TaxID != "C-111" {
		t.Errorf("CustomerInvoice customer TaxID = %q, want C-111 (unchanged by the tenant's own profile)", inv.Customer.TaxID)
	}

	validateUBLAgainstXSD(t, "UBL-Order-2.1.xsd", poRec.Body.Bytes())
	validateUBLAgainstXSD(t, "UBL-Invoice-2.1.xsd", invRec.Body.Bytes())
}

// TestUBLExport_NoOwnOrganizationLeavesTenantTaxIDEmpty is the
// regression case for every tenant that predates uc-infra#119 (and
// #63): no Party holds the own_organization role — setupUBLTenant's
// fixture never creates one — so the tenant's own PartyTaxScheme stays
// omitted entirely, the exact pre-#119 behavior, never a false empty
// CompanyID element.
func TestUBLExport_NoOwnOrganizationLeavesTenantTaxIDEmpty(t *testing.T) {
	f := setupUBLTenant(t)

	poRec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if poRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", poRec.Code, poRec.Body.String())
	}
	var po ublTestOrder
	if err := xml.Unmarshal(poRec.Body.Bytes(), &po); err != nil {
		t.Fatalf("response is not parseable UBL XML: %v\n%s", err, poRec.Body.String())
	}
	if po.Buyer.TaxID != "" {
		t.Errorf("buyer (tenant) TaxID = %q, want empty (no own_organization Party configured)", po.Buyer.TaxID)
	}
	if po.Buyer.Name == "" {
		t.Errorf("tenant party should still carry a display name, got empty")
	}
	// The vendor (seller) side legitimately has its own PartyTaxScheme
	// (S-999, from setupUBLTenant's fixture) — xmlParty's own omitempty
	// behavior for a zero-value TaxID is already covered by ubl_test.go
	// at the package level; po.Buyer.TaxID == "" above is the correct,
	// element-scoped assertion for the tenant side specifically.
}

// TestUBLExport_AmbiguousOwnOrganizationLeavesTenantTaxIDEmpty is UBL's
// own instance of the ambiguity-degrade case
// TestSAFTExport_AmbiguousOwnOrganizationFallsBackToNA already proves
// for the shared ownOrganizationParty helper: two Parties both holding
// own_organization is a data-quality problem this export has no
// business guessing at, so it must degrade exactly like the zero-match
// case, never pick one — asserted here at the UBL layer specifically
// (not just inherited from the shared function's own SAF-T coverage),
// since UBL's degrade is a silently-omitted element rather than SAF-T's
// visible NA marker (independent review finding, uc-infra#119).
func TestUBLExport_AmbiguousOwnOrganizationLeavesTenantTaxIDEmpty(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)
	actor := humanActor()

	for i, taxID := range []string{"AMBIG-TAX-1", "AMBIG-TAX-2"} {
		org, err := engine.Create(ctx, defs("Party"), map[string]any{
			"party_type": "organization", "name": "Demo Organization", "tax_id": taxID,
		}, actor)
		if err != nil {
			t.Fatalf("create own-organization Party %d: %v", i, err)
		}
		if _, err := engine.Create(ctx, defs("PartyRole"), map[string]any{
			"party_id": org.ID, "role_type": "own_organization",
		}, actor); err != nil {
			t.Fatalf("create own_organization PartyRole %d: %v", i, err)
		}
	}

	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var po ublTestOrder
	if err := xml.Unmarshal(rec.Body.Bytes(), &po); err != nil {
		t.Fatalf("response is not parseable UBL XML: %v\n%s", err, rec.Body.String())
	}
	if po.Buyer.TaxID != "" {
		t.Errorf("buyer (tenant) TaxID = %q, want empty (ambiguous: two own_organization Parties)", po.Buyer.TaxID)
	}
}

// TestUBLExport_OwnOrganizationBlankTaxIDOmitsPartyTaxScheme covers the
// state ublTenantParty's own doc comment doesn't mention: an
// own_organization Party that resolves unambiguously but was never
// given a tax_id (the single most likely real-world misconfiguration —
// the operator marked their own organization but left the statutory
// field blank). ownOrganizationParty returns ok=true here, unlike the
// zero/ambiguous/dangling cases; the empty string still flows through
// to TaxID and xmlParty still omits PartyTaxScheme for it, producing
// the same schema-valid document as no profile at all (independent
// review finding, uc-infra#119).
func TestUBLExport_OwnOrganizationBlankTaxIDOmitsPartyTaxScheme(t *testing.T) {
	f := setupUBLTenant(t)
	ctx := context.Background()
	engine := crud.NewEngine(f.db)
	defs := entityDefLookup(t, f.db)
	actor := humanActor()

	org, err := engine.Create(ctx, defs("Party"), map[string]any{
		"party_type": "organization", "name": "Demo Organization",
	}, actor)
	if err != nil {
		t.Fatalf("create own-organization Party: %v", err)
	}
	if _, err := engine.Create(ctx, defs("PartyRole"), map[string]any{
		"party_id": org.ID, "role_type": "own_organization",
	}, actor); err != nil {
		t.Fatalf("create own_organization PartyRole: %v", err)
	}

	rec := f.get(t, "/export/PurchaseOrder/"+f.poID+"/ubl")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var po ublTestOrder
	if err := xml.Unmarshal(rec.Body.Bytes(), &po); err != nil {
		t.Fatalf("response is not parseable UBL XML: %v\n%s", err, rec.Body.String())
	}
	if po.Buyer.TaxID != "" {
		t.Errorf("buyer (tenant) TaxID = %q, want empty (own_organization Party configured with no tax_id)", po.Buyer.TaxID)
	}
	validateUBLAgainstXSD(t, "UBL-Order-2.1.xsd", rec.Body.Bytes())
}

// TestUBLExport_OwnOrganizationRoleReadDeniedRefused403 proves
// ublDocReads' new PartyRole declaration (uc-infra#119) actually gates,
// for all three exportable document types (not just PurchaseOrder — a
// typo in either of the other two entries would otherwise pass CI
// unnoticed, independent review finding): an actor without PartyRole
// read access must be refused the whole document, not silently served
// one with the tenant's own tax registration quietly dropped (the
// "silently dropped" failure shape, same as saftFieldReads' own
// reasoning for its PartyRole entry) — the class of gap #67's
// independent review found on this export originally.
func TestUBLExport_OwnOrganizationRoleReadDeniedRefused403(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		extraPerms []map[string]any
	}{
		{
			name: "PurchaseOrder",
			extraPerms: []map[string]any{
				{"entity_type": "PurchaseOrder", "can_read": true, "can_write": false},
				{"entity_type": "POLine", "can_read": true, "can_write": false},
				{"entity_type": "Item", "can_read": true, "can_write": false},
			},
		},
		{
			name: "SalesOrder",
			extraPerms: []map[string]any{
				{"entity_type": "SalesOrder", "can_read": true, "can_write": false},
				{"entity_type": "SOLine", "can_read": true, "can_write": false},
				{"entity_type": "Item", "can_read": true, "can_write": false},
			},
		},
		{
			name: "CustomerInvoice",
			extraPerms: []map[string]any{
				{"entity_type": "CustomerInvoice", "can_read": true, "can_write": false},
				{"entity_type": "SalesOrder", "can_read": true, "can_write": false},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupUBLTenant(t)
			role := "role-" + strings.ToLower(c.name)
			userID := "user-" + strings.ToLower(c.name)
			perms := append([]map[string]any{
				{"entity_type": "Party", "can_read": true, "can_write": false},
				{"entity_type": "Currency", "can_read": true, "can_write": false},
				// No PartyRole permission: with rules present for the
				// type, absence of a grant is a deny (ADR-0006).
				{"entity_type": "PartyRole", "can_read": false, "can_write": false},
			}, c.extraPerms...)
			for i := range perms {
				perms[i]["role"] = role
			}
			seedRBAC(t, f.db, map[string][]string{role: {userID}}, perms)

			var recordID string
			switch c.name {
			case "PurchaseOrder":
				recordID = f.poID
			case "SalesOrder":
				recordID = f.soID
			case "CustomerInvoice":
				recordID = f.invoiceID
			}
			req := newRequest("GET", "/export/"+c.name+"/"+recordID+"/ubl", f.tenantID, userID, nil)
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for user without PartyRole read, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "PartyRole") {
				t.Errorf("403 body should name the missing entity type: %s", rec.Body.String())
			}
		})
	}
}
