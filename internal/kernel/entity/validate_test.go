package entity

import (
	"errors"
	"math"
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

	t.Run("wrong type bool", func(t *testing.T) {
		data := map[string]any{"name": "Acme", "active": "yes"}
		err := ValidateRecord(def, data)
		if err == nil {
			t.Fatal("expected error for wrong type on active")
		}
		if !strings.Contains(err.Error(), "expected bool") {
			t.Fatalf("expected the bool shape error, got: %v", err)
		}
	})

	t.Run("enum value not allowed", func(t *testing.T) {
		data := map[string]any{"name": "Acme", "payment_terms": "cash"}
		if err := ValidateRecord(def, data); err == nil {
			t.Fatal("expected error for invalid enum value")
		}
	})

	t.Run("enum value wrong type", func(t *testing.T) {
		// The enum's underlying carrier is a string; a non-string value
		// fails the shape check before EnumValues is even consulted.
		data := map[string]any{"name": "Acme", "payment_terms": float64(1)}
		err := ValidateRecord(def, data)
		if err == nil {
			t.Fatal("expected error for non-string enum value")
		}
		if !strings.Contains(err.Error(), "expected string for enum") {
			t.Fatalf("expected the enum shape error, got: %v", err)
		}
	})

	t.Run("optional field omitted is fine", func(t *testing.T) {
		data := map[string]any{"name": "Acme"}
		if err := ValidateRecord(def, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// #86: a direct JSON call (unlike the form handler and CSV importer,
	// which both convert a blank input to absent before validating) can
	// submit a required field as "" rather than omitting it. Required
	// must reject that the same as missing/nil, or every required
	// FieldString/FieldReference/FieldDate is satisfiable with no value.
	t.Run("required field submitted as empty string is rejected", func(t *testing.T) {
		data := map[string]any{"name": ""}
		if err := ValidateRecord(def, data); err == nil {
			t.Fatal("expected error for required field 'name' submitted as \"\"")
		}
	})

	t.Run("required FieldReference submitted as empty string is rejected", func(t *testing.T) {
		refDef := &Definition{
			EntityType: "InventoryMovement",
			Version:    1,
			Fields: []Field{
				{Name: "facility_id", Type: FieldReference, Target: "Facility", Required: true},
			},
		}
		if err := ValidateRecord(refDef, map[string]any{"facility_id": ""}); err == nil {
			t.Fatal("expected error for required reference field submitted as \"\"")
		}
	})

	t.Run("required bool false and required number zero are not treated as empty", func(t *testing.T) {
		// A naive rewrite of the Required check (e.g. reflect.IsZero, or
		// fmt.Sprint(v) == "") would wrongly reject these; only "" itself
		// (a string) counts as empty.
		zeroDef := &Definition{
			EntityType: "Vendor",
			Version:    1,
			Fields: []Field{
				{Name: "active", Type: FieldBool, Required: true},
				{Name: "lead_time_days", Type: FieldNumber, Required: true},
			},
		}
		data := map[string]any{"active": false, "lead_time_days": float64(0)}
		if err := ValidateRecord(zeroDef, data); err != nil {
			t.Fatalf("required bool=false / number=0 must validate as present, got: %v", err)
		}
	})

	t.Run("optional string field submitted as empty string still passes", func(t *testing.T) {
		// Only Required tightens on "" — an optional string field keeps
		// accepting "" as a legitimate value, same as before this fix.
		optDef := &Definition{
			EntityType: "Vendor",
			Version:    1,
			Fields: []Field{
				{Name: "name", Type: FieldString, Required: true},
				{Name: "notes", Type: FieldString},
			},
		}
		data := map[string]any{"name": "Acme", "notes": ""}
		if err := ValidateRecord(optDef, data); err != nil {
			t.Fatalf("unexpected error for optional string field left blank: %v", err)
		}
	})
}

// boundedDef declares Min-only, Max-only and Min+Max FieldNumber fields —
// the three shapes ADR-0018 §4's Min/Max mechanism supports.
func boundedDef() *Definition {
	return &Definition{
		EntityType: "LeaveRequest",
		Version:    1,
		Fields: []Field{
			{Name: "days", Type: FieldNumber, Min: Float64Ptr(0)},
			{Name: "probability", Type: FieldNumber, Min: Float64Ptr(0), Max: Float64Ptr(100)},
			{Name: "discount", Type: FieldNumber, Max: Float64Ptr(0)},
			{Name: "unbounded", Type: FieldNumber},
		},
	}
}

// TestValidateRecord_MinMax (uc-infra#80, ADR-0018 §4): entity.Field's
// Min/Max bounds are inclusive and enforced per-field, independent of any
// other declared bound.
func TestValidateRecord_MinMax(t *testing.T) {
	def := boundedDef()

	t.Run("below min is rejected", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"days": -99.0}); err == nil {
			t.Fatal("expected error for days below its Min")
		}
	})
	t.Run("at min is accepted (inclusive)", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"days": 0.0}); err != nil {
			t.Fatalf("unexpected error at the inclusive minimum: %v", err)
		}
	})
	t.Run("above min with no max is accepted", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"days": 1_000_000.0}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("above max is rejected", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"probability": 10000.0}); err == nil {
			t.Fatal("expected error for probability above its Max")
		}
	})
	t.Run("at max is accepted (inclusive)", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"probability": 100.0}); err != nil {
			t.Fatalf("unexpected error at the inclusive maximum: %v", err)
		}
	})
	t.Run("below min on a percentage field is rejected", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"probability": -400.0}); err == nil {
			t.Fatal("expected error for probability below its Min")
		}
	})
	t.Run("max-only field rejects above max", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"discount": 1.0}); err == nil {
			t.Fatal("expected error: discount has Max:0 and no Min")
		}
	})
	t.Run("max-only field accepts any value below max", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"discount": -1_000_000.0}); err != nil {
			t.Fatalf("unexpected error: a Max-only field has no floor: %v", err)
		}
	})
	t.Run("unbounded field accepts any value", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"unbounded": -1e18}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("int and int64 values are bound-checked like float64", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"days": -1}); err == nil {
			t.Fatal("expected error for an int value below Min")
		}
		if err := ValidateRecord(def, map[string]any{"days": int64(-1)}); err == nil {
			t.Fatal("expected error for an int64 value below Min")
		}
	})
	t.Run("NaN is rejected even on an unbounded field", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"unbounded": math.NaN()}); err == nil {
			t.Fatal("expected error: NaN is never a valid FieldNumber value")
		}
	})
	t.Run("+Inf is rejected", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"unbounded": math.Inf(1)}); err == nil {
			t.Fatal("expected error: +Inf is never a valid FieldNumber value")
		}
	})
	t.Run("-Inf is rejected", func(t *testing.T) {
		if err := ValidateRecord(def, map[string]any{"unbounded": math.Inf(-1)}); err == nil {
			t.Fatal("expected error: -Inf is never a valid FieldNumber value")
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

	// uc-infra#104: a required i18n_text can be PRESENT yet structurally
	// valid while carrying no content — a direct JSON API caller sending
	// {} or every locale blank, or a CSV/XLSX cell whose text is
	// literally "{}" (a genuinely blank form field or CSV cell is
	// already caught upstream as absent — see validate.go's comment on
	// this same check). None of those trip the `!present || v == nil`
	// guard, so {} (or every locale blank) must still fail Required, the
	// same as an absent field would.
	if err := ValidateRecord(reqDef, map[string]any{"label": map[string]any{}}); err == nil {
		t.Fatal("expected a required i18n_text with an empty {} object to fail validation")
	}
	if err := ValidateRecord(reqDef, map[string]any{"label": map[string]any{"en": ""}}); err == nil {
		t.Fatal("expected a required i18n_text with every locale blank to fail validation")
	}
	if err := ValidateRecord(reqDef, map[string]any{"label": map[string]any{"en": "", "tr": ""}}); err == nil {
		t.Fatal("expected a required i18n_text with every locale blank (multiple locales) to fail validation")
	}
	// At least one non-blank locale satisfies Required, even if others
	// are blank — "no Turkish translation yet" is not the same gap as
	// "no translation in any language at all".
	if err := ValidateRecord(reqDef, map[string]any{"label": map[string]any{"en": "", "tr": "Proje"}}); err != nil {
		t.Fatalf("expected a required i18n_text with one non-blank locale to pass, got: %v", err)
	}
	// The error carries the same Kind/shape as every other Required
	// rejection — uc-infra#96's translation layer keys purely on Kind,
	// so this needs no new catalog entry.
	err := ValidateRecord(reqDef, map[string]any{"label": map[string]any{}})
	var verr *ValidationError
	if err == nil || !errors.As(err, &verr) || verr.Kind != KindRequired || verr.FieldName != "label" {
		t.Fatalf("expected a KindRequired ValidationError naming %q, got: %v", "label", err)
	}

	// Optional i18n_text is untouched by this: an empty {} (or all-blank
	// locales) stays valid when the field isn't Required — this is
	// exactly the shape-only behavior documented above, unchanged.
	if err := ValidateRecord(def, map[string]any{"label": map[string]any{"en": ""}}); err != nil {
		t.Fatalf("optional i18n_text with a blank locale value should still be valid: %v", err)
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

// TestValidateRecord_NotBefore_DanglingChainReference covers
// validateNotBefore's defensive `fieldByName[name]` miss: reachable only
// when a field's NotBefore chain walks to a name the Definition doesn't
// actually declare. Definition.Validate rejects a dangling NotBefore
// reference at publish time (TestDefinitionValidate's own
// "not_before_referencing_a_nonexistent_field" case), so this
// deliberately builds the Definition without calling Validate first —
// the same "ad-hoc Definition bypassing the publish gate" shape
// TestValidateRecord_ValidationErrorStructured's "invalid" subtest uses
// for KindInvalid.
func TestValidateRecord_NotBefore_DanglingChainReference(t *testing.T) {
	def := &Definition{
		EntityType: "ChainThing",
		Fields: []Field{
			{Name: "b", Type: FieldDate, NotBefore: "a"}, // "a" is not a declared field
		},
	}
	if err := ValidateRecord(def, map[string]any{"b": "2026-07-10"}); err != nil {
		t.Fatalf("a NotBefore chain reference to an undeclared field should be tolerated (nothing to compare), got: %v", err)
	}
}

// TestValidateRecord_ValidationErrorStructured pins the structured shape
// (uc-infra#96) that internal/api's writeValidationErrorLocalized relies
// on to translate a ValidateRecord failure instead of showing the
// English, snake_case Detail text verbatim in a form-save toast. Every
// branch of ValidateRecord/validateFieldValue/validateNotBefore must
// return a *ValidationError with the right Kind/EntityType/FieldName (and
// OtherField for KindNotBefore) — a plain fmt.Errorf regression here
// would compile fine (both satisfy `error`) but silently break
// translation for that one failure kind, so this asserts the concrete
// type via errors.As on every kind, not just err != nil.
func TestValidateRecord_ValidationErrorStructured(t *testing.T) {
	asValidationError := func(t *testing.T, err error) *ValidationError {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("expected *ValidationError, got %T: %v", err, err)
		}
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected errors.Is(err, ErrValidation) to hold, got: %v", err)
		}
		return verr
	}

	t.Run("required", func(t *testing.T) {
		def := &Definition{EntityType: "Vendor", Fields: []Field{{Name: "name", Type: FieldString, Required: true}}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{}))
		if verr.Kind != KindRequired || verr.EntityType != "Vendor" || verr.FieldName != "name" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})

	t.Run("type mismatch", func(t *testing.T) {
		def := &Definition{EntityType: "Vendor", Fields: []Field{{Name: "lead_time_days", Type: FieldNumber}}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{"lead_time_days": "sixty"}))
		if verr.Kind != KindTypeMismatch || verr.EntityType != "Vendor" || verr.FieldName != "lead_time_days" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})

	t.Run("enum invalid", func(t *testing.T) {
		def := &Definition{EntityType: "Vendor", Fields: []Field{{Name: "payment_terms", Type: FieldEnum, EnumValues: []string{"LC"}}}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{"payment_terms": "cash"}))
		if verr.Kind != KindEnumInvalid || verr.EntityType != "Vendor" || verr.FieldName != "payment_terms" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})

	t.Run("i18n_text invalid", func(t *testing.T) {
		def := &Definition{EntityType: "Unit", Fields: []Field{{Name: "label", Type: FieldI18nText}}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{"label": "Each"}))
		if verr.Kind != KindI18nTextInvalid || verr.EntityType != "Unit" || verr.FieldName != "label" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})

	t.Run("not before", func(t *testing.T) {
		def := &Definition{EntityType: "PurchaseOrder", Fields: []Field{
			{Name: "ordered_at", Type: FieldDate},
			{Name: "shipped_at", Type: FieldDate, NotBefore: "ordered_at"},
		}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{"ordered_at": "2026-07-22", "shipped_at": "2026-07-01"}))
		if verr.Kind != KindNotBefore || verr.EntityType != "PurchaseOrder" || verr.FieldName != "shipped_at" || verr.OtherField != "ordered_at" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})

	t.Run("invalid (unknown field type, defensive-only)", func(t *testing.T) {
		def := &Definition{EntityType: "Vendor", Fields: []Field{{Name: "risk_level", Type: FieldType("no_such_type")}}}
		verr := asValidationError(t, ValidateRecord(def, map[string]any{"risk_level": "high"}))
		if verr.Kind != KindInvalid || verr.EntityType != "Vendor" || verr.FieldName != "risk_level" {
			t.Fatalf("unexpected fields: %+v", verr)
		}
	})
}
