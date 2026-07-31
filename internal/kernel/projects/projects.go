// Package projects is the Projects module (reference-data-model.md §8,
// BACKLOG.md R8) — Project, Task and TimeEntry. A licensed module like
// purchasing/sales/finance/assets, published by cmd/provision-tenant
// only for a tenant that has it.
//
// **Two things this module does NOT enforce**, stated up front because
// the reference types below read as if it might:
//   - A Party referenced as assignee_id/employee_id is not checked to
//     actually hold the `employee` PartyRole. FieldReference carries
//     only a Target, so any Party — including a pure vendor
//     organization — is accepted, and the picker offers them. Same gap
//     customer_id and vendor_id already have; it needs a declarative
//     role filter on FieldReference, filed on the board rather than
//     special-cased here.
//   - A Task's parent_task_id is not constrained to a task in the SAME
//     project. Nothing walks the parent chain today except the cycle
//     guard, so the damage is latent, but a cross-project subtree would
//     misattribute work to the wrong project in any future roll-up or
//     tree view. Also filed — it needs the same kind of "reference must
//     agree with a field of mine" constraint the kernel lacks.
//
// **There is no Employee entity, and this module does not add one.**
// §8's rows say TimeEntry "references Employee", but this kernel models
// a person's employment as a Party holding the `employee` PartyRole —
// the same Party-Role pattern vendor_id and customer_id already use,
// documented in foundation.go's own PartyRole comment ("a person can be
// both: an 'employee' PartyRole and…"). So employee_id and assignee_id
// target Party. Inventing a parallel Employee master table here is
// precisely the ERP failure mode reference-data-model.md's "Notes for
// the ADR" warns about: the same human duplicated across finance, sales
// and HR because each module grew its own.
//
// That decision reaches beyond this module: board card #16
// (Employee/Department/Position) currently plans to add a real
// foundation.Employee, and reference-data-model.md §7 gives it
// attributes (hire_date, employment_status, position_id) that have
// nowhere to live on Party or PartyRole. If #16 does add one,
// TimeEntry.employee_id and Task.assignee_id must be retargeted — a
// v2 Definition plus a data migration for every logged hour. Flagged on
// #16 so that cycle decides it deliberately instead of rediscovering
// the conflict (independent review).
//
// Deliberately NOT in this slice:
//   - **ProjectBudgetLine** (§8's fourth row): planned cost by category,
//     rolling up against TimeEntry and expense actuals. It needs an
//     expense concept this kernel doesn't have yet, so a budget line
//     could only roll up against half its inputs — filed rather than
//     half-built.
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
		EntityType:     "Project",
		Version:        1,
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
			{Name: "budget", Type: entity.FieldNumber, Default: float64(0)},
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
		},
	}
}

// Task is a unit of work (reference-data-model.md §8), hierarchical via
// parent_task_id. That self-reference is safe without anything extra
// here: ADR-0007 put cycle detection in crud.Engine generically, over
// any FieldReference whose Target is its own EntityType, precisely so a
// new hierarchy like this one inherits the guard rather than
// re-implementing (or forgetting) it.
//
// assignee_id targets Party — see this package's doc comment on why
// there is no Employee entity to point at.
func Task() *entity.Definition {
	return &entity.Definition{
		EntityType:     "Task",
		Version:        1,
		Module:         "projects",
		StatusTypeCode: "task_status",
		Fields: []entity.Field{
			{Name: "project_id", Type: entity.FieldReference, Required: true, Target: "Project"},
			{Name: "parent_task_id", Type: entity.FieldReference, Target: "Task"},
			{Name: "title", Type: entity.FieldI18nText, Required: true},
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "estimated_hours", Type: entity.FieldNumber, Default: float64(0)},
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
func TimeEntry() *entity.Definition {
	return &entity.Definition{
		EntityType: "TimeEntry",
		Version:    1,
		Module:     "projects",
		Fields: []entity.Field{
			{Name: "task_id", Type: entity.FieldReference, Required: true, Target: "Task"},
			// The person who did the work: a Party holding the employee
			// PartyRole (see the package comment).
			{Name: "employee_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "entry_date", Type: entity.FieldDate, Required: true},
			{Name: "hours", Type: entity.FieldNumber, Required: true},
			// billable is the flag project billing will read; whether an
			// hour becomes an invoice line is a Sales decision, not this
			// entity's.
			{Name: "billable", Type: entity.FieldBool, Default: true},
			{Name: "notes", Type: entity.FieldString},
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
	}
}
