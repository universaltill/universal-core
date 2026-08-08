package crud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// --- pure helper unit tests (no database required) ---

// sha256Hex mirrors uniqueKeyValue's own hashing exactly, so these tests
// assert against the real algorithm rather than a second hardcoded
// implementation that could quietly drift from it.
func sha256Hex(t *testing.T, ordered []any) string {
	t.Helper()
	b, err := json.Marshal(ordered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestUniqueConstraintError_Error(t *testing.T) {
	err := &UniqueConstraintError{EntityType: "AttendanceRecord", ConstraintName: "employee_id+entry_date", Fields: []string{"employee_id", "entry_date"}}
	got := err.Error()
	if !strings.Contains(got, "AttendanceRecord") || !strings.Contains(got, "employee_id+entry_date") {
		t.Fatalf("Error() = %q, expected it to name the entity type and constraint", got)
	}
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatal("expected errors.Is(err, ErrUniqueConstraintViolation) via Unwrap")
	}
}

func TestUniqueKeyValue(t *testing.T) {
	cases := []struct {
		name        string
		fields      []string
		data        map[string]any
		wantValue   string
		wantApplies bool
	}{
		{
			name:        "single string field",
			fields:      []string{"employee_number"},
			data:        map[string]any{"employee_number": "E-100"},
			wantValue:   sha256Hex(t, []any{"E-100"}),
			wantApplies: true,
		},
		{
			name:        "composite field set, canonical order",
			fields:      []string{"employee_id", "entry_date"},
			data:        map[string]any{"employee_id": "abc", "entry_date": "2026-08-01"},
			wantValue:   sha256Hex(t, []any{"abc", "2026-08-01"}),
			wantApplies: true,
		},
		{
			name:        "a missing field makes the constraint inapplicable",
			fields:      []string{"employee_id", "entry_date"},
			data:        map[string]any{"employee_id": "abc"},
			wantApplies: false,
		},
		{
			name:        "an explicit nil value makes the constraint inapplicable",
			fields:      []string{"employee_id", "entry_date"},
			data:        map[string]any{"employee_id": "abc", "entry_date": nil},
			wantApplies: false,
		},
		{
			// #86/ADR-0018 §4: Required treats "" as absent. A Unique field
			// must agree, not silently accept "" as a real, collidable
			// value while an omitted field is exempt (independent review,
			// uc-infra#81 follow-up).
			name:        "an empty string value makes the constraint inapplicable",
			fields:      []string{"employee_id", "entry_date"},
			data:        map[string]any{"employee_id": "abc", "entry_date": ""},
			wantApplies: false,
		},
		{
			// uc-infra#105 independent review: the first version of that
			// fix widened entity.ValidateRecord's Required check to treat
			// "   " the same as "", but left this function on its own
			// plain s == "" copy — reopening the exact #81 bug above for
			// whitespace instead of "". Both now call entity.IsBlankString.
			name:        "a whitespace-only string value makes the constraint inapplicable",
			fields:      []string{"employee_id", "entry_date"},
			data:        map[string]any{"employee_id": "abc", "entry_date": "  \t\n "},
			wantApplies: false,
		},
		{
			name:        "a numeric field value",
			fields:      []string{"quantity"},
			data:        map[string]any{"quantity": float64(42)},
			wantValue:   sha256Hex(t, []any{float64(42)}),
			wantApplies: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, applicable, err := uniqueKeyValue(tc.fields, tc.data)
			if err != nil {
				t.Fatalf("uniqueKeyValue: %v", err)
			}
			if applicable != tc.wantApplies {
				t.Fatalf("applicable = %v, want %v", applicable, tc.wantApplies)
			}
			if applicable && value != tc.wantValue {
				t.Fatalf("value = %q, want %q", value, tc.wantValue)
			}
			if applicable && !hexPattern.MatchString(value) {
				t.Fatalf("value %q is not a 64-char lowercase hex sha256 digest", value)
			}
		})
	}
}

// TestUniqueKeyValue_ArbitrarilyLongValueStaysFixedLength is the
// regression test for the bug independent review found: a raw
// (un-hashed) key_value has no length bound, and Postgres' btree index
// does — a long value made record_unique_keys' UNIQUE index reject the
// INSERT with "index row size exceeds btree maximum", which a naive
// substring-matching error classifier misreported as a duplicate-key
// violation for the FIRST such record ever created (see
// data.classifyUniqueKeyErr's own doc comment for the fix on that side).
// Hashing the value here is what removes the failure mode entirely,
// regardless of the classifier: this test pins that a 4000-character
// value still produces a 64-character digest, the same fixed size as a
// short one.
func TestUniqueKeyValue_ArbitrarilyLongValueStaysFixedLength(t *testing.T) {
	long := make([]byte, 4000)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	value, applicable, err := uniqueKeyValue([]string{"note"}, map[string]any{"note": string(long)})
	if err != nil {
		t.Fatalf("uniqueKeyValue: %v", err)
	}
	if !applicable {
		t.Fatal("expected a long non-empty value to be applicable")
	}
	if len(value) != 64 {
		t.Fatalf("expected a fixed 64-character hash regardless of input length, got %d chars: %q", len(value), value)
	}
}

func TestCanonicalSort(t *testing.T) {
	got := canonicalSort([]string{"entry_date", "employee_id"})
	want := []string{"employee_id", "entry_date"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("canonicalSort = %v, want %v", got, want)
	}
	// Must not mutate the input.
	original := []string{"entry_date", "employee_id"}
	_ = canonicalSort(original)
	if original[0] != "entry_date" {
		t.Fatalf("canonicalSort mutated its input: %v", original)
	}
}

// --- integration tests against a real Postgres ---

// attendanceLikeDef is a throwaway Definition shaped like
// hr.AttendanceRecord's driving case (employee_id, entry_date) — modeled
// generically here rather than importing internal/kernel/hr, so these
// tests exercise the GENERIC mechanism (same reasoning
// target_constraints_test.go's personDef/assignmentDef give), not one
// real module's Definition.
func attendanceLikeDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Attendance",
		Version:    1,
		Fields: []entity.Field{
			{Name: "employee_id", Type: entity.FieldString, Required: true},
			{Name: "entry_date", Type: entity.FieldDate, Required: true},
			{Name: "hours_worked", Type: entity.FieldNumber, Required: true},
		},
		Unique: [][]string{{"employee_id", "entry_date"}},
	}
}

// optionalKeyFieldDef is attendanceLikeDef's sibling with entry_date NOT
// Required — needed to actually exercise the engine's "!applicable"
// branches for real (attendanceLikeDef's own entry_date is Required, so
// entity.ValidateRecord rejects a submission missing it before
// crud.Engine's Unique enforcement ever runs; CountUniqueConstraintViolations'
// own tests sidestep this by bypassing Engine via RecordRepo directly,
// but the engine-level "!applicable" branches need a Definition where
// omitting the field is legitimately valid input).
func optionalKeyFieldDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "OptionalKeyField",
		Version:    1,
		Fields: []entity.Field{
			{Name: "employee_id", Type: entity.FieldString, Required: true},
			{Name: "entry_date", Type: entity.FieldDate},
			{Name: "hours_worked", Type: entity.FieldNumber},
		},
		Unique: [][]string{{"employee_id", "entry_date"}},
	}
}

// badgeDef exercises a single-field Unique set — the natural-key shape
// (employee_number, po_number, ...) rather than the composite case.
func badgeDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Badge",
		Version:    1,
		Fields: []entity.Field{
			{Name: "badge_number", Type: entity.FieldString, Required: true},
			{Name: "holder", Type: entity.FieldString},
		},
		Unique: [][]string{{"badge_number"}},
	}
}

func TestEngine_Create_UniqueConstraint_RejectsDuplicateCompositeKey(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("create first record: %v", err)
	}

	_, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(6),
	}, actor)
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected ErrUniqueConstraintViolation for a duplicate (employee_id, entry_date), got %v", err)
	}
	var uce *UniqueConstraintError
	if !errors.As(err, &uce) {
		t.Fatalf("expected *UniqueConstraintError, got %T: %v", err, err)
	}
	if uce.EntityType != "Attendance" || uce.ConstraintName != "employee_id+entry_date" {
		t.Fatalf("unexpected UniqueConstraintError: %+v", uce)
	}
}

func TestEngine_Create_UniqueConstraint_RollsBackOnConflict(t *testing.T) {
	// The rejected Create's own record and record_unique_keys rows must
	// not survive the transaction's rollback — otherwise a THIRD
	// duplicate submission would fail differently (or a records-only
	// leak would exist with no matching key row).
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	first, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create first record: %v", err)
	}

	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(6),
	}, actor); err == nil {
		t.Fatal("expected the duplicate create to fail")
	}

	records, err := engine.List(ctx, def)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 || records[0].ID != first.ID {
		t.Fatalf("expected only the first record to exist after the rejected duplicate's rollback, got %d records", len(records))
	}

	var keyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys`).Scan(&keyCount); err != nil {
		t.Fatalf("count record_unique_keys: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("expected exactly 1 record_unique_keys row after the rejected duplicate's rollback, got %d", keyCount)
	}
}

func TestEngine_Create_UniqueConstraint_AllowsDifferentEmployeeOrDate(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("create emp-1/08-01: %v", err)
	}
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-02", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("expected a different entry_date for the same employee to succeed: %v", err)
	}
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-2", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("expected a different employee_id on the same entry_date to succeed: %v", err)
	}
}

func TestEngine_Create_UniqueConstraint_SingleFieldNaturalKey(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := badgeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, map[string]any{"badge_number": "B-1", "holder": "Alex"}, actor); err != nil {
		t.Fatalf("create first badge: %v", err)
	}
	_, err := engine.Create(ctx, def, map[string]any{"badge_number": "B-1", "holder": "Sam"}, actor)
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected ErrUniqueConstraintViolation for a duplicate badge_number, got %v", err)
	}
}

func TestEngine_Update_UniqueConstraint_RejectsCollidingWithAnotherRecord(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("create emp-1/08-01: %v", err)
	}
	second, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-02", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create emp-1/08-02: %v", err)
	}

	// Updating the second record to collide with the first's key must
	// fail, the same as a Create would.
	_, err = engine.Update(ctx, def, second.ID, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(4),
	}, nil, actor)
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected ErrUniqueConstraintViolation updating into an existing key, got %v", err)
	}
}

func TestEngine_Update_UniqueConstraint_AllowsUnrelatedFieldChange(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Same key fields, only hours_worked changes — must succeed, and must
	// not spuriously collide with the record's OWN existing key row.
	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(7.5),
	}, nil, actor); err != nil {
		t.Fatalf("expected updating an unrelated field to succeed, got %v", err)
	}
}

func TestEngine_Update_UniqueConstraint_MovesKeyWhenFieldsChange(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-02", "hours_worked": float64(8),
	}, nil, actor); err != nil {
		t.Fatalf("expected changing entry_date to a free slot to succeed, got %v", err)
	}

	// The OLD key (emp-1, 08-01) must now be free for reuse — otherwise
	// updateUniqueConstraintKeys left a stale row behind.
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(3),
	}, actor); err != nil {
		t.Fatalf("expected the vacated key (emp-1, 08-01) to be reusable, got %v", err)
	}
}

func TestEngine_Update_UniqueConstraint_PreExistingRecordWithNoKeyRowGetsOneOnUpdate(t *testing.T) {
	// A record created before Unique existed on its Definition (or a
	// migration/import path that bypassed the engine) has no
	// record_unique_keys row yet — Update must backfill one (the
	// "RowsAffected == 0 → insert" branch), not silently skip it.
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM record_unique_keys WHERE record_id = $1`, rec.ID); err != nil {
		t.Fatalf("simulate a pre-existing record with no key row: %v", err)
	}

	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(9),
	}, nil, actor); err != nil {
		t.Fatalf("expected the backfilling update to succeed, got %v", err)
	}

	var keyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&keyCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("expected the update to backfill exactly 1 key row, got %d", keyCount)
	}

	// And the backfilled key is real: a genuine collision is still caught.
	_, err = engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(1),
	}, actor)
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected the backfilled key to be enforced, got %v", err)
	}
}

func TestEngine_Delete_UniqueConstraint_FreesTheKeyForReuse(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := engine.Delete(ctx, def, rec.ID, actor); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var keyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&keyCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if keyCount != 0 {
		t.Fatalf("expected 0 record_unique_keys rows after delete, got %d", keyCount)
	}

	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(2),
	}, actor); err != nil {
		t.Fatalf("expected the key freed by delete to be reusable, got %v", err)
	}
}

// TestEngine_Create_UniqueConstraint_ConcurrentInsertsOneWinsOneFails is
// the test ADR-0018 section 3 says a Go-side check alone cannot pass: two
// genuinely concurrent transactions both computing the same key, racing
// to commit. The record_unique_keys UNIQUE index — not application logic
// — must be what decides the outcome: exactly one Create succeeds, the
// other fails cleanly with *UniqueConstraintError (never a raw driver
// error, never both succeeding).
func TestEngine_Create_UniqueConstraint_ConcurrentInsertsOneWinsOneFails(t *testing.T) {
	db := freshTenantDB(t)
	// freshTenantDB sets no connection limit (database/sql's pool default
	// is unbounded), so the attempts below get real, separate backend
	// connections and their BeginTx/INSERT genuinely interleave — no
	// SetMaxOpenConns call needed here (independent review: an earlier
	// version of this test called SetMaxOpenConns(4) with a comment
	// claiming it "raises" concurrency, when it actually lowers the pool
	// below its already-sufficient default; removed rather than fixing
	// the comment, since the call added nothing real).
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := engine.Create(ctx, def, map[string]any{
				"employee_id": "emp-race", "entry_date": "2026-08-01", "hours_worked": float64(8),
			}, actor)
			results[i] = err
		}(i)
	}
	wg.Wait()

	successes, conflicts, other := 0, 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrUniqueConstraintViolation):
			conflicts++
		default:
			other++
			t.Logf("unexpected error type: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful concurrent create, got %d (conflicts=%d, other=%d)", successes, conflicts, other)
	}
	if other != 0 {
		t.Fatalf("expected every losing attempt to fail as *UniqueConstraintError, got %d unrecognized errors", other)
	}
	if conflicts != attempts-1 {
		t.Fatalf("expected %d conflicts, got %d", attempts-1, conflicts)
	}

	records, err := engine.List(ctx, def)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 surviving record after the race, got %d", len(records))
	}
}

// --- CountUniqueConstraintViolations ---

func TestCountUniqueConstraintViolations_NoExistingRecords(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)

	n, err := CountUniqueConstraintViolations(ctx, records, "Attendance", []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("CountUniqueConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 violations with no records, got %d", n)
	}
}

func TestCountUniqueConstraintViolations_CountsExistingCollisions(t *testing.T) {
	// This deliberately bypasses Engine.Create (records.CreateTx directly)
	// so the records collide WITHOUT the enforcement stage ever running —
	// simulating data that predates the constraint, or arrived via a path
	// that doesn't go through crud.Engine (a bulk import, a migration).
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for i := 0; i < 3; i++ {
		if _, err := records.CreateTx(ctx, tx, "Attendance", map[string]any{
			"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
		}); err != nil {
			t.Fatalf("create colliding record %d: %v", i, err)
		}
	}
	// A non-colliding record must not be counted.
	if _, err := records.CreateTx(ctx, tx, "Attendance", map[string]any{
		"employee_id": "emp-2", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}); err != nil {
		t.Fatalf("create unique record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := CountUniqueConstraintViolations(ctx, records, "Attendance", []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("CountUniqueConstraintViolations: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected all 3 colliding records to be counted, got %d", n)
	}
}

func TestCountUniqueConstraintViolations_SkipsRecordsMissingAKeyField(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for i := 0; i < 2; i++ {
		if _, err := records.CreateTx(ctx, tx, "Attendance", map[string]any{
			"employee_id": "emp-1", "hours_worked": float64(8),
			// entry_date deliberately absent
		}); err != nil {
			t.Fatalf("create record %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := CountUniqueConstraintViolations(ctx, records, "Attendance", []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("CountUniqueConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected records missing entry_date to be exempt (SQL-NULL-in-unique-index semantics), got %d", n)
	}
}

// TestEngine_Create_UniqueConstraint_MissingKeyFieldWritesNoKeyRow pins
// Create's "!applicable → continue" branch (independent review found it
// uncovered): a record missing one of a Unique set's fields must be
// exempt from that constraint AND must not write a key row for it —
// otherwise a later record that DOES have all fields could be rejected
// against a key that was never really meaningful.
func TestEngine_Create_UniqueConstraint_MissingKeyFieldWritesNoKeyRow(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := optionalKeyFieldDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "hours_worked": float64(8),
		// entry_date deliberately absent — Unique{employee_id,entry_date}
		// does not apply.
	}, actor)
	if err != nil {
		t.Fatalf("create with a missing key field: %v", err)
	}

	var keyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&keyCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if keyCount != 0 {
		t.Fatalf("expected no key row for a record missing a key field, got %d", keyCount)
	}
}

// TestEngine_Update_UniqueConstraint_ClearingAKeyFieldFreesTheKey pins
// Update's "!applicable → delete" branch (independent review found it
// uncovered): editing a record so a Unique field becomes absent must
// delete its key row, freeing that combination for another record — the
// mirror image of TestEngine_Update_UniqueConstraint_
// PreExistingRecordWithNoKeyRowGetsOneOnUpdate's insert branch.
func TestEngine_Update_UniqueConstraint_ClearingAKeyFieldFreesTheKey(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := optionalKeyFieldDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1 {
		t.Fatalf("expected 1 key row after create, got %d", before)
	}

	// Clear entry_date — Unique{employee_id,entry_date} no longer applies.
	if _, err := engine.Update(ctx, def, rec.ID, map[string]any{
		"employee_id": "emp-1", "hours_worked": float64(8),
	}, nil, actor); err != nil {
		t.Fatalf("update clearing entry_date: %v", err)
	}

	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Fatalf("expected the key row to be deleted once entry_date became absent, got %d", after)
	}

	// The freed key (emp-1, 2026-08-01) must now be usable by ANOTHER record.
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(3),
	}, actor); err != nil {
		t.Fatalf("expected the freed key to be reusable, got %v", err)
	}
}

// TestEngine_Update_UniqueConstraint_BackfillInsertConflicts pins the
// RowsAffected==0 branch's OWN conflict path (independent review found
// it uncovered): a record with no existing key row (predates the
// constraint) being updated into a combination ANOTHER record already
// holds must still be rejected via the insert-fallback branch, not just
// the plain UPDATE branch TestEngine_Update_UniqueConstraint_
// RejectsCollidingWithAnotherRecord already covers.
func TestEngine_Update_UniqueConstraint_BackfillInsertConflicts(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := attendanceLikeDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// Record A holds the key (emp-1, 2026-08-01) for real.
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	}, actor); err != nil {
		t.Fatalf("create record A: %v", err)
	}

	// Record B is created with a DIFFERENT, non-colliding key, then its
	// key row is deleted directly to simulate "predates the constraint"
	// (same technique TestEngine_Update_UniqueConstraint_
	// PreExistingRecordWithNoKeyRowGetsOneOnUpdate uses).
	recB, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-2", "entry_date": "2026-08-09", "hours_worked": float64(8),
	}, actor)
	if err != nil {
		t.Fatalf("create record B: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM record_unique_keys WHERE record_id = $1`, recB.ID); err != nil {
		t.Fatalf("simulate a pre-existing record with no key row: %v", err)
	}

	// Updating B into A's key must fail via the insert-fallback branch
	// (B has no existing row, so UPDATE affects 0 rows, falling into the
	// INSERT path, which must itself detect the conflict).
	_, err = engine.Update(ctx, def, recB.ID, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(1),
	}, nil, actor)
	if !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected the backfill-insert branch to reject the collision, got %v", err)
	}
}

// TestEngine_MultipleUniqueSets_EnforcedIndependently drives a
// Definition with TWO declared Unique sets through the engine end to end
// (independent review found nothing pinned this): each set is enforced
// on its own, a collision on one does not block a legitimate change on
// the other, and Delete frees both sets' key rows together.
func multiUniqueDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "MultiUnique",
		Version:    1,
		Fields: []entity.Field{
			{Name: "a", Type: entity.FieldString, Required: true},
			{Name: "b", Type: entity.FieldString, Required: true},
			{Name: "c", Type: entity.FieldString, Required: true},
		},
		Unique: [][]string{{"a"}, {"b", "c"}},
	}
}

func TestEngine_MultipleUniqueSets_EnforcedIndependently(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := multiUniqueDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	rec, err := engine.Create(ctx, def, map[string]any{"a": "1", "b": "x", "c": "y"}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var keyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&keyCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if keyCount != 2 {
		t.Fatalf("expected 2 key rows (one per declared Unique set), got %d", keyCount)
	}

	// Colliding on {a} alone (set 1) is rejected even though {b,c} (set 2)
	// would be fine.
	if _, err := engine.Create(ctx, def, map[string]any{"a": "1", "b": "different", "c": "different"}, actor); !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected a collision on set {a} alone to be rejected, got %v", err)
	}
	// Colliding on {b,c} alone (set 2) is rejected even though {a} would
	// be fine.
	if _, err := engine.Create(ctx, def, map[string]any{"a": "different", "b": "x", "c": "y"}, actor); !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected a collision on set {b,c} alone to be rejected, got %v", err)
	}
	// Colliding on neither succeeds.
	if _, err := engine.Create(ctx, def, map[string]any{"a": "2", "b": "different", "c": "different2"}, actor); err != nil {
		t.Fatalf("expected a non-colliding record to succeed, got %v", err)
	}

	if err := engine.Delete(ctx, def, rec.ID, actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var afterDelete int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, rec.ID).Scan(&afterDelete); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if afterDelete != 0 {
		t.Fatalf("expected both key rows freed by delete, got %d remaining", afterDelete)
	}
	// Both freed keys are now reusable.
	if _, err := engine.Create(ctx, def, map[string]any{"a": "1", "b": "x", "c": "y"}, actor); err != nil {
		t.Fatalf("expected both freed keys to be reusable, got %v", err)
	}
}

// TestCountUniqueConstraintViolations_MultiPage pins the pagination
// branch (offset += pageSize) independent review found uncovered:
// CountUniqueConstraintViolations must keep counting past the first
// page. pageSize is 200; 205 records (200 unique + a colliding pair
// spanning the page boundary) forces a second page read.
func TestCountUniqueConstraintViolations_MultiPage(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for i := 0; i < 203; i++ {
		if _, err := records.CreateTx(ctx, tx, "Attendance", map[string]any{
			"employee_id": fmt.Sprintf("emp-%d", i), "entry_date": "2026-08-01", "hours_worked": float64(8),
		}); err != nil {
			t.Fatalf("create record %d: %v", i, err)
		}
	}
	// Two more records, both on page 2 (offset 200+), colliding with each
	// other but not with any of the 203 above.
	for i := 0; i < 2; i++ {
		if _, err := records.CreateTx(ctx, tx, "Attendance", map[string]any{
			"employee_id": "emp-collide", "entry_date": "2026-08-02", "hours_worked": float64(8),
		}); err != nil {
			t.Fatalf("create colliding record %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := CountUniqueConstraintViolations(ctx, records, "Attendance", []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("CountUniqueConstraintViolations: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected the second page's 2 colliding records to be counted, got %d", n)
	}
}

// --- BackfillUniqueConstraintKeys ---

func TestBackfillUniqueConstraintKeys_NoExistingRecords(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	keys := data.NewRecordUniqueKeyRepo(db)

	backfilled, skipped, err := BackfillUniqueConstraintKeys(ctx, db, records, keys, "Attendance", []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("BackfillUniqueConstraintKeys: %v", err)
	}
	if backfilled != 0 || skipped != 0 {
		t.Fatalf("expected 0/0 with no records, got backfilled=%d skipped=%d", backfilled, skipped)
	}
}

// TestBackfillUniqueConstraintKeys_ProtectsExistingDataGoingForward is
// the regression test for the gap independent review found: without a
// backfill, a record that existed before Unique was declared has no key
// row, so a BRAND-NEW record can silently duplicate it even though the
// old record's own data was perfectly valid. This proves the fix: after
// backfilling, the old record's key exists and a fresh duplicate against
// it is rejected via the normal Engine.Create path.
func TestBackfillUniqueConstraintKeys_ProtectsExistingDataGoingForward(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	keys := data.NewRecordUniqueKeyRepo(db)
	def := attendanceLikeDef()

	// Simulate a pre-existing record from before Unique was declared:
	// written straight via RecordRepo, bypassing crud.Engine entirely, so
	// no key row is created.
	pre, err := records.Create(ctx, def.EntityType, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	})
	if err != nil {
		t.Fatalf("create pre-existing record: %v", err)
	}

	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, pre.ID).Scan(&before); err != nil {
		t.Fatalf("count before backfill: %v", err)
	}
	if before != 0 {
		t.Fatalf("expected no key row before backfill, got %d", before)
	}

	// WITHOUT backfill, a fresh duplicate would be silently accepted —
	// confirmed directly, not just asserted, so this test also documents
	// the exact gap the backfill closes.
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	dup, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(1),
	}, actor)
	if err != nil {
		t.Fatalf("expected the pre-backfill duplicate to be silently accepted (documenting the gap), got %v", err)
	}
	if err := engine.Delete(ctx, def, dup.ID, actor); err != nil {
		t.Fatalf("clean up the documented-gap duplicate: %v", err)
	}

	backfilled, skipped, err := BackfillUniqueConstraintKeys(ctx, db, records, keys, def.EntityType, []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("BackfillUniqueConstraintKeys: %v", err)
	}
	if backfilled != 1 || skipped != 0 {
		t.Fatalf("expected backfilled=1 skipped=0, got backfilled=%d skipped=%d", backfilled, skipped)
	}

	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, pre.ID).Scan(&after); err != nil {
		t.Fatalf("count after backfill: %v", err)
	}
	if after != 1 {
		t.Fatalf("expected 1 key row after backfill, got %d", after)
	}

	// NOW a fresh duplicate is rejected through the real engine.
	if _, err := engine.Create(ctx, def, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(2),
	}, actor); !errors.Is(err, ErrUniqueConstraintViolation) {
		t.Fatalf("expected the backfilled key to protect the pre-existing record, got %v", err)
	}
}

// TestBackfillUniqueConstraintKeys_OldestRecordWinsAmongCollisions covers
// the deterministic "oldest wins" tie-break: two pre-existing records
// already colliding with EACH OTHER — backfill must back exactly one
// (the older, since ListPage orders by created_at) and count the other
// as skipped, not fail the whole run.
func TestBackfillUniqueConstraintKeys_OldestRecordWinsAmongCollisions(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	records := data.NewRecordRepo(db)
	keys := data.NewRecordUniqueKeyRepo(db)
	def := attendanceLikeDef()

	older, err := records.Create(ctx, def.EntityType, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(8),
	})
	if err != nil {
		t.Fatalf("create older record: %v", err)
	}
	newer, err := records.Create(ctx, def.EntityType, map[string]any{
		"employee_id": "emp-1", "entry_date": "2026-08-01", "hours_worked": float64(4),
	})
	if err != nil {
		t.Fatalf("create newer record: %v", err)
	}

	backfilled, skipped, err := BackfillUniqueConstraintKeys(ctx, db, records, keys, def.EntityType, []string{"employee_id", "entry_date"})
	if err != nil {
		t.Fatalf("BackfillUniqueConstraintKeys: %v", err)
	}
	if backfilled != 1 || skipped != 1 {
		t.Fatalf("expected backfilled=1 skipped=1 for a colliding pair, got backfilled=%d skipped=%d", backfilled, skipped)
	}

	var olderCount, newerCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, older.ID).Scan(&olderCount); err != nil {
		t.Fatalf("count older: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_unique_keys WHERE record_id = $1`, newer.ID).Scan(&newerCount); err != nil {
		t.Fatalf("count newer: %v", err)
	}
	if olderCount != 1 || newerCount != 0 {
		t.Fatalf("expected the OLDER record to win the key (created_at order), got older=%d newer=%d", olderCount, newerCount)
	}
}
