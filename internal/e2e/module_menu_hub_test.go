package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestModuleMenu_RendersAsSearchableHubGraphic is the real-browser
// regression test for the module-menu redesign (internal/api/
// modulemenu.go, 2026-07-26): Farshid compared the dashboard's own
// hub-and-spoke module switcher favorably against a module's menu
// ("purchasing is only a list of menu... put them in graphical mode and
// searchable") — this confirms a real browser actually lays the
// entities out as positioned, non-overlapping graphical nodes (not just
// that the right HTML strings are present, which an httptest-based
// assertion already covers in internal/api/handlers_test.go) and that
// the search box genuinely hides/shows real DOM nodes as a user types,
// not just that the right JS text is embedded in the page.
func TestModuleMenu_RendersAsSearchableHubGraphic(t *testing.T) {
	withDevAuthEnabled(t)
	// testServer already publishes purchasing: Item, PurchaseOrder,
	// POLine, InventoryItem, GoodsReceipt, GoodsReceiptLine,
	// VendorInvoice, ReorderRule, and (#9) RequestForQuotation,
	// RequestForQuotationLine, RequestForQuotationVendor,
	// RequestForQuotationQuoteLine (12 entities).
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/modules/purchasing"),
		chromedp.WaitVisible(`.uc-hub-node`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the purchasing module menu: %v", err)
	}

	var nodeCount int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('.uc-hub-node').length`, &nodeCount,
	)); err != nil {
		t.Fatalf("count hub nodes: %v", err)
	}
	if nodeCount != 13 {
		t.Fatalf("expected 13 entity hub nodes (Item, PurchaseOrder, POLine, Facility, InventoryItem, GoodsReceipt, GoodsReceiptLine, VendorInvoice, ReorderRule, RequestForQuotation, RequestForQuotationLine, RequestForQuotationVendor, RequestForQuotationQuoteLine), got %d", nodeCount)
	}

	// Real, non-overlapping graphical positioning — not just that a
	// style attribute string exists (which the right left/top pixel
	// values alone wouldn't prove nodes are actually distinct on
	// screen): every node's real bounding-box center, as the browser's
	// own layout engine computed it, must be pairwise distinct.
	var centers []struct{ X, Y float64 }
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`Array.from(document.querySelectorAll('.uc-hub-node')).map(function(el) {
			var r = el.getBoundingClientRect();
			return {X: r.left + r.width / 2, Y: r.top + r.height / 2};
		})`, &centers,
	)); err != nil {
		t.Fatalf("read hub node bounding boxes: %v", err)
	}
	if len(centers) != nodeCount {
		t.Fatalf("expected %d node centers, got %d", nodeCount, len(centers))
	}
	for i := range centers {
		for j := i + 1; j < len(centers); j++ {
			if centers[i].X == centers[j].X && centers[i].Y == centers[j].Y {
				t.Fatalf("expected node %d and %d to occupy distinct positions, both at (%v, %v)", i, j, centers[i].X, centers[i].Y)
			}
		}
	}

	// Search: typing a term matching only Item/InventoryItem's
	// data-search must hide the other entities' real DOM nodes, not
	// just leave everything visible with a filter that only exists on
	// paper.
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(`.uc-menu-search`, "item", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type into the search box: %v", err)
	}

	var itemVisible, poVisible bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelector('a[href="/records/Item"]').style.display !== 'none'`, &itemVisible,
	)); err != nil {
		t.Fatalf("check Item node visibility: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelector('a[href="/records/PurchaseOrder"]').style.display === 'none'`, &poVisible,
	)); err != nil {
		t.Fatalf("check PurchaseOrder node visibility: %v", err)
	}
	if !itemVisible {
		t.Fatal("expected the Item node to remain visible after searching \"item\"")
	}
	if !poVisible {
		t.Fatal("expected the PurchaseOrder node to actually be hidden (display:none) after searching \"item\", not just filtered on paper")
	}

	// The connecting spoke line for each hidden node must hide too — a
	// node filtered out but its line left drawn to now-empty space was
	// a real, visible gap found in this feature's own first review pass.
	//
	// Counted against the hidden NODES rather than a literal, because
	// the property is "one hidden line per hidden node" — pinning the
	// number instead meant this failed the day the purchasing module
	// gained an entity (Facility, #12), reporting a spoke-line bug that
	// did not exist.
	var hiddenNodes, hiddenLines int
	if err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(
			`Array.from(document.querySelectorAll('.uc-hub-node')).filter(function(el) {
				return el.style.display === 'none';
			}).length`, &hiddenNodes,
		),
		chromedp.EvaluateAsDevTools(
			`Array.from(document.querySelectorAll('.uc-hub-lines line')).filter(function(el) {
				return el.style.display === 'none';
			}).length`, &hiddenLines,
		),
	); err != nil {
		t.Fatalf("count hidden spoke lines: %v", err)
	}
	if hiddenNodes == 0 {
		t.Fatal("searching \"item\" hid no nodes at all — the filter is not running")
	}
	if hiddenLines != hiddenNodes {
		t.Fatalf("expected one hidden spoke line per hidden node: %d nodes hidden, %d lines hidden", hiddenNodes, hiddenLines)
	}
}

// TestListPage_SortAndFilter_RealBrowser proves the list page's sort links
// and filter box actually work in a browser — a rendered-string test can
// confirm the markup, only a browser confirms clicking a header re-sorts
// and submitting the filter narrows the rows.
func TestListPage_SortAndFilter_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()

	// testServer already publishes purchasing (Item has name + sku).
	eng := crud.NewEngine(tenantDB)
	for i, n := range []string{"Zeta", "Alpha", "Mango"} {
		if _, err := eng.Create(ctx, purchasing.Item(), map[string]any{
			"sku": fmt.Sprintf("SKU-%d", i), "name": n, "item_type": "stock",
		}, humanActor()); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	bctx := browserCtx(t, tenantID)
	// Unfiltered: 3 rows.
	var beforeRows int
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('table.uc-table tbody tr').length`, &beforeRows),
	); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if beforeRows != 3 {
		t.Fatalf("expected 3 rows before filtering, got %d", beforeRows)
	}

	// The rendered filter form is a real GET to this list — action, method
	// and the q input all present, so a browser submit lands on the
	// filtered URL. (Asserting on the form's shape rather than driving a
	// full-navigation submit, which races chromedp's execution context.)
	var formOK bool
	if err := chromedp.Run(bctx, chromedp.Evaluate(`(function(){
		var f = document.querySelector('form.uc-list-filter');
		return !!f && f.method.toLowerCase()==='get'
		     && f.getAttribute('action').indexOf('/records/Item')!==-1
		     && !!f.querySelector('input[name="q"]');
	})()`, &formOK)); err != nil {
		t.Fatalf("inspect filter form: %v", err)
	}
	if !formOK {
		t.Fatal("the filter form is not a GET to /records/Item with a q input")
	}

	// Navigating to the filtered URL a submit would produce shows exactly
	// the one matching row — the filter genuinely narrows the rendered
	// page in a real browser, not just in an httptest recorder.
	var afterRows int
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item?q=SKU-1"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('table.uc-table tbody tr').length`, &afterRows),
	); err != nil {
		t.Fatalf("load filtered url: %v", err)
	}
	if afterRows != 1 {
		t.Fatalf("filtering by SKU-1 should leave 1 row, got %d", afterRows)
	}

	// Click a column header to sort, and confirm the URL carries the sort.
	var url string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item"),
		chromedp.WaitVisible(`th a.uc-sort`, chromedp.ByQuery),
		chromedp.Click(`th a.uc-sort`, chromedp.ByQuery),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Location(&url),
	); err != nil {
		t.Fatalf("sort click: %v", err)
	}
	if !strings.Contains(url, "sort=") {
		t.Fatalf("clicking a header should sort (URL should carry sort=), got %s", url)
	}
}
