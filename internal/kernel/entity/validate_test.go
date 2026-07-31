package entity

import (
	"strings"
	"testing"
)

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

// stagedDef is a minimal two-date chronology chain — the same shape
// PurchaseOrder's staged lead-time timestamps (#29) declare, without
// coupling this generic engine's tests to a purchasing entity.
func stagedDef() *Definition {
	return &Definition{
		EntityType: "Shipment",
		Version:    1,
		Fields: []Field{
			{Name: "ordered_at", Type: FieldDate},
			{Name: "shipped_at", Type: FieldDate, NotBefore: "ordered_at"},
		},
	}
}

// TestValidateRecord_NotBefore is the record-level matrix for
// Field.NotBefore (#29) — see validateNotBefore and the Field.NotBefore
// doc comment for the deliberate both-present-only scope every "skipped"
// case below pins.
func TestValidateRecord_NotBefore(t *testing.T) {
	def := stagedDef()

	t.Run("ordered dates pass", func(t *testing.T) {
		data := map[string]any{"ordered_at": "2026-07-01", "shipped_at": "2026-07-22"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("equal dates pass", func(t *testing.T) {
		// NotBefore means "must not precede", not "must strictly follow"
		// — same-day stages are real (goods sourced and production
		// started the same day).
		data := map[string]any{"ordered_at": "2026-07-01", "shipped_at": "2026-07-01"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error for equal dates: %v", err)
		}
	})

	t.Run("reversed dates fail naming both fields", func(t *testing.T) {
		data := map[string]any{"ordered_at": "2026-07-22", "shipped_at": "2026-07-01"}
		err := ValidateRecord(def, data)
		if err == nil {
			t.Fatal("expected error for shipped_at before ordered_at")
		}
		if !strings.Contains(err.Error(), "shipped_at") || !strings.Contains(err.Error(), "ordered_at") {
			t.Fatalf("error must name both fields so a user can fix the right inputs, got: %v", err)
		}
	})

	// The two one-side-missing cases pin the documented partial-update
	// gap (Field.NotBefore's doc comment): the generated form always
	// posts every field so the UI path is fully checked, but an API
	// update that omits one side deliberately bypasses the comparison —
	// ValidateRecord stays pure (never loads the stored record). This is
	// a known, accepted trade, not an oversight; if these ever start
	// failing, the scope of NotBefore changed and its doc comment (and
	// every Definition relying on the old semantics) must be revisited.
	t.Run("constrained field missing passes", func(t *testing.T) {
		data := map[string]any{"ordered_at": "2026-07-22"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("referenced field missing passes", func(t *testing.T) {
		data := map[string]any{"shipped_at": "2026-07-01"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("both missing passes", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty-string side passes", func(t *testing.T) {
		// An emptied form input posts "" — treated as absent, same as the
		// missing cases above.
		data := map[string]any{"ordered_at": "", "shipped_at": "2026-07-01"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unparseable date strings are skipped", func(t *testing.T) {
		// FieldDate's own value validation is shape-only (any string), so
		// chronology checking deliberately doesn't tighten it — see
		// validateNotBefore's doc comment.
		for _, data := range []map[string]any{
			{"ordered_at": "not-a-date", "shipped_at": "2026-07-01"},
			{"ordered_at": "2026-07-22", "shipped_at": "not-a-date"},
			{"ordered_at": "not-a-date", "shipped_at": "also-not-a-date"},
		} {
			if err := ValidateRecord(def, data); err != nil {
				t.Fatalf("unexpected error for %v: %v", data, err)
			}
		}
	})

	t.Run("non-string value fails shape validation not chronology", func(t *testing.T) {
		data := map[string]any{"ordered_at": "2026-07-01", "shipped_at": float64(20260722)}
		err := ValidateRecord(def, data)
		if err == nil {
			t.Fatal("expected the existing shape validation to reject a non-string date")
		}
		if !strings.Contains(err.Error(), "expected string") {
			t.Fatalf("expected the plain shape error, not a chronology one, got: %v", err)
		}
	})

	t.Run("non-string referenced value falls through to shape validation", func(t *testing.T) {
		data := map[string]any{"ordered_at": true, "shipped_at": "2026-07-22"}
		err := ValidateRecord(def, data)
		if err == nil {
			t.Fatal("expected the existing shape validation to reject a non-string date")
		}
		if !strings.Contains(err.Error(), "expected string") {
			t.Fatalf("expected the plain shape error, not a chronology one, got: %v", err)
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

// TestValidateRecord_NotBefore_TransitiveChain (#29 review finding 1):
// the comparison walks to the NEAREST PRESENT predecessor, so a blank
// middle stage (the generated form drops empty inputs from the
// submitted map — a domestic PO with no customs step is the ordinary
// case) does not unconstrain the stages after it.
func TestValidateRecord_NotBefore_TransitiveChain(t *testing.T) {
	def := &Definition{
		EntityType: "ChainThing",
		Fields: []Field{
			{Name: "a", Type: FieldDate},
			{Name: "b", Type: FieldDate, NotBefore: "a"},
			{Name: "c", Type: FieldDate, NotBefore: "b"},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// b absent: c must still be checked against a, transitively.
	err := ValidateRecord(def, map[string]any{"a": "2026-07-20", "c": "2026-07-10"})
	if err == nil {
		t.Fatal("expected reversed c-vs-a (b skipped) to fail — a blank middle stage must not unconstrain later ones")
	}
	if !strings.Contains(err.Error(), `"c"`) || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("error should name the two fields actually compared, got: %v", err)
	}
	// Same shape, ordered: passes.
	if err := ValidateRecord(def, map[string]any{"a": "2026-07-10", "c": "2026-07-20"}); err != nil {
		t.Fatalf("ordered c-vs-a with b skipped should pass: %v", err)
	}
	// Nearest-present wins: b present and consistent, c checked against
	// b (not a) — c equal to b passes even though a is later than
	// nothing (all ordered).
	if err := ValidateRecord(def, map[string]any{"a": "2026-07-01", "b": "2026-07-10", "c": "2026-07-10"}); err != nil {
		t.Fatalf("c equal to nearest predecessor b should pass: %v", err)
	}
	// Unparseable middle value stops the walk (shape laxness owns it).
	if err := ValidateRecord(def, map[string]any{"a": "2026-07-20", "b": "not a date", "c": "2026-07-10"}); err != nil {
		t.Fatalf("unparseable nearest predecessor should skip chronology, got: %v", err)
	}
	// Every ancestor absent: nothing to compare.
	if err := ValidateRecord(def, map[string]any{"c": "2026-07-10"}); err != nil {
		t.Fatalf("no present ancestor should pass: %v", err)
	}
}
