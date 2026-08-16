package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/testfixtures"
)

// TestReferencePicker_StatusIDOnlyOffersOwnStatusType is the real-browser
// proof of ADR-0032's status_id auto-scoping (uc-infra#250) —
// internal/kernel/crud/target_constraints_test.go and
// internal/api/reference_search_test.go already prove the mechanism at
// the engine and HTTP layers; this proves the picker a human actually
// types into on a real form never offers a status belonging to a
// DIFFERENT entity type's StatusType, matching the same "prove it at
// the layer a human actually interacts with" rationale
// target_filter_picker_test.go's own two tests already establish for
// TargetFilter/MustMatchParentField. Depends on
// internal/kernel/formrender rendering status_id exactly like any other
// FieldReference (a plain .uc-ref combobox, no field-name branching in
// the renderer — CLAUDE.md's kernel-boundary rule) and the client-side
// picker script (layout.go) sending source_entity_type/source_field off
// the form's own data-entity-type/data-field attributes, neither of
// which needed to change for this feature to work.
//
// purchasing's purchase_order_status seeds Draft/Submitted/Approved/
// Received/Cancelled; sales' sales_order_status seeds Draft/Confirmed/
// Fulfilled/Invoiced/Cancelled (purchasing/seed.go, sales/seed.go). "e"
// matches Submitted/Approved/Received/Cancelled on the purchasing side
// and Confirmed/Fulfilled/Invoiced on the sales side (neither StatusType's
// shared "Draft" contains an "e") — so an unnarrowed picker would return
// 7 options here, not 4; a narrowing bug that fails open is exactly what
// this query is chosen to catch, not just a bug that narrows to the
// wrong-but-still-single StatusType.
func TestReferencePicker_StatusIDOnlyOffersOwnStatusType(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	testfixtures.PublishFoundationPurchasingSalesFixtures(t, ctx, tenantDB, actor)

	bctx := browserCtx(t, tenantID)
	scope := `.uc-ref[data-field="status_id"]`
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/PurchaseOrder/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SendKeys(scope+` .uc-ref-search`, "e", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'e' into status_id: %v", err)
	}

	var labels []string
	if err := chromedp.Run(bctx, chromedp.Evaluate(
		`Array.prototype.map.call(document.querySelectorAll('`+scope+` .uc-ref-option'),function(e){return e.textContent;})`,
		&labels,
	)); err != nil {
		t.Fatalf("read narrowed options: %v", err)
	}

	want := map[string]bool{"Submitted": true, "Approved": true, "Received": true, "Cancelled": true}
	if len(labels) != len(want) {
		t.Fatalf("expected exactly PurchaseOrder's own purchase_order_status matches %v, got %v", want, labels)
	}
	for _, l := range labels {
		if !want[l] {
			t.Fatalf("unexpected status leaked into PurchaseOrder's status_id picker (likely sales_order_status): %v", labels)
		}
	}
}
