package finance

import "github.com/universaltill/universal-core/internal/kernel/form"

func AccountForm() *form.Definition {
	return &form.Definition{
		EntityType: "Account",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "type", Label: "Type"},
					{Name: "parent_account_id", Label: "Parent Account"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "is_active", Label: "Active"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func FiscalYearForm() *form.Definition {
	return &form.Definition{
		EntityType: "FiscalYear",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "name", Label: "Name"},
					{Name: "start_date", Label: "Start Date"},
					{Name: "end_date", Label: "End Date"},
					{Name: "status", Label: "Status"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func PeriodForm() *form.Definition {
	return &form.Definition{
		EntityType: "Period",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "fiscal_year_id", Label: "Fiscal Year"},
					{Name: "name", Label: "Name"},
					{Name: "start_date", Label: "Start Date"},
					{Name: "end_date", Label: "End Date"},
					{Name: "status", Label: "Status"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func TaxCodeForm() *form.Definition {
	return &form.Definition{
		EntityType: "TaxCode",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "rate", Label: "Rate"},
					{Name: "tax_type", Label: "Tax Type"},
					{Name: "jurisdiction", Label: "Jurisdiction"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

func CostCenterForm() *form.Definition {
	return &form.Definition{
		EntityType: "CostCenter",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "code", Label: "Code"},
					{Name: "name", Label: "Name"},
					{Name: "type", Label: "Type"},
				},
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// AllForms returns every Form Definition this package defines — the
// source of truth seed.go's PublishForms iterates.
func AllForms() []*form.Definition {
	return []*form.Definition{
		AccountForm(), FiscalYearForm(), PeriodForm(), TaxCodeForm(), CostCenterForm(),
	}
}
