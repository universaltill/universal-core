// Regression tests for uc-infra#116 and its follow-up uc-infra#158: all
// four of requiredFieldWarnings, targetConstraintWarnings,
// numericBoundWarnings and uniqueConstraintWarnings each treated a
// genuine Count*/error from their underlying repository call identically
// to "zero violations found" (`if err != nil || n == 0 { continue }`),
// so a transient DB error produced a false "all clear" instead of
// surfacing to the operator. #116 fixed the first two; #158 closed the
// same gap at the other two's own Count* call sites. These exercise the
// error branch directly against a real *sql.DB that has been Close()d —
// deterministic ("sql: database is closed"), no live Postgres connection
// needed, and it reaches the count call the exact same way a dropped
// connection or canceled query would in production.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// closedDB returns an open-then-immediately-closed *sql.DB: any query
// against it fails with "sql: database is closed" before ever touching
// the network, so this needs no real Postgres connection to force the
// error branch deterministically.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://ignored-never-dialed/ignored")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	return db
}

// captureLog redirects the standard logger's output for the duration of
// fn and returns what it wrote, restoring the previous output
// afterward — the two warnings functions log directly via the package
// "log" global (matching every other operator-facing WARNING in this
// file), so this is the only way to observe what they emitted.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

func TestRequiredFieldWarnings_CountErrorIsSurfacedNotSwallowed(t *testing.T) {
	// Two Required fields, not one: proves the error branch still
	// `continue`s to the next field (rather than an accidental `return`/
	// `break` regression that would also pass a single-field fixture) —
	// a fleet run's whole point is checking every field, not stopping at
	// the first transient error.
	def := &entity.Definition{
		EntityType: "Widget",
		Fields: []entity.Field{
			{Name: "facility_id", Type: entity.FieldString, Required: true},
			{Name: "sku", Type: entity.FieldString, Required: true},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Widget" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Widget", from: 0, to: 1}}

	var out []string
	logged := captureLog(t, func() {
		out = requiredFieldWarnings(context.Background(), closedDB(t), "Demo Organization", changes, defFor)
	})

	if len(out) != 0 {
		t.Errorf("requiredFieldWarnings on a count error must not report a fabricated violation count, got %v", out)
	}
	// A distinctive fragment of the new wording, not just "Widget"/the
	// field name alone — CountMissingField's own wrapped error text
	// ("count Widget records missing facility_id: ...") already contains
	// those in isolation, so asserting only on them would pass even if
	// the new WARNING line dropped this function's own message entirely.
	for _, field := range []string{"facility_id", "sku"} {
		want := `could not check existing records against the now-required "` + field + `"`
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING for %q to contain %q, got: %q", field, want, logged)
		}
	}
	for _, want := range []string{"WARNING", "Demo Organization"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the operator-facing WARNING to mention %q, got: %q", want, logged)
		}
	}
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("expected the WARNING to carry the underlying error, got: %q", logged)
	}
}

func TestRequiredFieldWarnings_ZeroCountStaysSilent(t *testing.T) {
	// Guards the other half of the branch split: n == 0 with no error
	// must still produce no output at all, not even a WARNING — needs a
	// real, empty tenant database (a closed one would hit the error
	// branch instead), same fixtures this package's other tests use.
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)

	def := &entity.Definition{
		EntityType: "Widget",
		Fields: []entity.Field{
			{Name: "facility_id", Type: entity.FieldString, Required: true},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Widget" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Widget", from: 0, to: 1}}

	logged := captureLog(t, func() {
		out := requiredFieldWarnings(context.Background(), tenantDB, name, changes, defFor)
		if len(out) != 0 {
			t.Errorf("expected no warnings against an empty table, got %v", out)
		}
	})
	if logged != "" {
		t.Errorf("zero violations (no error) must stay silent, got log output: %q", logged)
	}
}

func TestTargetConstraintWarnings_CountErrorIsSurfacedNotSwallowed(t *testing.T) {
	// Two constrained FieldReference fields, not one — same "the loop
	// must keep going past a transient error" reasoning as
	// TestRequiredFieldWarnings_CountErrorIsSurfacedNotSwallowed above.
	def := &entity.Definition{
		EntityType: "Assignment",
		Fields: []entity.Field{
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Party", MustMatchParentField: "party_id"},
			{Name: "reviewer_id", Type: entity.FieldReference, Target: "Party", MustMatchParentField: "party_id"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Assignment" {
			return nil, false
		}
		return def, true
	}
	// from: 0 (new-to-registry) so targetConstraintWarnings treats every
	// constrained field as newly-added without needing entityDefs to
	// resolve an old version — entityDefs below is only ever passed
	// through to GetVersion, never called for c.from == 0, so a closed
	// DB behind it is fine: this test isn't exercising that lookup.
	changes := []change{{entityType: "Assignment", from: 0, to: 1}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	var out []string
	logged := captureLog(t, func() {
		out = targetConstraintWarnings(context.Background(), closedDB(t), entityDefs, "Demo Organization", changes, defFor)
	})

	if len(out) != 0 {
		t.Errorf("targetConstraintWarnings on a count error must not report a fabricated violation count, got %v", out)
	}
	// A distinctive fragment of the new wording — CountTargetConstraintViolations'
	// own wrapped error text ("count target constraint violations for
	// Assignment.assignee_id: ...") already contains the entity/field
	// names in isolation, so asserting only on those would pass even if
	// this function's own WARNING line were dropped entirely.
	for _, field := range []string{"assignee_id", "reviewer_id"} {
		want := `could not check existing records against the new reference constraint on "` + field + `"`
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING for %q to contain %q, got: %q", field, want, logged)
		}
	}
	for _, want := range []string{"WARNING", "Demo Organization"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the operator-facing WARNING to mention %q, got: %q", want, logged)
		}
	}
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("expected the WARNING to carry the underlying error, got: %q", logged)
	}
}

// Regression tests for uc-infra#158, the same swallowed-error bug shape
// #116 fixed above (`if err != nil || n == 0 { continue }`), independently
// present at numericBoundWarnings' and uniqueConstraintWarnings' own
// Count* call sites — out of #116's scope, tracked separately, fixed here.

func TestNumericBoundWarnings_CountErrorIsSurfacedNotSwallowed(t *testing.T) {
	// Two bounded FieldNumber fields, not one — same "the loop must keep
	// going past a transient error" reasoning as the tests above.
	minVal := 0.0
	maxVal := 100.0
	def := &entity.Definition{
		EntityType: "Gauge",
		Fields: []entity.Field{
			{Name: "reading", Type: entity.FieldNumber, Min: &minVal},
			{Name: "capacity", Type: entity.FieldNumber, Max: &maxVal},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Gauge" {
			return nil, false
		}
		return def, true
	}
	// from: 0 (new-to-registry) so numericBoundWarnings treats every
	// bounded field as newly-added without needing entityDefs to resolve
	// an old version — entityDefs below is only ever passed through to
	// GetVersion, never called for c.from == 0, so a closed DB behind it
	// is fine: this test isn't exercising that lookup.
	changes := []change{{entityType: "Gauge", from: 0, to: 1}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	var out []string
	logged := captureLog(t, func() {
		out = numericBoundWarnings(context.Background(), closedDB(t), entityDefs, "Demo Organization", changes, defFor)
	})

	if len(out) != 0 {
		t.Errorf("numericBoundWarnings on a count error must not report a fabricated violation count, got %v", out)
	}
	// A distinctive fragment of the new wording, not just "Gauge"/the
	// field name alone — CountOutOfRangeField's own wrapped error text
	// ("count Gauge records out of range on reading: ...") already
	// contains those in isolation, so asserting only on them would pass
	// even if the new WARNING line dropped this function's own message
	// entirely.
	for _, field := range []string{"reading", "capacity"} {
		want := `could not check existing records against the new bound on "` + field + `"`
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING for %q to contain %q, got: %q", field, want, logged)
		}
	}
	for _, want := range []string{"WARNING", "Demo Organization"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the operator-facing WARNING to mention %q, got: %q", want, logged)
		}
	}
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("expected the WARNING to carry the underlying error, got: %q", logged)
	}
}

func TestNumericBoundWarnings_ZeroCountStaysSilent(t *testing.T) {
	// Guards the other half of the branch split, the same way
	// TestRequiredFieldWarnings_ZeroCountStaysSilent does above: n == 0
	// with no error must still produce no output at all, not even a
	// WARNING — needs a real, empty tenant database (a closed one would
	// hit the error branch instead). Independent review of this change
	// confirmed a mutation that emits a spurious WARNING on a zero count
	// (while leaving the returned []string correct) passed the rest of
	// this package's suite undetected before this test existed.
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)

	minVal := 0.0
	def := &entity.Definition{
		EntityType: "Gauge",
		Fields: []entity.Field{
			{Name: "reading", Type: entity.FieldNumber, Min: &minVal},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Gauge" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Gauge", from: 0, to: 1}}

	logged := captureLog(t, func() {
		out := numericBoundWarnings(context.Background(), tenantDB, entityDefs, name, changes, defFor)
		if len(out) != 0 {
			t.Errorf("expected no warnings against an empty table, got %v", out)
		}
	})
	if logged != "" {
		t.Errorf("zero violations (no error) must stay silent, got log output: %q", logged)
	}
}

func TestUniqueConstraintWarnings_CountErrorIsSurfacedNotSwallowed(t *testing.T) {
	// Two declared Unique sets, not one — same "the loop must keep going
	// past a transient error" reasoning as the tests above.
	def := &entity.Definition{
		EntityType: "Voucher",
		Unique: [][]string{
			{"code"},
			{"batch_id", "sequence"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Voucher" {
			return nil, false
		}
		return def, true
	}
	// from: 0 (new-to-registry) so newlyAddedUniqueSets treats every
	// declared set as newly-added without needing entityDefs to resolve
	// an old version — entityDefs below is only ever passed through to
	// GetVersion, never called for c.from == 0, so a closed DB behind it
	// is fine: this test isn't exercising that lookup.
	changes := []change{{entityType: "Voucher", from: 0, to: 1}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	ctx := context.Background()
	var out []string
	logged := captureLog(t, func() {
		out = uniqueConstraintWarnings(ctx, closedDB(t), "Demo Organization", changes, defFor, oldUniqueNameLookup(ctx, entityDefs, "Demo Organization"))
	})

	if len(out) != 0 {
		t.Errorf("uniqueConstraintWarnings on a count error must not report a fabricated violation count, got %v", out)
	}
	// A distinctive fragment of the new wording, not just the constraint
	// name alone — CountUniqueConstraintViolations' own wrapped error text
	// ("count unique constraint violations for Voucher: ...") already
	// contains the entity type in isolation, so asserting only on the
	// constraint name would pass even if the new WARNING line dropped
	// this function's own message entirely.
	for _, name := range []string{
		entity.UniqueConstraintName([]string{"code"}),
		entity.UniqueConstraintName([]string{"batch_id", "sequence"}),
	} {
		want := `could not check existing records against the new unique constraint "` + name + `"`
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING for %q to contain %q, got: %q", name, want, logged)
		}
	}
	for _, want := range []string{"WARNING", "Demo Organization"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the operator-facing WARNING to mention %q, got: %q", want, logged)
		}
	}
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("expected the WARNING to carry the underlying error, got: %q", logged)
	}
}

func TestUniqueConstraintWarnings_ZeroCountStaysSilent(t *testing.T) {
	// Guards the other half of the branch split, the same way
	// TestRequiredFieldWarnings_ZeroCountStaysSilent and
	// TestNumericBoundWarnings_ZeroCountStaysSilent do above: n == 0 with
	// no error must still produce no output at all, not even a WARNING —
	// needs a real, empty tenant database (a closed one would hit the
	// error branch instead).
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)

	def := &entity.Definition{
		EntityType: "Voucher",
		Unique: [][]string{
			{"code"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Voucher" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Voucher", from: 0, to: 1}}
	ctx := context.Background()

	logged := captureLog(t, func() {
		out := uniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldUniqueNameLookup(ctx, entityDefs, name))
		if len(out) != 0 {
			t.Errorf("expected no warnings against an empty table, got %v", out)
		}
	})
	if logged != "" {
		t.Errorf("zero violations (no error) must stay silent, got log output: %q", logged)
	}
}

func TestConditionalUniqueConstraintWarnings_CountErrorIsSurfacedNotSwallowed(t *testing.T) {
	// Two declared UniqueWhen entries, not one — same "the loop must keep
	// going past a transient error" reasoning as
	// TestUniqueConstraintWarnings_CountErrorIsSurfacedNotSwallowed.
	def := &entity.Definition{
		EntityType: "Voucher",
		UniqueWhen: []entity.ConditionalUnique{
			{Fields: []string{"role_type"}, WhenField: "role_type", WhenValue: "own_organization"},
			{Fields: []string{"is_base"}, WhenField: "is_base", WhenValue: "true"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Voucher" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Voucher", from: 0, to: 1}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	ctx := context.Background()
	var out []string
	logged := captureLog(t, func() {
		out = conditionalUniqueConstraintWarnings(ctx, closedDB(t), "Demo Organization", changes, defFor, oldConditionalUniqueNameLookup(ctx, entityDefs, "Demo Organization"))
	})

	if len(out) != 0 {
		t.Errorf("conditionalUniqueConstraintWarnings on a count error must not report a fabricated violation count, got %v", out)
	}
	for _, cu := range def.UniqueWhen {
		name := entity.ConditionalUniqueConstraintName(cu)
		want := `could not check existing records against the new conditional unique constraint "` + name + `"`
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING for %q to contain %q, got: %q", name, want, logged)
		}
	}
	for _, want := range []string{"WARNING", "Demo Organization"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the operator-facing WARNING to mention %q, got: %q", want, logged)
		}
	}
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("expected the WARNING to carry the underlying error, got: %q", logged)
	}
}

func TestConditionalUniqueConstraintWarnings_ZeroCountStaysSilent(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)

	def := &entity.Definition{
		EntityType: "Voucher",
		UniqueWhen: []entity.ConditionalUnique{
			{Fields: []string{"role_type"}, WhenField: "role_type", WhenValue: "own_organization"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Voucher" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Voucher", from: 0, to: 1}}
	ctx := context.Background()

	logged := captureLog(t, func() {
		out := conditionalUniqueConstraintWarnings(ctx, tenantDB, name, changes, defFor, oldConditionalUniqueNameLookup(ctx, entityDefs, name))
		if len(out) != 0 {
			t.Errorf("expected no warnings against an empty table, got %v", out)
		}
	})
	if logged != "" {
		t.Errorf("zero violations (no error) must stay silent, got log output: %q", logged)
	}
}

// TestBackfillNewUniqueConstraints_ErrorDoesNotOverclaimARetryPath is the
// regression test for uc-infra#228: backfillNewUniqueConstraints' error
// WARNING said "existing records are unprotected until this is retried",
// but re-running this command only re-attempts a backfill for an entity
// type whose Definition version actually moved again (newlyAddedUniqueSets,
// via diffVersions) — there is no operator-facing path to force a retry
// against an unchanged version, so the wording promised a mechanism that
// doesn't exist. backfillNewConditionalUniqueConstraints already avoids
// this overclaim (uc-infra#201's own review pass caught it there); this
// brings the pre-existing Unique path's wording in line. Uses the same
// closedDB+captureLog fault-injection this file's other tests use: a
// closed *sql.DB makes records.ListPage fail deterministically, no live
// Postgres needed to reach the error branch.
func TestBackfillNewUniqueConstraints_ErrorDoesNotOverclaimARetryPath(t *testing.T) {
	def := &entity.Definition{
		EntityType: "Voucher",
		Unique: [][]string{
			{"code"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Voucher" {
			return nil, false
		}
		return def, true
	}
	// from: 0 (new-to-registry) so newlyAddedUniqueSets treats "code" as
	// newly-added without needing oldNamesFor to resolve anything real.
	changes := []change{{entityType: "Voucher", from: 0, to: 1}}
	oldNamesFor := func(change) map[string]bool { return nil }

	logged := captureLog(t, func() {
		backfillNewUniqueConstraints(context.Background(), closedDB(t), "Demo Organization", changes, defFor, oldNamesFor)
	})

	if strings.Contains(logged, "until this is retried") {
		t.Errorf("WARNING must not promise a retry path that doesn't exist, got: %q", logged)
	}
	want := `could not backfill unique constraint "code", existing records are unprotected: `
	if !strings.Contains(logged, want) {
		t.Errorf("expected %q, got: %q", want, logged)
	}
	for _, want := range []string{"WARNING", "Demo Organization", "database is closed"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}
