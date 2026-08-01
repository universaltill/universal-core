// Package crm is the CRM module (reference-data-model.md §6,
// BACKLOG.md R20's B2B half) — currently just Case, the support and
// after-sales issue. A licensed module like purchasing/sales/hr, not
// part of every tenant's baseline.
//
// Lead, Opportunity, Contact and Campaign (§6's other rows) are board
// card #14 and land in this same package. Contact in particular is not
// an entity at all under this kernel's conventions: §6 itself says it
// is "Party (role=contact) linked to an Organization Party via
// PartyRelationship", which is the pattern ADR-0013 settled for
// Employee — worth deciding explicitly when #14 is picked up rather
// than discovering it mid-build.
//
// **Cross-module references.** Case points at Item and SalesOrder for
// product and warranty context, which live in the purchasing and sales
// modules. A tenant may license CRM without either. That is safe:
// entity.Definition.Validate checks a reference's shape, not whether
// its target is published (see the purchasing package's own note on
// resolving Status through the registry), so publishing this module
// alone works — the picker for an unpublished target simply has
// nothing to offer, and the field is optional precisely because a
// support case can legitimately concern no specific product or order.
//
// **Not enforced here**, stated in full because a partial list implies
// the rest are covered (ADR-0013's rule, learned the hard way in
// internal/kernel/hr):
//   - that a referenced Party holds the customer PartyRole — any Party
//     is accepted, the same gap sales.SalesOrder.customer_id has (#78);
//   - that the referenced SalesOrder actually belongs to the
//     referenced customer, so a case can cite another customer's order
//     as its warranty context (#78);
//   - that the referenced Item appears on that order;
//   - that any of those references points at a record that EXISTS —
//     a dangling id, or one whose entity type is not even published in
//     this tenant, is accepted (#78);
//   - that sla_due_at is in the future (it must not precede
//     opened_date — that part IS enforced, see below — but nothing
//     stops an SLA already past);
//   - that opened_date is not itself in the future: a case opened in
//     2099 saves;
//   - that assignee_id's Party holds any particular role;
//   - case_number uniqueness (#81).
//
// What IS enforced: the priority enum, the status graph below, and
// sla_due_at not preceding opened_date (on update as well as create).
package crm

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Case is a support or after-sales issue (reference-data-model.md §6's
// "Case / Ticket" row — one entity, since the two words name the same
// thing and picking both would give a tenant two places to file the
// same problem).
//
// sla_due_at is a FieldDate, so an SLA is tracked to the day rather
// than the hour. This kernel has no time-of-day type (the same
// limitation AttendanceRecord ran into with clock times, #17), and a
// string masquerading as a timestamp would be worse than an honest
// coarser one. A real hour-granularity SLA needs that type first.
func Case() *entity.Definition {
	return &entity.Definition{
		EntityType: "Case",
		Version:    1,
		Module:     "crm",
		// subject is the human-readable thing but it is free text of
		// arbitrary length; case_number is what an agent quotes on the
		// phone and what stays stable when the subject is edited. The
		// kernel's name/title convention matches neither field, so this
		// is declared rather than left to fall back to a raw id
		// (ADR-0013's closing rule, learned in #16).
		LabelField:     "case_number",
		StatusTypeCode: "case_status",
		Fields: []entity.Field{
			{Name: "case_number", Type: entity.FieldString, Required: true},
			{Name: "subject", Type: entity.FieldString, Required: true},
			{Name: "customer_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			// Optional: plenty of cases are about an account or a
			// delivery rather than a specific product or order.
			{Name: "item_id", Type: entity.FieldReference, Target: "Item"},
			{Name: "sales_order_id", Type: entity.FieldReference, Target: "SalesOrder"},
			{Name: "priority", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"low", "normal", "high", "urgent"},
				Default:    "normal"},
			{Name: "opened_date", Type: entity.FieldDate, Required: true},
			{Name: "sla_due_at", Type: entity.FieldDate, NotBefore: "opened_date"},
			{Name: "description", Type: entity.FieldString},
			// The agent handling it — a Party, like every other person
			// reference outside the HR domain (ADR-0013 rule 4): support
			// is routinely handled by contractors and partners who hold
			// no employment record.
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// All returns every Definition this module adds.
func All() []*entity.Definition {
	return []*entity.Definition{
		Case(),
	}
}
