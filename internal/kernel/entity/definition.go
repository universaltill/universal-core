// Package entity implements the generic Entity Definition model (ADR-0017
// §5): one declarative definition per entity type, from which storage,
// CRUD API, validation, and audit are all derived. This package must never
// contain business logic specific to one entity type — that belongs in a
// Definition (data), not in this code (CLAUDE.md).
package entity

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// FieldType enumerates the kinds of field a Definition can declare.
type FieldType string

const (
	FieldString    FieldType = "string"
	FieldNumber    FieldType = "number"
	FieldBool      FieldType = "bool"
	FieldDate      FieldType = "date"
	FieldEnum      FieldType = "enum"
	FieldReference FieldType = "reference" // points at an independently existing entity
	// FieldI18nText holds multilingual record data (ADR-0009): its value is
	// a JSON object keyed by locale, e.g. {"en":"Each","tr":"Adet"}, stored
	// in the same JSONB data column as every other field. Read locale-aware
	// via i18n.Catalog.ResolveLocalized; rendered as one input per locale.
	// The kernel validates it only structurally (an object of strings) and
	// never checks which locales are valid — that stays at the i18n/render
	// layer, keeping this generic engine decoupled from the catalog.
	FieldI18nText FieldType = "i18n_text"
)

// allFieldTypes is the exhaustive set of FieldType values a Definition may
// declare — keep in sync with the const block above and with
// validateFieldValue's own per-type switch (validate.go): a new FieldType
// constant added there but not here fails closed (every definition using
// it becomes unpublishable) rather than silently accepted, but it's still
// two places to remember.
var allFieldTypes = []FieldType{
	FieldString, FieldNumber, FieldBool, FieldDate, FieldEnum, FieldReference, FieldI18nText,
}

func (t FieldType) valid() bool {
	return slices.Contains(allFieldTypes, t)
}

// RelationshipKind distinguishes the three relationship mechanisms named
// in ADR-0017 §6 — they must stay distinct, not folded into one concept.
type RelationshipKind string

const (
	// RelationReference: a field pointing to another independently
	// existing entity (a picker widget).
	RelationReference RelationshipKind = "reference"
	// RelationComposition: master-detail. Detail rows have no existence
	// without the master, saved atomically, roll-up fields recompute.
	RelationComposition RelationshipKind = "composition"
	// RelationRelatedList: a read-only view of other independently
	// existing records, for context/navigation only.
	RelationRelatedList RelationshipKind = "related_list"
)

// Field is one field on an entity.
type Field struct {
	Name       string    `json:"name"`
	Type       FieldType `json:"type"`
	Required   bool      `json:"required,omitempty"`
	Default    any       `json:"default,omitempty"`
	EnumValues []string  `json:"enum_values,omitempty"` // required when Type == FieldEnum
	// Target is the referenced entity type, required when Type == FieldReference.
	Target string `json:"target,omitempty"`
	// VisibleIf is a conditional-visibility expression evaluated against
	// sibling field values, e.g. "payment_method == 'LC'" (ADR-0017 §6).
	VisibleIf string `json:"visible_if,omitempty"`
	// NotBefore names a sibling FieldDate this date field must not
	// precede — a generic, Definition-declared chronology constraint
	// (first user: PurchaseOrder's staged lead-time timestamps, #29,
	// where each stage must not predate the previous one). Only valid
	// on FieldDate, referencing another FieldDate in the same
	// Definition, with no chain cycles (Definition.Validate enforces
	// all three at publish time). ValidateRecord compares against the
	// NEAREST present, parseable predecessor along the chain (see
	// validateNotBefore — a blank middle stage doesn't unconstrain the
	// stages after it). What it deliberately does NOT do is load the
	// stored record: a partial submission that omits the constrained
	// field's own value, or every ancestor's, has nothing to compare
	// and passes — a documented, test-pinned trade to keep
	// ValidateRecord pure; declare chronology only where that's
	// acceptable data hygiene, never as a ledger-grade invariant.
	NotBefore string `json:"not_before,omitempty"`
	// TargetFilter constrains which records of Target are valid values
	// for this FieldReference field — e.g. TimeEntry.employee_id's
	// [{Entity: "PartyRole", EntityField: "party_id", Field: "role_type",
	// Value: "employee"}] restricts the field (and its picker) to a
	// Party that actually holds the employee PartyRole, not any Party
	// whatsoever (uc-infra#78). Only valid on FieldReference; every
	// condition in the slice must hold (AND) for a candidate to qualify.
	// See TargetFilterCondition's own doc comment for what Entity/
	// EntityField mean and why a filter is not always a plain field on
	// Target itself.
	TargetFilter []TargetFilterCondition `json:"target_filter,omitempty"`
	// MustMatchParentField names a field that must exist on BOTH this
	// Definition and Target, whose value on the candidate target record
	// must equal this record's OWN value of the same field — "the
	// target must share a field value with the record doing the
	// referencing" (uc-infra#78). Task.parent_task_id's
	// MustMatchParentField: "project_id" is the motivating case: a
	// parent task must be in the SAME project as the child, so a
	// candidate/submitted parent_task_id is only valid when its own
	// project_id matches the Task being created/updated's project_id.
	// Only valid on FieldReference; the named field must also be
	// declared on this same Definition (Validate below checks that much
	// statically — it cannot check Target's shape, a different
	// Definition entirely, without a registry this package doesn't have
	// access to).
	MustMatchParentField string `json:"must_match_parent_field,omitempty"`
	// Min and Max declare inclusive numeric bounds (ADR-0018 §4) — *float64
	// so "unset" and "zero" stay distinguishable, the same pointer pattern
	// this project already uses for optional declarative constraints (see
	// TargetFilterCondition's own doc comment for the analogous reasoning).
	// Only valid when Type == FieldNumber; Definition.Validate rejects them
	// on any other field type and rejects Min > Max. Generic and
	// declarative, same shape as NotBefore — no entity-type branching, the
	// bound itself lives on the Definition, not in this engine.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// UniqueConstraintName canonicalizes a declared field set into the stable
// name crud's enforcement stage and record_unique_keys both key on: the
// field names sorted, joined by "+". Sorting means {"a","b"} and {"b","a"}
// name the same constraint — Validate below rejects declaring both as
// distinct entries — and gives Dev/Reviewer a name that doesn't depend on
// declaration order. Exported because crud (a different package, by the
// kernel/deterministic-core boundary rule: entity stays generic, crud does
// the database-aware enforcement) needs the exact same name to look up and
// write record_unique_keys rows — computing it twice, differently, would
// silently desync the two.
func UniqueConstraintName(fields []string) string {
	sorted := append([]string(nil), fields...)
	slices.Sort(sorted)
	return strings.Join(sorted, "+")
}

// TargetFilterCondition is one field/value condition a candidate target
// record must satisfy — see Field.TargetFilter.
//
// Grounded in how PartyRole actually models "a Party holds a role"
// (foundation.go): role_type is NOT a field on Party itself, it lives on
// a separate many-to-many PartyRole row (party_id, role_type), the same
// pattern vendor_id/customer_id/employee_id all need to filter by. A
// plain "field on the target record itself" condition cannot express
// that, so this type supports two shapes:
//
//   - Entity == "": Field/Value are checked directly against the
//     candidate target record's own data (a field genuinely declared on
//     Target itself).
//   - Entity != "": a DIFFERENT entity type must hold at least one
//     record where EntityField equals the candidate target's own id AND
//     Field equals Value — the PartyRole join shape above. EntityField
//     is required in this case (Definition.Validate rejects Entity set
//     without it).
//
// Both shapes are evaluated generically (by field name/value, via
// reflection-free map lookups on the generic JSONB record data) —
// nothing here or in the engines that evaluate it names "Party",
// "PartyRole", "employee", or any other entity-specific literal; those
// live only in the Definition that declares a condition (CLAUDE.md's
// kernel/deterministic-core boundary rule).
type TargetFilterCondition struct {
	Entity      string `json:"entity,omitempty"`
	EntityField string `json:"entity_field,omitempty"`
	Field       string `json:"field"`
	Value       string `json:"value"`
}

// Relationship declares a composition or related-list link to another
// entity type — kept structurally distinct from a plain reference Field
// (ADR-0017 §6's three-way split, corrected after conflating two of them
// in an earlier draft).
type Relationship struct {
	Name   string           `json:"name"`
	Kind   RelationshipKind `json:"kind"`
	Target string           `json:"target"` // the child/related entity type
	// ParentField is the field on Target that points back to this entity
	// (required for RelationComposition and RelationRelatedList).
	ParentField string `json:"parent_field,omitempty"`
}

// Definition is one version of an entity type's shape. Stored as
// entity_definitions.definition (JSONB); this Go type is the schema for
// that JSON, not a database model itself.
type Definition struct {
	EntityType string `json:"entity_type"`
	Version    int    `json:"version"`
	// Module is the module key this entity belongs to (e.g.
	// "purchasing", "foundation") — set by the module's own seed
	// package (foundation.go, purchasing.go), used only to group the
	// UI's module switcher/menu (internal/api/nav.go,
	// internal/api/modulemenu.go). Purely descriptive metadata never
	// consulted by a generic engine (crud, formrender) — CLAUDE.md's
	// kernel boundary rule is about behavior, not about a Definition
	// carrying data a UI groups by.
	Module string `json:"module,omitempty"`
	// LabelField names the field that should label a record of this type
	// in a reference picker, a list cell, or anywhere else a human needs
	// to recognise it. Optional: when empty, the renderer falls back to
	// the "name"/"title" convention most Definitions follow.
	//
	// It exists for the entity that deliberately has no such field.
	// hr.Employee is the first (ADR-0013 makes it an employment record —
	// the person's name lives on Party and duplicating it here is the
	// whole thing that design prevents), and without this it rendered as
	// a raw UUID in every picker, list cell and export, AND became
	// unsearchable, because the picker can only sort/filter by a field
	// it knows to be the label. Declaring the label is data, not an
	// entity-type branch in the engine.
	LabelField string `json:"label_field,omitempty"`
	// StatusTypeCode names the foundation StatusType (reference-data-
	// model.md §0's generic "Status/StatusType" pattern — see
	// foundation.StatusType/Status/StatusTransition) that governs this
	// entity's lifecycle, replacing a bespoke per-entity FieldEnum status
	// field (what Party.status/PurchaseOrder.status still are today — not
	// migrated by this field's introduction, see foundation.go's doc
	// comment on StatusType for why). Empty means this entity has no
	// managed lifecycle; crud.Engine.ValidateStatusTransition is a no-op
	// for it. When set, Validate below requires a matching "status_id"
	// FieldReference — the field crud.Engine.ValidateStatusTransition
	// actually reads/writes.
	StatusTypeCode string         `json:"status_type_code,omitempty"`
	Fields         []Field        `json:"fields"`
	Relationships  []Relationship `json:"relationships,omitempty"`
	// QuickCreatable opts this entity into the reference-picker's inline
	// "+ Create new {Entity}" affordance (part 2 of #24,
	// universaltill/uc-infra#51) when a viewer holds create permission
	// on it — internal/api's loadReferenceCreateLabels checks both. Off
	// by default, deliberately: without this, EVERY FieldReference
	// target would be one click away from spontaneous creation,
	// including lookup/lifecycle entities like Status whose fields
	// (is_initial, is_terminal, status_type_id) are graph-shaping, not
	// something a form-filler picking a PurchaseOrder's status should
	// improvise from inside an unrelated modal (found by this feature's
	// own independent review). Set true only where inline creation is
	// genuinely useful for a business-record target (Department's own
	// org-chart parent picker is the first) — same opt-in-per-entity
	// discipline LabelField above already follows, not something a
	// generic engine should default to "everywhere" on inference.
	QuickCreatable bool `json:"quick_creatable,omitempty"`
	// Unique declares one or more sets of field names that must be unique
	// together across every live (non-soft-deleted) record of this entity
	// type — e.g. [][]string{{"employee_id","entry_date"}} on
	// AttendanceRecord rejects a second row for the same employee on the
	// same day (uc-infra#81). A single-element set covers a "natural key"
	// field on its own (employee_number, po_number, Item.sku, ...); this
	// task declares it only on AttendanceRecord's composite case — see
	// ADR-0018 §3 for why this is a Definition-level declaration enforced
	// by crud.Engine (a database-aware stage, not entity.ValidateRecord:
	// deciding uniqueness needs OTHER records, which would break
	// ValidateRecord's deliberate purity, see its own doc comment) rather
	// than a partial unique index generated from metadata (ADR-0018 §3(a),
	// rejected: would mean the metadata layer executing DDL at publish
	// time). A record with any named field absent/nil is exempt from that
	// constraint, mirroring ordinary SQL NULL-in-a-unique-index semantics
	// — Required already covers "must always be present" separately.
	Unique [][]string `json:"unique,omitempty"`
}

// Float64Ptr returns a pointer to v — a small helper so every Definition
// declaring a Field.Min/Max literal doesn't need its own local variable to
// take the address of a constant (Go has no `&0.0` for a literal). Exported
// because every module package declaring bounds needs it, not just this one.
func Float64Ptr(v float64) *float64 { return &v }

// FieldByName returns the field with the given name, if present.
func (d *Definition) FieldByName(name string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Validate checks internal consistency of a Definition — not the data of
// any particular record, just the shape declared. This is what a human
// reviews before approving an AI-drafted definition (ADR-0017 §14).
func (d *Definition) Validate() error {
	if d.EntityType == "" {
		return fmt.Errorf("entity_type is required")
	}
	// Checked before label_field below: label_field's own check switches
	// on the referenced field's Type to judge label-suitability, which
	// would otherwise misreport an unknown type as "not a valid label
	// type" instead of the clearer "unknown type" — independent review,
	// 2026-08-01.
	for _, f := range d.Fields {
		if !f.Type.valid() {
			return fmt.Errorf("field %q in %s has unknown type %q", f.Name, d.EntityType, f.Type)
		}
	}
	if d.LabelField != "" {
		f, ok := d.FieldByName(d.LabelField)
		if !ok {
			return fmt.Errorf("label_field %q is not a field of %s", d.LabelField, d.EntityType)
		}
		// A reference or bool labels nothing a human can read, and an
		// i18n_text label can't drive the picker's sort/filter (the same
		// reason reference_search degrades for one) — so the label must
		// be a plain scalar.
		switch f.Type {
		case FieldString, FieldNumber, FieldDate, FieldEnum:
		default:
			return fmt.Errorf("label_field %q on %s is a %s; a label must be a string, number, date or enum", d.LabelField, d.EntityType, f.Type)
		}
	}
	seen := make(map[string]bool, len(d.Fields))
	for _, f := range d.Fields {
		if f.Name == "" {
			return fmt.Errorf("field with empty name in %s", d.EntityType)
		}
		if strings.HasPrefix(f.Name, "_") {
			// The "_"-prefix namespace is reserved for request metadata a
			// caller round-trips alongside real fields — "_version" is the
			// one that exists today (internal/api's extractVersion,
			// optimistic locking) — not a declarable entity field. Without
			// this, a field genuinely named "_version" would collide with
			// that mechanism: independent review found the two extraction
			// paths (form-encoded vs JSON) don't even agree on which
			// duplicate value wins, and JSON's own two-pass validate/
			// extract order means a Required "_version" field would pass
			// one validation pass and fail the other. Catching it here
			// (fail loud on schema drift, same discipline as every other
			// check in this method) is simpler than making both paths
			// robust to a collision nothing legitimately needs.
			return fmt.Errorf("field %q in %s: names starting with \"_\" are reserved", f.Name, d.EntityType)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field %q in %s", f.Name, d.EntityType)
		}
		seen[f.Name] = true
		if f.Type == FieldEnum && len(f.EnumValues) == 0 {
			return fmt.Errorf("field %q is type enum but has no enum_values", f.Name)
		}
		if f.Type == FieldEnum && f.Default != nil {
			// Default only started being consulted by
			// internal/kernel/formrender's form rendering once reference/
			// enum fields got real "unselected" empty-option handling
			// (2026-07-20) — before that it was declared but inert
			// everywhere, so a typo'd Default was harmless dead data.
			// Now it sets the pre-selected <option>, and a Default that
			// isn't one of EnumValues would silently produce a <select>
			// with nothing selected despite formrender believing a
			// default was applied — catch it at definition-validation
			// time instead, the same "fail loud on schema drift" this
			// method already applies to enum_values/target.
			def, ok := f.Default.(string)
			if !ok {
				return fmt.Errorf("field %q is type enum but default %v is not a string", f.Name, f.Default)
			}
			if !slices.Contains(f.EnumValues, def) {
				return fmt.Errorf("field %q default %q is not one of its enum_values %v", f.Name, def, f.EnumValues)
			}
		}
		if f.Type == FieldReference && f.Target == "" {
			return fmt.Errorf("field %q is type reference but has no target", f.Name)
		}
		if len(f.TargetFilter) > 0 && f.Type != FieldReference {
			return fmt.Errorf("field %q has target_filter but is type %q, not reference", f.Name, f.Type)
		}
		for _, cond := range f.TargetFilter {
			if cond.Field == "" || cond.Value == "" {
				return fmt.Errorf("field %q has a target_filter condition missing field or value", f.Name)
			}
			if cond.Entity != "" && cond.EntityField == "" {
				return fmt.Errorf("field %q has a target_filter condition on entity %q with no entity_field", f.Name, cond.Entity)
			}
			// An entity-join condition's EntityField and Field must be
			// DIFFERENT keys in the equals map crud.ExistsByFieldsQ builds
			// from them ({EntityField: targetID, Field: cond.Value}) —
			// independent review: when they're equal, the second entry
			// silently overwrites the first, collapsing the query to just
			// "does entity=value exist" with no join to the candidate
			// target at all, so the constraint would silently pass for
			// EVERY candidate with no error anywhere. Rejected at
			// definition-validation time (this is data a human reviews
			// before publish, ADR-0017 §14) rather than working around the
			// collapse in the map-based query itself.
			if cond.Entity != "" && cond.EntityField == cond.Field {
				return fmt.Errorf("field %q has a target_filter condition on entity %q whose entity_field and field are both %q — they must name different columns", f.Name, cond.Entity, cond.Field)
			}
		}
		if f.MustMatchParentField != "" {
			if f.Type != FieldReference {
				return fmt.Errorf("field %q has must_match_parent_field but is type %q, not reference", f.Name, f.Type)
			}
			if _, ok := d.FieldByName(f.MustMatchParentField); !ok {
				return fmt.Errorf("field %q must_match_parent_field %q is not a field of %s", f.Name, f.MustMatchParentField, d.EntityType)
			}
		}
		if (f.Min != nil || f.Max != nil) && f.Type != FieldNumber {
			return fmt.Errorf("field %q has min/max but is type %q, not number", f.Name, f.Type)
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return fmt.Errorf("field %q has min %v greater than max %v", f.Name, *f.Min, *f.Max)
		}
	}
	// NotBefore checked in a second pass: it may reference a field
	// declared later in the slice (declaration order is display order,
	// not dependency order).
	fieldTypes := make(map[string]FieldType, len(d.Fields))
	for _, f := range d.Fields {
		fieldTypes[f.Name] = f.Type
	}
	for _, f := range d.Fields {
		if f.NotBefore == "" {
			continue
		}
		if f.Type != FieldDate {
			return fmt.Errorf("field %q has not_before but is type %q, not date", f.Name, f.Type)
		}
		if f.NotBefore == f.Name {
			return fmt.Errorf("field %q has not_before referencing itself", f.Name)
		}
		target, ok := fieldTypes[f.NotBefore]
		if !ok {
			return fmt.Errorf("field %q not_before references %q, which is not a field of %s", f.Name, f.NotBefore, d.EntityType)
		}
		if target != FieldDate {
			return fmt.Errorf("field %q not_before references %q, which is type %q, not date", f.Name, f.NotBefore, target)
		}
	}
	// Cycle check, separate from the per-field checks above: a two-hop
	// cycle (a→b, b→a) passes every individual check yet is schema
	// nonsense — and record validation walks these chains, so a cycle
	// must be unpublishable, not merely odd (independent review,
	// 2026-07-31; the self-reference check above is just this check's
	// one-hop special case, kept for its clearer error message).
	notBefore := make(map[string]string, len(d.Fields))
	for _, f := range d.Fields {
		if f.NotBefore != "" {
			notBefore[f.Name] = f.NotBefore
		}
	}
	for start := range notBefore {
		visited := map[string]bool{}
		for name := start; name != ""; name = notBefore[name] {
			if visited[name] {
				return fmt.Errorf("field %q is part of a not_before cycle in %s", start, d.EntityType)
			}
			visited[name] = true
		}
	}
	// Unique checked in its own pass, same reasoning as NotBefore's: static,
	// shape-only checks that don't need any other record (crud.Engine does
	// the database-aware enforcement — see Unique's own doc comment).
	seenConstraints := make(map[string]bool, len(d.Unique))
	for _, set := range d.Unique {
		if len(set) == 0 {
			return fmt.Errorf("unique constraint in %s has an empty field set", d.EntityType)
		}
		seenInSet := make(map[string]bool, len(set))
		for _, name := range set {
			if _, ok := d.FieldByName(name); !ok {
				return fmt.Errorf("unique constraint in %s references unknown field %q", d.EntityType, name)
			}
			if seenInSet[name] {
				return fmt.Errorf("unique constraint in %s repeats field %q within one constraint", d.EntityType, name)
			}
			seenInSet[name] = true
		}
		name := UniqueConstraintName(set)
		if seenConstraints[name] {
			return fmt.Errorf("unique constraint on fields %v is declared more than once in %s", set, d.EntityType)
		}
		seenConstraints[name] = true
	}
	for _, r := range d.Relationships {
		if r.Target == "" {
			return fmt.Errorf("relationship %q has no target", r.Name)
		}
		if (r.Kind == RelationComposition || r.Kind == RelationRelatedList) && r.ParentField == "" {
			return fmt.Errorf("relationship %q (%s) requires parent_field", r.Name, r.Kind)
		}
	}
	if d.StatusTypeCode != "" {
		sf, ok := d.FieldByName("status_id")
		if !ok {
			return fmt.Errorf("%s declares status_type_code %q but has no status_id field", d.EntityType, d.StatusTypeCode)
		}
		if sf.Type != FieldReference || sf.Target != "Status" {
			return fmt.Errorf("%s: status_id must be a reference field targeting Status, got type %q target %q", d.EntityType, sf.Type, sf.Target)
		}
		if !sf.Required {
			// Required, not just present: crud.Engine.Update replaces a
			// record's data wholesale (data.RecordRepo.UpdateTx), so a
			// caller that simply omits status_id from an update (or a
			// form submission that dropped an emptied field, see
			// internal/api's parseRecordFields) would otherwise pass
			// entity.ValidateRecord and read as "not touching status" to
			// crud.Engine.ValidateStatusTransition, silently wiping
			// status_id off the stored record — the record falls out of
			// its lifecycle instead of being rejected. Required:true
			// makes entity.ValidateRecord itself catch that omission on
			// every update, not just create, before it ever reaches the
			// transition check.
			return fmt.Errorf("%s: status_id must be Required — an optional status_id lets an update silently drop the record's status", d.EntityType)
		}
	}
	return nil
}

// Unmarshal decodes raw (the entity_definitions.definition JSONB column,
// read as plain []byte by internal/data — that package stays generic and
// never imports this one, matching how it already stores plain records'
// data as map[string]any rather than a typed per-entity struct) into a
// Definition and validates it before returning. A definition that made
// it into the registry is
// assumed already-validated at write time, but decoding here re-validates
// anyway: JSONB in Postgres isn't itself schema-checked against this
// Go type, so a row written by a future non-Go writer, or hand-edited in
// the database, must still fail loud rather than hand back a
// Definition this package's own Validate would reject.
func Unmarshal(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("unmarshal entity definition: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("invalid entity definition: %w", err)
	}
	return &d, nil
}
