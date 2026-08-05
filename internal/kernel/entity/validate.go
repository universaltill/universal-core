package entity

import (
	"errors"
	"fmt"
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
		if !present || v == nil {
			if f.Required {
				return &ValidationError{
					Kind:       KindRequired,
					EntityType: def.EntityType,
					FieldName:  f.Name,
					Detail:     fmt.Sprintf("field %q is required", f.Name),
				}
			}
			continue
		}
		if verr := validateFieldValue(def.EntityType, f, v); verr != nil {
			return verr
		}
	}
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
		switch v.(type) {
		case float64, int, int64:
		default:
			return newErr(KindTypeMismatch, fmt.Sprintf("expected number, got %T", v))
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
