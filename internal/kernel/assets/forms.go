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
		// Version 2, in lockstep with the entity (assets.go): the
		// maintenance section changed this form's content, and
		// moduleseed skips a (key, version) that is already published —
		// so editing v1 in place would have silently delivered nothing
		// to every tenant provisioned by the previous release, with no
		// error to notice. A Definition whose serialized content changes
		// gets a new version, form or entity (independent review).
		Version: 2,
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
			{
				// Read-only history: maintenance orders are independent
				// records with their own lifecycle, so the asset form
				// shows them rather than owning them.
				Title:     "Maintenance History",
				Component: form.ComponentRelatedList,
				Target:    "MaintenanceOrder",
			},
		},
		Actions: []form.Action{
			{Label: "Save", Op: form.OpSave},
		},
	}
}

// DepreciationScheduleForm exists so a schedule row is independently
// viewable and correctable — the rows are generated
// (GenerateDepreciationScheduleOnWrite, schedule_hook.go), but a
// correction goes through the same audited CRUD path as any other
// record rather than a dedicated regeneration action. That correction
// is not currently guaranteed to survive a later, unrelated save of the
// row's own FixedAsset, though — see DepreciationSchedule's own doc
// comment (assets.go) and uc-infra#236.
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

// MaintenanceOrderForm keeps the plan and the outcome visually
// separate: what was scheduled, then what actually happened and what
// it cost. No roll-up into FixedAsset — maintenance spend is not part
// of an asset's depreciable cost (capitalising a repair is a
// deliberate accounting decision, not an automatic one), so summing it
// onto the asset would quietly misstate the balance sheet.
func MaintenanceOrderForm() *form.Definition {
	return &form.Definition{
		EntityType: "MaintenanceOrder",
		Version:    1,
		Sections: []form.Section{
			{
				Title:     "Work",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "order_number", Label: "Order Number"},
					{Name: "asset_id", Label: "Asset"},
					{Name: "maintenance_type", Label: "Type"},
					{Name: "description", Label: "Description"},
					{Name: "status_id", Label: "Status"},
				},
			},
			{
				Title:     "Schedule and Cost",
				Component: form.ComponentFields,
				Fields: []form.FormField{
					{Name: "scheduled_date", Label: "Scheduled Date"},
					{Name: "completed_date", Label: "Completed Date"},
					{Name: "vendor_id", Label: "Service Provider"},
					{Name: "currency_id", Label: "Currency"},
					{Name: "cost", Label: "Cost"},
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
		MaintenanceOrderForm(),
	}
}
