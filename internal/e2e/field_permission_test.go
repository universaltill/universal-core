package e2e

import (
	"context"
	"database/sql"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
)

// The real-browser layer for ADR-0006's field-level enforcement.
//
// The internal/api tests already assert on the rendered HTML string, and
// that genuinely proves the input tag isn't emitted. What a string test
// can NOT prove is what a browser ends up with: that no element with that
// name exists in the live DOM, that the value isn't sitting in some
// attribute a serializer would submit back, and that the denial page a
// refused user lands on is actually styled rather than unstyled default
// markup (exactly the class of bug the issue-report page shipped with —
// see issue_report_test.go's own doc comment).

// e2eActorID is the actor every browserCtx request authenticates as (see
// browserCtx's network.SetExtraHTTPHeaders) — the identity a role has to
// be granted to for these tests to exercise anything.
const e2eActorID = "00000000-0000-0000-0000-0000000000e2"

// grantE2ERole creates roleCode and grants it to the browser's own
// actor, returning the role id — written through the raw engine, the
// "system setup" path authz deliberately carves out.
func grantE2ERole(t *testing.T, db *sql.DB, roleCode string) string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	actor := humanActor()

	role, err := engine.Create(ctx, foundation.Role(), map[string]any{"code": roleCode, "name": roleCode}, actor)
	if err != nil {
		t.Fatalf("create Role %s: %v", roleCode, err)
	}
	if _, err := engine.Create(ctx, foundation.UserRole(), map[string]any{"user_id": e2eActorID, "role_id": role.ID}, actor); err != nil {
		t.Fatalf("grant %s: %v", roleCode, err)
	}
	return role.ID
}

// seedFieldPermission hides entityType.fieldName from the browser actor.
func seedFieldPermission(t *testing.T, db *sql.DB, roleCode, entityType, fieldName string) {
	t.Helper()
	roleID := grantE2ERole(t, db, roleCode)
	_, err := crud.NewEngine(db).Create(context.Background(), foundation.FieldPermission(), map[string]any{
		"role_id": roleID, "entity_type": entityType, "field_name": fieldName, "hidden": true,
	}, humanActor())
	if err != nil {
		t.Fatalf("create FieldPermission: %v", err)
	}
}

// seedEntityPermission opts entityType into RBAC and grants the browser
// actor exactly the access described.
func seedEntityPermission(t *testing.T, db *sql.DB, roleCode, entityType string, canRead, canWrite bool) {
	t.Helper()
	roleID := grantE2ERole(t, db, roleCode)
	_, err := crud.NewEngine(db).Create(context.Background(), foundation.Permission(), map[string]any{
		"role_id": roleID, "entity_type": entityType, "can_read": canRead, "can_write": canWrite,
	}, humanActor())
	if err != nil {
		t.Fatalf("create Permission: %v", err)
	}
}

// TestFieldPermission_HiddenFieldAbsentFromLiveDOM drives a real browser
// to a record form whose `sku` field is hidden from the logged-in user's
// role, and asks the DOM — not the HTML string — whether anything named
// `sku` exists. Nothing may: not a visible input, and not one of
// formrender's hidden preservation inputs (which every OTHER off-form
// field legitimately gets, and which is precisely why "just don't render
// the visible input" would have been the wrong fix).
func TestFieldPermission_HiddenFieldAbsentFromLiveDOM(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()

	// Hide Item.sku from the browser's own actor.
	seedFieldPermission(t, tenantDB, "e2e_clerk", "Item", "sku")

	rec, err := crud.NewEngine(tenantDB).Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SECRET-SKU-1", "name": "Visible Widget", "item_type": "stock",
	}, humanActor())
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Item/"+rec.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the Item form: %v", err)
	}

	// Any element the browser would submit under that name — visible
	// input, hidden input, select, anything.
	var skuNodeCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('[name="sku"]').length`, &skuNodeCount,
	)); err != nil {
		t.Fatalf("count sku nodes: %v", err)
	}
	if skuNodeCount != 0 {
		t.Fatalf("hidden field is present in the live DOM (%d node(s) named sku) — a browser would submit it back", skuNodeCount)
	}

	// The value must not survive anywhere in the document either — an
	// attribute, a data-*, a comment.
	var leaks bool
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.outerHTML.includes("SECRET-SKU-1")`, &leaks,
	)); err != nil {
		t.Fatalf("scan document for the hidden value: %v", err)
	}
	if leaks {
		t.Fatal("hidden field's value reached the browser somewhere in the document")
	}

	// The rest of the form is intact and usable — redaction must be
	// surgical, not a reason for the form to stop working.
	var nameCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('[name="name"]').length`, &nameCount,
	)); err != nil {
		t.Fatalf("count name nodes: %v", err)
	}
	if nameCount == 0 {
		t.Fatal("redacting one field removed another — the form lost its visible fields")
	}
}

// TestFieldPermission_MasterDetailChildColumnAbsentFromLiveDOM
// (uc-infra#127): the real-browser layer for ChildRedactedFields — a
// FieldPermission hiding POLine.unit_price from the logged-in user's
// role must remove that column (header AND cell) from the PurchaseOrder
// form's Lines master_detail table, exactly as
// TestFieldPermission_HiddenFieldAbsentFromLiveDOM already proves for a
// redacted field on the FORM'S OWN entity. A rendered-HTML-string test
// (formrender's/internal/api's own unit and integration tests for this
// fix) can't prove no element named unit_price exists in the live DOM —
// only a real browser settles that.
func TestFieldPermission_MasterDetailChildColumnAbsentFromLiveDOM(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	// Hide POLine.unit_price from the browser's own actor.
	seedFieldPermission(t, tenantDB, "e2e_line_viewer", "POLine", "unit_price")

	if err := purchasing.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(tenantDB)
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Redaction Test Vendor", "party_type": "organization", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}
	statusID := publishedStatusID(t, tenantDB, "purchase_order_status", "approved")
	po, err := engine.Create(ctx, purchasing.PurchaseOrder(), map[string]any{
		"po_number": "PO-REDACT-1", "vendor_id": vendor.ID, "order_date": "2026-08-01", "status_id": statusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed PurchaseOrder: %v", err)
	}
	item, err := engine.Create(ctx, purchasing.Item(), map[string]any{
		"sku": "SKU-REDACT-1", "name": "Redaction Test Widget", "item_type": "stock",
	}, actor)
	if err != nil {
		t.Fatalf("seed Item: %v", err)
	}
	if _, err := engine.Create(ctx, purchasing.POLine(), map[string]any{
		"purchase_order_id": po.ID, "item_id": item.ID, "qty": 3, "unit_price": 42.5,
	}, actor); err != nil {
		t.Fatalf("seed POLine: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/"+po.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the PurchaseOrder form: %v", err)
	}

	// Neither the column header nor any row cell for the redacted child
	// field may exist in the live DOM.
	var unitPriceNodeCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('[data-field="unit_price"]').length`, &unitPriceNodeCount,
	)); err != nil {
		t.Fatalf("count unit_price nodes: %v", err)
	}
	if unitPriceNodeCount != 0 {
		t.Fatalf("redacted child column is present in the live DOM (%d node(s) for unit_price)", unitPriceNodeCount)
	}
	var leaks bool
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.outerHTML.includes("42.5")`, &leaks,
	)); err != nil {
		t.Fatalf("scan document for the redacted value: %v", err)
	}
	if leaks {
		t.Fatal("redacted child field's value reached the browser somewhere in the document")
	}

	// The sibling qty column is unaffected — redaction is surgical.
	var qtyNodeCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('[data-field="qty"]').length`, &qtyNodeCount,
	)); err != nil {
		t.Fatalf("count qty nodes: %v", err)
	}
	if qtyNodeCount == 0 {
		t.Fatal("redacting unit_price removed the qty column too — the Lines table lost an unrelated column")
	}
}

// TestAccessDeniedPage_IsStyledAndLocalized is the counterpart to
// issue_report_test.go's styling regression: a 403 page is one of the
// few pages a user reaches without meaning to, so it has to look like
// part of the app rather than unstyled default markup. Asserted against
// the browser's own computed styles, which is the only thing that proves
// app.css actually applied.
func TestAccessDeniedPage_IsStyledAndLocalized(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)

	// Deny the browser actor write on Item, then walk them to the create
	// form — a page that makes no CRUD call of its own and so used to
	// render in full for a denied user.
	seedEntityPermission(t, tenantDB, "e2e_reader", "Item", true, false)

	bctx := browserCtx(t, tenantID)
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Item/new"),
		chromedp.WaitVisible(`.uc-denied`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to a denied create form: %v", err)
	}

	// No form was rendered — the whole point of gating the shell.
	var formCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('form.uc-form').length`, &formCount,
	)); err != nil {
		t.Fatalf("count forms: %v", err)
	}
	if formCount != 0 {
		t.Fatalf("denied user was still served a usable create form (%d)", formCount)
	}

	// app.css's .uc-denied rule sets a real border; the browser default
	// for a <div> is "none". This distinguishes "styled" from "the CSS
	// never applied" the same way a screenshot would.
	var borderStyle string
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`getComputedStyle(document.querySelector(".uc-denied")).borderStyle`, &borderStyle,
	)); err != nil {
		t.Fatalf("read the denial panel's computed border style: %v", err)
	}
	if borderStyle != "solid" {
		t.Fatalf(`expected .uc-denied computed border-style "solid" (app.css), got %q — the denial page's CSS isn't applying`, borderStyle)
	}

	// The nav is still there: a denied user must be able to navigate
	// away, not land on a dead end.
	var navCount int
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll("nav.uc-nav").length`, &navCount,
	)); err != nil {
		t.Fatalf("count nav elements: %v", err)
	}
	if navCount == 0 {
		t.Fatal("denial page rendered without the nav — a dead end with no way back")
	}

	// Localized for real, in a real browser, including the document's own
	// dir attribute flipping for a right-to-left locale.
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Item/new?lang=ar"),
		chromedp.WaitVisible(`.uc-denied`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to the denied page in ar: %v", err)
	}
	var dir string
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.getAttribute("dir")`, &dir,
	)); err != nil {
		t.Fatalf("read document dir: %v", err)
	}
	if dir != "rtl" {
		t.Fatalf(`expected dir="rtl" on the Arabic denial page, got %q`, dir)
	}
}
