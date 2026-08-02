package crud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ErrTargetConstraintViolation is returned by Create/Update when a
// FieldReference field's value points at a record that fails a
// declared Field.TargetFilter condition or Field.MustMatchParentField
// check (uc-infra#78) — e.g. TimeEntry.employee_id pointing at a Party
// that does not hold the employee PartyRole, or Task.parent_task_id
// pointing at a Task in a different project. Same shape as
// ErrReferenceCycle/ErrHookRejected: the caller's own bad input, not a
// server fault, so internal/api's writeCrudError maps it to 400 with
// this error's own text (entity type, field, and exactly what failed —
// the same "safe to describe exactly what's wrong" reasoning those two
// sentinels already established, deliberately NOT run through i18n —
// this file matches their precedent rather than inventing a
// differently-localized error shape for the same family of kernel input
// validation).
var ErrTargetConstraintViolation = errors.New("reference target does not satisfy the field's required constraint")

// valueMatches compares two values pulled from record JSONB data (or a
// TargetFilterCondition's own declared Value, always a Go string) for
// equality regardless of which concrete type each happens to be —
// enum/reference fields decode as string, number fields as float64, so
// a plain `==` would wrongly report a mismatch between, say, the string
// "employee" and itself decoded through two different paths. Comparing
// via fmt.Sprint keeps this generic over any FieldType without a type
// switch that would need to grow every time a new one is added.
func valueMatches(a any, b string) bool {
	if a == nil {
		return false
	}
	return fmt.Sprint(a) == b
}

// checkReferenceTargetConstraints enforces every FieldReference field's
// declared Field.TargetFilter and Field.MustMatchParentField against
// the incoming fields map — the generic, Definition-driven mechanism
// uc-infra#78 asks for: this function never names a specific entity
// type or field; it only walks whatever def.Fields declares.
//
// A field with no value set is skipped (nothing to constrain). A
// target id that does not resolve to a live record is ALSO skipped —
// consistent with this kernel's existing, deliberate posture that a
// dangling FieldReference is not this layer's concern (see
// checkSelfReferenceCycle's own doc comment and ADR-0007): this
// function only judges a target that exists, never FK existence
// itself.
func checkReferenceTargetConstraints(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, def *entity.Definition, fields map[string]any) error {
	for _, f := range def.Fields {
		if f.Type != entity.FieldReference {
			continue
		}
		if len(f.TargetFilter) == 0 && f.MustMatchParentField == "" {
			continue
		}
		targetID, ok := fields[f.Name].(string)
		if !ok || targetID == "" {
			continue
		}

		var target data.Record
		var targetLoaded bool
		loadTarget := func() (data.Record, bool, error) {
			if targetLoaded {
				return target, true, nil
			}
			rec, err := records.GetTx(ctx, tx, f.Target, targetID)
			if err != nil {
				if errors.Is(err, data.ErrNotFound) {
					return data.Record{}, false, nil
				}
				return data.Record{}, false, fmt.Errorf("load %s.%s target %s: %w", def.EntityType, f.Name, targetID, err)
			}
			target = rec
			targetLoaded = true
			return target, true, nil
		}

		if len(f.TargetFilter) > 0 {
			// A dangling target id (points at nothing) exempts EVERY
			// TargetFilter condition on this field uniformly, whether
			// direct-field or entity-join — not just the direct-field
			// shape. Checked once, up front, rather than per-condition:
			// an entity-join condition's own ExistsByFieldsQ has no way
			// to tell "target doesn't exist" apart from "target exists
			// but the join has no matching row", so without this
			// up-front check a dangling reference would be wrongly
			// rejected as a constraint violation instead of being
			// tolerated the same way every other dangling FieldReference
			// in this kernel already is (ADR-0007).
			_, found, err := loadTarget()
			if err != nil {
				return err
			}
			if found {
				for _, cond := range f.TargetFilter {
					if cond.Entity == "" {
						rec, _, _ := loadTarget() // already loaded and found above
						if !valueMatches(rec.Data[cond.Field], cond.Value) {
							return fmt.Errorf("%s.%s: %w: target %s does not have %s=%q",
								def.EntityType, f.Name, ErrTargetConstraintViolation, targetID, cond.Field, cond.Value)
						}
						continue
					}
					exists, err := records.ExistsByFieldsQ(ctx, tx, cond.Entity, map[string]string{
						cond.EntityField: targetID,
						cond.Field:       cond.Value,
					})
					if err != nil {
						return fmt.Errorf("%s.%s: check target_filter via %s: %w", def.EntityType, f.Name, cond.Entity, err)
					}
					if !exists {
						return fmt.Errorf("%s.%s: %w: target %s has no %s record with %s=%q",
							def.EntityType, f.Name, ErrTargetConstraintViolation, targetID, cond.Entity, cond.Field, cond.Value)
					}
				}
			}
		}

		if f.MustMatchParentField != "" {
			ownValue, ok := fields[f.MustMatchParentField].(string)
			if !ok || ownValue == "" {
				// Nothing submitted for the sibling field to compare
				// against — the same "documented, test-pinned trade" gap
				// entity.validateNotBefore already takes for a missing
				// value, not a new one this mechanism invents. A
				// Required sibling field (Task.project_id) already
				// closes this in practice via entity.ValidateRecord,
				// which runs before this function on every Create/Update.
				continue
			}
			rec, found, err := loadTarget()
			if err != nil {
				return err
			}
			if !found {
				continue // dangling reference — not this guard's concern
			}
			if !valueMatches(rec.Data[f.MustMatchParentField], ownValue) {
				return fmt.Errorf("%s.%s: %w: target %s has %s=%v, expected %q",
					def.EntityType, f.Name, ErrTargetConstraintViolation, targetID, f.MustMatchParentField, rec.Data[f.MustMatchParentField], ownValue)
			}
		}
	}
	return nil
}

// ResolveReferenceFilter computes the extra data.ListPageOptions fields
// (EqualsFilters/IDIn) needed to narrow FieldReference field f's
// candidate list to only valid targets, given siblingValue — the
// submitting record's CURRENT value of f.MustMatchParentField (e.g. a
// Task form's in-progress project_id), empty when unknown or
// inapplicable. This is the single generic implementation the
// reference-picker search endpoint (internal/api/reference_search.go)
// calls to honour Field.TargetFilter/MustMatchParentField (uc-infra#78)
// — sharing it with checkReferenceTargetConstraints' declaration
// reading (both walk the same Field data) means the picker's narrowing
// and Create/Update's enforcement can never quietly diverge.
//
// An empty siblingValue contributes no MustMatchParentField narrowing —
// nothing to compare yet, the same "skip, don't reject" posture
// checkReferenceTargetConstraints itself takes for a record whose
// sibling field isn't set.
func (e *Engine) ResolveReferenceFilter(ctx context.Context, f entity.Field, siblingValue string) (data.ListPageOptions, error) {
	var opts data.ListPageOptions
	var idSets [][]string
	for _, cond := range f.TargetFilter {
		if cond.Entity == "" {
			opts.EqualsFilters = append(opts.EqualsFilters, data.FieldEquals{Field: cond.Field, Value: cond.Value})
			continue
		}
		ids, err := e.records.DistinctFieldValues(ctx, cond.Entity, cond.Field, cond.Value, cond.EntityField)
		if err != nil {
			return data.ListPageOptions{}, fmt.Errorf("resolve target_filter for %s: %w", f.Name, err)
		}
		idSets = append(idSets, ids)
	}
	if len(idSets) > 0 {
		opts.IDIn = intersectIDSets(idSets)
	}
	if f.MustMatchParentField != "" && siblingValue != "" {
		opts.EqualsFilters = append(opts.EqualsFilters, data.FieldEquals{Field: f.MustMatchParentField, Value: siblingValue})
	}
	return opts, nil
}

// intersectIDSets returns the intersection of every slice in sets — the
// candidate ids that satisfy EVERY join-based TargetFilter condition on
// one field (AND semantics, same as every other condition a field
// declares). A set that resolves to no matches collapses the whole
// intersection to an empty (non-nil) slice, matching
// data.ListPageOptions.IDIn's own "non-nil empty means no matches"
// convention.
func intersectIDSets(sets [][]string) []string {
	if len(sets) == 0 {
		return nil
	}
	counts := make(map[string]int, len(sets[0]))
	for _, set := range sets {
		seen := make(map[string]bool, len(set))
		for _, id := range set {
			if seen[id] {
				continue
			}
			seen[id] = true
			counts[id]++
		}
	}
	out := make([]string, 0, len(counts))
	for id, c := range counts {
		if c == len(sets) {
			out = append(out, id)
		}
	}
	return out
}
