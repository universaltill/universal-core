// Upserting commit (uc-infra#101): the piece that makes a re-import
// UPDATE the records it created last time instead of duplicating them.
// csvimport.CommitRows is create-only by design — it has no notion of
// where a row came from, so running the same pull twice doubles every
// record. This file adds the missing memory: an ExternalIdentity row
// (foundation.ExternalIdentity) per imported record, keyed by
// (source, relation, target entity type, legacy key), consulted before
// every write. The generic engines stay generic — which column is the
// legacy key is data (TemplateEntity.KeyColumn, or a caller's explicit
// choice), never an entity-type branch in here.
//
// The relation is part of the identity scope for a reason the NAV
// template makes concrete: $Customer and $Vendor both map onto Party,
// and NAV number series overlap across those tables (Customer 10000 and
// Vendor 10000 are different companies). Scoping by source alone would
// make a Vendor import silently overwrite Customer records — the
// independent review proved it against CRONUS-shaped data. Two
// relations, two namespaces, even when they land in one entity type;
// same for the same table across NAV companies ("A Ltd_$Customer" vs
// "B Ltd_$Customer").
package sqlsource

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// RecordEngine is the engine capability upserting needs — a superset of
// csvimport.RecordCreator (Create alone can only ever duplicate).
// Satisfied by both the raw *crud.Engine and internal/kernel/authz's
// RBAC-guarded engine. The import HTTP route passes the GUARDED engine
// for the target records (a user-driven write path, permission-checked
// like any other — ADR-0006) and a RAW engine for the identity rows:
// ExternalIdentity is control-plane-gated (see authz.controlPlaneTypes)
// so users cannot author or re-point identities through generic
// surfaces, which means the importer itself must write them through the
// system side-channel — the same split workflow steps and provisioning
// already use.
type RecordEngine interface {
	Create(ctx context.Context, def *entity.Definition, fields map[string]any, actor audit.Actor) (data.Record, error)
	Update(ctx context.Context, def *entity.Definition, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error)
	Get(ctx context.Context, def *entity.Definition, id string) (data.Record, error)
	ListByField(ctx context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error)
}

// UpsertResult is csvimport's per-row outcome plus which write happened:
// Updated=false with a RecordID means the row created a new record (and
// its identity row); Updated=true means it refreshed the record a
// previous import created. The embedding keeps the caller's counting
// trivial — created = RecordID != "" && !Updated, updated = Updated.
type UpsertResult struct {
	csvimport.RowResult
	Updated bool
}

// KeyIndex returns keyColumn's position in headers, or an error when it
// isn't one — shared by commit here and by the preview path in
// internal/api, so the two can never disagree about what a valid key
// column is.
func KeyIndex(headers []string, keyColumn string) (int, error) {
	for i, h := range headers {
		if h == keyColumn {
			return i, nil
		}
	}
	return -1, fmt.Errorf("key column %q is not in the source headers", keyColumn)
}

// rowKey extracts row i's trimmed key cell ("" for short/ragged rows).
func rowKey(rows [][]string, i, keyIdx int) string {
	if i >= len(rows) || keyIdx >= len(rows[i]) {
		return ""
	}
	return strings.TrimSpace(rows[i][keyIdx])
}

// MarkMissingKeys annotates preview results with the same missing-key
// row errors commit would raise, so a blank key cell is visible at
// preview time rather than surprising the user at commit (review
// finding: the two stages previously disagreed). results must be
// PreviewRows' output for the same headers/rows; rows already carrying
// an error keep it.
func MarkMissingKeys(headers []string, rows [][]string, keyColumn string, results []csvimport.RowResult) ([]csvimport.RowResult, error) {
	keyIdx, err := KeyIndex(headers, keyColumn)
	if err != nil {
		return nil, err
	}
	out := append([]csvimport.RowResult(nil), results...)
	for i := range out {
		if out[i].Err != nil {
			continue
		}
		if rowKey(rows, i, keyIdx) == "" {
			out[i].Err = missingKeyError(keyColumn)
		}
	}
	return out, nil
}

func missingKeyError(keyColumn string) error {
	return fmt.Errorf("missing key: column %q is empty on this row", keyColumn)
}

// identityTarget is one pre-loaded identity: which record a key maps to,
// and whether the mapping is ambiguous (more than one identity row for
// the same key — the app-level-uniqueness limitation surfacing).
type identityTarget struct {
	recordID  string
	ambiguous bool
}

// CommitRowsUpserting is csvimport.CommitRows with identity memory:
// every valid row is matched by its legacy key against the
// ExternalIdentity rows belonging to (sourceRecordID, sourceRelation,
// def.EntityType) — a hit updates the previously imported record, a
// miss creates the record and writes its identity row so the NEXT run
// hits.
//
// keyColumn names the header carrying the legacy system's own key
// (NAV's "No_"); a row whose key cell is empty gets a row-level error
// and is skipped, and a key already used by an EARLIER row of the same
// run is also a row error — a non-unique key column (a legacy view, a
// join) would otherwise collapse rows silently, last one winning.
// sourceRelation is the schema-qualified relation the rows came from
// ("dbo.CRONUS International Ltd_$Customer") — part of the identity
// scope, see the package comment.
//
// Updates MERGE onto the stored record: the existing data is read and
// only mapped fields (plus template constants) are overlaid, so fields
// a human set after the first import — or fields outside the mapping —
// survive a refresh (review finding: crud.Engine.Update is a full
// replacement, which silently erased them). The trade this buys is that
// a source value CLEARED in the legacy system does not clear the Core
// value — an empty cell means absent by csvimport's convention, so the
// stale value persists until edited by hand. Propagating deletions is a
// sync-engine (#33) concern, not a one-shot import's.
//
// The identity pre-load is one ListByField(source_id) per run rather
// than one lookup per row — 10k-row pulls stay O(n), and in-run
// created keys join the in-memory map, which is also what detects
// duplicates.
//
// records is the (guarded) engine for the target records; identities is
// the raw engine for ExternalIdentity rows (see RecordEngine's doc
// comment on why they differ). identityDef is
// foundation.ExternalIdentity(), passed in like every other Definition.
//
// Sequencing: record first, then its identity row, each atomic on its
// own but not jointly — a crash between the two leaves one record whose
// identity was never written, which a re-run would duplicate once
// (accepted until #81-class constraints allow a real transactional
// upsert). A row whose record landed but whose identity write failed is
// reported as a row error even though RecordID is set — the error text
// says exactly that.
func CommitRowsUpserting(ctx context.Context, headers []string, rows [][]string, def *entity.Definition, mapping csvimport.ColumnMapping, keyColumn, sourceRecordID, sourceRelation string, records RecordEngine, identities RecordEngine, identityDef *entity.Definition, actor audit.Actor) ([]UpsertResult, error) {
	keyIdx, err := KeyIndex(headers, keyColumn)
	if err != nil {
		return nil, err
	}

	// Same validate-everything-first contract as csvimport.CommitRows.
	previews, err := csvimport.PreviewRows(headers, rows, def, mapping)
	if err != nil {
		return nil, err
	}

	// One identity pre-load for the whole run. Keyed on the RELATION,
	// not the source: a source accumulates identities for every relation
	// ever imported from it (Items + Customers + Vendors, unboundedly),
	// while a relation's own set is exactly the working set this run
	// needs (re-verification finding — source-keyed pre-load loaded the
	// whole history). source and entity type filter in Go.
	existing, err := identities.ListByField(ctx, identityDef, "source_relation", sourceRelation)
	if err != nil {
		return nil, fmt.Errorf("load identities for relation %s: %w", sourceRelation, err)
	}
	byKey := make(map[string]identityTarget)
	for _, c := range existing {
		src, _ := c.Data["source_id"].(string)
		et, _ := c.Data["entity_type"].(string)
		key, _ := c.Data["external_key"].(string)
		if src != sourceRecordID || et != def.EntityType || key == "" {
			continue
		}
		if prev, dup := byKey[key]; dup {
			prev.ambiguous = true
			byKey[key] = prev
			continue
		}
		recordID, _ := c.Data["record_id"].(string)
		byKey[key] = identityTarget{recordID: recordID}
	}

	seenThisRun := make(map[string]int) // key -> 1-based row number that used it
	results := make([]UpsertResult, len(previews))
	for i, res := range previews {
		results[i] = UpsertResult{RowResult: res}
		if res.Err != nil {
			continue
		}
		// Same context honoring as csvimport.CommitRows: rows never
		// attempted carry the context error, so the per-row report stays
		// honest about what was and wasn't tried.
		if err := ctx.Err(); err != nil {
			results[i].Err = fmt.Errorf("not attempted: %w", err)
			continue
		}
		key := rowKey(rows, i, keyIdx)
		if key == "" {
			results[i].Err = missingKeyError(keyColumn)
			continue
		}
		if firstRow, dup := seenThisRun[key]; dup {
			results[i].Err = fmt.Errorf("duplicate key: %q was already used by row %d of this run — a key column must be unique per row, refusing to overwrite the earlier row's record", key, firstRow)
			continue
		}
		seenThisRun[key] = res.RowNumber

		target, known := byKey[key]
		switch {
		case !known:
			rec, err := records.Create(ctx, def, res.Data, actor)
			if err != nil {
				results[i].Err = err
				continue
			}
			results[i].RecordID = rec.ID
			// Identity written immediately after its record — see the
			// sequencing note in the doc comment above.
			if _, err := identities.Create(ctx, identityDef, map[string]any{
				"source_id":       sourceRecordID,
				"source_relation": sourceRelation,
				"entity_type":     def.EntityType,
				"record_id":       rec.ID,
				"external_key":    key,
			}, actor); err != nil {
				results[i].Err = fmt.Errorf("record %s created but its identity row failed (a re-run will duplicate it once): %w", rec.ID, err)
				continue
			}
			byKey[key] = identityTarget{recordID: rec.ID}
		case target.ambiguous:
			results[i].Err = fmt.Errorf("ambiguous identity: several ExternalIdentity rows match key %q for this source, relation and entity type — refusing to guess which record to update", key)
		case target.recordID == "":
			results[i].Err = fmt.Errorf("identity row for key %q has no record_id", key)
		default:
			current, err := records.Get(ctx, def, target.recordID)
			if err != nil {
				results[i].Err = fmt.Errorf("read record %s for merge: %w", target.recordID, err)
				continue
			}
			merged := make(map[string]any, len(current.Data)+len(res.Data))
			maps.Copy(merged, current.Data)
			maps.Copy(merged, res.Data)
			// The merge made this a read-modify-write, so the version
			// check is load-bearing now (it wasn't when the update was a
			// blind replacement): a human saving between our Get and
			// Update would otherwise have their edit clobbered with our
			// stale snapshot of the unmapped fields. A conflict is a
			// per-row error telling the user to re-run — honest and
			// cheap, versus a retry loop hiding a genuine race.
			if _, err := records.Update(ctx, def, target.recordID, merged, &current.Version, actor); err != nil {
				if errors.Is(err, data.ErrVersionConflict) {
					results[i].Err = fmt.Errorf("record %s was edited while this import ran — row skipped, re-run the import to refresh it", target.recordID)
					continue
				}
				results[i].Err = err
				continue
			}
			results[i].RecordID = target.recordID
			results[i].Updated = true
		}
	}
	return results, nil
}
