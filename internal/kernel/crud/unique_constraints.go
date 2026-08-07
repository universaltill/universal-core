package crud

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ErrUniqueConstraintViolation is returned by Create/Update when a
// record's values collide with an existing record on one of Definition
// .Unique's declared field sets (uc-infra#81) — e.g. a second
// AttendanceRecord for the same (employee_id, entry_date). The caller's
// own bad input, not a server fault, so internal/api maps it to 400 —
// and, following #78's TargetConstraintError precedent rather than the
// older plain-sentinel ErrReferenceCycle/ErrHookRejected one, this is
// always wrapped in a *UniqueConstraintError so writeCrudErrorLocalized
// can render a translated, per-field message instead of Error()
// verbatim: a duplicate key is a routine data-entry mistake a real end
// user hits, not a rare admin-only action.
// Error()/errors.Is(err, ErrUniqueConstraintViolation) still work
// (Unwrap), for any caller that only needs the sentinel.
var ErrUniqueConstraintViolation = errors.New("value already used by another record")

// UniqueConstraintError carries which entity/constraint/fields one
// ErrUniqueConstraintViolation failure named. Fields is the canonicalized
// (sorted) field-name set, matching entity.UniqueConstraintName's own
// ordering, so a caller resolving field labels can just range over it.
type UniqueConstraintError struct {
	EntityType     string
	ConstraintName string
	Fields         []string
}

func (e *UniqueConstraintError) Error() string {
	return fmt.Sprintf("%s.%s: %s", e.EntityType, e.ConstraintName, ErrUniqueConstraintViolation)
}

// Unwrap makes errors.Is(err, ErrUniqueConstraintViolation) keep matching
// every caller that checks the sentinel rather than this type.
func (e *UniqueConstraintError) Unwrap() error { return ErrUniqueConstraintViolation }

// uniqueKeyValue computes the deterministic key record_unique_keys
// stores for one constraint's canonical (sorted) field set, from a
// record's field values. applicable is false when any named field's
// value is absent, nil, OR an empty string — mirroring both ordinary SQL
// semantics (a NULL column never participates in a unique index) AND
// #86/ADR-0018 §4's "Required treats "" as absent" convention, so a
// Unique field's own emptiness rule agrees with Required's rather than
// silently disagreeing with it (independent review, uc-infra#81
// follow-up: the first version of this function let two records with
// badge_number:"" collide, when the same two records with badge_number
// omitted entirely did not — same logical input, different outcome by
// which shape submitted it).
//
// The stored value is SHA-256 of the field values' JSON encoding, hex,
// not the JSON itself — independent review, uc-infra#81 follow-up: a raw
// JSON key_value has no length bound, and Postgres' btree index has one
// (~2700 bytes); a long value (a FieldString with no declared Max, a
// FieldI18nText with many locales) would make record_unique_keys'
// UNIQUE index reject the INSERT with "index row size exceeds btree
// maximum" — an unrelated failure a naive substring-matching error
// classifier could misreport as a duplicate-key violation (the actual
// bug found). Hashing gives a fixed 64-character key regardless of input
// size or field type, so this can never happen, and
// data.classifyUniqueKeyErr's SQLSTATE-based classification means even a
// hash COLLISION (astronomically unlikely, not "an oversized value")
// would surface as ErrUniqueKeyConflict correctly, not incorrectly.
// encoding/json already gives a stable, unambiguous per-value
// representation to hash (no manual delimiter/escaping to get wrong),
// and fields map[string]any's values already come from the same
// encoding/json decode path record data elsewhere in this package does
// (see checkReferenceTargetConstraints' own fields map[string]any), so a
// number is consistently a float64 here, not sometimes an int64.
func uniqueKeyValue(sortedFields []string, fields map[string]any) (value string, applicable bool, err error) {
	ordered := make([]any, len(sortedFields))
	for i, name := range sortedFields {
		v, ok := fields[name]
		if !ok || v == nil {
			return "", false, nil
		}
		if s, isString := v.(string); isString && s == "" {
			return "", false, nil
		}
		ordered[i] = v
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		return "", false, fmt.Errorf("marshal unique key value: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), true, nil
}

// WriteUniqueConstraintKeys inserts one record_unique_keys row per
// declared Unique set that has every one of its fields present (and
// non-empty) on the new record, inside tx — called from Engine.Create,
// after data.RecordRepo.CreateTx so recordID is known, before commit.
// The Postgres UNIQUE index record_unique_keys' own migration declares
// is the actual correctness guarantee here, not this function's own
// logic: ADR-0018 §3 explains why a Go-side SELECT-before-INSERT check
// alone cannot close the race between two concurrent transactions (both
// would see no conflict and both insert) — data.RecordUniqueKeyRepo
// .InsertTx IS the enforcement, and a concurrent loser gets
// data.ErrUniqueKeyConflict, translated below into a
// *UniqueConstraintError instead of leaking a raw driver error.
//
// Exported (uc-infra#126) so a crud.Hook implementation — which by
// contract (Hook's own doc comment) gets only a *sql.Tx, not an *Engine,
// and so cannot route a write through Engine.Create/Update — can still
// participate correctly in the SAME generic enforcement every ordinary
// write goes through, instead of writing InventoryItem-shaped (or any
// other entity-shaped) duplicate logic to do it by hand. Still entirely
// generic: takes a Definition and a fields map, never an entity type
// name to special-case, same kernel-boundary discipline as this
// function's un-exported form always had.
//
// Known, deliberately deferred gap (uc-infra#107, tracked separately —
// not specific to Unique): a FieldReference value that reaches here in a
// non-canonical spelling (case, whitespace) of the same underlying id
// hashes differently from the canonical spelling, so two records
// referencing "the same" target via different literal spellings do not
// collide. #107 already tracks id-spelling canonicalization as a
// cross-cutting kernel gap; fixing it only here would leave every other
// consumer of a FieldReference value inconsistent with this one.
func WriteUniqueConstraintKeys(ctx context.Context, tx queryable, keys *data.RecordUniqueKeyRepo, def *entity.Definition, recordID string, fields map[string]any) error {
	for _, set := range def.Unique {
		sorted := canonicalSort(set)
		name := entity.UniqueConstraintName(sorted)
		value, applicable, err := uniqueKeyValue(sorted, fields)
		if err != nil {
			return err
		}
		if !applicable {
			continue
		}
		if err := keys.InsertTx(ctx, tx, def.EntityType, name, value, recordID); err != nil {
			if errors.Is(err, data.ErrUniqueKeyConflict) {
				return &UniqueConstraintError{EntityType: def.EntityType, ConstraintName: name, Fields: sorted}
			}
			return err
		}
	}
	return nil
}

// UpdateUniqueConstraintKeys re-derives each declared constraint's key
// from the record's post-update field values and reconciles
// record_unique_keys — called from Engine.Update, after
// data.RecordRepo.UpdateTx, before commit:
//
//   - no longer applicable (a key field became absent/empty) → delete
//     the row, freeing the old combination for reuse by another record.
//   - applicable, no existing row yet (the record predates this Unique
//     declaration — a Definition version bump, ADR-0018's Consequences —
//     or was previously inapplicable) → insert.
//   - applicable, existing row → update key_value in place.
//
// Every branch that touches the unique index can return
// *UniqueConstraintError, same translation as WriteUniqueConstraintKeys.
//
// Exported alongside WriteUniqueConstraintKeys (uc-infra#126) — same
// reasoning, a crud.Hook updating a record it looked up itself (not the
// one Engine.Update is already writing) needs this same reconciliation,
// not a hand-rolled duplicate of it.
func UpdateUniqueConstraintKeys(ctx context.Context, tx queryable, keys *data.RecordUniqueKeyRepo, def *entity.Definition, recordID string, fields map[string]any) error {
	for _, set := range def.Unique {
		sorted := canonicalSort(set)
		name := entity.UniqueConstraintName(sorted)
		value, applicable, err := uniqueKeyValue(sorted, fields)
		if err != nil {
			return err
		}
		if !applicable {
			if err := keys.DeleteForConstraintTx(ctx, tx, def.EntityType, name, recordID); err != nil {
				return err
			}
			continue
		}
		n, err := keys.UpdateValueTx(ctx, tx, def.EntityType, name, value, recordID)
		if err != nil {
			if errors.Is(err, data.ErrUniqueKeyConflict) {
				return &UniqueConstraintError{EntityType: def.EntityType, ConstraintName: name, Fields: sorted}
			}
			return err
		}
		if n == 0 {
			// No existing row — this record was created before the
			// constraint applied to it (version bump) or its key fields
			// were previously absent/empty. Insert now so it's covered
			// going forward, same conflict handling as the Create path.
			if err := keys.InsertTx(ctx, tx, def.EntityType, name, value, recordID); err != nil {
				if errors.Is(err, data.ErrUniqueKeyConflict) {
					return &UniqueConstraintError{EntityType: def.EntityType, ConstraintName: name, Fields: sorted}
				}
				return err
			}
		}
	}
	return nil
}

// deleteUniqueConstraintKeys removes every record_unique_keys row for a
// soft-deleted record — called from Engine.Delete, after
// data.RecordRepo.DeleteTx, before commit — so its key combination(s)
// become reusable by a new record, matching how a soft-deleted record no
// longer counts toward Required/other live-record checks elsewhere in
// this kernel.
func deleteUniqueConstraintKeys(ctx context.Context, tx queryable, keys *data.RecordUniqueKeyRepo, recordID string) error {
	return keys.DeleteForRecordTx(ctx, tx, recordID)
}

// canonicalSort returns a sorted copy of fields, the same canonical order
// entity.UniqueConstraintName sorts on — kept as a small unexported helper
// here (rather than re-exporting entity's own sort) since callers in this
// file need the sorted slice itself, not just its joined name.
func canonicalSort(fields []string) []string {
	sorted := append([]string(nil), fields...)
	slices.Sort(sorted)
	return sorted
}

// CountUniqueConstraintViolations counts, among EXISTING live records of
// entityType, how many collide with at least one other live record on
// the given (already-declared) field set — the Unique equivalent of
// CountTargetConstraintViolations, feeding cmd/sync-tenant-modules' same
// data-migration warning path when a Definition version bump adds or
// widens a Unique set on an entity type that may already hold
// conflicting data (ADR-0018's Consequences section). Unlike
// WriteUniqueConstraintKeys/UpdateUniqueConstraintKeys, this never
// writes record_unique_keys — it walks live records a page at a time and
// counts key values seen more than once, entirely in Go, so it can be
// run as a dry-run report before the constraint is ever enforced for
// real.
func CountUniqueConstraintViolations(ctx context.Context, records *data.RecordRepo, entityType string, fields []string) (int, error) {
	sorted := canonicalSort(fields)
	const pageSize = 200
	seen := make(map[string]int)
	offset := 0
	for {
		page, err := records.ListPage(ctx, entityType, pageSize, offset)
		if err != nil {
			return 0, fmt.Errorf("count unique constraint violations for %s: %w", entityType, err)
		}
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			value, applicable, err := uniqueKeyValue(sorted, rec.Data)
			if err != nil {
				return 0, fmt.Errorf("count unique constraint violations for %s: %w", entityType, err)
			}
			if !applicable {
				continue
			}
			seen[value]++
		}
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	violations := 0
	for _, count := range seen {
		if count > 1 {
			violations += count
		}
	}
	return violations, nil
}

// BackfillUniqueConstraintKeys populates record_unique_keys for EXISTING
// live records of entityType against a newly-declared (or newly-
// applicable) Unique field set — without this, a Unique constraint added
// to an entity type that already has data protects nothing: every
// pre-existing record has no key row, so a brand-new record can silently
// duplicate an old one's combination even though the old data itself was
// perfectly valid (independent review, uc-infra#81 follow-up — a real
// gap distinct from CountUniqueConstraintViolations' report, which only
// covers records that ALREADY collide with each other, not the "old
// record has no key at all" exposure this closes).
//
// Deliberately NOT a "repair" in the sense cmd/sync-tenant-modules'
// requiredFieldWarnings/targetConstraintWarnings avoid (both explicitly
// report only, never touch records.data) — record_unique_keys is the
// constraint mechanism's OWN bookkeeping table, not user data, so
// populating it is establishing infrastructure the feature needs to
// function, not silently correcting something a human should see. It
// never modifies records.data or record versions.
//
// Walks live records oldest-created-first a page at a time. For each
// record whose key set is applicable, attempts InsertTx: success means
// backfilled; a genuine ErrUniqueKeyConflict means an EARLIER (older)
// record already holds this key, so this record is left unbacked and
// counted as skipped — deterministic (oldest wins), and exactly the
// records CountUniqueConstraintViolations' warning already flags as
// needing attention, so the two never disagree about which records are
// the problem. Any other error aborts and is returned as-is.
func BackfillUniqueConstraintKeys(ctx context.Context, db *sql.DB, records *data.RecordRepo, keys *data.RecordUniqueKeyRepo, entityType string, fields []string) (backfilled, skipped int, err error) {
	sorted := canonicalSort(fields)
	name := entity.UniqueConstraintName(sorted)
	const pageSize = 200
	offset := 0
	for {
		page, err := records.ListPage(ctx, entityType, pageSize, offset)
		if err != nil {
			return backfilled, skipped, fmt.Errorf("backfill unique constraint keys for %s: %w", entityType, err)
		}
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			value, applicable, err := uniqueKeyValue(sorted, rec.Data)
			if err != nil {
				return backfilled, skipped, fmt.Errorf("backfill unique constraint keys for %s: %w", entityType, err)
			}
			if !applicable {
				continue
			}
			if err := keys.InsertTx(ctx, db, entityType, name, value, rec.ID); err != nil {
				if errors.Is(err, data.ErrUniqueKeyConflict) {
					skipped++
					continue
				}
				return backfilled, skipped, fmt.Errorf("backfill unique constraint keys for %s: %w", entityType, err)
			}
			backfilled++
		}
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return backfilled, skipped, nil
}
