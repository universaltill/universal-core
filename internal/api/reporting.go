package api

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/forecast"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	"github.com/universaltill/universal-core/internal/kernel/money"
)

// reportTopVendorLimit and reportStockoutLimit cap the two ranked tables
// on the purchasing report — a management report is meant to be read at
// a glance, not paginated; same "days, not weeks" scope this whole demo
// increment is held to (QUEUE.md's design-partner opportunity entry) rather
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

// purchasingReportEntityTypes is every entity type the report's
// aggregate queries read from — including join targets, not just the
// entity type each query's WHERE clause filters on:
// PurchaseOrderStatusBreakdown joins PurchaseOrder to Status (reads the
// status code off the Status record, not PurchaseOrder's own data), and
// TopVendorsBySpend/StockoutRiskItems join to Party/Item respectively.
// #30's additions: CompletedPOLeadTimes joins PurchaseOrder to Party
// and GoodsReceipt (the received_at fallback), OnOrderQtyByItem/
// LatestPOVendorByItem read POLine joined to PurchaseOrder (+Status),
// and the reorder-signal section reads ReorderRule (via the guarded
// engine, which would enforce this anyway — listed here too so the
// whole-page gate stays one honest unit).
// uc-infra#54's addition: OnOrderQtyByItem's netting subquery now also
// reads GoodsReceiptLine.qty_received — a role denied read on
// GoodsReceiptLine would otherwise still see its effect leak through
// every netted on-order quantity in this report, even with
// GoodsReceiptLine itself correctly gated everywhere else.
// uc-infra#82's addition: GoodsReceiptLineQualities reads
// GoodsReceiptLine joined to GoodsReceipt, PurchaseOrder, and Party.
// Missing a join target here is a real leak, not a cosmetic gap: a role
// denied read on Status alone would otherwise still see every status
// label and count in the breakdown even with PurchaseOrder itself
// correctly gated. ADR-0006's 2026-07-30 addendum
// (uc-infra/docs/adr/0006-rbac-enforcement-guarded-engine.md) gates the
// whole report on read access to all of these as one unit, ahead of
// running any of the queries — not per-row/per-column filtering within
// the report, which is a separate, still-deferred design problem (there
// is no entity.Definition for a hand-written aggregate to filter
// against).
var purchasingReportEntityTypes = []string{"PurchaseOrder", "Status", "Party", "InventoryItem", "Item", "POLine", "GoodsReceipt", "GoodsReceiptLine", "ReorderRule"}

// requireReportRead denies the whole report page unless the actor can
// read every one of entityTypes, reusing ts.crud.CanRead (the same
// GuardedEngine/Resolver method dashboard.go already uses to filter nav
// links) rather than inventing a new permission check: ReportingRepo
// itself bypasses the guarded engine for its aggregate SQL, but the
// question this must answer first — can this actor see PurchaseOrder/
// Party/InventoryItem/Item (or, for the RFQ comparison report,
// RequestForQuotation/.../Party) at all — is exactly what the Resolver
// already knows, opt-in semantics and all (a type with zero Permission
// rows stays readable, same as everywhere else this method is used).
// Shared by every whole-page report gate in this package (see
// purchasingReportEntityTypes/rfqReportEntityTypes) rather than each
// report re-implementing the same loop.
func (h *Handler) requireReportRead(w http.ResponseWriter, r *http.Request, rc *httpx.RequestContext, ts tenantScope, locale string, entityTypes []string) bool {
	for _, entityType := range entityTypes {
		allowed, err := ts.crud.CanRead(r.Context(), entityType)
		if !h.denyPageUnless(w, r, rc, locale, allowed, err, "check "+entityType+" read permission for report") {
			return false
		}
	}
	return true
}

// purchaseOrderTotalRedacted/partyNameRedacted/itemSKURedacted/
// itemNameRedacted (uc-infra#233) each answer whether THIS ACTOR's
// FieldPermission hides one specific field this report reads through
// ts.reporting's raw SQL (data.ReportingRepo) — entirely outside the
// ts.crud-redacted path a generated CRUD page or form would otherwise
// apply. Kept as their own named, unit-tested predicates rather than an
// inline map lookup at each call site, same reasoning
// project_budget_report.go's projectBudgetLinePlannedRedacted/
// projectBudgetLineCategoryRedacted already give: a typo in a literal
// field name would otherwise silently disable the redaction with no
// unit-test-level signal, only a slower, coarser HTTP-level regression
// test to (maybe) catch it. uc-infra#230 (a separate, still-open fix for
// this same report's InventoryItem.qty_on_hand/qty_available_to_promise)
// establishes an equivalent pair of predicates of its own when it lands
// — not present in this codebase yet, so not referenced as existing
// precedent here.
func purchaseOrderTotalRedacted(hidden map[string]bool) bool {
	return hidden["total"]
}

func partyNameRedacted(hidden map[string]bool) bool {
	return hidden["name"]
}

func itemSKURedacted(hidden map[string]bool) bool {
	return hidden["sku"]
}

func itemNameRedacted(hidden map[string]bool) bool {
	return hidden["name"]
}

// renderPurchasingReport is the "mgmt reporting workbench" QUEUE.md's
// design-partner opportunity entry has been tracking since the
// purchasing-module increment: a read-only, at-a-glance view over the
// purchasing/stock-intelligence data this kernel can already model —
// PurchaseOrder status/value breakdown, top vendors by spend, and a
// stock summary with a stockout-risk list (items with nothing left
// available to promise). #30 added the first slice of the R10 vision on
// top: empirical P50/P90 supplier lead times over completed POs
// (internal/kernel/forecast — classical stats, no AI) and
// position-based reorder signals against ReorderRule records. Still no
// R9 workflow alerts — the report shows signals, it doesn't act on
// them.
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
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)
	ctx := r.Context()

	if !h.requireReportRead(w, r, &rc, ts, locale, purchasingReportEntityTypes) {
		return
	}

	// PurchaseOrder.total, Party.name, and Item.sku/Item.name can each be
	// hidden per-role via a FieldPermission (ADR-0006) — same mechanism
	// project_budget_report.go's own redaction already uses (HiddenFields
	// answers for one (actor, entity type) pair, and every row of a given
	// entity type on this page shares the same actor). uc-infra#233: the
	// status cards' Value, the vendor table's Spend/Name, the Supplier
	// Lead Times/On-Time Delivery/Quality tables' Vendor column, and the
	// stockout table's SKU/Name are all read through this exact same
	// unredacted ts.reporting raw-SQL path — without this, a restricted
	// actor's real PurchaseOrder.total/Party.name/Item.sku/Item.name
	// sails straight into this report regardless of what their role
	// hides on /api/records/{PurchaseOrder,Party,Item} or any generated
	// form. Party.name in particular renders on FOUR separate tables on
	// this one page (vendor spend, lead time, on-time delivery, quality)
	// — vendorNameRedacted below is threaded into all four call sites,
	// not just the vendor-spend one (an earlier draft of this fix missed
	// the other three; caught by independent review before it shipped).
	//
	// This report's OTHER known raw-SQL-bypasses-FieldPermission gap —
	// InventoryItem.qty_on_hand/qty_available_to_promise, on this same
	// page's Stock Summary/Stockout Risk/Reorder Signals sections — is
	// tracked separately as uc-infra#230 and is NOT fixed by this diff;
	// do not assume those quantities are redacted just because this
	// comment block exists nearby.
	//
	// The reorder table's Item name is deliberately NOT covered here —
	// buildReorderSignals already reads it through ts.crud.Get, which
	// redacts on its own; resolving Item's HiddenFields a second time for
	// that one field would be redundant, not a gap.
	hiddenPOFields, err := ts.crud.HiddenFields(ctx, "PurchaseOrder")
	if err != nil {
		writeInternalError(w, "resolve PurchaseOrder field visibility", err)
		return
	}
	hiddenPartyFields, err := ts.crud.HiddenFields(ctx, "Party")
	if err != nil {
		writeInternalError(w, "resolve Party field visibility", err)
		return
	}
	hiddenItemFields, err := ts.crud.HiddenFields(ctx, "Item")
	if err != nil {
		writeInternalError(w, "resolve Item field visibility", err)
		return
	}
	totalRedacted := purchaseOrderTotalRedacted(hiddenPOFields)
	vendorNameRedacted := partyNameRedacted(hiddenPartyFields)
	skuRedacted := itemSKURedacted(hiddenItemFields)
	stockoutNameRedacted := itemNameRedacted(hiddenItemFields)

	statusRows, err := ts.reporting.PurchaseOrderStatusBreakdown(ctx)
	if err != nil {
		writeInternalError(w, "purchase order status breakdown", err)
		return
	}
	byStatus := make(map[string]struct {
		Count int
		Value money.Money
	}, len(statusRows))
	for _, row := range statusRows {
		byStatus[row.Status] = struct {
			Count int
			Value money.Money
		}{row.Count, row.Value}
	}

	vendors, err := ts.reporting.TopVendorsBySpend(ctx, reportTopVendorLimit)
	if err != nil {
		writeInternalError(w, "top vendors by spend", err)
		return
	}

	stock, err := ts.reporting.StockSummary(ctx)
	if err != nil {
		writeInternalError(w, "stock summary", err)
		return
	}

	stockouts, err := ts.reporting.StockoutRiskItems(ctx, reportStockoutLimit)
	if err != nil {
		writeInternalError(w, "stockout risk items", err)
		return
	}

	leadTimes, err := ts.reporting.CompletedPOLeadTimes(ctx)
	if err != nil {
		writeInternalError(w, "completed po lead times", err)
		return
	}
	stats := forecast.Compute(leadTimeSamples(leadTimes))
	onTimeStats := forecast.ComputeOnTime(onTimeSamples(leadTimes))

	qualityLines, err := ts.reporting.GoodsReceiptLineQualities(ctx)
	if err != nil {
		writeInternalError(w, "goods receipt line qualities", err)
		return
	}
	qualityStats := forecast.ComputeQuality(qualitySamples(qualityLines))

	signals, err := h.buildReorderSignals(ctx, ts, stats, locale)
	if err != nil {
		writeInternalError(w, "build reorder signals", err)
		return
	}

	view := purchasingReportView{
		Title:         h.catalog.T(locale, "report.purchasing.title"),
		NotAvailable:  h.catalog.T(locale, "report.purchasing.not_available"),
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

		LeadTimeHeading:    h.catalog.T(locale, "report.purchasing.leadtime_heading"),
		LeadTimeEmpty:      h.catalog.T(locale, "report.purchasing.leadtime_empty"),
		LeadTimeVendorCol:  h.catalog.TOrDefault(locale, "field.PurchaseOrder.vendor_id", "Vendor"),
		LeadTimeSamplesCol: h.catalog.T(locale, "report.purchasing.leadtime_samples_col"),
		LeadTimeP50Col:     h.catalog.T(locale, "report.purchasing.leadtime_p50_col"),
		LeadTimeP90Col:     h.catalog.T(locale, "report.purchasing.leadtime_p90_col"),

		OnTimeHeading:    h.catalog.T(locale, "report.purchasing.ontime_heading"),
		OnTimeEmpty:      h.catalog.T(locale, "report.purchasing.ontime_empty"),
		OnTimeVendorCol:  h.catalog.TOrDefault(locale, "field.PurchaseOrder.vendor_id", "Vendor"),
		OnTimeSamplesCol: h.catalog.T(locale, "report.purchasing.ontime_samples_col"),
		OnTimeRateCol:    h.catalog.T(locale, "report.purchasing.ontime_rate_col"),

		QualityHeading:    h.catalog.T(locale, "report.purchasing.quality_heading"),
		QualityEmpty:      h.catalog.T(locale, "report.purchasing.quality_empty"),
		QualityVendorCol:  h.catalog.TOrDefault(locale, "field.PurchaseOrder.vendor_id", "Vendor"),
		QualitySamplesCol: h.catalog.T(locale, "report.purchasing.quality_samples_col"),
		QualityRateCol:    h.catalog.T(locale, "report.purchasing.quality_rate_col"),

		ReorderHeading:     h.catalog.T(locale, "report.purchasing.reorder_heading"),
		ReorderEmpty:       h.catalog.T(locale, "report.purchasing.reorder_empty"),
		ReorderItemCol:     h.catalog.TOrDefault(locale, "entity.Item.name", "Item"),
		ReorderOnHandCol:   h.catalog.TOrDefault(locale, "field.InventoryItem.qty_on_hand", "Qty On Hand"),
		ReorderOnOrderCol:  h.catalog.T(locale, "report.purchasing.reorder_on_order_col"),
		ReorderPositionCol: h.catalog.T(locale, "report.purchasing.reorder_position_col"),
		ReorderPointCol:    h.catalog.TOrDefault(locale, "field.ReorderRule.reorder_point", "Reorder Point"),
		ReorderExpectedCol: h.catalog.T(locale, "report.purchasing.reorder_expected_col"),
		ReorderRows:        signals,
	}
	view.LeadTimeRows = h.buildLeadTimeRows(stats, leadTimes, locale, vendorNameRedacted)
	view.OnTimeRows = h.buildOnTimeRows(onTimeStats, leadTimes, locale, vendorNameRedacted)
	view.QualityRows = h.buildQualityRows(qualityStats, qualityLines, locale, vendorNameRedacted)
	for _, status := range poStatusDisplayOrder {
		row, ok := byStatus[status]
		if !ok {
			continue
		}
		// Count is NOT gated on totalRedacted: byStatus's presence and
		// row.Count both come from PurchaseOrderStatusBreakdown's
		// count(*), which has nothing to do with PurchaseOrder.total —
		// only the sub-value line (Value) is derived from the
		// FieldPermission-hideable field.
		card := statusCardView{
			Label: h.catalog.TOrDefault(locale, "field.PurchaseOrder.status."+status, status),
			Count: strconv.Itoa(row.Count),
		}
		if !totalRedacted {
			card.ValueAvailable = true
			// row.Value is money.Money (minor units, uc-infra#136) — a
			// plain FormatFieldValue call would print the raw integer
			// ("9500") instead of the major-unit decimal a human reads as
			// money ("95.00"), the same fix formrender's own FieldMoney
			// cases already apply (uc-infra#68).
			card.Value = row.Value.String()
		}
		view.StatusCards = append(view.StatusCards, card)
	}
	// Row PRESENCE in the vendor table (TopVendorsBySpend's own
	// ORDER BY spend DESC LIMIT $1) is still computed from the real,
	// un-redacted PurchaseOrder.total even when Spend itself renders
	// NotAvailable below — a restricted actor sees the correct top-N
	// vendors in the correct relative order without ever seeing the
	// number that order is based on. Deliberately left open here rather
	// than silently gating the whole row (which vendor a business buys
	// from at all is not itself a value FieldPermission on `total`
	// protects) — a residual-ordering concern, same "mechanical display
	// redaction, not a business-behavior call" scope this fix draws
	// relative to uc-infra#231/#232's still-open fire/no-fire questions
	// for a different field on a different table. NOT fixed here; if
	// this needs closing, it is its own follow-up card, the same way
	// #231/#232 are their own cards rather than silently folded into
	// #230's.
	for _, v := range vendors {
		row := vendorRowView{Orders: strconv.Itoa(v.OrderCount)}
		if !vendorNameRedacted {
			row.NameAvailable = true
			row.Name = v.VendorName
		}
		if !totalRedacted {
			row.SpendAvailable = true
			row.Spend = v.Total.String()
		}
		view.Vendors = append(view.Vendors, row)
	}
	// SKU/Name redaction here is orthogonal to OnHand/ATP: row
	// MEMBERSHIP in this table is driven entirely by
	// StockoutRiskItems' own qty_available_to_promise threshold, which
	// has no relationship to Item.sku/Item.name — hiding either field
	// blanks only its own cell, never removes the row. (OnHand/ATP
	// themselves are NOT redacted by this diff at all — that is
	// uc-infra#230's still-open, separate fix; this loop renders them
	// unconditionally, same as before this diff.)
	for _, item := range stockouts {
		row := stockoutRowView{
			OnHand: formrender.FormatFieldValue(item.QtyOnHand),
			ATP:    formrender.FormatFieldValue(item.QtyATP),
			Href:   "/forms/Item/" + item.ItemID,
		}
		if !skuRedacted {
			row.SKUAvailable = true
			row.SKU = item.SKU
		}
		if !stockoutNameRedacted {
			row.NameAvailable = true
			row.Name = item.Name
		}
		view.Stockouts = append(view.Stockouts, row)
	}

	var buf bytes.Buffer
	if err := purchasingReportTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render purchasing report", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render purchasing report shell", err)
	}
}

// leadTimeSamples converts the raw completed-PO rows into forecast
// samples. Dates are parsed leniently: a PO whose order/received date
// doesn't parse is skipped entirely — dates are noisy user input (issue
// #29's review: chronology validation is data hygiene, not ledger-grade,
// and CSV-imported dates bypass it untouched), so bad data degrades the
// sample set, never the page. Stage durations are deliberately NOT fed
// here: the report renders no per-stage medians today (BA R4 marks them
// optional), so forecast.LeadTimeSample.StageDurations stays a tested
// kernel capability with no report-side wiring until a section actually
// shows it (independent review of #30 — don't compute what nothing
// renders).
func leadTimeSamples(rows []data.CompletedPOLeadTime) []forecast.LeadTimeSample {
	samples := make([]forecast.LeadTimeSample, 0, len(rows))
	for _, row := range rows {
		ordered, err := time.Parse("2006-01-02", row.OrderDate)
		if err != nil {
			continue
		}
		received, err := time.Parse("2006-01-02", row.ReceivedDate)
		if err != nil {
			continue
		}
		samples = append(samples, forecast.LeadTimeSample{VendorID: row.VendorID, OrderDate: ordered, ReceivedDate: received})
	}
	return samples
}

// onTimeSamples converts the raw completed-PO rows into forecast
// on-time samples (#11). A row with no promised_delivery_date at all —
// expected to be most of them, since the field is optional and every PO
// written before #11 has none — fails to parse ("" is never a valid
// "2006-01-02") and is skipped here, same as one whose date is present
// but genuinely unparseable (e.g. a CSV import that bypassed
// entity.ValidateRecord — same "noisy user input, degrade the sample
// set" reasoning as leadTimeSamples' own doc comment).
//
// Uses LastReceivedDate, NOT ReceivedDate: on-time has to mean the whole
// order arrived by the promise, not that the first partial shipment did
// (see CompletedPOLeadTime.LastReceivedDate's own doc comment).
func onTimeSamples(rows []data.CompletedPOLeadTime) []forecast.OnTimeSample {
	samples := make([]forecast.OnTimeSample, 0, len(rows))
	for _, row := range rows {
		promised, err := time.Parse("2006-01-02", row.PromisedDeliveryDate)
		if err != nil {
			continue
		}
		received, err := time.Parse("2006-01-02", row.LastReceivedDate)
		if err != nil {
			continue
		}
		samples = append(samples, forecast.OnTimeSample{VendorID: row.VendorID, PromisedDate: promised, ReceivedDate: received})
	}
	return samples
}

// qualitySamples converts the raw GoodsReceiptLine quality rows into
// forecast quality samples (uc-infra#82). A row's QtyAccepted/
// QtyRejected being empty — expected to be most of them, since both
// fields are optional and every line written before uc-infra#82 has
// neither — means HasData stays false, the same "absent, not a
// fabricated zero" treatment onTimeSamples gives an unparseable date. A
// row where exactly one of the two parses (data corrupted after
// bypassing the write-time hook, e.g. a direct DB edit) is treated the
// same as neither parsing: purchasing.validateGoodsReceiptLineQuality's
// whole point is that these two fields are only ever meaningful
// together, so a sample this package can't trust both halves of is not
// a sample it should count as HasData at all, rather than silently
// crediting an accepted (or rejected) total that has no matching other
// half.
func qualitySamples(rows []data.GoodsReceiptLineQuality) []forecast.QualitySample {
	samples := make([]forecast.QualitySample, 0, len(rows))
	for _, row := range rows {
		if row.QtyAccepted == "" && row.QtyRejected == "" {
			continue
		}
		accepted, errA := strconv.ParseFloat(row.QtyAccepted, 64)
		rejected, errR := strconv.ParseFloat(row.QtyRejected, 64)
		if errA != nil || errR != nil {
			continue
		}
		samples = append(samples, forecast.QualitySample{
			VendorID: row.VendorID, QtyAccepted: accepted, QtyRejected: rejected, HasData: true,
		})
	}
	return samples
}

// formatDays renders a fractional day count rounded to one decimal —
// "10.8", or "11" when the decimal is zero. A quantile interpolation
// can produce long float tails; a management report showing
// "10.799999999999999 days" would be noise pretending to be precision.
func formatDays(days float64) string {
	return formrender.FormatFieldValue(math.Round(days*10) / 10)
}

// formatRate renders a [0, 1] fraction as a percentage rounded to one
// decimal — "66.7%", or "75%" when the decimal is zero — same
// "one-decimal, no float noise" reasoning as formatDays.
func formatRate(rate float64) string {
	return formrender.FormatFieldValue(math.Round(rate*1000)/10) + "%"
}

// buildLeadTimeRows shapes forecast output into the supplier lead-time
// table: one row per vendor (alphabetical, stable) plus an all-vendors
// summary row last. A vendor below forecast.MinSamples shows the
// localized insufficient-history text in both quantile cells rather
// than a number — never a fabricated quantile (issue #30's BA note R1).
// Vendor display names come from the same rows the samples came from
// (CompletedPOLeadTimes already joins Party), no second lookup.
//
// vendorNameRedacted (uc-infra#233) blanks the per-vendor Vendor cell to
// NotAvailable when Party.name is hidden from this actor — this table
// reads Party.name through the exact same unredacted ts.reporting raw
// SQL as the vendor-spend table above (an earlier version of this fix
// missed this table entirely; independent review caught it). The
// trailing "All vendors" summary row is NEVER gated — its label is a
// locale string (report.purchasing.leadtime_overall_label), not a real
// Party.name, so there is nothing to redact there.
//
// Sort ORDER is still computed from the real, un-redacted vendor names
// even when a redacted actor sees every Vendor cell blank — same
// documented, deliberately out-of-scope residual channel as the
// vendor-spend table's own ORDER BY (see renderPurchasingReport's
// comment above the vendors loop): fixing this table's cell display is
// this card's job, closing a name-ordering side channel is not.
func (h *Handler) buildLeadTimeRows(stats forecast.Result, rows []data.CompletedPOLeadTime, locale string, vendorNameRedacted bool) []leadTimeRowView {
	if stats.Overall.N == 0 {
		return nil
	}
	insufficient := h.catalog.T(locale, "report.purchasing.leadtime_insufficient")

	nameByVendor := make(map[string]string, len(rows))
	for _, r := range rows {
		nameByVendor[r.VendorID] = r.VendorName
	}

	row := func(label string, available bool, s forecast.LeadTimeStats) leadTimeRowView {
		v := leadTimeRowView{VendorAvailable: available, N: strconv.Itoa(s.N), P50: insufficient, P90: insufficient}
		if available {
			v.Vendor = label
		}
		if s.Sufficient() {
			v.P50, v.P90 = formatDays(s.P50Days), formatDays(s.P90Days)
		}
		return v
	}

	vendorIDs := make([]string, 0, len(stats.ByVendor))
	for id := range stats.ByVendor {
		vendorIDs = append(vendorIDs, id)
	}
	sort.Slice(vendorIDs, func(i, j int) bool {
		ni, nj := nameByVendor[vendorIDs[i]], nameByVendor[vendorIDs[j]]
		if ni != nj {
			return ni < nj
		}
		return vendorIDs[i] < vendorIDs[j]
	})

	out := make([]leadTimeRowView, 0, len(vendorIDs)+1)
	for _, id := range vendorIDs {
		out = append(out, row(nameByVendor[id], !vendorNameRedacted, stats.ByVendor[id]))
	}
	out = append(out, row(h.catalog.T(locale, "report.purchasing.leadtime_overall_label"), true, stats.Overall))
	return out
}

// buildOnTimeRows shapes forecast.OnTimeResult into the on-time-delivery
// table (#11): one row per vendor with at least one resolvable promise
// (alphabetical, stable — same ordering as buildLeadTimeRows) plus an
// all-vendors summary row last. A vendor below forecast.MinSamples shows
// the localized insufficient-history text instead of a rate — never a
// fabricated percentage, same no-fabrication discipline buildLeadTimeRows
// already applies to quantiles. Unlike buildLeadTimeRows, an empty
// result here is the COMMON case (most tenants will have no
// promised_delivery_date set on anything yet), not a sign of no
// purchasing activity at all — the empty state's own copy says so.
//
// Disclosure, not silence (independent review of #11, see
// forecast.OnTimeResult's own doc comment for the full reasoning): this
// table only ever sees orders that DID eventually arrive. A vendor with
// several promised orders that never showed up at all won't have those
// counted against it here — the "Orders Received" column header (not
// bare "Orders") is the honest label for what N actually counts.
// vendorNameRedacted (uc-infra#233): see buildLeadTimeRows' own comment
// — same gate, same table shape, same reasoning, applied to this table's
// Vendor column.
func (h *Handler) buildOnTimeRows(stats forecast.OnTimeResult, rows []data.CompletedPOLeadTime, locale string, vendorNameRedacted bool) []onTimeRowView {
	if stats.Overall.N == 0 {
		return nil
	}
	insufficient := h.catalog.T(locale, "report.purchasing.ontime_insufficient")

	nameByVendor := make(map[string]string, len(rows))
	for _, r := range rows {
		nameByVendor[r.VendorID] = r.VendorName
	}

	row := func(label string, available bool, s forecast.OnTimeStats) onTimeRowView {
		v := onTimeRowView{VendorAvailable: available, N: strconv.Itoa(s.N), Rate: insufficient}
		if available {
			v.Vendor = label
		}
		if s.Sufficient() {
			v.Rate = formatRate(s.Rate())
		}
		return v
	}

	vendorIDs := make([]string, 0, len(stats.ByVendor))
	for id := range stats.ByVendor {
		vendorIDs = append(vendorIDs, id)
	}
	sort.Slice(vendorIDs, func(i, j int) bool {
		ni, nj := nameByVendor[vendorIDs[i]], nameByVendor[vendorIDs[j]]
		if ni != nj {
			return ni < nj
		}
		return vendorIDs[i] < vendorIDs[j]
	})

	out := make([]onTimeRowView, 0, len(vendorIDs)+1)
	for _, id := range vendorIDs {
		out = append(out, row(nameByVendor[id], !vendorNameRedacted, stats.ByVendor[id]))
	}
	out = append(out, row(h.catalog.T(locale, "report.purchasing.leadtime_overall_label"), true, stats.Overall))
	return out
}

// buildQualityRows shapes forecast.QualityResult into the quality table
// (uc-infra#82): one row per vendor with at least one GoodsReceiptLine
// carrying quality data (alphabetical, stable — same ordering as
// buildOnTimeRows/buildLeadTimeRows) plus an all-vendors summary row
// last. A vendor below forecast.MinSamples shows the localized
// insufficient-history text instead of a rate — never a fabricated
// percentage, same no-fabrication discipline every other section here
// already applies. An empty result is the COMMON case (most tenants
// will have no qty_accepted/qty_rejected set on anything yet), not a
// sign of no purchasing activity at all — the empty state's own copy
// says so, same reasoning buildOnTimeRows' doc comment gives for its own
// empty case.
// vendorNameRedacted (uc-infra#233): see buildLeadTimeRows' own comment
// — same gate, same table shape, same reasoning, applied to this table's
// Vendor column.
func (h *Handler) buildQualityRows(stats forecast.QualityResult, rows []data.GoodsReceiptLineQuality, locale string, vendorNameRedacted bool) []qualityRowView {
	if stats.Overall.N == 0 {
		return nil
	}
	insufficient := h.catalog.T(locale, "report.purchasing.quality_insufficient")

	nameByVendor := make(map[string]string, len(rows))
	for _, r := range rows {
		nameByVendor[r.VendorID] = r.VendorName
	}

	row := func(label string, available bool, s forecast.QualityStats) qualityRowView {
		v := qualityRowView{VendorAvailable: available, N: strconv.Itoa(s.N), Rate: insufficient}
		if available {
			v.Vendor = label
		}
		if s.Sufficient() {
			v.Rate = formatRate(s.Rate())
		}
		return v
	}

	vendorIDs := make([]string, 0, len(stats.ByVendor))
	for id := range stats.ByVendor {
		vendorIDs = append(vendorIDs, id)
	}
	sort.Slice(vendorIDs, func(i, j int) bool {
		ni, nj := nameByVendor[vendorIDs[i]], nameByVendor[vendorIDs[j]]
		if ni != nj {
			return ni < nj
		}
		return vendorIDs[i] < vendorIDs[j]
	})

	out := make([]qualityRowView, 0, len(vendorIDs)+1)
	for _, id := range vendorIDs {
		out = append(out, row(nameByVendor[id], !vendorNameRedacted, stats.ByVendor[id]))
	}
	out = append(out, row(h.catalog.T(locale, "report.purchasing.leadtime_overall_label"), true, stats.Overall))
	return out
}

// buildReorderSignals evaluates every ReorderRule against the current
// inventory position and returns a row for each rule that fires —
// #30's deterministic signal math, exactly as hand-reviewed in the
// issue's design comment (it recommends spending money):
//
//	position = qty_on_hand + on-order (open POs' undelivered qty)
//	fires per forecast.Fires: position <= reorder_point + safety_stock
//	(missing safety_stock = 0)
//
// Position-based on purpose (BA acceptance #4): an item with a huge
// open PO does NOT fire just because on-hand is low — the goods are
// already coming.
//
// The expected-days context picks the lead-time stats of the vendor of
// the item's most recent PO (an Item has no direct vendor link); when
// that vendor is unknown or has insufficient history the overall stats
// stand in — rendered through a DISTINCT "(all suppliers)" string, so a
// buyer never mistakes a fleet-wide quantile for this vendor's own
// track record (independent review of #30) — and when even the overall
// stats are insufficient the row says so explicitly instead of showing
// a number.
//
// ReorderRule/Item records are read through the guarded engine
// (ts.crud — the same RBAC path every CRUD page uses), not raw SQL:
// unlike the aggregates, these are plain per-record reads the generic
// engine already serves. On-hand quantities come from
// ReportingRepo.OnHandQtyByItem like every other aggregate on this page
// — the report needs per-item sums, not every InventoryItem record. A
// tenant without the Purchasing module published has no ReorderRule
// Definition to read — data.ErrNotFound here just means "no rules",
// not an error page. The Item lookup below degrades the same way and
// for the same reason: a ReorderRule with no readable Item Definition
// has nothing this section can meaningfully render either (uc-infra#157
// — independent review found this lookup didn't mirror the ReorderRule
// one above it, so an inconsistent-publish tenant state — real
// ReorderRule records but no published Item Definition — 500'd the
// whole report instead of degrading the reorder section like every
// other missing-Definition case here).
//
// authz.ErrDenied from either call is a DIFFERENT, already-unreachable
// case: renderPurchasingReport's own requireReportRead gates the whole
// page on read access to every type buildReorderSignals touches
// (purchasingReportEntityTypes includes both ReorderRule and Item), so
// a viewer denied read on either one never reaches this function at all
// — the page 403s first. Not handled specially here; any ErrDenied that
// somehow did arrive would still correctly hard-fail below, same as any
// other unexpected error, rather than silently showing a wrong "no
// signals" state to someone who might otherwise have seen real ones.
func (h *Handler) buildReorderSignals(ctx context.Context, ts tenantScope, stats forecast.Result, locale string) ([]reorderRowView, error) {
	reorderDef, err := ts.entityDef(ctx, "ReorderRule")
	if errors.Is(err, data.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rules, err := ts.crud.List(ctx, reorderDef)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}

	itemDef, err := ts.entityDef(ctx, "Item")
	if errors.Is(err, data.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	onHandByItem, err := ts.reporting.OnHandQtyByItem(ctx)
	if err != nil {
		return nil, err
	}
	onOrderByItem, err := ts.reporting.OnOrderQtyByItem(ctx)
	if err != nil {
		return nil, err
	}
	vendorByItem, err := ts.reporting.LatestPOVendorByItem(ctx)
	if err != nil {
		return nil, err
	}

	expectDaysTmpl := h.catalog.T(locale, "report.purchasing.reorder_expect_days")
	expectDaysOverallTmpl := h.catalog.T(locale, "report.purchasing.reorder_expect_days_overall")
	insufficient := h.catalog.T(locale, "report.purchasing.reorder_insufficient")

	var out []reorderRowView
	for _, rule := range rules {
		itemID, _ := rule.Data["item_id"].(string)
		if itemID == "" {
			continue
		}
		reorderPoint, ok := rule.Data["reorder_point"].(float64)
		if !ok {
			continue // unusable rule (bad import) — no threshold to compare against
		}
		safetyStock, _ := rule.Data["safety_stock"].(float64) // missing = 0, per the design

		position := onHandByItem[itemID] + onOrderByItem[itemID]
		if !forecast.Fires(position, reorderPoint, safetyStock) {
			continue // healthy — no signal
		}

		item, err := ts.crud.Get(ctx, itemDef, itemID)
		if errors.Is(err, data.ErrNotFound) {
			continue // dangling item_id — nothing meaningful to show
		}
		if err != nil {
			return nil, err
		}
		itemName, _ := item.Data["name"].(string)
		if itemName == "" {
			itemName = itemID
		}

		ruleStats, contextTmpl := stats.Overall, expectDaysOverallTmpl
		if vendorStats, ok := stats.ByVendor[vendorByItem[itemID]]; ok && vendorStats.Sufficient() {
			ruleStats, contextTmpl = vendorStats, expectDaysTmpl
		}
		expected := insufficient
		if ruleStats.Sufficient() {
			days := ruleStats.P90Days
			if conf, _ := rule.Data["target_lead_time_confidence"].(string); conf == "p50" {
				days = ruleStats.P50Days
			}
			expected = strings.ReplaceAll(contextTmpl, "{days}", formatDays(days))
		}

		out = append(out, reorderRowView{
			Item:         itemName,
			Href:         "/forms/Item/" + itemID,
			OnHand:       formrender.FormatFieldValue(onHandByItem[itemID]),
			OnOrder:      formrender.FormatFieldValue(onOrderByItem[itemID]),
			Position:     formrender.FormatFieldValue(position),
			ReorderPoint: formrender.FormatFieldValue(reorderPoint),
			Expected:     expected,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Item != out[j].Item {
			return out[i].Item < out[j].Item
		}
		return out[i].Href < out[j].Href
	})
	return out, nil
}

type purchasingReportView struct {
	Title string

	// NotAvailable (uc-infra#233) is the placeholder every
	// FieldPermission-hideable cell on this report renders instead of a
	// real value when the acting role's FieldPermission hides the field
	// it comes from — same "resolved once, reused by every guard on the
	// page" shape as project_budget_report.go's own NotAvailable.
	NotAvailable string

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

	LeadTimeHeading    string
	LeadTimeEmpty      string
	LeadTimeVendorCol  string
	LeadTimeSamplesCol string
	LeadTimeP50Col     string
	LeadTimeP90Col     string
	LeadTimeRows       []leadTimeRowView

	OnTimeHeading    string
	OnTimeEmpty      string
	OnTimeVendorCol  string
	OnTimeSamplesCol string
	OnTimeRateCol    string
	OnTimeRows       []onTimeRowView

	QualityHeading    string
	QualityEmpty      string
	QualityVendorCol  string
	QualitySamplesCol string
	QualityRateCol    string
	QualityRows       []qualityRowView

	ReorderHeading     string
	ReorderEmpty       string
	ReorderItemCol     string
	ReorderOnHandCol   string
	ReorderOnOrderCol  string
	ReorderPositionCol string
	ReorderPointCol    string
	ReorderExpectedCol string
	ReorderRows        []reorderRowView
}

type leadTimeRowView struct {
	// VendorAvailable (uc-infra#233) gates Party.name — false only for a
	// per-vendor row when the actor's FieldPermission hides it; the
	// trailing "All vendors" summary row is never gated (its Vendor value
	// is a locale label, not a real Party.name).
	VendorAvailable bool
	Vendor          string
	N               string
	P50             string
	P90             string
}

type onTimeRowView struct {
	// VendorAvailable (uc-infra#233): see leadTimeRowView's own comment.
	VendorAvailable bool
	Vendor          string
	N               string
	Rate            string
}

type qualityRowView struct {
	// VendorAvailable (uc-infra#233): see leadTimeRowView's own comment.
	VendorAvailable bool
	Vendor          string
	N               string
	Rate            string
}

type reorderRowView struct {
	Item         string
	Href         string
	OnHand       string
	OnOrder      string
	Position     string
	ReorderPoint string
	Expected     string
}

type statusCardView struct {
	Label string
	Count string
	// ValueAvailable (uc-infra#233) distinguishes a real
	// PurchaseOrder.total sum from "not available" (total hidden from
	// this actor) — collapsing the two would render a real aggregate to
	// an actor whose FieldPermission hides the per-record field it's
	// summed from. Count is never gated the same way — it comes from
	// count(*), unrelated to total.
	ValueAvailable bool
	Value          string
}

type vendorRowView struct {
	// NameAvailable/SpendAvailable (uc-infra#233) gate Party.name and
	// PurchaseOrder.total independently — a role hiding only one of the
	// two must still see the other's real value on this same row.
	NameAvailable  bool
	Name           string
	Orders         string
	SpendAvailable bool
	Spend          string
}

type stockoutRowView struct {
	// SKUAvailable/NameAvailable (uc-infra#233) gate Item.sku and
	// Item.name independently, same reasoning as vendorRowView's own
	// NameAvailable/SpendAvailable pair above. Row membership itself is
	// unaffected by either — see the handler's own comment on why this
	// table's inclusion criterion (qty_available_to_promise) has no
	// relationship to SKU/name.
	SKUAvailable  bool
	SKU           string
	NameAvailable bool
	Name          string
	OnHand        string
	ATP           string
	Href          string
}

var purchasingReportTmpl = template.Must(template.New("purchasingReport").Parse(`
<h1>{{.Title}}</h1>

<h2>{{.StatusHeading}}</h2>
<div class="uc-report-cards">
{{range .StatusCards}}
<div class="uc-report-card">
  <div class="uc-report-card-label">{{.Label}}</div>
  <div class="uc-report-card-value">{{.Count}}</div>
  <div class="uc-report-card-sub">{{if .ValueAvailable}}{{.Value}}{{else}}{{$.NotAvailable}}{{end}}</div>
</div>
{{end}}
</div>

<h2>{{.VendorHeading}}</h2>
{{if .Vendors}}
<table class="uc-table">
<thead><tr><th>{{.VendorNameCol}}</th><th>{{.VendorOrdersCol}}</th><th>{{.VendorSpendCol}}</th></tr></thead>
<tbody>
{{range .Vendors}}
<tr><td>{{if .NameAvailable}}{{.Name}}{{else}}{{$.NotAvailable}}{{end}}</td><td>{{.Orders}}</td><td>{{if .SpendAvailable}}{{.Spend}}{{else}}{{$.NotAvailable}}{{end}}</td></tr>
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
<tr><td><a href="{{.Href}}">{{if .SKUAvailable}}{{.SKU}}{{else}}{{$.NotAvailable}}{{end}}</a></td><td>{{if .NameAvailable}}{{.Name}}{{else}}{{$.NotAvailable}}{{end}}</td><td>{{.OnHand}}</td><td>{{.ATP}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.StockoutEmpty}}</p>
{{end}}

<h2>{{.LeadTimeHeading}}</h2>
{{if .LeadTimeRows}}
<table class="uc-table">
<thead><tr><th>{{.LeadTimeVendorCol}}</th><th>{{.LeadTimeSamplesCol}}</th><th>{{.LeadTimeP50Col}}</th><th>{{.LeadTimeP90Col}}</th></tr></thead>
<tbody>
{{range .LeadTimeRows}}
<tr><td>{{if .VendorAvailable}}{{.Vendor}}{{else}}{{$.NotAvailable}}{{end}}</td><td>{{.N}}</td><td>{{.P50}}</td><td>{{.P90}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.LeadTimeEmpty}}</p>
{{end}}

<h2>{{.OnTimeHeading}}</h2>
{{if .OnTimeRows}}
<table class="uc-table">
<thead><tr><th>{{.OnTimeVendorCol}}</th><th>{{.OnTimeSamplesCol}}</th><th>{{.OnTimeRateCol}}</th></tr></thead>
<tbody>
{{range .OnTimeRows}}
<tr><td>{{if .VendorAvailable}}{{.Vendor}}{{else}}{{$.NotAvailable}}{{end}}</td><td>{{.N}}</td><td>{{.Rate}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.OnTimeEmpty}}</p>
{{end}}

<h2>{{.QualityHeading}}</h2>
{{if .QualityRows}}
<table class="uc-table">
<thead><tr><th>{{.QualityVendorCol}}</th><th>{{.QualitySamplesCol}}</th><th>{{.QualityRateCol}}</th></tr></thead>
<tbody>
{{range .QualityRows}}
<tr><td>{{if .VendorAvailable}}{{.Vendor}}{{else}}{{$.NotAvailable}}{{end}}</td><td>{{.N}}</td><td>{{.Rate}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.QualityEmpty}}</p>
{{end}}

<h2>{{.ReorderHeading}}</h2>
{{if .ReorderRows}}
<table class="uc-table">
<thead><tr><th>{{.ReorderItemCol}}</th><th>{{.ReorderOnHandCol}}</th><th>{{.ReorderOnOrderCol}}</th><th>{{.ReorderPositionCol}}</th><th>{{.ReorderPointCol}}</th><th>{{.ReorderExpectedCol}}</th></tr></thead>
<tbody>
{{range .ReorderRows}}
<tr><td><a href="{{.Href}}">{{.Item}}</a></td><td>{{.OnHand}}</td><td>{{.OnOrder}}</td><td>{{.Position}}</td><td>{{.ReorderPoint}}</td><td>{{.Expected}}</td></tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="uc-empty">{{.ReorderEmpty}}</p>
{{end}}
`))
