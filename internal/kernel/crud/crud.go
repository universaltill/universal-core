// Package crud is the generator described in ADR-0017 §5: given an Entity
// Definition, it provides create/read/update/delete against the generic
// records table, with validation and audit logging on every mutation. It
// must never special-case an entity_type by name — behaviour comes only
// from the Definition passed in (CLAUDE.md).
package crud

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
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
	hooks   map[string]Hook
}

func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		db:      db,
		records: data.NewRecordRepo(db),
		audit:   data.NewAuditRepo(db),
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

	rec, err := e.records.CreateTx(ctx, tx, def.EntityType, fields)
	if err != nil {
		return data.Record{}, fmt.Errorf("create record: %w", err)
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

// Update validates and applies a full replacement of fields, atomically
// with its audit entry. expectedVersion is optimistic-locking's hook —
// nil skips the check (unconditional update, the original behaviour);
// non-nil rejects with data.ErrVersionConflict if the record has moved on
// since the caller last read it (see data.RecordRepo.Update). Returns the
// record's new version on success, so a caller re-rendering the record
// (a form, an API response) can embed the version it should check against
// next time.
func (e *Engine) Update(ctx context.Context, def *entity.Definition, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error) {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return 0, fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	newVersion, err := e.records.UpdateTx(ctx, tx, def.EntityType, id, fields, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("update record: %w", err)
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

// ListByField returns every def record whose fieldName == value — used
// to fetch a master-detail section's child rows (see
// data.RecordRepo.ListByField).
func (e *Engine) ListByField(ctx context.Context, def *entity.Definition, fieldName, value string) ([]data.Record, error) {
	return e.records.ListByField(ctx, def.EntityType, fieldName, value)
}
