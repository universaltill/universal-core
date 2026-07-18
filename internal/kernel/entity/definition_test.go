package entity

import "testing"

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
