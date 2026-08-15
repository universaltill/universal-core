package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	"github.com/universaltill/universal-core/internal/kernel/money"
)

// rfqReportEntityTypes is every entity type renderRFQComparisonReport
// reads from — the RequestForQuotation header itself, its two
// composition children (RequestForQuotationLine/Vendor), the independent
// RequestForQuotationQuoteLine, and every join target their human labels
// resolve through (Item for a line's item name, Party for a vendor's
// name, Status for the header's own status label). Same "missing a join
// target here is a real leak" reasoning purchasingReportEntityTypes' own
// doc comment gives — gated as one unit via requireReportRead, ahead of
// running any query.
var rfqReportEntityTypes = []string{
	"RequestForQuotation", "RequestForQuotationLine", "RequestForQuotationVendor",
	"RequestForQuotationQuoteLine", "Item", "Party", "Status",
}

// renderRFQComparisonReport (#9) is the read-only vendor-comparison view
// over one RequestForQuotation: one row per requested line (item + qty),
// one column per invited vendor, the quoted unit price in each cell
// where that vendor actually responded to that line — a genuinely
// missing quote renders as a blank ("—"), never a fabricated zero (see
// data.ReportingRepo.RFQComparison's own doc comment) — with the lowest
// PRESENT price in each row visually marked (.uc-rfq-lowest, app.css),
// and a footer row summing each vendor's own quoted total across only
// the lines that vendor actually quoted.
//
// Explicitly informational only, per #9's own non-goals: no "select
// winning quote" action and no RFQ->PurchaseOrder conversion live here —
// a buyer reads this page to decide, nothing on it writes anything.
func (h *Handler) renderRFQComparisonReport(w http.ResponseWriter, r *http.Request) {
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

	if !h.requireReportRead(w, r, &rc, ts, locale, rfqReportEntityTypes) {
		return
	}

	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	rfqDef, err := ts.entityDef(ctx, "RequestForQuotation")
	if err != nil {
		writeDefinitionLookupError(w, "RequestForQuotation", err)
		return
	}
	rfq, err := ts.crud.Get(ctx, rfqDef, id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("RequestForQuotation %q not found", id))
		return
	}
	if err != nil {
		h.writeCrudPageError(w, r, &rc, locale, "get RequestForQuotation "+id, err)
		return
	}

	lines, vendors, err := ts.reporting.RFQComparison(ctx, id)
	if err != nil {
		writeInternalError(w, "rfq comparison", err)
		return
	}

	view := h.buildRFQReportView(ctx, ts, rfq, lines, vendors, locale)
	// ADR-0023's "?" help affordance (uc-infra#152) — same pattern
	// project_budget_report.go established.
	rfqReportTopicID := help.RouteTopicID("reports/rfq")
	view.Help = h.buildHelpView(locale, rfqReportTopicID, help.HasContent(locale, rfqReportTopicID))

	var buf bytes.Buffer
	if err := rfqReportTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render rfq comparison report", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render rfq comparison report shell", err)
	}
}

// buildRFQReportView shapes the loaded RFQ header + comparison grid into
// the template's view model. The lowest-price mark and the per-vendor
// footer total are both computed here, over PRESENT quotes only — a
// vendor who quoted nothing on a row contributes neither a candidate for
// "lowest" nor anything to their own footer total (data.
// RFQComparisonLine's own doc comment: a missing quote is a real fact,
// never a fabricated zero). The header's own status label resolves the
// same way renderPurchasingReport's status cards do — the human code off
// the referenced Status record, not a raw id — but degrades to the raw
// code (or blank, if unresolvable) rather than failing the whole page: a
// dangling/malformed status_id on the header is not a reason to hide the
// comparison grid itself.
func (h *Handler) buildRFQReportView(ctx context.Context, ts tenantScope, rfq data.Record, lines []data.RFQComparisonLine, vendors []data.RFQComparisonVendor, locale string) rfqReportView {
	rfqNumber, _ := rfq.Data["rfq_number"].(string)
	dueDate, _ := rfq.Data["due_date"].(string)

	statusLabel := ""
	if statusID, _ := rfq.Data["status_id"].(string); statusID != "" {
		if statusDef, err := ts.entityDef(ctx, "Status"); err == nil {
			if statusRec, err := ts.crud.Get(ctx, statusDef, statusID); err == nil {
				if code, _ := statusRec.Data["code"].(string); code != "" {
					statusLabel = h.catalog.TOrDefault(locale, "field.RequestForQuotation.status."+code, code)
				}
			}
		}
	}

	view := rfqReportView{
		Title:          h.catalog.T(locale, "report.rfq.title"),
		RFQNumberLabel: h.catalog.TOrDefault(locale, "field.RequestForQuotation.rfq_number", "RFQ Number"),
		RFQNumber:      rfqNumber,
		DueDateLabel:   h.catalog.TOrDefault(locale, "field.RequestForQuotation.due_date", "Due Date"),
		DueDate:        dueDate,
		StatusLabel:    h.catalog.TOrDefault(locale, "field.RequestForQuotation.status_id", "Status"),
		Status:         statusLabel,
		ItemCol:        h.catalog.TOrDefault(locale, "entity.Item.name", "Item"),
		QtyCol:         h.catalog.TOrDefault(locale, "field.RequestForQuotationLine.qty", "Quantity"),
		FooterLabel:    h.catalog.T(locale, "report.rfq.footer_total"),
		Empty:          h.catalog.T(locale, "report.rfq.empty"),
		Missing:        h.catalog.T(locale, "report.rfq.missing_quote"),
	}

	// Zero lines or zero invited vendors: nothing to cross into a grid —
	// render the empty state instead of a table with no columns/rows
	// (same "empty-state message, not an empty table" pattern
	// VendorEmpty/StockoutEmpty already use in reporting.go).
	if len(lines) == 0 || len(vendors) == 0 {
		return view
	}

	for _, v := range vendors {
		view.Vendors = append(view.Vendors, rfqVendorColView{ID: v.ID, Name: v.Name})
	}

	// totals accumulates in money.Money (minor-unit int64), not float64
	// (uc-infra#68): this is the exact map the originating independent
	// review found summing to a visible IEEE artifact (0.1 + 0.2 =
	// 0.30000000000000004) when quoted prices were plain FieldNumber
	// floats. RequestForQuotationQuoteLine.unit_price is FieldMoney now,
	// data.ReportingRepo.RFQComparison hands back QuotesByVendor as
	// money.Money already, and `totals[v.ID] += price` below is exact
	// int64 addition — no float ever enters this accumulation.
	totals := make(map[string]money.Money, len(vendors))
	quoted := make(map[string]bool, len(vendors))
	for _, line := range lines {
		row := rfqLineRowView{Item: line.ItemName, Qty: formrender.FormatFieldValue(line.Qty)}
		var lowest money.Money
		haveLowest := false
		for _, v := range vendors {
			price, ok := line.QuotesByVendor[v.ID]
			if ok && (!haveLowest || price < lowest) {
				lowest = price
				haveLowest = true
			}
		}
		for _, v := range vendors {
			cell := rfqCellView{}
			if price, ok := line.QuotesByVendor[v.ID]; ok {
				cell.Value = price.String()
				cell.Present = true
				cell.Lowest = haveLowest && price == lowest
				totals[v.ID] += price
				quoted[v.ID] = true
			}
			row.Cells = append(row.Cells, cell)
		}
		view.Rows = append(view.Rows, row)
	}

	for _, v := range vendors {
		cell := rfqCellView{}
		if quoted[v.ID] {
			cell.Value = totals[v.ID].String()
			cell.Present = true
		}
		view.FooterCells = append(view.FooterCells, cell)
	}

	return view
}

type rfqReportView struct {
	Title string

	RFQNumberLabel string
	RFQNumber      string
	DueDateLabel   string
	DueDate        string
	StatusLabel    string
	Status         string

	ItemCol string
	QtyCol  string

	Vendors []rfqVendorColView
	Rows    []rfqLineRowView

	FooterLabel string
	FooterCells []rfqCellView

	Empty   string
	Missing string

	// Help is the page-level "?" affordance (ADR-0023, uc-infra#143/#152)
	// — see buildHelpView.
	Help helpView
}

type rfqVendorColView struct {
	ID   string
	Name string
}

type rfqLineRowView struct {
	Item  string
	Qty   string
	Cells []rfqCellView
}

// rfqCellView is one (line, vendor) cell — Present distinguishes a real
// quoted price from a genuinely missing one, which the template renders
// as Missing ("—") rather than a blank string that could be confused
// with a real empty cell in a screenshot/DOM assertion.
type rfqCellView struct {
	Value   string
	Present bool
	Lowest  bool
}

var rfqReportTmpl = template.Must(template.New("rfqReport").Parse(`
<h1>{{.Title}} <a class="uc-help-affordance" role="link" data-help-topic="{{.Help.TopicID}}"{{if .Help.Href}} href="{{.Help.Href}}"{{end}}{{if .Help.Disabled}} aria-disabled="true"{{end}} tabindex="0" aria-label="{{.Help.Label}}">?</a></h1>
<p>
  <strong>{{.RFQNumberLabel}}:</strong> {{.RFQNumber}}
  &nbsp;&nbsp;<strong>{{.DueDateLabel}}:</strong> {{.DueDate}}
  &nbsp;&nbsp;<strong>{{.StatusLabel}}:</strong> {{.Status}}
</p>

{{if and .Vendors .Rows}}
<table class="uc-table">
<thead>
<tr>
  <th>{{.ItemCol}}</th>
  <th>{{.QtyCol}}</th>
  {{range .Vendors}}<th class="uc-rfq-vendor-col">{{.Name}}</th>{{end}}
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr>
  <td>{{.Item}}</td>
  <td>{{.Qty}}</td>
  {{range .Cells}}<td{{if .Lowest}} class="uc-rfq-lowest"{{end}}>{{if .Present}}{{.Value}}{{else}}{{$.Missing}}{{end}}</td>{{end}}
</tr>
{{end}}
</tbody>
<tfoot>
<tr>
  <td>{{.FooterLabel}}</td>
  <td></td>
  {{range .FooterCells}}<td>{{if .Present}}{{.Value}}{{else}}{{$.Missing}}{{end}}</td>{{end}}
</tr>
</tfoot>
</table>
{{else}}
<p class="uc-empty">{{.Empty}}</p>
{{end}}
`))
