// Package assets is the Assets/EAM module (reference-data-model.md §9,
// BACKLOG.md R8) — FixedAsset and its DepreciationSchedule. A licensed
// module like purchasing/sales/finance, not part of every tenant's
// baseline: published by cmd/provision-tenant only when a tenant has
// actually licensed it.
//
// The depreciation arithmetic lives in this package's depreciation.go
// and is held to the deterministic-core standard (see its own file
// comment) rather than the looser one a CRUD module gets — a schedule
// that drifts by a minor unit per period is a balance-sheet error, not
// a display bug.
//
// Deliberately NOT in this slice, each for its own reason:
//   - **Posting depreciation to the ledger**: the schedule this module
//     generates is the input a periodic posting run would consume
//     (debit depreciation expense, credit accumulated depreciation),
//     and the account references below exist precisely so that run has
//     somewhere to post. But the run itself is a scheduled job writing
//     journal entries — internal/kernel/ledger's hand-written,
//     human-reviewed territory plus workflow's cron trigger — and
//     bolting it onto a first CRUD slice is exactly what
//     sales.CustomerInvoice's own doc comment warned against. Filed as
//     follow-up work.
//   - **Revaluation, impairment, disposal accounting**: each is its own
//     accounting event with its own postings, not a field on this
//     entity.
package assets

import "github.com/universaltill/universal-core/internal/kernel/entity"

// FixedAsset is an owned asset being depreciated
// (reference-data-model.md §9). The three account references are what
// tie it to Finance: an asset's cost sits in one account, its
// accumulated depreciation contra-account offsets it, and each period's
// charge hits an expense account. They are optional at the Definition
// level because an asset can legitimately be registered before its
// posting accounts are decided — the posting run (follow-up work) is
// where their absence becomes an error, not data entry.
//
// currency_id is carried explicitly rather than assumed: cost and
// salvage are money, and depreciation.MinorUnits needs the currency's
// scale to convert them without guessing that everything is 2dp.
func FixedAsset() *entity.Definition {
	return &entity.Definition{
		EntityType: "FixedAsset",
		// Version 3 (uc-infra#80): cost, salvage_value and
		// useful_life_months gained Min:0 bounds. Version 2 (board #20)
		// gained the maintenance related list below. The registry is
		// append-only, so each is a new published version rather than an
		// edit to the last — moduleseed publishes it alongside the old
		// ones and GetPublished takes the highest, which is the designed
		// upgrade path for a module that ships again.
		Version:        3,
		Module:         "assets",
		StatusTypeCode: "fixed_asset_status",
		Fields: []entity.Field{
			// asset_number is the natural key, same application-level
			// convention as po_number/so_number/Item.sku.
			{Name: "asset_number", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldI18nText, Required: true},
			{Name: "acquisition_date", Type: entity.FieldDate, Required: true},
			{Name: "cost", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "salvage_value", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			// Useful life in MONTHS, not years: depreciation is charged
			// monthly, and storing years would force every consumer to
			// re-derive months and disagree about how to handle a
			// part-year life.
			{Name: "useful_life_months", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "depreciation_method", Type: entity.FieldEnum, Required: true,
				// Only the method depreciation.Build actually implements
				// is offered — an enum value with no implementation
				// behind it is a promise the schedule generator breaks.
				EnumValues: []string{"straight_line"},
				Default:    "straight_line"},
			{Name: "location", Type: entity.FieldString},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "asset_account_id", Type: entity.FieldReference, Target: "Account"},
			{Name: "depreciation_expense_account_id", Type: entity.FieldReference, Target: "Account"},
			{Name: "accumulated_depreciation_account_id", Type: entity.FieldReference, Target: "Account"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			// Composition, not a related list: a schedule row has no
			// meaning without its asset, and regenerating a schedule
			// replaces the set wholesale. ParentField is what
			// loadMasterDetailChildren looks up for the form's
			// master-detail section.
			{Name: "schedule", Kind: entity.RelationComposition, Target: "DepreciationSchedule", ParentField: "fixed_asset_id"},
			// Related list, NOT composition: a maintenance order exists
			// independently of the asset's own lifecycle (it has its own
			// status, its own cost, and survives the asset being fully
			// depreciated), so the asset form shows its history read-only
			// rather than owning and cascading it. This is exactly the
			// reference/composition/related-list distinction R11a says to
			// make explicitly rather than default.
			{Name: "maintenance", Kind: entity.RelationRelatedList, Target: "MaintenanceOrder", ParentField: "asset_id"},
		},
	}
}

// DepreciationSchedule is one period's charge and the resulting book
// value (reference-data-model.md §9) — generated by depreciation.Build,
// not hand-entered, though nothing structurally prevents a correction:
// these are ordinary records through the same guarded engine, so an
// adjustment is auditable like any other change rather than a silent
// recomputation.
//
// Amounts are stored as ordinary FieldNumber values (the engine's
// float64) because that is what every other money field in this kernel
// stores today; the exact arithmetic happens in integer minor units and
// is converted once at the boundary. When the kernel grows a real money
// type, this entity converts with the rest of them.
func DepreciationSchedule() *entity.Definition {
	return &entity.Definition{
		EntityType: "DepreciationSchedule",
		// Version 2 (uc-infra#80): depreciation_amount and book_value
		// gained Min:0 bounds. sequence is left unbounded — it is a
		// display/ordering hint, not a quantity or money value.
		Version: 2,
		Module:  "assets",
		Fields: []entity.Field{
			{Name: "fixed_asset_id", Type: entity.FieldReference, Required: true, Target: "FixedAsset"},
			{Name: "sequence", Type: entity.FieldNumber, Required: true},
			// period_end is the last day of the month this row covers —
			// the date a posting for it carries.
			{Name: "period_end", Type: entity.FieldDate, Required: true},
			{Name: "depreciation_amount", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "book_value", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			// posted_at stays empty until a ledger posting run (follow-up
			// work) claims the row. Present from the start so that run
			// doesn't need a version-bump migration to become
			// idempotent — the same foresight PurchaseOrder's own
			// status_id doc comment says was missing there.
			{Name: "posted_at", Type: entity.FieldDate},
		},
	}
}

// MaintenanceOrder is service or repair work against an asset
// (reference-data-model.md §9). §9's row names asset_id,
// scheduled_date, status and cost; this Definition adds an
// order_number natural key (every document entity in this kernel has
// one), a maintenance_type, a completed_date, a description, and a
// vendor_id — a service order that cannot record who performed the
// work is not usable in practice, and Party already models exactly
// that (the same Party-Role pattern purchasing's vendor_id uses).
//
// completed_date deliberately carries NO NotBefore constraint against
// scheduled_date: work finished ahead of schedule is a good outcome,
// not a validation error. The staged-timestamp NotBefore chain on
// PurchaseOrder models a strict physical sequence; this is a plan
// versus an actual, which is a different thing.
//
// Predictive maintenance (BACKLOG.md R10) is explicitly not built
// here — this entity is the history such a model would learn from,
// which is why completed_date and cost are first-class rather than
// free text.
func MaintenanceOrder() *entity.Definition {
	return &entity.Definition{
		EntityType: "MaintenanceOrder",
		// Version 2 (uc-infra#80): cost gained a Min:0 bound.
		Version:        2,
		Module:         "assets",
		StatusTypeCode: "maintenance_order_status",
		Fields: []entity.Field{
			{Name: "order_number", Type: entity.FieldString, Required: true},
			{Name: "asset_id", Type: entity.FieldReference, Required: true, Target: "FixedAsset"},
			{Name: "maintenance_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"preventive", "corrective", "inspection"},
				Default:    "corrective"},
			{Name: "description", Type: entity.FieldI18nText},
			{Name: "scheduled_date", Type: entity.FieldDate, Required: true},
			{Name: "completed_date", Type: entity.FieldDate},
			{Name: "vendor_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "cost", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// All returns every Definition this module adds — the set a tenant gets
// once Assets is one of their licensed modules (seed.go's Publish).
func All() []*entity.Definition {
	return []*entity.Definition{
		FixedAsset(),
		DepreciationSchedule(),
		MaintenanceOrder(),
	}
}
