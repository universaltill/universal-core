package e2e

import (
	"context"
	"path"
	"regexp"
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

// TestDepartmentPosition_RealBrowser drives the generated Department and
// Position forms through a real headless browser — the org-chart entities
// added alongside Party in foundation.All()/AllForms() (`erp/BACKLOG-
// TASKS.md`'s "Department/org-chart model" task). A rendered-HTML-string
// test would prove the form markup exists; this proves the real <select>
// reference pickers (Department.parent_department_id,
// Position.department_id) actually resolve to real records created
// moments earlier and persist across a fresh page load, the same bar
// TestFormSaveButton_RealBrowser already set for Item.
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
	// real <select> self-reference picker — not a hardcoded id, the one
	// this browser session actually created above.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Department/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="code"]`, "fin", chromedp.ByQuery),
		chromedp.SetValue(`input[name="name"]`, "Finance", chromedp.ByQuery),
		chromedp.SetValue(`select[name="parent_department_id"]`, parentDeptID, chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save child Department: %v", err)
	}
	departmentID := savedRecordID(t, ctx, "Department")

	// Reload the child Department's own edit URL fresh (a new navigation,
	// not an htmx swap) to confirm parent_department_id actually
	// persisted to Postgres, not just reflected client-side.
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Department/"+departmentID)); err != nil {
		t.Fatalf("re-navigate to saved child Department: %v", err)
	}
	var reloadedParentID string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`select[name="parent_department_id"]`, &reloadedParentID, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read parent_department_id after reload: %v", err)
	}
	if reloadedParentID != parentDeptID {
		t.Fatalf("expected the Department's parent_department_id to persist as %q across a fresh page load, got %q", parentDeptID, reloadedParentID)
	}

	// Create a Position that references the Finance Department through
	// the real <select> reference picker.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="title"]`, "Finance Manager", chromedp.ByQuery),
		chromedp.SetValue(`select[name="department_id"]`, departmentID, chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save new Position: %v", err)
	}
	managerID := savedRecordID(t, ctx, "Position")

	// Reload the Position's own edit URL fresh to confirm department_id
	// actually persisted to Postgres as the real Department id.
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Position/"+managerID)); err != nil {
		t.Fatalf("re-navigate to saved Position: %v", err)
	}
	var reloadedDeptID string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`select[name="department_id"]`, &reloadedDeptID, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read department_id after reload: %v", err)
	}
	if reloadedDeptID != departmentID {
		t.Fatalf("expected the Position's department_id to persist as %q across a fresh page load, got %q", departmentID, reloadedDeptID)
	}

	// A second Position, reporting to the Finance Manager just created,
	// through the real reports_to_position_id <select> picker.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Position/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.SetValue(`input[name="title"]`, "Accountant", chromedp.ByQuery),
		chromedp.SetValue(`select[name="department_id"]`, departmentID, chromedp.ByQuery),
		chromedp.SetValue(`select[name="reports_to_position_id"]`, managerID, chromedp.ByQuery),
		submitForm(),
	); err != nil {
		t.Fatalf("fill + save reporting Position: %v", err)
	}
	reportID := savedRecordID(t, ctx, "Position")
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/forms/Position/"+reportID)); err != nil {
		t.Fatalf("re-navigate to saved reporting Position: %v", err)
	}
	var reloadedManagerID string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`select[name="reports_to_position_id"]`, &reloadedManagerID, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read reports_to_position_id after reload: %v", err)
	}
	if reloadedManagerID != managerID {
		t.Fatalf("expected the Position's reports_to_position_id to persist as %q across a fresh page load, got %q", managerID, reloadedManagerID)
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
	var reloadedCFODept string
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.Value(`select[name="department_id"]`, &reloadedCFODept, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read department_id after reload for department-less Position: %v", err)
	}
	if reloadedCFODept != "" {
		t.Fatalf("expected the department-less Position's department_id to stay empty across a fresh page load, got %q", reloadedCFODept)
	}
}
