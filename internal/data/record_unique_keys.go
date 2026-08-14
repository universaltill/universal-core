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

// InsertTx establishes that recordID owns keyValue for (entityType,
// constraintName) inside the caller's transaction — the actual
// enforcement moment: if another live record already holds this
// (entityType, constraintName, keyValue), Postgres' UNIQUE index rejects
// the write atomically and this returns ErrUniqueKeyConflict, never a
// raw driver error.
//
// Reconciling, not a plain insert-only (uc-infra#237): if recordID
// already owns a row for (entityType, constraintName) — regardless of
// what key_value that row currently holds — this repoints it to keyValue
// (via UpdateValueTx) instead of inserting a second row. A plain,
// non-reconciling INSERT here would leave that stale row behind: two
// rows for the same (record, constraint) is exactly the shape
// independent review caught this exposing — a record whose Unique set
// was removed, edited while absent (UpdateUniqueConstraintKeys only ever
// walks the CURRENT Definition, so an absent constraint's old row is
// never touched going forward), then re-added: a later backfill's plain
// INSERT of the record's new key_value would collide with nothing (the
// old row's key_value differs), silently creating a second row for that
// (record_id, entity_type, constraint_name) — after which
// UpdateValueTx's own WHERE clause (record_id+entity_type+constraint_name)
// matches BOTH rows on the record's next real edit, and the UPDATE
// itself collides with record_unique_keys_key_uq trying to set them both
// to the same value, permanently rejecting every future edit of that
// record with a bogus "already used by another record". Reconciling
// closes this at the source: InsertTx can now never coexist with more
// than one row per (record, constraint) it touches, regardless of
// caller or how many times it is called.
//
// Also idempotent when recordID already owns this exact (entityType,
// constraintName, keyValue) triple — that call's UpdateValueTx becomes a
// same-value no-op rather than a self-collision (the original driver for
// this method's rewrite): record_unique_keys_key_uq is UNIQUE on
// (entity_type, constraint_name, key_value) alone, no record_id
// carve-out, so the pre-fix plain INSERT collided with ITSELF on a
// repeat call. Both existing non-backfill callers (Engine.Create's
// brand-new record; Engine.Update's n==0 branch in updateConstraintKey)
// only ever reach InsertTx when no row for (recordID, entityType,
// constraintName) exists yet, so UpdateValueTx is a guaranteed no-op
// (0 rows) for them and this never changes their behavior — reconciling
// only matters for a caller that can legitimately re-attempt an insert
// against a record that may already be backed (possibly under a stale
// key_value), which today is cmd/sync-tenant-modules' backfill,
// including its forced -backfill-only retry (uc-infra#237 gap 2).
//
// The fallback INSERT (reached only once UpdateValueTx has confirmed
// recordID owns no row here) still uses ON CONFLICT ... DO UPDATE with a
// record_id-gated WHERE, not a plain INSERT: it costs nothing extra and
// self-heals a narrow concurrent-caller race (two InsertTx calls for the
// SAME record landing between this method's own UpdateValueTx and
// INSERT) instead of misreporting it as ErrUniqueKeyConflict against
// itself. A genuinely different record's conflict still surfaces via
// sql.ErrNoRows exactly as before — Postgres, not a Go-side SELECT
// first, remains the arbiter (ADR-0018 §3).
//
// Two isolation-dependent details independent review flagged, both
// latent today (every BeginTx call in this codebase passes nil opts, so
// every transaction reaching this method runs at Postgres' default READ
// COMMITTED) but worth naming rather than leaving implicit: (1) ON
// CONFLICT DO UPDATE's row-locking behavior means a losing caller (one
// that gets ErrUniqueKeyConflict from the fallback INSERT) now holds a
// lock on the OTHER record's conflicting row until its own transaction
// ends, not released merely because its own write didn't apply — a new
// caller that catches ErrUniqueKeyConflict and keeps using the same
// transaction, rather than rolling back, would hold that lock longer
// than the pre-reconciling version of this method did. (2) under
// REPEATABLE READ/SERIALIZABLE (not used anywhere today) ON CONFLICT DO
// UPDATE can raise 40001 instead of 23505, which classifyUniqueKeyErr
// does not classify as ErrUniqueKeyConflict — it would pass through as a
// raw error. Neither changes this method's behavior under the isolation
// level this codebase actually uses; both would need addressing before
// anything here moved off READ COMMITTED.
func (r *RecordUniqueKeyRepo) InsertTx(ctx context.Context, q querier, entityType, constraintName, keyValue, recordID string) error {
	n, err := r.UpdateValueTx(ctx, q, entityType, constraintName, keyValue, recordID)
	if err != nil {
		// ErrUniqueKeyConflict here means keyValue is already owned by a
		// DIFFERENT record — UpdateValueTx's own WHERE is scoped to
		// recordID, so this can only be a genuine cross-record collision,
		// never recordID's own row.
		return err
	}
	if n > 0 {
		return nil // reconciled recordID's existing row in place.
	}

	var ignored int
	err = q.QueryRowContext(ctx,
		`INSERT INTO record_unique_keys (entity_type, constraint_name, key_value, record_id)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT ON CONSTRAINT `+uniqueKeyConstraintName+`
		 DO UPDATE SET key_value = EXCLUDED.key_value
		 WHERE record_unique_keys.record_id = EXCLUDED.record_id
		 RETURNING 1`,
		entityType, constraintName, keyValue, recordID).Scan(&ignored)
	if errors.Is(err, sql.ErrNoRows) {
		// INSERT collided on (entity_type, constraint_name, key_value),
		// but the conflicting row's record_id is NOT recordID — a
		// genuine different-record conflict, same case the pre-idempotent
		// version of this method reported as ErrUniqueKeyConflict via a
		// raw INSERT failure.
		return ErrUniqueKeyConflict
	}
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
