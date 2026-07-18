package formrender

import (
	"fmt"
	"strconv"
	"strings"
)

// evalVisibleIf evaluates a field's visible_if expression against the
// current header field values. The grammar is deliberately closed and
// tiny — "<field> == <literal>" or "<field> != <literal>", literal being a
// quoted string, true/false, or a number — the same "never a scripting
// language" guardrail form.ActionOp's closed op set enforces (ADR-0001
// §6). An empty expression means always visible.
func evalVisibleIf(expr string, fields map[string]any) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}

	// Whichever operator occurs first wins — searching "==" unconditionally
	// before "!=" would misparse an expression like "status != 'a==b'" (the
	// "==" inside the quoted literal would be matched first, corrupting
	// both the field name and the literal).
	eqIdx := strings.Index(expr, "==")
	neIdx := strings.Index(expr, "!=")
	var op string
	var idx int
	switch {
	case eqIdx < 0 && neIdx < 0:
		return false, fmt.Errorf("visible_if %q: unsupported expression (only '==' and '!=' against a literal are supported)", expr)
	case eqIdx < 0 || (neIdx >= 0 && neIdx < eqIdx):
		op, idx = "!=", neIdx
	default:
		op, idx = "==", eqIdx
	}

	name := strings.TrimSpace(expr[:idx])
	if name == "" {
		return false, fmt.Errorf("visible_if %q: missing field name", expr)
	}
	litStr := strings.TrimSpace(expr[idx+len(op):])
	lit, err := parseLiteral(litStr)
	if err != nil {
		return false, fmt.Errorf("visible_if %q: %w", expr, err)
	}

	actual, ok := fields[name]
	equal := ok && looseEqual(actual, lit)
	if op == "==" {
		return equal, nil
	}
	return !equal, nil
}

func parseLiteral(s string) (any, error) {
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1], nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, nil
	}
	return nil, fmt.Errorf("unrecognized literal %q (expected a quoted string, true/false, or a number)", s)
}

// looseEqual compares a record's stored value — a bool, string, or
// json.Unmarshal's float64 for any number — against a parsed literal.
func looseEqual(actual, lit any) bool {
	switch l := lit.(type) {
	case string:
		s, ok := actual.(string)
		return ok && s == l
	case bool:
		b, ok := actual.(bool)
		return ok && b == l
	case float64:
		switch a := actual.(type) {
		case float64:
			return a == l
		case int:
			return float64(a) == l
		}
	}
	return false
}
