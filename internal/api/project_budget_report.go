package api

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/money"
)

// projectBudgetReportEntityTypes is every entity type
// renderProjectBudgetReport reads from — Project itself (the header),
// ProjectBudgetLine (planned amounts), and every entity
// data.ReportingRepo.ProjectBudgetActuals' SQL joins to compute the
// labour actual: Task (a TimeEntry's own project dimension is only
// reachable via its Task), TimeEntry, and Employee (cost_rate). Missing
// a join target here is a real leak, same "the query reads it, the gate
// must cover it" reasoning purchasingReportEntityTypes' own doc comment
// gives — gated as one unit via requireReportRead, ahead of running any
// query. Employee is included even though this report never renders a
// per-employee row or cost_rate value directly (ProjectBudgetActuals
// only ever hands back an aggregated sum) — the same whole-page,
// entity-level gate every other report in this file uses, not a new,
// narrower mechanism invented just for this card (uc-infra#187's own
// body raised the question; ADR-0006's FieldPermission is the tool for
// a narrower per-field gate). Note this whole-page gate is not a
// guarantee cost_rate can never be inferred: for a project with exactly
// one priced employee, Actual / their hours reproduces their rate. That
// is a defensible limit of an entity-level gate, not something this
// report claims to close.
var projectBudgetReportEntityTypes = []string{"Project", "ProjectBudgetLine", "Task", "TimeEntry", "Employee"}

// renderProjectBudgetReport (uc-infra#187) is the read-only planned-vs-
// actual view over one Project's budget lines — the UI surface
// data.ReportingRepo.ProjectBudgetActuals (uc-infra#134) shipped without
// one. One row per category present on the project's ProjectBudgetLine
// rows: planned_amount (scaled from major units, uc-infra#79 predates
// FieldMoney) alongside the computed actual.
//
// Two things this deliberately never collapses into a plainer number,
// both load-bearing per ProjectCategoryActual's own doc comment:
//   - Actual == nil ("not available" — no source exists for this
//     category yet, e.g. every non-labour category today) renders
//     distinctly from a real, confirmed Actual == 0.
//   - A non-zero UnpricedHours/UnpricedEntries on the labour row (hours
//     logged by a party with no priced Employee record) renders as an
//     explicit note next to that row, not silently folded into a
//     smaller-looking total.
//
// Informational only, same as renderRFQComparisonReport (#9): nothing on
// this page writes anything.
func (h *Handler) renderProjectBudgetReport(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)
	ctx := r.Context()

	if !h.requireReportRead(w, r, &rc, ts, locale, projectBudgetReportEntityTypes) {
		return
	}

	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	projectDef, err := ts.entityDef(ctx, "Project")
	if err != nil {
		writeDefinitionLookupError(w, "Project", err)
		return
	}
	project, err := ts.crud.Get(ctx, projectDef, id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("Project %q not found", id))
		return
	}
	if err != nil {
		h.writeCrudPageError(w, r, &rc, locale, "get Project "+id, err)
		return
	}

	budgetLineDef, err := ts.entityDef(ctx, "ProjectBudgetLine")
	if err != nil {
		writeDefinitionLookupError(w, "ProjectBudgetLine", err)
		return
	}
	budgetLines, err := ts.crud.ListByField(ctx, budgetLineDef, "project_id", id)
	if err != nil {
		writeInternalError(w, "list project budget lines", err)
		return
	}

	// ProjectBudgetLine.planned_amount can be hidden per-role via a
	// FieldPermission (ADR-0006) — same mechanism export.go/import.go
	// already check. ts.crud.ListByField above has already stripped the
	// key from every budgetLines row's Data for an actor this applies
	// to, which is exactly why projectBudgetPlannedMinor can't tell
	// "redacted" from "genuinely zero" on its own (uc-infra#216): it
	// only ever sees the already-redacted Data map. Resolved once here,
	// for the whole report, not per-row — HiddenFields answers for one
	// (actor, entity type) pair, and every ProjectBudgetLine row on this
	// page belongs to the same entity type.
	hiddenBudgetFields, err := ts.crud.HiddenFields(ctx, "ProjectBudgetLine")
	if err != nil {
		writeInternalError(w, "resolve ProjectBudgetLine field visibility", err)
		return
	}
	plannedRedacted := projectBudgetLinePlannedRedacted(hiddenBudgetFields)

	actuals, err := ts.reporting.ProjectBudgetActuals(ctx, id)
	if err != nil {
		writeInternalError(w, "project budget actuals", err)
		return
	}

	view := h.buildProjectBudgetReportView(project, budgetLines, actuals, locale, plannedRedacted)
	topicID := help.RouteTopicID("reports/project-budget")
	view.Help = h.buildHelpView(locale, topicID, help.HasContent(locale, topicID))

	var buf bytes.Buffer
	if err := projectBudgetReportTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render project budget report", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render project budget report shell", err)
	}
}

// projectBudgetPlannedMinor sums planned_amount (major units,
// entity.FieldNumber — uc-infra#79 predates FieldMoney) across every
// budgetLines row for one category, scaling each row to money.Money
// (minor units) BEFORE summing — money.Sum's own convention (exact
// int64 addition), not a float64 sum converted once at the end, which
// can land a minor unit away from this on some inputs. More than one
// ProjectBudgetLine row per category is not prevented today; summing
// them matches ProjectBudgetActuals' own "share one combined actual"
// doc comment, so planned and actual stay comparable the same way for
// that case.
//
// A row whose planned_amount isn't a well-formed number, OR whose value
// money.FromMajorUnits can't represent (e.g. a value with no Max bound,
// uc-infra#80, that overflows what money.Decimals minor units can hold),
// is skipped rather than aborting the whole report — the same "one bad
// row doesn't abort the aggregate" resolution ProjectBudgetActuals' own
// doc comment applies at the SQL layer, kept consistent here at the Go
// layer (an earlier version of this function aborted the whole report
// on the second case, undermining its own doc comment's claim).
// projectBudgetLinePlannedRedacted (uc-infra#216, independent review
// finding) decides whether this actor's view of a report row's Planned
// figure is trustworthy, given the set of ProjectBudgetLine fields a
// FieldPermission hides from them. It is NOT just "is planned_amount
// hidden": projectBudgetPlannedMinor also reads "category" out of the
// same already-redacted budgetLines Data map to decide which rows to
// sum for a given actuals category — a FieldPermission hiding category
// alone makes every row's stored category compare empty against the
// real category ProjectBudgetActuals' un-redacted SQL query still
// reports, so NO row is ever matched and money.Sum returns a fabricated
// 0, indistinguishable from a genuine zero-budget category, exactly the
// failure this whole mechanism exists to prevent. An earlier version of
// this function gated on planned_amount alone and let a category-only
// redaction sail straight through as a confidently-wrong Planned=0.00
// (and, worse, a confidently-wrong non-zero Variance) — reproduced by
// independent review before this fix.
func projectBudgetLinePlannedRedacted(hidden map[string]bool) bool {
	return hidden["planned_amount"] || hidden["category"]
}

func projectBudgetPlannedMinor(budgetLines []data.Record, category string) money.Money {
	var amounts []money.Money
	for _, line := range budgetLines {
		cat, _ := line.Data["category"].(string)
		if cat != category {
			continue
		}
		var major float64
		switch v := line.Data["planned_amount"].(type) {
		case float64:
			major = v
		case int64:
			major = float64(v)
		default:
			continue
		}
		m, err := money.FromMajorUnits(major, money.Decimals)
		if err != nil {
			continue
		}
		amounts = append(amounts, m)
	}
	return money.Sum(amounts...)
}

// buildProjectBudgetReportView shapes the loaded Project + budget lines
// + computed actuals into the template's view model. actuals (not
// budgetLines) drives the set of rows rendered — it is the authoritative
// category list (data.ReportingRepo.ProjectBudgetActuals derives it from
// the same ProjectBudgetLine rows this handler already fetched), so a
// row here always has a real planned amount to pair with, never a
// synthesized zero for a category nobody budgeted.
//
// plannedRedacted (uc-infra#216) is true when the acting role's
// FieldPermission hides ProjectBudgetLine.planned_amount — a single
// tenant-and-actor-wide answer, not a per-row one (see the handler's own
// comment on why it's resolved once). When true, every row's Planned
// column renders NotAvailable instead of projectBudgetPlannedMinor's sum
// — which, over an already-redacted budgetLines slice, would otherwise
// be indistinguishable from a real, confirmed planned amount of zero
// (the exact fabricated-number failure Actual/Available already guards
// against). Variance also becomes unavailable in that case: it's a
// function of Planned, so it can't be a real number when Planned isn't.
func (h *Handler) buildProjectBudgetReportView(project data.Record, budgetLines []data.Record, actuals []data.ProjectCategoryActual, locale string, plannedRedacted bool) projectBudgetReportView {
	projectCode, _ := project.Data["project_code"].(string)

	view := projectBudgetReportView{
		Title:            h.catalog.T(locale, "report.project_budget.title"),
		ProjectCodeLabel: h.catalog.TOrDefault(locale, "field.Project.project_code", "Project Code"),
		ProjectCode:      projectCode,
		CategoryCol:      h.catalog.TOrDefault(locale, "field.ProjectBudgetLine.category", "Category"),
		PlannedCol:       h.catalog.TOrDefault(locale, "field.ProjectBudgetLine.planned_amount", "Planned"),
		ActualCol:        h.catalog.T(locale, "report.project_budget.actual_col"),
		VarianceCol:      h.catalog.T(locale, "report.project_budget.variance_col"),
		Empty:            h.catalog.T(locale, "report.project_budget.empty"),
		NotAvailable:     h.catalog.T(locale, "report.project_budget.not_available"),
	}

	for _, a := range actuals {
		plannedMinor := projectBudgetPlannedMinor(budgetLines, a.Category)
		row := projectBudgetCategoryRowView{
			Category: h.catalog.TOrDefault(locale, "field.ProjectBudgetLine.category."+a.Category, a.Category),
		}
		if !plannedRedacted {
			row.PlannedAvailable = true
			row.Planned = plannedMinor.String()
		}
		if a.Actual != nil {
			row.Available = true
			row.Actual = a.Actual.String()
			if row.PlannedAvailable {
				row.Variance = (plannedMinor - *a.Actual).String()
			}
		}
		if a.Category == "labour" && (a.UnpricedHours > 0 || a.UnpricedEntries > 0) {
			row.UnpricedNote = h.catalog.T(locale, "report.project_budget.unpriced_note")
		}
		view.Rows = append(view.Rows, row)
	}

	return view
}

type projectBudgetReportView struct {
	Title string

	ProjectCodeLabel string
	ProjectCode      string

	CategoryCol  string
	PlannedCol   string
	ActualCol    string
	VarianceCol  string
	NotAvailable string

	Rows []projectBudgetCategoryRowView

	Empty string

	Help helpView
}

type projectBudgetCategoryRowView struct {
	Category string
	Planned  string
	// PlannedAvailable distinguishes a real planned_amount sum from
	// "not available" (the acting role's FieldPermission hides
	// planned_amount, uc-infra#216) — collapsing the two would render a
	// fabricated Planned=0.00 for an actor who genuinely can't see
	// planned_amount at all, the same under-reporting risk Available
	// (below) already guards against for Actual.
	PlannedAvailable bool
	// Available distinguishes a real, computed Actual/Variance (even a
	// genuine 0) from "not available" (no source exists for this
	// category at all) — collapsing the two would reproduce the exact
	// under-reporting risk ProjectCategoryActual's own doc comment
	// warns about.
	Available bool
	Actual    string
	// Variance is only set when BOTH Available and PlannedAvailable —
	// it's Planned minus Actual, so it can't be a real number when
	// either half is unavailable.
	Variance string
	// UnpricedNote is set only on the "labour" row when
	// UnpricedHours/UnpricedEntries is non-zero — a labour actual that
	// silently omitted unpriced hours would read as a smaller, complete
	// number with no indication it isn't.
	UnpricedNote string
}

var projectBudgetReportTmpl = template.Must(template.New("projectBudgetReport").Parse(`
<h1>{{.Title}} <a class="uc-help-affordance" role="link" data-help-topic="{{.Help.TopicID}}"{{if .Help.Href}} href="{{.Help.Href}}"{{end}}{{if .Help.Disabled}} aria-disabled="true"{{end}} tabindex="0" aria-label="{{.Help.Label}}">?</a></h1>
<p><strong>{{.ProjectCodeLabel}}:</strong> {{.ProjectCode}}</p>

{{if .Rows}}
<table class="uc-table">
<thead>
<tr>
  <th>{{.CategoryCol}}</th>
  <th>{{.PlannedCol}}</th>
  <th>{{.ActualCol}}</th>
  <th>{{.VarianceCol}}</th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr>
  <td>{{.Category}}{{if .UnpricedNote}} <span class="uc-project-budget-unpriced">({{.UnpricedNote}})</span>{{end}}</td>
  <td>{{if .PlannedAvailable}}{{.Planned}}{{else}}{{$.NotAvailable}}{{end}}</td>
  <td>{{if .Available}}{{.Actual}}{{else}}{{$.NotAvailable}}{{end}}</td>
  <td>{{if and .Available .PlannedAvailable}}{{.Variance}}{{else}}{{$.NotAvailable}}{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.Empty}}</p>
{{end}}
`))
