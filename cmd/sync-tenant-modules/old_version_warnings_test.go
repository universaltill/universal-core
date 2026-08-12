// Regression tests for uc-infra#188: targetConstraintWarnings,
// numericBoundWarnings and newlyAddedUniqueSets (via its shared
// oldUniqueNameLookup closure) each looked up c's OLD published version
// (entityDefs.GetVersion, then json.Unmarshal) to decide whether a
// constraint/bound/unique-set is actually NEW since the last version, and
// each silently treated a GetVersion error or an Unmarshal failure the
// same as "no old version" (oldDef/oldNames left nil) — functionally safe
// (it fails open to "check everything", never to "check nothing", so no
// violation is ever missed) but with zero operator-visible trace, exactly
// the class of gap uc-infra#159 already closed one level up for the
// shared defFor closure (published_deffor_test.go). These exercise the
// same two failure branches (GetVersion error, Unmarshal error) at the
// three surviving call sites uc-infra#159 didn't touch.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestTargetConstraintWarnings_OldVersionGetVersionErrorIsSurfaced(t *testing.T) {
	def := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldReference, Target: "Party", MustMatchParentField: "party_id"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "PurchaseOrder" {
			return nil, false
		}
		return def, true
	}
	// from: 5, not 0 — this is what makes targetConstraintWarnings look
	// up the OLD version at all; entityDefs is backed by a closed DB, so
	// that lookup fails before the field loop even runs.
	changes := []change{{entityType: "PurchaseOrder", from: 5, to: 6}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	logged := captureLog(t, func() {
		targetConstraintWarnings(context.Background(), closedDB(t), entityDefs, "Demo Organization", changes, defFor)
	})

	for _, want := range []string{
		"WARNING", "Demo Organization", "PurchaseOrder v5",
		"could not re-read the previous published version",
		"reference constraint",
		"database is closed",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestTargetConstraintWarnings_OldVersionUnmarshalErrorIsSurfaced(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	// Stored as a draft — GetVersion is status-agnostic (it fetches by
	// exact version, not "the published one"), so this doesn't need
	// Approve/Publish to be reachable as an "old" version. "fields" as a
	// string instead of an array fails the strict, typed
	// json.Unmarshal into entity.Definition, same corruption shape
	// published_deffor_test.go's own Unmarshal-error test uses.
	bad := []byte(`{"entity_type":"PurchaseOrder","version":5,"fields":"not-an-array"}`)
	if _, err := entityDefs.CreateDraft(ctx, "PurchaseOrder", 5, bad, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	newDef := &entity.Definition{
		EntityType: "PurchaseOrder",
		Fields: []entity.Field{
			{Name: "vendor_id", Type: entity.FieldReference, Target: "Party", MustMatchParentField: "party_id"},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "PurchaseOrder" {
			return nil, false
		}
		return newDef, true
	}
	changes := []change{{entityType: "PurchaseOrder", from: 5, to: 6}}

	logged := captureLog(t, func() {
		out := targetConstraintWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)
		// The old version couldn't be read, so every field is treated as
		// newly-constrained — but the tenant has zero PurchaseOrder
		// records, so the actual violation count is 0 and this must NOT
		// fabricate a violation warning on top of the decode WARNING.
		if len(out) != 0 {
			t.Errorf("expected no violation warnings against an empty table, got %v", out)
		}
	})

	for _, want := range []string{
		"WARNING", name, "PurchaseOrder v5",
		"could not be decoded",
		"reference constraint",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestNumericBoundWarnings_OldVersionGetVersionErrorIsSurfaced(t *testing.T) {
	maxVal := 100.0
	def := &entity.Definition{
		EntityType: "Gauge",
		Fields: []entity.Field{
			{Name: "capacity", Type: entity.FieldNumber, Max: &maxVal},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Gauge" {
			return nil, false
		}
		return def, true
	}
	changes := []change{{entityType: "Gauge", from: 3, to: 4}}
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))

	logged := captureLog(t, func() {
		numericBoundWarnings(context.Background(), closedDB(t), entityDefs, "Demo Organization", changes, defFor)
	})

	for _, want := range []string{
		"WARNING", "Demo Organization", "Gauge v3",
		"could not re-read the previous published version",
		"numeric bound",
		"database is closed",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestNumericBoundWarnings_OldVersionUnmarshalErrorIsSurfaced(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	bad := []byte(`{"entity_type":"Gauge","version":3,"fields":"not-an-array"}`)
	if _, err := entityDefs.CreateDraft(ctx, "Gauge", 3, bad, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	maxVal := 100.0
	newDef := &entity.Definition{
		EntityType: "Gauge",
		Fields: []entity.Field{
			{Name: "capacity", Type: entity.FieldNumber, Max: &maxVal},
		},
	}
	defFor := func(entityType string) (*entity.Definition, bool) {
		if entityType != "Gauge" {
			return nil, false
		}
		return newDef, true
	}
	changes := []change{{entityType: "Gauge", from: 3, to: 4}}

	logged := captureLog(t, func() {
		out := numericBoundWarnings(ctx, tenantDB, entityDefs, name, changes, defFor)
		if len(out) != 0 {
			t.Errorf("expected no violation warnings against an empty table, got %v", out)
		}
	})

	for _, want := range []string{
		"WARNING", name, "Gauge v3",
		"could not be decoded",
		"numeric bound",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestOldUniqueNameLookup_NewToRegistryStaysSilent(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	lookup := oldUniqueNameLookup(context.Background(), entityDefs, "Demo Organization")

	logged := captureLog(t, func() {
		if names := lookup(change{entityType: "Voucher", from: 0, to: 1}); names != nil {
			t.Errorf("expected nil for a brand-new-to-the-registry change, got %v", names)
		}
	})
	if logged != "" {
		t.Errorf("from == 0 is not an error and must not warn, got log output: %q", logged)
	}
}

func TestOldUniqueNameLookup_GetVersionErrorIsSurfaced(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	lookup := oldUniqueNameLookup(context.Background(), entityDefs, "Demo Organization")

	var names map[string]bool
	logged := captureLog(t, func() {
		names = lookup(change{entityType: "Voucher", from: 5, to: 6})
	})

	if names != nil {
		t.Errorf("expected a nil result on a GetVersion error, got %v", names)
	}
	for _, want := range []string{
		"WARNING", "Demo Organization", "Voucher v5",
		"could not re-read the previous published version",
		"unique constraint",
		"database is closed",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

func TestOldUniqueNameLookup_UnmarshalErrorIsSurfaced(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	bad := []byte(`{"entity_type":"Voucher","version":5,"fields":"not-an-array"}`)
	if _, err := entityDefs.CreateDraft(ctx, "Voucher", 5, bad, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	lookup := oldUniqueNameLookup(ctx, entityDefs, name)
	var names map[string]bool
	logged := captureLog(t, func() {
		names = lookup(change{entityType: "Voucher", from: 5, to: 6})
	})

	if names != nil {
		t.Errorf("expected a nil result on an unmarshal error, got %v", names)
	}
	for _, want := range []string{
		"WARNING", name, "Voucher v5",
		"could not be decoded",
		"unique constraint",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected the WARNING to mention %q, got: %q", want, logged)
		}
	}
}

// TestOldUniqueNameLookup_RepeatedFailureForSameEntityTypeWarnsOnce
// guards the reason oldUniqueNameLookup is a shared closure at all
// (rather than a plain function called independently by each of
// newlyAddedUniqueSets' two callers): on a real sync,
// uniqueConstraintWarnings and backfillNewUniqueConstraints both call
// newlyAddedUniqueSets with the exact same `changes`
// (cmd/sync-tenant-modules/main.go's syncTenant), so without dedup the
// identical failure would log twice per syncTenant call for the same
// entity type — the same "buries the signal" problem
// publishedDefFor's own dedup (published_deffor_test.go) exists to avoid.
func TestOldUniqueNameLookup_RepeatedFailureForSameEntityTypeWarnsOnce(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	lookup := oldUniqueNameLookup(context.Background(), entityDefs, "Demo Organization")
	c := change{entityType: "Voucher", from: 5, to: 6}

	logged := captureLog(t, func() {
		// Two calls, matching the real two-caller shape this closure is
		// actually shared across in syncTenant's real-run path.
		lookup(c)
		lookup(c)
	})

	if n := strings.Count(logged, "WARNING"); n != 1 {
		t.Errorf("expected exactly one WARNING for two failing calls against the same entity type, got %d in: %q", n, logged)
	}
}

// TestOldUniqueNameLookup_DifferentEntityTypesEachWarnOnce guards the
// other half of the dedup: a second, DIFFERENT entity type failing must
// still be reported, not silently absorbed by the first type's dedup
// entry — same shape as publishedDefFor's own equivalent test.
func TestOldUniqueNameLookup_DifferentEntityTypesEachWarnOnce(t *testing.T) {
	entityDefs := data.NewEntityDefinitionRepo(closedDB(t))
	lookup := oldUniqueNameLookup(context.Background(), entityDefs, "Demo Organization")

	logged := captureLog(t, func() {
		lookup(change{entityType: "Voucher", from: 5, to: 6})
		lookup(change{entityType: "Voucher", from: 5, to: 6}) // repeat: must not add a second WARNING
		lookup(change{entityType: "Invoice", from: 2, to: 3}) // different type: must warn independently
	})

	if n := strings.Count(logged, "WARNING"); n != 2 {
		t.Errorf("expected one WARNING per distinct entity type (2 total), got %d in: %q", n, logged)
	}
	for _, want := range []string{"Voucher", "Invoice"} {
		if !strings.Contains(logged, want) {
			t.Errorf("expected a WARNING naming %q, got: %q", want, logged)
		}
	}
}

func TestOldUniqueNameLookup_SuccessResolvesNamesAndStaysSilent(t *testing.T) {
	_, control, router := controlPlane(t)
	const name = "Demo Organization"
	_, tenantDB := newTenant(t, control, router, name)
	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	ctx := context.Background()

	good := []byte(`{"entity_type":"Voucher","version":5,"unique":[["code"]]}`)
	if _, err := entityDefs.CreateDraft(ctx, "Voucher", 5, good, setupActor); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	lookup := oldUniqueNameLookup(ctx, entityDefs, name)
	var names map[string]bool
	logged := captureLog(t, func() {
		names = lookup(change{entityType: "Voucher", from: 5, to: 6})
	})

	if logged != "" {
		t.Errorf("a successful lookup must stay silent, got log output: %q", logged)
	}
	want := entity.UniqueConstraintName([]string{"code"})
	if !names[want] {
		t.Errorf("expected the old version's declared unique set %q to resolve, got %v", want, names)
	}
}

// TestNewlyAddedUniqueSets_LookupFailureFailsOpen confirms the promise
// newlyAddedUniqueSets' own doc comment makes: when oldNamesFor can't
// resolve the old version (nil result, already warned by the lookup
// itself), every declared set on the new Definition is still treated as
// newly-added — a failed lookup must never cause a real new-or-changed
// unique constraint to go unchecked.
func TestNewlyAddedUniqueSets_LookupFailureFailsOpen(t *testing.T) {
	newDef := &entity.Definition{
		EntityType: "Voucher",
		Unique: [][]string{
			{"code"},
			{"batch_id", "sequence"},
		},
	}
	c := change{entityType: "Voucher", from: 5, to: 6}
	failingLookup := func(change) map[string]bool { return nil }

	sets := newlyAddedUniqueSets(c, newDef, failingLookup)

	if len(sets) != 2 {
		t.Fatalf("expected both declared sets to be treated as new when the old version can't be resolved, got %v", sets)
	}
}

// TestNewlyAddedUniqueSets_AlreadyDeclaredSetIsSkipped is
// TestNewlyAddedUniqueSets_LookupFailureFailsOpen's other half, and the
// one that actually exercises the point of a SUCCESSFUL oldNamesFor
// result: a set already present on the old version must be excluded, not
// re-reported (that's the whole reason newlyAddedUniqueSets exists,
// rather than treating every declared set as new every run — see its
// own doc comment and uniqueConstraintWarnings'/oldUniqueNameLookup's).
// Independent review of this fix found this branch (main.go's
// `if oldNames != nil && oldNames[...] { continue }`) reachable by no
// test in the repo — deletable outright with the rest of the suite
// still green — because every OTHER test here uses either from: 0 or a
// failing lookup. This is the missing case: a real, successful lookup
// that actually returns a name.
func TestNewlyAddedUniqueSets_AlreadyDeclaredSetIsSkipped(t *testing.T) {
	newDef := &entity.Definition{
		EntityType: "Voucher",
		Unique: [][]string{
			{"code"},                 // already declared on the old version
			{"batch_id", "sequence"}, // genuinely new
		},
	}
	c := change{entityType: "Voucher", from: 5, to: 6}
	oldNames := map[string]bool{
		entity.UniqueConstraintName([]string{"code"}): true,
	}
	successfulLookup := func(change) map[string]bool { return oldNames }

	sets := newlyAddedUniqueSets(c, newDef, successfulLookup)

	if len(sets) != 1 {
		t.Fatalf("expected exactly the one NOT-already-declared set, got %v", sets)
	}
	want := entity.UniqueConstraintName([]string{"batch_id", "sequence"})
	if got := entity.UniqueConstraintName(sets[0]); got != want {
		t.Errorf("expected the newly-added set %q, got %q (%v)", want, got, sets[0])
	}
}
