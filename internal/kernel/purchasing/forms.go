package purchasing

import "github.com/universaltill/universal-core/internal/kernel/form"

// ItemForm, POLineForm and InventoryItemForm are plain single-section
// forms — each entity is independently CRUD-able via the generic
// /forms/{entityType}/new|{id} route (internal/api/handlers.go) even
// though POLine and InventoryItem are usually reached through
// PurchaseOrderForm's master-detail section or a report, not navigated to
// directly; having a real form means the generic route works for them
// too, same as every other entity in this kernel.
func ItemForm() *form.Definition {
	return &form.Definition{
		EntityType: "Item",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "sku", Label: "SKU"},
					{Name: "name", Label: "Name"},
					{Name: "item_type", Label: "Type"},
					{Name: "base_uom_id", Label: "Unit of Measure"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// PurchaseOrderForm's Lines section is a real master-detail (Target:
// "POLine", rolling POLine.line_total up into PurchaseOrder.total) — the
// same pattern formrender/render_test.go's fixture already exercises,
// now backing an actual entity instead of just a test fixture. No
// workflow/report actions: unlike that fixture's "Submit for Approval"/
// "Print", this kernel doesn't have a po_approval workflow or po_print
// report defined yet (QUEUE.md) — adding an action wired to a
// nonexistent workflow/report would 404 the moment someone clicked it,
// so Save is the only real action for now.
func PurchaseOrderForm() *form.Definition {
	return &form.Definition{
		EntityType: "PurchaseOrder",
		Version:    2,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "po_number", Label: "PO Number"},
					{Name: "vendor_id", Label: "Vendor"},
					{Name: "order_date", Label: "Order Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "status", Label: "Status"},
					{Name: "total", Label: "Total"},
				},
			},
			{
				Title:        "Lines",
				Component:    form.ComponentMasterDetail,
				Target:       "POLine",
				RollUp:       "line_total",
				RollUpTarget: "total",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func POLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "POLine",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "purchase_order_id", Label: "Purchase Order"},
					{Name: "item_id", Label: "Item"},
					{Name: "qty", Label: "Quantity"},
					{Name: "unit_price", Label: "Unit Price"},
					{Name: "line_total", Label: "Line Total"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// AllForms returns every Form Definition this package defines — the
// source of truth seed.go's PublishForms iterates (see
// foundation.AllForms's doc comment for why this exists instead of a
// second, separately-maintained list).
func AllForms() []*form.Definition {
	return []*form.Definition{ItemForm(), PurchaseOrderForm(), POLineForm(), InventoryItemForm()}
}

func InventoryItemForm() *form.Definition {
	return &form.Definition{
		EntityType: "InventoryItem",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "item_id", Label: "Item"},
					{Name: "qty_on_hand", Label: "Qty On Hand"},
					{Name: "qty_available_to_promise", Label: "Qty Available to Promise"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}
