// Package finance is the third *operational* module built on top of the
// foundation layer (internal/kernel/foundation) — the base
// chart-of-accounts scaffolding (reference-data-model.md §1) real
// postings will eventually flow through. Like purchasing/sales, this is
// NOT part of every tenant's baseline (ADR-0001 §8) — a tenant only gets
// these once Finance is one of their licensed modules.
//
// Scoped deliberately narrow for this first slice: Account, FiscalYear,
// Period, TaxCode, CostCenter — the reference/master-data shape a real
// posting needs to point at. Deliberately NOT built in this pass, real
// future work not forgotten: JournalEntry/JournalLine as the
// CRUD-visible record of what internal/kernel/ledger posts, the actual
// wiring that calls ledger from CustomerInvoice/GoodsReceipt, period
// close enforcement (Period.status exists here but nothing rejects a
// posting into a closed period yet — that needs JournalEntry to exist
// first), Budget/BudgetLine, BankAccount/BankStatementLine reconciliation,
// FixedAsset (its own module, Assets/EAM). See erp/BACKLOG-TASKS.md
// Phase 1 for the ordered remainder.
//
// TaxCode here is reference/master data only (a code+rate+type a future
// invoice line can point at) — not a tax *computation engine*. Per
// universal-core/CLAUDE.md's plugin-first rule, actual jurisdiction tax
// logic (which code applies to which transaction, country-specific
// rules) belongs in a plugin, never core; this entity is the stable
// thing a plugin (or a human) assigns, not the thing that decides it.
package finance

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Account is one node in the chart of accounts (reference-data-model.md
// §1). parent_account_id is a self-reference (RelationReference, not
// composition — an account's children are independently
// creatable/editable, not saved atomically with the parent the way a
// composition child would be), the same hierarchical-self-reference
// shape reference-data-model.md already uses for ProductCategory.
func Account() *entity.Definition {
	return &entity.Definition{
		EntityType: "Account",
		// v2 (uc-infra#204 independent review, finding 3): code is
		// gl_accounts' own natural key (UpsertByCode) — two Account
		// records sharing a code used to silently clobber each other's
		// projected gl_accounts row with no trace, once
		// SyncGLAccountOnWrite (seed.go) made real writes actually reach
		// that projection. Unique now enforces it the same way
		// PurchaseOrder.po_number/InventoryItem's own natural keys
		// already are — see entity.Definition's Unique field.
		Version: 2,
		Module:  "finance",
		Fields: []entity.Field{
			// code is the natural key (chart-of-accounts numbering, e.g.
			// "1000", "4000") — real DB-level uniqueness as of v2 above
			// (record_unique_keys), not just an application convention.
			//
			// NOT enforced immutable once written: renaming an existing
			// Account's code is still allowed, and SyncGLAccountOnWrite
			// upserts gl_accounts by the NEW code — the OLD code's
			// gl_accounts row is orphaned (left behind, still active,
			// no longer reachable from any Account), not renamed or
			// deactivated in place, because gl_accounts carries no link
			// back to the Account record it came from to find it by.
			// Real gap (uc-infra#204 independent review, finding 2),
			// deliberately not closed in this change — closing it
			// properly needs gl_accounts to gain a source-record
			// linkage (a migration) or Account.code to become
			// immutable-after-create, either of which is a bigger,
			// separable change; tracked as its own backlog card rather
			// than expanding this one.
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"asset", "liability", "equity", "income", "expense"}},
			{Name: "parent_account_id", Type: entity.FieldReference, Target: "Account"},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "is_active", Type: entity.FieldBool, Default: true},
		},
		Unique: [][]string{{"code"}},
	}
}

// FiscalYear is the top of the accounting calendar (reference-data-model.md
// §1) — Period below belongs to one. status is a plain FieldEnum, not
// the foundation Status/StatusType pattern: there's no approval workflow
// over a fiscal year's own lifecycle yet (unlike PurchaseOrder/SalesOrder,
// which opted into StatusType from day one because they have a real
// multi-step transition graph) — open/closed is a two-state flag. If a
// real close-workflow requirement lands later (BACKLOG-TASKS.md's
// "Period close" task), migrate then, against a concrete need, not now
// against a hypothetical one.
func FiscalYear() *entity.Definition {
	return &entity.Definition{
		EntityType: "FiscalYear",
		Version:    1,
		Module:     "finance",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "start_date", Type: entity.FieldDate, Required: true},
			{Name: "end_date", Type: entity.FieldDate, Required: true},
			{Name: "status", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"open", "closed"}, Default: "open"},
		},
	}
}

// Period is one accounting period within a FiscalYear (reference-data-
// model.md §1) — a plain reference to FiscalYear (not composition: a
// period is independently listable/editable, and reference-data-model.md
// itself describes this as "Period belongs to FiscalYear," the ordinary
// reference relationship, not master-detail). status has a third state
// ("locked") beyond FiscalYear's open/closed — reference-data-model.md's
// own field list for Period names all three; "locked" is the harder stop
// a period-close workflow will eventually set (BACKLOG-TASKS.md), left
// as a declared-but-currently-unenforced state here, same "declare the
// shape now, enforce it once the real caller exists" approach the
// Account/FiscalYear fields above take.
func Period() *entity.Definition {
	return &entity.Definition{
		EntityType: "Period",
		Version:    1,
		Module:     "finance",
		Fields: []entity.Field{
			{Name: "fiscal_year_id", Type: entity.FieldReference, Required: true, Target: "FiscalYear"},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "start_date", Type: entity.FieldDate, Required: true},
			{Name: "end_date", Type: entity.FieldDate, Required: true},
			{Name: "status", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"open", "closed", "locked"}, Default: "open"},
		},
	}
}

// TaxCode is a tax rate + rule reference (reference-data-model.md §1) —
// master data an invoice/PO/SO line can point at, not a computation
// engine (see this file's own package doc comment on the plugin-first
// boundary this entity deliberately stays on the core side of).
func TaxCode() *entity.Definition {
	return &entity.Definition{
		EntityType: "TaxCode",
		// Version 2 (uc-infra#80): rate gained a Min:0 bound — a negative
		// tax rate has no meaning. Deliberately no Max: unlike
		// crm.Opportunity.probability, a rate's upper bound is jurisdiction
		// policy (compound/luxury rates can legitimately exceed 100 in
		// some tax regimes), not a kernel-invariant ceiling — plugin
		// territory per this repo's own plugin-first rule, not something
		// to cap here.
		Version: 2,
		Module:  "finance",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			// rate is a percentage (e.g. 5.0 for 5%), not a fraction —
			// matches how a human enters/reads a tax rate; whichever code
			// eventually computes an actual tax amount divides by 100
			// itself rather than this field storing a pre-divided fraction
			// that would look wrong rendered directly in a form.
			{Name: "rate", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			{Name: "tax_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"vat", "withholding", "sales"}},
			{Name: "jurisdiction", Type: entity.FieldString},
		},
	}
}

// CostCenter is a cost/profit allocation dimension (reference-data-model.md
// §1, listed there as "CostCenter / Dimension"). Modeled as one entity
// for this first slice, not a separate generic "Dimension" abstraction —
// reference-data-model.md's own text calls CostCenter and Dimension the
// same idea, and this repo's own CLAUDE.md warns against building an
// abstraction for a second case that doesn't exist yet; generalize if a
// genuinely different second dimension type (e.g. Project) needs the same
// mechanism later, against that real example, not speculatively now.
func CostCenter() *entity.Definition {
	return &entity.Definition{
		EntityType: "CostCenter",
		Version:    1,
		Module:     "finance",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "type", Type: entity.FieldString},
		},
	}
}

// All returns every Definition this module adds — the set a tenant gets
// once Finance is one of their licensed modules (seed.go's Publish).
func All() []*entity.Definition {
	return []*entity.Definition{
		Account(),
		FiscalYear(),
		Period(),
		TaxCode(),
		CostCenter(),
	}
}
