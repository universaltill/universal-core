package entity

import (
	"fmt"
	"slices"
	"time"
)

// ValidateRecord checks a record's data against its Definition — the
// server-side half of "validation is defined once, applied identically
// client- and server-side" (ADR-0017 §5). It never inspects entity_type
// by name; only the Definition's declared fields drive it.
func ValidateRecord(def *Definition, data map[string]any) error {
	for _, f := range def.Fields {
		v, present := data[f.Name]
		absent := !present || v == nil
		// Required additionally rejects an empty string (#86): the form
		// handler and CSV importer both convert a blank input to absent
		// before it ever reaches here, but a direct JSON call can submit
		// "" instead of omitting the key. `v == ""` only matches a `v`
		// whose dynamic type is string, so a required bool `false` or
		// number `0` is unaffected — this tightens Required only, never
		// loosens the type check below for an optional field's `""`.
		if f.Required && (absent || v == "") {
			return fmt.Errorf("field %q is required", f.Name)
		}
		if absent {
			continue
		}
		if err := validateFieldValue(f, v); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
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
		if err := validateNotBefore(fieldByName, f, data); err != nil {
			return err
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
func validateNotBefore(fieldByName map[string]Field, f Field, data map[string]any) error {
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
				return fmt.Errorf("field %q must not be before %q", f.Name, name)
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

func validateFieldValue(f Field, v any) error {
	switch f.Type {
	case FieldString, FieldDate, FieldReference:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case FieldNumber:
		switch v.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("expected number, got %T", v)
		}
	case FieldBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected bool, got %T", v)
		}
	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected string for enum, got %T", v)
		}
		if !slices.Contains(f.EnumValues, s) {
			return fmt.Errorf("value %q not in enum %v", s, f.EnumValues)
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
			return fmt.Errorf("expected an object of locale->string for i18n_text, got %T", v)
		}
		for loc, val := range m {
			if _, ok := val.(string); !ok {
				return fmt.Errorf("i18n_text value for locale %q must be a string, got %T", loc, val)
			}
		}
	default:
		return fmt.Errorf("unknown field type %q", f.Type)
	}
	return nil
}
