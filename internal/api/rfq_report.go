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

	// Item.name (the grid's row label) and Party.name (the vendor column
	// header) are both read straight through data.ReportingRepo.
	// RFQComparison's raw SQL — entirely outside the ts.crud-redacted
	// path a form/CRUD page would otherwise apply. Same FieldPermission-
	// bypass shape reporting.go's purchasing report already fixed twice
	// (uc-infra#230/#233); reusing its exact itemNameRedacted/
	// partyNameRedacted predicates rather than re-declaring them, since
	// this file lives in the same api package. Neither field drives row
	// or column MEMBERSHIP here (every requested line and every invited
	// vendor still gets a row/column regardless of whether its label is
	// hidden) — this is pure cell-level redaction, the same scope #234
	// itself called for. Resolved here, after the id is validated and the
	// RFQ is loaded (not ahead of requireReportRead, like an earlier
	// draft did) — matching project_budget_report.go's own ordering: a
	// malformed id or a nonexistent RFQ is handled by the checks above
	// before spending two extra authz lookups, and an authz failure here
	// stays a 500 on a REAL RFQ rather than masking a 404 on a bad one.
	hiddenItemFields, err := ts.crud.HiddenFields(ctx, "Item")
	if err != nil {
		writeInternalError(w, "resolve Item field visibility", err)
		return
	}
	hiddenPartyFields, err := ts.crud.HiddenFields(ctx, "Party")
	if err != nil {
		writeInternalError(w, "resolve Party field visibility", err)
		return
	}
	// RequestForQuotationLine.qty/RequestForQuotationQuoteLine.unit_price
	// (uc-infra#235) are resolved the same way: two more per-entity-type
	// HiddenFields lookups (a third and fourth, alongside Item/Party
	// above), since HiddenFields answers for one (actor, entity type)
	// pair at a time and both fields reach this report through the exact
	// same unredacted data.ReportingRepo.RFQComparison raw SQL as
	// Item.name/Party.name.
	hiddenLineFields, err := ts.crud.HiddenFields(ctx, "RequestForQuotationLine")
	if err != nil {
		writeInternalError(w, "resolve RequestForQuotationLine field visibility", err)
		return
	}
	hiddenQuoteLineFields, err := ts.crud.HiddenFields(ctx, "RequestForQuotationQuoteLine")
	if err != nil {
		writeInternalError(w, "resolve RequestForQuotationQuoteLine field visibility", err)
		return
	}
	itemNameHidden := itemNameRedacted(hiddenItemFields)
	vendorNameHidden := partyNameRedacted(hiddenPartyFields)
	qtyHidden := rfqLineQtyRedacted(hiddenLineFields)
	unitPriceHidden := rfqQuoteLineUnitPriceRedacted(hiddenQuoteLineFields)

	view := h.buildRFQReportView(ctx, ts, rfq, lines, vendors, locale, itemNameHidden, vendorNameHidden, qtyHidden, unitPriceHidden)
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

// rfqLineQtyRedacted/rfqQuoteLineUnitPriceRedacted (uc-infra#235) answer
// whether THIS ACTOR's FieldPermission rows hide
// RequestForQuotationLine.qty / RequestForQuotationQuoteLine.unit_price
// respectively — the same one-line "look the field name up in the
// HiddenFields map" shape as reporting.go's own
// purchaseOrderTotalRedacted/partyNameRedacted/itemSKURedacted/
// itemNameRedacted, which this file's itemNameRedacted/partyNameRedacted
// calls above already reuse rather than redeclare. Declared here, not in
// reporting.go, since neither field is read by any report outside this
// one — unlike Item.name/Party.name, which multiple reports share.
func rfqLineQtyRedacted(hidden map[string]bool) bool {
	return hidden["qty"]
}

func rfqQuoteLineUnitPriceRedacted(hidden map[string]bool) bool {
	return hidden["unit_price"]
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
//
// itemNameHidden/vendorNameHidden (uc-infra#234) blank the Item/vendor-
// name cells to NotAvailable when this actor's FieldPermission hides
// Item.name/Party.name respectively — mirroring reporting.go's own
// vendorNameRedacted threading into buildLeadTimeRows/buildOnTimeRows/
// buildQualityRows. Neither flag touches row/column membership.
//
// qtyRedacted/unitPriceRedacted (uc-infra#235) cover the two fields #234
// deliberately left open: RequestForQuotationLine.qty and
// RequestForQuotationQuoteLine.unit_price, both FieldPermission-hideable
// exactly like Item.name/Party.name and reaching this page through the
// identical unredacted raw-SQL path. Like the name fields, neither drives
// row/column MEMBERSHIP (every requested line and every invited vendor
// still gets a row/column) — but unlike a pure display field,
// unitPriceRedacted also has to gate the DERIVED values price drives:
// the .uc-rfq-lowest mark (which cell is cheapest is itself information
// about the hidden prices) and the per-vendor footer total (an aggregate
// over the same hidden field, same "can't see it summed if you can't see
// it per-row" rule StockOnHand/StockATP already apply in reporting.go).
// A cell whose price genuinely exists but is redacted renders
// NotAvailable (rfqCellView.Hidden), distinct from Missing — a vendor who
// never quoted at all — the same distinction itemNameHidden/
// vendorNameHidden already draw between "redacted" and "never had a
// value".
//
// Also NOT addressed: vendors are ordered ORDER BY p.data->>'name' (data.
// ReportingRepo.RFQComparison) even when vendorNameHidden — so a
// restricted actor still learns every invited vendor's alphabetical rank
// by name from column POSITION alone, a residual channel over the exact
// value being redacted. Same documented, deliberately out-of-scope shape
// reporting.go's own vendor-spend/lead-time tables carry for Party.name
// ordering (see reporting.go's "Sort ORDER is still computed from the
// real, un-redacted vendor names" comment) — not fixed here for the same
// reason: changing the sort key is a behavior call, not a mechanical
// redaction.
//
// A related, deliberate residual channel of its own (independent review,
// uc-infra#235): rendering Hidden distinctly from Missing tells a
// unitPriceRedacted actor exactly WHICH (line, vendor) pairs have a real
// quote on record and which vendors quoted anything at all — i.e.
// RequestForQuotationQuoteLine record EXISTENCE, not the redacted
// unit_price VALUE itself. Accepted rather than collapsed into a single
// blank state: this is what the ticket asked for (a genuinely missing
// quote must never be confused with a redacted one), and the actor
// already cleared requireReportRead on RequestForQuotationQuoteLine to
// reach this page at all, so record existence on an entity type they can
// read is a smaller disclosure than the value FieldPermission protects.
func (h *Handler) buildRFQReportView(ctx context.Context, ts tenantScope, rfq data.Record, lines []data.RFQComparisonLine, vendors []data.RFQComparisonVendor, locale string, itemNameHidden, vendorNameHidden, qtyRedacted, unitPriceRedacted bool) rfqReportView {
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
		NotAvailable:   h.catalog.T(locale, "report.rfq.not_available"),
	}

	// Zero lines or zero invited vendors: nothing to cross into a grid —
	// render the empty state instead of a table with no columns/rows
	// (same "empty-state message, not an empty table" pattern
	// VendorEmpty/StockoutEmpty already use in reporting.go).
	if len(lines) == 0 || len(vendors) == 0 {
		return view
	}

	for _, v := range vendors {
		col := rfqVendorColView{ID: v.ID}
		if !vendorNameHidden {
			col.NameAvailable = true
			col.Name = v.Name
		}
		view.Vendors = append(view.Vendors, col)
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
	// quotedAny tracks "did this vendor quote this line at all", computed
	// regardless of unitPriceRedacted — the footer loop below needs it
	// even when totals is never populated, to tell a genuinely
	// never-quoted vendor (Missing) apart from a redacted one (Hidden).
	quotedAny := make(map[string]bool, len(vendors))
	for _, line := range lines {
		row := rfqLineRowView{}
		if !qtyRedacted {
			row.QtyAvailable = true
			row.Qty = formrender.FormatFieldValue(line.Qty)
		}
		if !itemNameHidden {
			row.ItemAvailable = true
			row.Item = line.ItemName
		}
		// The lowest-price mark is itself derived from the hidden prices
		// (which cell is cheapest is information about them) — skipped
		// entirely, not just left unmarked, when unitPriceRedacted, same
		// "gate the derived value too" rule the footer totals below
		// follow.
		var lowest money.Money
		haveLowest := false
		if !unitPriceRedacted {
			for _, v := range vendors {
				price, ok := line.QuotesByVendor[v.ID]
				if ok && (!haveLowest || price < lowest) {
					lowest = price
					haveLowest = true
				}
			}
		}
		for _, v := range vendors {
			cell := rfqCellView{}
			if price, ok := line.QuotesByVendor[v.ID]; ok {
				quotedAny[v.ID] = true
				if unitPriceRedacted {
					cell.Hidden = true
				} else {
					cell.Value = price.String()
					cell.Present = true
					cell.Lowest = haveLowest && price == lowest
					totals[v.ID] += price
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		view.Rows = append(view.Rows, row)
	}

	for _, v := range vendors {
		cell := rfqCellView{}
		switch {
		case unitPriceRedacted && quotedAny[v.ID]:
			cell.Hidden = true
		case !unitPriceRedacted && quotedAny[v.ID]:
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

	Empty        string
	Missing      string
	NotAvailable string

	// Help is the page-level "?" affordance (ADR-0023, uc-infra#143/#152)
	// — see buildHelpView.
	Help helpView
}

type rfqVendorColView struct {
	ID            string
	Name          string
	NameAvailable bool
}

type rfqLineRowView struct {
	Item          string
	ItemAvailable bool
	Qty           string
	QtyAvailable  bool
	Cells         []rfqCellView
}

// rfqCellView is one (line, vendor) cell, or one footer (vendor) total —
// Present/Hidden/neither are mutually exclusive: Present is a real quoted
// price this actor may see; Hidden (uc-infra#235) is a real quoted price
// that exists but is FieldPermission-redacted, rendered as NotAvailable —
// distinct from neither Present nor Hidden, a genuinely missing quote,
// which the template renders as Missing ("—") rather than a blank string
// that could be confused with a real empty cell in a screenshot/DOM
// assertion.
type rfqCellView struct {
	Value   string
	Present bool
	Hidden  bool
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
  {{range .Vendors}}<th class="uc-rfq-vendor-col">{{if .NameAvailable}}{{.Name}}{{else}}{{$.NotAvailable}}{{end}}</th>{{end}}
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr>
  <td>{{if .ItemAvailable}}{{.Item}}{{else}}{{$.NotAvailable}}{{end}}</td>
  <td>{{if .QtyAvailable}}{{.Qty}}{{else}}{{$.NotAvailable}}{{end}}</td>
  {{range .Cells}}<td{{if .Lowest}} class="uc-rfq-lowest"{{end}}>{{if .Present}}{{.Value}}{{else if .Hidden}}{{$.NotAvailable}}{{else}}{{$.Missing}}{{end}}</td>{{end}}
</tr>
{{end}}
</tbody>
<tfoot>
<tr>
  <td>{{.FooterLabel}}</td>
  <td></td>
  {{range .FooterCells}}<td>{{if .Present}}{{.Value}}{{else if .Hidden}}{{$.NotAvailable}}{{else}}{{$.Missing}}{{end}}</td>{{end}}
</tr>
</tfoot>
</table>
{{else}}
<p class="uc-empty">{{.Empty}}</p>
{{end}}
`))
