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

// Engine is the generic CRUD engine. One Engine serves every entity type;
// the Definition supplied per call is what makes each entity distinct.
type Engine struct {
	db      *sql.DB
	records *data.RecordRepo
	audit   *data.AuditRepo
}

func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		db:      db,
		records: data.NewRecordRepo(db),
		audit:   data.NewAuditRepo(db),
	}
}

// Create validates the incoming data against def, inserts the record, and
// writes an audit entry — atomically in one transaction, so a record can
// never exist without its audit trail (ADR-0017 §14/§16: audit is written
// from the same transaction as the mutation, never bolted on after).
func (e *Engine) Create(ctx context.Context, def *entity.Definition, tenantID string, fields map[string]any, actor audit.Actor) (data.Record, error) {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return data.Record{}, fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return data.Record{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after a successful commit

	rec, err := e.records.CreateTx(ctx, tx, tenantID, def.EntityType, fields)
	if err != nil {
		return data.Record{}, fmt.Errorf("create record: %w", err)
	}

	auditEntry, err := audit.New(tenantID, def.EntityType, rec.ID, audit.ActionCreate, actor, fields)
	if err != nil {
		return data.Record{}, fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return data.Record{}, fmt.Errorf("write audit entry: %w", err)
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
func (e *Engine) Update(ctx context.Context, def *entity.Definition, tenantID, id string, fields map[string]any, expectedVersion *int, actor audit.Actor) (int, error) {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return 0, fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	newVersion, err := e.records.UpdateTx(ctx, tx, tenantID, def.EntityType, id, fields, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("update record: %w", err)
	}

	auditEntry, err := audit.New(tenantID, def.EntityType, id, audit.ActionUpdate, actor, fields)
	if err != nil {
		return 0, fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return 0, fmt.Errorf("write audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	return newVersion, nil
}

func (e *Engine) Get(ctx context.Context, def *entity.Definition, tenantID, id string) (data.Record, error) {
	return e.records.Get(ctx, tenantID, def.EntityType, id)
}

func (e *Engine) List(ctx context.Context, def *entity.Definition, tenantID string) ([]data.Record, error) {
	return e.records.List(ctx, tenantID, def.EntityType)
}

// Count returns how many def records tenantID has — see
// data.RecordRepo.CountByEntityType.
func (e *Engine) Count(ctx context.Context, def *entity.Definition, tenantID string) (int, error) {
	return e.records.CountByEntityType(ctx, tenantID, def.EntityType)
}

// ListPage returns one page of def records — see data.RecordRepo.ListPage.
func (e *Engine) ListPage(ctx context.Context, def *entity.Definition, tenantID string, limit, offset int) ([]data.Record, error) {
	return e.records.ListPage(ctx, tenantID, def.EntityType, limit, offset)
}

// ListByField returns every def record whose fieldName == value — used
// to fetch a master-detail section's child rows (see
// data.RecordRepo.ListByField).
func (e *Engine) ListByField(ctx context.Context, def *entity.Definition, tenantID, fieldName, value string) ([]data.Record, error) {
	return e.records.ListByField(ctx, tenantID, def.EntityType, fieldName, value)
}
