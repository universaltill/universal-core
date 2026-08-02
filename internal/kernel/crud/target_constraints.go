package crud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ErrTargetConstraintViolation is returned by Create/Update when a
// FieldReference field's value points at a record that fails a
// declared Field.TargetFilter condition or Field.MustMatchParentField
// check (uc-infra#78) — e.g. TimeEntry.employee_id pointing at a Party
// that does not hold the employee PartyRole, or Task.parent_task_id
// pointing at a Task in a different project. The caller's own bad input,
// not a server fault, so internal/api maps it to 400 — but UNLIKE
// ErrReferenceCycle/ErrHookRejected (whose plain Error() text ships
// straight to the caller, untranslated), this one is always wrapped in a
// *TargetConstraintError (see below) and internal/api's
// writeCrudErrorLocalized renders a translated per-field message from
// its structured fields instead of Error() verbatim — independent
// review: the untranslated precedent was the wrong one to follow for a
// routine data-entry mistake a real end user hits constantly, as
// opposed to the rare admin-only actions that precedent was set for.
// Error()/errors.Is(err, ErrTargetConstraintViolation) still work
// (Unwrap), for any caller (tests, logs) that only needs the sentinel.
var ErrTargetConstraintViolation = errors.New("reference target does not satisfy the field's required constraint")

// TargetConstraintError carries the entity type, field name, and full
// descriptive detail one ErrTargetConstraintViolation failure produced —
// unlike ErrReferenceCycle/ErrHookRejected (whose plain Error() text IS
// the user-facing message, untranslated), this failure is a routine
// data-entry mistake, not a rare admin action (independent review,
// uc-infra#78 follow-up), so CLAUDE.md's "no hardcoded user-facing
// strings" applies: internal/api's writeCrudErrorLocalized uses these
// fields to render a translated, per-field message instead of Detail
// verbatim, and logs Detail (which stays untranslated, English,
// entity/field/value-naming — safe and useful for an operator regardless
// of the caller's locale).
//
// JoinEntity additionally lets authz.GuardedEngine generalize the message
// when the caller cannot even read the joined entity type an entity-join
// TargetFilter condition (Field.TargetFilter with Entity set) consulted —
// see GuardedEngine's own sanitizeTargetConstraintError doc comment for
// why. It is empty for the direct-field TargetFilter shape and for
// MustMatchParentField: neither reads an entity type the caller doesn't
// already hold via its own submitted record's fields.
type TargetConstraintError struct {
	EntityType string
	FieldName  string
	JoinEntity string
	Detail     string
}

func (e *TargetConstraintError) Error() string {
	return fmt.Sprintf("%s.%s: %s: %s", e.EntityType, e.FieldName, ErrTargetConstraintViolation, e.Detail)
}

// Unwrap makes errors.Is(err, ErrTargetConstraintViolation) keep matching
// every existing caller that checks the sentinel rather than this type.
func (e *TargetConstraintError) Unwrap() error { return ErrTargetConstraintViolation }

// looksLikeUUID matches the shape every records.id actually is (Postgres
// gen_random_uuid()) — the same shape internal/api's own idPattern
// validates at the HTTP boundary. A FieldReference value that isn't
// UUID-shaped can never resolve to a real target row (records.id is a
// uuid column), so loadTarget below treats it the same as "target not
// found": a tolerated dangling reference (ADR-0007), not a constraint
// violation and not a raw Postgres "invalid input syntax for type uuid"
// error propagating up as an internal 500 — independent review: before
// TargetFilter/MustMatchParentField existed, no write path resolved a
// FieldReference's target at all, so a non-UUID value was accepted the
// same as any other dangling reference; this restores that tolerance for
// the new target-loading path this file adds.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func looksLikeUUID(s string) bool { return uuidPattern.MatchString(s) }

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

// queryable is the read-only method set checkReferenceTargetConstraints
// actually needs (records.GetTx/ExistsByFieldsQ's own "querier"
// parameter, restated here since that one is unexported in package
// data). Both *sql.Tx (every write-path caller, Create/Update) and
// *sql.DB (CountTargetConstraintViolations' read-only reporting path,
// which has no write to be atomic with and so no reason to force a
// throwaway transaction into existence) satisfy it — this lets the
// exact same constraint-evaluation logic serve both without
// duplicating it.
type queryable interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
func checkReferenceTargetConstraints(ctx context.Context, tx queryable, records *data.RecordRepo, def *entity.Definition, fields map[string]any) error {
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
			if !looksLikeUUID(targetID) {
				// Not shaped like a records.id at all — records.GetTx's
				// `WHERE id = $1` runs against a uuid column, so this would
				// otherwise surface as a raw Postgres "invalid input syntax
				// for type uuid" driver error, not data.ErrNotFound (see
				// looksLikeUUID's own doc comment). Treated identically to
				// "target not found": a tolerated dangling reference.
				return data.Record{}, false, nil
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
							return &TargetConstraintError{
								EntityType: def.EntityType,
								FieldName:  f.Name,
								Detail: fmt.Sprintf("target %s does not have %s=%q",
									targetID, cond.Field, cond.Value),
							}
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
						return &TargetConstraintError{
							EntityType: def.EntityType,
							FieldName:  f.Name,
							JoinEntity: cond.Entity,
							Detail: fmt.Sprintf("target %s has no %s record with %s=%q",
								targetID, cond.Entity, cond.Field, cond.Value),
						}
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
			targetValue, present := rec.Data[f.MustMatchParentField]
			if !present || targetValue == nil {
				// The target's OWN stored data has nothing for this field
				// (never set, or the field genuinely isn't part of
				// Target's Definition at all — this package can't tell
				// those apart without a cross-Definition registry, and
				// doesn't need to: same "not this guard's concern"
				// posture already taken above for a submission that
				// omits the field's own current value. Deliberately made
				// consistent with that posture rather than flat-out
				// rejecting — independent review: the two cases used to disagree
				// (a non-string own value silently skipped the check, but
				// a nil target value always failed valueMatches and
				// rejected), which is documented here as the fix: both
				// "nothing to compare" cases now skip, neither rejects.
				continue
			}
			if !valueMatches(targetValue, ownValue) {
				return &TargetConstraintError{
					EntityType: def.EntityType,
					FieldName:  f.Name,
					Detail: fmt.Sprintf("target %s has %s=%v, expected %q",
						targetID, f.MustMatchParentField, targetValue, ownValue),
				}
			}
		}
	}
	return nil
}

// CountTargetConstraintViolations counts EXISTING live records of def
// whose FieldReference field fieldName already fails that field's
// declared TargetFilter/MustMatchParentField constraint — the
// FieldReference equivalent of RecordRepo.CountMissingField, which
// cmd/sync-tenant-modules already calls to warn about a newly-Required
// field. This closes the same class of gap for a newly-added (or
// tightened) TargetFilter/MustMatchParentField (uc-infra#78 follow-up,
// same family of bug as uc-infra#73): without it, an operator running
// sync-tenant-modules against a tenant with pre-existing violating
// records sees "0 data migrations needed", and every subsequent
// edit-and-save of one of those records then 400s with no warning this
// was coming.
//
// Read-only and takes a plain *sql.DB — unlike checkReferenceTargetConstraints's
// transaction-scoped write-path callers, this never writes and has
// nothing to be atomic with, so forcing a throwaway *sql.Tx into
// existence would add nothing (see queryable's own doc comment). Walks
// every live record of def.EntityType a page at a time rather than
// loading them all into memory at once — this can run against a
// tenant's full table, potentially large, and this is a reporting path,
// not the hot request path DistinctFieldValues' own fix (finding #2)
// was about, but the same "don't materialize an unbounded result set"
// discipline still applies.
//
// A record is checked against a Definition narrowed to just fieldName —
// checkReferenceTargetConstraints only needs the ONE Field declaration
// to evaluate that field's own constraint (MustMatchParentField's
// sibling value is read from the record's own data map, not from the
// Definition's field list), so this deliberately does not run every
// OTHER constrained field's check too, which would double-count a
// record violating more than one field when the caller is asking about
// one specific field at a time (mirroring how the sync command calls
// CountMissingField once per newly-Required field, not once per
// record).
func CountTargetConstraintViolations(ctx context.Context, db *sql.DB, records *data.RecordRepo, def *entity.Definition, fieldName string) (int, error) {
	f, ok := def.FieldByName(fieldName)
	if !ok || f.Type != entity.FieldReference || (len(f.TargetFilter) == 0 && f.MustMatchParentField == "") {
		return 0, nil
	}
	narrow := &entity.Definition{EntityType: def.EntityType, Fields: []entity.Field{f}}

	const pageSize = 200
	violations := 0
	offset := 0
	for {
		page, err := records.ListPage(ctx, def.EntityType, pageSize, offset)
		if err != nil {
			return 0, fmt.Errorf("count target constraint violations for %s.%s: %w", def.EntityType, fieldName, err)
		}
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			err := checkReferenceTargetConstraints(ctx, db, records, narrow, rec.Data)
			if err == nil {
				continue
			}
			if errors.Is(err, ErrTargetConstraintViolation) {
				violations++
				continue
			}
			return 0, fmt.Errorf("count target constraint violations for %s.%s: %w", def.EntityType, fieldName, err)
		}
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return violations, nil
}

// ResolveReferenceFilter computes the extra data.ListPageOptions fields
// (EqualsFilters/JoinFilters) needed to narrow FieldReference field f's
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
// An entity-join TargetFilter condition becomes a data.JoinFilter, a
// correlated-EXISTS narrowing evaluated entirely in SQL — NOT resolved
// here into an id list (independent review, uc-infra#78 follow-up: the
// original shape called RecordRepo.DistinctFieldValues and intersected
// the results in Go, which read the ENTIRE matching set of the joined
// entity type into memory on every debounced picker keystroke; see
// data.ListPageOptions.JoinFilters' own doc comment). Multiple
// entity-join conditions on the same field simply become multiple
// JoinFilters entries, ANDed by filterWhereClause exactly like multiple
// EqualsFilters entries already are — no in-Go intersection needed.
//
// An empty siblingValue contributes no MustMatchParentField narrowing —
// nothing to compare yet, the same "skip, don't reject" posture
// checkReferenceTargetConstraints itself takes for a record whose
// sibling field isn't set.
func (e *Engine) ResolveReferenceFilter(ctx context.Context, f entity.Field, siblingValue string) (data.ListPageOptions, error) {
	var opts data.ListPageOptions
	for _, cond := range f.TargetFilter {
		if cond.Entity == "" {
			opts.EqualsFilters = append(opts.EqualsFilters, data.FieldEquals{Field: cond.Field, Value: cond.Value})
			continue
		}
		opts.JoinFilters = append(opts.JoinFilters, data.JoinFilter{
			Entity:      cond.Entity,
			EntityField: cond.EntityField,
			Field:       cond.Field,
			Value:       cond.Value,
		})
	}
	if f.MustMatchParentField != "" && siblingValue != "" {
		opts.EqualsFilters = append(opts.EqualsFilters, data.FieldEquals{Field: f.MustMatchParentField, Value: siblingValue})
	}
	return opts, nil
}
