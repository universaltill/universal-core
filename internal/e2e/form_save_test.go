package e2e

import (
	"context"
	"database/sql"
	"path"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
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
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
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

// TestPurchaseOrderFormStages_RealBrowser (#29): the six staged
// lead-time date inputs are actually present and *visible* on
// /forms/PurchaseOrder/new — rendered as real <input type="date">
// elements a buyer can click, each under its localized label.
//
// Driven in TURKISH (?lang=tr), deliberately: the Go form Definition's
// fallback Labels are the same English strings as the en catalog keys,
// so an English-locale assertion passes identically whether the
// field.PurchaseOrder.* keys are wired into the render path or deleted
// outright — a tautology the independent review caught (2026-07-31).
// Asserting the tr strings proves the catalog is actually consulted.
func TestPurchaseOrderFormStages_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/new?lang=tr"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /forms/PurchaseOrder/new: %v", err)
	}

	stages := []struct{ name, label string }{
		{"sourced_at", "Tedarik Edildi"},
		{"production_start_at", "Üretim Başladı"},
		{"production_ready_at", "Üretim Hazır"},
		{"shipped_at", "Sevk Edildi"},
		{"customs_cleared_at", "Gümrükten Çekildi"},
		{"received_at", "Teslim Alındı"},
	}
	for _, s := range stages {
		inputSel := `input[name="` + s.name + `"]`
		if err := chromedp.Run(ctx, chromedp.WaitVisible(inputSel, chromedp.ByQuery)); err != nil {
			t.Fatalf("stage input %s not visible: %v", s.name, err)
		}
		var inputType string
		var hasType bool
		if err := chromedp.Run(ctx, chromedp.AttributeValue(inputSel, "type", &inputType, &hasType, chromedp.ByQuery)); err != nil {
			t.Fatalf("read type of %s: %v", s.name, err)
		}
		if !hasType || inputType != "date" {
			t.Errorf("stage input %s: expected a real date input, got type=%q", s.name, inputType)
		}
		var labelText string
		if err := chromedp.Run(ctx, chromedp.Text(`label[for="`+s.name+`"]`, &labelText, chromedp.ByQuery)); err != nil {
			t.Fatalf("read label for %s: %v", s.name, err)
		}
		if got := strings.TrimSpace(labelText); got != s.label {
			t.Errorf("stage %s: expected localized label %q, got %q", s.name, s.label, got)
		}
	}
}

// TestFormSectionTitlesAndSaveButtonAreLocalized_RealBrowser (#53):
// section titles and the Save button are real, rendered, translated text
// in a real browser — not just markup-string assertions, per
// CLAUDE.md's UI testing rule.
//
// Driven in TURKISH (?lang=tr), deliberately, same reasoning as
// TestPurchaseOrderFormStages_RealBrowser above (that test's own doc
// comment, and the independent review it cites): PurchaseOrderForm's
// literal Go Section.Title ("Header") and Action.Label ("Save") equal
// their English catalog values, so asserting the English text would
// pass identically whether formrender.SectionCatalogKey/
// SaveActionCatalogKey are actually wired into the render path or
// deleted outright.
func TestFormSectionTitlesAndSaveButtonAreLocalized_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := testServer(t)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/new?lang=tr"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /forms/PurchaseOrder/new?lang=tr: %v", err)
	}

	var headerTitle string
	if err := chromedp.Run(ctx, chromedp.Text(`section.uc-section h2`, &headerTitle, chromedp.ByQuery)); err != nil {
		t.Fatalf("read Header section title: %v", err)
	}
	if got := strings.TrimSpace(headerTitle); got != "Başlık" {
		t.Fatalf("expected the Turkish translation of the Header section title, got %q", got)
	}

	var saveLabel string
	if err := chromedp.Run(ctx, chromedp.Text(`form.uc-form button[type="submit"]`, &saveLabel, chromedp.ByQuery)); err != nil {
		t.Fatalf("read Save button text: %v", err)
	}
	if got := strings.TrimSpace(saveLabel); got != "Kaydet" {
		t.Fatalf("expected the Turkish translation of the Save button, got %q", got)
	}
}

// submitForm clicks the form's Save button and waits for htmx's own
// afterSettle event — see clickAndSettle's doc comment in
// csv_import_test.go for why WaitVisible on the swapped content isn't
// enough by itself in a headless browser.
func submitForm() chromedp.Action {
	return clickAndSettle(`form.uc-form button[type="submit"]`)
}

// TestPurchaseOrderStages_ReversedDates_ToastInRealBrowser (#29, the
// Architect note's second e2e deliverable — flagged as silently dropped
// by the independent review): entering a stage date before its
// predecessor and saving surfaces the validation error through the real
// error toast, in a real browser. Driven through the EDIT form of a
// PO created with valid references (the create form's reference pickers
// are orthogonal machinery this test doesn't need to fight).
func TestPurchaseOrderStages_ReversedDates_ToastInRealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// testServer publishes entities/forms but not the status graph —
	// PurchaseOrder.status_id needs it (idempotent, same call
	// cmd/provision-tenant makes).
	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	vendorID, err := crud.NewEngine(tenantDB).Create(ctx, foundation.Party(), map[string]any{
		"name": "Toast Vendor", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	// uc-infra#78: PurchaseOrder.vendor_id now requires the referenced
	// Party to hold the vendor PartyRole.
	if _, err := crud.NewEngine(tenantDB).Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendorID.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	statusID := publishedStatusID(t, tenantDB, "purchase_order_status", "approved")
	po, err := crud.NewEngine(tenantDB).Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-TOAST-1", "vendor_id": vendorID.ID, "order_date": "2026-07-01",
		"status_id": statusID, "production_ready_at": "2026-07-10", "shipped_at": "2026-07-12",
	}, actor)
	if err != nil {
		t.Fatalf("seed PO: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/"+po.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="shipped_at"]`, "2026-07-05", chromedp.ByQuery),
		// A plain click, not submitForm(): clickAndSettle's settle helper
		// rejects on a non-2xx response, and a 400 is exactly what this
		// test wants — the toast's own visibility is the wait condition.
		chromedp.Click(`form.uc-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-toast.uc-toast-visible`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("reversed-date save + toast: %v", err)
	}
	var toastText string
	if err := chromedp.Run(bctx, chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read toast: %v", err)
	}
	if !strings.Contains(toastText, "must not be before") {
		t.Fatalf("toast %q should carry the chronology error", toastText)
	}
}

// publishedStatusID resolves a Status record id by status-type code +
// status code — the two-step lookup Status's own status_type_id scoping
// requires (status codes are unique per type, not globally).
func publishedStatusID(t *testing.T, db *sql.DB, typeCode, statusCode string) string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	types, err := engine.ListByField(ctx, foundation.StatusType(), "code", typeCode)
	if err != nil || len(types) != 1 {
		t.Fatalf("lookup StatusType %s: %v (n=%d)", typeCode, err, len(types))
	}
	statuses, err := engine.ListByField(ctx, foundation.Status(), "status_type_id", types[0].ID)
	if err != nil {
		t.Fatalf("list Status for %s: %v", typeCode, err)
	}
	for _, st := range statuses {
		if code, _ := st.Data["code"].(string); code == statusCode {
			return st.ID
		}
	}
	t.Fatalf("no %q Status under %s", statusCode, typeCode)
	return ""
}
