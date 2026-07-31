package e2e

import (
	"context"
	"path"
	"regexp"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// savedRecordID runs a submitted uc-form's post-save assertions: confirms
// the form's hx-post attribute actually exists (chromedp.AttributeValue's
// ok output — a missing attribute otherwise silently yields "", which
// path.Base turns into the misleading value "." rather than a clear
// failure) and that it matches the real create->edit route shape
// (/api/records/{entityType}/{id}), not just "not exactly the create
// route" (a typo'd or truncated href would pass a bare inequality check
// too). Returns the new record's id.
func savedRecordID(t *testing.T, ctx context.Context, entityType string) string {
	t.Helper()
	var href string
	var ok bool
	if err := chromedp.Run(ctx, chromedp.AttributeValue(`form.uc-form`, "hx-post", &href, &ok, chromedp.ByQuery)); err != nil {
		t.Fatalf("read hx-post after %s save: %v", entityType, err)
	}
	if !ok {
		t.Fatalf("expected the %s form to have an hx-post attribute after save, found none", entityType)
	}
	if !regexp.MustCompile(`^/api/records/` + entityType + `/[^/]+$`).MatchString(href) {
		t.Fatalf("expected the %s form to target its own record id (create -> edit) via /api/records/%s/{id}, got: %s", entityType, entityType, href)
	}
	return path.Base(href)
}

// pickReference drives the searchable reference combobox (#24) that
// replaced the old <select> for reference fields. The real user gesture is
// now: type a query into the field's search box, wait for the debounced
// async /api/references results to render as .uc-ref-option divs, then
// click the option whose label matches. chromedp.SetValue on a <select>
// no longer applies because a reference field is no longer a <select> —
// this helper is what every reference interaction in this package now goes
// through, exercising the endpoint + JS + DOM the way a person would.
func pickReference(t *testing.T, ctx context.Context, field, query, label string) {
	t.Helper()
	scope := `.uc-ref[data-field="` + cssAttr(field) + `"]`
	searchSel := scope + ` .uc-ref-search`
	optionSel := scope + ` .uc-ref-option`
	// find(...).click() — a missing match throws a clear TypeError that
	// surfaces as the Evaluate error below, rather than silently picking
	// nothing and letting a later "field didn't persist" assertion take
	// the blame.
	clickJS := `Array.prototype.find.call(` +
		`document.querySelectorAll(` + strconv.Quote(optionSel) + `),` +
		`function(e){return e.textContent===` + strconv.Quote(label) + `;}).click()`
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(searchSel, query, chromedp.ByQuery),
		chromedp.WaitVisible(optionSel, chromedp.ByQuery),
		chromedp.Evaluate(clickJS, nil),
	); err != nil {
		t.Fatalf("pick reference %s=%q (query %q): %v", field, label, query, err)
	}
}

// cssAttr escapes a field name for safe interpolation into a CSS attribute
// selector. Field names in this kernel are plain identifiers, so this is
// belt-and-braces, not a live concern — but a selector built by string
// concatenation should never assume its input.
func cssAttr(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

// referenceHiddenValue reads the id a reference combobox actually holds —
// the hidden input the form submits, not the visible search text. This is
// the equivalent of the old chromedp.Value(select) read, now that the id
// lives in a hidden input beside the search box.
func referenceHiddenValue(t *testing.T, ctx context.Context, field string) string {
	t.Helper()
	var v string
	sel := `.uc-ref[data-field="` + cssAttr(field) + `"] input[type="hidden"]`
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(sel, &v, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read reference hidden value for %s: %v", field, err)
	}
	return v
}

// TestDepartmentPosition_RealBrowser drives the generated Department and
// Position forms through a real headless browser — the org-chart entities
// added alongside Party in foundation.All()/AllForms() (`erp/BACKLOG-
// TASKS.md`'s "Department/org-chart model" task). A rendered-HTML-string
// test would prove the form markup exists; this proves the real reference
// pickers (Department.parent_department_id, Position.department_id) — now
// the searchable combobox from #24 — actually resolve to real records
// created moments earlier and persist across a fresh page load, the same
// bar TestFormSaveButton_RealBrowser already set for Item.
//
// testServer() only publishes purchasing's forms by default (its own doc
// comment) — foundation.PublishForms is called explicitly here so
// /forms/Department/new and /forms/Position/new are reachable at all,
// rather than 404ing the way they do in every other e2e test that
// doesn't need them.
func TestDepartmentPosition_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	if err := foundation.PublishForms(context.Background(), tenantDB, humanActor()); err != nil {
		t.Fatalf("foundation.PublishForms: %v", err)
	}
	ctx := browserCtx(t, tenantID)

	// Create a top-level Department (no parent) through the real form.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="code"]`, "co", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Company", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save parent Department: %v", err)
	}
	parentDeptID := savedRecordID(t, ctx, "Department")

	// Create a child Department that references the parent through the
	// real combobox self-reference picker — not a hardcoded id, the one
	// this browser session actually created above, found by typing.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="code"]`, "fin", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Finance", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("fill child Department: %v", err)
	}
	pickReference(t, ctx, "parent_department_id", "Company", "Company")
	if err := chromedp.Run(ctx, submitForm()); err != nil {
		t.Fatalf("save child Department: %v", err)
	}
	departmentID := savedRecordID(t, ctx, "Department")

	// Reload the child Department's own edit URL fresh (a new navigation,
	// not an htmx swap) to confirm parent_department_id actually
	// persisted to Postgres, not just reflected client-side.
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Department/"+departmentID)); err != nil {
		t.Fatalf("re-navigate to saved child Department: %v", err)
	}
	if got := referenceHiddenValue(t, ctx, "parent_department_id"); got != parentDeptID {
		t.Fatalf("expected the Department's parent_department_id to persist as %q across a fresh page load, got %q", parentDeptID, got)
	}
	// And the picker must show the parent's NAME, not a bare id, on load —
	// the CurrentLabel resolution #24 relies on for an existing record.
	var reloadedLabel string
	if err := chromedp.Run(ctx,
		chromedp.Value(`.uc-ref[data-field="parent_department_id"] .uc-ref-search`, &reloadedLabel, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read parent_department_id label after reload: %v", err)
	}
	if reloadedLabel != "Company" {
		t.Fatalf("expected the reloaded picker to show the parent's name %q, got %q", "Company", reloadedLabel)
	}

	// Create a Position that references the Finance Department through
	// the real combobox reference picker.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="title"]`, "Finance Manager", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("fill new Position: %v", err)
	}
	pickReference(t, ctx, "department_id", "Finance", "Finance")
	if err := chromedp.Run(ctx, submitForm()); err != nil {
		t.Fatalf("save new Position: %v", err)
	}
	managerID := savedRecordID(t, ctx, "Position")

	// Reload the Position's own edit URL fresh to confirm department_id
	// actually persisted to Postgres as the real Department id.
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Position/"+managerID)); err != nil {
		t.Fatalf("re-navigate to saved Position: %v", err)
	}
	if got := referenceHiddenValue(t, ctx, "department_id"); got != departmentID {
		t.Fatalf("expected the Position's department_id to persist as %q across a fresh page load, got %q", departmentID, got)
	}

	// A second Position, reporting to the Finance Manager just created,
	// through the real reports_to_position_id combobox picker.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="title"]`, "Accountant", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("fill reporting Position: %v", err)
	}
	pickReference(t, ctx, "department_id", "Finance", "Finance")
	pickReference(t, ctx, "reports_to_position_id", "Finance Manager", "Finance Manager")
	if err := chromedp.Run(ctx, submitForm()); err != nil {
		t.Fatalf("save reporting Position: %v", err)
	}
	reportID := savedRecordID(t, ctx, "Position")
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Position/"+reportID)); err != nil {
		t.Fatalf("re-navigate to saved reporting Position: %v", err)
	}
	if got := referenceHiddenValue(t, ctx, "reports_to_position_id"); got != managerID {
		t.Fatalf("expected the Position's reports_to_position_id to persist as %q across a fresh page load, got %q", managerID, got)
	}

	// department_id must stay genuinely optional — a company-level
	// Position with no Department at all (independent review's own
	// "CFO reporting to no single department" scenario, the real reason
	// Position.department_id isn't Required — see foundation.go's
	// Position doc comment) must save cleanly with the field left unset,
	// mirroring TestPosition_DepartmentIsOptional's unit-level assertion
	// through the real form/browser path.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="title"]`, "CFO", chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save department-less Position: %v", err)
	}
	cfoID := savedRecordID(t, ctx, "Position")
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Position/"+cfoID)); err != nil {
		t.Fatalf("re-navigate to saved CFO Position: %v", err)
	}
	if got := referenceHiddenValue(t, ctx, "department_id"); got != "" {
		t.Fatalf("expected the department-less Position's department_id to stay empty across a fresh page load, got %q", got)
	}
}
