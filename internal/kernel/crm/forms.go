package crm

import "github.com/universaltill/universal-core/internal/kernel/form"

// CaseForm separates the complaint from its context: what the problem
// is, then which customer/product/order it concerns, then how urgently
// it must be answered.
func CaseForm() *form.Definition {
	return &form.Definition{
		EntityType: "Case",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Case",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "case_number", Label: "Case Number"},
					{Name: "subject", Label: "Subject"},
					{Name: "description", Label: "Description"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Context",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "customer_id", Label: "Customer"},
					{Name: "item_id", Label: "Product"},
					{Name: "sales_order_id", Label: "Sales Order"},
				},
			},
			{
				Title:     "Handling",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "priority", Label: "Priority"},
					{Name: "assignee_id", Label: "Assignee"},
					{Name: "opened_date", Label: "Opened"},
					{Name: "sla_due_at", Label: "SLA Due"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// AllForms returns every Form Definition this module adds.
func AllForms() []*form.Definition {
	return []*form.Definition{
		CaseForm(),
	}
}
