package e2e

import (
	"path"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestFormSaveButton_RealBrowser is the regression test — driven by a
// real browser, not curl or httptest — for the bug found while
// continuing this session's work past the CSV-import-wizard scenario:
// formrender's own <form> tag (hx-post + hx-target="this"
// hx-swap="outerHTML", see render.go's tmplSrc) was never actually
// exercised through real htmx execution before. It had two compounding
// bugs, both invisible to every prior curl-based test:
//
//  1. htmx submits a plain <form> as application/x-www-form-urlencoded
//     (no hx-encoding override on the tag), but createRecord only ever
//     decoded JSON — every real "Save" click 400'd with "invalid JSON
//     body" before validation even ran.
//  2. POST /api/records/{entityType}/{id} (what saving an *existing*
//     record submits to) had no route registered at all — editing
//     anything 404'd outright.
//
// Both are fixed in internal/api/handlers.go (parseRecordFields,
// isHTMXRequest, the new updateRecord handler/route). This test proves
// the fix through the real interaction a user has: navigate to a new
// record's form, fill real <input>/<select> elements, click the real
// Save button, confirm the form transforms into an edit form for the
// record it just created (the standard htmx create->edit pattern), edit
// a field, click Save again, and confirm the update actually persisted.
func TestFormSaveButton_RealBrowser(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	srv, tenantID := testServer(t, db)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Item/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="sku"]`, "STEEL-BAR-10", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "10mm Steel Rebar", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save new record: %v", err)
	}

	var postHref string
	if err := chromedp.Run(ctx, chromedp.AttributeValue(`form.uc-form`, "hx-post", &postHref, nil, chromedp.ByQuery)); err != nil {
		t.Fatalf("read hx-post after save: %v", err)
	}
	if postHref == "/api/records/Item" {
		t.Fatalf("expected the form to now target its own record id (create -> edit), still targets the create route: %s", postHref)
	}

	var nameValue string
	if err := chromedp.Run(ctx, chromedp.Value(`input[name="name"]`, &nameValue, chromedp.ByQuery)); err != nil {
		t.Fatalf("read name value after save: %v", err)
	}
	if nameValue != "10mm Steel Rebar" {
		t.Fatalf("expected the saved value pre-filled after create, got %q", nameValue)
	}

	// Edit the now-existing record and save again — this is the route
	// that had no handler at all before this fix (POST
	// /api/records/Item/{id}).
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`input[name="name"]`, "10mm Steel Rebar (Grade 60)", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("edit + save existing record: %v", err)
	}

	var updatedName string
	if err := chromedp.Run(ctx, chromedp.Value(`input[name="name"]`, &updatedName, chromedp.ByQuery)); err != nil {
		t.Fatalf("read name value after update: %v", err)
	}
	if updatedName != "10mm Steel Rebar (Grade 60)" {
		t.Fatalf("expected the updated value reflected after save, got %q", updatedName)
	}

	// Reload the same record's own edit URL fresh (a new page navigation,
	// not an htmx swap) to confirm the update actually persisted to
	// Postgres, not just reflected in the client-side DOM the swap left
	// behind. postHref is the API record endpoint
	// (/api/records/Item/{id}), not the form page — only the id is
	// reused here.
	recordID := path.Base(postHref)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Item/"+recordID)); err != nil {
		t.Fatalf("re-navigate to record: %v", err)
	}
	var reloadedName string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`input[name="name"]`, &reloadedName, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read name value after reload: %v", err)
	}
	if reloadedName != "10mm Steel Rebar (Grade 60)" {
		t.Fatalf("expected the update to have persisted across a fresh page load, got %q", reloadedName)
	}
}

// submitForm clicks the form's Save button and waits for htmx's own
// afterSettle event — see clickAndSettle's doc comment in
// csv_import_test.go for why WaitVisible on the swapped content isn't
// enough by itself in a headless browser.
func submitForm() chromedp.Action {
	return clickAndSettle(`form.uc-form button[type="submit"]`)
}
