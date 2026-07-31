package ubl

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// orderInput is a representative two-line order exercising both parties
// carrying tax ids, sku identification, and fractional quantities.
func orderInput() OrderInput {
	return OrderInput{
		ID:           "PO-1042",
		IssueDate:    "2026-07-01",
		CurrencyCode: "USD",
		MinorUnit:    2,
		Buyer:        Party{Name: "Demo Organization"},
		Seller:       Party{Name: "Supplier One", TaxID: "S-999"},
		Lines: []Line{
			{ID: "line-1", ItemSKU: "SKU-1", ItemName: "Widget", Qty: 10, UnitPrice: 12.5, LineTotal: 125},
			{ID: "line-2", ItemSKU: "SKU-2", ItemName: "Gadget", Qty: 2.5, UnitPrice: 8, LineTotal: 20},
		},
		Total: 145,
	}
}

func invoiceInput() InvoiceInput {
	return InvoiceInput{
		ID:           "INV-77",
		IssueDate:    "2026-07-15",
		OrderRef:     "SO-2001",
		CurrencyCode: "USD",
		MinorUnit:    2,
		Supplier:     Party{Name: "Demo Organization"},
		Customer:     Party{Name: "Customer One", TaxID: "C-123"},
		Total:        145,
	}
}

// validateAgainstXSD runs xmllint against the vendored UBL 2.1 maindoc
// schema for the document kind. Skips when xmllint isn't installed
// locally; CI installs libxml2-utils so the validation always runs
// there — same convention as internal/kernel/saft.
func validateAgainstXSD(t *testing.T, schema string, payload []byte) {
	t.Helper()
	if _, err := exec.LookPath("xmllint"); err != nil {
		t.Skip("xmllint not installed; XSD validation runs in CI")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.xml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write temp document: %v", err)
	}
	out, err := exec.Command("xmllint", "--noout", "--schema", filepath.Join("testdata", "xsd", "maindoc", schema), path).CombinedOutput()
	if err != nil {
		t.Fatalf("generated document does not validate against %s: %v\n%s\n---\n%s", schema, err, out, payload)
	}
}

func TestBuildOrder_ValidatesAgainstXSD(t *testing.T) {
	doc, err := BuildOrder(orderInput())
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	payload, err := MarshalOrder(doc)
	if err != nil {
		t.Fatalf("MarshalOrder: %v", err)
	}
	validateAgainstXSD(t, "UBL-Order-2.1.xsd", payload)
}

func TestBuildInvoice_ValidatesAgainstXSD(t *testing.T) {
	doc, err := BuildInvoice(invoiceInput())
	if err != nil {
		t.Fatalf("BuildInvoice: %v", err)
	}
	payload, err := MarshalInvoice(doc)
	if err != nil {
		t.Fatalf("MarshalInvoice: %v", err)
	}
	validateAgainstXSD(t, "UBL-Invoice-2.1.xsd", payload)
}

// The minimal shapes — no tax ids, no sku, no order reference — must
// also validate: every optional element has to be genuinely optional in
// the emitted XML, not present-but-empty.
func TestBuildOrder_MinimalValidatesAgainstXSD(t *testing.T) {
	in := OrderInput{
		ID:           "PO-1",
		IssueDate:    "2026-07-01",
		CurrencyCode: "USD",
		MinorUnit:    2,
		Buyer:        Party{Name: "Demo Organization"},
		Seller:       Party{Name: "Supplier One"},
		Lines:        []Line{{ID: "l1", ItemName: "Widget", Qty: 1, UnitPrice: 1, LineTotal: 1}},
	}
	doc, err := BuildOrder(in)
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	payload, err := MarshalOrder(doc)
	if err != nil {
		t.Fatalf("MarshalOrder: %v", err)
	}
	validateAgainstXSD(t, "UBL-Order-2.1.xsd", payload)
}

func TestBuildInvoice_MinimalValidatesAgainstXSD(t *testing.T) {
	in := InvoiceInput{
		ID:           "INV-1",
		IssueDate:    "2026-07-15",
		CurrencyCode: "USD",
		MinorUnit:    2,
		Supplier:     Party{Name: "Demo Organization"},
		Customer:     Party{Name: "Customer One"},
		Total:        10,
	}
	doc, err := BuildInvoice(in)
	if err != nil {
		t.Fatalf("BuildInvoice: %v", err)
	}
	payload, err := MarshalInvoice(doc)
	if err != nil {
		t.Fatalf("MarshalInvoice: %v", err)
	}
	validateAgainstXSD(t, "UBL-Invoice-2.1.xsd", payload)
}

func TestBuildOrder_Golden(t *testing.T) {
	doc, err := BuildOrder(orderInput())
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	payload, err := MarshalOrder(doc)
	if err != nil {
		t.Fatalf("MarshalOrder: %v", err)
	}
	assertGolden(t, "order_golden.xml", payload)
}

func TestBuildInvoice_Golden(t *testing.T) {
	doc, err := BuildInvoice(invoiceInput())
	if err != nil {
		t.Fatalf("BuildInvoice: %v", err)
	}
	payload, err := MarshalInvoice(doc)
	if err != nil {
		t.Fatalf("MarshalInvoice: %v", err)
	}
	assertGolden(t, "invoice_golden.xml", payload)
}

// assertGolden compares payload to the checked-in golden file;
// UPDATE_GOLDEN=1 rewrites it (same update flow as other golden tests
// in this repo).
func assertGolden(t *testing.T, name string, payload []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_GOLDEN=1 to create): %v", name, err)
	}
	if string(want) != string(payload) {
		t.Fatalf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, payload)
	}
}

func TestBuildOrder_Errors(t *testing.T) {
	base := orderInput()

	for name, mutate := range map[string]func(*OrderInput){
		"empty id":       func(in *OrderInput) { in.ID = "  " },
		"bad issue date": func(in *OrderInput) { in.IssueDate = "01/07/2026" },
		"empty currency": func(in *OrderInput) { in.CurrencyCode = "" },
		"bad currency":   func(in *OrderInput) { in.CurrencyCode = "US" },
		"no lines":       func(in *OrderInput) { in.Lines = nil },
		"negative minor": func(in *OrderInput) { in.MinorUnit = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Lines = append([]Line(nil), base.Lines...)
			mutate(&in)
			if _, err := BuildOrder(in); err == nil {
				t.Fatalf("BuildOrder accepted invalid input (%s)", name)
			}
		})
	}
}

func TestBuildInvoice_Errors(t *testing.T) {
	base := invoiceInput()
	for name, mutate := range map[string]func(*InvoiceInput){
		"empty id":       func(in *InvoiceInput) { in.ID = "" },
		"bad issue date": func(in *InvoiceInput) { in.IssueDate = "2026-13-40" },
		"bad currency":   func(in *InvoiceInput) { in.CurrencyCode = "DOLLARS" },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := BuildInvoice(in); err == nil {
				t.Fatalf("BuildInvoice accepted invalid input (%s)", name)
			}
		})
	}
}

// XML metacharacters in names/ids must be escaped, and the escaped
// document must still validate — the classic injection edge case for a
// serializer fed user-entered names.
func TestBuildOrder_EscapesMetacharacters(t *testing.T) {
	in := orderInput()
	in.Buyer.Name = `Demo <&> "Org"`
	in.Seller.Name = "Supplier <script>alert(1)</script>"
	in.Lines[0].ItemName = "Widget & Sons <XL>"

	doc, err := BuildOrder(in)
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	payload, err := MarshalOrder(doc)
	if err != nil {
		t.Fatalf("MarshalOrder: %v", err)
	}
	if strings.Contains(string(payload), "<script>") {
		t.Fatalf("unescaped markup in output:\n%s", payload)
	}
	var roundTrip Order
	if err := xml.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("escaped output does not re-parse: %v", err)
	}
	validateAgainstXSD(t, "UBL-Order-2.1.xsd", payload)
}

// Amount formatting follows the currency's minor unit; quantities never
// carry artificial decimals.
func TestAmountAndQuantityFormatting(t *testing.T) {
	in := orderInput()
	in.CurrencyCode = "BHD" // three minor units
	in.MinorUnit = 3
	in.Lines = in.Lines[:1]
	in.Lines[0].UnitPrice = 12.5
	in.Lines[0].LineTotal = 125
	in.Lines[0].Qty = 10
	in.Total = 125

	doc, err := BuildOrder(in)
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	li := doc.OrderLines[0].LineItem
	if got := li.Price.PriceAmount.Value; got != "12.500" {
		t.Errorf("unit price = %q, want 12.500", got)
	}
	if got := li.LineExtensionAmount.Value; got != "125.000" {
		t.Errorf("line total = %q, want 125.000", got)
	}
	if got := li.Quantity.Value; got != "10" {
		t.Errorf("qty = %q, want 10 (no artificial decimals)", got)
	}
	if got := doc.AnticipatedMonetaryTotal.PayableAmount.CurrencyID; got != "BHD" {
		t.Errorf("currencyID = %q, want BHD", got)
	}

	zero := orderInput()
	zero.CurrencyCode = "JPY"
	zero.MinorUnit = 0
	docJPY, err := BuildOrder(zero)
	if err != nil {
		t.Fatalf("BuildOrder JPY: %v", err)
	}
	if got := docJPY.AnticipatedMonetaryTotal.PayableAmount.Value; got != "145" {
		t.Errorf("JPY total = %q, want 145", got)
	}
}

// TestAmountFloatBoundaryArtifact pins the documented float-storage
// artifact (see amount's own comment): a decimal half-unit boundary
// value stored as a binary double formats to the NEAREST representable
// value, which for 2.675 is DOWN. When FieldNumber amounts move to an
// integer-minor-units money type, this test is the one that must
// consciously change.
func TestAmountFloatBoundaryArtifact(t *testing.T) {
	for v, want := range map[float64]string{
		2.675:  "2.67", // binary double is 2.67499…, formats down
		1.005:  "1.00", // same artifact
		2.685:  "2.69", // binary double is 2.68500000…1, formats up
		125.5:  "125.50",
		0.1:    "0.10",
		145.00: "145.00",
	} {
		if got := amount(v, 2, "USD").Value; got != want {
			t.Errorf("amount(%v) = %q, want %q", v, got, want)
		}
	}
}

// Currency codes normalize to upper case, and the mandatory currencyID
// attribute appears on every amount in the document.
func TestCurrencyNormalizationAndAttribution(t *testing.T) {
	in := invoiceInput()
	in.CurrencyCode = " usd "
	doc, err := BuildInvoice(in)
	if err != nil {
		t.Fatalf("BuildInvoice: %v", err)
	}
	if doc.DocumentCurrencyCode != "USD" {
		t.Errorf("DocumentCurrencyCode = %q, want USD", doc.DocumentCurrencyCode)
	}
	if doc.LegalMonetaryTotal.PayableAmount.CurrencyID != "USD" {
		t.Errorf("total currencyID = %q, want USD", doc.LegalMonetaryTotal.PayableAmount.CurrencyID)
	}
	if doc.InvoiceLines[0].LineExtensionAmount.CurrencyID != "USD" {
		t.Errorf("line currencyID = %q, want USD", doc.InvoiceLines[0].LineExtensionAmount.CurrencyID)
	}
}

// The invoice's summary line mirrors the total and names the order it
// bills — the honest-to-stored-data shape the package comment explains.
func TestBuildInvoice_SummaryLine(t *testing.T) {
	doc, err := BuildInvoice(invoiceInput())
	if err != nil {
		t.Fatalf("BuildInvoice: %v", err)
	}
	if len(doc.InvoiceLines) != 1 {
		t.Fatalf("expected exactly one summary line, got %d", len(doc.InvoiceLines))
	}
	line := doc.InvoiceLines[0]
	if line.LineExtensionAmount.Value != doc.LegalMonetaryTotal.PayableAmount.Value {
		t.Errorf("summary line %q != total %q", line.LineExtensionAmount.Value, doc.LegalMonetaryTotal.PayableAmount.Value)
	}
	if !strings.Contains(line.Item.Name, "SO-2001") {
		t.Errorf("summary line item %q does not reference the sales order", line.Item.Name)
	}
	if doc.OrderReference == nil || doc.OrderReference.ID != "SO-2001" {
		t.Errorf("OrderReference = %+v, want SO-2001", doc.OrderReference)
	}

	noRef := invoiceInput()
	noRef.OrderRef = ""
	docNoRef, err := BuildInvoice(noRef)
	if err != nil {
		t.Fatalf("BuildInvoice without order ref: %v", err)
	}
	if docNoRef.OrderReference != nil {
		t.Errorf("OrderReference should be omitted when there is no order number")
	}
}
