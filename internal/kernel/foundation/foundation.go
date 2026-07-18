// Package foundation seeds the always-on base entities every module
// depends on (ADR-0001 §8, reference-data-model.md §0): the
// Party–Role–Relationship pattern, the generic Attachment entity any
// record can carry files against, and the cross-cutting entities (unit of
// measure, currency) that Sales, Procurement, Inventory, and Manufacturing
// all reference. These ship with the kernel, not as an optional module —
// a tenant licensing only one operational module still needs a Party to
// exist.
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
		Version:    1,
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
		Version:    1,
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
		Version:    1,
		Fields: []entity.Field{
			{Name: "party_id_from", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "party_id_to", Type: entity.FieldReference, Required: true, Target: "Party"},
			{Name: "relationship_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"employs", "supplies", "parent_of"}},
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
		Version:    1,
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
		Version:    1,
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true},
			{Name: "name", Type: entity.FieldString, Required: true},
		},
	}
}

// Currency is a base currency; ExchangeRate (date-effective rates) is a
// separate entity referencing it, not modeled in this first increment.
func Currency() *entity.Definition {
	return &entity.Definition{
		EntityType: "Currency",
		Version:    1,
		Fields: []entity.Field{
			{Name: "code", Type: entity.FieldString, Required: true}, // ISO 4217, e.g. "QAR", "USD"
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "minor_unit", Type: entity.FieldNumber, Default: float64(2)},
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
		Attachment(),
		UnitOfMeasure(),
		Currency(),
	}
}
