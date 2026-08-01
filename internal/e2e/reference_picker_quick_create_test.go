package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// TestReferencePickerQuickCreate_CreatesAndSelectsWithoutLosingParentForm
// is the real-browser test for part 2 of #24 (universaltill/uc-infra#51):
// a "+ Create new {Entity}" affordance on the reference picker that opens
// the target entity's own generated create form in a modal, creates it,
// and selects it back into the picker — all without navigating the
// parent form away or losing its in-progress edits.
//
// Driven exactly the way a person would use it: type into the OUTER
// Department form first (so there is something at stake to lose), open
// the parent_department_id picker's quick-create modal, fill in and
// submit the INNER Department form it renders, and assert three things a
// rendered-HTML-string test could never prove: the modal actually closes,
// the outer form's own fields are untouched, and the picker now holds
// the real id of a record that did not exist a moment ago (not just
// some arbitrary non-empty string).
func TestReferencePickerQuickCreate_CreatesAndSelectsWithoutLosingParentForm(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		// Something at stake in the OUTER form before the modal ever opens.
		chromedp.SetValue(`input[name="code"]`, "hq", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Headquarters", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("seed outer Department form: %v", err)
	}

	scope := `.uc-ref[data-field="parent_department_id"]`
	var createLabel string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(scope+` .uc-ref-create`, chromedp.ByQuery),
		chromedp.Text(scope+` .uc-ref-create`, &createLabel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("find quick-create button: %v", err)
	}
	if createLabel != "+ Create new Department" {
		t.Fatalf("expected the quick-create button labelled %q, got %q", "+ Create new Department", createLabel)
	}

	if err := chromedp.Run(ctx,
		chromedp.Click(scope+` .uc-ref-create`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-quick-create[open]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-quick-create-body form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open quick-create modal: %v", err)
	}

	// The modal's own form is a second, independent Department create
	// form — scoped selectors throughout, since an unscoped
	// `input[name="code"]` would now match two elements (the outer form
	// is still in the DOM, untouched, behind the modal).
	if err := chromedp.Run(ctx,
		chromedp.SetValue(`#uc-quick-create-body input[name="code"]`, "reg", chromedp.ByQuery),
		chromedp.SetValue(`#uc-quick-create-body input[name="name"]`, "Regional Office", chromedp.ByQuery),
		clickAndSettle(`#uc-quick-create-body form.uc-form button[type="submit"]`),
	); err != nil {
		t.Fatalf("submit quick-create modal: %v", err)
	}

	// The modal closes and its body is cleared — a stale form must not
	// sit there for the next quick-create to accidentally reuse.
	var dialogOpen bool
	var bodyHTML string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById("uc-quick-create").open`, &dialogOpen),
		chromedp.Evaluate(`document.getElementById("uc-quick-create-body").innerHTML`, &bodyHTML),
	); err != nil {
		t.Fatalf("read modal state after submit: %v", err)
	}
	if dialogOpen {
		t.Fatal("expected the quick-create dialog to close after a successful create")
	}
	if bodyHTML != "" {
		t.Fatalf("expected the quick-create dialog body to be cleared after close, got %q", bodyHTML)
	}

	// The hidden id and visible label are read from the picker's own
	// inputs — i.e. whatever the modal JS wrote into them. That alone
	// doesn't prove a Department record actually exists server-side with
	// that id: the stronger proof (the outer form actually SAVING with
	// this reference and it resolving back to "Regional Office" on a
	// fresh page load) follows below.
	gotID := referenceHiddenValue(t, ctx, "parent_department_id")
	if gotID == "" {
		t.Fatal("expected the parent_department_id picker to hold a new record id after quick-create")
	}
	var gotLabel string
	if err := chromedp.Run(ctx, chromedp.Value(scope+` .uc-ref-search`, &gotLabel, chromedp.ByQuery)); err != nil {
		t.Fatalf("read picker search value: %v", err)
	}
	if gotLabel != "Regional Office" {
		t.Fatalf("expected the picker to show the newly created record's name %q, got %q", "Regional Office", gotLabel)
	}

	// The outer form's own in-progress edits, entered before the modal
	// ever opened, must still be exactly what was typed — the whole
	// point of a modal over a full navigation.
	var outerCode, outerName string
	if err := chromedp.Run(ctx,
		chromedp.Value(`input[name="code"]`, &outerCode, chromedp.ByQuery),
		chromedp.Value(`input[name="name"]`, &outerName, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read outer form fields after quick-create: %v", err)
	}
	if outerCode != "hq" || outerName != "Headquarters" {
		t.Fatalf("expected the outer form's own fields to survive the quick-create untouched, got code=%q name=%q", outerCode, outerName)
	}

	// The real proof the quick-created record isn't just client-side
	// JS state: save the OUTER form (which now references it), reload
	// its edit page from scratch, and confirm the reference resolved on
	// a completely fresh page load — the same round trip
	// TestReferencePicker_SearchNarrowsAndGuardsStaleID's own combobox
	// already proves for a picked EXISTING record, now for one this
	// test just quick-created.
	if err := chromedp.Run(ctx, submitForm()); err != nil {
		t.Fatalf("submit outer Department form: %v", err)
	}
	outerID := savedRecordID(t, ctx, "Department")
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/"+outerID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("reload saved outer Department: %v", err)
	}
	var reloadedLabel string
	if err := chromedp.Run(ctx, chromedp.Value(scope+` .uc-ref-search`, &reloadedLabel, chromedp.ByQuery)); err != nil {
		t.Fatalf("read reloaded picker search value: %v", err)
	}
	if reloadedLabel != "Regional Office" {
		t.Fatalf("expected the reloaded parent_department_id picker to resolve the quick-created record's real, persisted name, got %q", reloadedLabel)
	}
	if got := referenceHiddenValue(t, ctx, "parent_department_id"); got != gotID {
		t.Fatalf("expected the persisted parent_department_id to be the same id the quick-create wrote, got %q want %q", got, gotID)
	}
}

// TestReferencePickerQuickCreate_HiddenWithoutCreatePermission proves the
// RBAC gate is real, not cosmetic: an actor who can create a Position but
// cannot create a Department must not see a "create a Department inline"
// affordance on Position's department_id picker at all. Denying the
// button on the CLIENT would be trivially bypassable (nothing stops a
// client from just POSTing to /forms/Department/new directly); the actual
// enforcement is renderForm's own CanWrite gate on that route (same one
// guards full-page navigation to /forms/Department/new) — this test's
// job is only to confirm the affordance a legitimate user sees agrees
// with what they're actually allowed to do.
func TestReferencePickerQuickCreate_HiddenWithoutCreatePermission(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	// Opts Department into RBAC (ADR-0006) with read but not write for
	// the browser's own actor — Position itself is deliberately left
	// un-opted-in (full default access), so the actor can still reach
	// /forms/Position/new at all; only the REFERENCED entity is denied.
	seedEntityPermission(t, tenantDB, "e2e_position_clerk", "Department", true, false)
	ctx := browserCtx(t, tenantID)

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate to Position/new: %v", err)
	}

	var buttonCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.querySelectorAll('.uc-ref[data-field="department_id"] .uc-ref-create').length`,
		&buttonCount,
	)); err != nil {
		t.Fatalf("count quick-create buttons: %v", err)
	}
	if buttonCount != 0 {
		t.Fatalf("expected no quick-create button on department_id without Department create permission, found %d", buttonCount)
	}

	// The search/select half of the picker (part 1, #24) must still work
	// normally — this actor can READ Department, just not create one —
	// so denying quick-create must not have broken the rest of the
	// widget.
	if err := chromedp.Run(ctx, chromedp.WaitVisible(`.uc-ref[data-field="department_id"] .uc-ref-search`, chromedp.ByQuery)); err != nil {
		t.Fatalf("expected the department_id search box to still render: %v", err)
	}
}

// TestReferencePickerQuickCreate_NestedClickIgnoredAndLabelsScopedToModal
// covers two bugs this feature's own independent review found and no
// other test caught:
//
//  1. Department self-references Department (parent_department_id), so
//     the modal's OWN fetched form renders its own nested
//     ".uc-ref-create" button. Before the fix, clicking it reassigned
//     the single shared quickCreateBox and immediately reset the
//     dialog body's innerHTML — silently discarding whatever the
//     outer quick-create was mid-way through, with no error and no
//     visible sign anything went wrong. The same guard also closes the
//     sibling race (open picker A, then click picker B before A's
//     fetch resolves) — not independently reproducible from a single
//     browser tab in a deterministic test, so this covers the nested
//     case, which is.
//  2. The modal's fetched fragment reuses formrender's normal
//     id="{fieldName}" convention — identical to the outer form's own
//     field ids, since it's the same Department entity twice. Before
//     the id-prefixing fix, <label for="name"> inside the modal
//     resolved (by document order) to the OUTER form's input instead
//     of the modal's own, so clicking a label inside the quick-create
//     modal focused nothing useful.
func TestReferencePickerQuickCreate_NestedClickIgnoredAndLabelsScopedToModal(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	ctx := browserCtx(t, tenantID)

	scope := `.uc-ref[data-field="parent_department_id"]`
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Click(scope+` .uc-ref-create`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-quick-create[open]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#uc-quick-create-body form.uc-form`, chromedp.ByQuery),
		// In-progress edits inside the modal — the thing at stake if a
		// nested click wrongly resets the dialog body.
		chromedp.SetValue(`#uc-quick-create-body input[name="code"]`, "reg", chromedp.ByQuery),
		chromedp.SetValue(`#uc-quick-create-body input[name="name"]`, "Regional Office", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open modal and seed its form: %v", err)
	}

	// The modal's OWN parent_department_id picker (Department
	// self-references itself) has its own nested quick-create button —
	// confirm it's really there before proving the click on it is a
	// no-op, so a selector typo can't make this pass vacuously.
	nestedScope := `#uc-quick-create-body ` + scope
	var nestedButtonCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.querySelectorAll(`+"`"+nestedScope+` .uc-ref-create`+"`"+`).length`, &nestedButtonCount,
	)); err != nil {
		t.Fatalf("count nested quick-create buttons: %v", err)
	}
	if nestedButtonCount != 1 {
		t.Fatalf("expected exactly one nested quick-create button inside the modal, found %d", nestedButtonCount)
	}

	if err := chromedp.Run(ctx, chromedp.Click(nestedScope+` .uc-ref-create`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the nested quick-create button: %v", err)
	}

	// The nested click must be a complete no-op: the dialog stays open,
	// the SAME modal form (with the values just typed) is still there
	// — not reset, not replaced by a second fetch.
	var dialogOpen bool
	var code, name string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById("uc-quick-create").open`, &dialogOpen),
		chromedp.Value(`#uc-quick-create-body input[name="code"]`, &code, chromedp.ByQuery),
		chromedp.Value(`#uc-quick-create-body input[name="name"]`, &name, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read modal state after nested click: %v", err)
	}
	if !dialogOpen {
		t.Fatal("expected the dialog to remain open after an ignored nested quick-create click")
	}
	if code != "reg" || name != "Regional Office" {
		t.Fatalf("expected the modal's in-progress edits to survive the ignored nested click, got code=%q name=%q", code, name)
	}

	// Clicking the modal's OWN <label> for the name field must focus
	// the MODAL's input, not the outer form's identically-named-and-id
	// one sitting behind the dialog.
	if err := chromedp.Run(ctx, chromedp.Click(`#uc-quick-create-body label[for$="name"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click the modal's own name label: %v", err)
	}
	var focusedInModal bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.getElementById("uc-quick-create-body").contains(document.activeElement)`, &focusedInModal,
	)); err != nil {
		t.Fatalf("read document.activeElement: %v", err)
	}
	if !focusedInModal {
		t.Fatal("expected clicking the modal's own label to focus an input inside the modal, not the outer form's identically-id'd one")
	}
}
