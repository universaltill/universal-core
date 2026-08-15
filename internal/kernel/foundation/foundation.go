// Package foundation seeds the always-on base entities every module
// depends on (ADR-0001 §8, reference-data-model.md §0): the
// Party–Role–Relationship pattern with its typed, multiple-per-party
// Address/ContactMechanism, the generic Attachment entity any record can
// carry files against, and the cross-cutting entities (unit of measure +
// conversions, currency + exchange rates) that Sales, Procurement,
// Inventory, and Manufacturing all reference. These ship with the
// kernel, not as an optional module — a tenant licensing only one
// operational module still needs a Party to exist.
package foundation

import "github.com/universaltill/universal-core/internal/kernel/entity"

// Party is one entity for anything that can act in a business
// relationship — a person or an organization. Customer/Vendor/Employee
// are roles held via PartyRole, not separate tables: this is what
// prevents the classic ERP failure of the same real-world company
// existing three times because finance, purchasing, and HR each created
// their own master record for it.
// v3 (uc-infra#63) added registration_number/contact_first_name/
// contact_last_name — the tenant's statutory identity, consumed by
// internal/kernel/saft.Build's Company/Contact block instead of the
// spec's "NA" markers. These three fields are meaningful only on the
// Party record that also holds the new own_organization PartyRole
// (below) — the same "optional/contextual field on the shared Party
// shape" pattern tax_id/preferred_language already establish (not every
// Party has a tax_id either). tax_id itself is reused as the VAT/tax
// registration number for that record rather than adding a fourth,
// redundant field. Additive fields, no data migration: rows written
// against v2 still hold legal values.
func Party() *entity.Definition {
	return &entity.Definition{
		EntityType: "Party",
		Version:    3,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"person", "organization"}},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "tax_id", Type: entity.FieldString},
			{Name: "status", Type: entity.FieldEnum,
				EnumValues: []string{"active", "inactive"}, Default: "active"},
			{Name: "preferred_language", Type: entity.FieldString, Default: "en"},
			{Name: "registration_number", Type: entity.FieldString},
			{Name: "contact_first_name", Type: entity.FieldString},
			{Name: "contact_last_name", Type: entity.FieldString},
		},
	}
}

// PartyRole records that a Party holds a given role — many-to-many, so
// one Party can be a vendor and a customer simultaneously (e.g. a
// supplier who also buys after-sales service).
//
// v3 (uc-infra#63) added own_organization: the Party holding this role
// is the tenant's own legal entity, resolved by
// internal/api/saftexport.go's saftCompanyProfile to populate
// internal/kernel/saft.Input's RegistrationNumber/TaxRegistrationNumber/
// ContactFirstName/ContactLastName instead of the spec's "NA" markers.
// Exactly one Party is expected to hold it per tenant — unlike
// AIProviderConnection (uc-infra#180), an UNCONDITIONAL singleton the
// ordinary Unique mechanism enforces via a marker field, this needs
// uniqueness CONDITIONAL on role_type=="own_organization" specifically
// (a plain Unique on role_type would wrongly cap every role_type —
// vendor, customer, ... — at one row each).
//
// v4 (uc-infra#201, ADR-0028) closes that gap: UniqueWhen declares
// exactly that condition, enforced by crud.Write/UpdateUniqueConstraintKeys
// against record_unique_keys the same way Unique is — a second
// own_organization PartyRole (create OR a role_type edit that produces
// one) now fails with *crud.UniqueConstraintError instead of silently
// saving. foundation.Currency.is_base has the identical shape (that
// Definition's own doc comment cross-references this one) and migrates
// onto the same mechanism in the same change. A caller finding zero, or
// (for records predating this version, before cmd/sync-tenant-modules'
// backfill runs) rows naming more than one DISTINCT Party, still
// degrades to treating it as absent rather than guessing — defense in
// depth for pre-existing data, not the primary guarantee anymore, the
// same fail-safe posture SystemOfRecord's own doc comment argues for.
// Additive enum value/constraint, no data migration: rows written
// against v2/v3 still hold legal values.
func PartyRole() *entity.Definition {
	return &entity.Definition{
		EntityType: "PartyRole",
		Version:    4,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "role_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"customer", "vendor", "employee", "contact", "prospect", "own_organization"}},
		},
		UniqueWhen: []entity.ConditionalUnique{
			{Fields: []string{"role_type"}, WhenField: "role_type", WhenValue: "own_organization"},
		},
	}
}

// PartyRelationship models connections between two parties — org charts,
// vendor/subsidiary links, employment, CRM contacts — with one mechanism
// instead of a bespoke foreign key per module.
//
// **Direction is per value, and the kernel cannot enforce it.** The enum
// says what the relationship is, not which end is which, so every value
// must state its own direction here or the table quietly fills with
// reversed rows nobody can tell apart:
//
//   - employs:     party_id_from EMPLOYS party_id_to (organization → person)
//   - supplies:    party_id_from SUPPLIES party_id_to (vendor → customer)
//   - parent_of:   party_id_from IS PARENT OF party_id_to (parent org → subsidiary)
//   - contact_for: party_id_from IS A CONTACT FOR party_id_to (person → organization)
//
// Note that contact_for runs person → organization while employs runs
// organization → person. That is deliberate — each reads naturally in
// its own direction — and it is exactly why the list above is an
// obligation on any future value, not decoration (ADR-0014).
//
// v3 added contact_for: a CRM contact is a person Party holding the
// `contact` PartyRole, joined to an organization Party by one of these
// rows (reference-data-model.md §6, ADR-0014). Reusing `employs` for it
// was rejected because at the time nothing in this schema identified the
// tenant's own organization Party, so one value would have had to carry
// both "our staff" and "their staff" with no field able to separate
// them. That gap is closed now — PartyRole's own_organization value
// (v3, uc-infra#63) — but contact_for still isn't reused for the
// statutory contact person question #63 actually needed: that's a
// single first/last name pair on the org Party itself
// (Party.contact_first_name/contact_last_name), not a second Party+
// relationship graph, since modeling a full CRM-style contact for one
// statutory field pair was judged overkill for that task. Additive enum
// value, no data migration: rows written against v2 still hold legal
// values.
func PartyRelationship() *entity.Definition {
	return &entity.Definition{
		EntityType: "PartyRelationship",
		Version:    3,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id_from", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "party_id_to", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "relationship_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"employs", "supplies", "parent_of", "contact_for"}},
		},
	}
}

// Address is a postal address attachable to a Party — typed and multiple
// per Party (reference-data-model.md §0), not a single hardcoded address
// field set on Party itself. A Party with billing and shipping addresses
// in different countries is the common case, not an edge case.
func Address() *entity.Definition {
	return &entity.Definition{
		EntityType: "Address",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "address_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"billing", "shipping", "registered", "other"}},
			{Name: "line1", Type: entity.FieldString, Required: true},
			{Name: "line2", Type: entity.FieldString},
			{Name: "city", Type: entity.FieldString, Required: true},
			{Name: "region", Type: entity.FieldString},
			{Name: "postal_code", Type: entity.FieldString},
			{Name: "country_code", Type: entity.FieldString, Required: true}, // ISO 3166-1 alpha-2
			{Name: "is_primary", Type: entity.FieldBool, Default: false},
		},
	}
}

// ContactMechanism is a typed contact channel (phone/email/fax/mobile),
// multiple per Party — same "typed and multiple" pattern as Address
// (reference-data-model.md §0), rather than fixed phone/email columns on
// Party that can't represent a second phone number or a fax-only contact.
func ContactMechanism() *entity.Definition {
	return &entity.Definition{
		EntityType: "ContactMechanism",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "mechanism_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"phone", "mobile", "email", "fax"}},
			{Name: "value", Type: entity.FieldString, Required: true},
			{Name: "is_primary", Type: entity.FieldBool, Default: false},
		},
	}
}

// Attachment is a generic file reference usable from any entity type —
// reference-data-model.md §0 calls this out as "usable from any entity",
// which is why entity_type/record_id are plain string fields rather than
// a FieldReference with a fixed Target: a FieldReference can only ever
// point at one target entity type (see entity.Field.Target), but an
// Attachment on a PurchaseOrder today and a Vendor tomorrow needs to name
// a different target each time. This mirrors how the generic `records`
// table and `audit_log` already store entity_type+record_id (CLAUDE.md's
// generic-storage pattern), not a new mechanism. Who uploaded it isn't a
// field here — crud.Engine writes an audit_log row (with actor identity)
// for every record's creation, Attachment included, so duplicating actor
// identity onto Attachment itself would be redundant *as long as
// Attachment records only ever get created through crud.Engine*. If a
// future bulk-import or direct-upload path ever writes Attachment records
// through internal/data directly, bypassing crud.Engine, that assumption
// breaks silently — revisit then, don't assume this holds forever.
func Attachment() *entity.Definition {
	return &entity.Definition{
		EntityType: "Attachment",
		// Version 3 (uc-infra#80): size_bytes gained a Min:0 bound.
		Version: 3,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "record_id", Type: entity.FieldString, Required: true},
			{Name: "file_name", Type: entity.FieldString, Required: true},
			{Name: "mime_type", Type: entity.FieldString, Required: true},
			{Name: "size_bytes", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)},
			// storage_path is where the actual bytes live (e.g. an object
			// store key) — this kernel spike models the metadata record
			// only, not a storage backend.
			{Name: "storage_path", Type: entity.FieldString, Required: true},
		},
	}
}

// UnitOfMeasure is a base unit (each, box, kg, litre) referenced by
// Inventory, Procurement, Sales, and Manufacturing alike.
func UnitOfMeasure() *entity.Definition {
	return &entity.Definition{
		EntityType: "UnitOfMeasure",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

// UomConversion is a conversion factor between two UnitOfMeasure entries
// (e.g. 1 box = 12 each: from_uom_id=box, to_uom_id=each, factor=12) —
// reference-data-model.md §0 calls this out alongside UnitOfMeasure
// itself, since Inventory/Procurement/Sales/Manufacturing all need to
// convert between a stocking unit and an ordering/selling unit. The
// "from multiplies into to" direction is a documented convention only,
// not schema-enforced (entity.Field has no way to express it) — build a
// conversion helper that bakes the direction in once a caller actually
// needs to convert a quantity, rather than each caller re-deriving which
// way to multiply.
func UomConversion() *entity.Definition {
	return &entity.Definition{
		EntityType: "UomConversion",
		// Version 3 (uc-infra#80): factor gained a Min:0 bound — a
		// negative multiplier is nonsensical for a unit conversion. Not a
		// strict positivity check (factor == 0 would collapse every
		// converted quantity to zero, also nonsensical) since Min is
		// inclusive; that residual gap is the same one this issue's own
		// tracking left for StockTransfer.qty, not solved here either.
		Version: 3,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "from_uom_id", Type: entity.FieldReference, Required: true, Target: "UnitOfMeasure"},
			{Name: "to_uom_id", Type: entity.FieldReference, Required: true, Target: "UnitOfMeasure"},
			{Name: "factor", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)}, // to_qty = from_qty × factor
		},
	}
}

// Currency is a base currency; ExchangeRate (date-effective rates) is a
// separate entity referencing it.
//
// v4 (uc-infra#120) added is_base: the tenant's functional/base currency,
// consumed by finance.ResolveBaseCurrency as the fallback
// finance.SyncGLAccounts and the SAF-T export use in place of a
// per-account/per-ledger currency. Exactly one Currency row is expected
// to hold is_base=true per tenant (ADR-0003: the database is the tenant,
// so "per tenant" here just means "per database") — the same
// uniqueness-conditional-on-a-field's-value shape PartyRole.
// own_organization needs (see PartyRole's own doc comment) and split off
// from AIProviderConnection's simpler true-singleton case (which reused
// the ordinary Unique mechanism via a marker field, uc-infra#180)
// precisely because it doesn't fit that mechanism either. Additive
// field, no data migration: rows written against v3 still hold legal
// values (is_base defaults false).
//
// v5 (uc-infra#181) closes a DIFFERENT gap on this same Definition: code
// (the ISO 4217 natural key, e.g. "QAR", "USD") was, until now, unique
// only by application-level convention — the exact shape po_number's own
// doc comment (purchasing.PurchaseOrder) used to describe before
// uc-infra#121 closed it the same way this does. Mechanically identical
// fix: a plain (unconditional, every-row) Unique declaration, closed by
// crud.Engine/record_unique_keys (ADR-0018 §3(c)) — see
// hr.AttendanceRecord's Unique doc comment for how the mechanism itself
// works. Not the same gap as is_base above: code's uniqueness doesn't
// depend on any OTHER field's value, so it fits the ordinary Unique
// mechanism directly, no conditional-Unique capability needed.
//
// v6 (uc-infra#201, ADR-0028) closes v4's is_base gap: UniqueWhen
// declares "unique when is_base==true," enforced by
// crud.Write/UpdateUniqueConstraintKeys against record_unique_keys the
// same way Unique/code is — a second is_base=true row (create OR an
// is_base edit that produces one) now fails with
// *crud.UniqueConstraintError instead of silently saving.
// finance.ResolveBaseCurrency's degrade-to-fallback-on-zero-or-multiple
// behavior stays, unchanged, as defense in depth for records that
// predate this version (before cmd/sync-tenant-modules' backfill runs) —
// not the primary guarantee anymore.
func Currency() *entity.Definition {
	return &entity.Definition{
		EntityType: "Currency",
		// Version 3 (uc-infra#80): minor_unit gained a [0, 6] bound,
		// matching the range assets.MinorUnits (depreciation.go) already
		// enforces downstream at the money-conversion boundary
		// (MaxMinorUnitScale) — declaring it here on the Definition
		// catches the same bad value earlier, at the same generic layer
		// every other bound in this sweep uses, instead of only when a
		// depreciation schedule happens to touch this currency.
		// Version 4 (uc-infra#120): added is_base, see doc comment above.
		// Version 5 (uc-infra#181): code gained a Unique declaration, see
		// doc comment above.
		// Version 6 (uc-infra#201): is_base gained a UniqueWhen
		// declaration, see doc comment above.
		Version: 6,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true}, // ISO 4217, e.g. "QAR", "USD"
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "minor_unit", Type: entity.FieldNumber, Default: float64(2), Min: entity.Float64Ptr(0), Max: entity.Float64Ptr(6)},
			{Name: "is_base", Type: entity.FieldBool, Default: false},
		},
		Unique: [][]string{{"code"}},
		UniqueWhen: []entity.ConditionalUnique{
			{Fields: []string{"is_base"}, WhenField: "is_base", WhenValue: "true"},
		},
	}
}

// ExchangeRate is a date-effective rate between two currencies, kept as
// its own entity (not a field on Currency) since rates change daily while
// a Currency's own code/name/minor_unit don't — Finance, Sales, and
// Procurement all consume this for multi-currency documents. rate follows
// the same "from multiplies into to" convention as UomConversion.factor
// above (e.g. 1 USD = 3.64 QAR: from_currency_id=USD, to_currency_id=QAR,
// rate=3.64) — like factor, this is a documented convention only, not
// schema-enforced; a conversion helper should bake the direction in
// before any caller consumes this field, rather than each caller
// re-deriving which way to multiply.
func ExchangeRate() *entity.Definition {
	return &entity.Definition{
		EntityType: "ExchangeRate",
		// Version 3 (uc-infra#80): rate gained a Min:0 bound — same
		// reasoning as UomConversion.factor above (a negative exchange
		// rate is nonsensical; zero is a separate, undeclared gap).
		Version: 3,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "from_currency_id", Type: entity.FieldReference, Required: true, Target: "Currency"},
			{Name: "to_currency_id", Type: entity.FieldReference, Required: true, Target: "Currency"},
			{Name: "effective_date", Type: entity.FieldDate, Required: true},
			{Name: "rate", Type: entity.FieldNumber, Required: true, Min: entity.Float64Ptr(0)}, // to_amount = from_amount × rate
		},
	}
}

// StatusType is one entity type's lifecycle state machine — reference-
// data-model.md §0's "Status/StatusType" pattern (OFBiz's StatusType/
// StatusItem/StatusValidChange), built as a real generic mechanism rather
// than the bespoke FieldEnum status field every entity that needs one has
// used so far (Party.status, purchasing.PurchaseOrder.status, the
// FiscalYear/Period status in reference-data-model.md §1). A FieldEnum
// validates that a value is one of a fixed set, but nothing stops an
// update jumping straight from "draft" to "cancelled" bypassing
// "submitted"/"approved", and nothing declares which value a new record
// starts in. StatusType/Status/StatusTransition below close that gap
// once, generically, for any entity that opts in — not by inventing new
// storage: these are ordinary foundation Definitions, stored as ordinary
// records via crud.Engine like Party or Address, not a bespoke SQL table.
//
// Deliberately not migrating Party/PurchaseOrder/FiscalYear's existing
// status fields to this pattern here — that's a real per-entity decision
// (what are that entity's valid states and transitions?) belonging to a
// session working that entity, not a side effect of introducing the
// generic mechanism. An entity opts in by declaring
// entity.Definition.StatusTypeCode and a "status_id" FieldReference
// targeting Status (entity.Definition.Validate enforces the pairing);
// until an entity does that, this package's addition is inert for it.
//
// No separate StatusHistory entity: crud.Engine.Update already writes an
// audit_log row with a diff for every update (ADR-0001 §14/§16,
// Attachment's own doc comment reasons through the same "don't duplicate
// what audit_log already gives for free" tradeoff for actor identity) —
// that diff already captures status_id's old/new value, who changed it,
// and when. A bespoke StatusHistory entity duplicating (entity_type,
// record_id, from_status, to_status, actor, timestamp) would just be
// audit_log's diff reshaped, not new information.
func StatusType() *entity.Definition {
	return &entity.Definition{
		EntityType: "StatusType",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			// entity_type names the business entity this state machine
			// governs (e.g. "PurchaseOrder") — a plain string, not a
			// FieldReference, for the same reason Attachment.entity_type
			// is a plain string: it must be able to name any entity type,
			// including ones that don't exist yet when this Definition is
			// authored (a FieldReference's Target is fixed at one type).
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "code", Type: entity.FieldString, Required: true}, // e.g. "purchase_order_status" — what entity.Definition.StatusTypeCode names
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

// Status is one allowed state within a StatusType (OFBiz's StatusItem)
// — e.g. draft/submitted/approved/cancelled. is_initial marks which
// Status(es) a new record may start in; crud.Engine.ValidateStatusTransition
// rejects creating a record whose status_id isn't flagged is_initial.
// is_terminal is descriptive only in this increment (a UI hint that no
// further transition is expected) — not enforced, since StatusTransition
// rows already define the actual transition graph and a terminal status
// with no outgoing StatusTransition rows is already unreachable by
// construction.
//
// Version 1->2 (uc-infra#214, ADR-0030): "name"'s TYPE changed
// (FieldString -> FieldI18nText), the same class of change PurchaseOrder's
// v3->v4 bump made for "status" -> "status_id" (see that Definition's own
// doc comment) — not just a new field added. Before this bump, every
// status-managed entity's status picker/record view rendered the literal
// English name seeded at provisioning time in every locale (uc-infra#214's
// own finding): handlers.Handler.recordLabel only translates a reference's
// label field when it is declared i18n_text, and plain FieldString never
// was. A Status row written under v1 still holds a plain string until
// cmd/backfill-status-name-i18n converts it — reads are unaffected
// (entity.ValidateRecord only runs on write), so the practical window is
// between publishing v2 and running that backfill, same "reads still work,
// writes need the new shape" property every prior Version-bump-replaces-a-
// field-type migration in this kernel has had.
//
// This bump ships the GENERIC mechanism and English-only content only:
// internal/kernel/statusgraph.Seed now wraps every module's plain-string
// Spec.Name as {"en": name} when it creates a Status row, so recordLabel's
// existing i18n_text branch starts resolving it for free — no
// entity-specific special-casing anywhere. Real per-locale translations
// for each module's actual status names (the "Onaylandı" side of
// uc-infra#214's own finding) are deliberately deferred to a follow-up
// card: getting ~30-50 status names translated correctly across tr/fa/ar
// is real content work, orthogonal to (and shouldn't block) making the
// mechanism itself sound — see uc-infra#214's close-out for the follow-up
// issue number. i18n.Catalog.ResolveLocalized's existing fallback chain
// (locale -> base language -> catalog fallback -> any translation) means
// an English-only value renders exactly the same "Draft" in every locale
// today, so how an already-seeded status's name RENDERS doesn't change —
// only what a future translation pass can now do without a second schema
// change. The one real, narrow exception: StatusForm() declares "name"
// with no override, so an admin hand-authoring a Status record now fills
// in a per-locale name instead of one plain string (rare — see this
// Definition's own doc comment on when that path is actually used) — see
// internal/help/content/*/entity/Status.md and
// internal/e2e/status_i18n_test.go for that surface.
func Status() *entity.Definition {
	return &entity.Definition{
		EntityType: "Status",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldI18nText, Required: true},
			{Name: "sequence", Type: entity.FieldNumber, Default: float64(0)}, // display/ordering hint only
			{Name: "is_initial", Type: entity.FieldBool, Default: false},
			{Name: "is_terminal", Type: entity.FieldBool, Default: false},
		},
	}
}

// StatusTransition is one legal edge (OFBiz's StatusValidChange) in a
// StatusType's state graph: from_status_id -> to_status_id is allowed,
// absence means it isn't. requires_workflow optionally names an R9
// workflow.Definition that should gate this transition (e.g. a
// PurchaseOrder moving to "approved" needing a require_approval step) —
// declared here as the data a future workflow trigger type (e.g. an
// on_status_change alongside internal/kernel/workflow's existing
// on_create/on_update — not added in this increment) would consult;
// crud.Engine.ValidateStatusTransition itself only checks the transition
// is graph-legal, it does not look at requires_workflow or block on it.
// Recording the field now rather than adding it later avoids a migration
// once a workflow trigger consumes it.
func StatusTransition() *entity.Definition {
	return &entity.Definition{
		EntityType: "StatusTransition",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "from_status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "to_status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
			{Name: "requires_workflow", Type: entity.FieldString}, // optional workflow.Definition Name; not yet consulted, see doc comment above
		},
	}
}

// IssueReport is a user-submitted bug/feedback report from the in-app
// issue logger — voice-note transcription (internal/kernel/speechassist,
// a self-hosted Whisper ASR instance) plus typed context, captured and
// stored as an ordinary record like anything else in this kernel rather
// than a bespoke table: the generic entity/crud/audit machinery already
// gives tenant scoping, an audit trail of who submitted what and when,
// and a browsable list/form for free, for zero extra persistence code.
//
// Foundation, not an optional module: reporting a problem shouldn't
// require a tenant to have licensed anything specific — every tenant
// gets this the same way every tenant gets Party.
//
// Deliberately a plain FieldEnum status, not the Status/StatusType
// pattern (see StatusType's own doc comment) — new -> triaged/dismissed
// is a flat, three-state lifecycle with no real transition graph worth
// declaring, unlike PurchaseOrder's actual multi-step flow. Per that
// pattern's doc comment, opting in is a real per-entity decision, not
// automatic just because the mechanism exists.
//
// Automatic GitHub issue filing (the original ask: "open an issue for
// us maybe in the github issue") isn't wired yet — needs a GitHub
// credential with issue-write access to the target repo, which per
// this kernel's standing secret-creation discipline needs to be
// explicitly authorized and provisioned, not created unilaterally. This
// entity is the interim (and permanent, regardless — an audit trail of
// every report ever submitted, filed or not) store; filing to GitHub is
// a later step that would read from here, not replace it.
// console_log (v2, optional) is the browser's own console.error/warn
// output plus any uncaught error/unhandledrejection since the page that
// captured it loaded — see internal/api/layout.go's shellTmpl and
// internal/api/issuereport.go. Optional, same as transcript: a browser
// with nothing captured (or an old row from before this field existed)
// just omits the key, no recordmigrate backfill needed since this isn't
// Required. Version bumped 1->2 regardless (same reasoning as UserRole's
// v1->v2 department_id addition, b90ff56): an unbumped Version is a
// no-op for every tenant whose registry already has v1 published
// (moduleseed.publishOne returns early once a version's status is
// already Published), so the field would silently never reach an
// already-provisioned tenant without this.
//
// Version bumped 2->3 again for MaxLength on every plain-text field
// below (uc-infra#174) — same mechanism, no new field this time, but
// moduleseed.publishOne keys strictly on (entity_type, version) already
// being Published, so a tenant whose registry already has v2 published
// would never pick up the tightened bound without a version bump either;
// "the shape didn't add a field" doesn't change that.
//
// All four FieldStrings get a bound, not just description/console_log:
// the exploit this closes is "an authenticated caller posts directly to
// /issue-report/submit, no browser involved" (independent review of
// uc-infra#92 found it; independent review of THIS fix caught that an
// earlier draft only capped two of the four fields it rides in the same
// request — title and transcript were just as reachable through the
// identical unbounded-FieldString gap, only smaller in practice because
// a real browser's own UI happens not to invite pasting megabytes into a
// one-line title, not because anything stopped it).
//
// A version bump that TIGHTENS a bound (unlike one that adds a Required
// field) has no recordmigrate story and deliberately gets none here: an
// already-stored row whose text happens to exceed its field's new
// MaxLength keeps rendering and stays readable exactly as before — only
// a future UPDATE of that specific row would need edited text within
// the new bound to save (entity.ValidateRecord runs identically on
// Update as on Create, the same way Required/Min/Max already do; there
// is no special case for pre-existing data on any other bound this
// kernel enforces, e.g. Min/Max, and this doesn't invent one for
// MaxLength either). recordmigrate's own doc comment scopes it to a
// version bump that "adds or replaces a Required field" — this bump does
// neither, so this deliberately doesn't reach for it; a Definition that
// TRUNCATED submitted evidence text to make old rows fit would be doing
// something recordmigrate's Transform pattern was never meant for
// (silently discarding part of a user's bug report). Tracked instead as
// a known, narrow, accepted gap — see uc-infra#(follow-up filed at
// close-out) for whether existing over-length rows exist in practice and
// what, if anything, should be done about them.
func IssueReport() *entity.Definition {
	return &entity.Definition{
		EntityType: "IssueReport",
		Version:    3,
		Module:     "foundation",
		// title: MaxLength 500 characters — a one-line summary, generous
		// for hand-typed text, same closing-the-gap reasoning as every
		// other field below.
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true, MaxLength: entity.IntPtr(500)},
			// description is the field a human actually reads: typed text
			// plus the voice transcript appended, if there was one — see
			// internal/api/issuereport.go's submit handler. transcript
			// below keeps the raw, unedited ASR output separately so a
			// human's own edits to description never lose the original.
			//
			// MaxLength 20,000 characters (uc-infra#174): generous room for
			// hand-typed text plus an appended Whisper transcript — the
			// capture page's own script clamps the appended combination to
			// this same bound before it ever reaches the textarea (see
			// issueReportTmpl's transcribe .then() handler), since
			// transcript's OWN bound below is deliberately larger than
			// this field's (a long recording's full transcript still
			// belongs verbatim in transcript even when only part of it
			// fits in the appended description). Closes the gap that let
			// one authenticated /issue-report/submit request write up to
			// ~61 MiB of text here (maxIssueReportSubmitBytes,
			// issuereport.go), inherited from the screen-recording
			// upload's own byte cap because entity.FieldString had no
			// length concept before this. issueReportTmpl's own textarea
			// carries the identical bound as its browser-native maxlength
			// attribute — read directly from THIS Definition at render
			// time (issueReportNewPage), not duplicated as a separate
			// constant, so the two can't drift; entity.ValidateRecord is
			// what actually enforces it either way.
			{Name: "description", Type: entity.FieldString, Required: true, MaxLength: entity.IntPtr(20000)},
			// transcript: MaxLength 40,000 characters — the raw, unedited
			// ASR output of a full maxVoiceNoteBytes (10 MiB) recording.
			// Sized above description's own bound deliberately (independent
			// review: MediaRecorder's default opus bitrate is UA-chosen,
			// not pinned to a rate that guarantees a short transcript —
			// at a low real-world bitrate a full-length recording can
			// transcribe past 30-40k characters), since this field is the
			// durable, complete record of what the ASR actually produced;
			// only the copy appended into description is length-clamped
			// client-side, this field never is.
			{Name: "transcript", Type: entity.FieldString, MaxLength: entity.IntPtr(40000)},
			// console_log: MaxLength 30,000 characters — headroom above
			// the capture page's own client-side rolling cap (layout.go's
			// ucLogMaxChars = 20000) for encoding/formatting overhead,
			// while still closing the same unbounded-text gap description
			// above documents — a non-browser caller posting directly to
			// /issue-report/submit bypasses ucLogMaxChars entirely, so the
			// real enforcement has to live here, not just client-side.
			// (layout.go's own capture loop separately got its
			// single-oversized-entry gap fixed alongside this — see
			// ucAppendLog's doc comment — so a legitimately-captured log
			// should not hit this bound in practice either.)
			{Name: "console_log", Type: entity.FieldString, MaxLength: entity.IntPtr(30000)},
			{Name: "page_url", Type: entity.FieldString},
			{Name: "user_agent", Type: entity.FieldString},
			{Name: "status", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"new", "triaged", "dismissed"}, Default: "new"},
		},
	}
}

// AIProviderConnection is a tenant's own configured AI backend for
// text-generation-assisted features (currently: the import wizard's
// column-mapping suggestion, internal/kernel/csvimport.
// SuggestMappingAI) — a BYOK plugin, per internal/kernel/aiprovider's
// own doc comment: the platform's own self-hosted Ollama instance stays
// the default every tenant already gets for free; this is how a tenant
// opts into their own Anthropic or OpenAI API key instead (their own
// account, their own cost, their own explicit consent to that
// provider's data handling), or into their own self-hosted Ollama
// server (a laptop, a home server, their own cloud VM — anywhere they
// control) instead of the platform's shared one.
//
// One row per tenant (an upsert, not a list — internal/api's settings
// handler enforces that). v2 (uc-infra#180) closed the two gaps this
// doc comment used to just document:
//
//   - singleton_key is a caller-invisible marker field the settings
//     handler (internal/api/aiprovidersettings.go) always sets to the
//     same fixed constant on every write, so this Definition's ordinary
//     Unique mechanism — which can only express "at most one row per
//     VALUE," not "at most one row, full stop" (entity.Definition
//     .Validate() rejects an empty Unique field set outright) — has a
//     value to be unique ON. Every tenant's other fields legitimately
//     differ (provider, model, base_url), which is exactly why this
//     needed a dedicated field rather than reusing one of them: no real
//     field is a natural key here the way PurchaseOrder.po_number or
//     SystemOfRecord's (entity_type, source_id) pair already are
//     (uc-infra#121). Not rendered anywhere in the settings form and not
//     meaningful to a human — purely the mechanism's own bookkeeping,
//     the same role api_key_encrypted's ciphertext plays for secrecy
//     rather than uniqueness.
//   - AIProviderConnection joined authz.systemOnlyWriteTypes, closing the
//     gap where an ordinary user with ordinary create permission could
//     POST directly to /api/records/AIProviderConnection and create a
//     second row, bypassing this settings handler's upsert-only logic —
//     "the one place that writes it" is now an enforced fact, not just
//     this file's stated intent. systemOnlyWriteTypes denies the
//     RBAC-guarded engine unconditionally for this type — even to an
//     admin, even to a machine/service-token caller (CanWrite checks
//     systemOnlyWriteTypes before its machine bypass specifically so
//     this holds, authz.go's own doc comment) — so the settings handler
//     itself writes through a raw crud.Engine instead
//     (internal/api/aiprovidersettings.go), the same system-path bypass
//     ExternalIdentity's import engine already uses. Because that raw
//     engine carries no RBAC check of its own, the settings handler
//     gates itself independently on the tenant_admin role
//     (aiProviderRequireAdmin) — an independent review of this change
//     caught that switching to the raw engine without adding a
//     replacement gate would have made the page writable by any
//     authenticated tenant member, RBAC configuration notwithstanding,
//     since these routes carry no route-level role check otherwise. List
//     (reads) stays on the ordinary RBAC-guarded ts.crud throughout —
//     systemOnlyWriteTypes only touches CanWrite, never CanRead, so
//     there is no reason to give up GuardedEngine's FieldPermission
//     redaction on the read path.
//
// api_key_encrypted is never the plaintext key: internal/kernel/
// secretcrypt encrypts it before internal/api's handler ever calls
// crud.Engine.Create/Update, and the settings page never echoes it back
// in plaintext after saving — see secretcrypt's own doc comment on why
// this needed a real encrypted-at-rest mechanism instead of just
// another plain FieldString like every other field in this kernel.
// Deliberately not Required at the entity level (unlike IssueReport's
// status field, say): whether it's needed at all depends on `provider`
// (Ollama doesn't use it, Anthropic/OpenAI do) — conditional-on-another-
// field validation isn't something entity.Field can express, so that
// check lives in internal/api's settings handler instead, the same
// "provider-specific business logic belongs in the feature layer, not
// the generic entity engine" discipline every other per-provider
// validation in this feature already follows.
func AIProviderConnection() *entity.Definition {
	return &entity.Definition{
		EntityType: "AIProviderConnection",
		// Version 2 (uc-infra#180): singleton_key added, with a Unique
		// declaration enforcing the one-row-per-tenant invariant for
		// real — see this function's own doc comment above.
		Version: 2,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "provider", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"ollama", "anthropic", "openai"}},
			// base_url: Ollama only (the tenant's own server address).
			{Name: "base_url", Type: entity.FieldString},
			// model: required in practice for every provider (enforced
			// by the handler, not here — same conditional-on-provider
			// reasoning as api_key_encrypted above) — deliberately no
			// default for any provider: a tenant configuring their own
			// Anthropic/OpenAI connection explicitly picks what they're
			// paying for, this kernel doesn't guess a model name on
			// their behalf.
			{Name: "model", Type: entity.FieldString},
			// api_key_encrypted: Anthropic/OpenAI only. See this
			// function's own doc comment above.
			{Name: "api_key_encrypted", Type: entity.FieldString},
			// singleton_key: see this function's own doc comment above.
			// Required so a row that somehow reaches storage without it
			// (a pre-v2 row that predates this field, until the settings
			// handler next rewrites it) is at least visibly incomplete
			// rather than silently exempt from the Unique check below —
			// uniqueKeyValue's own "any named field absent/empty is
			// exempt" rule (mirroring SQL NULL-in-a-unique-index
			// semantics) already covers that pre-v2 case gracefully, this
			// Required is belt-and-braces for any *new* row.
			{Name: "singleton_key", Type: entity.FieldString, Required: true},
		},
		Unique: [][]string{{"singleton_key"}},
	}
}

// ExternalSQLSource is a tenant-registered connection to an external SQL
// database — a legacy ERP's SQL Server (the NAV 2009 case ADR-0019 was
// written for), or any Postgres/MSSQL database the tenant wants to pull
// records from through the import wizard's mapping/preview/commit flow.
// Unlike AIProviderConnection this IS a list, not an upsert-one: a
// tenant may register several sources (a NAV company database and a
// reporting mirror, say) and each import run picks one.
//
// password_encrypted follows api_key_encrypted's discipline exactly:
// internal/kernel/secretcrypt encrypts it before internal/api's handler
// ever calls crud.Engine.Create/Update, it is never echoed back to a
// browser after saving, and it is deliberately not Required at the
// entity level — a source may legitimately have no password (integrated
// auth on a dev box, a passwordless local Postgres), and requiring it
// here would also force the settings handler to round-trip the secret on
// every edit instead of treating "blank on edit" as "unchanged".
//
// The connection details are plain fields, not a single opaque DSN
// string: the settings UI edits them individually, and
// internal/kernel/sqlsource composes the driver-specific DSN — keeping
// DSN syntax (a driver concern) out of stored data.
func ExternalSQLSource() *entity.Definition {
	return &entity.Definition{
		EntityType: "ExternalSQLSource",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "driver", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"mssql", "postgres"}},
			{Name: "host", Type: entity.FieldString, Required: true},
			{Name: "port", Type: entity.FieldNumber},
			{Name: "database", Type: entity.FieldString, Required: true},
			{Name: "username", Type: entity.FieldString},
			{Name: "password_encrypted", Type: entity.FieldString},
			// options: driver-specific extras a DSN needs verbatim
			// (e.g. "encrypt=disable" against an old on-prem SQL
			// Server that predates TLS-by-default drivers).
			{Name: "options", Type: entity.FieldString},
		},
	}
}

// ExternalIdentity remembers which legacy record a Core record came from
// (uc-infra#101): one row per imported record, keyed by the source it was
// pulled from (source_id → ExternalSQLSource), the entity type it landed
// as, and the legacy system's own key for it (external_key — e.g. NAV's
// "No_"). This is what lets a re-import UPDATE the record it created last
// time instead of duplicating it (internal/kernel/sqlsource.
// CommitRowsUpserting is the consumer — and the only writer, see below).
//
// entity_type/record_id are plain string fields rather than a
// FieldReference with a fixed Target, for exactly Attachment's reason: a
// FieldReference can only ever point at one target entity type (see
// entity.Field.Target), but an ExternalIdentity for an imported Item
// today and an imported Party tomorrow needs to name a different target
// each time. This mirrors how the generic `records` table and
// `audit_log` already store entity_type+record_id (CLAUDE.md's
// generic-storage pattern), not a new mechanism.
//
// Uniqueness on (source_id, source_relation, entity_type, external_key) is
// schema-enforced as of Version 2 (uc-infra#121) via this Definition's own
// Unique declaration below, closed by crud.Engine/record_unique_keys
// (ADR-0018 §3(c)) — see hr.AttendanceRecord's Unique doc comment for how
// the mechanism itself works; #81 landed the mechanism this field cites as
// "in flight elsewhere" 2026-08-05, after this doc comment was first
// written. What keeps the convention honest independent of that: user-
// driven WRITES are denied unconditionally (authz.systemOnlyWriteTypes —
// even admins, even before a tenant configures RBAC), so the import
// engine's raw-engine side-channel is the only write path — and that path
// goes through the same crud.Engine.Create/Update this Unique declaration
// enforces against (internal/kernel/sqlsource.CommitRowsUpserting's
// RecordEngine interface matches crud.Engine's own signature, it is not a
// true bypass of the generic engine, only of authz's permission check). A
// duplicate now fails the write itself for a tenant whose sync has
// reached this Version bump; the "ambiguous identity" import error path
// (internal/kernel/sqlsource's identityTarget.ambiguous) still exists and
// still matters for a pre-migration tenant, whose existing rows have no
// record_unique_keys entry yet (ADR-0018's Consequences — same reasoning
// as SystemOfRecord's fallback above), so it is not deleted or
// superseded, only no longer the ONLY defense. Reads through the generic
// API remain possible — the legacy-key→record map is not a secret, just
// not user-editable.
func ExternalIdentity() *entity.Definition {
	return &entity.Definition{
		EntityType: "ExternalIdentity",
		// Version 2 (uc-infra#121): source_id+source_relation+entity_type+
		// external_key gained a Unique declaration.
		Version: 2,
		Module:  "foundation",
		Fields: []entity.Field{
			{Name: "source_id", Type: entity.FieldReference, Required: true, Target: "ExternalSQLSource"},
			// source_relation is part of the identity scope, not
			// decoration: NAV's $Customer and $Vendor both land in Party
			// and their number series overlap, so scoping by source
			// alone would let a Vendor import overwrite Customer records
			// (independent review, 2026-08-01). Schema-qualified
			// relation name, exactly as fetched.
			{Name: "source_relation", Type: entity.FieldString, Required: true},
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "record_id", Type: entity.FieldString, Required: true},
			{Name: "external_key", Type: entity.FieldString, Required: true},
		},
		Unique: [][]string{{"source_id", "source_relation", "entity_type", "external_key"}},
	}
}

// SystemOfRecord declares who owns an entity type's records (uc-infra#102):
// the tenant's statement of which system is the master for, say, Item or
// Party — this platform, or an external legacy database registered as an
// ExternalSQLSource. It maps to ADR-0001's Observe→Assist→Transact→Own
// adoption gates: "read_only" is the Observe/Assist posture (Core mirrors
// the legacy system's records via the import wizard and must never
// hand-edit them — the legacy system would just overwrite the edit on the
// next sync, silently), "platform_owned" is the Own posture (Core is the
// master, edit freely), and "bidirectional" is the Transact posture
// (two-way sync).
//
// "bidirectional" is a declared-but-reserved value: the sync engine that
// would honor it is a later slice, so the enforcement layer
// (internal/kernel/authz's guarded-engine check) treats it as
// invalid-to-save for now — deliberately via that config-level check, NOT
// by omitting it from this enum, because removing an enum value later
// would be a breaking change to already-stored rows while reserving it
// now is purely additive: when the sync engine ships, only the authz
// rejection is lifted, no entity/schema change.
//
// source_id names WHICH external source owns the records — required in
// spirit for read_only (an ownership claim with no owner names nothing),
// but not Required at the entity level because it is meaningless for
// platform_owned (there is no external party to point at), and
// conditional-on-another-field validation isn't something entity.Field
// can express (AIProviderConnection.api_key_encrypted documents the same
// limitation).
//
// One DECLARATION per (entity_type, source) is schema-enforced as of
// Version 2 (uc-infra#121) via this Definition's own Unique declaration
// below, closed by crud.Engine/record_unique_keys (ADR-0018 §3(c)) — see
// hr.AttendanceRecord's Unique doc comment for how the mechanism itself
// works. Deliberately (entity_type, source_id) together, NOT entity_type
// alone: authz.checkSoRReadOnly is explicitly written to accept SEVERAL
// rows per entity_type, one per external source it mirrors from
// (readOnlySources is a set, not a single value) — a tenant mirroring
// Party from both a NAV instance and an old CRM legitimately declares
// TWO read_only rows for "Party," one per source, and blocking that
// configuration would have been a real capability loss an independent
// review caught (uc-infra#121 follow-up) that this repo's own test
// suite could not have detected on its own. What this constraint DOES
// still close: the same (entity_type, source_id) pair declared twice —
// a genuinely redundant/contradictory pair of rows, not a multi-source
// configuration. A blank source_id (the "no owner named" fail-safe case
// below) stays exempt from this constraint the same way any Unique
// field's absence exempts a record (mirrors SQL NULL semantics) — more
// than one blank-source row for the same entity_type can still exist,
// same as before this Version bump; nothing here narrows that case.
//
// The "most restrictive wins" resolution below is NOT just a fallback
// for pre-migration duplicate data — it is the ACTUAL mechanism this
// multi-source configuration relies on going forward: any read_only row
// (from a set of otherwise-legitimate, non-colliding rows) makes the
// type read_only for records identified against that row's source,
// regardless of what other rows for other sources say. It also still
// covers pre-migration rows a tenant's sync hasn't reached yet
// (cmd/sync-tenant-modules' generic uniqueConstraintWarnings flags a
// tenant already holding an exact (entity_type, source_id) collision,
// ADR-0018's Consequences) — deliberately not deleted either way.
// Fail-safe by design — conflicting ownership claims must degrade to
// "don't let a hand-edit clobber a sync," never the reverse.
//
// Enforcement lives in internal/kernel/authz (GuardedEngine's
// system-of-record check), NOT here and NOT in crud/entity — the generic
// engines stay free of entity-type-specific behavior per CLAUDE.md's
// kernel-boundary rule; authz is config-plumbing over its own named
// types, the same way controlPlaneTypes already names types.
func SystemOfRecord() *entity.Definition {
	return &entity.Definition{
		EntityType: "SystemOfRecord",
		// Version 2 (uc-infra#121): (entity_type, source_id) gained a
		// Unique declaration.
		Version: 2,
		Module:  "foundation",
		Fields: []entity.Field{
			// entity_type is a plain string for Attachment's reason: it
			// must be able to name any entity type, including ones that
			// don't exist yet when this Definition is authored.
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "source_id", Type: entity.FieldReference, Target: "ExternalSQLSource"},
			{Name: "mode", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"read_only", "bidirectional", "platform_owned"}},
		},
		Unique: [][]string{{"entity_type", "source_id"}},
	}
}

// Role is a tenant-defined, tenant-customizable access-control role —
// "Warehouse Supervisor," "Finance Manager," whatever a given tenant
// actually needs (Farshid, 2026-07-29: "we need lots of role in the
// tenant"), not a fixed global list. Deliberately Core-owned rather than
// modeled in Zitadel (ADR-0005, `uc-infra/docs/adr/0005-role-permission-
// model-core-owned.md`) — Zitadel's own single `tenant_member` grant
// stays the coarse "is this user in this tenant at all" boundary; Role
// is the finer "what can they do once they're in" layer, living in the
// same per-tenant database as everything else this kernel already
// authorizes against (no per-request external API call).
//
// Not to be confused with `PartyRole` (above) — that's a business
// relationship a Party holds (customer/vendor/employee), unrelated to
// system access control. A person can be both: an "employee" PartyRole
// and, separately, hold a "Finance Manager" access-control Role.
//
// Deliberately NOT built in this pass (ADR-0005's own scoping note):
// any actual permission *enforcement* — this is the data model and
// assignment mechanism only. A `Permission` model wiring this into
// `internal/kernel/entity`/`crud`'s read/write paths is real, separate,
// future work (`erp/BACKLOG-TASKS.md` Phase 2, directly below the task
// this entity ships as part of).
func Role() *entity.Definition {
	return &entity.Definition{
		EntityType: "Role",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "description", Type: entity.FieldString},
		},
	}
}

// UserRole grants one Role to one authenticated user — many-to-many
// (a user can hold more than one Role; a Role can be held by more than
// one user), the same shape PartyRole already uses for Party<->role_type.
//
// user_id is a plain FieldString carrying the Zitadel OIDC "sub" claim
// (`internal/webauth.Session.Subject`), not a FieldReference — there is
// no Core-side User/Person-as-login-identity entity to reference (see
// ADR-0005's own explanation): this kernel already uses that same
// identifier as `audit.Actor.ID` on every authenticated request
// (`internal/webauth/bridge.go`), so UserRole reuses it directly rather
// than inventing a second identity concept.
//
// department_id (v2, optional) scopes a grant to one Department — e.g.
// "Finance Manager, but only within the Sales department" — for R17's
// department-based approval routing (`erp/BACKLOG-TASKS.md` Phase 2).
// Optional and nil-means-global by design, same reversible choice as
// Position.department_id above and for the same reason: a company-wide
// grant (e.g. a tenant-wide "Admin" role) shouldn't need a synthetic
// root Department to stay ungated. Deliberately NOT wired into anything
// yet: this ships the scoping data + a resolver
// (RoleCodesForUserInDepartment, roles.go); resolving *which* department
// a given approval request belongs to (the other half of R17's routing)
// is separate, still-open work needing either Employee.department_id
// (now shipped — hr.Employee, board #16/ADR-0013, so this half is
// unblocked) or a new department field on trigger entities like
// PurchaseOrder — see BACKLOG-TASKS.md's note on this task.
func UserRole() *entity.Definition {
	return &entity.Definition{
		EntityType: "UserRole",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "user_id", Type: entity.FieldString, Required: true},
			{Name: "role_id", Type: entity.FieldReference, Required: true, Target: "Role"},
			{Name: "department_id", Type: entity.FieldReference, Target: "Department"},
		},
	}
}

// Permission is one entity-level RBAC grant: "this Role may read
// (and/or write) this entity type" (ADR-0006, `uc-infra/docs/adr/0006-
// rbac-enforcement-guarded-engine.md`). Enforcement semantics live in
// internal/kernel/authz, but the two facts that shape this entity's
// design are worth restating at the data model:
//
//   - Grants are additive (union over the user's roles); there is no
//     "deny" row. Absence of any Permission row for an entity type
//     means that type is not opted into RBAC at all and stays fully
//     accessible (backward compatibility with every tenant provisioned
//     before this entity existed).
//   - The moment ANY Permission row exists for an entity type, that
//     type flips to deny-unless-granted for non-admin, non-machine
//     users — creating a narrow grant is also the act of opting the
//     entity type into enforcement.
//
// entity_type is a plain FieldString, not a reference — Entity
// Definitions live in the registry (entity_definitions), not in
// records, so there is nothing for a FieldReference to point at. A
// typo'd entity_type yields an inert rule (ADR-0006 records this as an
// accepted limitation), visible and fixable in the generated UI.
func Permission() *entity.Definition {
	return &entity.Definition{
		EntityType: "Permission",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "role_id", Type: entity.FieldReference, Required: true, Target: "Role"},
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "can_read", Type: entity.FieldBool},
			{Name: "can_write", Type: entity.FieldBool},
		},
	}
}

// FieldPermission is one field-level RBAC rule: "for holders of this
// Role, this field on this entity type is hidden" (R5's "a hidden field
// is hidden by RBAC, not by the layout"). Deliberately flat and
// standalone rather than a composition under Permission — a field rule
// is meaningful on its own (field hiding applies even to entity types
// that aren't entity-level opted-in, e.g. hide `credit_limit` on
// Customer for a junior role while Customer itself stays open), and
// R11a's composition machinery (atomic save, roll-up) buys nothing
// here. Visibility resolves as the union of what each held role can
// see: a field is hidden from a user only when EVERY role they hold
// hides it (ADR-0006).
//
// Enforcement of these rows (read stripping, form filtering, write
// rejection) ships as the second commit of the ADR-0006 task; the
// entity lands with the model so both commits publish one registry
// shape.
func FieldPermission() *entity.Definition {
	return &entity.Definition{
		EntityType: "FieldPermission",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "role_id", Type: entity.FieldReference, Required: true, Target: "Role"},
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "field_name", Type: entity.FieldString, Required: true},
			{Name: "hidden", Type: entity.FieldBool},
		},
	}
}

// Department is one node in a tenant's org-chart (Phase 2 of
// `erp/BACKLOG-TASKS.md`, reference-data-model.md §7) — built as pure
// org-structure data, at a time when no Employee entity existed.
// hr.Employee and hr.LeaveRequest have since shipped (board #16,
// ADR-0013) and reference Department from there; AttendanceRecord is
// still later HR work (#17). This exists to give R17's
// role/department-based
// approval routing something to route against; it does not itself
// implement routing.
//
// parent_department_id is a self-referencing FieldReference — the same
// hierarchical-self-reference shape already proven end to end by
// Account.parent_account_id (finance.go), not the two-party-join shape
// PartyRelationship uses: reference-data-model.md's own field name for
// Position ("reports_to_position_id", a singular FK) and every
// hierarchy actually built in this kernel so far (Account, Task) use
// this shape, so it's the one reused here despite the spec text's
// "PartyRelationship pattern reused" wording, which predates Account's
// hierarchy and reads as referring to the general pattern rather than
// literally that entity.
func Department() *entity.Definition {
	return &entity.Definition{
		EntityType: "Department",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "parent_department_id", Type: entity.FieldReference, Target: "Department"},
		},
		// QuickCreatable (part 2 of #24, universaltill/uc-infra#51): the
		// org-chart's own first candidate for inline creation — a
		// parent_department_id picker on a not-yet-saved Department is
		// exactly the "the record I need doesn't exist yet" case the
		// feature targets, and Department has no lifecycle/graph-shaping
		// fields (unlike Status) that make spontaneous creation risky.
		QuickCreatable: true,
	}
}

// Position is a role/seat within a Department's org-chart (reference-
// data-model.md §7) — deliberately not the same thing as an Employee
// (which occupies a Position, a later HR task): a Position can exist,
// and sit in a reporting line, before anyone is assigned to it.
//
// department_id is NOT in reference-data-model.md's literal field list
// (it puts department_id on the future Employee entity instead) and is
// deliberately optional here rather than required, despite this
// package's own doc comment on an earlier draft arguing the opposite —
// independent review caught the real cost of requiring it: a
// company-level/matrix position (e.g. CFO, reporting to no single
// department) would have needed a synthetic root Department just to
// satisfy the constraint, and now that Employee has landed (hr.Employee, ADR-0013), a required
// Position.department_id would be a second, easily-diverging source of
// truth alongside Employee's own department_id. Optional is the
// reversible choice — reports_to_position_id is the same self-
// referencing FieldReference shape as Department.parent_department_id
// above.
func Position() *entity.Definition {
	return &entity.Definition{
		EntityType: "Position",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true},
			{Name: "department_id", Type: entity.FieldReference, Target: "Department"},
			{Name: "reports_to_position_id", Type: entity.FieldReference, Target: "Position"},
		},
	}
}

// Delegation authorises delegate_user_id to act as an approver anywhere
// delegator_user_id could (R17 "delegation/substitute approver" —
// uc-infra#8). Deliberately narrow: it is consulted ONLY by workflow
// approval-eligibility resolution (internal/api/workflow.go's
// userMeetsApproval, via ActiveDelegatorsFor in roles.go) — it does NOT
// grant the delegate the delegator's Permission/FieldPermission
// entity/field access, admin status, or anything else
// internal/kernel/authz resolves. A substitute approver gets no access
// beyond standing in for approvals the delegator's OWN role/department
// grants would already have covered; full impersonation would be a
// materially different, bigger feature and is deliberately not what
// this is (see uc-infra ADR-0006's delegation addendum).
//
// Non-transitive by construction: A delegating to B does not let B's own
// delegate C stand in for A — ActiveDelegatorsFor only ever resolves one
// hop, checking each delegator's OWN role grants, never anyone who has
// in turn delegated to that delegator.
//
// ends_at is optional (FieldDate, inclusive through the end of that
// day in the server's own local time zone — see ActiveDelegatorsFor in
// roles.go) — empty means the delegation runs indefinitely until
// explicitly revoked. There is no automatic expiry tied to a real
// return-from-leave event: when this was written no Employee/
// LeaveRequest entity existed to detect one, and both now do (hr,
// board #16, ADR-0013), so leave-aware expiry is buildable — it is
// simply not built. This stays a manually-toggled substitute-approver
// switch, not leave-aware automation.
//
// revoked is the immediate off switch — set true to end a delegation
// before its ends_at (or an indefinite one) without deleting the row, so
// its history stays visible in the generated UI rather than erased.
//
// Self-delegation (delegator_user_id == delegate_user_id) is not
// rejected at validation time — internal/kernel/entity has no generic
// cross-field inequality check (CLAUDE.md's kernel-boundary rule keeps
// that engine free of anything entity-specific), and it would be inert
// anyway: ActiveDelegatorsFor excludes it explicitly, since it would
// only grant a user standing they already hold in their own right.
//
// Escalation timers — the other half of uc-infra#8's original ticket —
// are NOT this entity. They need a per-job elapsed-time sweep, a
// different mechanism entirely, and are split into their own follow-up
// backlog issue rather than bundled in here.
func Delegation() *entity.Definition {
	return &entity.Definition{
		EntityType: "Delegation",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "delegator_user_id", Type: entity.FieldString, Required: true},
			{Name: "delegate_user_id", Type: entity.FieldString, Required: true},
			{Name: "ends_at", Type: entity.FieldDate},
			{Name: "revoked", Type: entity.FieldBool},
		},
	}
}

// All returns every foundation Definition — the set that must exist
// before any operational module is enabled for a tenant.
func All() []*entity.Definition {
	return []*entity.Definition{
		Party(),
		PartyRole(),
		PartyRelationship(),
		Address(),
		ContactMechanism(),
		Attachment(),
		UnitOfMeasure(),
		UomConversion(),
		Currency(),
		ExchangeRate(),
		StatusType(),
		Status(),
		StatusTransition(),
		IssueReport(),
		AIProviderConnection(),
		ExternalSQLSource(),
		ExternalIdentity(),
		SystemOfRecord(),
		Role(),
		UserRole(),
		Permission(),
		FieldPermission(),
		Department(),
		Position(),
		Delegation(),
	}
}
