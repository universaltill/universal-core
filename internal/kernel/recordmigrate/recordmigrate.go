// Package recordmigrate drives one-off migrations of record *data* —
// the JSONB contents of `records` rows — as opposed to
// internal/db/migrations, which only ever touches schema and has no way
// to reach inside a record.
//
// It exists because this kernel's Definitions are versioned and
// append-only, so a Version bump that adds or replaces a Required field
// leaves every already-written row of that type invalid until something
// stamps the new field in. Two entities have needed that now:
// PurchaseOrder v3->v4 (a plain "status" enum replaced by a "status_id"
// reference) and InventoryItem v2->v3 (a required facility_id added,
// ADR-0015).
//
// **This package is the second-example generalization
// cmd/backfill-purchase-order-status asked for**, in its own words:
// "If a second entity ever needs the same kind of fix, generalize then,
// against two real examples instead of one imagined one." Both commands
// are written against it, so what is shared is shared once.
//
// What is worth sharing here is not the loop, which is trivial, but the
// handful of decisions inside it that are easy to get subtly wrong and
// were each arrived at deliberately:
//
//   - **Already-migrated rows are skipped, not rewritten**, so a re-run
//     after a partial failure is safe and cheap.
//   - **Full field replacement.** Update replaces the whole JSONB blob
//     (data.RecordRepo.UpdateTx does `SET data = $1`), so a transform
//     returns the complete new field map. Dropping a key is done by
//     omitting it. A transform that returned only its own changes would
//     silently erase every other field.
//   - **Validation runs BEFORE the dry-run branch**, so a dry run's
//     report matches what a real run would actually do. Validating
//     after would let a dry run promise migrations that then fail.
//   - **A bad record is skipped with a warning; a bad database aborts.**
//     A legacy row missing some unrelated Required field is a problem
//     with that row, not a reason to abandon a batch that is otherwise
//     fine. A version conflict or a dead connection is the opposite.
//   - **Updates are version-checked**, so a concurrent edit loses the
//     race loudly instead of being silently clobbered.
//
// Generic in the sense CLAUDE.md's kernel boundary requires: nothing
// here names an entity type or a field. The caller supplies the
// Definition and a Transform.
package recordmigrate

import (
	"context"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// Action is what a Transform decided to do with one record.
type Action int

const (
	// Migrate writes the returned fields. The zero value is deliberately
	// NOT this: a Transform that forgets to set an action should do
	// nothing rather than write whatever it happened to return.
	Migrate Action = iota + 1
	// AlreadyDone means the record already carries the new shape.
	// Counted separately from Skip because it is the expected outcome of
	// a re-run, not a problem anyone needs to look at.
	AlreadyDone
	// Skip means this record cannot be migrated automatically and needs
	// a human. Always accompanied by a reason, which is logged.
	Skip
)

// Transform decides what happens to one record. It returns the COMPLETE
// new field map when the action is Migrate — see the package comment on
// full field replacement.
//
// The note string does double duty, because both uses are "one line of
// per-record context only the caller can supply": on Skip it is the
// REASON, and it is required, since "3 skipped" with no way to learn
// which three is useless. On Migrate it is an optional DRY-RUN PREVIEW
// appended to the "would migrate" line — the mapping an operator is
// actually dry-running to inspect. The first version of this package
// had no such field, which silently downgraded the PurchaseOrder
// backfill's dry run from `status "draft" -> status_id <uuid>` to a
// bare record id (independent review): the tally already told you how
// many, so the id alone added nothing. Ignored for AlreadyDone.
type Transform func(rec data.Record) (fields map[string]any, action Action, note string)

// Result tallies a run.
type Result struct {
	Migrated    int
	AlreadyDone int
	Skipped     int
}

// Logf is the reporting hook — log.Printf satisfies it. Warnings about
// skipped records and dry-run previews go here, so a caller decides
// where they land and tests can capture them.
type Logf func(format string, args ...any)

// Options configures one Run.
type Options struct {
	// DryRun reports what would change and writes nothing.
	DryRun bool
	// Logf receives per-record warnings and dry-run previews. Required.
	Logf Logf
}

// Run applies transform to every record of def's type.
//
// Returns an error only for infrastructure failures — a listing error,
// or an Update that failed for a reason other than the record's own
// content. Records that cannot be migrated are counted in
// Result.Skipped and reported through Options.Logf; they do not abort
// the run, and they do not make Run return an error, because a batch
// migration that stops dead on one bad legacy row is worse than one
// that finishes and tells you which rows need a human.
func Run(
	ctx context.Context,
	engine *crud.Engine,
	def *entity.Definition,
	transform Transform,
	actor audit.Actor,
	opts Options,
) (Result, error) {
	var res Result
	if opts.Logf == nil {
		return res, fmt.Errorf("recordmigrate: Options.Logf is required")
	}

	records, err := engine.List(ctx, def)
	if err != nil {
		return res, fmt.Errorf("list %s records: %w", def.EntityType, err)
	}

	for _, rec := range records {
		fields, action, note := transform(rec)
		switch action {
		case AlreadyDone:
			res.AlreadyDone++
			continue
		case Skip:
			opts.Logf("WARNING: %s %s skipped, needs manual review: %s", def.EntityType, rec.ID, note)
			res.Skipped++
			continue
		case Migrate:
		default:
			opts.Logf("WARNING: %s %s skipped: transform returned no action", def.EntityType, rec.ID)
			res.Skipped++
			continue
		}

		// Before the dry-run branch on purpose — see the package
		// comment. A row that fails validation for some reason this
		// migration does not supply a value for is that row's problem.
		if err := entity.ValidateRecord(def, fields); err != nil {
			opts.Logf("WARNING: %s %s would fail validation after migration (%v) — skipped, needs manual review", def.EntityType, rec.ID, err)
			res.Skipped++
			continue
		}

		if opts.DryRun {
			if note != "" {
				opts.Logf("DRY RUN: would migrate %s %s: %s", def.EntityType, rec.ID, note)
			} else {
				opts.Logf("DRY RUN: would migrate %s %s", def.EntityType, rec.ID)
			}
			res.Migrated++
			continue
		}

		expectedVersion := rec.Version
		if _, err := engine.Update(ctx, def, rec.ID, fields, &expectedVersion, actor); err != nil {
			return res, fmt.Errorf("update %s %s: %w", def.EntityType, rec.ID, err)
		}
		res.Migrated++
	}
	return res, nil
}

// CopyExcept returns a copy of a record's data with the named keys
// omitted — the building block for the full-field-replacement rule.
// Callers add their new keys to the result.
//
// A migration that only ADDS a field passes no keys to omit; one that
// REPLACES a field names the old one, which is how the old key gets
// dropped from the stored blob (Update rewrites it wholesale).
func CopyExcept(data map[string]any, omit ...string) map[string]any {
	drop := make(map[string]bool, len(omit))
	for _, k := range omit {
		drop[k] = true
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		if drop[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// Deliberately no shared Summary()/reporting helper. Each command
// prints its own line, because the useful half of "0 already had
// status_id" is the phrase naming the specific field, which only the
// caller knows — and a generic "0 already migrated" would be a worse
// message shared for its own sake. What is worth sharing is the loop
// and its decisions, not the sentence.
