package entity

import (
	"strings"
	"testing"
)

func TestDefinitionValidate(t *testing.T) {
	cases := []struct {
		name    string
		def     Definition
		wantErr bool
	}{
		{
			name: "valid simple definition",
			def: Definition{
				EntityType: "Vendor",
				Version:    1,
				Fields: []Field{
					{Name: "name", Type: FieldString, Required: true},
					{Name: "lead_time_days", Type: FieldNumber},
				},
			},
			wantErr: false,
		},
		{
			name:    "missing entity type",
			def:     Definition{Fields: []Field{{Name: "x", Type: FieldString}}},
			wantErr: true,
		},
		{
			name: "duplicate field",
			def: Definition{
				EntityType: "Vendor",
				Fields: []Field{
					{Name: "name", Type: FieldString},
					{Name: "name", Type: FieldString},
				},
			},
			wantErr: true,
		},
		{
			// The "_" namespace is reserved for request metadata like
			// "_version" (internal/api's optimistic-locking extraction) —
			// a declared field can't collide with it.
			name: "reserved underscore-prefixed field name",
			def: Definition{
				EntityType: "Vendor",
				Fields:     []Field{{Name: "_version", Type: FieldString}},
			},
			wantErr: true,
		},
		{
			name: "enum without values",
			def: Definition{
				EntityType: "PurchaseOrder",
				Fields:     []Field{{Name: "payment_method", Type: FieldEnum}},
			},
			wantErr: true,
		},
		{
			name: "reference without target",
			def: Definition{
				EntityType: "PurchaseOrder",
				Fields:     []Field{{Name: "vendor_id", Type: FieldReference}},
			},
			wantErr: true,
		},
		{
			name: "composition without parent_field",
			def: Definition{
				EntityType: "PurchaseOrder",
				Relationships: []Relationship{
					{Name: "lines", Kind: RelationComposition, Target: "POLine"},
				},
			},
			wantErr: true,
		},
		{
			name: "valid composition",
			def: Definition{
				EntityType: "PurchaseOrder",
				Relationships: []Relationship{
					{Name: "lines", Kind: RelationComposition, Target: "POLine", ParentField: "purchase_order_id"},
				},
			},
			wantErr: false,
		},
		{
			// Regression coverage for a real gap independent review
			// found once formrender actually started consulting
			// Default (2026-07-20) — before that a typo'd Default was
			// harmless dead data; now it silently produces a <select>
			// with nothing selected, so it's caught here instead.
			name: "enum default not one of enum_values",
			def: Definition{
				EntityType: "Item",
				Fields: []Field{
					{Name: "item_type", Type: FieldEnum, EnumValues: []string{"stock", "service"}, Default: "actve"},
				},
			},
			wantErr: true,
		},
		{
			name: "enum default is a valid enum value",
			def: Definition{
				EntityType: "Item",
				Fields: []Field{
					{Name: "item_type", Type: FieldEnum, EnumValues: []string{"stock", "service"}, Default: "stock"},
				},
			},
			wantErr: false,
		},
		{
			name: "enum default not a string",
			def: Definition{
				EntityType: "Item",
				Fields: []Field{
					{Name: "item_type", Type: FieldEnum, EnumValues: []string{"stock", "service"}, Default: float64(1)},
				},
			},
			wantErr: true,
		},
		{
			// NotBefore only means anything for a date-vs-date comparison
			// (validateNotBefore time.Parses both sides) — declaring it on
			// any other type is schema drift to fail loud on.
			name: "not_before on a non-date field",
			def: Definition{
				EntityType: "Shipment",
				Fields: []Field{
					{Name: "ordered_at", Type: FieldDate},
					{Name: "qty", Type: FieldNumber, NotBefore: "ordered_at"},
				},
			},
			wantErr: true,
		},
		{
			name: "not_before referencing a nonexistent field",
			def: Definition{
				EntityType: "Shipment",
				Fields: []Field{
					{Name: "shipped_at", Type: FieldDate, NotBefore: "orderd_at"},
				},
			},
			wantErr: true,
		},
		{
			name: "not_before referencing a non-date field",
			def: Definition{
				EntityType: "Shipment",
				Fields: []Field{
					{Name: "carrier", Type: FieldString},
					{Name: "shipped_at", Type: FieldDate, NotBefore: "carrier"},
				},
			},
			wantErr: true,
		},
		{
			name: "not_before referencing itself",
			def: Definition{
				EntityType: "Shipment",
				Fields: []Field{
					{Name: "shipped_at", Type: FieldDate, NotBefore: "shipped_at"},
				},
			},
			wantErr: true,
		},
		{
			// Declaration order is display order, not dependency order
			// (Validate's own second-pass comment) — a chain may point at
			// a field declared later in Fields.
			name: "not_before forward reference to a later-declared field is valid",
			def: Definition{
				EntityType: "Shipment",
				Fields: []Field{
					{Name: "shipped_at", Type: FieldDate, NotBefore: "ordered_at"},
					{Name: "ordered_at", Type: FieldDate},
				},
			},
			wantErr: false,
		},
		{
			name: "status_type_code without a status_id field",
			def: Definition{
				EntityType:     "PurchaseOrder",
				StatusTypeCode: "purchase_order_status",
				Fields:         []Field{{Name: "vendor_id", Type: FieldReference, Target: "Party"}},
			},
			wantErr: true,
		},
		{
			name: "status_type_code with status_id of the wrong field type",
			def: Definition{
				EntityType:     "PurchaseOrder",
				StatusTypeCode: "purchase_order_status",
				Fields:         []Field{{Name: "status_id", Type: FieldString}},
			},
			wantErr: true,
		},
		{
			name: "status_type_code with status_id targeting the wrong entity",
			def: Definition{
				EntityType:     "PurchaseOrder",
				StatusTypeCode: "purchase_order_status",
				Fields:         []Field{{Name: "status_id", Type: FieldReference, Target: "Party"}},
			},
			wantErr: true,
		},
		{
			// A non-Required status_id would let an update that omits it
			// pass entity.ValidateRecord and read as "not touching
			// status" to crud.Engine.ValidateStatusTransition, silently
			// wiping the field instead of being rejected — see
			// Validate()'s status_id Required check.
			name: "status_type_code with status_id not marked Required",
			def: Definition{
				EntityType:     "PurchaseOrder",
				StatusTypeCode: "purchase_order_status",
				Fields:         []Field{{Name: "status_id", Type: FieldReference, Target: "Status"}},
			},
			wantErr: true,
		},
		{
			name: "valid status_type_code with matching required status_id reference",
			def: Definition{
				EntityType:     "PurchaseOrder",
				StatusTypeCode: "purchase_order_status",
				Fields:         []Field{{Name: "status_id", Type: FieldReference, Required: true, Target: "Status"}},
			},
			wantErr: false,
		},
		{
			// #71: a field type outside the declared FieldType allowlist must
			// not silently publish -- downstream code (formrender, csvimport,
			// the JSON API) all switch on Type, and an unknown value's
			// behavior there is undefined.
			name: "unknown field type",
			def: Definition{
				EntityType: "Vendor",
				Fields:     []Field{{Name: "risk_level", Type: FieldType("no_such_type")}},
			},
			wantErr: true,
		},
		{
			name: "empty field type",
			def: Definition{
				EntityType: "Vendor",
				Fields:     []Field{{Name: "risk_level", Type: FieldType("")}},
			},
			wantErr: true,
		},
		{
			name: "all seven valid field types on one definition",
			def: Definition{
				EntityType: "Vendor",
				Fields: []Field{
					{Name: "name", Type: FieldString},
					{Name: "lead_time_days", Type: FieldNumber},
					{Name: "is_preferred", Type: FieldBool},
					{Name: "onboarded_on", Type: FieldDate},
					{Name: "risk_level", Type: FieldEnum, EnumValues: []string{"low", "high"}},
					{Name: "primary_contact_id", Type: FieldReference, Target: "Party"},
					{Name: "notes", Type: FieldI18nText},
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestUnmarshal_RejectsUnknownFieldType (#71): a JSONB row declaring a
// misspelled or unknown "type" — whether hand-authored, from an
// erp_module bundle (ADR-0012), or hand-edited directly in Postgres —
// must fail loud through the same path Validate() now guards, not just
// when constructed as a Go literal.
func TestUnmarshal_RejectsUnknownFieldType(t *testing.T) {
	raw := []byte(`{
		"entity_type": "Vendor",
		"version": 1,
		"fields": [{"name": "risk_level", "type": "risky"}]
	}`)
	if _, err := Unmarshal(raw); err == nil {
		t.Fatal("expected Unmarshal to reject an unknown field type")
	}
}

// TestDefinitionValidate_UnknownTypeReportedBeforeLabelFieldCheck
// (independent review, 2026-08-01): label_field's own check switches on
// the referenced field's Type to judge label-suitability. Before the
// type-allowlist check was moved ahead of it, a label_field pointing at
// a field with an unknown type reported the confusing "is a %s; a label
// must be a string, number, date or enum" message instead of the
// clearer "has unknown type" — this pins the fixed ordering.
func TestDefinitionValidate_UnknownTypeReportedBeforeLabelFieldCheck(t *testing.T) {
	def := &Definition{
		EntityType: "Vendor",
		LabelField: "name",
		Fields:     []Field{{Name: "name", Type: FieldType("no_such_type")}},
	}
	err := def.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected an 'unknown type' error, got: %v", err)
	}
	if strings.Contains(err.Error(), "label must be") {
		t.Fatalf("expected the unknown-type error, not the label-suitability error, got: %v", err)
	}
}

func TestFieldByName(t *testing.T) {
	def := Definition{
		EntityType: "Vendor",
		Fields:     []Field{{Name: "name", Type: FieldString}},
	}
	if _, ok := def.FieldByName("name"); !ok {
		t.Fatal("expected to find field 'name'")
	}
	if _, ok := def.FieldByName("missing"); ok {
		t.Fatal("did not expect to find field 'missing'")
	}
}

// TestDefinitionValidate_NotBeforeCycle (#29 review finding 2): a
// multi-hop not_before cycle must be unpublishable — record validation
// walks these chains, and a cycle would otherwise be silent schema
// nonsense (and, pre-guard, an infinite loop).
func TestDefinitionValidate_NotBeforeCycle(t *testing.T) {
	def := &Definition{
		EntityType: "Cyclic",
		Fields: []Field{
			{Name: "a", Type: FieldDate, NotBefore: "b"},
			{Name: "b", Type: FieldDate, NotBefore: "a"},
		},
	}
	err := def.Validate()
	if err == nil {
		t.Fatal("expected a two-hop not_before cycle to fail Definition.Validate")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got: %v", err)
	}

	// A three-hop cycle reached through a non-cyclic entry point fails
	// too (the entry field's own walk revisits a chain member).
	def = &Definition{
		EntityType: "Cyclic3",
		Fields: []Field{
			{Name: "entry", Type: FieldDate, NotBefore: "a"},
			{Name: "a", Type: FieldDate, NotBefore: "b"},
			{Name: "b", Type: FieldDate, NotBefore: "a"},
		},
	}
	if err := def.Validate(); err == nil {
		t.Fatal("expected a cycle reached via a non-cyclic entry to fail")
	}
}
