package e2e

import (
	"testing"

	"github.com/chromedp/chromedp"
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
	// POLine, InventoryItem, GoodsReceipt, GoodsReceiptLine, VendorInvoice
	// (7 entities).
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
	if nodeCount != 7 {
		t.Fatalf("expected 7 entity hub nodes (Item, PurchaseOrder, POLine, InventoryItem, GoodsReceipt, GoodsReceiptLine, VendorInvoice), got %d", nodeCount)
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
	if len(centers) != 7 {
		t.Fatalf("expected 7 node centers, got %d", len(centers))
	}
	for i := range centers {
		for j := i + 1; j < len(centers); j++ {
			if centers[i].X == centers[j].X && centers[i].Y == centers[j].Y {
				t.Fatalf("expected node %d and %d to occupy distinct positions, both at (%v, %v)", i, j, centers[i].X, centers[i].Y)
			}
		}
	}

	// Search: typing a term matching only Item/InventoryItem's
	// data-search must hide the other 4 entities' real DOM nodes, not
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
	var hiddenLines int
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`Array.from(document.querySelectorAll('.uc-hub-lines line')).filter(function(el) {
			return el.style.display === 'none';
		}).length`, &hiddenLines,
	)); err != nil {
		t.Fatalf("count hidden spoke lines: %v", err)
	}
	if hiddenLines != 5 {
		t.Fatalf("expected exactly 5 spoke lines hidden (matching the 5 filtered-out nodes: PurchaseOrder, POLine, GoodsReceipt, GoodsReceiptLine, VendorInvoice — none of their SearchKeys contain \"item\"), got %d", hiddenLines)
	}
}
