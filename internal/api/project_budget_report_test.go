package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/money"
	"github.com/universaltill/universal-core/internal/kernel/projects"
)

// projectStatusID mirrors rfqStatusID's own two-step, status_type_id-
// scoped lookup (status codes are unique per StatusType, not globally).
func projectStatusID(t *testing.T, db *sql.DB, typeCode, code string) string {
	t.Helper()
	ctx := context.Background()
	engine := crud.NewEngine(db)
	types, err := engine.ListByField(ctx, foundation.StatusType(), "code", typeCode)
	if err != nil || len(types) != 1 {
		t.Fatalf("lookup StatusType %s: %v (n=%d)", typeCode, err, len(types))
	}
	statuses, err := engine.ListByField(ctx, foundation.Status(), "status_type_id", types[0].ID)
	if err != nil {
		t.Fatalf("list Status: %v", err)
	}
	for _, s := range statuses {
		if c, _ := s.Data["code"].(string); c == code {
			return s.ID
		}
	}
	t.Fatalf("no %s Status with code %q", typeCode, code)
	return ""
}

// setupProjectBudgetTenant publishes foundation + projects + hr
// (entities, forms, statuses) into an already-provisioned tenant and
// seeds one Project with a "labour" budget line (planned 500.00) and a
// "materials" budget line (planned 200.00, deliberately no expense
// source — the "not available" row) — one Task, one priced TimeEntry
// (Alice, 2h @ $25.00/hr = $50.00) and one unpriced TimeEntry (Bob, 1h,
// no cost_rate on his Employee record). Mirrors
// internal/data/reporting_test.go's projectActualsFixture, but through
// the real crud.Engine (RBAC/validation applied), not RecordRepo
// directly, matching this package's own setupRFQTenant convention.
func setupProjectBudgetTenant(t *testing.T, db *sql.DB) (projectID string) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := projects.Publish(ctx, db, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("projects.PublishForms: %v", err)
	}
	if err := projects.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("projects.PublishStatuses: %v", err)
	}
	if err := hr.Publish(ctx, db, actor); err != nil {
		t.Fatalf("hr.Publish: %v", err)
	}
	if err := hr.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("hr.PublishStatuses: %v", err)
	}

	engine := crud.NewEngine(db)

	project, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-BUDGET-1", "name": map[string]any{"en": "Demo Project"},
		"start_date": "2026-01-01", "status_id": projectStatusID(t, db, "project_status", "planned"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}

	if _, err := engine.Create(ctx, projects.ProjectBudgetLine(), map[string]any{
		"project_id": project.ID, "category": "labour", "planned_amount": 500.0,
	}, actor); err != nil {
		t.Fatalf("create labour ProjectBudgetLine: %v", err)
	}
	if _, err := engine.Create(ctx, projects.ProjectBudgetLine(), map[string]any{
		"project_id": project.ID, "category": "materials", "planned_amount": 200.0,
	}, actor); err != nil {
		t.Fatalf("create materials ProjectBudgetLine: %v", err)
	}

	task, err := engine.Create(ctx, projects.Task(), map[string]any{
		"project_id": project.ID, "title": map[string]any{"en": "Work"},
		"status_id": projectStatusID(t, db, "task_status", "todo"),
	}, actor)
	if err != nil {
		t.Fatalf("create Task: %v", err)
	}

	alice, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Alice", "party_type": "person",
	}, actor)
	if err != nil {
		t.Fatalf("create Party Alice: %v", err)
	}
	bob, err := engine.Create(ctx, foundation.Party(), map[string]any{
		"name": "Bob", "party_type": "person",
	}, actor)
	if err != nil {
		t.Fatalf("create Party Bob: %v", err)
	}
	for _, partyID := range []string{alice.ID, bob.ID} {
		if _, err := engine.Create(ctx, foundation.PartyRole(), map[string]any{
			"party_id": partyID, "role_type": "employee",
		}, actor); err != nil {
			t.Fatalf("grant employee PartyRole to %s: %v", partyID, err)
		}
	}

	empStatus := projectStatusID(t, db, "employee_status", "probation")
	if _, err := engine.Create(ctx, hr.Employee(), map[string]any{
		"employee_number": "E-ALICE", "party_id": alice.ID, "hire_date": "2020-01-01",
		"status_id": empStatus, "cost_rate": int64(2500), // $25.00/hr
	}, actor); err != nil {
		t.Fatalf("create Employee Alice: %v", err)
	}
	// Bob's Employee record deliberately has no cost_rate — this is the
	// unpriced case.
	if _, err := engine.Create(ctx, hr.Employee(), map[string]any{
		"employee_number": "E-BOB", "party_id": bob.ID, "hire_date": "2021-01-01",
		"status_id": empStatus,
	}, actor); err != nil {
		t.Fatalf("create Employee Bob: %v", err)
	}

	if _, err := engine.Create(ctx, projects.TimeEntry(), map[string]any{
		"task_id": task.ID, "employee_id": alice.ID, "entry_date": "2026-02-01", "hours": 2.0,
	}, actor); err != nil {
		t.Fatalf("create TimeEntry Alice: %v", err)
	}
	if _, err := engine.Create(ctx, projects.TimeEntry(), map[string]any{
		"task_id": task.ID, "employee_id": bob.ID, "entry_date": "2026-02-01", "hours": 1.0,
	}, actor); err != nil {
		t.Fatalf("create TimeEntry Bob: %v", err)
	}

	return project.ID
}

// TestProjectBudgetReport_RendersPlannedActualAndUnpricedNote (uc-infra
// #187) is the core proof: labour's real, priced Actual (Alice's 2h @
// $25.00/hr = $50.00) renders alongside its planned amount ($500.00,
// scaled from ProjectBudgetLine's major-unit planned_amount) and its
// variance ($450.00) — while materials, which has a budget line but no
// expense source at all, renders "Not available" rather than a
// fabricated $0.00. Bob's unpriced hour surfaces as a note on the
// labour row rather than silently vanishing from the total.
func TestProjectBudgetReport_RendersPlannedActualAndUnpricedNote(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	projectID := setupProjectBudgetTenant(t, db)

	req := newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Labour", "Materials",
		"500.00", // labour planned
		"50.00",  // labour actual (2h * $25.00)
		"450.00", // labour variance (500.00 - 50.00)
		"200.00", // materials planned
		"Not available",
		"some logged hours could not be priced",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q:\n%s", want, body)
		}
	}
}

// TestProjectBudgetReport_EmptyStateWhenNoBudgetLines confirms the
// empty-state message renders (not an empty <table>) for a Project with
// no ProjectBudgetLine rows at all — ProjectBudgetActuals returns an
// empty slice for that case (its own doc comment), which must translate
// to the empty state, not a table with zero rows.
func TestProjectBudgetReport_EmptyStateWhenNoBudgetLines(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	actor := humanActor()

	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := projects.Publish(ctx, db, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishStatuses(ctx, db, actor); err != nil {
		t.Fatalf("projects.PublishStatuses: %v", err)
	}
	engine := crud.NewEngine(db)
	project, err := engine.Create(ctx, projects.Project(), map[string]any{
		"project_code": "PRJ-EMPTY", "name": map[string]any{"en": "Empty"},
		"start_date": "2026-01-01", "status_id": projectStatusID(t, db, "project_status", "planned"),
	}, actor)
	if err != nil {
		t.Fatalf("create Project: %v", err)
	}

	req := newRequest("GET", "/reports/project-budget/"+project.ID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<table") {
		t.Errorf("expected the empty-state message, not a table:\n%s", body)
	}
	if !strings.Contains(body, "nothing to compare") {
		t.Errorf("expected the empty-state message, got:\n%s", body)
	}
}

// TestProjectBudgetReport_UnknownProjectID404s mirrors
// TestRFQComparisonReport_UnknownRFQID404s.
func TestProjectBudgetReport_UnknownProjectID404s(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()
	if err := foundation.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := projects.Publish(ctx, db, humanActor()); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}

	req := newRequest("GET", "/reports/project-budget/00000000-0000-0000-0000-000000000000", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown Project id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestProjectBudgetReport_RBACDenied mirrors
// TestRFQComparisonReport_RBACDenied: once any Permission row exists for
// one of projectBudgetReportEntityTypes, an actor without that specific
// grant gets a 403 for the whole page. Table-driven over EVERY entry in
// projectBudgetReportEntityTypes (independent review finding: an
// earlier version of this test only exercised "Employee" — since
// authz.Resolver.CanRead treats a type with zero Permission rows as
// openly readable, that alone does not prove any of the other four
// entries actually does anything; deleting "Project"/"ProjectBudgetLine"/
// "Task"/"TimeEntry" from the var would have left that version green).
// Each case uses its own tenant/role pair so the four subtests can't
// interfere with each other's seeded Permission rows.
func TestProjectBudgetReport_RBACDenied(t *testing.T) {
	for _, entityType := range projectBudgetReportEntityTypes {
		t.Run(entityType, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)

			projectID := setupProjectBudgetTenant(t, db)

			seedRBAC(t, db,
				map[string][]string{"projectmgr": {"user-projectmgr"}},
				[]map[string]any{{"role": "projectmgr", "entity_type": entityType, "can_read": true}})

			req := newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "user-clerk", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for an actor without %s read, got %d: %s", entityType, rec.Code, rec.Body.String())
			}

			req = newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "user-projectmgr", nil)
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for the actor granted %s read, got %d: %s", entityType, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestProjectBudgetReport_WritesNothing pins "read-only" as an
// executable fact, same as TestRFQComparisonReport_WritesNothing: GET
// /reports/project-budget/{id} must not add a single audit_log row or
// record.
func TestProjectBudgetReport_WritesNothing(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)
	ctx := context.Background()

	projectID := setupProjectBudgetTenant(t, db)

	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	auditBefore, recordsBefore := count("audit_log"), count("records")

	req := newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := count("audit_log"); got != auditBefore {
		t.Errorf("the report route wrote %d audit_log rows — it must be read-only", got-auditBefore)
	}
	if got := count("records"); got != recordsBefore {
		t.Errorf("the report route wrote %d records — it must be read-only", got-recordsBefore)
	}
}

// TestAPI_ProjectForm_ShowsBudgetVsActualLinkForExistingRecord mirrors
// TestAPI_RFQForm_ShowsCompareQuotesLinkForExistingRecord (uc-infra#69's
// own pattern): proves the real, published Project Form Definition
// wires the navigate action up end to end, against a real record.
func TestAPI_ProjectForm_ShowsBudgetVsActualLinkForExistingRecord(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	projectID := setupProjectBudgetTenant(t, db)

	req := newRequest("GET", "/forms/Project/"+projectID, tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forms/Project/%s: expected 200, got %d: %s", projectID, rec.Code, rec.Body.String())
	}
	wantHref := "/reports/project-budget/" + projectID
	body := rec.Body.String()
	if !strings.Contains(body, `<a href="`+wantHref+`">`) {
		t.Fatalf("expected a Budget vs Actual link to %q on the Project form page, got:\n%s", wantHref, body)
	}
	if !strings.Contains(body, "Budget vs Actual") {
		t.Fatalf("expected the link's English label to appear, got:\n%s", body)
	}
}

// TestAPI_ProjectForm_OmitsBudgetVsActualLinkForNewRecord is the
// negative case: a not-yet-saved Project has no id to report budget
// actuals for.
func TestAPI_ProjectForm_OmitsBudgetVsActualLinkForNewRecord(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	ctx := context.Background()
	actor := humanActor()
	if err := foundation.Publish(ctx, db, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := projects.Publish(ctx, db, actor); err != nil {
		t.Fatalf("projects.Publish: %v", err)
	}
	if err := projects.PublishForms(ctx, db, actor); err != nil {
		t.Fatalf("projects.PublishForms: %v", err)
	}

	req := newRequest("GET", "/forms/Project/new", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forms/Project/new: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Budget vs Actual") || strings.Contains(body, `href="/reports/project-budget/`) {
		t.Fatalf("expected no Budget vs Actual link on the new/unsaved Project form, got:\n%s", body)
	}
}

// TestBuildProjectBudgetReportView_NilVsZeroAndScaling is the unit layer
// for the arithmetic/rendering rules the HTTP-level tests above can't
// cheaply isolate: a nil Actual renders as NotAvailable (never a
// fabricated 0), a real, confirmed Actual of exactly 0 renders as
// "0.00" and stays Available (distinct from nil), and planned_amount
// (major units) is scaled by money.Decimals before comparison against
// Actual (minor units) — the 100x bug uc-infra#187's own body warned
// against. Also pins UnpricedNote row-by-row (independent review
// finding): present on labour when UnpricedHours/UnpricedEntries is
// non-zero, ABSENT on labour when both are zero, and ABSENT on a
// non-labour row even if it somehow carried non-zero counters (defends
// the `a.Category == "labour"` guard specifically — ProjectBudgetActuals
// never sets these for a non-labour category itself, but this test
// doesn't rely on that upstream invariant holding).
func TestBuildProjectBudgetReportView_NilVsZeroAndScaling(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}

	project := data.Record{Data: map[string]any{"project_code": "PRJ-1"}}
	budgetLines := []data.Record{
		{Data: map[string]any{"category": "labour", "planned_amount": 10.0}},
		{Data: map[string]any{"category": "materials", "planned_amount": 5.0}},
		{Data: map[string]any{"category": "travel", "planned_amount": 1.0}},
	}
	zero := money.Money(0)
	priced := money.Money(250) // $2.50
	actuals := []data.ProjectCategoryActual{
		{Category: "labour", Actual: &priced, UnpricedHours: 1.5, UnpricedEntries: 1},
		{Category: "materials", Actual: nil, UnpricedHours: 9, UnpricedEntries: 9}, // non-labour: must NOT surface the note even with non-zero counters
		{Category: "travel", Actual: &zero},                                        // confirmed zero activity, zero unpriced: no note
	}

	view := h.buildProjectBudgetReportView(project, budgetLines, actuals, "en")
	if len(view.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(view.Rows))
	}

	labour := view.Rows[0]
	if !labour.Available || labour.Planned != "10.00" || labour.Actual != "2.50" || labour.Variance != "7.50" {
		t.Errorf("labour row = %+v, want Planned=10.00 Actual=2.50 Variance=7.50", labour)
	}
	if labour.UnpricedNote == "" {
		t.Errorf("labour row = %+v, want a non-empty UnpricedNote (UnpricedHours=1.5, UnpricedEntries=1)", labour)
	}

	materials := view.Rows[1]
	if materials.Available {
		t.Errorf("materials row = %+v, want Available=false (nil Actual, no source)", materials)
	}
	if materials.UnpricedNote != "" {
		t.Errorf("materials row = %+v, want NO UnpricedNote — the note is labour-only regardless of the (non-labour, should-never-happen) counters", materials)
	}

	travel := view.Rows[2]
	if !travel.Available || travel.Actual != "0.00" || travel.Variance != "1.00" {
		t.Errorf("travel row = %+v, want Available=true Actual=0.00 (confirmed zero, not nil) Variance=1.00", travel)
	}
	if travel.UnpricedNote != "" {
		t.Errorf("travel row = %+v, want NO UnpricedNote — not the labour category at all", travel)
	}
}

// TestBuildProjectBudgetReportView_NoRowsForNoActuals confirms an empty
// actuals slice (no ProjectBudgetLine rows on the project at all, per
// ProjectBudgetActuals' own "empty slice, not an error" contract)
// produces zero rows, not a nil-pointer panic walking budgetLines.
func TestBuildProjectBudgetReportView_NoRowsForNoActuals(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}

	view := h.buildProjectBudgetReportView(data.Record{Data: map[string]any{}}, nil, nil, "en")
	if len(view.Rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(view.Rows))
	}
}

// TestProjectBudgetPlannedMinor_SumsMultipleLinesInSameCategory pins the
// doc comment's explicit claim (independent review finding): more than
// one ProjectBudgetLine row for the same category is not prevented
// today, and their planned_amount must be SUMMED, not last-write-wins or
// first-only.
func TestProjectBudgetPlannedMinor_SumsMultipleLinesInSameCategory(t *testing.T) {
	budgetLines := []data.Record{
		{Data: map[string]any{"category": "labour", "planned_amount": 10.0}},
		{Data: map[string]any{"category": "labour", "planned_amount": 5.5}},
		{Data: map[string]any{"category": "materials", "planned_amount": 100.0}}, // different category — must not bleed in
	}
	got := projectBudgetPlannedMinor(budgetLines, "labour")
	if want := money.Money(1550); got != want { // (10.00 + 5.50) * 100 = 1550 minor
		t.Errorf("projectBudgetPlannedMinor(labour) = %d, want %d (10.00 + 5.50 summed)", got, want)
	}
}

// TestProjectBudgetPlannedMinor_MalformedOrOverflowingRowsSkippedNotAborted
// pins the independent review's finding that an earlier version of this
// function aborted the WHOLE report (via a returned error) when one
// line's planned_amount overflowed what money.Decimals minor units can
// represent — contradicting its own "one bad row doesn't abort" doc
// comment. A malformed (non-numeric type) row and an out-of-range row
// must each be skipped, and a well-formed row alongside either must
// still be summed correctly — this function has no error return at all
// now, so there is nothing to abort with.
func TestProjectBudgetPlannedMinor_MalformedOrOverflowingRowsSkippedNotAborted(t *testing.T) {
	budgetLines := []data.Record{
		{Data: map[string]any{"category": "labour", "planned_amount": "not-a-number"}}, // wrong type — skipped
		{Data: map[string]any{"category": "labour"}},                                   // missing key entirely — skipped
		{Data: map[string]any{"category": "labour", "planned_amount": 1e300}},          // well-formed float, but money.FromMajorUnits can't represent it — skipped, not fatal
		{Data: map[string]any{"category": "labour", "planned_amount": 20.0}},           // the one good row
	}
	got := projectBudgetPlannedMinor(budgetLines, "labour")
	if want := money.Money(2000); got != want { // only the 20.00 row counts
		t.Errorf("projectBudgetPlannedMinor(labour) = %d, want %d (bad rows skipped, only the well-formed 20.00 counted)", got, want)
	}
}
