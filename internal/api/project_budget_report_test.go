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

// TestProjectBudgetReport_PlannedRedactedRendersNotAvailable (uc-infra
// #216, extended by uc-infra#222) is the HTTP-level proof that a
// FieldPermission hiding a ProjectBudgetLine field this report reads
// makes the affected column(s) render "Not available" instead of a
// fabricated or leaked real value — while Actual, a genuinely separate
// source, keeps rendering normally. Table-driven over BOTH fields
// projectBudgetPlannedMinor reads out of budgetLines' Data (independent
// review finding, uc-infra#216): not just "planned_amount" (the field
// that holds the number) but "category" too (the field the summing loop
// matches rows by) — hiding category alone makes every budgetLines row's
// `cat` compare empty against the actuals' real category, so no row is
// ever summed, and an earlier version of this fix only gated on
// "planned_amount" being hidden, letting a category-only redaction sail
// straight through as a fabricated 0.00 Planned and, worse, a
// confidently wrong non-zero Variance (Planned 0 minus a real Actual).
//
// uc-infra#222 (independent review of #216 itself) found a second,
// separate leak the #216 fix never touched: even with Planned/Variance
// correctly blanked, the Category column's own text came from
// data.ReportingRepo.ProjectBudgetActuals' raw SQL — read straight out
// of `records`, entirely outside the FieldPermission-redacted
// budgetLines slice — so an actor with "category" hidden still saw the
// real category names ("Labour"/"Materials") as every row's own label.
// wantCategoryTextShown/wantNotAvailableCount below distinguish the two
// hiddenField cases precisely: redacting "planned_amount" alone must
// NOT touch Category (still real text, still 5 "Not available" cells,
// matching #216's original count); redacting "category" must ALSO blank
// Category on every row (2 more "Not available" cells, one per row, on
// top of #216's existing 5 — 7 total), and the real category names must
// not appear anywhere in the body. Uses seedFieldPermission
// (handlers_test.go) rather than seedRBAC, matching every other
// FieldPermission-specific HTTP test in this package (e.g.
// TestAPI_Export_RedactsHiddenFieldColumn's own convention).
func TestProjectBudgetReport_PlannedRedactedRendersNotAvailable(t *testing.T) {
	cases := []struct {
		hiddenField           string
		wantCategoryTextShown bool
		wantNotAvailableCount int
	}{
		{"planned_amount", true, 5},
		{"category", false, 7},
	}
	for _, tc := range cases {
		t.Run(tc.hiddenField, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)

			projectID := setupProjectBudgetTenant(t, db)
			seedFieldPermission(t, db, "budget_redacted", "user-redacted", "ProjectBudgetLine", tc.hiddenField)

			req := newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "user-redacted", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()

			if strings.Contains(body, "500.00") || strings.Contains(body, "200.00") {
				t.Errorf("report body still shows a planned amount for an actor whose role has %s redacted:\n%s", tc.hiddenField, body)
			}
			// A category-only redaction must not silently turn into a
			// wrong, confidently-rendered NEGATIVE variance (Planned
			// fabricated as 0 minus a real 50.00 Actual) — the exact
			// failure mode independent review reproduced.
			if strings.Contains(body, "-50.00") {
				t.Errorf("report body shows a fabricated negative variance for an actor with %s redacted:\n%s", tc.hiddenField, body)
			}
			// Labour's real Actual ($50.00, from TimeEntry — a source
			// entirely separate from ProjectBudgetLine) must still
			// render: redaction of one ProjectBudgetLine field must not
			// black out an unrelated column.
			if !strings.Contains(body, "50.00") {
				t.Errorf("report body missing labour's Actual (50.00) — redacting %s must not also hide Actual:\n%s", tc.hiddenField, body)
			}
			// uc-infra#222: the real category names must appear when
			// only planned_amount is redacted (category itself is
			// untouched), and must NOT appear anywhere in the body when
			// category is the redacted field — the leak this card fixes.
			categoryTextShown := strings.Contains(body, "Labour") && strings.Contains(body, "Materials")
			if categoryTextShown != tc.wantCategoryTextShown {
				t.Errorf("report body category-name visibility = %v, want %v, for an actor with %s redacted:\n%s", categoryTextShown, tc.wantCategoryTextShown, tc.hiddenField, body)
			}
			// Labour's Variance cell specifically must read "Not
			// available", not merely be absent from a loose substring
			// count — independent review found a template regression
			// (the Variance guard silently dropped its PlannedAvailable
			// half) that a "Not available" occurs somewhere in the body
			// count style of assertion did not catch: with that
			// regression, labour's Variance cell renders empty
			// ("<td></td>") instead of "Not available", since
			// buildProjectBudgetReportView never sets Variance when
			// Planned is redacted. Pinned here as the literal three-cell
			// sequence the template emits for the labour row's own
			// trailing Planned/Actual/Variance cells: "Not available" /
			// "50.00" (real) / "Not available" — proving the Variance
			// guard is doing its own, distinct job, not riding along on
			// Planned's. Unaffected by whether Category (the cell
			// immediately BEFORE this sequence) is itself redacted.
			const labourPlannedActualVarianceCells = "  <td>Not available</td>\n  <td>50.00</td>\n  <td>Not available</td>"
			if !strings.Contains(body, labourPlannedActualVarianceCells) {
				t.Errorf("expected the labour row's Planned/Actual/Variance cells to read Not available / 50.00 / Not available in sequence, got:\n%s", body)
			}
			if !tc.wantCategoryTextShown {
				// category redacted: the labour row's full 4-cell
				// sequence (Category/Planned/Actual/Variance) must read
				// Not available / Not available / 50.00 / Not available
				// — pinning Category's own cell right alongside Planned's,
				// not just trusting the "Labour" substring's absence. The
				// Category cell also still carries the unpriced-note span
				// right after "Not available" (uc-infra#222 independent
				// review: the note must survive category's redaction,
				// it's a fact about hours, not the category name) — pinned
				// here too, not just by the separate "note text present
				// somewhere in body" check below.
				const labourAllFourCells = `  <td>Not available <span class="uc-project-budget-unpriced">(some logged hours could not be priced)</span></td>` + "\n" +
					`  <td>Not available</td>` + "\n" +
					`  <td>50.00</td>` + "\n" +
					`  <td>Not available</td>`
				if !strings.Contains(body, labourAllFourCells) {
					t.Errorf("expected the labour row's Category/Planned/Actual/Variance cells to read (Not available + unpriced note) / Not available / 50.00 / Not available in sequence, got:\n%s", body)
				}
			}
			// Exact count, not a loose lower bound (independent review
			// finding: a "< 3" bound stayed green through the Variance-
			// guard regression above, since it only dropped the count by
			// one).
			if got := strings.Count(body, "Not available"); got != tc.wantNotAvailableCount {
				t.Errorf(`expected exactly %d "Not available" cells for %s redacted, got %d:\n%s`, tc.wantNotAvailableCount, tc.hiddenField, got, body)
			}

			// An actor with no such redaction still sees the real
			// planned amounts and real category names — the fix must be
			// actor-specific, not a global change.
			req = newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "farshid", nil)
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			unredactedBody := rec.Body.String()
			if !strings.Contains(unredactedBody, "500.00") {
				t.Errorf("an actor without the redaction should still see the real planned amount:\n%s", unredactedBody)
			}
			if !strings.Contains(unredactedBody, "Labour") || !strings.Contains(unredactedBody, "Materials") {
				t.Errorf("an actor without the redaction should still see the real category names:\n%s", unredactedBody)
			}
		})
	}
}

// TestProjectBudgetReport_CategoryRedactedRendersNotAvailable (uc-infra
// #222, independent review of this same fix) is the HTTP-level, real-
// template proof that the unpriced-hours note keeps rendering for an
// actor with ProjectBudgetLine.category redacted — the exact regression
// an earlier draft of this fix shipped (nesting the note's template
// guard inside CategoryAvailable's, so the note silently vanished
// alongside the real category name it has nothing to do with). Separate
// from TestBuildProjectBudgetReportView_CategoryRedactedRendersNotAvailable
// (Go view-model level, doesn't exercise projectBudgetReportTmpl at all)
// and from TestProjectBudgetReport_PlannedRedactedRendersNotAvailable's
// own "category" subtest (asserts the leak is closed, never asserted the
// note either way).
func TestProjectBudgetReport_CategoryRedactedRendersNotAvailable(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	// setupProjectBudgetTenant seeds Bob's unpriced TimeEntry alongside
	// Alice's priced one, which is exactly what makes the labour row's
	// UnpricedNote non-empty.
	projectID := setupProjectBudgetTenant(t, db)
	seedFieldPermission(t, db, "budget_category_redacted", "user-redacted", "ProjectBudgetLine", "category")

	req := newRequest("GET", "/reports/project-budget/"+projectID, tenantID, "user-redacted", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, "Labour") || strings.Contains(body, "Materials") {
		t.Errorf("report body still shows real category names for an actor with category redacted:\n%s", body)
	}
	if !strings.Contains(body, "some logged hours could not be priced") {
		t.Errorf("report body missing the unpriced-hours note for an actor with category redacted — the note is unrelated to the category name and must not be suppressed by its redaction:\n%s", body)
	}
	if !strings.Contains(body, `<span class="uc-project-budget-unpriced">`) {
		t.Errorf("report body missing the unpriced-note span markup for an actor with category redacted:\n%s", body)
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

	view := h.buildProjectBudgetReportView(project, budgetLines, actuals, "en", false, false)
	if len(view.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(view.Rows))
	}

	labour := view.Rows[0]
	if !labour.CategoryAvailable || labour.Category != "Labour" {
		t.Errorf("labour row = %+v, want CategoryAvailable=true Category=Labour", labour)
	}
	if !labour.PlannedAvailable || !labour.Available || labour.Planned != "10.00" || labour.Actual != "2.50" || labour.Variance != "7.50" {
		t.Errorf("labour row = %+v, want PlannedAvailable=true Planned=10.00 Actual=2.50 Variance=7.50", labour)
	}
	if labour.UnpricedNote == "" {
		t.Errorf("labour row = %+v, want a non-empty UnpricedNote (UnpricedHours=1.5, UnpricedEntries=1)", labour)
	}

	materials := view.Rows[1]
	if materials.Available {
		t.Errorf("materials row = %+v, want Available=false (nil Actual, no source)", materials)
	}
	if !materials.PlannedAvailable || materials.Planned != "5.00" {
		t.Errorf("materials row = %+v, want PlannedAvailable=true Planned=5.00 (planned_amount is not what's redacted here)", materials)
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

// TestBuildProjectBudgetReportView_PlannedRedactedRendersNotAvailable
// (uc-infra#216) pins the fix: when plannedRedacted is true (the acting
// role's FieldPermission hides ProjectBudgetLine.planned_amount),
// EVERY row's Planned must render PlannedAvailable=false — never a
// fabricated Planned=0.00 that reads identically to a genuine zero
// budget — and Variance must go unavailable right alongside it even on
// a row whose Actual is itself genuinely available, since Variance is a
// function of Planned and can't be a real number when Planned isn't.
func TestBuildProjectBudgetReportView_PlannedRedactedRendersNotAvailable(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}

	project := data.Record{Data: map[string]any{"project_code": "PRJ-1"}}
	budgetLines := []data.Record{
		{Data: map[string]any{"category": "labour", "planned_amount": 10.0}},
	}
	priced := money.Money(250) // $2.50
	actuals := []data.ProjectCategoryActual{
		{Category: "labour", Actual: &priced},
	}

	view := h.buildProjectBudgetReportView(project, budgetLines, actuals, "en", true, false)
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}

	labour := view.Rows[0]
	if labour.PlannedAvailable {
		t.Errorf("labour row = %+v, want PlannedAvailable=false — planned_amount is redacted for this actor", labour)
	}
	if labour.Planned != "" {
		t.Errorf("labour row = %+v, want Planned=\"\" (no fabricated amount) when redacted", labour)
	}
	if !labour.Available || labour.Actual != "2.50" {
		t.Errorf("labour row = %+v, want Available=true Actual=2.50 — Actual is a separate source, unaffected by planned_amount's redaction", labour)
	}
	if labour.Variance != "" {
		t.Errorf("labour row = %+v, want Variance=\"\" — it depends on the redacted Planned, so it can't be a real number either", labour)
	}
	// categoryRedacted=false here — redacting planned_amount alone must
	// not also blank the Category column (uc-infra#222 keeps these two
	// independent at the view-model level, even though the real handler
	// always sets both true together when "category" itself is hidden).
	if !labour.CategoryAvailable || labour.Category != "Labour" {
		t.Errorf("labour row = %+v, want CategoryAvailable=true Category=Labour — only planned_amount is redacted here, not category", labour)
	}
}

// TestBuildProjectBudgetReportView_CategoryRedactedRendersNotAvailable
// (uc-infra#222) pins the fix for the leak independent review found in
// uc-infra#216's own redaction fix: ProjectBudgetActuals' raw-SQL
// category values are read straight from a.Category — a field the
// SQL query in internal/data/reporting.go reads directly out of
// `records`, entirely outside the already-redacted budgetLines slice
// ts.crud.ListByField produces. Hiding ProjectBudgetLine.category via a
// FieldPermission must therefore also stop this view from putting the
// real category name in the Category column, not just blank
// Planned/Variance (which projectBudgetLinePlannedRedacted already did
// before this fix, since it ORs in hidden["category"] for exactly this
// reason — but that only ever protected Planned/Variance, never the
// Category label itself). categoryRedacted=true here arrives alongside
// plannedRedacted=true, matching what renderProjectBudgetReport actually
// passes (projectBudgetLinePlannedRedacted treats a hidden "category" as
// redacting Planned too).
//
// Also pins a second, independent-review-only finding (uc-infra#222,
// review of this same fix): UnpricedNote must still be set even when
// categoryRedacted is true. UnpricedHours/UnpricedEntries are non-zero
// here specifically so a regression that (re-)nests the note's template
// guard inside CategoryAvailable — the actual bug an earlier draft of
// this fix shipped, silently dropping the note for a category-redacted
// actor — would leave UnpricedNote computed-but-unrendered, invisible to
// a test that only checks the Go view model's Category/CategoryAvailable
// fields. See TestProjectBudgetReport_CategoryRedactedRendersNotAvailable
// (HTTP-level) for the template-rendering half of this same pin.
func TestBuildProjectBudgetReportView_CategoryRedactedRendersNotAvailable(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	h := &Handler{catalog: catalog}

	project := data.Record{Data: map[string]any{"project_code": "PRJ-1"}}
	budgetLines := []data.Record{
		{Data: map[string]any{"category": "labour", "planned_amount": 10.0}},
	}
	priced := money.Money(250) // $2.50
	actuals := []data.ProjectCategoryActual{
		{Category: "labour", Actual: &priced, UnpricedHours: 1.5, UnpricedEntries: 1},
	}

	view := h.buildProjectBudgetReportView(project, budgetLines, actuals, "en", true, true)
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}

	labour := view.Rows[0]
	if labour.CategoryAvailable {
		t.Errorf("labour row = %+v, want CategoryAvailable=false — category is redacted for this actor", labour)
	}
	if labour.Category != "" {
		t.Errorf("labour row = %+v, want Category=\"\" (real category name never assigned) when redacted, got %q", labour, labour.Category)
	}
	if !labour.Available || labour.Actual != "2.50" {
		t.Errorf("labour row = %+v, want Available=true Actual=2.50 — Actual is a separate source, unaffected by category's redaction", labour)
	}
	if labour.UnpricedNote == "" {
		t.Errorf("labour row = %+v, want a non-empty UnpricedNote — it's a boolean-shaped fact (some hours were unpriced), not the category name, and must not be suppressed by category's redaction", labour)
	}
}

// TestProjectBudgetLinePlannedRedacted_CoversBothFieldsProjectBudgetPlannedMinorReads
// (uc-infra#216, independent review finding) pins projectBudgetLinePlannedRedacted's
// full truth table directly, at the smallest possible unit: neither
// field hidden must not redact; EITHER "planned_amount" alone OR
// "category" alone must redact (category is the field
// projectBudgetPlannedMinor matches rows by — hiding it alone empties
// the match just as effectively as hiding the amount itself); both
// hidden together must still redact; and an unrelated hidden field
// (e.g. "notes") must NOT redact — the gate is specific to the two
// fields the summing function actually reads, not "any FieldPermission
// row exists at all".
func TestProjectBudgetLinePlannedRedacted_CoversBothFieldsProjectBudgetPlannedMinorReads(t *testing.T) {
	cases := []struct {
		name   string
		hidden map[string]bool
		want   bool
	}{
		{"nothing hidden", map[string]bool{}, false},
		{"nil map", nil, false},
		{"planned_amount hidden", map[string]bool{"planned_amount": true}, true},
		{"category hidden", map[string]bool{"category": true}, true},
		{"both hidden", map[string]bool{"planned_amount": true, "category": true}, true},
		{"unrelated field hidden", map[string]bool{"notes": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectBudgetLinePlannedRedacted(tc.hidden); got != tc.want {
				t.Errorf("projectBudgetLinePlannedRedacted(%v) = %v, want %v", tc.hidden, got, tc.want)
			}
		})
	}
}

// TestProjectBudgetLineCategoryRedacted_OnlyCategoryGates (uc-infra#222)
// pins projectBudgetLineCategoryRedacted's full truth table directly, at
// the smallest possible unit — mirroring
// TestProjectBudgetLinePlannedRedacted_CoversBothFieldsProjectBudgetPlannedMinorReads
// just above. The one case that matters most here: "planned_amount
// hidden" alone must NOT redact Category — this predicate is
// deliberately narrower than projectBudgetLinePlannedRedacted, which
// treats a hidden planned_amount as reason enough to blank Planned/
// Variance but must never blank the Category column on its own.
func TestProjectBudgetLineCategoryRedacted_OnlyCategoryGates(t *testing.T) {
	cases := []struct {
		name   string
		hidden map[string]bool
		want   bool
	}{
		{"nothing hidden", map[string]bool{}, false},
		{"nil map", nil, false},
		{"planned_amount hidden", map[string]bool{"planned_amount": true}, false},
		{"category hidden", map[string]bool{"category": true}, true},
		{"both hidden", map[string]bool{"planned_amount": true, "category": true}, true},
		{"unrelated field hidden", map[string]bool{"notes": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectBudgetLineCategoryRedacted(tc.hidden); got != tc.want {
				t.Errorf("projectBudgetLineCategoryRedacted(%v) = %v, want %v", tc.hidden, got, tc.want)
			}
		})
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

	view := h.buildProjectBudgetReportView(data.Record{Data: map[string]any{}}, nil, nil, "en", false, false)
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
