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

// PurchaseOrderStatusBreakdown groups every one of tenantID's
// PurchaseOrder records by status. Callers that want a fixed display
// order (draft, submitted, approved, received, cancelled) should
// reorder the result themselves — this returns whatever combination of
// statuses actually has at least one order, in no particular order.
func (r *ReportingRepo) PurchaseOrderStatusBreakdown(ctx context.Context, tenantID string) ([]PurchaseOrderStatusCount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT data->>'status' AS status, count(*), coalesce(sum((data->>'total')::numeric), 0)
		 FROM records
		 WHERE tenant_id = $1 AND entity_type = 'PurchaseOrder' AND deleted_at IS NULL
		 GROUP BY data->>'status'`,
		tenantID,
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

// TopVendorsBySpend returns tenantID's vendors ranked by total
// PurchaseOrder value, highest first, capped at limit. A vendor_id that
// doesn't resolve to a live Party row (deleted, or simply malformed —
// see uuidPattern's doc comment) is excluded rather than aborting the
// whole query.
func (r *ReportingRepo) TopVendorsBySpend(ctx context.Context, tenantID string, limit int) ([]VendorSpend, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.id, coalesce(v.data->>'name', v.id::text), count(po.id), coalesce(sum((po.data->>'total')::numeric), 0) AS spend
		 FROM records po
		 JOIN records v
		   ON v.tenant_id = po.tenant_id
		  AND v.entity_type = 'Party'
		  AND v.deleted_at IS NULL
		  AND v.id = (po.data->>'vendor_id')::uuid
		 WHERE po.tenant_id = $1
		   AND po.entity_type = 'PurchaseOrder'
		   AND po.deleted_at IS NULL
		   AND po.data->>'vendor_id' ~ $3
		 GROUP BY v.id, v.data->>'name'
		 ORDER BY spend DESC, v.id
		 LIMIT $2`,
		tenantID, limit, uuidPattern,
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

// StockSummary is the tenant-wide roll-up over every InventoryItem row.
type StockSummary struct {
	ItemCount     int
	TotalOnHand   float64
	TotalATP      float64
	StockoutCount int // items with qty_available_to_promise <= 0
}

func (r *ReportingRepo) StockSummary(ctx context.Context, tenantID string) (StockSummary, error) {
	var s StockSummary
	err := r.db.QueryRowContext(ctx,
		`SELECT
		   count(*),
		   coalesce(sum((data->>'qty_on_hand')::numeric), 0),
		   coalesce(sum((data->>'qty_available_to_promise')::numeric), 0),
		   count(*) FILTER (WHERE (data->>'qty_available_to_promise')::numeric <= 0)
		 FROM records
		 WHERE tenant_id = $1 AND entity_type = 'InventoryItem' AND deleted_at IS NULL`,
		tenantID,
	).Scan(&s.ItemCount, &s.TotalOnHand, &s.TotalATP, &s.StockoutCount)
	if err != nil {
		return StockSummary{}, fmt.Errorf("stock summary: %w", err)
	}
	return s, nil
}

// StockoutRiskItem is one Item with no quantity available to promise —
// nothing left to sell/allocate without a new order arriving, the
// concrete "stock intelligence" signal this workbench surfaces (per
// QUEUE.md's Ansar synthetic-data demo entry). Deliberately not a
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

// StockoutRiskItems returns tenantID's Items with qty_available_to_promise
// <= 0, most-negative first, capped at limit. Same malformed-reference
// guard as TopVendorsBySpend.
func (r *ReportingRepo) StockoutRiskItems(ctx context.Context, tenantID string, limit int) ([]StockoutRiskItem, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT i.id, coalesce(i.data->>'sku', ''), coalesce(i.data->>'name', i.id::text),
		        coalesce((inv.data->>'qty_on_hand')::numeric, 0),
		        coalesce((inv.data->>'qty_available_to_promise')::numeric, 0)
		 FROM records inv
		 JOIN records i
		   ON i.tenant_id = inv.tenant_id
		  AND i.entity_type = 'Item'
		  AND i.deleted_at IS NULL
		  AND i.id = (inv.data->>'item_id')::uuid
		 WHERE inv.tenant_id = $1
		   AND inv.entity_type = 'InventoryItem'
		   AND inv.deleted_at IS NULL
		   AND inv.data->>'item_id' ~ $3
		   AND (inv.data->>'qty_available_to_promise')::numeric <= 0
		 ORDER BY (inv.data->>'qty_available_to_promise')::numeric, i.id
		 LIMIT $2`,
		tenantID, limit, uuidPattern,
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
