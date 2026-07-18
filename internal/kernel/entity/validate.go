package entity

import (
	"fmt"
	"slices"
)

// ValidateRecord checks a record's data against its Definition — the
// server-side half of "validation is defined once, applied identically
// client- and server-side" (ADR-0017 §5). It never inspects entity_type
// by name; only the Definition's declared fields drive it.
func ValidateRecord(def *Definition, data map[string]any) error {
	for _, f := range def.Fields {
		v, present := data[f.Name]
		if !present || v == nil {
			if f.Required {
				return fmt.Errorf("field %q is required", f.Name)
			}
			continue
		}
		if err := validateFieldValue(f, v); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
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
	default:
		return fmt.Errorf("unknown field type %q", f.Type)
	}
	return nil
}
