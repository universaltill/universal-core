package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestReferencePicker_SearchNarrowsAndGuardsStaleID is the focused
// browser test for the #24 searchable reference picker's own behaviour,
// beyond "a pick persists" (which TestDepartmentPosition_RealBrowser
// already covers). It proves the two things that make the combobox worth
// building over the old full-list <select>:
//
//  1. Typing actually FILTERS server-side — a query that matches one of
//     several records returns only the match, not the whole list. This is
//     the entire point: the old <select> shipped every record; this must
//     not.
//  2. Editing the search text AFTER choosing an option clears the hidden
//     id (layout.go's stale-reference guard) — so a form can never submit
//     a label the user sees with a different id underneath it.
func TestReferencePicker_SearchNarrowsAndGuardsStaleID(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	ctx := browserCtx(t, tenantID)

	// Seed three Departments so a type-ahead has something to narrow from.
	for _, d := range []struct{ code, name string }{
		{"eng", "Engineering"},
		{"fin", "Finance"},
		{"fac", "Facilities"},
	} {
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL+"/forms/Department/new"),
			chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
			chromedp.SetValue(`input[name="code"]`, d.code, chromedp.ByQuery),
			chromedp.SetValue(`input[name="name"]`, d.name, chromedp.ByQuery),
			submitForm(),
		); err != nil {
			t.Fatalf("seed Department %q: %v", d.name, err)
		}
		savedRecordID(t, ctx, "Department")
	}

	// On a fresh Department form, type "Fin" into the parent picker.
	scope := `.uc-ref[data-field="parent_department_id"]`
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SendKeys(scope+` .uc-ref-search`, "Fin", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'Fin': %v", err)
	}

	// Exactly one option — "Finance" — must show. "Facilities" and
	// "Engineering" must NOT: a broken filter that returned the whole list
	// would be the very regression this endpoint exists to prevent.
	var labels []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`Array.prototype.map.call(document.querySelectorAll('`+scope+` .uc-ref-option'),function(e){return e.textContent;})`,
		&labels,
	)); err != nil {
		t.Fatalf("read narrowed options: %v", err)
	}
	if len(labels) != 1 || labels[0] != "Finance" {
		t.Fatalf("expected the type-ahead to narrow to exactly [Finance], got %v", labels)
	}

	// Choose it (options are already showing), and confirm the hidden id
	// is now set (non-empty).
	if err := chromedp.Run(ctx, chromedp.Click(scope+` .uc-ref-option`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the Finance option: %v", err)
	}
	if referenceHiddenValue(t, ctx, "parent_department_id") == "" {
		t.Fatal("expected a non-empty hidden id after choosing an option")
	}

	// Now EDIT the search text. The stale-reference guard must blank the
	// hidden id immediately — the label the user is now typing no longer
	// corresponds to the previously chosen id.
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(scope+` .uc-ref-search`, "x", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("edit search after pick: %v", err)
	}
	if got := referenceHiddenValue(t, ctx, "parent_department_id"); got != "" {
		t.Fatalf("editing the search text after a pick must clear the hidden id, still had %q", got)
	}
}
