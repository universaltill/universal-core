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
func Party() *entity.Definition {
	return &entity.Definition{
		EntityType: "Party",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"person", "organization"}},
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "tax_id", Type: entity.FieldString},
			{Name: "status", Type: entity.FieldEnum,
				EnumValues: []string{"active", "inactive"}, Default: "active"},
			{Name: "preferred_language", Type: entity.FieldString, Default: "en"},
		},
	}
}

// PartyRole records that a Party holds a given role — many-to-many, so
// one Party can be a vendor and a customer simultaneously (e.g. a
// supplier who also buys after-sales service).
func PartyRole() *entity.Definition {
	return &entity.Definition{
		EntityType: "PartyRole",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "role_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"customer", "vendor", "employee", "contact", "prospect"}},
		},
	}
}

// PartyRelationship models connections between two parties — org charts,
// vendor/subsidiary links, employment — with one mechanism instead of a
// bespoke foreign key per module.
func PartyRelationship() *entity.Definition {
	return &entity.Definition{
		EntityType: "PartyRelationship",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "party_id_from", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "party_id_to", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "relationship_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"employs", "supplies", "parent_of"}},
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
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "entity_type", Type: entity.FieldString, Required: true},
			{Name: "record_id", Type: entity.FieldString, Required: true},
			{Name: "file_name", Type: entity.FieldString, Required: true},
			{Name: "mime_type", Type: entity.FieldString, Required: true},
			{Name: "size_bytes", Type: entity.FieldNumber, Required: true},
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
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "from_uom_id", Type: entity.FieldReference, Required: true, Target: "UnitOfMeasure"},
			{Name: "to_uom_id", Type: entity.FieldReference, Required: true, Target: "UnitOfMeasure"},
			{Name: "factor", Type: entity.FieldNumber, Required: true}, // to_qty = from_qty × factor
		},
	}
}

// Currency is a base currency; ExchangeRate (date-effective rates) is a
// separate entity referencing it.
func Currency() *entity.Definition {
	return &entity.Definition{
		EntityType: "Currency",
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true}, // ISO 4217, e.g. "QAR", "USD"
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "minor_unit", Type: entity.FieldNumber, Default: float64(2)},
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
		Version:    2,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "from_currency_id", Type: entity.FieldReference, Required: true, Target: "Currency"},
			{Name: "to_currency_id", Type: entity.FieldReference, Required: true, Target: "Currency"},
			{Name: "effective_date", Type: entity.FieldDate, Required: true},
			{Name: "rate", Type: entity.FieldNumber, Required: true}, // to_amount = from_amount × rate
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
func Status() *entity.Definition {
	return &entity.Definition{
		EntityType: "Status",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "status_type_id", Type: entity.FieldReference, Required: true, Target: "StatusType"},
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
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
func IssueReport() *entity.Definition {
	return &entity.Definition{
		EntityType: "IssueReport",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true},
			// description is the field a human actually reads: typed text
			// plus the voice transcript appended, if there was one — see
			// internal/api/issuereport.go's submit handler. transcript
			// below keeps the raw, unedited ASR output separately so a
			// human's own edits to description never lose the original.
			{Name: "description", Type: entity.FieldString, Required: true},
			{Name: "transcript", Type: entity.FieldString},
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
// handler enforces that; the generic entity/crud layer has no unique-
// constraint concept, same "application-level convention, not a DB
// constraint" limitation purchasing.PurchaseOrder.po_number's own doc
// comment already documents for this kernel).
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
		Version:    1,
		Module:     "foundation",
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
		},
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
func UserRole() *entity.Definition {
	return &entity.Definition{
		EntityType: "UserRole",
		Version:    1,
		Module:     "foundation",
		Fields: []entity.Field{
			{Name: "user_id", Type: entity.FieldString, Required: true},
			{Name: "role_id", Type: entity.FieldReference, Required: true, Target: "Role"},
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
		Role(),
		UserRole(),
		Permission(),
		FieldPermission(),
	}
}
