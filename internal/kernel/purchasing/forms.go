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
		Version:    5,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "po_number", Label: "PO Number"},
					{Name: "vendor_id", Label: "Vendor"},
					{Name: "order_date", Label: "Order Date"},
					{Name: "promised_delivery_date", Label: "Promised Delivery Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "status_id", Label: "Status"},
					{Name: "total", Label: "Total"},
				},
			},
			{
				// The six R10 stage timestamps (#29) — their own section
				// so the header stays scannable; labels localize via the
				// field.PurchaseOrder.* catalog keys like every other
				// field label.
				Title:     "Lead-time stages",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "sourced_at", Label: "Sourced"},
					{Name: "production_start_at", Label: "Production Started"},
					{Name: "production_ready_at", Label: "Production Ready"},
					{Name: "shipped_at", Label: "Shipped"},
					{Name: "customs_cleared_at", Label: "Customs Cleared"},
					{Name: "received_at", Label: "Received"},
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
	return []*form.Definition{
		ItemForm(), PurchaseOrderForm(), POLineForm(), FacilityForm(), InventoryItemForm(),
		StockTransferForm(),
		GoodsReceiptForm(), GoodsReceiptLineForm(), VendorInvoiceForm(),
		ReorderRuleForm(),
		RequestForQuotationForm(), RequestForQuotationLineForm(),
		RequestForQuotationVendorForm(), RequestForQuotationQuoteLineForm(),
	}
}

// RequestForQuotationForm's Vendors and Lines sections are both
// master-detail (composition children — see RequestForQuotation's own
// doc comment on why each is a real child entity, not an array field).
// Neither carries a RollUp, unlike PurchaseOrderForm's own Lines
// section: there's no header total field to roll a line sum into — an
// RFQ line has no unit_price at all (RequestForQuotationLine's own doc
// comment: this is a request for pricing, not a priced commitment).
func RequestForQuotationForm() *form.Definition {
	return &form.Definition{
		EntityType: "RequestForQuotation",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "rfq_number", Label: "RFQ Number"},
					{Name: "due_date", Label: "Due Date"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Vendors",
				Component: form.ComponentMasterDetail,
				Target:    "RequestForQuotationVendor",
			},
			{
				Title:     "Lines",
				Component: form.ComponentMasterDetail,
				Target:    "RequestForQuotationLine",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func RequestForQuotationLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "RequestForQuotationLine",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "request_for_quotation_id", Label: "Request for Quotation"},
					{Name: "item_id", Label: "Item"},
					{Name: "qty", Label: "Quantity"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func RequestForQuotationVendorForm() *form.Definition {
	return &form.Definition{
		EntityType: "RequestForQuotationVendor",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "request_for_quotation_id", Label: "Request for Quotation"},
					{Name: "vendor_id", Label: "Vendor"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// RequestForQuotationQuoteLineForm is a plain single-section form — the
// manual data-entry surface procurement staff use to record a vendor's
// quoted price against one RFQ line (see
// RequestForQuotationQuoteLine's own doc comment: there is no vendor
// self-service submission in this codebase).
func RequestForQuotationQuoteLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "RequestForQuotationQuoteLine",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "rfq_line_id", Label: "RFQ Line"},
					{Name: "vendor_id", Label: "Vendor"},
					{Name: "unit_price", Label: "Unit Price"},
					{Name: "quoted_at", Label: "Quoted At"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// ReorderRuleForm is a plain single-section form, same shape as
// InventoryItemForm — a ReorderRule is a small policy record, usually
// reached from the purchasing report's reorder-signal table rather than
// navigated to directly, but the generic /forms/ReorderRule/new route
// works for it the same as every other entity (this file's own top doc
// comment). Labels localize via the field.ReorderRule.* catalog keys.
func ReorderRuleForm() *form.Definition {
	return &form.Definition{
		EntityType: "ReorderRule",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "item_id", Label: "Item"},
					{Name: "reorder_point", Label: "Reorder Point"},
					{Name: "safety_stock", Label: "Safety Stock"},
					{Name: "target_lead_time_confidence", Label: "Lead-Time Confidence"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// VendorInvoiceForm is a plain single-section form, same shape as
// CustomerInvoice's own (sales/forms.go) — no master-detail Lines
// section, matching VendorInvoice's own doc comment on why this task
// stayed header-only (no VendorInvoiceLine breakdown).
//
// match_exception_reason (Version 2 — matching VendorInvoice's own field
// Version bump) is listed here for the same reason every other field is:
// so it's actually visible on the record's own detail/edit page, not
// only present in stored data — the whole point of
// MatchVendorInvoiceOnUpdate (ledger.go) recording *why* a match failed
// is defeated if a reader can never see it. This kernel's FormField has
// no read-only concept yet (form/definition.go), so this stays a plain
// editable field like every other one here — same trust-the-form-
// submission posture this kernel already takes with status_id and total,
// not a gap unique to this field.
func VendorInvoiceForm() *form.Definition {
	return &form.Definition{
		EntityType: "VendorInvoice",
		Version:    2,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "invoice_number", Label: "Invoice Number"},
					{Name: "purchase_order_id", Label: "Purchase Order"},
					{Name: "vendor_id", Label: "Vendor"},
					{Name: "invoice_date", Label: "Invoice Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "status_id", Label: "Status"},
					{Name: "total", Label: "Total"},
					{Name: "match_exception_reason", Label: "Match Exception Reason"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// StockTransferForm is a plain single-section form, same shape as
// InventoryItemForm/FacilityForm — StockTransfer is a single-item record
// (StockTransfer's own doc comment on why this stays single-item, not
// header+lines), so there is no master-detail section to add.
func StockTransferForm() *form.Definition {
	return &form.Definition{
		EntityType: "StockTransfer",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "item_id", Label: "Item"},
					{Name: "from_facility_id", Label: "From Facility"},
					{Name: "to_facility_id", Label: "To Facility"},
					{Name: "qty", Label: "Quantity"},
					{Name: "transfer_date", Label: "Transfer Date"},
					{Name: "status_id", Label: "Status"},
					{Name: "notes", Label: "Notes"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// GoodsReceiptForm's Lines section is a real master-detail (Target:
// "GoodsReceiptLine"), same pattern as PurchaseOrderForm's own Lines
// section — no RollUp here, unlike PurchaseOrder.total: a goods receipt
// has no header total field of its own to roll a line sum into.
func GoodsReceiptForm() *form.Definition {
	return &form.Definition{
		EntityType: "GoodsReceipt",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "purchase_order_id", Label: "Purchase Order"},
					{Name: "received_date", Label: "Received Date"},
					{Name: "notes", Label: "Notes"},
				},
			},
			{
				Title:     "Lines",
				Component: form.ComponentMasterDetail,
				Target:    "GoodsReceiptLine",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func GoodsReceiptLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "GoodsReceiptLine",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "goods_receipt_id", Label: "Goods Receipt"},
					{Name: "po_line_id", Label: "PO Line"},
					{Name: "item_id", Label: "Item"},
					{Name: "qty_received", Label: "Qty Received"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// FacilityForm is a plain single-section form — a facility is a short
// record of identity and status, with no child rows and nothing to
// stage across sections.
func FacilityForm() *form.Definition {
	return &form.Definition{
		EntityType: "Facility",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Facility",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "facility_type", Label: "Type"},
					{Name: "address_id", Label: "Address"},
					{Name: "is_active", Label: "Active"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func InventoryItemForm() *form.Definition {
	return &form.Definition{
		EntityType: "InventoryItem",
		Version:    2,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "item_id", Label: "Item"},
					{Name: "facility_id", Label: "Facility"},
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
