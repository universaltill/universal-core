// Regression tests for uc-infra#116: requiredFieldWarnings and
// targetConstraintWarnings each treated a genuine Count*/error from
// their underlying repository call identically to "zero violations
// found" (`if err != nil || n == 0 { continue }`), so a transient DB
// error produced a false "all clear" instead of surfacing to the
// operator. These exercise the error branch directly against a real
// *sql.DB that has been Close()d — deterministic ("sql: database is
// closed"), no live Postgres connection needed, and it reaches the
// count call the exact same way a dropped connection or canceled query
// would in production.
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
