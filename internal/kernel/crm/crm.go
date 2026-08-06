// Package crm is the CRM module (reference-data-model.md §6,
// BACKLOG.md R20's B2B half): Campaign, Lead and Opportunity — the
// sales pipeline — plus Case, the support and after-sales issue. A
// licensed module like purchasing/sales/hr, not part of every tenant's
// baseline.
//
// **There is no Contact entity, and there never will be one** unless
// something gives it attributes of its own (ADR-0014). §6's Contact row
// describes a modelling instruction rather than fields: a contact is a
// person Party holding the `contact` PartyRole, joined to an
// organization Party by a `contact_for` PartyRelationship. Every
// attribute a contact has already lives somewhere — name and language
// on Party, phone and email on ContactMechanism, postal address on
// Address — so an entity would have no fields of its own and would be
// a second person master wearing a CRM label. This is the opposite
// conclusion ADR-0013 reached for Employee, from the same test:
// Employee earned an entity by having employment attributes
// (hire_date, position_id) that fit nowhere else. Contact has none.
//
// The consequence worth knowing before you look for it: **a job title
// at the organization has nowhere to live.** PartyRelationship carries
// no attributes, so "Jane is Head of Procurement at Acme" records only
// that Jane is a contact at Acme. Attributed relationships are a
// foundation change, filed separately.
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
// Campaign, Lead and Opportunity reference only foundation (Party,
// Currency, Status) and this package — the pipeline half of the module
// is standalone by construction, unlike Case.
//
// **Not enforced here**, stated in full because a partial list implies
// the rest are covered (ADR-0013's rule, learned the hard way in
// internal/kernel/hr):
//
// Shared by every reference in this package except status_id (which
// ValidateStatusTransition does check, both for existence and for
// membership of the right status type):
//   - that any reference points at a record that EXISTS — a dangling
//     id, or one whose entity type is not even published in this
//     tenant, is accepted (#78);
//   - that a referenced Party holds the role its field name implies:
//     Case.customer_id, Opportunity.customer_id, Lead.owner_id,
//     Opportunity.owner_id and Case.assignee_id all take any Party at
//     all, the same gap sales.SalesOrder.customer_id has (#78).
//
// Case:
//   - that the referenced SalesOrder actually belongs to the
//     referenced customer, so a case can cite another customer's order
//     as its warranty context (#78);
//   - that the referenced Item appears on that order;
//   - that sla_due_at is in the future (it must not precede
//     opened_date — that part IS enforced — but nothing stops an SLA
//     already past);
//   - that opened_date is not itself in the future: a case opened in
//     2099 saves;
//   - case_number uniqueness (#81).
//
// Lead:
//   - any agreement between source and campaign_id. source="campaign"
//     with no campaign_id saves, and so does source="web" with one
//     (see Lead's own doc comment for why they are separate fields).
//   - that converted_party_id is set when the status reaches
//     `converted`, or unset when it has not. The status graph orders
//     the states; nothing ties the field to them.
//   - **anything at all about the SHAPE converted_party_id points at.**
//     ADR-0014 says a contact is a person Party, holding the `contact`
//     PartyRole, with a contact_for PartyRelationship to the
//     organization. None of those three is checked: an organization
//     Party with a `vendor` role and no relationship saves happily
//     (#78). cmd/seed-demo-data's seedPipeline is the only thing that
//     builds the intended shape, which is precisely why ADR-0013
//     clause 2 makes the demo tenant model it. Called out separately
//     from the role bullet above because this field's whole meaning is
//     that shape, so "no role check" understates it.
//   - email format, or that two enquiries from the same address are
//     one lead rather than two (#81).
//
// Opportunity:
//   - that expected_close_date is in the future, or after the
//     Lead it came from was created.
//   - any coherence between lead_id and the rest of the record: the
//     cited Lead need not be in `converted`, and its own
//     converted_party_id and company_name need have nothing to do with
//     this deal's customer_id. Same shape as Case's SalesOrder gap
//     above, and the same #78.
//
// Campaign:
//   - name uniqueness, on any of the three (#81).
//
// What IS enforced: every enum, the four status graphs seeded below
// (three added here plus Case's), the status_id checks noted above, the
// two NotBefore date orderings (Case.sla_due_at after opened_date,
// Campaign.end_date after start_date) — on update as well as create —
// and (uc-infra#80) Opportunity.probability's [0, 100] bound,
// Opportunity.amount's Min:0, and Campaign.budget's Min:0.
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

// Campaign is a marketing activity leads can be attributed to
// (reference-data-model.md §6). Labelled by `name` through the kernel's
// default name/title convention, so no explicit LabelField is needed.
//
// budget is a plain FieldNumber rather than integer minor units,
// matching SalesOrder.total and Opportunity.amount — consistency within
// the module beats correctness-in-isolation here, and the whole
// money-representation question is one change across every module, not
// something to start diverging on in a CRM card.
func Campaign() *entity.Definition {
	return &entity.Definition{
		EntityType: "Campaign",
		// Version 2 (uc-infra#80): budget gained a Min:0 bound.
		Version:        2,
		Module:         "crm",
		StatusTypeCode: "campaign_status",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "channel", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"email", "event", "web", "social", "print", "phone", "partner"}},
			{Name: "budget", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "start_date", Type: entity.FieldDate, Required: true},
			{Name: "end_date", Type: entity.FieldDate, NotBefore: "start_date"},
			{Name: "description", Type: entity.FieldString},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
		Relationships: []entity.Relationship{
			// Related list, not composition: a Lead exists perfectly well
			// without a Campaign (most do), and deleting a campaign must
			// not take its leads with it — which is exactly the
			// distinction R11a says to make explicitly rather than
			// default.
			{Name: "leads", Kind: entity.RelationRelatedList, Target: "Lead", ParentField: "campaign_id"},
		},
	}
}

// Lead is an unqualified prospect (reference-data-model.md §6) — someone
// who has shown interest but is not yet anyone in the party master.
//
// **name/company_name/email/phone are free text on purpose.** A lead is
// pre-Party by definition: the whole point is that a name arrived from a
// web form or a trade show and nobody has yet decided it is worth a
// record in the customer master. ContactMechanism cannot hold the email
// because it requires a party_id, and creating a Party for every
// unqualified lead is precisely the master-data pollution the Party
// pattern exists to avoid. On conversion these become a real Party plus
// its ContactMechanism rows, and converted_party_id records which one.
//
// **source and campaign_id are deliberately separate**, though §6
// compresses them into one column ("Campaign … referenced by
// Lead.source"). Taken literally that makes source a reference to
// Campaign, which has no value for the walk-in, the referral or the
// inbound web enquiry — most leads, in other words. So source is the
// enum of how the lead arrived and campaign_id is the optional link to
// a specific campaign. Nothing enforces that the two agree; see the
// package comment.
func Lead() *entity.Definition {
	return &entity.Definition{
		EntityType:     "Lead",
		Version:        1,
		Module:         "crm",
		StatusTypeCode: "lead_status",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "company_name", Type: entity.FieldString},
			{Name: "email", Type: entity.FieldString},
			{Name: "phone", Type: entity.FieldString},
			{Name: "source", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"web", "referral", "event", "campaign", "cold_call", "partner", "other"},
				Default:    "other"},
			{Name: "campaign_id", Type: entity.FieldReference, Target: "Campaign"},
			{Name: "owner_id", Type: entity.FieldReference, Target: "Party"},
			// Set when the lead becomes a real Party — the record of a
			// conversion that, for now, a human performs by hand (see
			// PublishStatuses' note on the `converted` state).
			{Name: "converted_party_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "notes", Type: entity.FieldString},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// Opportunity is a pipeline deal (reference-data-model.md §6).
//
// The deal's stage is the generic Status graph, not a plain FieldEnum —
// following SalesOrder's precedent of opting in from day one rather
// than paying for the enum-to-graph version bump later, which
// PurchaseOrder already did once in this codebase. §6 calls the field
// "stage"; it is status_id here because that is what the kernel's
// StatusTypeCode mechanism looks for, and the status type is named
// opportunity_stage so the vocabulary survives.
//
// lead_id is the single record of where a deal came from. Lead does not
// carry a converse converted_opportunity_id: two fields for one edge
// can disagree, and the child pointing at the parent is the direction
// every other reference in this codebase uses.
//
// **§6's "has many Quotations" is not built**, and is the third place
// this package departs from that table (the other two — Contact is not
// an entity, and source/campaign_id are separate fields — are argued
// where they apply). There is no Quotation entity anywhere in this
// codebase: purchasing has RequestForQuotation, which is the buy side
// and a different document. A sell-side quotation is its own card,
// with its own line items and its own relationship to SalesOrder, and
// inventing half of it here to satisfy a table cell would be worse
// than the gap.
func Opportunity() *entity.Definition {
	return &entity.Definition{
		EntityType: "Opportunity",
		// Version 2 (uc-infra#80): amount gained a Min:0 bound;
		// probability gained a [0, 100] bound (ADR-0018 §Consequences:
		// "a percentage within 0-100") — it is entered as a whole
		// percentage point, e.g. 25.0 for 25%, same convention as
		// finance.TaxCode.rate.
		Version:        2,
		Module:         "crm",
		StatusTypeCode: "opportunity_stage",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			// §6: "references Party (customer/prospect)" — the prospect
			// case is why this is a Party with the `prospect` PartyRole
			// rather than anything sales-side.
			{Name: "customer_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "lead_id", Type: entity.FieldReference, Target: "Lead"},
			{Name: "amount", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0)},
			{Name: "currency_id", Type: entity.FieldReference, Target: "Currency"},
			{Name: "probability", Type: entity.FieldNumber, Default: float64(0), Min: entity.Float64Ptr(0), Max: entity.Float64Ptr(100)},
			{Name: "expected_close_date", Type: entity.FieldDate, Required: true},
			{Name: "owner_id", Type: entity.FieldReference, Target: "Party"},
			{Name: "description", Type: entity.FieldString},
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

// All returns every Definition this module adds.
func All() []*entity.Definition {
	return []*entity.Definition{
		Case(),
		Campaign(),
		Lead(),
		Opportunity(),
	}
}
