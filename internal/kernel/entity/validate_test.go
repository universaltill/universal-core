package entity

import "testing"

func vendorDef() *Definition {
	return &Definition{
		EntityType: "Vendor",
		Version:    1,
		Fields: []Field{
			{Name: "name", Type: FieldString, Required: true},
			{Name: "lead_time_days", Type: FieldNumber},
			{Name: "active", Type: FieldBool},
			{Name: "payment_terms", Type: FieldEnum, EnumValues: []string{"prepaid", "DP", "TT", "LC"}},
		},
	}
}

func TestValidateRecord(t *testing.T) {
	def := vendorDef()

	t.Run("valid record", func(t *testing.T) {
		data := map[string]any{
			"name":           "Acme Textiles",
			"lead_time_days": float64(60),
			"active":         true,
			"payment_terms":  "LC",
		}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		data := map[string]any{"lead_time_days": float64(60)}
		if err := ValidateRecord(def, data); err == nil {
			t.Fatal("expected error for missing required field 'name'")
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		data := map[string]any{"name": "Acme", "lead_time_days": "sixty"}
		if err := ValidateRecord(def, data); err == nil {
			t.Fatal("expected error for wrong type on lead_time_days")
		}
	})

	t.Run("enum value not allowed", func(t *testing.T) {
		data := map[string]any{"name": "Acme", "payment_terms": "cash"}
		if err := ValidateRecord(def, data); err == nil {
			t.Fatal("expected error for invalid enum value")
		}
	})

	t.Run("optional field omitted is fine", func(t *testing.T) {
		data := map[string]any{"name": "Acme"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
