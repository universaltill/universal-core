// Package hr is the HR/HCM module (reference-data-model.md §7,
// BACKLOG.md R22) — Employee, LeaveRequest and AttendanceRecord. A
// licensed module like
// purchasing/sales/assets/projects. Department and Position already
// live in internal/kernel/foundation (shipped 2026-07-30 with the
// org-chart work) and are referenced from here rather than moved:
// relocating a published Definition would cost a version bump and a
// migration for no behavioural gain.
//
// **Employee is an employment record, not a person** — ADR-0013. The
// person lives on Party and nowhere else; this entity carries only the
// employment: which position, which department, hired when, still
// employed or not. That is what lets one human hold two employments
// over time (a rehire is a second record, not an edit that destroys the
// first one's history) without ever duplicating their name or contact
// details.
//
// Read ADR-0013 before adding a reference to "the employee" from a new
// module. Its rule 4 is the one that catches people out: HR-domain
// records (LeaveRequest and AttendanceRecord here, Payslip later)
// reference **Employee**, because they are about the employment
// relationship; cross-domain records reference **Party**, because a
// contractor who is not an employee can still log project time or be
// assigned a task. projects.TimeEntry.employee_id pointing at Party is
// therefore correct and deliberate, not an inconsistency to "fix".
//
// **Not enforced here.** Listed in full rather than partially, because
// enumerating some invariants makes a reader assume the rest are
// covered — the independent review found four more beyond the three
// this list originally had, every one of them accepted by the live
// engine:
//
//   - the referenced Party actually holding the `employee` PartyRole;
//
//   - an Employee's department_id agreeing with its position's own;
//
//   - a Party having at most one ACTIVE employment at a time (two are
//     accepted, which #16's own test relies on for the rehire case —
//     but two SIMULTANEOUS active ones are equally accepted);
//
//   - employee_number being unique (nothing in entity.Field expresses
//     uniqueness; "natural key" below is a convention, not a
//     guarantee);
//
//   - `days` being positive, or agreeing with the start/end span: -99
//     days and 500 days over a two-day span both save cleanly. The
//     same is true of AttendanceRecord.hours_worked — a negative or
//     30-hour day saves. entity.Field has no Min/Max (#80);
//
//   - leave falling inside its employment's hire/end window: leave
//     dated 2030 against an employment that ended in 2020 saves;
//
//   - employee_id pointing at an Employee that exists at all — a
//     dangling reference is accepted (#78).
//
//   - attendance falling inside its employment's hire/end window, or
//     against an employment that has not been terminated: a 1999 row
//     against a 2024 hire saves;
//
//   - entry_date not being in the future.
//
// What IS enforced: the leave_type and attendance source enums, and
// both NotBefore date chains (an employment ending before it started,
// or leave ending before it starts, are rejected).
//
// AttendanceRecord is deliberately NOT surfaced as a related list on
// Employee, unlike LeaveRequest. Adding one would change Employee's
// published Definition and therefore require a v2 — and #20 showed
// that a version bump has to carry the FORM too or it silently
// delivers nothing to existing tenants. That is real cost and churn
// for a screen convenience, one card after Employee v1 shipped, and
// this card's own brief says "minimal shape". Attendance is reachable
// from its own list page filtered by employee. When Employee next
// needs a version bump for a substantive reason, the attendance
// related list should ride along with it.
//
// Deliberately NOT in this module yet: PayrollRun/Payslip (§7's last
// row — payroll calculation is explicitly out of R22's scoped-down
// phase, and it is country-specific, so per jurisdiction it is plugin
// work), and leave-balance accrual — LeaveRequest records a request and its approval, and a
// balance engine needs policy (carry-over, accrual rate, public
// holidays) that is per-tenant and per-jurisdiction.
package hr

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Employee is one employment relationship (reference-data-model.md §7,
// ADR-0013). party_id is required and is the ONLY link to who the
// person is — no name, no contact, no language: those are Party's, and
// duplicating them here is the failure mode this shape exists to
// prevent.
func Employee() *entity.Definition {
	return &entity.Definition{
		EntityType: "Employee",
		Version:    1,
		Module:     "hr",
		// Employee has no name of its own by design (ADR-0013 — the
		// person's name is Party's), which without this made every
		// picker, list cell and export show a raw UUID and made the
		// picker unsearchable: it can only sort/filter by a field it
		// knows is the label (independent review). employee_number is
		// what a payroll clerk actually calls the record.
		LabelField:     "employee_number",
		StatusTypeCode: "employee_status",
		Fields: []entity.Field{
			// employee_number is the identifier payroll and badges use,
			// distinct from the Party's own identity. Conventionally
			// unique per tenant, but nothing enforces that — see the
			// package comment; entity.Field has no Unique.
			{Name: "employee_number", Type: entity.FieldString, Required: true},
			{Name: "party_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "position_id", Type: entity.FieldReference, Target: "Position"},
			// department_id is carried directly rather than derived through
			// position_id: an employee can be seconded to a department that
			// isn't their position's, and a headcount-by-department report
			// shouldn't need a join through the org chart to answer. The two
			// can disagree; nothing enforces agreement yet (see the package
			// comment and #78).
			{Name: "department_id", Type: entity.FieldReference, Target: "Department"},
			{Name: "hire_date", Type: entity.FieldDate, Required: true},
			// end_date is when THIS employment ended. A rehire is a new
			// Employee record against the same Party (ADR-0013), so this
			// never needs clearing to bring someone back.
			{Name: "end_date", Type: entity.FieldDate, NotBefore: "hire_date"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			// Related list, not composition: a leave request records
			// something that was asked and decided, and it outlives the
			// employment — someone's leave history must not vanish because
			// they left. Same call TimeEntry gets on Task.
			{Name: "leave", Kind: entity.RelationRelatedList, Target: "LeaveRequest", ParentField: "employee_id"},
		},
	}
}

// LeaveRequest is time off asked for and decided
// (reference-data-model.md §7). Its status graph is an approval chain —
// unlike Employee's, which is a lifecycle — and it is the R9 approval
// workflow's intended subject: the routing built in Phase 2
// (department-scoped approval, delegation, escalation) applies to this
// entity without anything new here, because that machinery is generic
// over entity type.
//
// employee_id references Employee, not Party: leave accrues against an
// employment (ADR-0013 rule 4). Someone with two employments over time
// has two separate leave histories, which is correct.
func LeaveRequest() *entity.Definition {
	return &entity.Definition{
		EntityType:     "LeaveRequest",
		Version:        1,
		Module:         "hr",
		StatusTypeCode: "leave_request_status",
		Fields: []entity.Field{
			{Name: "request_number", Type: entity.FieldString, Required: true},
			{Name: "employee_id", Type: entity.FieldReference, Required: true, Target: "Employee"},
			{Name: "leave_type", Type: entity.FieldEnum, Required: true,
				// Jurisdiction-invariant categories only. Which of these a
				// tenant actually grants, how much, and how it accrues is
				// per-country policy — plugin territory, not an enum value
				// to guess at here.
				EnumValues: []string{"annual", "sick", "unpaid", "parental", "other"},
				Default:    "annual"},
			{Name: "start_date", Type: entity.FieldDate, Required: true},
			{Name: "end_date", Type: entity.FieldDate, Required: true, NotBefore: "start_date"},
			// Days rather than a computed span: half-days, public holidays
			// and non-working days make "end minus start" wrong in every
			// jurisdiction, and the correct calculation is exactly the
			// per-country policy this module doesn't own.
			{Name: "days", Type: entity.FieldNumber, Required: true},
			{Name: "reason", Type: entity.FieldString},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// AttendanceRecord is one day's attendance for one employment
// (reference-data-model.md §7) — the "clock in/out or timesheet
// source" row, kept to the minimal shape this card asked for: no
// payroll calculation engine, and no approval lifecycle, because
// attendance is a record of what happened rather than a request
// somebody decides on (LeaveRequest is the entity in this module that
// has an approval chain, and the difference is deliberate).
//
// employee_id references Employee, not Party (ADR-0013 rule 4):
// attendance is owed under an employment, so a person's two
// employments have two separate attendance histories.
//
// `source` records where the row came from rather than modelling clock
// times as fields. A minimal shape that stores hours_worked can be fed
// by a badge reader, an imported timesheet or a manual correction, and
// which of those it was matters for trusting the number; modelling
// clock_in/clock_out properly needs a time-of-day type this kernel
// does not have (FieldDate is a date), and faking it with strings
// would be worse than leaving it out.
//
// hours_worked has no upper or lower bound: entity.Field cannot
// express one yet (#80), so a negative or 30-hour day saves cleanly.
// Listed in this package's "Not enforced" comment with the rest.
//
// Unique: [{employee_id, entry_date}] closes uc-infra#81 — one attendance
// row per employee per day, enforced by crud.Engine (ADR-0018 §3(c), a
// record_unique_keys side table with a real Postgres UNIQUE index, not a
// best-effort Go-side check alone). Version bumped 1→2 for it: an
// existing tenant needs cmd/sync-tenant-modules to warn about any
// already-colliding rows before this starts rejecting their next edit
// (ADR-0018's Consequences), same as any other constraint-tightening
// version bump.
func AttendanceRecord() *entity.Definition {
	return &entity.Definition{
		EntityType: "AttendanceRecord",
		Version:    2,
		Module:     "hr",
		// Like Employee, this has no name — ADR-0013's closing rule says
		// such an entity must declare its own label rather than fall
		// back to a raw id. Nothing references AttendanceRecord today,
		// so there is no visible symptom yet; declaring it now costs a
		// line and means the first thing that does reference it works.
		LabelField: "entry_date",
		Fields: []entity.Field{
			{Name: "employee_id", Type: entity.FieldReference, Required: true, Target: "Employee"},
			{Name: "entry_date", Type: entity.FieldDate, Required: true},
			{Name: "hours_worked", Type: entity.FieldNumber, Required: true},
			{Name: "source", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"clock", "timesheet", "manual"},
				Default:    "timesheet"},
			{Name: "notes", Type: entity.FieldString},
		},
		Unique: [][]string{{"employee_id", "entry_date"}},
	}
}

// All returns every Definition this module adds.
func All() []*entity.Definition {
	return []*entity.Definition{
		Employee(),
		LeaveRequest(),
		AttendanceRecord(),
	}
}
