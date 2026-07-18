package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// AuditRepo persists audit.Entry values. It never runs outside the same
// transaction as the mutation it records (see CRUD engine).
type AuditRepo struct {
	db *sql.DB
}

func NewAuditRepo(db *sql.DB) *AuditRepo {
	return &AuditRepo{db: db}
}

// execer is satisfied by both *sql.DB and *sql.Tx, so the same repository
// method can run standalone or inside a caller's transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (r *AuditRepo) Insert(ctx context.Context, ex execer, e audit.Entry) error {
	var raw []byte
	if e.Diff != nil {
		var err error
		raw, err = json.Marshal(e.Diff)
		if err != nil {
			return fmt.Errorf("marshal audit diff: %w", err)
		}
	}

	var modelVersion, inputHash any
	if e.Actor.ModelVersion != "" {
		modelVersion = e.Actor.ModelVersion
	}
	if h := e.Actor.InputHash(); h != "" {
		inputHash = h
	}

	_, err := ex.ExecContext(ctx,
		`INSERT INTO audit_log
		 (tenant_id, entity_type, record_id, action, actor_type, actor_id, model_version, input_hash, diff)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.TenantID, e.EntityType, nullableID(e.RecordID), string(e.Action),
		string(e.Actor.Type), e.Actor.ID, modelVersion, inputHash, raw,
	)
	if err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}

func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
