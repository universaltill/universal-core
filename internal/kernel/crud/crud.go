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
// with its audit entry.
func (e *Engine) Update(ctx context.Context, def *entity.Definition, tenantID, id string, fields map[string]any, actor audit.Actor) error {
	if err := entity.ValidateRecord(def, fields); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := e.records.UpdateTx(ctx, tx, tenantID, def.EntityType, id, fields); err != nil {
		return fmt.Errorf("update record: %w", err)
	}

	auditEntry, err := audit.New(tenantID, def.EntityType, id, audit.ActionUpdate, actor, fields)
	if err != nil {
		return fmt.Errorf("build audit entry: %w", err)
	}
	if err := e.audit.Insert(ctx, tx, auditEntry); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	return tx.Commit()
}

func (e *Engine) Get(ctx context.Context, def *entity.Definition, tenantID, id string) (data.Record, error) {
	return e.records.Get(ctx, tenantID, def.EntityType, id)
}

func (e *Engine) List(ctx context.Context, def *entity.Definition, tenantID string) ([]data.Record, error) {
	return e.records.List(ctx, tenantID, def.EntityType)
}
