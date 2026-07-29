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

// ErrVersionConflict is returned by UpdateTx/Update when the caller
// passed an expectedVersion that no longer matches the record's current
// version — someone else (or the same user, in another tab) saved a
// change since this caller last read the record. Distinct from
// ErrNotFound: the record is right there, just not at the version the
// caller thought it was.
var ErrVersionConflict = errors.New("data: record version conflict")

// Record is one row of the generic records table. Version is the
// optimistic-locking counter (starts at 1, incremented on every
// successful update) — see 005_record_version.sql's doc comment.
type Record struct {
	ID         string
	EntityType string
	Data       map[string]any
	Version    int
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
// Every method takes a *sql.DB/querier already resolved to one tenant's
// own database (internal/tenantdb.Router) — tenant isolation is physical
// (ADR-0003), not a query-level filter, so no method here takes a
// tenantID.
type RecordRepo struct {
	db *sql.DB
}

func NewRecordRepo(db *sql.DB) *RecordRepo {
	return &RecordRepo{db: db}
}

// Create inserts a record using the repo's own connection pool (no
// caller-supplied transaction). Use CreateTx when the write must be
// atomic with another operation, such as an audit entry.
func (r *RecordRepo) Create(ctx context.Context, entityType string, data map[string]any) (Record, error) {
	return r.CreateTx(ctx, r.db, entityType, data)
}

func (r *RecordRepo) CreateTx(ctx context.Context, q querier, entityType string, data map[string]any) (Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Record{}, fmt.Errorf("marshal record data: %w", err)
	}
	var id string
	var version int
	err = q.QueryRowContext(ctx,
		`INSERT INTO records (entity_type, data)
		 VALUES ($1, $2)
		 RETURNING id, version`,
		entityType, raw,
	).Scan(&id, &version)
	if err != nil {
		return Record{}, fmt.Errorf("insert record: %w", err)
	}
	return Record{ID: id, EntityType: entityType, Data: data, Version: version}, nil
}

func (r *RecordRepo) Get(ctx context.Context, entityType, id string) (Record, error) {
	return r.get(ctx, r.db, entityType, id)
}

// GetTx is Get against a caller-supplied querier/tx instead of r.db —
// the same "same shape as the Tx write methods, now needed for a read
// too" reasoning CreateTx/UpdateTx/DeleteTx already established. First
// real caller: internal/kernel/crud's post-write hooks (ADR-0001's
// "lifecycle hooks" mechanism), which need to read another record (e.g.
// a GoodsReceiptLine's POLine) from within the same transaction as the
// write that triggered the hook, not a separate connection.
func (r *RecordRepo) GetTx(ctx context.Context, q querier, entityType, id string) (Record, error) {
	return r.get(ctx, q, entityType, id)
}

func (r *RecordRepo) get(ctx context.Context, q querier, entityType, id string) (Record, error) {
	var raw []byte
	var version int
	err := q.QueryRowContext(ctx,
		`SELECT data, version FROM records
		 WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
	).Scan(&raw, &version)
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
	return Record{ID: id, EntityType: entityType, Data: data, Version: version}, nil
}

func (r *RecordRepo) List(ctx context.Context, entityType string) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND deleted_at IS NULL
		 ORDER BY created_at`,
		entityType,
	)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// CountByEntityType returns how many non-deleted records of entityType
// exist — the total a pager needs to compute page count, kept as its own
// query rather than folded into ListPage via a window function
// (count(*) OVER()) so the common "page beyond the last one" case (an
// empty LIMIT/OFFSET result set) doesn't lose the total along with the
// rows; two simple queries over one query with an edge case.
func (r *RecordRepo) CountByEntityType(ctx context.Context, entityType string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE entity_type = $1 AND deleted_at IS NULL`,
		entityType,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count records: %w", err)
	}
	return n, nil
}

// ListPage returns one page of entityType records — the same ordering as
// List (created_at, with id as a tiebreaker so pagination stays
// deterministic even when two records share a created_at). Kept distinct
// from List rather than adding limit/offset params there: every existing
// List caller (the JSON API, reference-option dropdowns, master-detail
// child loading) genuinely wants every matching row, not a page of them
// — this is additive for the one caller that doesn't (the HTML list
// page).
func (r *RecordRepo) ListPage(ctx context.Context, entityType string, limit, offset int) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND deleted_at IS NULL
		 ORDER BY created_at, id
		 LIMIT $2 OFFSET $3`,
		entityType, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list records page: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// ListByField returns every non-deleted record of entityType whose data
// has fieldName == value — the query a master-detail section needs to
// find its child rows (e.g. every POLine whose purchase_order_id matches
// the parent PurchaseOrder's id). fieldName is passed as a bind
// parameter to the ->> operator, never concatenated into the query text,
// so a caller-controlled field name can't alter the query's structure.
func (r *RecordRepo) ListByField(ctx context.Context, entityType, fieldName, value string) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, data, version FROM records
		 WHERE entity_type = $1 AND data->>$2 = $3 AND deleted_at IS NULL
		 ORDER BY created_at`,
		entityType, fieldName, value,
	)
	if err != nil {
		return nil, fmt.Errorf("list records by field: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var id string
		var raw []byte
		var version int
		if err := rows.Scan(&id, &raw, &version); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("unmarshal record data: %w", err)
		}
		out = append(out, Record{ID: id, EntityType: entityType, Data: data, Version: version})
	}
	return out, rows.Err()
}

// Update replaces a record's data using the repo's own connection pool.
// Use UpdateTx when the write must be atomic with another operation.
// expectedVersion is optimistic-locking's whole point: nil means "don't
// check" (today's original unconditional-update behaviour, preserved so
// every caller that predates versioning keeps working unchanged); non-nil
// must match the record's current version or the update is rejected with
// ErrVersionConflict instead of silently clobbering a concurrent edit.
// Returns the record's new version on success.
func (r *RecordRepo) Update(ctx context.Context, entityType, id string, data map[string]any, expectedVersion *int) (int, error) {
	return r.UpdateTx(ctx, r.db, entityType, id, data, expectedVersion)
}

func (r *RecordRepo) UpdateTx(ctx context.Context, q querier, entityType, id string, data map[string]any, expectedVersion *int) (int, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshal record data: %w", err)
	}
	var newVersion int
	err = q.QueryRowContext(ctx,
		`UPDATE records SET data = $1, version = version + 1, updated_at = now()
		 WHERE id = $2 AND entity_type = $3 AND deleted_at IS NULL
		   AND ($4::int IS NULL OR version = $4)
		 RETURNING version`,
		raw, id, entityType, expectedVersion,
	).Scan(&newVersion)
	if err == nil {
		return newVersion, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("update record: %w", err)
	}
	// Zero rows updated — either the record doesn't exist (deleted, wrong
	// id) or it exists but expectedVersion didn't match. The single
	// UPDATE above can't distinguish those (its WHERE clause ANDs both
	// conditions together), so a follow-up existence check resolves
	// which error the caller actually needs: 404 vs. 409 are different
	// user-facing outcomes ("this record is gone" vs. "someone else just
	// changed it, reload and retry").
	var exists bool
	if checkErr := q.QueryRowContext(ctx,
		`SELECT true FROM records WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
	).Scan(&exists); checkErr != nil {
		if errors.Is(checkErr, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("check record existence after failed update: %w", checkErr)
	}
	return 0, ErrVersionConflict
}

// Delete soft-deletes a record using the repo's own connection pool (no
// caller-supplied transaction). Use DeleteTx when the write must be
// atomic with another operation, such as an audit entry.
func (r *RecordRepo) Delete(ctx context.Context, entityType, id string) error {
	return r.DeleteTx(ctx, r.db, entityType, id)
}

func (r *RecordRepo) DeleteTx(ctx context.Context, q querier, entityType, id string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE records SET deleted_at = now()
		 WHERE id = $1 AND entity_type = $2 AND deleted_at IS NULL`,
		id, entityType,
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
