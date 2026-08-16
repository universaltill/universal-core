package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/testfixtures"
)

// setUpStatusTransitionPickerTest is the shared setup for the
// from_status_id/to_status_id picker tests below: seed both
// purchase_order_status and sales_order_status (so a narrowing bug that
// fails open has a real wrong-StatusType candidate set to leak into),
// then navigate to a fresh StatusTransition/new form and select
// status_type_id = "Purchase Order Status" — the sibling value
// from_status_id/to_status_id's MustMatchParentField narrowing reads off
// the form. Each test gets its OWN fresh page/tenant via testServer,
// rather than sharing one page across both field checks, to avoid
// interference between the two pickers' own dropdown/search state on a
// single page (a same-page, clear-between-fields version of this test
// was flaky for exactly that reason).
func setUpStatusTransitionPickerTest(t *testing.T) context.Context {
	t.Helper()
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	testfixtures.PublishFoundationPurchasingSalesFixtures(t, ctx, tenantDB, actor)

	bctx := browserCtx(t, tenantID)
	statusTypeScope := `.uc-ref[data-field="status_type_id"]`
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/StatusTransition/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SendKeys(statusTypeScope+` .uc-ref-search`, "Purchase Order Status", chromedp.ByQuery),
		chromedp.WaitVisible(statusTypeScope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead status_type_id 'Purchase Order Status': %v", err)
	}
	if err := chromedp.Run(bctx, chromedp.Click(statusTypeScope+` .uc-ref-option`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click Purchase Order Status option: %v", err)
	}
	return bctx
}

// assertStatusTransitionPickerNarrowedToOwnStatusType is shared by the
// from_status_id/to_status_id tests below: type "e" into field's picker.
// purchase_order_status's own 4 Status rows (Submitted/Approved/
// Received/Cancelled, not Draft) match — but so does at least one Status
// row on every OTHER StatusType purchasing.PublishStatuses/
// sales.PublishStatuses seed (vendor_invoice_status, stock_transfer_
// status, rfq_status, sales_order_status, customer_invoice_status: 17
// Status rows contain an "e" tenant-wide), so an unnarrowed picker would
// return well more than 4 options here, matching
// TestReferencePicker_StatusIDOnlyOffersOwnStatusType's own choice of
// query for the same reason) and assert only purchase_order_status's
// own 4 come back.
func assertStatusTransitionPickerNarrowedToOwnStatusType(t *testing.T, bctx context.Context, field string) {
	t.Helper()
	scope := `.uc-ref[data-field="` + field + `"]`
	if err := chromedp.Run(bctx,
		chromedp.SendKeys(scope+` .uc-ref-search`, "e", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'e' into %s: %v", field, err)
	}

	var labels []string
	if err := chromedp.Run(bctx, chromedp.Evaluate(
		`Array.prototype.map.call(document.querySelectorAll('`+scope+` .uc-ref-option'),function(e){return e.textContent;})`,
		&labels,
	)); err != nil {
		t.Fatalf("read narrowed options for %s: %v", field, err)
	}

	want := map[string]bool{"Submitted": true, "Approved": true, "Received": true, "Cancelled": true}
	if len(labels) != len(want) {
		t.Fatalf("%s: expected exactly purchase_order_status's own matches %v, got %v", field, want, labels)
	}
	for _, l := range labels {
		if !want[l] {
			t.Fatalf("%s: unexpected status leaked into the picker (likely sales_order_status): %v", field, labels)
		}
	}
}

// TestReferencePicker_StatusTransitionFromStatusOnlyOffersOwnStatusType
// and its to_status_id twin below are the real-browser proof of
// StatusTransition.from_status_id/to_status_id's MustMatchParentField:
// "status_type_id" (uc-infra#252) — the twin of
// TestReferencePicker_StatusIDOnlyOffersOwnStatusType above, which
// proves status_id's own (differently-mechanized, ADR-0032 auto-scoped)
// picker narrowing; these prove the declared-field MustMatchParentField
// mechanism at the layer a human actually types into: an admin
// authoring a StatusTransition for purchase_order_status must never be
// offered a Status belonging to sales_order_status in the
// from_status_id/to_status_id pickers, even though both StatusTypes'
// Status rows exist in the same tenant.
// internal/kernel/foundation's own
// TestStatusTransition_RejectsCrossStatusTypeFromAndTo already proves
// the write path rejects the mismatch; these prove the picker itself
// never offers it as a candidate in the first place (formrender's
// data-must-match-field attribute, read live off the status_type_id
// field already selected on the form — same mechanism
// target_filter_picker_test.go's own parent_task_id test proves for
// Task/Project).
func TestReferencePicker_StatusTransitionFromStatusOnlyOffersOwnStatusType(t *testing.T) {
	bctx := setUpStatusTransitionPickerTest(t)
	assertStatusTransitionPickerNarrowedToOwnStatusType(t, bctx, "from_status_id")
}

func TestReferencePicker_StatusTransitionToStatusOnlyOffersOwnStatusType(t *testing.T) {
	bctx := setUpStatusTransitionPickerTest(t)
	assertStatusTransitionPickerNarrowedToOwnStatusType(t, bctx, "to_status_id")
}
