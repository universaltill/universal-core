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

// TestValidateRecord_I18nText covers the i18n_text field type (ADR-0009):
// the structural validation accepts an object of locale->string, and
// rejects a non-object or a non-string value — but deliberately does NOT
// check which locales are valid (that stays at the i18n/render layer).
func TestValidateRecord_I18nText(t *testing.T) {
	def := &Definition{
		EntityType: "Unit",
		Version:    1,
		Fields:     []Field{{Name: "label", Type: FieldI18nText}},
	}

	// A well-formed locale->string object is valid.
	if err := ValidateRecord(def, map[string]any{"label": map[string]any{"en": "Each", "tr": "Adet"}}); err != nil {
		t.Fatalf("valid i18n_text object rejected: %v", err)
	}
	// An empty object is valid (reads back as "no translation").
	if err := ValidateRecord(def, map[string]any{"label": map[string]any{}}); err != nil {
		t.Fatalf("empty i18n_text object rejected: %v", err)
	}
	// An arbitrary/unknown locale key is NOT rejected here — locale
	// validity is the render/API layer's concern, not the kernel's.
	if err := ValidateRecord(def, map[string]any{"label": map[string]any{"zz": "x"}}); err != nil {
		t.Fatalf("kernel must not reject unknown locales: %v", err)
	}
	// A plain string (not an object) is rejected.
	if err := ValidateRecord(def, map[string]any{"label": "Each"}); err == nil {
		t.Fatal("expected a non-object i18n_text value to be rejected")
	}
	// An object whose value isn't a string is rejected.
	if err := ValidateRecord(def, map[string]any{"label": map[string]any{"en": 42}}); err == nil {
		t.Fatal("expected a non-string i18n_text value to be rejected")
	}

	// Required semantics are unchanged: an absent required i18n_text fails.
	reqDef := &Definition{
		EntityType: "Unit",
		Version:    1,
		Fields:     []Field{{Name: "label", Type: FieldI18nText, Required: true}},
	}
	if err := ValidateRecord(reqDef, map[string]any{}); err == nil {
		t.Fatal("expected an absent required i18n_text field to fail validation")
	}
}
