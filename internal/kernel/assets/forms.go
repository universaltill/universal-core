package assets

import "github.com/universaltill/universal-core/internal/kernel/form"

// FixedAssetForm groups the fields the way someone registering an asset
// thinks about it: what it is, what it cost and over how long, and
// which accounts its depreciation touches. The schedule is a
// master-detail section with NO roll-up target — unlike an order's
// lines summing to a total, a depreciation schedule's charges sum to
// the depreciable base, which is derived from cost and salvage rather
// than stored, so rolling it up into a field would create a second
// source of truth that could disagree with the first.
func FixedAssetForm() *form.Definition {
	return &form.Definition{
		EntityType: "FixedAsset",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Asset",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "asset_number", Label: "Asset Number"},
					{Name: "name", Label: "Name"},
					{Name: "location", Label: "Location"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Valuation",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "acquisition_date", Label: "Acquisition Date"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "cost", Label: "Cost"},
					{Name: "salvage_value", Label: "Salvage Value"},
					{Name: "useful_life_months", Label: "Useful Life (months)"},
					{Name: "depreciation_method", Label: "Depreciation Method"},
				},
			},
			{
				Title:     "Posting Accounts",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "asset_account_id", Label: "Asset Account"},
					{Name: "depreciation_expense_account_id", Label: "Depreciation Expense Account"},
					{Name: "accumulated_depreciation_account_id", Label: "Accumulated Depreciation Account"},
				},
			},
			{
				Title:     "Depreciation Schedule",
				Component: form.ComponentMasterDetail,
				Target:    "DepreciationSchedule",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// DepreciationScheduleForm exists so a schedule row is independently
// viewable and correctable — the rows are generated, but a correction
// should go through the same audited CRUD path as any other record
// rather than requiring a regeneration that would discard history.
func DepreciationScheduleForm() *form.Definition {
	return &form.Definition{
		EntityType: "DepreciationSchedule",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Period",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "fixed_asset_id", Label: "Fixed Asset"},
					{Name: "sequence", Label: "Sequence"},
					{Name: "period_end", Label: "Period End"},
					{Name: "depreciation_amount", Label: "Depreciation"},
					{Name: "book_value", Label: "Book Value"},
					{Name: "posted_at", Label: "Posted"},
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
		FixedAssetForm(),
		DepreciationScheduleForm(),
	}
}
