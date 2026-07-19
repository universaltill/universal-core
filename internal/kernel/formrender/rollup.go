package formrender

import "fmt"

// computeRollUp sums a numeric field across a master_detail section's
// child records — the roll-up form.Section already declares (RollUp /
// RollUpTarget) but that nothing evaluates until the renderer.
func computeRollUp(children []map[string]any, field string) (float64, error) {
	var total float64
	for i, child := range children {
		v, ok := child[field]
		if !ok {
			continue
		}
		n, ok := v.(float64)
		if !ok {
			return 0, fmt.Errorf("roll_up field %q on child %d is not numeric (got %T)", field, i, v)
		}
		total += n
	}
	return total, nil
}
