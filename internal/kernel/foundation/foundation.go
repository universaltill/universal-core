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
	}
}
