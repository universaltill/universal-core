// Package entity implements the generic Entity Definition model (ADR-0017
// §5): one declarative definition per entity type, from which storage,
// CRUD API, validation, and audit are all derived. This package must never
// contain business logic specific to one entity type — that belongs in a
// Definition (data), not in this code (CLAUDE.md).
package entity

import (
	"encoding/json"
	"fmt"
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
	EntityType    string         `json:"entity_type"`
	Version       int            `json:"version"`
	Fields        []Field        `json:"fields"`
	Relationships []Relationship `json:"relationships,omitempty"`
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
	seen := make(map[string]bool, len(d.Fields))
	for _, f := range d.Fields {
		if f.Name == "" {
			return fmt.Errorf("field with empty name in %s", d.EntityType)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field %q in %s", f.Name, d.EntityType)
		}
		seen[f.Name] = true
		if f.Type == FieldEnum && len(f.EnumValues) == 0 {
			return fmt.Errorf("field %q is type enum but has no enum_values", f.Name)
		}
		if f.Type == FieldReference && f.Target == "" {
			return fmt.Errorf("field %q is type reference but has no target", f.Name)
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
