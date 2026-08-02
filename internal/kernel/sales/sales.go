// Package sales is the second *operational* module built on top of the
// foundation layer (internal/kernel/foundation) — SalesOrder, SOLine, and
// CustomerInvoice, per reference-data-model.md §5. Like purchasing
// (internal/kernel/purchasing), this is NOT part of every tenant's
// baseline (ADR-0001 §8) — a tenant only gets these once Sales is one of
// their licensed modules, hence its own Publish (seed.go).
//
// Scoped to exactly the slice INTEGRATIONS.md's own "what Universal
// Till's connector still needs" list names as prerequisite #1:
// SalesOrder/SOLine/CustomerInvoice, mirroring purchasing's
// PurchaseOrder/POLine/GoodsReceipt shape (a header entity with a
// composition-child line, plus a header entity for the resulting
// fulfillment/billing document). Deliberately NOT built in this pass —
// real future work, not forgotten (reference-data-model.md §5 names all
// of it): Quotation (a pre-order price offer that may convert to a
// SalesOrder), Shipment (the outbound-delivery counterpart to
// purchasing's GoodsReceipt — StockMovement wiring belongs there, same
// deliberate deferral purchasing.GoodsReceipt's own doc comment takes
// with InventoryItem.qty_on_hand), PriceList/PriceListLine (default
// pricing), and a dedicated InvoiceLine breakdown for CustomerInvoice
// (kept a single header entity for this first slice, the same way
// GoodsReceipt shipped before GoodsReceiptLine's own line-level
// granularity mattered enough to justify — except here a second entity
// genuinely isn't needed yet: nothing reads invoice line detail today).
//
// Depends on purchasing.Item existing (SOLine.item_id targets it) —
// Item is reference-data-model.md's own shared master data for both
// sides of a business (what you buy and what you sell can be, and
// usually are, the same catalog), it's just implemented under the
// purchasing package today, not a separate foundation-level Item. A
// tenant licensing Sales without Purchasing has no Item catalog to sell
// against; module-gating/dependency enforcement isn't built yet (same
// gap purchasing.go's own doc comment already flags for its own
// foundation dependency — QUEUE.md), so this is a runtime prerequisite
// documented here, not something Publish itself checks.
package sales

import "github.com/universaltill/universal-core/internal/kernel/entity"

// SalesOrder is a committed customer order (reference-data-model.md §5,
// UBL `Order`) — the sell-side mirror of purchasing.PurchaseOrder.
// customer_id references Party directly, not a separate Customer entity
// — the same Party-Role pattern purchasing.PurchaseOrder's own doc
// comment describes for vendor_id (reference-data-model.md §0): a
// company can hold both the vendor and customer PartyRole on one Party
// record, cmd/seed-demo-data's seedParties already demonstrates keeping
// vendors/customers as two role-tagged sets over the same Party entity
// type rather than two master tables.
//
// status_id/StatusTypeCode ("sales_order_status") follows
// PurchaseOrder's own precedent of opting into foundation.go's generic
// Status/StatusType pattern from day one, rather than starting with a
// plain FieldEnum and needing a version-bump migration later the way
// PurchaseOrder itself did (see that Definition's own doc comment on the
// backfill this caused). The transition graph is seeded as real tenant
// data by sales.PublishStatuses (seed.go), not part of this Definition.
//
// customer_id's TargetFilter (v2, uc-infra#78) requires the referenced
// Party to actually hold the customer PartyRole — before this, a vendor
// Party (never having done business as a customer) could be picked here
// just as easily as a real customer, the same gap PurchaseOrder.vendor_id
// had in the other direction.
func SalesOrder() *entity.Definition {
	return &entity.Definition{
		EntityType: "SalesOrder",
		// Version 3 (uc-infra#80): total gained a Min:0 bound.
		Version:        3,
		Module:         "sales",
		StatusTypeCode: "sales_order_status",
		Fields: []entity.Field{
			// so_number is the natural key, same role po_number plays on
			// PurchaseOrder — not schema-enforced unique (no such
			// constraint concept exists yet in entity.Field, same
			// application-level convention as po_number/Item.sku/
			// Currency.code).
			{Name: "so_number", Type: entity.FieldString, Required: true},
			{Name: "customer_id", Type: entity.FieldReference, Required: true, Target: "Party", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "customer"},
			}},
			{Name: "order_date", Type: entity.FieldDate, Required: true},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
		},
		Relationships: []entity.Relationship{
			// ParentField ("sales_order_id") is what
			// internal/api/handlers.go's loadMasterDetailChildren looks up
			// to find this SalesOrder's SOLine rows for SalesOrderForm's
			// master-detail section — same mechanism PurchaseOrder's own
			// Relationship doc comment describes.
			{Name: "lines", Kind: entity.RelationComposition, Target: "SOLine", ParentField: "sales_order_id"},
		},
	}
}

// SOLine is one ordered item + qty + price, the composition child of
// SalesOrder (reference-data-model.md §5) — the sell-side mirror of
// purchasing.POLine. Kept independently CRUD-able/importable, same
// reasoning as POLine's own doc comment.
func SOLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "SOLine",
		// Version 2 (uc-infra#80): qty, unit_price and line_total gained
		// Min:0 bounds.
		Version: 2,
		Module:  "sales",
		Fields: []entity.Field{
			{Name: "sales_order_id", Type: entity.FieldReference, Required: true, Target: "SalesOrder"},
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "unit_price", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "line_total", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
		},
	}
}

// CustomerInvoice is a bill issued to a customer against a SalesOrder
// (reference-data-model.md §5) — the sell-side mirror of purchasing's
// GoodsReceipt, but unlike GoodsReceipt it does carry its own status:
// an invoice's draft/issued/paid/void lifecycle is real accounting state
// with consequences (has it been sent, has it been paid), not just a
// record of something that already physically happened the way a goods
// receipt is. customer_id is carried directly alongside sales_order_id
// (not derived by looking it up at read time) for the same reason
// GoodsReceiptLine carries item_id directly alongside po_line_id — a
// report or CSV import over CustomerInvoice shouldn't need a join
// through SalesOrder just to know who owes the money.
//
// Deliberately NOT built in this pass: an InvoiceLine breakdown (see
// this package's own doc comment), and any actual posting to
// internal/kernel/ledger — a real invoice eventually needs a journal
// entry (issued -> AR debit/revenue credit; paid -> cash debit/AR
// credit), which is exactly the kind of deterministic-core work
// CLAUDE.md reserves for ledger's own hand-written, human-reviewed code,
// not something to bolt onto a first CRUD slice of this metadata-driven
// module.
func CustomerInvoice() *entity.Definition {
	return &entity.Definition{
		EntityType: "CustomerInvoice",
		// Version 2 (uc-infra#80): total gained a Min:0 bound.
		Version:        2,
		Module:         "sales",
		StatusTypeCode: "customer_invoice_status",
		Fields: []entity.Field{
			{Name: "invoice_number", Type: entity.FieldString, Required: true},
			{Name: "sales_order_id", Type: entity.FieldReference, Required: true, Target: "SalesOrder"},
			{Name: "customer_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "invoice_date", Type: entity.FieldDate, Required: true},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
		},
	}
}

// All returns every Definition this module adds — the set a tenant gets
// once Sales is one of their licensed modules (seed.go's Publish).
func All() []*entity.Definition {
	return []*entity.Definition{
		SalesOrder(),
		SOLine(),
		CustomerInvoice(),
	}
}
