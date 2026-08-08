package data

import (
	"context"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/money"
)

// uuidPattern guards every join below that casts a FieldReference's
// stored text value to uuid before comparing it against another
// record's id column. entity.ValidateRecord (internal/kernel/entity)
// only checks a reference field is *a string* — it never checks the
// string is a well-formed UUID — so a value that reached this table
// through, say, a CSV import with a bad mapping could be anything.
// Postgres's ::uuid cast raises a hard error on a malformed value,
// which (without this guard) would abort the whole aggregate query for
// every other, perfectly valid row too. Filtering to values that at
// least look like a UUID before casting turns "one bad row breaks the
// whole report" into "one bad row is silently excluded from it."
const uuidPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

// numericPattern is uuidPattern's own reasoning applied to a
// FieldNumber instead of a FieldReference (uc-infra#54's netting
// subquery is this file's first cast of a JSONB text value to
// `::numeric` — every existing numeric cast here, e.g. `qty`/`unit_price`,
// predates this guard and is a separately-tracked pre-existing gap, not
// something this pattern retrofits everywhere at once): a malformed or
// non-numeric value would abort the aggregate for every other valid row
// with it. Accepts an optional leading `-` (a negative FieldNumber is
// still a well-formed number, even though #80's missing Min concept
// means one should not have reached storage) and an optional decimal
// part.
const numericPattern = `^-?[0-9]+(\.[0-9]+)?$`

// ReportingRepo holds the read-only aggregate queries behind the
// management reporting workbench (internal/api/reporting.go). Unlike
// every other repo in this package, these queries are inherently
// specific to the Purchasing module's entity shapes (PurchaseOrder,
// POLine, Item, InventoryItem, Party) — CLAUDE.md's kernel-boundary rule
// only constrains internal/kernel/entity|form|workflow (the generic
// engines) against this kind of entity-specific knowledge; a reporting
// repo, like internal/kernel/purchasing itself, is exactly where it's
// supposed to live.
type ReportingRepo struct {
	db querier
}

func NewReportingRepo(db querier) *ReportingRepo {
	return &ReportingRepo{db: db}
}

// PurchaseOrderStatusCount is one row of the status breakdown — order
// count and total (summed) value for every PurchaseOrder in that status.
//
// Value is money.Money (minor units), not float64 (uc-infra#136,
// following up uc-infra#68's own RFQComparisonLine.QuotesByVendor
// precedent): PurchaseOrder.total is FieldMoney now, and this is a raw
// SQL sum of that stored value, so keeping it in minor units all the way
// to the caller is what makes internal/api/reporting.go's own display
// conversion (money.Money.String()/.Major()) the one and only place a
// float touches this amount.
type PurchaseOrderStatusCount struct {
	Status string
	Count  int
	Value  money.Money
}

// PurchaseOrderStatusBreakdown groups every PurchaseOrder record by
// status. Callers that want a fixed display order (draft, submitted,
// approved, received, cancelled) should reorder the result themselves —
// this returns whatever combination of statuses actually has at least
// one order, in no particular order.
//
// Joins to Status for its code rather than reading a plain enum value
// off PurchaseOrder directly — status_id/StatusTypeCode
// (purchasing.PurchaseOrder's own doc comment) replaced the old
// FieldEnum "status" field, so the human-readable code this report (and
// internal/api/reporting.go's field.PurchaseOrder.status.<code> i18n
// lookup) has always kept as PurchaseOrderStatusCount.Status now lives
// on the referenced Status record, not PurchaseOrder's own JSONB blob.
// Same uuidPattern guard as TopVendorsBySpend/StockoutRiskItems below:
// a malformed status_id (bad CSV import, say) is excluded from the
// breakdown rather than aborting the whole aggregate with a cast error.
//
// The sum itself casts to ::bigint, not ::numeric (uc-infra#136:
// PurchaseOrder.total is FieldMoney now, a whole number of minor
// units), guarded by moneyMinorUnitsPattern (rfq_reporting.go's own
// const, same package) inside a CASE rather than the WHERE clause: a
// malformed/not-yet-backfilled total must not exclude that PurchaseOrder
// from count(*) too — it still really exists and is still really in
// this status — only from the SUM, where it contributes 0 instead of
// aborting the whole aggregate with a Postgres ::bigint cast error on a
// legacy fractional value (same "one bad row's problem, not the whole
// report's" resolution rfq_reporting.go's own guard doc comment
// describes, applied to a SUM instead of a per-row grid cell).
func (r *ReportingRepo) PurchaseOrderStatusBreakdown(ctx context.Context) ([]PurchaseOrderStatusCount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT st.data->>'code' AS status, count(*),
		        coalesce(sum(CASE WHEN po.data->>'total' ~ $2 THEN (po.data->>'total')::bigint ELSE 0 END), 0)
		 FROM records po
		 JOIN records st
		   ON st.entity_type = 'Status'
		  AND st.deleted_at IS NULL
		  AND st.id = (po.data->>'status_id')::uuid
		 WHERE po.entity_type = 'PurchaseOrder'
		   AND po.deleted_at IS NULL
		   AND po.data->>'status_id' ~ $1
		 GROUP BY st.data->>'code'`,
		uuidPattern, moneyMinorUnitsPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("purchase order status breakdown: %w", err)
	}
	defer rows.Close()

	var out []PurchaseOrderStatusCount
	for rows.Next() {
		var row PurchaseOrderStatusCount
		var valueMinor int64
		if err := rows.Scan(&row.Status, &row.Count, &valueMinor); err != nil {
			return nil, fmt.Errorf("scan status breakdown row: %w", err)
		}
		row.Value = money.Money(valueMinor)
		out = append(out, row)
	}
	return out, rows.Err()
}

// VendorSpend is one vendor's aggregate spend across every PurchaseOrder
// pointing at it (regardless of status — a submitted-but-not-yet-
// received order is still committed spend for a management report,
// unlike, say, revenue recognition, which would care about status).
//
// Total is money.Money (minor units), not float64 — see
// PurchaseOrderStatusCount.Value's own doc comment (uc-infra#136).
type VendorSpend struct {
	VendorID   string
	VendorName string
	OrderCount int
	Total      money.Money
}

// TopVendorsBySpend returns vendors ranked by total PurchaseOrder value,
// highest first, capped at limit. A vendor_id that doesn't resolve to a
// live Party row (deleted, or simply malformed — see uuidPattern's doc
// comment) is excluded rather than aborting the whole query.
//
// Same CASE-guarded ::bigint sum as PurchaseOrderStatusBreakdown above,
// and the same reasoning for why the guard lives in the SUM's CASE
// rather than the WHERE clause: a malformed/not-yet-backfilled total
// shouldn't drop that PurchaseOrder out of this vendor's order_count
// too, only out of the spend sum.
func (r *ReportingRepo) TopVendorsBySpend(ctx context.Context, limit int) ([]VendorSpend, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.id, coalesce(v.data->>'name', v.id::text), count(po.id),
		        coalesce(sum(CASE WHEN po.data->>'total' ~ $3 THEN (po.data->>'total')::bigint ELSE 0 END), 0) AS spend
		 FROM records po
		 JOIN records v
		   ON v.entity_type = 'Party'
		  AND v.deleted_at IS NULL
		  AND v.id = (po.data->>'vendor_id')::uuid
		 WHERE po.entity_type = 'PurchaseOrder'
		   AND po.deleted_at IS NULL
		   AND po.data->>'vendor_id' ~ $2
		 GROUP BY v.id, v.data->>'name'
		 ORDER BY spend DESC, v.id
		 LIMIT $1`,
		limit, uuidPattern, moneyMinorUnitsPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("top vendors by spend: %w", err)
	}
	defer rows.Close()

	var out []VendorSpend
	for rows.Next() {
		var row VendorSpend
		var totalMinor int64
		if err := rows.Scan(&row.VendorID, &row.VendorName, &row.OrderCount, &totalMinor); err != nil {
			return nil, fmt.Errorf("scan vendor spend row: %w", err)
		}
		row.Total = money.Money(totalMinor)
		out = append(out, row)
	}
	return out, rows.Err()
}

// StockSummary is the roll-up over every InventoryItem row.
//
// **Every figure here is per ITEM, not per InventoryItem row.** Since
// #12/ADR-0015 re-keyed InventoryItem by (item, facility), one item can
// hold several rows, and this report is an organization-level summary:
// re-keying how stock is stored must not silently change what a
// dashboard number means. So ItemCount counts distinct items rather
// than rows, and StockoutCount counts items whose availability summed
// ACROSS facilities is exhausted — an item sitting empty in one depot
// while another is full is not an organization-level stockout.
//
// Per-facility stock reporting is a genuinely useful and genuinely
// different report, with a facility column and a filter. It is
// deliberately not this one.
//
// Two edge cases the grouping made easy to get wrong, both found by
// independent review and both pinned by tests:
//
//   - **Rows with no item_id are excluded**, matching OnHandQtyByItem's
//     long-standing guard. Without it, GROUP BY collapses every such
//     row into a single NULL group that count(*) then reports as an
//     "item" — a phantom that joins to no Item record, so the headline
//     count and the StockoutRiskItems table below it would disagree
//     about what exists.
//   - **A missing qty_available_to_promise is not a stockout.** sum()
//     over no non-null values is NULL, and the FILTER checks IS NOT
//     NULL explicitly, preserving the old per-row behaviour where
//     `NULL <= 0` was simply not counted. Wrapping the sum in coalesce
//     would turn "unknown" into "zero available", which reads as an
//     alert. Barely reachable — the field is Required with default 0 —
//     but this report's whole design rule is that re-keying storage
//     must not silently change what a number means.
type StockSummary struct {
	ItemCount     int
	TotalOnHand   float64
	TotalATP      float64
	StockoutCount int // distinct items whose ATP, summed across facilities, is <= 0
}

func (r *ReportingRepo) StockSummary(ctx context.Context) (StockSummary, error) {
	var s StockSummary
	err := r.db.QueryRowContext(ctx,
		`WITH per_item AS (
		   SELECT data->>'item_id' AS item_id,
		          coalesce(sum((data->>'qty_on_hand')::numeric), 0) AS on_hand,
		          sum((data->>'qty_available_to_promise')::numeric) AS atp
		   FROM records
		   WHERE entity_type = 'InventoryItem'
		     AND deleted_at IS NULL
		     AND coalesce(data->>'item_id', '') <> ''
		   GROUP BY data->>'item_id'
		 )
		 SELECT
		   count(*),
		   coalesce(sum(on_hand), 0),
		   coalesce(sum(atp), 0),
		   count(*) FILTER (WHERE atp IS NOT NULL AND atp <= 0)
		 FROM per_item`,
	).Scan(&s.ItemCount, &s.TotalOnHand, &s.TotalATP, &s.StockoutCount)
	if err != nil {
		return StockSummary{}, fmt.Errorf("stock summary: %w", err)
	}
	return s, nil
}

// StockoutRiskItem is one Item with no quantity available to promise —
// nothing left to sell/allocate without a new order arriving, the
// concrete "stock intelligence" signal this workbench surfaces (per
// QUEUE.md's design-partner synthetic-data demo entry). Deliberately not a
// reorder-point/forecasting alert (BACKLOG.md R10 — a whole prediction
// service, explicitly out of scope for this kernel today) — just "this
// item is at or below zero available right now," computed directly from
// data that already exists.
//
// **One row per Item, summed across facilities** (#12/ADR-0015). Since
// InventoryItem became keyed by (item, facility), a per-row query would
// list an item as at risk because one depot happens to be empty while
// another is full — and list it more than once, in a table that has no
// facility column to explain why. Aggregating first keeps this report
// meaning exactly what it meant when there was one row per item.
type StockoutRiskItem struct {
	ItemID    string
	SKU       string
	Name      string
	QtyOnHand float64
	QtyATP    float64
}

// CompletedPOLeadTime is one completed PurchaseOrder's observed lead
// time, plus whichever of #29's intermediate stage timestamps it
// recorded (empty string where a stage was never filled in — an
// in-flight prefix is realistic censored data, see purchasing.
// PurchaseOrder's own doc comment). All dates are the ISO-8601 strings
// exactly as stored in the record JSONB — parsing (and tolerating noisy
// values, per issue #29's review note on chronology being data hygiene,
// not ledger-grade) is the caller's job, keeping this repo a plain
// fetch.
type CompletedPOLeadTime struct {
	POID string
	// VendorID/VendorName are empty when the PO's vendor_id doesn't
	// resolve to a live Party (malformed, dangling, or absent) — the
	// row still counts toward the OVERALL lead-time distribution, it
	// just can't be attributed to a vendor bucket
	// (forecast.Compute's own documented handling of "").
	VendorID          string
	VendorName        string
	OrderDate         string
	SourcedAt         string
	ProductionStartAt string
	ProductionReadyAt string
	ShippedAt         string
	CustomsClearedAt  string
	// ReceivedDate is COALESCE(received_at, MIN(GoodsReceipt.
	// received_date)) — never empty; a PO with neither is not
	// "completed" and isn't returned at all. This is the FIRST evidence
	// of arrival — correct for #30's lead-time quantiles (how long until
	// something shows up), but deliberately NOT what #11's on-time
	// determination uses (see LastReceivedDate) — a single early partial
	// shipment would otherwise mark a PO "on time" while the bulk of the
	// order was still months late (independent review of #11, 2026-07-31:
	// reproduced with a 1-unit receipt one day early followed by a
	// 99-unit receipt 82 days late).
	ReceivedDate string
	// PromisedDeliveryDate (#11) is the PO's own promised_delivery_date
	// field, empty when never set — most rows, since it's optional and
	// every PO written before #11 has none. The caller (forecast.
	// OnTimeSample) is responsible for treating "" as "no on-time
	// sample here", same division of responsibility as every other
	// date on this struct.
	PromisedDeliveryDate string
	// LastReceivedDate is COALESCE(received_at, MAX(GoodsReceipt.
	// received_date)) — the LAST evidence of arrival, i.e. when the PO
	// was actually fully satisfied. #11's on-time determination
	// (forecast.OnTimeSample.ReceivedDate) uses THIS, not ReceivedDate:
	// "on time" has to mean the whole order arrived by the promise, not
	// that the first box did. Equal to ReceivedDate whenever there's at
	// most one GoodsReceipt (the common case) or the PO's own received_at
	// stage is set directly (which both fields fall back from equally) —
	// they only diverge across multiple partial GoodsReceipt rows.
	LastReceivedDate string
}

// CompletedPOLeadTimes returns every PurchaseOrder with a known receipt
// time: its own received_at stage if recorded, else the earliest
// GoodsReceipt.received_date posted against it. This query-time
// derivation is the deliberate resolution of the decision inherited
// from #29 (see issue #30's first comment): the forecast is the only
// consumer of "when did this PO actually arrive", so deriving it here
// makes a stored auto-stamp on the PO — and the cross-record crud.Hook
// machinery it would have required — redundant.
//
// MIN over the ISO-8601 date strings is a correct "earliest" because
// the format sorts lexicographically. The vendor join is a LEFT JOIN on
// purpose: a completed PO whose vendor_id is malformed, dangling, or
// absent still holds real lead-time evidence for the OVERALL
// distribution, so it's returned with an empty VendorID rather than
// excluded (independent review of #30 — dropping it silently biased the
// overall quantiles). The join compares id::text against the stored
// value instead of ::uuid-casting the stored value, so no uuidPattern
// guard is even needed on this hop — a malformed vendor_id simply
// matches nothing; the GoodsReceipt back-reference inside the lateral
// keeps the usual cast guard. Ordered by order date then id for a
// deterministic result.
func (r *ReportingRepo) CompletedPOLeadTimes(ctx context.Context) ([]CompletedPOLeadTime, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT po.id, coalesce(v.id::text, ''), coalesce(v.data->>'name', v.id::text, ''),
		        coalesce(po.data->>'order_date', ''),
		        coalesce(po.data->>'sourced_at', ''),
		        coalesce(po.data->>'production_start_at', ''),
		        coalesce(po.data->>'production_ready_at', ''),
		        coalesce(po.data->>'shipped_at', ''),
		        coalesce(po.data->>'customs_cleared_at', ''),
		        coalesce(nullif(po.data->>'received_at', ''), gr.first_received),
		        coalesce(po.data->>'promised_delivery_date', ''),
		        coalesce(nullif(po.data->>'received_at', ''), gr.last_received)
		 FROM records po
		 LEFT JOIN records v
		   ON v.entity_type = 'Party'
		  AND v.deleted_at IS NULL
		  AND v.id::text = po.data->>'vendor_id'
		 LEFT JOIN LATERAL (
		   SELECT min(g.data->>'received_date') AS first_received,
		          max(g.data->>'received_date') AS last_received
		   FROM records g
		   WHERE g.entity_type = 'GoodsReceipt'
		     AND g.deleted_at IS NULL
		     AND g.data->>'purchase_order_id' ~ $1
		     AND (g.data->>'purchase_order_id')::uuid = po.id
		     AND coalesce(g.data->>'received_date', '') <> ''
		 ) gr ON true
		 WHERE po.entity_type = 'PurchaseOrder'
		   AND po.deleted_at IS NULL
		   AND coalesce(po.data->>'order_date', '') <> ''
		   AND coalesce(nullif(po.data->>'received_at', ''), gr.first_received) IS NOT NULL
		 ORDER BY po.data->>'order_date', po.id`,
		uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("completed po lead times: %w", err)
	}
	defer rows.Close()

	var out []CompletedPOLeadTime
	for rows.Next() {
		var row CompletedPOLeadTime
		if err := rows.Scan(&row.POID, &row.VendorID, &row.VendorName, &row.OrderDate,
			&row.SourcedAt, &row.ProductionStartAt, &row.ProductionReadyAt,
			&row.ShippedAt, &row.CustomsClearedAt, &row.ReceivedDate,
			&row.PromisedDeliveryDate, &row.LastReceivedDate); err != nil {
			return nil, fmt.Errorf("scan completed po lead time row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OnOrderQtyByItem sums POLine.qty per item over PurchaseOrders whose
// status code is submitted or approved, NET of whatever has already
// been received against each line — the "on order" half of an
// inventory position (issue #30's BA note R3). Received and cancelled
// orders are excluded wholesale (their goods either already count in
// qty_on_hand or are never coming), drafts too (nothing is committed
// yet).
//
// **Per-line netting against GoodsReceiptLine.qty_received** (uc-infra#54,
// shipped in the same commit as the GoodsReceipt→InventoryItem wiring
// it exists to stay correct against — see purchasing.GoodsReceipt's own
// doc comment on why the two cannot ship separately): the moment
// receiving credits qty_on_hand, a still-open PO's remaining line qty is
// its ordered qty minus whatever that line has already received, not
// the full ordered qty — otherwise a partially-received but still-open
// PO double-counts the delivered portion on both sides of
// on_hand + on_order. Netted PER LINE and floored at zero per line
// (GREATEST) before summing per item, not netted at the item-total
// level: flooring after summing would let an over-received line mask a
// genuinely under-received one for the same item. Returned as a map
// (item id -> qty) because every caller is a lookup, not a table. Items
// with no open PO simply have no entry.
//
// The netting subquery guards THREE things independently, each a real,
// separately-reachable bad-data case, not belt-and-suspenders for the
// same one:
//   - `numericPattern` on `qty_received` before the cast — same
//     uuidPattern reasoning (this file's own doc comment above), applied
//     to a numeric field for the first time: a CSV import or a
//     hand-written Go caller can write anything into a JSONB text value,
//     and an unguarded `::numeric` cast here would 500 the whole report
//     over one bad row, same as an unguarded `::uuid` cast would.
//   - a received qty is floored at zero (per line, via the inner
//     `greatest(sum(...), 0)`) before it ever nets against the ordered
//     qty — a negative `qty_received` (no Min concept exists yet, #80)
//     must never REDUCE what counts as received, which would inflate
//     remaining/on-order instead of the (still real, still worth fixing
//     via #80) harm being confined to the receipt-side numbers.
//   - the joined GoodsReceipt itself (not just the GoodsReceiptLine) must
//     be live: `crud.Engine.Delete` does not cascade to composition
//     children (internal/kernel/crud/crud.go), so a soft-deleted
//     GoodsReceipt would otherwise leave its lines still netting down
//     on-order and still crediting InventoryItem forever.
//
// Same uuidPattern guard as every other join in this file, now on five
// reference hops (po_line_id and goods_receipt_id in the netting
// subquery, in addition to the three PurchaseOrder/Status hops above).
func (r *ReportingRepo) OnOrderQtyByItem(ctx context.Context) (map[string]float64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT sub.item_id, coalesce(sum(greatest(sub.remaining_qty, 0)), 0)
		 FROM (
			 SELECT l.data->>'item_id' AS item_id,
			        (l.data->>'qty')::numeric - coalesce(gr.received_qty, 0) AS remaining_qty
			 FROM records l
			 JOIN records po
			   ON po.entity_type = 'PurchaseOrder'
			  AND po.deleted_at IS NULL
			  AND po.id = (l.data->>'purchase_order_id')::uuid
			 JOIN records st
			   ON st.entity_type = 'Status'
			  AND st.deleted_at IS NULL
			  AND st.id = (po.data->>'status_id')::uuid
			 LEFT JOIN (
				 SELECT (grl.data->>'po_line_id')::uuid AS po_line_id,
				        greatest(sum((grl.data->>'qty_received')::numeric), 0) AS received_qty
				 FROM records grl
				 JOIN records gr
				   ON gr.entity_type = 'GoodsReceipt'
				  AND gr.deleted_at IS NULL
				  AND gr.id = (grl.data->>'goods_receipt_id')::uuid
				 WHERE grl.entity_type = 'GoodsReceiptLine'
				   AND grl.deleted_at IS NULL
				   AND grl.data->>'po_line_id' ~ $1
				   AND grl.data->>'goods_receipt_id' ~ $1
				   AND grl.data->>'qty_received' ~ $2
				 GROUP BY (grl.data->>'po_line_id')::uuid
			 ) gr ON gr.po_line_id = l.id
			 WHERE l.entity_type = 'POLine'
			   AND l.deleted_at IS NULL
			   AND l.data->>'purchase_order_id' ~ $1
			   AND l.data->>'item_id' ~ $1
			   AND po.data->>'status_id' ~ $1
			   AND st.data->>'code' IN ('submitted', 'approved')
		 ) sub
		 GROUP BY sub.item_id`,
		uuidPattern, numericPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("on-order qty by item: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var itemID string
		var qty float64
		if err := rows.Scan(&itemID, &qty); err != nil {
			return nil, fmt.Errorf("scan on-order qty row: %w", err)
		}
		out[itemID] = qty
	}
	return out, rows.Err()
}

// OnHandQtyByItem sums InventoryItem.qty_on_hand per item — the other
// half of the inventory position OnOrderQtyByItem's doc comment
// describes. An aggregate here rather than a guarded-engine List over
// every InventoryItem record in the report handler (independent review
// of #30): the report only ever needs the per-item sums, and every
// other number on the page already comes from this repo's aggregates.
// Summing (not first-row-wins) matches StockSummary's own treatment of
// multiple rows per item. Rows with no item_id are skipped — they can
// never match a ReorderRule's item.
func (r *ReportingRepo) OnHandQtyByItem(ctx context.Context) (map[string]float64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT data->>'item_id', coalesce(sum((data->>'qty_on_hand')::numeric), 0)
		 FROM records
		 WHERE entity_type = 'InventoryItem'
		   AND deleted_at IS NULL
		   AND coalesce(data->>'item_id', '') <> ''
		 GROUP BY data->>'item_id'`,
	)
	if err != nil {
		return nil, fmt.Errorf("on-hand qty by item: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var itemID string
		var qty float64
		if err := rows.Scan(&itemID, &qty); err != nil {
			return nil, fmt.Errorf("scan on-hand qty row: %w", err)
		}
		out[itemID] = qty
	}
	return out, rows.Err()
}

// LatestPOVendorByItem maps each item to the vendor of its most recent
// PurchaseOrder (by order_date, any status — a received order is still
// the best evidence of who supplies this item). Items have no direct
// vendor link in the model (issue #30's Architect design), so this
// POLine->PurchaseOrder hop is how a reorder signal picks whose lead
// time to show. Ties on order_date break deterministically by PO id.
func (r *ReportingRepo) LatestPOVendorByItem(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT ON (l.data->>'item_id') l.data->>'item_id', po.data->>'vendor_id'
		 FROM records l
		 JOIN records po
		   ON po.entity_type = 'PurchaseOrder'
		  AND po.deleted_at IS NULL
		  AND po.id = (l.data->>'purchase_order_id')::uuid
		 WHERE l.entity_type = 'POLine'
		   AND l.deleted_at IS NULL
		   AND l.data->>'purchase_order_id' ~ $1
		   AND l.data->>'item_id' ~ $1
		   AND po.data->>'vendor_id' ~ $1
		 ORDER BY l.data->>'item_id', po.data->>'order_date' DESC NULLS LAST, po.id`,
		uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("latest po vendor by item: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var itemID, vendorID string
		if err := rows.Scan(&itemID, &vendorID); err != nil {
			return nil, fmt.Errorf("scan latest po vendor row: %w", err)
		}
		out[itemID] = vendorID
	}
	return out, rows.Err()
}

// StockoutRiskItems returns Items with qty_available_to_promise <= 0,
// most-negative first, capped at limit. Same malformed-reference guard
// as TopVendorsBySpend.
func (r *ReportingRepo) StockoutRiskItems(ctx context.Context, limit int) ([]StockoutRiskItem, error) {
	rows, err := r.db.QueryContext(ctx,
		`WITH per_item AS (
		   SELECT (inv.data->>'item_id')::uuid AS item_id,
		          coalesce(sum((inv.data->>'qty_on_hand')::numeric), 0) AS on_hand,
		          coalesce(sum((inv.data->>'qty_available_to_promise')::numeric), 0) AS atp
		   FROM records inv
		   WHERE inv.entity_type = 'InventoryItem'
		     AND inv.deleted_at IS NULL
		     AND inv.data->>'item_id' ~ $2
		   GROUP BY inv.data->>'item_id'
		 )
		 SELECT i.id, coalesce(i.data->>'sku', ''), coalesce(i.data->>'name', i.id::text),
		        p.on_hand, p.atp
		 FROM per_item p
		 JOIN records i
		   ON i.entity_type = 'Item'
		  AND i.deleted_at IS NULL
		  AND i.id = p.item_id
		 WHERE p.atp <= 0
		 ORDER BY p.atp, i.id
		 LIMIT $1`,
		limit, uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("stockout risk items: %w", err)
	}
	defer rows.Close()

	var out []StockoutRiskItem
	for rows.Next() {
		var row StockoutRiskItem
		if err := rows.Scan(&row.ItemID, &row.SKU, &row.Name, &row.QtyOnHand, &row.QtyATP); err != nil {
			return nil, fmt.Errorf("scan stockout risk row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GoodsReceiptLineQuality is one GoodsReceiptLine's vendor + recorded
// inspection outcome (uc-infra#82). QtyAccepted/QtyRejected are the
// stored qty_accepted/qty_rejected FieldNumber values, passed through as
// text and empty when either is absent — same "coalesce to empty
// string, let the caller decide what absence means" division of
// responsibility CompletedPOLeadTime.PromisedDeliveryDate already uses;
// forecast.QualitySample is the caller that turns "" into HasData=false.
// A line's own quality invariant (accepted+rejected == received when
// both are set) is enforced at write time by
// purchasing.validateGoodsReceiptLineQuality, not re-checked here — this
// is a read path, not a second source of truth for it.
type GoodsReceiptLineQuality struct {
	LineID      string
	VendorID    string
	VendorName  string
	QtyAccepted string
	QtyRejected string
}

// GoodsReceiptLineQualities returns every GoodsReceiptLine resolvable to
// a live (non-deleted) PurchaseOrder via its GoodsReceipt's
// purchase_order_id, regardless of whether the line carries quality
// data — same "return everything, let the forecast package decide
// validity" shape CompletedPOLeadTimes already uses for on-time samples.
//
// The GoodsReceipt and PurchaseOrder hops are INNER joins with
// deleted_at IS NULL, deliberately: a line whose GoodsReceipt or
// PurchaseOrder no longer exists (soft-deleted) is excluded, matching
// CompletedPOLeadTimes' own precedent of requiring PurchaseOrder itself
// to be a live record rather than treating a deleted one as still-valid
// business history. This is NARROWER than the vendor hop below — a
// correction, not the same rule applied twice (independent review of
// uc-infra#82 caught an earlier doc comment overclaiming "never
// dropped" here, which was only ever true of the vendor hop).
//
// The vendor join, in contrast, IS LEFT and compares id::text (not
// ::uuid-cast): a line whose LIVE PO has a malformed or absent
// vendor_id still contributes its quality data to the OVERALL aggregate
// with an empty VendorID, rather than being dropped — the same
// reasoning CompletedPOLeadTimes' own vendor join gives. The
// GoodsReceipt/PurchaseOrder hops DO need the uuidPattern guard: unlike
// the vendor hop, these are ::uuid casts (a malformed goods_receipt_id
// or purchase_order_id would otherwise abort the whole query for every
// other, perfectly valid line too). Ordered by line id for a
// deterministic result.
func (r *ReportingRepo) GoodsReceiptLineQualities(ctx context.Context) ([]GoodsReceiptLineQuality, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT l.id, coalesce(v.id::text, ''), coalesce(v.data->>'name', v.id::text, ''),
		        coalesce(l.data->>'qty_accepted', ''), coalesce(l.data->>'qty_rejected', '')
		 FROM records l
		 JOIN records g
		   ON g.entity_type = 'GoodsReceipt'
		  AND g.deleted_at IS NULL
		  AND g.id = (l.data->>'goods_receipt_id')::uuid
		 JOIN records po
		   ON po.entity_type = 'PurchaseOrder'
		  AND po.deleted_at IS NULL
		  AND po.id = (g.data->>'purchase_order_id')::uuid
		 LEFT JOIN records v
		   ON v.entity_type = 'Party'
		  AND v.deleted_at IS NULL
		  AND v.id::text = po.data->>'vendor_id'
		 WHERE l.entity_type = 'GoodsReceiptLine'
		   AND l.deleted_at IS NULL
		   AND l.data->>'goods_receipt_id' ~ $1
		   AND g.data->>'purchase_order_id' ~ $1
		 ORDER BY l.id`,
		uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("goods receipt line qualities: %w", err)
	}
	defer rows.Close()

	var out []GoodsReceiptLineQuality
	for rows.Next() {
		var row GoodsReceiptLineQuality
		if err := rows.Scan(&row.LineID, &row.VendorID, &row.VendorName, &row.QtyAccepted, &row.QtyRejected); err != nil {
			return nil, fmt.Errorf("scan goods receipt line quality row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
