package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
)

// TestAttendanceRecord_HoursWorkedMinMaxEnforcedByTheBrowser (uc-infra#80,
// ADR-0018 §4) is the real-browser counterpart formrender's own
// TestRender_NumberFieldMinMaxAttributes test can't be: a
// strings.Contains assertion on the rendered HTML proves the min/max
// attributes are IN the markup, never that a real browser actually reads
// them and blocks an out-of-bounds value — CLAUDE.md's own distinction
// ("a rendered-HTML-string test proves markup structure, never proves...
// behaves correctly"). AttendanceRecord.hours_worked ([0, 24]) is used
// because it needs no StatusType/Status graph (AttendanceRecord has none)
// and no other record to exist first — checkValidity() is exercised on
// the one input directly, not through a full form submission, so the
// other required fields (employee_id, source) being empty is irrelevant
// to what this test is checking.
func TestAttendanceRecord_HoursWorkedMinMaxEnforcedByTheBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"foundation", func() error { return foundation.Publish(ctx, tenantDB, actor) }},
		{"foundation forms", func() error { return foundation.PublishForms(ctx, tenantDB, actor) }},
		{"hr", func() error { return hr.Publish(ctx, tenantDB, actor) }},
		{"hr forms", func() error { return hr.PublishForms(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	browser := browserCtx(t, tenantID)

	var minAttr, maxAttr string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/AttendanceRecord/new"),
		chromedp.WaitVisible(`input[name="hours_worked"]`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').min`, &minAttr),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').max`, &maxAttr),
	); err != nil {
		t.Fatalf("render AttendanceRecord form: %v", err)
	}
	if minAttr != "0" || maxAttr != "24" {
		t.Fatalf("hours_worked DOM properties = min:%q max:%q, want min:\"0\" max:\"24\"", minAttr, maxAttr)
	}

	// The actual behavior a browser gives a user for free once min/max
	// are declared: native constraint validation. This is what the
	// markup-only formrender test cannot exercise.
	var belowValid, aboveValid, atMinValid, atMaxValid bool
	var belowMessage string
	if err := chromedp.Run(browser,
		chromedp.SetValue(`input[name="hours_worked"]`, "-1", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').checkValidity()`, &belowValid),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').validationMessage`, &belowMessage),

		chromedp.SetValue(`input[name="hours_worked"]`, "30", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').checkValidity()`, &aboveValid),

		chromedp.SetValue(`input[name="hours_worked"]`, "0", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').checkValidity()`, &atMinValid),

		chromedp.SetValue(`input[name="hours_worked"]`, "24", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('input[name="hours_worked"]').checkValidity()`, &atMaxValid),
	); err != nil {
		t.Fatalf("exercise native constraint validation: %v", err)
	}
	if belowValid {
		t.Error("the browser must reject -1 as below the declared min — checkValidity() returned true")
	}
	if belowMessage == "" {
		t.Error("a browser rejecting an out-of-range value must surface a non-empty validationMessage to the user")
	}
	if aboveValid {
		t.Error("the browser must reject 30 as above the declared max — checkValidity() returned true")
	}
	if !atMinValid {
		t.Error("the browser must accept 0, the inclusive minimum — checkValidity() returned false")
	}
	if !atMaxValid {
		t.Error("the browser must accept 24, the inclusive maximum — checkValidity() returned false")
	}
}
