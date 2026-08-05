package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/projects"
)

// TestProjects_TaskTableRendersLocalizedAndAligned is the browser test
// board #18 shipped without, and the reason its blocker survived the
// whole dev cycle: every other test asserted markup structure, and the
// structure was fine — the CELL CONTENT was a raw Go map.
//
// Task.title is the repo's first i18n_text field on a composition
// child, so the project form's task table is the first place a
// per-locale value has ever been rendered through buildChildRows. It
// used to print "map[en:Discovery tr:Keşif]" — identical in every
// language — and, because columns came from each row's own keys, the
// subtask row (which alone sets parent_task_id) got an extra cell that
// shifted every column after it.
func TestProjects_TaskTableRendersLocalizedAndAligned(t *testing.T) {
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
		{"projects", func() error { return projects.Publish(ctx, tenantDB, actor) }},
		{"projects forms", func() error { return projects.PublishForms(ctx, tenantDB, actor) }},
		{"projects statuses", func() error { return projects.PublishStatuses(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	defs := func(entityType string) *entity.Definition {
		t.Helper()
		v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	status := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, defs("StatusType"), "code", typeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", typeCode)
		}
		rows, _ := engine.ListByField(ctx, defs("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", typeCode, code)
		return ""
	}

	project, err := engine.Create(ctx, defs("Project"), map[string]any{
		"project_code": "PRJ-E2E", "name": map[string]any{"en": "Rollout", "tr": "Kurulum"},
		"start_date": "2026-02-01", "status_id": status("project_status", "active"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}
	taskDef := defs("Task")
	parent, err := engine.Create(ctx, taskDef, map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Discovery", "tr": "Keşif"},
		"estimated_hours": 40.0, "status_id": status("task_status", "in_progress"),
	}, actor)
	if err != nil {
		t.Fatalf("create parent Task: %v", err)
	}
	// The subtask sets parent_task_id where its sibling does not — the
	// ragged-row shape.
	if _, err := engine.Create(ctx, taskDef, map[string]any{
		"project_id": project.ID, "parent_task_id": parent.ID,
		"title":           map[string]any{"en": "Interviews", "tr": "Görüşmeler"},
		"estimated_hours": 12.0, "status_id": status("task_status", "todo"),
	}, actor); err != nil {
		t.Fatalf("create subtask: %v", err)
	}

	browser := browserCtx(t, tenantID)
	var tableText string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Project/"+project.ID),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.WaitVisible(`section[data-component="master_detail"] table`, chromedp.ByQuery),
		chromedp.Text(`section[data-component="master_detail"] table`, &tableText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render project form: %v", err)
	}

	if strings.Contains(tableText, "map[") {
		t.Errorf("a raw Go map is rendered in the task table:\n%s", tableText)
	}
	if !strings.Contains(tableText, "Discovery") || !strings.Contains(tableText, "Interviews") {
		t.Errorf("task titles missing from the table:\n%s", tableText)
	}

	// Every row has the same number of cells as the header — the
	// alignment property, measured in a real DOM rather than inferred
	// from markup.
	var cellCounts []int
	if err := chromedp.Run(browser, chromedp.Evaluate(`
		(() => {
			const t = document.querySelector('section[data-component="master_detail"] table');
			const head = t.querySelectorAll('thead th').length;
			const rows = [...t.querySelectorAll('tbody tr')].map(r => r.querySelectorAll('td').length);
			return [head, ...rows];
		})()`, &cellCounts)); err != nil {
		t.Fatalf("count cells: %v", err)
	}
	if len(cellCounts) < 3 {
		t.Fatalf("expected a header and two rows, got %v", cellCounts)
	}
	for i, n := range cellCounts {
		if n != cellCounts[0] {
			t.Errorf("row %d has %d cells, header has %d — columns are misaligned (%v)", i, n, cellCounts[0], cellCounts)
		}
	}

	// Turkish viewer: the same stored record, a different rendered word.
	var turkish string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Project/"+project.ID+"?lang=tr"),
		chromedp.WaitVisible(`section[data-component="master_detail"] table`, chromedp.ByQuery),
		chromedp.Text(`section[data-component="master_detail"] table`, &turkish, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render Turkish project form: %v", err)
	}
	if !strings.Contains(turkish, "Keşif") {
		t.Errorf("Turkish viewer did not get the Turkish task title:\n%s", turkish)
	}
}

// TestProjects_BudgetLinesTableRendersLocalizedWithNoRollUp (uc-infra#79):
// the second master-detail section on ProjectForm, driven end-to-end in
// a real browser rather than the string-level formrender unit tests
// alone (CLAUDE.md: a rendered-HTML-string test proves markup structure,
// never proves the real page actually shows it). Two things this test
// exists to catch that a template-only check can't:
//   - the table is selected by data-target="ProjectBudgetLine" (not
//     position), so it can't accidentally read the Tasks table instead
//     now that the form has two master-detail sections;
//   - the section renders with NO roll-up element at all — the
//     package doc comment's deliberate decision (budget lines are
//     advisory, not derived into/from Project.budget) is what a real
//     page shows, not just what the Definition claims.
func TestProjects_BudgetLinesTableRendersLocalizedWithNoRollUp(t *testing.T) {
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
		{"projects", func() error { return projects.Publish(ctx, tenantDB, actor) }},
		{"projects forms", func() error { return projects.PublishForms(ctx, tenantDB, actor) }},
		{"projects statuses", func() error { return projects.PublishStatuses(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	defs := func(entityType string) *entity.Definition {
		t.Helper()
		v, err := data.NewEntityDefinitionRepo(tenantDB).GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	status := func(typeCode, code string) string {
		t.Helper()
		types, _ := engine.ListByField(ctx, defs("StatusType"), "code", typeCode)
		if len(types) == 0 {
			t.Fatalf("no StatusType %s", typeCode)
		}
		rows, _ := engine.ListByField(ctx, defs("Status"), "status_type_id", types[0].ID)
		for _, r := range rows {
			if c, _ := r.Data["code"].(string); c == code {
				return r.ID
			}
		}
		t.Fatalf("no %s/%s", typeCode, code)
		return ""
	}

	project, err := engine.Create(ctx, defs("Project"), map[string]any{
		"project_code": "PRJ-BUDGET-E2E", "name": map[string]any{"en": "Budget Rollout"},
		"start_date": "2026-02-01", "status_id": status("project_status", "active"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}
	if _, err := engine.Create(ctx, defs("ProjectBudgetLine"), map[string]any{
		"project_id": project.ID, "category": "labour", "planned_amount": 45000.0,
	}, actor); err != nil {
		t.Fatalf("create ProjectBudgetLine: %v", err)
	}

	browser := browserCtx(t, tenantID)
	var headerText, categoryCell, amountCell string
	if err := chromedp.Run(browser,
		chromedp.Navigate(srv.URL+"/forms/Project/"+project.ID+"?lang=tr"),
		chromedp.WaitVisible(`table.uc-master-detail[data-target="ProjectBudgetLine"]`, chromedp.ByQuery),
		chromedp.Text(`table.uc-master-detail[data-target="ProjectBudgetLine"] thead th[data-field="category"]`, &headerText, chromedp.ByQuery),
		chromedp.Text(`table.uc-master-detail[data-target="ProjectBudgetLine"] tbody td[data-field="category"]`, &categoryCell, chromedp.ByQuery),
		chromedp.Text(`table.uc-master-detail[data-target="ProjectBudgetLine"] tbody td[data-field="planned_amount"]`, &amountCell, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("render project form: %v", err)
	}

	if got := strings.TrimSpace(headerText); got != "Kategori" {
		t.Errorf("expected the Turkish translation of the category column header, got %q", got)
	}
	// The CELL value, not just the header, must also be translated —
	// the childCellValue fix this card adds (formrender's own unit
	// tests pin the mechanism; this proves the real page shows it).
	if got := strings.TrimSpace(categoryCell); got != "İşçilik" {
		t.Errorf("expected the Turkish translation of category=labour in the cell, got %q", got)
	}
	if got := strings.TrimSpace(amountCell); got != "45000" {
		t.Errorf("expected the planned_amount cell to show 45000, got %q", got)
	}

	// No roll-up element for this section at all — the deliberate
	// "Project.budget stays advisory, not derived" decision.
	var rollUpCount int
	if err := chromedp.Run(browser, chromedp.Evaluate(`
		document.querySelectorAll('table.uc-master-detail[data-target="ProjectBudgetLine"] ~ p.uc-rollup').length
	`, &rollUpCount)); err != nil {
		t.Fatalf("count roll-up elements: %v", err)
	}
	if rollUpCount != 0 {
		t.Errorf("expected no roll-up element for the Budget Lines section, found %d", rollUpCount)
	}
}
