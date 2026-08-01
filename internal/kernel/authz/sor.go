// System-of-record enforcement (uc-infra#102): a tenant declares who owns
// an entity type via foundation.SystemOfRecord rows, and a record imported
// from a read_only source cannot be hand-edited — the legacy master would
// just overwrite the edit on its next sync, silently, so the guarded
// engine refuses the edit up front instead.
//
// This is data ownership, not privilege: an admin is NOT exempt (unlike
// the tenant_admin RBAC bypass), because being allowed to write Party
// records in general says nothing about whether THIS Party record's
// master copy lives in an external database. The same reasoning covers
// machine actors — they bypass RBAC but not this check.
//
// The one sanctioned writer of a read_only source's records is that
// source's own import — and the import commit path writes target RECORDS
// through THIS guarded engine, not the raw one (internal/api's import
// route passes the guarded engine as sqlsource.CommitRowsUpserting's
// records engine, so imports stay RBAC-checked like any user-driven
// write; only the ExternalIdentity rows use the raw side-channel — see
// sqlsource.RecordEngine's doc comment). So the bypass is scoped, not
// structural: ForImportFrom (below) hands the commit path a copy of the
// engine that skips the read-only block exactly when the block would be
// attributed to the source being imported — that source writing its own
// records IS the sync read_only exists to protect. System paths that
// genuinely use raw crud.Engine (workflow steps, seeding, provisioning)
// still bypass everything here by construction, as before.
//
// Keyed off the constant entity-type names below — config-plumbing in
// authz over its own named types, the same way controlPlaneTypes already
// names Role/Permission/etc. Acceptable HERE per CLAUDE.md's
// kernel-boundary rule; never in crud/entity.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// ErrSystemOfRecordReadOnly is the sentinel every read-only
// system-of-record refusal matches via errors.Is — the same
// typed-sentinel convention crud.ErrHookRejected and ErrDenied already
// established for internal/api to map to an HTTP status and a
// TRANSLATED message later (the Error() strings in this file are for
// logs only, never rendered to a user — no user-facing English lives
// here).
var ErrSystemOfRecordReadOnly = errors.New("record is owned by a read-only system of record")

// ErrSystemOfRecordModeReserved rejects saving a SystemOfRecord row with
// mode "bidirectional": the value is declared in the entity's enum (so
// honoring it later is not a breaking enum change) but reserved for the
// sync engine's later slices — see foundation.SystemOfRecord's doc
// comment. Config validation in the guarded engine, deliberately not an
// entity-enum restriction.
var ErrSystemOfRecordModeReserved = errors.New("system-of-record mode is reserved and cannot be saved yet")

// SoRReadOnlyError carries which entity type and which external source
// blocked the write, so internal/api can render a specific translated
// message ("owned by <source name>") without re-querying. Matches
// ErrSystemOfRecordReadOnly under errors.Is.
type SoRReadOnlyError struct {
	EntityType string
	SourceID   string
	SourceName string
}

func (e *SoRReadOnlyError) Error() string {
	return fmt.Sprintf("%s record is owned by read-only external source %s (%s)", e.EntityType, e.SourceName, e.SourceID)
}

func (e *SoRReadOnlyError) Is(target error) bool {
	return target == ErrSystemOfRecordReadOnly
}

// The entity-type and field names this check is plumbed with — constants
// here, data everywhere else (see the package-section comment above).
const (
	systemOfRecordType   = "SystemOfRecord"
	externalIdentityType = "ExternalIdentity"
	externalSourceType   = "ExternalSQLSource"

	sorModeReadOnly      = "read_only"
	sorModeBidirectional = "bidirectional"
)

// sorRule is one SystemOfRecord row reduced to what enforcement uses.
type sorRule struct {
	mode     string
	sourceID string
}

// ForImportFrom returns a copy of g whose system-of-record read-only
// check treats sourceID as the writer: a block that would be attributed
// to EXACTLY that source — the matched ExternalIdentity row's source_id
// equals sourceID — is skipped, because an import from a source is that
// source's own pen and refusing it would freeze the mirror at its first
// snapshot (re-verification finding, 2026-08-01: the importer writes
// records through the GUARDED engine, so without this every keyed
// re-import of a read_only type failed per-row on its own protection).
// A read_only rule for a DIFFERENT source still blocks — an import from
// source B touching source A's records errors per-row, honestly.
//
// ONLY the SoR read-only block is scoped out. RBAC entity/field checks
// run unchanged — the import stays a permission-checked user-driven
// write path (ADR-0006) — and the reserved-mode check is untouched.
//
// Callers: the import commit path (internal/api's extsqlimport route),
// passing the id of the ExternalSQLSource actually being imported —
// nothing else should hold one of these. A copy, deliberately not a
// mutation of g: the per-request GuardedEngine is shared by every other
// handler code path in the request, and disarming the check in place
// would disarm it for all of them; the copy shares g's resolver and
// memos (same request, same data) but confines the bypass to the
// import's own engine value.
func (g *GuardedEngine) ForImportFrom(sourceID string) *GuardedEngine {
	c := *g
	c.sorBypassSourceID = sourceID
	return &c
}

// sorRules loads the SystemOfRecord rows governing entityType, memoized
// per GuardedEngine instance (per-request, like the Resolver's memos —
// R6's caching ladder starts at per-request, not cross-request). One
// ListByField per entity type per request, and only on Update/Delete
// paths — reads and creates never pay for it.
func (g *GuardedEngine) sorRules(ctx context.Context, entityType string) ([]sorRule, error) {
	if rules, ok := g.sorMemo[entityType]; ok {
		return rules, nil
	}
	rows, err := data.NewRecordRepo(g.res.db).ListByField(ctx, systemOfRecordType, "entity_type", entityType)
	if err != nil {
		return nil, fmt.Errorf("list %s rows for %s: %w", systemOfRecordType, entityType, err)
	}
	rules := make([]sorRule, 0, len(rows))
	for _, row := range rows {
		mode, _ := row.Data["mode"].(string)
		src, _ := row.Data["source_id"].(string)
		rules = append(rules, sorRule{mode: mode, sourceID: src})
	}
	if g.sorMemo == nil {
		g.sorMemo = make(map[string][]sorRule)
	}
	g.sorMemo[entityType] = rules
	return rules, nil
}

// checkSoRReadOnly refuses an Update/Delete of a record that (a) belongs
// to an entity type some SystemOfRecord row declares read_only, and (b)
// is itself identified (via an ExternalIdentity row) as having been
// imported from that read_only source. Records with no identity row stay
// editable — a hand-created Party in a tenant that also mirrors NAV
// customers is still the platform's own record. Entity types with no
// SystemOfRecord rows are unaffected entirely.
//
// Most-restrictive-wins over multiple rows (foundation.SystemOfRecord's
// doc comment): any read_only row blocks, whatever other rows for the
// same entity type say. A read_only row with a BLANK source_id — an
// ownership claim naming no owner, which the entity level cannot forbid
// (source_id is optional for platform_owned's sake) — is read the
// fail-safe way too: it marks every identified (imported) record of the
// type read-only regardless of which source it came from, rather than
// silently protecting nothing.
//
// Runs AFTER checkWrite: RBAC answers "may you write this type at all"
// first (and its refusal, not this one, is what a denied user sees);
// this answers the narrower "may you write THIS record."
func (g *GuardedEngine) checkSoRReadOnly(ctx context.Context, def *entity.Definition, id string) error {
	rules, err := g.sorRules(ctx, def.EntityType)
	if err != nil {
		return err
	}
	readOnlySources := make(map[string]bool)
	anySource := false // a read_only rule with blank source_id
	for _, r := range rules {
		if r.mode != sorModeReadOnly {
			continue
		}
		if r.sourceID == "" {
			anySource = true
		} else {
			readOnlySources[r.sourceID] = true
		}
	}
	if !anySource && len(readOnlySources) == 0 {
		return nil
	}

	records := data.NewRecordRepo(g.res.db)
	idents, err := records.ListByField(ctx, externalIdentityType, "record_id", id)
	if err != nil {
		return fmt.Errorf("list %s rows for %s %s: %w", externalIdentityType, def.EntityType, id, err)
	}
	for _, ident := range idents {
		et, _ := ident.Data["entity_type"].(string)
		if et != def.EntityType {
			// record_id values are UUIDs so cross-type collisions are not
			// expected, but the identity scope includes entity_type and the
			// filter honors it (ListByField filters one field; the rest is
			// filtered here in Go).
			continue
		}
		src, _ := ident.Data["source_id"].(string)
		if g.sorBypassSourceID != "" && src == g.sorBypassSourceID {
			// ForImportFrom's scoped bypass: this engine copy IS that
			// source's import writing its own records. Decided by the
			// IDENTITY row's source, not the rule's — a blank-source
			// read_only rule has no source to compare, and what makes the
			// write legitimate is whose record it provably is, not how
			// the rule was spelled.
			continue
		}
		if !anySource && !readOnlySources[src] {
			continue
		}
		// Failure path only: one extra Get to carry the source's display
		// name into the error. Best-effort — a missing/deleted source
		// still blocks (the identity row is what proves foreign
		// ownership), just without a name.
		name := ""
		if src != "" {
			if srcRec, err := records.Get(ctx, externalSourceType, src); err == nil {
				name, _ = srcRec.Data["name"].(string)
			}
		}
		return &SoRReadOnlyError{EntityType: def.EntityType, SourceID: src, SourceName: name}
	}
	return nil
}

// checkSoRModeReserved refuses saving a SystemOfRecord row whose mode is
// the reserved "bidirectional" value — see ErrSystemOfRecordModeReserved.
// Applies to the user-driven Create AND Update paths (a reserved value is
// no more savable by editing an existing row into it).
func checkSoRModeReserved(def *entity.Definition, fields map[string]any) error {
	if def.EntityType != systemOfRecordType {
		return nil
	}
	if mode, _ := fields["mode"].(string); mode == sorModeBidirectional {
		return fmt.Errorf("mode %q: %w", mode, ErrSystemOfRecordModeReserved)
	}
	return nil
}
