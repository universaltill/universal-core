// Package data holds every repository — the only place raw SQL is allowed
// to live outside internal/db/migrations (CLAUDE.md, mirroring
// universal-till's enforced pattern).
package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrNotFound = errors.New("data: record not found")

// Record is one row of the generic records table.
type Record struct {
	ID         string
	TenantID   string
	EntityType string
	Data       map[string]any
}

// querier is satisfied by both *sql.DB and *sql.Tx, so every repository
// method works standalone or inside a caller-managed transaction (needed
// so a record write and its audit entry commit atomically together).
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordRepo is the repository for the generic entity-storage table.
// Every method takes tenantID explicitly — no query relies on an implicit
// tenant context (CLAUDE.md's multi-tenancy rule).
type RecordRepo struct {
	db *sql.DB
}

func NewRecordRepo(db *sql.DB) *RecordRepo {
	return &RecordRepo{db: db}
}

// Create inserts a record using the repo's own connection pool (no
// caller-supplied transaction). Use CreateTx when the write must be
// atomic with another operation, such as an audit entry.
func (r *RecordRepo) Create(ctx context.Context, tenantID, entityType string, data map[string]any) (Record, error) {
	return r.CreateTx(ctx, r.db, tenantID, entityType, data)
}

func (r *RecordRepo) CreateTx(ctx context.Context, q querier, tenantID, entityType string, data map[string]any) (Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Record{}, fmt.Errorf("marshal record data: %w", err)
	}
	var id string
	err = q.QueryRowContext(ctx,
		`INSERT INTO records (tenant_id, entity_type, data)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		tenantID, entityType, raw,
	).Scan(&id)
	if err != nil {
		return Record{}, fmt.Errorf("insert record: %w", err)
	}
	return Record{ID: id, TenantID: tenantID, EntityType: entityType, Data: data}, nil
}

func (r *RecordRepo) Get(ctx context.Context, tenantID, entityType, id string) (Record, error) {
	return r.get(ctx, r.db, tenantID, entityType, id)
}

func (r *RecordRepo) get(ctx context.Context, q querier, tenantID, entityType, id string) (Record, error) {
	var raw []byte
	err := q.QueryRowContext(ctx,
		`SELECT data FROM records
		 WHERE id = $1 AND tenant_id = $2 AND entity_type = $3 AND deleted_at IS NULL`,
		id, tenantID, entityType,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get record: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Record{}, fmt.Errorf("unmarshal record data: %w", err)
	}
	return Record{ID: id, TenantID: tenantID, EntityType: entityType, Data: data}, nil
}

func (r *RecordRepo) List(ctx context.Context, tenantID, entityType string) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, data FROM records
		 WHERE tenant_id = $1 AND entity_type = $2 AND deleted_at IS NULL
		 ORDER BY created_at`,
		tenantID, entityType,
	)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, TenantID: tenantID, EntityType: entityType, Data: data})
	}
	return out, rows.Err()
}

// Update replaces a record's data using the repo's own connection pool.
// Use UpdateTx when the write must be atomic with another operation.
func (r *RecordRepo) Update(ctx context.Context, tenantID, entityType, id string, data map[string]any) error {
	return r.UpdateTx(ctx, r.db, tenantID, entityType, id, data)
}

func (r *RecordRepo) UpdateTx(ctx context.Context, q querier, tenantID, entityType, id string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal record data: %w", err)
	}
	res, err := q.ExecContext(ctx,
		`UPDATE records SET data = $1, updated_at = now()
		 WHERE id = $2 AND tenant_id = $3 AND entity_type = $4 AND deleted_at IS NULL`,
		raw, id, tenantID, entityType,
	)
	if err != nil {
		return fmt.Errorf("update record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *RecordRepo) Delete(ctx context.Context, tenantID, entityType, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE records SET deleted_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND entity_type = $3 AND deleted_at IS NULL`,
		id, tenantID, entityType,
	)
	if err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
