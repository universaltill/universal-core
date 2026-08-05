package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueKeyConstraintName is the Postgres constraint name
// 0006_record_unique_keys.sql gives record_unique_keys' composite UNIQUE
// index — named explicitly so classifyUniqueKeyErr can match on it.
const uniqueKeyConstraintName = "record_unique_keys_key_uq"

// ErrUniqueKeyConflict is returned by RecordUniqueKeyRepo's write methods
// when the (entity_type, constraint_name, key_value) triple already
// belongs to a different record — the real Postgres UNIQUE index
// (record_unique_keys_key_uq) is what decides this, not application
// logic (ADR-0018 §3: a Go-side check alone cannot close the race
// between two concurrent transactions).
//
// Classified via pgconn.PgError's SQLSTATE (23505, unique_violation) AND
// its ConstraintName, never by substring-matching the driver error's
// TEXT — independent review, uc-infra#81 follow-up: a substring match on
// "record_unique_keys_key_uq" also matches Postgres' unrelated "index
// row size exceeds btree version 4 maximum" error (which names the
// index in its own message when key_value is too large for a btree
// entry), misreporting a genuinely oversized value as a duplicate. The
// SQLSTATE distinguishes the two; the row-size case is instead avoided
// structurally by RecordUniqueKeyRepo storing a fixed-length hash of the
// key rather than the raw field values (crud.uniqueKeyValue).
var ErrUniqueKeyConflict = errors.New("data: unique key already used by another record")

// RecordUniqueKeyRepo is the repository for record_unique_keys — the
// side table entity.Definition.Unique's enforcement stage
// (internal/kernel/crud) depends on for its actual correctness guarantee
// (ADR-0018 §3(c)). Same "generic table, caller supplies all field-name-
// derived values as parameters" shape as RecordRepo; crud.Engine never
// executes SQL directly, per CLAUDE.md's "raw SQL lives only in
// internal/data" rule.
type RecordUniqueKeyRepo struct {
	db *sql.DB
}

func NewRecordUniqueKeyRepo(db *sql.DB) *RecordUniqueKeyRepo {
	return &RecordUniqueKeyRepo{db: db}
}

// InsertTx adds one record_unique_keys row inside the caller's
// transaction — the actual enforcement moment: if another live record
// already holds this (entityType, constraintName, keyValue), Postgres'
// UNIQUE index rejects the INSERT atomically and this returns
// ErrUniqueKeyConflict, never a raw driver error.
func (r *RecordUniqueKeyRepo) InsertTx(ctx context.Context, q querier, entityType, constraintName, keyValue, recordID string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO record_unique_keys (entity_type, constraint_name, key_value, record_id) VALUES ($1, $2, $3, $4)`,
		entityType, constraintName, keyValue, recordID)
	if classified := classifyUniqueKeyErr(err); classified != nil {
		if errors.Is(classified, ErrUniqueKeyConflict) {
			return classified
		}
		return fmt.Errorf("insert unique key %s.%s: %w", entityType, constraintName, classified)
	}
	return nil
}

// UpdateValueTx re-points an existing record_unique_keys row at a new
// key_value (the record's key fields changed) — returns rowsAffected so
// the caller (updateUniqueConstraintKeys) can tell "no row existed yet"
// (0, the record predates this constraint) from "updated" (1), the same
// distinction sql.Result.RowsAffected always gives.
func (r *RecordUniqueKeyRepo) UpdateValueTx(ctx context.Context, q querier, entityType, constraintName, keyValue, recordID string) (rowsAffected int64, err error) {
	res, execErr := q.ExecContext(ctx,
		`UPDATE record_unique_keys SET key_value = $1 WHERE record_id = $2 AND entity_type = $3 AND constraint_name = $4`,
		keyValue, recordID, entityType, constraintName)
	if classified := classifyUniqueKeyErr(execErr); classified != nil {
		if errors.Is(classified, ErrUniqueKeyConflict) {
			return 0, classified
		}
		return 0, fmt.Errorf("update unique key %s.%s: %w", entityType, constraintName, classified)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("update unique key %s.%s: rows affected: %w", entityType, constraintName, err)
	}
	return n, nil
}

// DeleteForConstraintTx removes the record_unique_keys row for one
// (recordID, constraintName) pair — the "a key field became absent"
// branch of an Update, freeing that combination for reuse.
func (r *RecordUniqueKeyRepo) DeleteForConstraintTx(ctx context.Context, q querier, entityType, constraintName, recordID string) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM record_unique_keys WHERE record_id = $1 AND entity_type = $2 AND constraint_name = $3`,
		recordID, entityType, constraintName,
	); err != nil {
		return fmt.Errorf("delete unique key %s.%s for record %s: %w", entityType, constraintName, recordID, err)
	}
	return nil
}

// DeleteForRecordTx removes EVERY record_unique_keys row for recordID,
// across all of its Definition's declared Unique sets in one statement —
// called on soft-delete (crud.Engine.Delete), so every combination the
// record held becomes reusable, regardless of how many sets it belongs to.
func (r *RecordUniqueKeyRepo) DeleteForRecordTx(ctx context.Context, q querier, recordID string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM record_unique_keys WHERE record_id = $1`, recordID); err != nil {
		return fmt.Errorf("delete unique keys for record %s: %w", recordID, err)
	}
	return nil
}

// classifyUniqueKeyErr narrows err to ErrUniqueKeyConflict only when it
// is genuinely a violation of record_unique_keys' own UNIQUE constraint
// (SQLSTATE 23505 unique_violation, matched by name) — any other
// Postgres error (including the unrelated "index row size exceeds btree
// maximum" a too-long key_value would raise, SQLSTATE 54000) passes
// through unchanged, so callers never misreport a different failure as a
// duplicate-key business rule violation.
func classifyUniqueKeyErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == uniqueKeyConstraintName {
		return ErrUniqueKeyConflict
	}
	return err
}
