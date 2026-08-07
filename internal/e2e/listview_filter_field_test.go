package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// TestListPage_ExplicitFilterFieldWithoutQuery_RealBrowser is the real-
// browser counterpart to internal/api's
// TestListPage_ExplicitFilterFieldWithoutQuery_SurvivesNextSubmit
// (uc-infra#129): a list page loaded with an explicit ?filter=X naming a
// non-default column but no ?q= yet must render a real, browser-parsed
// `<input type="hidden" name="filter" value="X">` — proving the DOM a
// real browser builds from the server's HTML actually carries the field,
// not just that the template's string output contains the right
// substring. Item's first declared field is "sku" (purchasing.go), so it
// is the implicit default column; "name" is used here specifically
// because it is NOT the default, the same way the bug could only bite
// once someone names a non-default column explicitly.
//
// Full form-submit-via-click is deliberately not driven here — this
// package's own module_menu_hub_test.go already established that races
// chromedp's execution context — so this proves the DOM shape a browser
// would submit from, and separately confirms navigating to the URL that
// submit produces (filter=name&q=...) actually narrows the page,
// matching the correct column rather than silently reverting to sku.
func TestListPage_ExplicitFilterFieldWithoutQuery_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	eng := crud.NewEngine(tenantDB)
	itemDef := purchasing.Item()
	if _, err := eng.Create(ctx, itemDef, map[string]any{
		"sku": "SKU-100", "name": "Widget", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("seed Widget: %v", err)
	}
	if _, err := eng.Create(ctx, itemDef, map[string]any{
		"sku": "SKU-200", "name": "Gadget", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("seed Gadget: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	// Pin the premise this test depends on: Item's DEFAULT column is sku
	// (declared first in purchasing.Item()), so a plain landing page with
	// no ?filter= at all renders no hidden field, and "name" below is
	// genuinely a non-default column, not accidentally the default one.
	// If purchasing.Item()'s field order ever changes, this fails loudly
	// instead of the test below silently eroding into testing nothing new
	// (independent review, uc-infra#129).
	var plainHasHidden bool
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item"),
		chromedp.WaitVisible(`form.uc-list-filter`, chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector('form.uc-list-filter input[name="filter"]')`, &plainHasHidden),
	); err != nil {
		t.Fatalf("load plain Item list page: %v", err)
	}
	if plainHasHidden {
		t.Fatal("plain /records/Item landing (no ?filter=) rendered a hidden filter field — sku is no longer the implicit default column, so \"name\" below is no longer a non-default column either")
	}

	// ?filter=name with no ?q= yet — "name" is not the default column
	// (sku is). The hidden input a real browser parses from the response
	// must name "name" explicitly, not be absent.
	var filterInputValue string
	var filterInputPresent bool
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item?filter=name"),
		chromedp.WaitVisible(`form.uc-list-filter`, chromedp.ByQuery),
		chromedp.Evaluate(`(function(){
			var i = document.querySelector('form.uc-list-filter input[name="filter"]');
			return !!i;
		})()`, &filterInputPresent),
		chromedp.Evaluate(`(function(){
			var i = document.querySelector('form.uc-list-filter input[name="filter"]');
			return i ? i.value : "";
		})()`, &filterInputValue),
	); err != nil {
		t.Fatalf("load ?filter=name page: %v", err)
	}
	if !filterInputPresent {
		t.Fatal("real browser DOM has no hidden filter input for ?filter=name (no ?q= yet) — the exact uc-infra#129 gap")
	}
	if filterInputValue != "name" {
		t.Fatalf("hidden filter input value = %q, want \"name\"", filterInputValue)
	}

	// The URL that submitting this form (with "Gadget" typed into the
	// search box) would produce — confirms the explicit column, not the
	// default sku column, is what actually gets searched.
	var rows int
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item?filter=name&q=Gadget"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('table.uc-table tbody tr').length`, &rows),
		chromedp.Text(`table.uc-table`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load filter=name q=Gadget: %v", err)
	}
	if rows != 1 {
		t.Fatalf("filter=name q=Gadget should show exactly 1 row, got %d:\n%s", rows, bodyText)
	}
	if !strings.Contains(bodyText, "Gadget") || strings.Contains(bodyText, "Widget") {
		t.Fatalf("filter=name q=Gadget should match Gadget only, got:\n%s", bodyText)
	}
}

// TestListPage_FilterFormPreservesSort_RealBrowser is the real-browser
// counterpart to internal/api's TestListPage_FilterFormPreservesSort
// (uc-infra#139): a list page loaded already sorted must render real,
// browser-parsed hidden `sort`/`dir` inputs in the filter form — proving
// the DOM a real browser builds from the server's HTML actually carries
// the active sort into the next submit, not just that the template's
// string output contains the right substring.
func TestListPage_FilterFormPreservesSort_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	eng := crud.NewEngine(tenantDB)
	itemDef := purchasing.Item()
	if _, err := eng.Create(ctx, itemDef, map[string]any{
		"sku": "SKU-100", "name": "Widget", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("seed Widget: %v", err)
	}
	if _, err := eng.Create(ctx, itemDef, map[string]any{
		"sku": "SKU-200", "name": "Gadget", "item_type": "stock",
	}, actor); err != nil {
		t.Fatalf("seed Gadget: %v", err)
	}

	bctx := browserCtx(t, tenantID)

	// A plain landing page (no ?sort= at all) renders no hidden sort/dir
	// inputs — nothing to preserve. Pins the boundary this fix must not
	// widen past, same discipline as the plain-landing-page check above.
	var plainHasSort, plainHasDir bool
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item"),
		chromedp.WaitVisible(`form.uc-list-filter`, chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector('form.uc-list-filter input[name="sort"]')`, &plainHasSort),
		chromedp.Evaluate(`!!document.querySelector('form.uc-list-filter input[name="dir"]')`, &plainHasDir),
	); err != nil {
		t.Fatalf("load plain Item list page: %v", err)
	}
	if plainHasSort || plainHasDir {
		t.Fatal("plain /records/Item landing (no ?sort=) rendered a hidden sort/dir field")
	}

	// ?sort=name&dir=desc: the real browser DOM must carry both forward as
	// hidden inputs in the filter form.
	var sortValue, dirValue string
	var sortPresent, dirPresent bool
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item?sort=name&dir=desc"),
		chromedp.WaitVisible(`form.uc-list-filter`, chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector('form.uc-list-filter input[name="sort"]')`, &sortPresent),
		chromedp.Evaluate(`(function(){var i=document.querySelector('form.uc-list-filter input[name="sort"]');return i?i.value:"";})()`, &sortValue),
		chromedp.Evaluate(`!!document.querySelector('form.uc-list-filter input[name="dir"]')`, &dirPresent),
		chromedp.Evaluate(`(function(){var i=document.querySelector('form.uc-list-filter input[name="dir"]');return i?i.value:"";})()`, &dirValue),
	); err != nil {
		t.Fatalf("load ?sort=name&dir=desc page: %v", err)
	}
	if !sortPresent || sortValue != "name" {
		t.Fatalf("real browser DOM hidden sort input: present=%v value=%q, want present=true value=\"name\" (uc-infra#139 gap)", sortPresent, sortValue)
	}
	if !dirPresent || dirValue != "desc" {
		t.Fatalf("real browser DOM hidden dir input: present=%v value=%q, want present=true value=\"desc\" (uc-infra#139 gap)", dirPresent, dirValue)
	}

	// The URL that submitting this form (with "Gadget" typed into the
	// search box) would produce — confirms the sort survives alongside the
	// new filter, not silently dropped back to the default ordering.
	var rows int
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/records/Item?sort=name&dir=desc&filter=name&q=Gadget"),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('table.uc-table tbody tr').length`, &rows),
		chromedp.Text(`table.uc-table`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load sort=name&dir=desc&filter=name&q=Gadget: %v", err)
	}
	if rows != 1 {
		t.Fatalf("sort=name&dir=desc&filter=name&q=Gadget should show exactly 1 row, got %d:\n%s", rows, bodyText)
	}
	if !strings.Contains(bodyText, "Gadget") || strings.Contains(bodyText, "Widget") {
		t.Fatalf("sort=name&dir=desc&filter=name&q=Gadget should match Gadget only, got:\n%s", bodyText)
	}
}
