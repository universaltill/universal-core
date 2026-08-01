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
		Version:        6,
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
			// promised_delivery_date (#11) is the vendor's own commitment
			// made at order time — a buyer/vendor-agreed date, entered
			// manually, never derived — as opposed to the six #29 stage
			// timestamps below, which record what ACTUALLY happened. It is
			// the only honestly-computable half of #11's original
			// "SupplierScorecard" ask that this kernel can support today:
			// on_time_rate compares this promise against the actual
			// receipt date already surfaced by CompletedPOLeadTimes
			// (internal/data/reporting.go). Optional, since a PO written
			// before this Version bump has none, and not every PO
			// negotiation pins one down. NotBefore: order_date rules out
			// the nonsensical case of promising delivery before the order
			// is even placed, same discipline as every stage timestamp
			// below.
			{Name: "promised_delivery_date", Type: entity.FieldDate, NotBefore: "order_date"},
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
// 3-way match (ledger.go's MatchVendorInvoiceOnUpdate) is therefore
// header-level — it compares this invoice's total against the sum of
// everything actually received against this PurchaseOrder
// (GoodsReceiptLine.qty_received × the matching POLine.unit_price), not
// a line-by-line reconciliation.
//
// status_id follows the same StatusType/Status pattern as PurchaseOrder
// and CustomerInvoice (purchasing.PublishStatuses seeds
// "vendor_invoice_status": draft is the only is_initial status,
// draft->matched->paid is the happy path. A match that disagrees no
// longer fails the transition closed — it lands the invoice in
// "match_exception" instead (match_exception_reason below carries why),
// which has no declared edge to "paid": that missing edge is what
// actually blocks payment release until someone corrects the data and
// retries match_exception->matched, see MatchVendorInvoiceOnUpdate's own
// doc comment. void is reachable from draft, matched, or match_exception
// but not paid — money has already moved by then, same reasoning as
// CustomerInvoice's own graph).
func VendorInvoice() *entity.Definition {
	return &entity.Definition{
		EntityType:     "VendorInvoice",
		Version:        2,
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
			// match_exception_reason: set by MatchVendorInvoiceOnUpdate
			// (ledger.go) with the human-readable reason the last match
			// attempt failed, the moment status_id lands on
			// "match_exception"; cleared back to "" the moment a later
			// match agrees. Added in Version 2 — a plain additive optional
			// field, same as every other Version bump in this file that
			// isn't PurchaseOrder's status->status_id replacement, so a
			// VendorInvoice row written before this bump just has it
			// absent from its JSONB, no backfill needed.
			{Name: "match_exception_reason", Type: entity.FieldString},
		},
	}
}

// ReorderRule is the per-item replenishment policy behind the
// purchasing report's reorder signals (universal-core#30,
// reference-data-model.md § ReorderRule): when the item's inventory
// position (qty_on_hand + open-PO on-order qty) falls to
// reorder_point + safety_stock or below, the report tells the buyer to
// order now, with the supplier's P50 or P90 lead time — per
// target_lead_time_confidence — as the "how long until it arrives"
// context (internal/kernel/forecast). The rule itself is pure data; all
// signal math lives in the report composition (internal/api/
// reporting.go) and the stats kernel, so this stays an ordinary generic
// Definition with no entity-specific engine code.
//
// Deliberately WITHOUT reference-data-model.md's warehouse_id:
// Warehouse/Facility is card #12 and InventoryItem is item-keyed today
// (one global stock row per Item — see InventoryItem's own doc
// comment), so a warehouse-scoped rule would reference an entity that
// doesn't exist yet and scope a quantity that isn't scoped. Add the
// field (Version bump) when #12 lands, the same grow-don't-guess
// discipline as every other deferral in this file.
func ReorderRule() *entity.Definition {
	return &entity.Definition{
		EntityType: "ReorderRule",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "reorder_point", Type: entity.FieldNumber, Required: true},
			// safety_stock defaults to 0 rather than being required —
			// "no buffer" is a legitimate policy and the signal math
			// treats a missing value as 0 anyway (issue #30's design).
			{Name: "safety_stock", Type: entity.FieldNumber, Default: float64(0)},
			// p90 default: the conservative choice — a buyer who never
			// touches this field gets "order early enough that 90% of
			// history would have arrived in time", not the coin-flip P50.
			{Name: "target_lead_time_confidence", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"p50", "p90"}, Default: "p90"},
		},
	}
}

// RequestForQuotation (RFQ) is a buyer's request to one or more vendors
// for pricing on a set of items before committing to a PurchaseOrder
// (reference-data-model.md's aspirational RFQ shape, Phase 3 R23,
// universal-core#9) — the vendor-comparison step that precedes a
// PurchaseOrder in a real procurement flow, not built until now. Like
// PurchaseOrder, an invited vendor is a Party record holding the vendor
// PartyRole, not a separate Vendor entity — same Party-Role reasoning as
// PurchaseOrder's own doc comment: a company that is both a customer and
// a vendor stays one Party record, not two, and every "vendor" reference
// field in this module (here included) targets Party directly.
//
// rfq_number is the natural key, the same application-level convention
// as PurchaseOrder.po_number (not schema-enforced unique — no such
// constraint concept exists in entity.Field yet, see PurchaseOrder's own
// doc comment on po_number).
//
// Relationships: "lines" (the items being quoted, composition child
// RequestForQuotationLine) and "vendors" (the invited vendors,
// composition child RequestForQuotationVendor). Both are composition
// children rather than fields on this header for the same reason: this
// kernel's entity.FieldReference targets exactly one entity type (no
// array/multi-reference field exists), so reference-data-model.md's
// aspirational "vendor_ids[]" shape has to become a real child entity —
// see RequestForQuotationVendor's own doc comment for the full
// reasoning, which applies equally to why "lines" isn't a single
// multi-valued field either.
//
// status_id/StatusTypeCode ("rfq_status") follows the same
// Status/StatusType pattern as PurchaseOrder/VendorInvoice — see
// PublishStatuses (seed.go) for the actual draft -> sent ->
// quotes_received -> closed/cancelled graph this needs published before
// any RequestForQuotation create/update can pass
// crud.Engine.ValidateStatusTransition.
func RequestForQuotation() *entity.Definition {
	return &entity.Definition{
		EntityType:     "RequestForQuotation",
		Version:        1,
		Module:         "purchasing",
		StatusTypeCode: "rfq_status",
		Fields: []entity.Field{
			{Name: "rfq_number", Type: entity.FieldString, Required: true},
			{Name: "due_date", Type: entity.FieldDate, Required: true},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			{Name: "lines", Kind: entity.RelationComposition, Target: "RequestForQuotationLine", ParentField: "request_for_quotation_id"},
			{Name: "vendors", Kind: entity.RelationComposition, Target: "RequestForQuotationVendor", ParentField: "request_for_quotation_id"},
		},
	}
}

// RequestForQuotationLine is one item + qty a RequestForQuotation is
// asking vendors to price — the composition child of
// RequestForQuotation, mirroring POLine's own role for PurchaseOrder.
// Deliberately NO unit_price here, unlike POLine: an RFQ line is a
// REQUEST, not a priced commitment — the price is exactly the thing
// being solicited from each vendor, and lives instead on
// RequestForQuotationQuoteLine, one row per vendor's response to this
// line.
func RequestForQuotationLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "RequestForQuotationLine",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "request_for_quotation_id", Type: entity.FieldReference, Required: true, Target: "RequestForQuotation"},
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty", Type: entity.FieldNumber, Required: true},
		},
	}
}

// RequestForQuotationVendor is one vendor invited to quote against a
// RequestForQuotation — the composition-child modeling of what
// reference-data-model.md's aspirational RFQ shape describes as a
// "vendor_ids[]" array field. This kernel has no array/multi-reference
// field type at all (entity.FieldReference targets exactly one entity
// type per field — see entity.Field's own doc comment), so "many
// vendors attached to one RFQ header" has to become a real child
// entity, one row per invited vendor, exactly the same composition-child
// pattern POLine/GoodsReceiptLine already use for "many of these
// attached to a header." vendor_id references Party directly — same
// Party-Role reasoning as RequestForQuotation's own doc comment, not a
// separate Vendor entity.
func RequestForQuotationVendor() *entity.Definition {
	return &entity.Definition{
		EntityType: "RequestForQuotationVendor",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "request_for_quotation_id", Type: entity.FieldReference, Required: true, Target: "RequestForQuotation"},
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
		},
	}
}

// RequestForQuotationQuoteLine is one vendor's quoted unit price against
// one RequestForQuotationLine — one row per (line, vendor) response,
// manually entered by procurement staff recording what a vendor quoted
// (phone/email/portal response). There is no vendor self-service quote
// submission anywhere in this codebase, and building one is explicitly
// out of scope for #9 — this is a plain data-entry record, same as every
// other entity in this kernel.
//
// Deliberately NOT a composition child of anything, unlike
// RequestForQuotationLine/RequestForQuotationVendor above: a composition
// relationship has exactly one parent (entity.Relationship.ParentField),
// but a quote line is naturally keyed by TWO independent things — which
// line, and which vendor. That's the same "independent entity with
// plain reference fields, not a child of either side" shape
// VendorInvoice's own doc comment already describes for referencing
// both PurchaseOrder and Party directly rather than being modeled as a
// child of one of them.
//
// quoted_at is optional: a quote line is sometimes entered before the
// vendor's actual response date is confirmed, or backfilled without
// recording one at all — the price itself (unit_price, Required) is the
// one thing this record must carry to be useful for comparison.
func RequestForQuotationQuoteLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Version:    1,
		Module:     "purchasing",
		Fields: []entity.Field{
			{Name: "rfq_line_id", Type: entity.FieldReference, Required: true, Target: "RequestForQuotationLine"},
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "unit_price", Type: entity.FieldNumber, Required: true},
			{Name: "quoted_at", Type: entity.FieldDate},
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
		ReorderRule(),
		RequestForQuotation(),
		RequestForQuotationLine(),
		RequestForQuotationVendor(),
		RequestForQuotationQuoteLine(),
	}
}
