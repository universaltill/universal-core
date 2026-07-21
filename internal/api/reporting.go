package api

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"

	"github.com/universaltill/universal-core/internal/kernel/formrender"
)

// reportTopVendorLimit and reportStockoutLimit cap the two ranked tables
// on the purchasing report — a management report is meant to be read at
// a glance, not paginated; same "days, not weeks" scope this whole demo
// increment is held to (QUEUE.md's Ansar Group opportunity entry) rather
// than building real pagination/sorting controls for a first workbench.
const (
	reportTopVendorLimit = 10
	reportStockoutLimit  = 20
)

// poStatusDisplayOrder is the fixed left-to-right order the status
// breakdown cards render in — entity.PurchaseOrder's own EnumValues
// order (draft → submitted → approved → received → cancelled), not
// whatever order Postgres's GROUP BY happens to return, which is
// unspecified. A status with zero orders is simply omitted, not shown
// as a zero row — with only 5 statuses this reads better as "the
// statuses that actually have activity," not a wall of zeroes for a new
// tenant.
var poStatusDisplayOrder = []string{"draft", "submitted", "approved", "received", "cancelled"}

// renderPurchasingReport is the "mgmt reporting workbench" QUEUE.md's
// Ansar Group opportunity entry has been tracking since the
// purchasing-module increment: a read-only, at-a-glance view over the
// purchasing/stock-intelligence data this kernel can already model —
// PurchaseOrder status/value breakdown, top vendors by spend, and a
// stock summary with a stockout-risk list (items with nothing left
// available to promise). Deliberately not the R9/R10 vision (workflow-
// triggered reorder alerts, P50/P90 lead-time forecasting) — this reads
// data that already exists, it doesn't predict anything.
//
// Plain server-rendered HTML, no htmx/JS — same reasoning
// list-page-pagination's own review doc gave for skipping a browser e2e
// test: there's no client-side interactivity here for a browser-only
// bug class to hide in.
func (h *Handler) renderPurchasingReport(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)
	ctx := r.Context()

	statusRows, err := h.reporting.PurchaseOrderStatusBreakdown(ctx, rc.TenantID)
	if err != nil {
		writeInternalError(w, "purchase order status breakdown", err)
		return
	}
	byStatus := make(map[string]struct {
		Count int
		Value float64
	}, len(statusRows))
	for _, row := range statusRows {
		byStatus[row.Status] = struct {
			Count int
			Value float64
		}{row.Count, row.Value}
	}

	vendors, err := h.reporting.TopVendorsBySpend(ctx, rc.TenantID, reportTopVendorLimit)
	if err != nil {
		writeInternalError(w, "top vendors by spend", err)
		return
	}

	stock, err := h.reporting.StockSummary(ctx, rc.TenantID)
	if err != nil {
		writeInternalError(w, "stock summary", err)
		return
	}

	stockouts, err := h.reporting.StockoutRiskItems(ctx, rc.TenantID, reportStockoutLimit)
	if err != nil {
		writeInternalError(w, "stockout risk items", err)
		return
	}

	view := purchasingReportView{
		Title:         h.catalog.T(locale, "report.purchasing.title"),
		StatusHeading: h.catalog.T(locale, "report.purchasing.status_heading"),
		VendorHeading: h.catalog.T(locale, "report.purchasing.vendor_heading"),
		VendorEmpty:   h.catalog.T(locale, "report.purchasing.vendor_empty"),
		// Column headers reuse the same field.{EntityType}.{FieldName}
		// i18n keys forms/list pages already use where the concept is
		// identical (a report's "Name" column is the same "Name" a
		// Party form already labels) — new keys only for concepts that
		// don't already have one (order counts, per-status/stockout
		// counts, headings, empty states).
		VendorNameCol:     h.catalog.TOrDefault(locale, "field.Party.name", "Name"),
		VendorOrdersCol:   h.catalog.T(locale, "report.purchasing.vendor_orders_col"),
		VendorSpendCol:    h.catalog.TOrDefault(locale, "field.PurchaseOrder.total", "Total"),
		StockHeading:      h.catalog.T(locale, "report.purchasing.stock_heading"),
		StockItemsLabel:   h.catalog.T(locale, "report.purchasing.stock_items_label"),
		StockOnHandLabel:  h.catalog.TOrDefault(locale, "field.InventoryItem.qty_on_hand", "Qty On Hand"),
		StockATPLabel:     h.catalog.TOrDefault(locale, "field.InventoryItem.qty_available_to_promise", "Qty Available to Promise"),
		StockoutHeading:   h.catalog.T(locale, "report.purchasing.stockout_heading"),
		StockoutEmpty:     h.catalog.T(locale, "report.purchasing.stockout_empty"),
		StockoutSKUCol:    h.catalog.TOrDefault(locale, "field.Item.sku", "SKU"),
		StockoutNameCol:   h.catalog.TOrDefault(locale, "field.Item.name", "Name"),
		StockoutOnHandCol: h.catalog.TOrDefault(locale, "field.InventoryItem.qty_on_hand", "Qty On Hand"),
		StockoutATPCol:    h.catalog.TOrDefault(locale, "field.InventoryItem.qty_available_to_promise", "Qty Available to Promise"),
		StockItemCount:    strconv.Itoa(stock.ItemCount),
		StockOnHand:       formrender.FormatFieldValue(stock.TotalOnHand),
		StockATP:          formrender.FormatFieldValue(stock.TotalATP),
		StockoutCount:     strconv.Itoa(stock.StockoutCount),
	}
	for _, status := range poStatusDisplayOrder {
		row, ok := byStatus[status]
		if !ok {
			continue
		}
		view.StatusCards = append(view.StatusCards, statusCardView{
			Label: h.catalog.TOrDefault(locale, "field.PurchaseOrder.status."+status, status),
			Count: strconv.Itoa(row.Count),
			Value: formrender.FormatFieldValue(row.Value),
		})
	}
	for _, v := range vendors {
		view.Vendors = append(view.Vendors, vendorRowView{
			Name:   v.VendorName,
			Orders: strconv.Itoa(v.OrderCount),
			Spend:  formrender.FormatFieldValue(v.Total),
		})
	}
	for _, item := range stockouts {
		view.Stockouts = append(view.Stockouts, stockoutRowView{
			SKU:    item.SKU,
			Name:   item.Name,
			OnHand: formrender.FormatFieldValue(item.QtyOnHand),
			ATP:    formrender.FormatFieldValue(item.QtyATP),
			Href:   "/forms/Item/" + item.ItemID,
		})
	}

	var buf bytes.Buffer
	if err := purchasingReportTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render purchasing report", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render purchasing report shell", err)
	}
}

type purchasingReportView struct {
	Title string

	StatusHeading string
	StatusCards   []statusCardView

	VendorHeading   string
	VendorEmpty     string
	VendorNameCol   string
	VendorOrdersCol string
	VendorSpendCol  string
	Vendors         []vendorRowView

	StockHeading     string
	StockItemsLabel  string
	StockOnHandLabel string
	StockATPLabel    string
	StockItemCount   string
	StockOnHand      string
	StockATP         string

	StockoutHeading   string
	StockoutEmpty     string
	StockoutSKUCol    string
	StockoutNameCol   string
	StockoutOnHandCol string
	StockoutATPCol    string
	StockoutCount     string
	Stockouts         []stockoutRowView
}

type statusCardView struct {
	Label string
	Count string
	Value string
}

type vendorRowView struct {
	Name   string
	Orders string
	Spend  string
}

type stockoutRowView struct {
	SKU    string
	Name   string
	OnHand string
	ATP    string
	Href   string
}

var purchasingReportTmpl = template.Must(template.New("purchasingReport").Parse(`
<h1>{{.Title}}</h1>

<h2>{{.StatusHeading}}</h2>
<div class="uc-report-cards">
{{range .StatusCards}}
<div class="uc-report-card">
  <div class="uc-report-card-label">{{.Label}}</div>
  <div class="uc-report-card-value">{{.Count}}</div>
  <div class="uc-report-card-sub">{{.Value}}</div>
</div>
{{end}}
</div>

<h2>{{.VendorHeading}}</h2>
{{if .Vendors}}
<table class="uc-table">
<thead><tr><th>{{.VendorNameCol}}</th><th>{{.VendorOrdersCol}}</th><th>{{.VendorSpendCol}}</th></tr></thead>
<tbody>
{{range .Vendors}}
<tr><td>{{.Name}}</td><td>{{.Orders}}</td><td>{{.Spend}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.VendorEmpty}}</p>
{{end}}

<h2>{{.StockHeading}}</h2>
<div class="uc-report-cards">
<div class="uc-report-card">
  <div class="uc-report-card-label">{{.StockItemsLabel}}</div>
  <div class="uc-report-card-value">{{.StockItemCount}}</div>
</div>
<div class="uc-report-card">
  <div class="uc-report-card-label">{{.StockOnHandLabel}}</div>
  <div class="uc-report-card-value">{{.StockOnHand}}</div>
</div>
<div class="uc-report-card">
  <div class="uc-report-card-label">{{.StockATPLabel}}</div>
  <div class="uc-report-card-value">{{.StockATP}}</div>
</div>
</div>

<h2>{{.StockoutHeading}} ({{.StockoutCount}})</h2>
{{if .Stockouts}}
<table class="uc-table">
<thead><tr><th>{{.StockoutSKUCol}}</th><th>{{.StockoutNameCol}}</th><th>{{.StockoutOnHandCol}}</th><th>{{.StockoutATPCol}}</th></tr></thead>
<tbody>
{{range .Stockouts}}
<tr><td><a href="{{.Href}}">{{.SKU}}</a></td><td>{{.Name}}</td><td>{{.OnHand}}</td><td>{{.ATP}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.StockoutEmpty}}</p>
{{end}}
`))
