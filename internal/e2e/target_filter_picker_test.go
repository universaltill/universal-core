package e2e

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/projects"
)

// TestReferencePicker_TargetFilterOnlyOffersEmployees is the real-browser
// proof of uc-infra#78's picker requirement: TimeEntry.employee_id
// declares a TargetFilter requiring the referenced Party to hold the
// employee PartyRole, and that must narrow the reference-picker's
// SEARCH RESULTS themselves — not just be enforced after the fact on
// save (internal/kernel/crud/target_constraints_test.go already proves
// the save-time rejection at the engine level; this proves the picker a
// human actually types into never offers the disallowed option in the
// first place). Two Party records share the same "Acme" name prefix —
// one holds the employee PartyRole, one holds only the vendor
// PartyRole — so a query that would otherwise match both must narrow to
// exactly the employee.
func TestReferencePicker_TargetFilterOnlyOffersEmployees(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := projects.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.PublishForms: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	employee, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"party_type": "person", "name": "Acme Employee", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed employee Party: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": employee.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("seed employee PartyRole: %v", err)
	}
	vendor, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"party_type": "organization", "name": "Acme Vendor Co", "status": "active",
	}, actor)
	if err != nil {
		t.Fatalf("seed vendor Party: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": vendor.ID, "role_type": "vendor",
	}, actor); err != nil {
		t.Fatalf("seed vendor PartyRole: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	scope := `.uc-ref[data-field="employee_id"]`
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/TimeEntry/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		// "Acme" alone would match BOTH the employee and the vendor
		// Party on a plain label search — proving the narrowing comes
		// from the declared TargetFilter, not from the query text
		// happening to disambiguate them.
		chromedp.SendKeys(scope+` .uc-ref-search`, "Acme", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'Acme' into employee_id: %v", err)
	}

	var labels []string
	if err := chromedp.Run(bctx, chromedp.Evaluate(
		`Array.prototype.map.call(document.querySelectorAll('`+scope+` .uc-ref-option'),function(e){return e.textContent;})`,
		&labels,
	)); err != nil {
		t.Fatalf("read narrowed options: %v", err)
	}
	if len(labels) != 1 || labels[0] != "Acme Employee" {
		t.Fatalf("expected the employee-role TargetFilter to narrow results to exactly [Acme Employee], got %v", labels)
	}
}

// TestReferencePicker_MustMatchParentFieldOnlyOffersSameProjectTasks is
// the real-browser proof of Task.parent_task_id's
// MustMatchParentField: "project_id" — the picker must only offer a
// parent task from the SAME project as the task being edited, reading
// the sibling project_id value live off the form (formrender's
// data-must-match-field attribute), not just reject a cross-project
// pick after save.
func TestReferencePicker_MustMatchParentFieldOnlyOffersSameProjectTasks(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	if err := projects.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.PublishForms: %v", err)
	}
	if err := projects.PublishStatuses(ctx, tenantDB, actor); err != nil {
		t.Fatalf("projects.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(tenantDB)
	plannedStatusID := publishedStatusID(t, tenantDB, "project_status", "planned")
	projectA, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-A", "name": map[string]any{"en": "Project A"}, "start_date": "2026-01-01",
		"status_id": plannedStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed Project A: %v", err)
	}
	projectB, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-B", "name": map[string]any{"en": "Project B"}, "start_date": "2026-01-01",
		"status_id": plannedStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed Project B: %v", err)
	}
	todoStatusID := publishedStatusID(t, tenantDB, "task_status", "todo")
	taskInA, err := engine.Create(ctx, projects.Task(), map[string]any{
		"project_id": projectA.ID, "title": map[string]any{"en": "Shared Task Name"}, "status_id": todoStatusID,
	}, actor)
	if err != nil {
		t.Fatalf("seed task in project A: %v", err)
	}
	if _, err := engine.Create(ctx, projects.Task(), map[string]any{
		"project_id": projectB.ID, "title": map[string]any{"en": "Shared Task Name"}, "status_id": todoStatusID,
	}, actor); err != nil {
		t.Fatalf("seed task in project B: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	scope := `.uc-ref[data-field="parent_task_id"]`
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/forms/Task/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		// Pick project_id = Project A first, so the sibling value the
		// picker reads off the form is set before searching for a
		// parent task.
		chromedp.SendKeys(`.uc-ref[data-field="project_id"] .uc-ref-search`, "Project A", chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-ref[data-field="project_id"] .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead project_id 'Project A': %v", err)
	}
	if err := chromedp.Run(bctx, chromedp.Click(`.uc-ref[data-field="project_id"] .uc-ref-option`, chromedp.ByQuery)); err != nil {
		t.Fatalf("click Project A option: %v", err)
	}

	if err := chromedp.Run(bctx,
		// Both seeded tasks share the exact same title (Task's label
		// field, "title", is FieldI18nText — the picker's own label
		// search degrades to an unfiltered listing for it, same as any
		// i18n_text-labeled target, so this query only serves to trigger
		// the debounced fetch; MustMatchParentField's own EqualsFilters
		// narrowing is independent of the label search and is what this
		// test actually proves).
		chromedp.SendKeys(scope+` .uc-ref-search`, "Shared", chromedp.ByQuery),
		chromedp.WaitVisible(scope+` .uc-ref-option`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("type-ahead 'Shared' into parent_task_id: %v", err)
	}

	var count int
	if err := chromedp.Run(bctx, chromedp.Evaluate(
		`document.querySelectorAll('`+scope+` .uc-ref-option').length`,
		&count,
	)); err != nil {
		t.Fatalf("count narrowed options: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected MustMatchParentField to narrow parent_task_id results to exactly 1 (the same-project task), got %d", count)
	}

	var gotID string
	if err := chromedp.Run(bctx, chromedp.AttributeValue(scope+` .uc-ref-option`, "data-id", &gotID, nil, chromedp.ByQuery)); err != nil {
		t.Fatalf("read narrowed option's data-id: %v", err)
	}
	if gotID != taskInA.ID {
		t.Fatalf("expected the narrowed option to be the project-A task %s, got %s", taskInA.ID, gotID)
	}
}
