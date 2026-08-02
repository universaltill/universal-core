package sales

import "github.com/universaltill/universal-core/internal/kernel/form"

// SalesOrderForm's Lines section is a real master-detail (Target:
// "SOLine", rolling SOLine.line_total up into SalesOrder.total) — the
// same pattern PurchaseOrderForm uses for POLine. Still no workflow/
// report actions, same reasoning as PurchaseOrderForm's own doc comment:
// this kernel has no so_approval workflow or sales report defined yet.
// The "Download UBL file" navigate action (v2, uc-infra#66) mirrors
// PurchaseOrderForm's own — see that Definition's doc comment for why
// it's safe to add without a workflow/report behind it.
func SalesOrderForm() *form.Definition {
	return &form.Definition{
		EntityType: "SalesOrder",
		Version:    2,
		Sections: []form.Section{
			{
				Title:     "Header",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "so_number", Label: "SO Number"},
					{Name: "customer_id", Label: "Customer"},
					{Name: "order_date", Label: "Order Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "status_id", Label: "Status"},
					{Name: "total", Label: "Total"},
				},
			},
			{
				Title:        "Lines",
				Component:    form.ComponentMasterDetail,
				Target:       "SOLine",
				RollUp:       "line_total",
				RollUpTarget: "total",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
			{Label: "Download UBL file", Op: form.OpNavigate, Route: "/export/SalesOrder/{id}/ubl"},
		},
	}
}

func SOLineForm() *form.Definition {
	return &form.Definition{
		EntityType: "SOLine",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "sales_order_id", Label: "Sales Order"},
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

// CustomerInvoiceForm has no master-detail section, unlike
// SalesOrderForm/GoodsReceiptForm — CustomerInvoice has no line-item
// child entity in this first slice (this package's own doc comment on
// CustomerInvoice). The "Download UBL file" navigate action (v2,
// uc-infra#66) mirrors PurchaseOrderForm's own.
func CustomerInvoiceForm() *form.Definition {
	return &form.Definition{
		EntityType: "CustomerInvoice",
		Version:    2,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "invoice_number", Label: "Invoice Number"},
					{Name: "sales_order_id", Label: "Sales Order"},
					{Name: "customer_id", Label: "Customer"},
					{Name: "invoice_date", Label: "Invoice Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "status_id", Label: "Status"},
					{Name: "total", Label: "Total"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
			{Label: "Download UBL file", Op: form.OpNavigate, Route: "/export/CustomerInvoice/{id}/ubl"},
		},
	}
}

// AllForms returns every Form Definition this package defines — the
// source of truth seed.go's PublishForms iterates (see
// foundation.AllForms's doc comment for why this exists instead of a
// second, separately-maintained list).
func AllForms() []*form.Definition {
	return []*form.Definition{
		SalesOrderForm(), SOLineForm(), CustomerInvoiceForm(),
	}
}
