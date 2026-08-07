// Package crud is the generator described in ADR-0017 §5: given an Entity
// Definition, it provides create/read/update/delete against the generic
// records table, with validation and audit logging on every mutation. It
// must never special-case an entity_type by name — behaviour comes only
// from the Definition passed in (CLAUDE.md).
package crud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/ids"
)

// Hook is a post-write callback the caller who constructs an Engine may
// register per entity type via SetHook — the "lifecycle hooks (on-
// create/update → ...)" mechanism ADR-0001's base-model design already
// anticipated but never built (see uc-infra ADR-0004's own "not fully
// closed" note, which is what this closes). Called from within the same
// transaction as the record write, right before commit, so a hook that
// returns an error rolls back the whole write — e.g. a ledger posting
// that fails must also fail the GoodsReceipt/CustomerInvoice write that
// triggered it, not leave an un-posted business document behind.
//
// This does NOT reintroduce entity-type branching into the generic
// engine (CLAUDE.md's kernel-boundary rule): Create/Update look the
// entity type up in a plain map and call whatever was registered — the
// engine itself has zero knowledge of what a "GoodsReceipt" or
// "CustomerInvoice" is, exactly the same "generic dispatch over
// caller-supplied data" shape cmd/provision-tenant's own
// modulePublishers map already uses.
type Hook func(ctx context.Context, tx *sql.Tx, def *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error

// Engine is the generic CRUD engine, operating against one tenant's own
// database (ADR-0003 — the *sql.DB passed to NewEngine is already
// resolved to a specific tenant via internal/tenantdb.Router, not shared
// across tenants). One Engine serves every entity type within that
// tenant; the Definition supplied per call is what makes each entity
// distinct.
type Engine struct {
	db      *sql.DB
	records *data.RecordRepo
	audit   *data.AuditRepo
	keys    *data.RecordUniqueKeyRepo
	hooks   map[string]Hook
}

func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		db:      db,
		records: data.NewRecordRepo(db),
		audit:   data.NewAuditRepo(db),
		keys:    data.NewRecordUniqueKeyRepo(db),
		hooks:   map[string]Hook{},
	}
}

// SetHook registers hook to run after every Create/Update of entityType,
// within the write's own transaction — see Hook's own doc comment. Not
// part of NewEngine's signature deliberately: every existing caller
// (dozens, across cmd/ and every test in this repo) stays unchanged;
// only application wiring that actually needs a hook calls this after
// construction — cmd/seed-demo-data does so directly, and
// cmd/universal-core's real HTTP path does so indirectly via
// internal/api.Handler.RegisterHook (applied per request in that
// package's own scope(), so internal/api itself never has to import a
// specific kernel module to wire one in).
func (e *Engine) SetHook(entityType string, hook Hook) {
	e.hooks[entityType] = hook
}

func (e *Engine) runHook(ctx context.Context, tx *sql.Tx, def *entity.Definition, rec data.Record, action audit.Action, actor audit.Actor) error {
	hook, ok := e.hooks[def.EntityType]
	if !ok {
		return nil
	}
	if err := hook(ctx, tx, def, rec, action, actor); err != nil {
		return fmt.Errorf("%s hook: %w", action, err)
	}
	return nil
}

// Create validates the incoming data against def, inserts the record, and
// writes an audit entry — atomically in one transaction, so a record can
// never exist without its audit trail (ADR-0017 §14/§16: audit is written
// from the same transaction as the mutation, never bolted on after).
func (e *Engine) Create(ctx context.Context, def *entity.Definition, fields map[string]any, actor audit.Actor) (data.Record, error) {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return data.Record{}, fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return data.Record{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	if err := checkReferenceTargetConstraints(ctx, tx, e.records, def, fields); err != nil {
		return data.Record{}, err
	}

	rec, err := e.records.CreateTx(ctx, tx, def.EntityType, fields)
	if err != nil {
		return data.Record{}, fmt.Errorf("create record: %w", err)
	}

	if err := WriteUniqueConstraintKeys(ctx, tx, e.keys, def, rec.ID, fields); err != nil {
		return data.Record{}, err
	}

	auditEntry, err := audit.New(def.EntityType, rec.ID, audit.ActionCreate, actor, fields)
	if err != nil {
		return data.Record{}, fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return data.Record{}, fmt.Errorf("write audit entry: %w", err)
	}

	if err := e.runHook(ctx, tx, def, rec, audit.ActionCreate, actor); err != nil {
		return data.Record{}, err
	}

	if err := tx.Commit(); err != nil {
		return data.Record{}, fmt.Errorf("commit tx: %w", err)
	}
	return rec, nil
}

// ErrHookRejected wraps a Hook's own rejection of a write as the
// caller's bad input rather than an infrastructure failure — a hook
// wraps its specific reason with %w against this (e.g. purchasing.
// ValidateStockTransfer rejecting from_facility_id ==
// to_facility_id), and internal/api's writeCrudError checks errors.Is
// against this the same way it already does for ErrReferenceCycle,
// mapping to 400 with the hook's own message rather than falling through
// to a generic 500. Exists because internal/api/handlers.go is
// deliberately entity-agnostic (RegisterHook's own doc comment — an
// independent review already caught and fixed one case of this file
// importing a concrete kernel module by name) and so cannot check a
// purchasing- or sales-specific sentinel directly; every hook that wants
// to reject a write as a 400 needs this one generic, kernel-level
// sentinel to wrap instead. Not every hook error should use this — a
// genuine infrastructure failure (PostGoodsReceiptLineToLedger's ledger-
// posting errors, say) is correctly a 500, and MatchVendorInvoiceOnUpdate
// deliberately never rejects at all (redirects to match_exception
// instead, its own doc comment explains why) — this is only for the
// narrower case of a hook enforcing a real business-rule input
// constraint the generic entity/crud engine has no field-level mechanism
// for (cross-field inequality — entity.Field's Min/Max, uc-infra#80,
// covers single-field bounds but not "field A must differ from field B").
var ErrHookRejected = errors.New("hook rejected the write")

// ErrReferenceCycle is returned by Update when a self-referencing
// FieldReference field (e.g. Account.parent_account_id,
// Department.parent_department_id, Position.reports_to_position_id)
// would form a cycle — see checkSelfReferenceCycle's own doc comment.
var ErrReferenceCycle = errors.New("self-reference would form a cycle")

// selfReferenceFields returns every field on def that references def's
// own entity type — the shape any hierarchy field (parent/reports-to)
// takes generically, without def ever naming a specific entity type.
func selfReferenceFields(def *entity.Definition) []entity.Field {
	var out []entity.Field
	for _, f := range def.Fields {
		if f.Type == entity.FieldReference && f.Target == def.EntityType {
			out = append(out, f)
		}
	}
	return out
}

// maxReferenceChainWalk bounds checkSelfReferenceCycle's walk cost for a
// pathologically long but genuinely acyclic chain — the visited-set check
// below already guarantees termination on any cycle (self-involving or
// not) within the number of distinct nodes actually visited, well under
// this cap for any realistic org chart or account hierarchy; this is
// purely a defensive cap on a chain that just keeps going.
const maxReferenceChainWalk = 10_000

// checkSelfReferenceCycle walks a self-referencing field's chain starting
// at newParentID (already ids.Canonical-normalized by Update, its only
// caller), following the same field name up through each visited record,
// and fails only if the walk reaches back to recordID itself (the record
// being updated — a direct or indirect cycle this write would introduce).
// If the walk instead revisits some OTHER id first, that's a pre-existing
// cycle elsewhere in the chain that this write neither causes nor
// perpetuates — recordID provably can never be reached from a node once
// it repeats (each node has exactly one outgoing edge, so a repeat makes
// the remaining walk exactly periodic among nodes already ruled out) — so
// the walk stops and the write is allowed, the same "not this guard's
// concern" posture already taken for a dangling reference. This is the
// generic fix for the gap `finance.Account.parent_account_id` already had
// and `Department.parent_department_id`/`Position.reports_to_position_id`
// inherited: none of them had any protection against a chain that loops
// back on itself, which would be an infinite loop the moment anything
// (e.g. R17 approval routing) walks the chain upward. Lives in
// crud.Engine, not internal/kernel/entity, because it needs the record
// repo to walk stored data — entity.ValidateRecord is pure field-shape
// validation with no DB access (see ADR-0007).
//
// Every id compared, stored in visited, or queried by is passed through
// ids.Canonical first — records.id is a uuid column, so a stored field
// value that reached it through, say, a legacy CSV import (or any write
// that predates a shape check) can be a different-but-equivalent spelling
// of the same id (mixed case, no hyphens, brace-wrapped) as well as
// outright garbage. Comparing raw, un-canonicalized strings would both
// crash on garbage (uc-infra#107's original 500: GetTxIncludingDeleted's
// `WHERE id = $1` runs against a uuid column, so a genuinely non-uuid
// value surfaces as a raw Postgres "invalid input syntax for type uuid"
// driver error, not data.ErrNotFound) AND silently miss a real cycle
// routed through a differently-spelled-but-valid id (a review of the
// #107 fix's first draft caught this: the walk would treat
// "{<recordID>}" as an unrelated, resolvable id instead of recordID
// itself, letting a live cycle through). ids.Canonical's ok=false return
// is the tolerated-dangling-reference case (ADR-0007): a value that can
// never be a real records.id is treated exactly like "target not found".
//
// Reads via GetTxIncludingDeleted, not GetTx: a soft-deleted record's data
// is still physically stored, and a chain routed through one can still
// form a real cycle with a live record — the normal deleted-is-absent
// view would let that slip past as "reached the top of the chain".
//
// Callers must hold Update's per-entity-type advisory lock before calling
// this — see Update's own doc comment on why an unlocked walk is
// bypassable by two concurrent updates each closing one half of a cycle.
func checkSelfReferenceCycle(ctx context.Context, tx *sql.Tx, records *data.RecordRepo, entityType, recordID, fieldName, newParentID string) error {
	visited := map[string]bool{}
	currentID := newParentID
	for i := 0; i < maxReferenceChainWalk; i++ {
		if currentID == recordID {
			return fmt.Errorf("%s.%s: %w", entityType, fieldName, ErrReferenceCycle)
		}
		if visited[currentID] {
			return nil // a pre-existing cycle elsewhere, not involving recordID
		}
		visited[currentID] = true

		rec, err := records.GetTxIncludingDeleted(ctx, tx, entityType, currentID)
		if err != nil {
			if errors.Is(err, data.ErrNotFound) {
				return nil // dangling reference — not this guard's concern
			}
			return fmt.Errorf("walk %s.%s reference chain: %w", entityType, fieldName, err)
		}
		next, ok := rec.Data[fieldName].(string)
		if !ok || next == "" {
			return nil // reached the top of the chain
		}
		canonicalNext, validShape := ids.Canonical(next)
		if !validShape {
			return nil // tolerated dangling reference — see doc comment above
		}
		currentID = canonicalNext
	}
	return fmt.Errorf("%s.%s: reference chain exceeded %d hops (possibly corrupted): %w", entityType, fieldName, maxReferenceChainWalk, ErrReferenceCycle)
}

// Update validates and applies a full replacement of fields, atomically
// with its audit entry. expectedVersion is optimistic-locking's hook —
// nil skips the check (unconditional update, the original behaviour);
// non-nil rejects with data.ErrVersionConflict if the record has moved on
// since the caller last read it (see data.RecordRepo.Update). Returns the
// record's new version on success, so a caller re-rendering the record
// (a form, an API response) can embed the version it should check against
// next time.
//
// Also rejects a self-referencing FieldReference update that would form a
// cycle (ErrReferenceCycle) — see checkSelfReferenceCycle. Create never
// needs this check: a newly-created record's id doesn't exist until after
// the insert, so it cannot yet appear in anything's reference chain.
//
// A self-referencing update first takes a per-entity-type Postgres
// advisory transaction lock (pg_advisory_xact_lock), serializing every
// concurrent Update that touches that entity type's self-referencing
// field(s) within this tenant's database. Without it, two concurrent
// updates each walking the chain under plain READ COMMITTED can each see
// a cycle-free chain and each commit — e.g. "set A.parent = B" and "set
// B.parent = A" running at the same time neither observes the other's
// still-uncommitted write, so both checks pass and the two commits
// together produce a live two-node cycle that no single write ever
// appeared to create. expectedVersion's optimistic locking cannot catch
// this either: it guards one record's own version, not a chain spanning
// several records. The lock is released automatically at the end of this
// transaction (COMMIT or ROLLBACK) — "_xact" in the name, not held
// across requests. Scoped by entity type, not by record, deliberately:
// locking only the specific rows walked would need to know the chain in
// advance (the very thing being computed), and self-referencing writes
// (org-chart/account-hierarchy edits) are rare enough that serializing
// all of them for one entity type has no realistic throughput cost.
func (e *Engine) Update(ctx context.Context, def *entity.Definition, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error) {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return 0, fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	selfRefFields := selfReferenceFields(def)
	if len(selfRefFields) > 0 {
		if err := e.records.LockSelfReferenceChainTx(ctx, tx, def.EntityType); err != nil {
			return 0, err
		}
	}
	for _, f := range selfRefFields {
		newParentID, ok := fields[f.Name].(string)
		if !ok || newParentID == "" {
			continue
		}
		// Canonicalize both sides before comparing: id is this record's own
		// DB-generated id (always canonical), but newParentID is caller
		// input and can be a different-but-equivalent spelling of the same
		// id (mixed case, no hyphens, brace-wrapped) — a raw string compare
		// would miss a direct self-reference spelled that way. ok=false
		// means newParentID can never be a real records.id at all: a
		// tolerated dangling reference (ADR-0007), not a cycle to check —
		// see checkSelfReferenceCycle's own doc comment (uc-infra#107).
		canonicalParentID, validShape := ids.Canonical(newParentID)
		if !validShape {
			continue
		}
		canonicalID, ok := ids.Canonical(id)
		if !ok {
			canonicalID = id
		}
		if canonicalParentID == canonicalID {
			return 0, fmt.Errorf("%s.%s: %w", def.EntityType, f.Name, ErrReferenceCycle)
		}
		if err := checkSelfReferenceCycle(ctx, tx, e.records, def.EntityType, canonicalID, f.Name, canonicalParentID); err != nil {
			return 0, err
		}
	}

	if err := checkReferenceTargetConstraints(ctx, tx, e.records, def, fields); err != nil {
		return 0, err
	}

	newVersion, err := e.records.UpdateTx(ctx, tx, def.EntityType, id, fields, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("update record: %w", err)
	}

	if err := UpdateUniqueConstraintKeys(ctx, tx, e.keys, def, id, fields); err != nil {
		return 0, err
	}

	auditEntry, err := audit.New(def.EntityType, id, audit.ActionUpdate, actor, fields)
	if err != nil {
		return 0, fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return 0, fmt.Errorf("write audit entry: %w", err)
	}

	rec := data.Record{ID: id, EntityType: def.EntityType, Data: fields, Version: newVersion}
	if err := e.runHook(ctx, tx, def, rec, audit.ActionUpdate, actor); err != nil {
		return 0, err
	}

	// A hook is allowed to write to this same record again inside tx (a
	// hook-driven redirect — e.g. purchasing.MatchVendorInvoiceOnUpdate
	// overriding status_id past what the caller requested) — re-read
	// whatever version that left the row on rather than trusting
	// newVersion, captured before the hook ran. Returning the stale value
	// here would violate this method's own documented contract ("the
	// version it should check against next time") and fail the caller's
	// very next optimistic-locked Update with a false ErrVersionConflict.
	// One extra read only when a hook is registered for this entity type
	// — negligible next to the rest of this transaction's own work.
	if _, hasHook := e.hooks[def.EntityType]; hasHook {
		finalRec, err := e.records.GetTx(ctx, tx, def.EntityType, id)
		if err != nil {
			return 0, fmt.Errorf("re-read %s %s version after hook: %w", def.EntityType, id, err)
		}
		newVersion = finalRec.Version
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	return newVersion, nil
}

// Delete soft-deletes a record and writes an audit entry, atomically in
// one transaction — same "audit is written from the same transaction as
// the mutation, never bolted on after" discipline as Create/Update
// (ADR-0001 §14). Its first real caller is the AI-provider settings
// page's "revert to platform default" action (internal/api/
// aiprovidersettings.go), deleting a tenant's own AIProviderConnection
// override — not a generic per-entity-type route (there's no DELETE
// exposed on /api/records yet).
func (e *Engine) Delete(ctx context.Context, def *entity.Definition, id string, actor audit.Actor) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	if err := e.records.DeleteTx(ctx, tx, def.EntityType, id); err != nil {
		return fmt.Errorf("delete record: %w", err)
	}

	if err := deleteUniqueConstraintKeys(ctx, tx, e.keys, id); err != nil {
		return err
	}

	auditEntry, err := audit.New(def.EntityType, id, audit.ActionDelete, actor, nil)
	if err != nil {
		return fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (e *Engine) Get(ctx context.Context, def *entity.Definition, id string) (data.Record, error) {
	return e.records.Get(ctx, def.EntityType, id)
}

func (e *Engine) List(ctx context.Context, def *entity.Definition) ([]data.Record, error) {
	return e.records.List(ctx, def.EntityType)
}

// Count returns how many def records exist — see
// data.RecordRepo.CountByEntityType.
func (e *Engine) Count(ctx context.Context, def *entity.Definition) (int, error) {
	return e.records.CountByEntityType(ctx, def.EntityType)
}

// ListPage returns one page of def records — see data.RecordRepo.ListPage.
func (e *Engine) ListPage(ctx context.Context, def *entity.Definition, limit, offset int) ([]data.Record, error) {
	return e.records.ListPage(ctx, def.EntityType, limit, offset)
}

// ListPageFiltered is ListPage with an optional sort + substring filter —
// see data.ListPageOptions. The generic engine stays generic: it passes
// the options straight through, never branching on a field name.
func (e *Engine) ListPageFiltered(ctx context.Context, def *entity.Definition, opts data.ListPageOptions) ([]data.Record, error) {
	return e.records.ListPageFiltered(ctx, def.EntityType, opts)
}

// CountFiltered counts def records matching opts' filter (see
// data.RecordRepo.CountFiltered).
func (e *Engine) CountFiltered(ctx context.Context, def *entity.Definition, opts data.ListPageOptions) (int, error) {
	return e.records.CountFiltered(ctx, def.EntityType, opts)
}

// ListByField returns every def record whose fieldName == value — used
// to fetch a master-detail section's child rows (see
// data.RecordRepo.ListByField).
func (e *Engine) ListByField(ctx context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error) {
	return e.records.ListByField(ctx, def.EntityType, fieldName, value)
}
