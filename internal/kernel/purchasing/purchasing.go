// Package purchasing is the first *operational* module built on the
// foundation layer (internal/kernel/foundation) — Item, PurchaseOrder,
// POLine, and a simplified InventoryItem, per reference-data-model.md
// §2/§3. Unlike foundation, this is NOT always present for every
// tenant (ADR-0001 §8 draws that line specifically around the
// foundation set) — a tenant only gets these once Purchasing is one of
// their licensed modules, hence its own Publish (seed.go), separate
// from foundation.Publish.
//
// Scoped deliberately small for its first slice (the Ansar Group
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
		Version:    1,
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
func PurchaseOrder() *entity.Definition {
	return &entity.Definition{
		EntityType: "PurchaseOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "order_date", Type: entity.FieldDate, Required: true},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"draft", "submitted", "approved", "received", "cancelled"}, Default: "draft"},
			{Name: "total", Type: entity.FieldNumber, Default: float64(0)},
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
		Version:    1,
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
		Version:    1,
		Fields: []entity.Field{
			{Name: "item_id", Type: entity.FieldReference, Required: true, Target: "Item"},
			{Name: "qty_on_hand", Type: entity.FieldNumber, Required: true, Default: float64(0)},
			{Name: "qty_available_to_promise", Type: entity.FieldNumber, Required: true, Default: float64(0)},
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
	}
}
