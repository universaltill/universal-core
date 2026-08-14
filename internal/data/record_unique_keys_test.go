package data

import (
	"context"
	"errors"
	"testing"
)

// TestRecordUniqueKeyRepo_InsertTx_IdempotentForSameRecord is the
// regression test for uc-infra#237 gap 3: record_unique_keys_key_uq is
// UNIQUE on (entity_type, constraint_name, key_value) alone, with no
// record_id carve-out, so re-inserting a key a record ALREADY owns used
// to collide with itself and be misreported as ErrUniqueKeyConflict —
// exactly the shape of false-negative a re-run of
// cmd/sync-tenant-modules' backfill hits when it re-attempts an entity
// type whose Definition version didn't move. This proves the fix: a
// second InsertTx for the same (entityType, constraintName, keyValue,
// recordID) is now a no-op success, and still only one row exists.
func TestRecordUniqueKeyRepo_InsertTx_IdempotentForSameRecord(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := NewRecordRepo(db)
	keys := NewRecordUniqueKeyRepo(db)

	rec, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-1"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}

	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec.ID); err != nil {
		t.Fatalf("first InsertTx: %v", err)
	}
	// The re-run: same triple, same record. Must succeed, not report
	// ErrUniqueKeyConflict against itself.
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec.ID); err != nil {
		t.Fatalf("second InsertTx for the same record's own key: expected nil (idempotent), got %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM record_unique_keys WHERE entity_type = $1 AND constraint_name = $2 AND key_value = $3`,
		"Attendance", "employee_id", "emp-1",
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after two idempotent inserts, got %d", count)
	}
}

// TestRecordUniqueKeyRepo_InsertTx_StillRejectsADifferentRecord confirms
// the idempotency fix above is scoped to "the SAME record re-claiming its
// own key" only — a genuinely different record contesting the same key
// must still be rejected as ErrUniqueKeyConflict, same as before.
func TestRecordUniqueKeyRepo_InsertTx_StillRejectsADifferentRecord(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := NewRecordRepo(db)
	keys := NewRecordUniqueKeyRepo(db)

	rec1, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-1"})
	if err != nil {
		t.Fatalf("create record 1: %v", err)
	}
	rec2, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-2"})
	if err != nil {
		t.Fatalf("create record 2: %v", err)
	}

	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec1.ID); err != nil {
		t.Fatalf("first InsertTx: %v", err)
	}
	err = keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec2.ID)
	if !errors.Is(err, ErrUniqueKeyConflict) {
		t.Fatalf("expected ErrUniqueKeyConflict for a different record contesting the same key, got %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM record_unique_keys WHERE entity_type = $1 AND constraint_name = $2 AND key_value = $3`,
		"Attendance", "employee_id", "emp-1",
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row (rec1's, untouched by rec2's rejected attempt), got %d", count)
	}

	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT record_id FROM record_unique_keys WHERE entity_type = $1 AND constraint_name = $2 AND key_value = $3`,
		"Attendance", "employee_id", "emp-1",
	).Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if owner != rec1.ID {
		t.Fatalf("expected rec1 (%s) to still own the key, got %s", rec1.ID, owner)
	}
}

// TestRecordUniqueKeyRepo_InsertTx_ReplayingTheSameValueIsAStrictNoOp
// covers the plain idempotent-replay case at the row level (the counting
// behavior is already covered by
// TestRecordUniqueKeyRepo_InsertTx_IdempotentForSameRecord): a second
// InsertTx with the SAME key_value the record already owns must leave
// exactly one row, unchanged.
func TestRecordUniqueKeyRepo_InsertTx_ReplayingTheSameValueIsAStrictNoOp(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := NewRecordRepo(db)
	keys := NewRecordUniqueKeyRepo(db)

	rec, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-1"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec.ID); err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1", rec.ID); err != nil {
		t.Fatalf("replay InsertTx: %v", err)
	}

	var count int
	var value string
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), max(key_value) FROM record_unique_keys WHERE record_id = $1 AND entity_type = $2 AND constraint_name = $3`,
		rec.ID, "Attendance", "employee_id",
	).Scan(&count, &value); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row, got %d", count)
	}
	if value != "emp-1" {
		t.Fatalf("expected key_value to stay %q, got %q", "emp-1", value)
	}
}

// TestRecordUniqueKeyRepo_InsertTx_ReconcilesAStaleRowInsteadOfDuplicatingIt
// is the regression test for the bug independent review of uc-infra#237
// found in this method's first draft: a record that already owns a row
// for (entityType, constraintName) under an OLD key_value — the shape
// left behind when a Unique set is removed from a Definition (so
// UpdateUniqueConstraintKeys stops reconciling that record's row going
// forward), the record is edited while the constraint is absent, and the
// set is later re-added — must have that row REPOINTED to the new
// key_value on the next InsertTx, never a second row inserted alongside
// the stale one. Two rows for the same (record, constraint) would make
// UpdateValueTx's own WHERE clause (record_id+entity_type+constraint_name)
// match both on the record's next real edit, and the UPDATE itself would
// then collide with record_unique_keys_key_uq trying to set both rows to
// the same value — permanently rejecting every future edit of that
// record with a bogus "already used by another record", the exact defect
// this test pins as fixed.
func TestRecordUniqueKeyRepo_InsertTx_ReconcilesAStaleRowInsteadOfDuplicatingIt(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := NewRecordRepo(db)
	keys := NewRecordUniqueKeyRepo(db)

	rec, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-1"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	// Establish a row under an OLD key_value — standing in for a row a
	// constraint's own removal left behind unreconciled.
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1-old", rec.ID); err != nil {
		t.Fatalf("InsertTx (old value): %v", err)
	}

	// A later backfill/re-insert against the record's CURRENT data
	// supplies a DIFFERENT key_value for the same (record, constraint).
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "emp-1-new", rec.ID); err != nil {
		t.Fatalf("InsertTx (new value): %v", err)
	}

	var count int
	var value string
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), max(key_value) FROM record_unique_keys WHERE record_id = $1 AND entity_type = $2 AND constraint_name = $3`,
		rec.ID, "Attendance", "employee_id",
	).Scan(&count, &value); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the stale row to be REPOINTED (still exactly 1 row), got %d rows", count)
	}
	if value != "emp-1-new" {
		t.Fatalf("expected the single remaining row to hold the NEW value %q, got %q", "emp-1-new", value)
	}

	// The real proof this closes the permanent-rejection defect: a
	// further, ordinary UpdateValueTx against this record (standing in
	// for the record's next real edit through crud.Engine.Update) must
	// still work — it would fail with a bogus ErrUniqueKeyConflict if two
	// rows had been left behind for InsertTx's own WHERE clause to match.
	if _, err := keys.UpdateValueTx(ctx, db, "Attendance", "employee_id", "emp-1-newer", rec.ID); err != nil {
		t.Fatalf("expected the record's next real update to still work cleanly, got %v", err)
	}
}

// TestRecordUniqueKeyRepo_InsertTx_ReconciliationStillRejectsAnotherRecordsValue
// confirms the reconciling rewrite didn't loosen enforcement: repointing
// recordID's stale row to a key_value ANOTHER live record already owns
// must still fail as ErrUniqueKeyConflict, leaving both rows exactly as
// they were.
func TestRecordUniqueKeyRepo_InsertTx_ReconciliationStillRejectsAnotherRecordsValue(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := NewRecordRepo(db)
	keys := NewRecordUniqueKeyRepo(db)

	rec1, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-1"})
	if err != nil {
		t.Fatalf("create record 1: %v", err)
	}
	rec2, err := records.Create(ctx, "Attendance", map[string]any{"employee_id": "emp-2"})
	if err != nil {
		t.Fatalf("create record 2: %v", err)
	}
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "taken", rec1.ID); err != nil {
		t.Fatalf("InsertTx rec1: %v", err)
	}
	if err := keys.InsertTx(ctx, db, "Attendance", "employee_id", "rec2-old", rec2.ID); err != nil {
		t.Fatalf("InsertTx rec2 (old value): %v", err)
	}

	// rec2 tries to reconcile its stale row onto the value rec1 owns.
	err = keys.InsertTx(ctx, db, "Attendance", "employee_id", "taken", rec2.ID)
	if !errors.Is(err, ErrUniqueKeyConflict) {
		t.Fatalf("expected ErrUniqueKeyConflict when reconciling onto a value another record owns, got %v", err)
	}

	var rec2Value string
	if err := db.QueryRowContext(ctx,
		`SELECT key_value FROM record_unique_keys WHERE record_id = $1`, rec2.ID,
	).Scan(&rec2Value); err != nil {
		t.Fatalf("read rec2's row: %v", err)
	}
	if rec2Value != "rec2-old" {
		t.Fatalf("expected the rejected reconciliation to leave rec2's row untouched at %q, got %q", "rec2-old", rec2Value)
	}
}
