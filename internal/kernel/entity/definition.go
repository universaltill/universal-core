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
}

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
