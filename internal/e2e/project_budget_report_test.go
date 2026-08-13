package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
)

// TestProjectBudgetReport_RealBrowser (uc-infra#187) mirrors
// rfq_report_test.go's own structure: seed a real Project with budget
// lines, a Task, and TimeEntry rows (one priced, one unpriced) through
// the real server, drive chromedp to the real
// /reports/project-budget/{id} URL, and assert the rendered page's real
// DOM text content — proving the route, the real i18n catalog, and the
// real stylesheet load together end to end, not just that the handler's
// own return value contains the right substrings (internal/api's own
// TestProjectBudgetReport_* tests already cover that layer).
//
// No computed-style/CSS-behavior assertion here, unlike
// TestRFQComparisonReport_RealBrowser's cheapest-price marking: this
// page is plain server-rendered HTML with no client-side interactivity
// and no CSS-driven state (same "no browser-only bug class to hide
// here" reasoning renderPurchasingReport's own doc comment already
// gives for skipping that class of assertion) — this test's job is
// confirming the real page loads and reads correctly, not proving a
// stylesheet rule applies.
func TestProjectBudgetReport_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"projects", func() error { return projects.Publish(ctx, tenantDB, actor) }},
		{"projects statuses", func() error { return projects.PublishStatuses(ctx, tenantDB, actor) }},
		{"hr", func() error { return hr.Publish(ctx, tenantDB, actor) }},
		{"hr statuses", func() error { return hr.PublishStatuses(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	engine := crud.NewEngine(tenantDB)
	planned := publishedStatusID(t, tenantDB, "project_status", "planned")
	todo := publishedStatusID(t, tenantDB, "task_status", "todo")
	probation := publishedStatusID(t, tenantDB, "employee_status", "probation")

	project, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-E2E-BUDGET", "name": map[string]any{"en": "E2E Budget Project"},
		"start_date": "2026-01-01", "status_id": planned,
	}, actor)
	if err != nil {
		t.Fatalf("seed Project: %v", err)
	}
	if _, err := engine.Create(ctx, projects.ProjectBudgetLine(), map[string]any{
		"project_id": project.ID, "category": "labour", "planned_amount": 500.0,
	}, actor); err != nil {
		t.Fatalf("seed labour ProjectBudgetLine: %v", err)
	}
	if _, err := engine.Create(ctx, projects.ProjectBudgetLine(), map[string]any{
		"project_id": project.ID, "category": "materials", "planned_amount": 200.0,
	}, actor); err != nil {
		t.Fatalf("seed materials ProjectBudgetLine: %v", err)
	}

	task, err := engine.Create(ctx, projects.Task(), map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "E2E Work"}, "status_id": todo,
	}, actor)
	if err != nil {
		t.Fatalf("seed Task: %v", err)
	}

	alice, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Alice", "party_type": "person",
	}, actor)
	if err != nil {
		t.Fatalf("seed Party Alice: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": alice.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("grant employee PartyRole: %v", err)
	}
	if _, err := engine.Create(ctx, hr.Employee(), map[string]any{
		"employee_number": "E2E-ALICE", "party_id": alice.ID, "hire_date": "2020-01-01",
		"status_id": probation, "cost_rate": int64(2500), // $25.00/hr
	}, actor); err != nil {
		t.Fatalf("seed Employee Alice: %v", err)
	}
	if _, err := engine.Create(ctx, projects.TimeEntry(), map[string]any{
		"task_id": task.ID, "employee_id": alice.ID, "entry_date": "2026-02-01", "hours": 2.0,
	}, actor); err != nil {
		t.Fatalf("seed TimeEntry: %v", err)
	}

	// Bob has an Employee record with no cost_rate set — his hour is the
	// real, unpriced case the labour row's note must surface.
	bob, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Bob", "party_type": "person",
	}, actor)
	if err != nil {
		t.Fatalf("seed Party Bob: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": bob.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("grant employee PartyRole (Bob): %v", err)
	}
	if _, err := engine.Create(ctx, hr.Employee(), map[string]any{
		"employee_number": "E2E-BOB", "party_id": bob.ID, "hire_date": "2021-01-01",
		"status_id": probation,
	}, actor); err != nil {
		t.Fatalf("seed Employee Bob: %v", err)
	}
	if _, err := engine.Create(ctx, projects.TimeEntry(), map[string]any{
		"task_id": task.ID, "employee_id": bob.ID, "entry_date": "2026-02-01", "hours": 1.0,
	}, actor); err != nil {
		t.Fatalf("seed TimeEntry Bob: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/project-budget/"+project.ID),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/project-budget/%s: %v", project.ID, err)
	}

	for _, want := range []string{
		"PRJ-E2E-BUDGET",
		"Labour", "Materials",
		"500.00", "50.00", "450.00", // labour planned/actual/variance
		"200.00", // materials planned
		"Not available",
		"some logged hours could not be priced",
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("report page missing %q; body text:\n%s", want, bodyText)
		}
	}

	// The real DOM/CSS proof (CLAUDE.md: a rendered-HTML-string test
	// never proves CSS is actually applied): the unpriced-note span must
	// carry a real, distinct computed style, not just the class name —
	// same reasoning TestRFQComparisonReport_RealBrowser's own
	// .uc-rfq-lowest check gives.
	var noteStyle, plainStyle string
	if err := chromedp.Run(bctx,
		chromedp.Evaluate(`getComputedStyle(document.querySelector('.uc-project-budget-unpriced')).fontStyle`, &noteStyle),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('table.uc-table tbody td')).fontStyle`, &plainStyle),
	); err != nil {
		t.Fatalf("inspect unpriced-note styling: %v", err)
	}
	if noteStyle != "italic" {
		t.Errorf(".uc-project-budget-unpriced font-style = %q, want \"italic\"", noteStyle)
	}
	if plainStyle == noteStyle {
		t.Errorf("expected the unpriced note to resolve a real, distinct style from a plain cell, both got %q", plainStyle)
	}
}

// TestProjectBudgetReport_PlannedRedacted_RealBrowser (uc-infra#216) is
// the real-browser proof that hiding ProjectBudgetLine.planned_amount
// via a FieldPermission removes the real "500.00"/"200.00" planned
// figures from the live DOM entirely — not merely from a rendered-HTML-
// string assertion (internal/api's own HTTP-level test for this fix) —
// while the labour row's Actual (a genuinely separate source) still
// reaches the page.
func TestProjectBudgetReport_PlannedRedacted_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := context.Background()
	actor := humanActor()

	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"projects", func() error { return projects.Publish(ctx, tenantDB, actor) }},
		{"projects statuses", func() error { return projects.PublishStatuses(ctx, tenantDB, actor) }},
		{"hr", func() error { return hr.Publish(ctx, tenantDB, actor) }},
		{"hr statuses", func() error { return hr.PublishStatuses(ctx, tenantDB, actor) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("publish %s: %v", step.name, err)
		}
	}

	// Hide ProjectBudgetLine.planned_amount from the browser's own actor
	// BEFORE seeding data, same ordering seed_field_permission_test.go's
	// own tests use — the FieldPermission only needs to exist by the
	// time the report is requested, not before the budget lines are.
	seedFieldPermission(t, tenantDB, "e2e_budget_redacted", "ProjectBudgetLine", "planned_amount")

	engine := crud.NewEngine(tenantDB)
	planned := publishedStatusID(t, tenantDB, "project_status", "planned")
	todo := publishedStatusID(t, tenantDB, "task_status", "todo")
	probation := publishedStatusID(t, tenantDB, "employee_status", "probation")

	project, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-E2E-REDACT", "name": map[string]any{"en": "E2E Redacted Budget Project"},
		"start_date": "2026-01-01", "status_id": planned,
	}, actor)
	if err != nil {
		t.Fatalf("seed Project: %v", err)
	}
	if _, err := engine.Create(ctx, projects.ProjectBudgetLine(), map[string]any{
		"project_id": project.ID, "category": "labour", "planned_amount": 500.0,
	}, actor); err != nil {
		t.Fatalf("seed labour ProjectBudgetLine: %v", err)
	}

	task, err := engine.Create(ctx, projects.Task(), map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "E2E Redacted Work"}, "status_id": todo,
	}, actor)
	if err != nil {
		t.Fatalf("seed Task: %v", err)
	}
	alice, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "E2E Redact Alice", "party_type": "person",
	}, actor)
	if err != nil {
		t.Fatalf("seed Party Alice: %v", err)
	}
	if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
		"party_id": alice.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("grant employee PartyRole: %v", err)
	}
	if _, err := engine.Create(ctx, hr.Employee(), map[string]any{
		"employee_number": "E2E-REDACT-ALICE", "party_id": alice.ID, "hire_date": "2020-01-01",
		"status_id": probation, "cost_rate": int64(2500), // $25.00/hr
	}, actor); err != nil {
		t.Fatalf("seed Employee Alice: %v", err)
	}
	if _, err := engine.Create(ctx, projects.TimeEntry(), map[string]any{
		"task_id": task.ID, "employee_id": alice.ID, "entry_date": "2026-02-01", "hours": 2.0,
	}, actor); err != nil {
		t.Fatalf("seed TimeEntry: %v", err)
	}

	bctx := browserCtx(t, tenantID)
	var bodyText string
	if err := chromedp.Run(bctx,
		chromedp.Navigate(srv.URL+"/reports/project-budget/"+project.ID),
		chromedp.WaitVisible(`table.uc-table`, chromedp.ByQuery),
		chromedp.Text(`body`, &bodyText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open /reports/project-budget/%s: %v", project.ID, err)
	}

	if strings.Contains(bodyText, "500.00") {
		t.Errorf("the real planned amount reached the live page for an actor with planned_amount redacted:\n%s", bodyText)
	}
	if !strings.Contains(bodyText, "50.00") {
		t.Errorf("labour's Actual (50.00, a separate source from planned_amount) should still render:\n%s", bodyText)
	}

	// A presence-only check ("Not available" appears somewhere) is not
	// enough to prove the Variance cell specifically shows it (internal/
	// api's own HTTP-level regression for this exact gap, uc-infra#216
	// independent review): a template regression that silently drops the
	// Variance guard's PlannedAvailable half renders that cell as
	// genuinely EMPTY, not "Not available" — and an empty string is
	// still present nowhere near this bodyText check either way, so a
	// bare Contains("Not available") would stay green through it (it
	// only proves the labour row's Planned cell, which is the first of
	// the two affected cells, shows the right text). ProjectBudgetActuals
	// orders rows by category (data/reporting.go), so labour is
	// tbody's first row; its cells are Category/Planned/Actual/Variance
	// in that fixed order (the template's own column order) — index 3 is
	// Variance.
	var labourVarianceText string
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.querySelectorAll('table.uc-table tbody tr')[0].querySelectorAll('td')[3].textContent`,
		&labourVarianceText,
	)); err != nil {
		t.Fatalf("read the labour row's Variance cell: %v", err)
	}
	if labourVarianceText != "Not available" {
		t.Errorf(`labour row's Variance cell = %q, want "Not available" (it must not render blank, and must not render a fabricated number)`, labourVarianceText)
	}

	// Belt-and-braces, same reasoning as the master-detail redaction
	// test's own leak check: confirm the figure isn't sitting anywhere
	// in the document outside the visible text either.
	var leaks bool
	if err := chromedp.Run(bctx, chromedp.EvaluateAsDevTools(
		`document.documentElement.outerHTML.includes("500.00")`, &leaks,
	)); err != nil {
		t.Fatalf("scan document for the redacted planned amount: %v", err)
	}
	if leaks {
		t.Fatal("redacted planned amount reached the browser somewhere in the document")
	}
}
