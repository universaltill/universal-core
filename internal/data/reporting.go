package data

import (
	"context"
	"fmt"
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
type PurchaseOrderStatusCount struct {
	Status string
	Count  int
	Value  float64
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
func (r *ReportingRepo) PurchaseOrderStatusBreakdown(ctx context.Context) ([]PurchaseOrderStatusCount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT st.data->>'code' AS status, count(*), coalesce(sum((po.data->>'total')::numeric), 0)
		 FROM records po
		 JOIN records st
		   ON st.entity_type = 'Status'
		  AND st.deleted_at IS NULL
		  AND st.id = (po.data->>'status_id')::uuid
		 WHERE po.entity_type = 'PurchaseOrder'
		   AND po.deleted_at IS NULL
		   AND po.data->>'status_id' ~ $1
		 GROUP BY st.data->>'code'`,
		uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("purchase order status breakdown: %w", err)
	}
	defer rows.Close()

	var out []PurchaseOrderStatusCount
	for rows.Next() {
		var row PurchaseOrderStatusCount
		if err := rows.Scan(&row.Status, &row.Count, &row.Value); err != nil {
			return nil, fmt.Errorf("scan status breakdown row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// VendorSpend is one vendor's aggregate spend across every PurchaseOrder
// pointing at it (regardless of status — a submitted-but-not-yet-
// received order is still committed spend for a management report,
// unlike, say, revenue recognition, which would care about status).
type VendorSpend struct {
	VendorID   string
	VendorName string
	OrderCount int
	Total      float64
}

// TopVendorsBySpend returns vendors ranked by total PurchaseOrder value,
// highest first, capped at limit. A vendor_id that doesn't resolve to a
// live Party row (deleted, or simply malformed — see uuidPattern's doc
// comment) is excluded rather than aborting the whole query.
func (r *ReportingRepo) TopVendorsBySpend(ctx context.Context, limit int) ([]VendorSpend, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.id, coalesce(v.data->>'name', v.id::text), count(po.id), coalesce(sum((po.data->>'total')::numeric), 0) AS spend
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
		limit, uuidPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("top vendors by spend: %w", err)
	}
	defer rows.Close()

	var out []VendorSpend
	for rows.Next() {
		var row VendorSpend
		if err := rows.Scan(&row.VendorID, &row.VendorName, &row.OrderCount, &row.Total); err != nil {
			return nil, fmt.Errorf("scan vendor spend row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// StockSummary is the roll-up over every InventoryItem row.
type StockSummary struct {
	ItemCount     int
	TotalOnHand   float64
	TotalATP      float64
	StockoutCount int // items with qty_available_to_promise <= 0
}

func (r *ReportingRepo) StockSummary(ctx context.Context) (StockSummary, error) {
	var s StockSummary
	err := r.db.QueryRowContext(ctx,
		`SELECT
		   count(*),
		   coalesce(sum((data->>'qty_on_hand')::numeric), 0),
		   coalesce(sum((data->>'qty_available_to_promise')::numeric), 0),
		   count(*) FILTER (WHERE (data->>'qty_available_to_promise')::numeric <= 0)
		 FROM records
		 WHERE entity_type = 'InventoryItem' AND deleted_at IS NULL`,
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
	// "completed" and isn't returned at all.
	ReceivedDate string
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
		        coalesce(nullif(po.data->>'received_at', ''), gr.first_received)
		 FROM records po
		 LEFT JOIN records v
		   ON v.entity_type = 'Party'
		  AND v.deleted_at IS NULL
		  AND v.id::text = po.data->>'vendor_id'
		 LEFT JOIN LATERAL (
		   SELECT min(g.data->>'received_date') AS first_received
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
			&row.ShippedAt, &row.CustomsClearedAt, &row.ReceivedDate); err != nil {
			return nil, fmt.Errorf("scan completed po lead time row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OnOrderQtyByItem sums POLine.qty per item over PurchaseOrders whose
// status code is submitted or approved — the "on order" half of an
// inventory position (issue #30's BA note R3). Deliberately kept
// simple, per the Architect design: received and cancelled orders are
// excluded wholesale (their goods either already count in qty_on_hand
// or are never coming), drafts too (nothing is committed yet), and no
// per-line received-qty netting is attempted — partial receipt against
// a still-open PO is beyond this first worked example. Returned as a
// map (item id -> qty) because every caller is a lookup, not a table.
// Items with no open PO simply have no entry. Same uuidPattern guard as
// every other join in this file, on all three reference hops.
func (r *ReportingRepo) OnOrderQtyByItem(ctx context.Context) (map[string]float64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT l.data->>'item_id', coalesce(sum((l.data->>'qty')::numeric), 0)
		 FROM records l
		 JOIN records po
		   ON po.entity_type = 'PurchaseOrder'
		  AND po.deleted_at IS NULL
		  AND po.id = (l.data->>'purchase_order_id')::uuid
		 JOIN records st
		   ON st.entity_type = 'Status'
		  AND st.deleted_at IS NULL
		  AND st.id = (po.data->>'status_id')::uuid
		 WHERE l.entity_type = 'POLine'
		   AND l.deleted_at IS NULL
		   AND l.data->>'purchase_order_id' ~ $1
		   AND l.data->>'item_id' ~ $1
		   AND po.data->>'status_id' ~ $1
		   AND st.data->>'code' IN ('submitted', 'approved')
		 GROUP BY l.data->>'item_id'`,
		uuidPattern,
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
		`SELECT i.id, coalesce(i.data->>'sku', ''), coalesce(i.data->>'name', i.id::text),
		        coalesce((inv.data->>'qty_on_hand')::numeric, 0),
		        coalesce((inv.data->>'qty_available_to_promise')::numeric, 0)
		 FROM records inv
		 JOIN records i
		   ON i.entity_type = 'Item'
		  AND i.deleted_at IS NULL
		  AND i.id = (inv.data->>'item_id')::uuid
		 WHERE inv.entity_type = 'InventoryItem'
		   AND inv.deleted_at IS NULL
		   AND inv.data->>'item_id' ~ $2
		   AND (inv.data->>'qty_available_to_promise')::numeric <= 0
		 ORDER BY (inv.data->>'qty_available_to_promise')::numeric, i.id
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
