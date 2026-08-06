package entity

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// ValidationErrorKind classifies a ValidateRecord failure so a caller
// that renders it to an end user (internal/api) can translate it —
// CLAUDE.md's "no hardcoded user-facing strings" applies to
// ValidateRecord's errors exactly like it does to
// crud.TargetConstraintError (see that type's own doc comment, which
// this mirrors): this package stays generic and English-only
// internally (no i18n import here — that would pull a UI concern into
// the kernel), and internal/api maps Kind + FieldName (and, for
// KindNotBefore, OtherField) to a translated message via the i18n
// catalog, the same field.{EntityType}.{FieldName} label lookup form
// rendering and target-constraint errors already use.
type ValidationErrorKind string

const (
	// KindRequired: a Required field was omitted or submitted as nil.
	KindRequired ValidationErrorKind = "required"
	// KindTypeMismatch: the submitted value's Go type doesn't match
	// what the field's declared Type expects (string/number/bool, or
	// FieldEnum's underlying string carrier).
	KindTypeMismatch ValidationErrorKind = "type_mismatch"
	// KindEnumInvalid: a FieldEnum value that IS a string, but isn't
	// one of Field.EnumValues.
	KindEnumInvalid ValidationErrorKind = "enum_invalid"
	// KindI18nTextInvalid: a FieldI18nText value that isn't a
	// locale->string object, or has a non-string value for some locale.
	KindI18nTextInvalid ValidationErrorKind = "i18n_text_invalid"
	// KindNotBefore: a FieldDate value is chronologically before the
	// nearest present predecessor named by Field.NotBefore.
	KindNotBefore ValidationErrorKind = "not_before"
	// KindBelowMinimum/KindAboveMaximum: a FieldNumber value violates the
	// Field.Min/Field.Max bound declared on it (uc-infra#80).
	//
	// Their catalog messages deliberately do NOT name the bound itself
	// ("is below the allowed minimum", not "must be at least 0"): this
	// error's translation layer substitutes {field} and, for
	// KindNotBefore only, {other_field} — there is no mechanism for
	// interpolating a numeric parameter, and inventing one belongs in its
	// own change rather than in the merge that first brought Min/Max and
	// the translation layer together. The untranslated Detail still
	// carries the exact bound for logs and API callers.
	KindBelowMinimum ValidationErrorKind = "below_minimum"
	KindAboveMaximum ValidationErrorKind = "above_maximum"
	// KindInvalid is the defensive catch-all for a field whose Type
	// isn't one ValidateRecord knows how to check at all. Definition.
	// Validate rejects an unknown FieldType at publish time, so this
	// only fires for a Definition built without going through Validate
	// first (e.g. an ad-hoc one in a test) — a real end user submitting
	// a form against a published Definition can never trigger it, but
	// it still gets a translated message rather than an English
	// fmt.Errorf leaking through in the one place a corrupt Definition
	// could reach this far.
	KindInvalid ValidationErrorKind = "invalid"
)

// AllValidationErrorKinds lists every ValidationErrorKind ValidateRecord
// can produce. internal/api's own i18n coverage test walks this to
// confirm every Kind has a translated entity.validation.{kind} catalog
// key in every shipped locale — the same convention
// internal/kernel/formrender/i18n_coverage_test.go established for
// section titles and the Save action (universal-core/CLAUDE.md's i18n
// section). Without it, a typo'd or newly-added Kind with no matching
// catalog entry would put the literal key ("entity.validation.foo") on
// a user's screen with no build-time signal — Catalog.T's fallback is
// silent by design for exactly this failure mode (independent review,
// uc-infra#96).
func AllValidationErrorKinds() []ValidationErrorKind {
	return []ValidationErrorKind{
		KindRequired,
		KindTypeMismatch,
		KindEnumInvalid,
		KindI18nTextInvalid,
		KindNotBefore,
		KindBelowMinimum,
		KindAboveMaximum,
		KindInvalid,
	}
}

// ErrValidation is the sentinel every *ValidationError unwraps to, for a
// caller that only needs errors.Is(err, ErrValidation) (tests, logs) —
// mirrors crud.ErrTargetConstraintViolation/TargetConstraintError's own
// sentinel-plus-struct split.
var ErrValidation = errors.New("validation failed")

// ValidationError is what ValidateRecord actually returns (still typed
// as the plain `error` interface, so every existing errors.Is/err != nil
// caller is unaffected). Detail is the untranslated, English,
// developer/log-facing message — kept identical in shape to
// ValidateRecord's pre-existing fmt.Errorf text so log lines and any
// caller inspecting Error() directly don't change behavior. Kind,
// EntityType, FieldName and (for KindNotBefore) OtherField are what a
// caller renders instead, for an end user, via internal/api's
// writeValidationErrorLocalized.
type ValidationError struct {
	Kind       ValidationErrorKind
	EntityType string
	FieldName  string
	OtherField string // set only for KindNotBefore: the earlier field being compared against
	Detail     string
}

func (e *ValidationError) Error() string { return e.Detail }

// Unwrap makes errors.Is(err, ErrValidation) match regardless of Kind.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// ValidateRecord checks a record's data against its Definition — the
// server-side half of "validation is defined once, applied identically
// client- and server-side" (ADR-0017 §5). It never inspects entity_type
// by name; only the Definition's declared fields drive it.
func ValidateRecord(def *Definition, data map[string]any) error {
	for _, f := range def.Fields {
		v, present := data[f.Name]
		absent := !present || v == nil
		// Required additionally rejects an empty string (uc-infra#86):
		// the form handler and CSV importer both convert a blank input to
		// absent before it ever reaches here, but a direct JSON call can
		// submit "" instead of omitting the key. `v == ""` only matches a
		// `v` whose dynamic type is string, so a required bool `false` or
		// number `0` is unaffected — this tightens Required only, never
		// loosens the type check below for an optional field's `""`.
		//
		// Returns the same structured *ValidationError (KindRequired) the
		// absent case has used since uc-infra#96's translation layer
		// landed, NOT the bare fmt.Errorf this check originally shipped
		// with — the two are the same failure to a user, so they must
		// localize identically rather than one leaking raw English.
		if f.Required && (absent || v == "") {
			return &ValidationError{
				Kind:       KindRequired,
				EntityType: def.EntityType,
				FieldName:  f.Name,
				Detail:     fmt.Sprintf("field %q is required", f.Name),
			}
		}
		if absent {
			continue
		}
		if verr := validateFieldValue(def.EntityType, f, v); verr != nil {
			return verr
		}
		// A required i18n_text can be PRESENT yet structurally valid
		// (per ADR-0009, {} is a legal "no translation yet" shape) while
		// still carrying no actual content — e.g. a direct JSON API
		// caller sending {"name":{}} or {"name":{"en":""}}, or a CSV/XLSX
		// cell whose text is literally "{}" (a genuinely blank CSV cell
		// is already caught upstream as absent — csvimport.buildRowData
		// skips it before Coerce ever runs, and the generated HTML form
		// does the same for an all-blank field — handlers.go's
		// parseRecordFields). None of those trip the presence check
		// above, so Required must also mean "at least one locale has
		// content" for this field type specifically — checked here,
		// after the generic shape check, rather than in
		// validateFieldValue, which stays structural-only per ADR-0009
		// and must not know about Required (uc-infra#104). Reuses
		// KindRequired/the identical Detail shape the presence check
		// above uses, so uc-infra#96's translation layer (keyed on Kind)
		// needs no new catalog entry for this.
		if f.Required && f.Type == FieldI18nText && !i18nTextHasContent(v) {
			return &ValidationError{
				Kind:       KindRequired,
				EntityType: def.EntityType,
				FieldName:  f.Name,
				Detail:     fmt.Sprintf("field %q is required", f.Name),
			}
		}
	}
	// Note (independent review, uc-infra#104): this is intentionally
	// LOOSER than formrender's client-side rule for the same field —
	// formrender marks only the fallback/primary locale's input as the
	// browser's `required` attribute (render.go: "a required multilingual
	// field must be named in the primary language at least"), while this
	// accepts ANY single non-blank locale. That's not a violation of
	// ADR-0017 §5's "validation is defined once" — the browser's
	// `required` never lets an all-blank submission through in the first
	// place, so anything the form actually submits already satisfies this
	// looser server rule too. The gap this function closes is for callers
	// that bypass the form entirely (direct JSON API, CSV/XLSX import),
	// where "primary locale filled in" isn't a precondition to begin
	// with — narrowing the server to match the form's primary-locale-only
	// rule would need a real design pass (which primary locale, for a
	// tenant with none configured?), not a guess folded into this fix.
	var fieldByName map[string]Field
	for _, f := range def.Fields {
		if f.NotBefore == "" {
			continue
		}
		if fieldByName == nil {
			fieldByName = make(map[string]Field, len(def.Fields))
			for _, ff := range def.Fields {
				fieldByName[ff.Name] = ff
			}
		}
		if verr := validateNotBefore(def.EntityType, fieldByName, f, data); verr != nil {
			return verr
		}
	}
	return nil
}

// validateNotBefore enforces Field.NotBefore chronology against the
// NEAREST present predecessor: it walks the NotBefore chain until it
// finds a field with a submitted, parseable value and compares against
// that — so a skipped intermediate stage (a domestic PO with no customs
// step, say) doesn't silently unconstrain everything after it
// (independent review, 2026-07-31: the first draft compared only the
// immediate predecessor, and the generated form drops empty inputs from
// the submitted map, so one blank middle field disabled the check for
// every later one). The remaining, deliberate gaps: a submission that
// omits the constrained field itself or every ancestor has nothing to
// compare, and values that don't parse as dates are skipped rather than
// rejected — FieldDate's own value validation is shape-only (a string),
// and chronology checking shouldn't be the place that tightens it.
// The visited set is belt-and-braces: Definition.Validate rejects
// NotBefore cycles at publish time, but an infinite loop is the wrong
// failure mode to leave reachable from record validation regardless.
func validateNotBefore(entityType string, fieldByName map[string]Field, f Field, data map[string]any) *ValidationError {
	this, okThis := data[f.Name].(string)
	if !okThis || this == "" {
		return nil
	}
	thisT, err := time.Parse("2006-01-02", this)
	if err != nil {
		return nil
	}
	visited := make(map[string]bool, len(fieldByName))
	for name := f.NotBefore; name != "" && !visited[name]; {
		visited[name] = true
		if other, ok := data[name].(string); ok && other != "" {
			otherT, err := time.Parse("2006-01-02", other)
			if err != nil {
				return nil
			}
			if thisT.Before(otherT) {
				return &ValidationError{
					Kind:       KindNotBefore,
					EntityType: entityType,
					FieldName:  f.Name,
					OtherField: name,
					Detail:     fmt.Sprintf("field %q must not be before %q", f.Name, name),
				}
			}
			return nil
		}
		prev, ok := fieldByName[name]
		if !ok {
			return nil
		}
		name = prev.NotBefore
	}
	return nil
}

func validateFieldValue(entityType string, f Field, v any) *ValidationError {
	newErr := func(kind ValidationErrorKind, detail string) *ValidationError {
		return &ValidationError{
			Kind:       kind,
			EntityType: entityType,
			FieldName:  f.Name,
			Detail:     fmt.Sprintf("field %q: %s", f.Name, detail),
		}
	}
	switch f.Type {
	case FieldString, FieldDate, FieldReference:
		if _, ok := v.(string); !ok {
			return newErr(KindTypeMismatch, fmt.Sprintf("expected string, got %T", v))
		}
	case FieldNumber:
		var num float64
		switch n := v.(type) {
		case float64:
			num = n
		case int:
			num = float64(n)
		case int64:
			num = float64(n)
		default:
			return newErr(KindTypeMismatch, fmt.Sprintf("expected number, got %T", v))
		}
		// A bound can't be enforced on a value ordering can't compare:
		// NaN < min and NaN > max are both false, so without this check a
		// NaN would silently sail past any declared Min/Max. Rejected
		// unconditionally, not just when a bound is declared — "not a real
		// number" is never a valid FieldNumber value, bound or no bound,
		// and this is the one place in the kernel that decides that
		// (depreciation.go's MinorUnits has had to guard against exactly
		// this reaching it from an unbounded field).
		if math.IsNaN(num) || math.IsInf(num, 0) {
			return newErr(KindTypeMismatch, fmt.Sprintf("value %v is not a finite number", v))
		}
		if f.Min != nil && num < *f.Min {
			return newErr(KindBelowMinimum, fmt.Sprintf("value %v is below the minimum %v", num, *f.Min))
		}
		if f.Max != nil && num > *f.Max {
			return newErr(KindAboveMaximum, fmt.Sprintf("value %v is above the maximum %v", num, *f.Max))
		}
	case FieldBool:
		if _, ok := v.(bool); !ok {
			return newErr(KindTypeMismatch, fmt.Sprintf("expected bool, got %T", v))
		}
	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			return newErr(KindTypeMismatch, fmt.Sprintf("expected string for enum, got %T", v))
		}
		if !slices.Contains(f.EnumValues, s) {
			return newErr(KindEnumInvalid, fmt.Sprintf("value %q not in enum %v", s, f.EnumValues))
		}
	case FieldI18nText:
		// Structural only (ADR-0009): a JSON object of locale -> string.
		// Which locales are valid is deliberately NOT checked here — that
		// would couple this generic engine to the i18n catalog; the
		// render/API layer only ever offers real locales. An empty object
		// is allowed (it reads back as "no translation", same as an unset
		// field), so this is purely a shape check.
		m, ok := v.(map[string]any)
		if !ok {
			return newErr(KindI18nTextInvalid, fmt.Sprintf("expected an object of locale->string for i18n_text, got %T", v))
		}
		for loc, val := range m {
			if _, ok := val.(string); !ok {
				return newErr(KindI18nTextInvalid, fmt.Sprintf("i18n_text value for locale %q must be a string, got %T", loc, val))
			}
		}
	default:
		return newErr(KindInvalid, fmt.Sprintf("unknown field type %q", f.Type))
	}
	return nil
}

// i18nTextHasContent reports whether an i18n_text value — already
// shape-validated by validateFieldValue at this point, so safely a
// map[string]any of locale->string — has at least one locale with a
// non-empty string. Only called for Required fields (see ValidateRecord);
// an optional i18n_text stays governed by validateFieldValue's
// structural-only ADR-0009 check regardless of what this returns.
//
// A whitespace-only string ("   ") counts as content here, matching this
// field's pre-existing baseline for what "non-empty" means. Whether
// whitespace-only should also fail Required is uc-infra#105's gap
// (JSON API vs. CSV import disagreeing on blank for other field types)
// — deliberately not folded in here; that's a distinct, separately
// scoped fix.
func i18nTextHasContent(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, val := range m {
		if s, ok := val.(string); ok && s != "" {
			return true
		}
	}
	return false
}
