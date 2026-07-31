// Package purchasing is the first *operational* module built on the
// foundation layer (internal/kernel/foundation) — Item, PurchaseOrder,
// POLine, and a simplified InventoryItem, per reference-data-model.md
// §2/§3. Unlike foundation, this is NOT always present for every
// tenant (ADR-0001 §8 draws that line specifically around the
// foundation set) — a tenant only gets these once Purchasing is one of
// their licensed modules, hence its own Publish (seed.go), separate
// from foundation.Publish.
//
// Scoped deliberately small for its first slice (a design partner's
// synthetic-data demo, BACKLOG.md's R9/R10 vision): real entities a
// tenant can actually browse/import/report on, not the staged
// lead-time timestamps, P50/P90 forecasting, or workflow-triggered
// reorder alerts reference-data-model.md's PurchaseOrder/ReorderRule
// rows describe — those are R9 (workflow engine wiring) and R10 (a
// whole prediction service) laid on top of this base model, not part of
// it. InventoryItem here is a single qty_on_hand/qty_available_to_promise
// pair per Item — no Warehouse/Facility/Bin (reference-data-model.md's
// per-item×facility×lot shape) — multi-location stock is real future
// work, not needed for a first demo of "the kernel can model purchasing
// data at all."
package purchasing

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Item is a sellable/stockable thing (reference-data-model.md §3).
// base_uom_id references foundation.UnitOfMeasure — Purchasing depends
// on the foundation layer already being published for a tenant, the
// same way every operational module does per ADR-0001 §8.
func Item() *entity.Definition {
	return &entity.Definition{
		EntityType: "Item",
		Version:    2,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "sku", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "item_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"stock", "service", "non_stock"}, Default: "stock"},
			{Name: "base_uom_id", Type: entity.FieldReference, Target: "UnitOfMeasure"},
		},
	}
}

// PurchaseOrder is a committed order to a vendor (reference-data-model.md
// §2, UBL `Order`). vendor_id references Party directly — not a separate
// Vendor entity — matching the Party-Role pattern's whole point
// (reference-data-model.md §0): a vendor is a Party holding the vendor
// PartyRole, not a second master record for the same real-world company.
//
// status_id/StatusTypeCode ("purchase_order_status") is the first real
// entity to opt into foundation.go's generic Status/StatusType pattern —
// the plain FieldEnum this replaces let an update jump straight from
// "draft" to "cancelled" bypassing "submitted"/"approved" with nothing
// to stop it. The actual transition graph (draft is the only initial
// status; draft->submitted->approved->received is the happy path;
// cancellation reachable from draft/submitted/approved but not from
// received, since goods already arrived at that point — reversing that
// is a return/credit-note event, not a status edit) is seeded as real
// tenant data, not part of this Definition — see
// purchasing.PublishStatuses (seed.go), which every Purchasing-licensed
// tenant needs run once (cmd/provision-tenant) before any PurchaseOrder
// create/update can pass crud.Engine.ValidateStatusTransition.
//
// A PurchaseOrder row written before this Version bump (3->4, plain
// "status" enum -> "status_id" reference) has its old "status" string
// sitting unread in its JSONB, not automatically fixed: this kernel
// still has no *generic* record-data migration mechanism (internal/db/
// migrations only ever touches schema), and this was the first Version
// bump to replace an existing Required-bearing field rather than only
// add one. cmd/backfill-purchase-order-status (branch
// purchase-order-status-backfill) is the targeted, one-off fix for this
// specific gap — run it once against any tenant provisioned before this
// change (idempotent, dry-run supported) rather than wiping and
// re-seeding. A generic migration framework was deliberately not built
// for this one case (that would be exactly the premature abstraction
// CLAUDE.md warns against) — if a second entity ever needs the same
// kind of fix, generalize then, against two real examples.
func PurchaseOrder() *entity.Definition {
	return &entity.Definition{
		EntityType:     "PurchaseOrder",
		Version:        5,
		Module:         "purchasing",
		StatusTypeCode: "purchase_order_status",
		Fields: []entity.Field{
			// po_number is the natural key reference-data-model.md's own
			// PurchaseOrder row was missing (every real PO has one — a
			// buyer references it talking to the vendor) and the thing
			// cmd/seed-demo-data's seedPurchaseOrders was working around
			// with a coarse "skip if any exist" guard for lack of one
			// (QUEUE.md, 2026-07-20). Not schema-enforced unique (no
			// such constraint concept exists yet in entity.Field — same
			// as Currency.code/Item.sku, which rely on the same
			// application-level convention, not a DB constraint).
			{Name: "po_number", Type: entity.FieldString, Required: true},
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "order_date", Type: entity.FieldDate, Required: true},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0)},
			// Staged lead-time timestamps (#29) — reference-data-model.md
			// §2's six stages, in order; the raw material for R10's
			// P50/P90 lead-time forecast (#30). All optional (an
			// in-flight PO has only a prefix filled in — realistic
			// censored data the forecast has to live with), each chained
			// to its predecessor via NotBefore so a form submission
			// can't record a stage before the one it follows.
			// received_at is entered manually for now: auto-stamping it
			// from the first GoodsReceipt was considered and deferred to
			// #30's cycle (see the issue's design comment — the forecast
			// may derive actual receipt time from GoodsReceipt data
			// instead, making a stored stamp redundant).
			{Name: "sourced_at", Type: entity.FieldDate, NotBefore: "order_date"},
			{Name: "production_start_at", Type: entity.FieldDate, NotBefore: "sourced_at"},
			{Name: "production_ready_at", Type: entity.FieldDate, NotBefore: "production_start_at"},
			{Name: "shipped_at", Type: entity.FieldDate, NotBefore: "production_ready_at"},
			{Name: "customs_cleared_at", Type: entity.FieldDate, NotBefore: "shipped_at"},
			{Name: "received_at", Type: entity.FieldDate, NotBefore: "customs_cleared_at"},
		},
		Relationships: []entity.Relationship{
			// ParentField ("purchase_order_id") is what
			// internal/api/handlers.go's loadMasterDetailChildren looks up
			// to find this PurchaseOrder's POLine rows for
			// PurchaseOrderForm's master-detail section — not just
			// documentation of the real target shape
			// (reference-data-model.md: "has many POLines").
			{Name: "lines", Kind: entity.RelationComposition, Target: "POLine", ParentField: "purchase_order_id"},
		},
	}
}

// POLine is one ordered item + qty + price, the composition child of
// PurchaseOrder (reference-data-model.md §2). Kept as its own
// independently CRUD-able/importable entity for now (its own
// /api/records/POLine, /forms/POLine/new, CSV import) rather than only
// reachable through a parent PurchaseOrder — same reasoning as the
// Relationship note on PurchaseOrder above.
func POLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "POLine",
		Version:    2,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldReference, Required: true, Target: "PurchaseOrder"},
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty", Type: entity.FieldNumber, Required: true},
			{Name: "unit_price", Type: entity.FieldNumber, Required: true},
			{Name: "line_total", Type: entity.FieldNumber, Default: float64(0)},
		},
	}
}

// InventoryItem is on-hand + available-to-promise quantity per Item —
// deliberately simplified from reference-data-model.md's per-item×
// facility×lot shape (see package doc comment): one row per Item,
// global, no Warehouse/Bin/Lot yet.
func InventoryItem() *entity.Definition {
	return &entity.Definition{
		EntityType: "InventoryItem",
		Version:    2,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty_on_hand", Type: entity.FieldNumber, Required: true, Default: float64(0)},
			{Name: "qty_available_to_promise", Type: entity.FieldNumber, Required: true, Default: float64(0)},
		},
	}
}

// GoodsReceipt is the physical arrival of goods against a PurchaseOrder
// (reference-data-model.md §2, UBL `ReceiptAdvice`) — the next real step
// this module's own doc comment already named as future work ("the
// staged lead-time timestamps... those are laid on top of this base
// model, not part of it"; this is the base-model piece itself, not the
// staged-timestamp layer). Deliberately its own document, not just
// PurchaseOrder.status_id reaching "received": a real PO is routinely
// received in more than one physical delivery (partial shipments,
// especially for the import-heavy, multi-country sourcing pattern
// the design partner's own evidence describes — reference-data-model.md's design-partner
// cross-check explicitly quotes their Inventory Manager asking for GRN
// auto-population from a purchase invoice), so "received" needs to be a
// repeatable event with its own record, not a single header flag.
//
// No status field of its own (unlike PurchaseOrder) — a goods receipt
// records something that already happened (goods physically arrived);
// there's no draft/approval lifecycle to gate before that fact is true,
// so a StatusType/StatusTransition seed (PublishStatuses' own pattern)
// would be pure ceremony with no real transition to enforce.
//
// Deliberately NOT built in this pass, a real next step not forgotten
// (QUEUE.md): actually incrementing InventoryItem.qty_on_hand when a
// GoodsReceiptLine is created. That's a genuine business-logic side
// effect (concurrency-safe, idempotent against re-edits) worth its own
// careful pass, not something to bolt onto a first CRUD slice — this
// entity is real, tenant-visible, importable/reportable data on its own
// even before that wiring exists, the same way POLine was useful before
// PurchaseOrder.total's roll-up existed.
func GoodsReceipt() *entity.Definition {
	return &entity.Definition{
		EntityType: "GoodsReceipt",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "purchase_order_id", Type: entity.FieldReference, Required: true, Target: "PurchaseOrder"},
			{Name: "received_date", Type: entity.FieldDate, Required: true},
			{Name: "notes", Type: entity.FieldString},
		},
		Relationships: []entity.Relationship{
			// Same master-detail pattern as PurchaseOrder/POLine — see
			// that Relationship's own doc comment on why ParentField
			// exists (internal/api/handlers.go's loadMasterDetailChildren
			// looks this up by name, not by convention).
			{Name: "lines", Kind: entity.RelationComposition, Target: "GoodsReceiptLine", ParentField: "goods_receipt_id"},
		},
	}
}

// GoodsReceiptLine is one received item + qty against a specific POLine
// — the composition child of GoodsReceipt (reference-data-model.md §2).
// item_id is carried alongside po_line_id (not derived by looking up
// po_line_id.item_id at read time) for the same reason POLine carries
// its own item_id rather than only a line-number: every other entity in
// this kernel that references an Item does so directly, and a report or
// CSV import over GoodsReceiptLine shouldn't need a join through POLine
// just to know what was actually received.
func GoodsReceiptLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "GoodsReceiptLine",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "goods_receipt_id", Type: entity.FieldReference, Required: true, Target: "GoodsReceipt"},
			{Name: "po_line_id", Type: entity.FieldReference, Required: true, Target: "POLine"},
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty_received", Type: entity.FieldNumber, Required: true},
		},
	}
}

// VendorInvoice is a bill received from a vendor against a PurchaseOrder
// (reference-data-model.md §2, UBL `Invoice`) — the buy-side mirror of
// sales.CustomerInvoice, "3-way matched against PO + GoodsReceipt" per
// that same reference. vendor_id is carried directly alongside
// purchase_order_id (not derived by looking it up at read time), same
// reasoning as every other entity in this kernel that carries its
// "obviously derivable" reference directly (GoodsReceiptLine.item_id,
// CustomerInvoice.customer_id).
//
// Deliberately NOT built in this pass, matching CustomerInvoice's own
// "no line breakdown" scope-down: a VendorInvoiceLine itemization. The
// 3-way match this task implements (ledger.go's
// MatchVendorInvoiceOnUpdate) is therefore header-level — it compares
// this invoice's total against the sum of everything actually received
// against this PurchaseOrder (GoodsReceiptLine.qty_received × the
// matching POLine.unit_price), not a line-by-line reconciliation. A real
// match-exception workflow (what happens when they disagree beyond
// "reject the transition") is Phase 3's own scoped task
// (erp/BACKLOG-TASKS.md), not this one — this task's match either
// passes (transition allowed) or fails closed (transition rejected,
// nothing written), no partial/flagged state in between yet.
//
// status_id follows the same StatusType/Status pattern as PurchaseOrder
// and CustomerInvoice (purchasing.PublishStatuses seeds
// "vendor_invoice_status": draft is the only is_initial status,
// draft->matched->paid is the happy path, void is reachable from draft
// or matched but not paid — money has already moved by then, same
// reasoning as CustomerInvoice's own graph).
func VendorInvoice() *entity.Definition {
	return &entity.Definition{
		EntityType:     "VendorInvoice",
		Version:        1,
		Module:         "purchasing",
		StatusTypeCode: "vendor_invoice_status",
		Fields: []entity.Field{
			{Name: "invoice_number", Type: entity.FieldString, Required: true},
			{Name: "purchase_order_id", Type: entity.FieldReference, Required: true, Target: "PurchaseOrder"},
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "invoice_date", Type: entity.FieldDate, Required: true},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0)},
		},
	}
}

// All returns every Definition this module adds — the set a tenant gets
// once Purchasing is one of their licensed modules (seed.go's Publish).
func All() []*entity.Definition {
	return []*entity.Definition{
		Item(),
		PurchaseOrder(),
		POLine(),
		InventoryItem(),
		GoodsReceipt(),
		GoodsReceiptLine(),
		VendorInvoice(),
	}
}
