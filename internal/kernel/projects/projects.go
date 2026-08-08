// Package projects is the Projects module (reference-data-model.md §8,
// BACKLOG.md R8) — Project, Task, TimeEntry and ProjectBudgetLine. A
// licensed module like purchasing/sales/finance/assets, published by
// cmd/provision-tenant only for a tenant that has it.
//
// **Two gaps this module used to have, now closed by declarative
// constraints on the fields themselves (uc-infra#78)**, kept documented
// here because they're what makes the field declarations below read
// correctly:
//   - TimeEntry.employee_id and Task.assignee_id each declare a
//     TargetFilter requiring the referenced Party to actually hold the
//     `employee` PartyRole (foundation.PartyRole) — a pure vendor
//     organization is no longer accepted, and the reference picker no
//     longer offers one. SalesOrder.customer_id/PurchaseOrder.vendor_id
//     got the equivalent customer/vendor role filters in the same
//     change.
//   - Task.parent_task_id declares MustMatchParentField: "project_id" —
//     a parent task must be in the SAME project as the child, checked
//     by crud.Engine on every create/update and by the reference
//     picker's own search, not just the pre-existing cycle guard.
//
// Both are generic, Definition-declared mechanisms
// (entity.Field.TargetFilter/MustMatchParentField) evaluated by
// crud.Engine and the reference-search endpoint — nothing here or in
// those engines special-cases "Task" or "employee" by name; see
// entity.TargetFilterCondition's own doc comment for the mechanism.
//
// **TimeEntry and Task reference Party, not hr.Employee — and that is
// deliberate (ADR-0013 rule 4).** An Employee entity now exists (the HR
// module, board #16), and it is an employment RECORD keyed by party_id
// rather than a person master. HR-domain records reference it; these
// two do not, because a contractor who is not an employee can log
// project time and be assigned a task. Retargeting them would forbid a
// real case and cost a v2 Definition plus a migration of every logged
// hour.
//
// The rest of this note is the reasoning ADR-0013 records, kept because
// it is what makes rule 4 make sense: this kernel models a person's
// employment as a Party holding the `employee` PartyRole —
// the same Party-Role pattern vendor_id and customer_id already use,
// documented in foundation.go's own PartyRole comment ("a person can be
// both: an 'employee' PartyRole and…"). So employee_id and assignee_id
// target Party. Inventing a parallel Employee master table here is
// precisely the ERP failure mode reference-data-model.md's "Notes for
// the ADR" warns about: the same human duplicated across finance, sales
// and HR because each module grew its own.
//
// That question — raised here by #18's independent review — was settled
// by ADR-0013 when #16 shipped: Employee exists, carries only the
// employment, and these two fields stay on Party. No migration was
// needed.
//
// **ProjectBudgetLine** (§8's fourth row, uc-infra#79) ships in this
// slice as planning-only: category + planned_amount, composed under
// Project exactly as POLine composes under PurchaseOrder. It
// deliberately has NO computed "actual" column and NO currency_id of
// its own:
//   - An "actual" needs to price TimeEntry's hours and/or a non-labour
//     expense record. At the time this entity shipped, neither existed:
//     no employee cost-rate concept anywhere in this kernel, and no
//     non-labour expense source (VendorInvoice/GoodsReceiptLine still
//     carry no project dimension). #79's own body warned against
//     shipping a partial actual that "silently compares planned cost
//     against labour-only actuals" and "reads as complete while being
//     wrong in the direction that matters" — filed as uc-infra#134
//     rather than guessed at here. uc-infra#134 has since shipped the
//     labour half (Employee.cost_rate, internal/data/reporting.go's
//     ProjectBudgetActuals) — deliberately as a REPORT-time computed
//     value, never a field on THIS entity, which is why ProjectBudgetLine
//     itself is still exactly the 3 fields below (see this package's own
//     TestProjectBudgetLine_HasNoCurrencyOrComputedActual, still
//     unmodified) and why the warning above still applies undiminished
//     to the non-labour categories: ProjectBudgetActuals reports those
//     as "not available" (nil), not a computed 0, for precisely the
//     reason this paragraph originally named.
//   - No currency_id: every OTHER composition child line-item in this
//     kernel (POLine, SOLine, GoodsReceiptLine) has none either — the
//     parent document (here, Project.currency_id, already a field)
//     carries it once, not once per line. Adding one here would be a
//     new, one-off convention with no precedent.
//   - Nothing rejects a negative planned_amount: entity.Field has no
//     Min/Max concept yet (tracked as uc-infra#80, same pre-existing gap
//     POLine.qty/unit_price already have — not a regression this entity
//     introduces), and a negative budget line is a more obviously wrong
//     record than a negative quantity, so worth naming explicitly here
//     rather than only in #80's own tracking.
//   - Project.budget is unchanged and NOT derived from the lines
//     (matches ProjectForm's own doc comment on the Tasks section:
//     summing children onto the parent creates a second source of
//     truth the moment one is edited outside the form). Budget lines
//     are an advisory breakdown of the single number Project already
//     commits to, not a replacement for it.
//
// Deliberately NOT in this slice:
//   - **Overrun prediction (R10)**: the card says so explicitly. This
//     module is the history such a model would learn from, which is why
//     estimated_hours and logged hours are both first-class.
//   - **Billing**: TimeEntry carries `billable` so the data is there,
//     but turning logged hours into an invoice is a Sales concern and a
//     posting decision, not a field on this entity.
package projects

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Project is a container for work (reference-data-model.md §8).
// customer_id is optional because internal projects are as real as
// billable ones; when set it targets Party, holding the customer
// PartyRole, exactly as SalesOrder.customer_id does.
func Project() *entity.Definition {
	return &entity.Definition{
		EntityType: "Project",
		// Version 2 (uc-infra#80): budget gained a Min:0 bound.
		Version:        2,
		Module:         "projects",
		StatusTypeCode: "project_status",
		Fields: []entity.Field{
			// project_code is the natural key — same application-level
			// convention as po_number/so_number/asset_number.
			{Name: "project_code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldI18nText, Required: true},
			{Name: "customer_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "start_date", Type: entity.FieldDate, Required: true},
			// A project's end date cannot precede its start — unlike an
			// asset's completed_date, which legitimately can precede its
			// schedule, this is a span rather than a plan-versus-actual.
			{Name: "end_date", Type: entity.FieldDate, NotBefore: "start_date"},
			{Name: "budget", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			// Composition: a task has no meaning without its project, and
			// the project form edits its task list in place — the same
			// call POLine/SOLine make. Note the practical difference
			// between the two kinds here is RENDERING only: crud.Engine
			// soft-deletes exactly one record and cascades nothing, so
			// composition does not actually propagate a delete (verified
			// by independent review). Task is also an unusual
			// composition child — it has its own status graph, its own
			// related list and a self-hierarchy, where POLine and
			// friends have none of those — but the master-detail editing
			// experience is what a project's task list wants.
			{Name: "tasks", Kind: entity.RelationComposition, Target: "Task", ParentField: "project_id"},
			// Composition, same reasoning as tasks above: a budget line
			// is planning detail for exactly one project and has no
			// meaning detached from it, and ProjectForm edits the
			// breakdown in place — the same call POLine/SOLine make
			// under their own parents.
			{Name: "budget_lines", Kind: entity.RelationComposition, Target: "ProjectBudgetLine", ParentField: "project_id"},
		},
	}
}

// Task is a unit of work (reference-data-model.md §8), hierarchical via
// parent_task_id. That self-reference is safe without anything extra
// here: ADR-0007 put cycle detection in crud.Engine generically, over
// any FieldReference whose Target is its own EntityType, precisely so a
// new hierarchy like this one inherits the guard rather than
// re-implementing (or forgetting) it. parent_task_id's
// MustMatchParentField: "project_id" (v2, uc-infra#78) closes the
// remaining gap the cycle guard alone didn't: a parent task must be in
// the SAME project as its child, checked generically by crud.Engine and
// the reference picker's own search, not just here in a comment.
//
// assignee_id targets Party, not hr.Employee — see this package's doc
// comment and ADR-0013 rule 4: a contractor who holds no employment can
// still be assigned a task. Its TargetFilter (v2, uc-infra#78) requires
// the referenced Party to actually hold the employee PartyRole — a
// contractor still qualifies as long as they hold that role
// (foundation.go's PartyRole comment: a Party can hold several roles at
// once), but a pure vendor/customer Party no longer does.
func Task() *entity.Definition {
	return &entity.Definition{
		EntityType: "Task",
		// Version 3 (uc-infra#80): estimated_hours gained a Min:0 bound.
		Version:        3,
		Module:         "projects",
		StatusTypeCode: "task_status",
		Fields: []entity.Field{
			{Name: "project_id", Type: entity.FieldReference, Required: true, Target: "Project"},
			{Name: "parent_task_id", Type: entity.FieldReference, Target: "Task", MustMatchParentField: "project_id"},
			{Name: "title", Type: entity.FieldI18nText, Required: true},
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Party", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
			}},
			{Name: "estimated_hours", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "due_date", Type: entity.FieldDate},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			// Related list, not composition: a time entry is a record of
			// something a person did and it outlives the task's own
			// lifecycle — closing a task must not make the hours logged
			// against it (possibly already billed) disappear from view.
			// Same call MaintenanceOrder gets on FixedAsset. This is a
			// statement about ownership and presentation, not about
			// cascade: the engine deletes one record at a time either
			// way (see the composition note above).
			{Name: "time_entries", Kind: entity.RelationRelatedList, Target: "TimeEntry", ParentField: "task_id"},
		},
	}
}

// TimeEntry is logged effort (reference-data-model.md §8) — the entity
// that makes a project's actuals real, and the history R10's overrun
// prediction would learn from.
//
// hours is a plain FieldNumber rather than minutes-as-integer: unlike
// money, a fractional hour has no exactness requirement that float
// storage threatens, and everyone who enters time thinks in "1.5".
//
// Bounded to [0, 24] (uc-infra#80, ADR-0018 §Consequences: "hours within
// a day") — one logged entry cannot exceed a calendar day.
func TimeEntry() *entity.Definition {
	return &entity.Definition{
		EntityType: "TimeEntry",
		// Version 3 (uc-infra#80): hours gained a [0, 24] bound.
		Version: 3,
		Module:  "projects",
		Fields: []entity.Field{
			{Name: "task_id", Type: entity.FieldReference, Required: true, Target: "Task"},
			// The person who did the work: a Party holding the employee
			// PartyRole (see the package comment). TargetFilter (v2,
			// uc-infra#78) enforces that, not just documents it: a pure
			// vendor Party can no longer be logged against, and the
			// reference picker no longer offers one.
			{Name: "employee_id", Type: entity.FieldReference, Required: true, Target: "Party", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "PartyRole", EntityField: "party_id", Field: "role_type", Value: "employee"},
			}},
			{Name: "entry_date", Type: entity.FieldDate, Required: true},
			{Name: "hours", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0), Max: entity.Float64Ptr(24)},
			// billable is the flag project billing will read; whether an
			// hour becomes an invoice line is a Sales decision, not this
			// entity's.
			{Name: "billable", Type: entity.FieldBool, Default: true},
			{Name: "notes", Type: entity.FieldString},
		},
	}
}

// ProjectBudgetLine is planned cost by category (reference-data-model.md
// §8, uc-infra#79) — the fourth Projects entity, composed under Project.
// See this package's doc comment for why it ships planning-only: no
// computed actual, no currency_id of its own.
func ProjectBudgetLine() *entity.Definition {
	return &entity.Definition{
		EntityType: "ProjectBudgetLine",
		Version:    1,
		Module:     "projects",
		Fields: []entity.Field{
			{Name: "project_id", Type: entity.FieldReference, Required: true, Target: "Project"},
			// A closed list rather than free text: a category is only
			// useful as a roll-up/reporting dimension if a report can
			// group by it without normalizing free-text spelling first.
			// "labour" is the one category TimeEntry can eventually
			// price against once a cost-rate concept exists (see the
			// package doc comment); the rest are placeholders for the
			// non-labour costs this kernel has no expense entity for
			// yet.
			{Name: "category", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"labour", "materials", "travel", "equipment", "other"},
				Default:    "labour"},
			{Name: "planned_amount", Type: entity.FieldNumber, Required: true, Default: float64(0)},
		},
	}
}

// All returns every Definition this module adds — the set a tenant gets
// once Projects is one of their licensed modules (seed.go's Publish).
func All() []*entity.Definition {
	return []*entity.Definition{
		Project(),
		Task(),
		TimeEntry(),
		ProjectBudgetLine(),
	}
}
