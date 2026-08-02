package crud

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// categoryDef is a throwaway self-referencing entity for these tests —
// the same shape Account.parent_account_id/Department.parent_department_id/
// Position.reports_to_position_id all take (a FieldReference whose Target
// is the entity's own EntityType), used here instead of any one real
// entity so these tests exercise the generic guard, not one entity's
// Definition.
func categoryDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Category",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "parent_category_id", Type: entity.FieldReference, Target: "Category"},
		},
	}
}

func createCategory(t *testing.T, ctx context.Context, engine *Engine, def *entity.Definition, name, parentID string) string {
	t.Helper()
	fields := map[string]any{"name": name}
	if parentID != "" {
		fields["parent_category_id"] = parentID
	}
	rec, err := engine.Create(ctx, def, fields, audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if err != nil {
		t.Fatalf("create category %s: %v", name, err)
	}
	return rec.ID
}

func TestEngine_Update_SelfReferenceDirectCycleRejected(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	a := createCategory(t, ctx, engine, def, "A", "")

	_, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": a}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if !errors.Is(err, ErrReferenceCycle) {
		t.Fatalf("expected ErrReferenceCycle for a record pointing at itself, got %v", err)
	}

	got, getErr := engine.Get(ctx, def, a)
	if getErr != nil {
		t.Fatalf("get after rejected update: %v", getErr)
	}
	if _, set := got.Data["parent_category_id"]; set {
		t.Fatalf("expected the rejected update to leave parent_category_id unset, got %v", got.Data["parent_category_id"])
	}
}

func TestEngine_Update_SelfReferenceIndirectCycleRejected(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	// Chain: A <- B <- C  (B.parent = A, C.parent = B)
	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", a)
	c := createCategory(t, ctx, engine, def, "C", b)

	// Re-pointing A to C would close the loop A -> C -> B -> A.
	_, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": c}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"})
	if !errors.Is(err, ErrReferenceCycle) {
		t.Fatalf("expected ErrReferenceCycle for an indirect cycle, got %v", err)
	}
}

func TestEngine_Update_SelfReferenceNonCyclicChainAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", "")
	c := createCategory(t, ctx, engine, def, "C", a)

	// Re-parenting C from A to B is a perfectly valid, non-cyclic edit.
	if _, err := engine.Update(ctx, def, c, map[string]any{"name": "C", "parent_category_id": b}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"}); err != nil {
		t.Fatalf("expected a non-cyclic re-parent to succeed, got %v", err)
	}

	got, err := engine.Get(ctx, def, c)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Data["parent_category_id"] != b {
		t.Fatalf("expected parent_category_id %s, got %v", b, got.Data["parent_category_id"])
	}
}

func TestEngine_Update_SelfReferenceDanglingParentAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	a := createCategory(t, ctx, engine, def, "A", "")

	// A parent id that doesn't exist is a dangling reference, not a cycle
	// — entity.ValidateRecord doesn't check FK existence today either, so
	// this guard shouldn't newly reject something the rest of the system
	// already tolerates.
	if _, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": "00000000-0000-0000-0000-000000000000"}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"}); err != nil {
		t.Fatalf("expected a dangling parent reference to be allowed, got %v", err)
	}
}

// TestEngine_Update_SelfReferenceNonUUIDParentToleratedAsDangling is the
// regression test for uc-infra#107: a self-referencing field's value that
// isn't shaped like a records.id at all (e.g. hand-typed garbage, or a
// legacy key from an external import) used to reach
// records.GetTxIncludingDeleted's `WHERE id = $1` against a uuid column
// and surface as a raw Postgres "invalid input syntax for type uuid"
// driver error — a 500, not the "tolerated dangling reference" (ADR-0007)
// treatment TestEngine_Update_SelfReferenceDanglingParentAllowed already
// covers for a well-formed-but-nonexistent id. Fixed by canonicalizing
// (ids.Canonical) before querying, the same shape uc-infra#78's
// loadTarget fix already established for FieldReference target
// constraints. Also confirms (via a re-read) that the value was actually
// walked-and-tolerated, not silently dropped from the write.
func TestEngine_Update_SelfReferenceNonUUIDParentToleratedAsDangling(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	a := createCategory(t, ctx, engine, def, "A", "")

	if _, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": "not-a-uuid-at-all"}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"}); err != nil {
		t.Fatalf("expected a non-UUID-shaped parent reference to be tolerated as dangling, got %v", err)
	}

	got, err := engine.Get(ctx, def, a)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Data["parent_category_id"] != "not-a-uuid-at-all" {
		t.Fatalf("expected the tolerated value to actually be written, got %v", got.Data["parent_category_id"])
	}
}

// TestEngine_Update_SelfReferenceNonUUIDMidChainToleratedAsDangling is the
// mid-chain counterpart to the test above: the non-UUID-shaped value sits
// on an ANCESTOR the walk reaches via a stored record's own field data
// (data.RecordRepo.GetTxIncludingDeleted → rec.Data[fieldName]), not on
// the value the caller passed in directly. That's the realistic shape of
// uc-infra#107's own scenario ("a legacy key from an external import")
// and exercises a different code path than the test above: the guard
// canonicalizing `next` after reading it out of a stored record, not
// canonicalizing the caller-supplied newParentID up front.
func TestEngine_Update_SelfReferenceNonUUIDMidChainToleratedAsDangling(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", "")

	// Directly seed B's parent as legacy-import garbage, bypassing the
	// picker (which would never offer a non-UUID value) — standing in for
	// data written by an external import before this guard existed.
	if _, err := engine.Update(ctx, def, b, map[string]any{"name": "B", "parent_category_id": "legacy-key-42"}, nil, actor); err != nil {
		t.Fatalf("seed B with a non-UUID parent: %v", err)
	}

	// Re-parenting A onto B must walk A -> B -> "legacy-key-42" and stop
	// there tolerantly, not 500 on the second hop.
	if _, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": b}, nil, actor); err != nil {
		t.Fatalf("expected a non-UUID-shaped ancestor reference to be tolerated as dangling, got %v", err)
	}
}

// TestEngine_Update_SelfReferenceCycleDetectedAcrossEquivalentSpellings is
// the regression test for a real bug an independent review found in
// uc-infra#107's first draft (a strict-regex UUID shape check, rather
// than canonicalization): Postgres's uuid_in accepts several spellings of
// the same id — mixed case, no hyphens, brace-wrapped — that a
// stored/caller-supplied value can legitimately use. A first-draft fix
// that only checked "is this shaped like the canonical form" (and
// otherwise compared/queried the raw string) would treat
// "{<b's real id>}" as an unrelated, always-resolvable id instead of
// recognizing it as b — silently admitting a live two-node cycle instead
// of rejecting it. Confirms each equivalent spelling is still correctly
// caught, both as a direct self-reference and one hop into the chain.
func TestEngine_Update_SelfReferenceCycleDetectedAcrossEquivalentSpellings(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	brace := func(id string) string { return "{" + id + "}" }
	noHyphens := func(id string) string {
		out := ""
		for _, r := range id {
			if r != '-' {
				out += string(r)
			}
		}
		return out
	}
	upper := func(id string) string {
		out := make([]byte, len(id))
		for i := 0; i < len(id); i++ {
			c := id[i]
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			out[i] = c
		}
		return string(out)
	}

	spellings := []struct {
		name    string
		spelled func(id string) string
	}{
		{"brace-wrapped", brace},
		{"no hyphens", noHyphens},
		{"uppercase", upper},
	}

	for _, tc := range spellings {
		t.Run(tc.name+"/direct", func(t *testing.T) {
			a := createCategory(t, ctx, engine, def, "A", "")
			_, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": tc.spelled(a)}, nil, actor)
			if !errors.Is(err, ErrReferenceCycle) {
				t.Fatalf("expected a %s self-reference to still be rejected as a cycle, got %v", tc.name, err)
			}
		})

		t.Run(tc.name+"/one hop", func(t *testing.T) {
			a := createCategory(t, ctx, engine, def, "A", "")
			b := createCategory(t, ctx, engine, def, "B", a)
			// Re-pointing A to a differently-spelled B closes A -> B -> A.
			_, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": tc.spelled(b)}, nil, actor)
			if !errors.Is(err, ErrReferenceCycle) {
				t.Fatalf("expected a cycle through a %s-spelled ancestor to still be rejected, got %v", tc.name, err)
			}
		})
	}
}

// TestEngine_Update_ConcurrentUpdatesCannotFormATwoNodeCycle is the
// regression test for a real bug an independent review found: under
// plain READ COMMITTED with no locking, two concurrent updates each
// closing one half of a cycle ("A.parent = B" and, at the same time,
// "B.parent = A") can each walk a chain that looks cycle-free from its
// own transaction's point of view and both commit — together producing
// a live two-node cycle that no single write ever appeared to create.
// Fixed by Update taking a per-entity-type pg_advisory_xact_lock before
// walking, serializing every concurrent self-referencing update to the
// same entity type. Run several times (not just once) since a race is
// exactly the kind of bug that can pass a single iteration by luck.
func TestEngine_Update_ConcurrentUpdatesCannotFormATwoNodeCycle(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	for iter := 0; iter < 20; iter++ {
		a := createCategory(t, ctx, engine, def, "A", "")
		b := createCategory(t, ctx, engine, def, "B", "")

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": b}, nil, actor)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errs[1] = engine.Update(ctx, def, b, map[string]any{"name": "B", "parent_category_id": a}, nil, actor)
		}()
		close(start)
		wg.Wait()

		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("iteration %d: expected at least one of the two concurrent updates to be rejected — both succeeding forms a live two-node cycle", iter)
		}

		gotA, err := engine.Get(ctx, def, a)
		if err != nil {
			t.Fatalf("iteration %d: get A: %v", iter, err)
		}
		gotB, err := engine.Get(ctx, def, b)
		if err != nil {
			t.Fatalf("iteration %d: get B: %v", iter, err)
		}
		if gotA.Data["parent_category_id"] == b && gotB.Data["parent_category_id"] == a {
			t.Fatalf("iteration %d: stored data forms a live two-node cycle: A.parent=%v B.parent=%v", iter, gotA.Data["parent_category_id"], gotB.Data["parent_category_id"])
		}
	}
}

// TestEngine_Update_ForeignCycleDoesNotBlockUnrelatedUpdate is the
// regression test for a real bug an independent review found: the walk
// used to flag ANY revisited id as a cycle, not just the record actually
// being updated — so a pre-existing cycle upstream (unrelated to this
// write) permanently bricked every descendant's unrelated edits with a
// misleading "would form a cycle" error. Seeds a genuine A<->B cycle
// directly at the data layer (bypassing the guard, simulating data that
// predates it or was written some other way), then confirms updating C
// (parented under B, otherwise untouched) still succeeds.
func TestEngine_Update_ForeignCycleDoesNotBlockUnrelatedUpdate(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", a)
	c := createCategory(t, ctx, engine, def, "C", b)

	// Seed a genuine A<->B cycle directly via the record repo, bypassing
	// crud.Engine.Update (and therefore its guard) entirely — standing in
	// for data written before this guard existed.
	if _, err := engine.records.Update(ctx, def.EntityType, a, map[string]any{"name": "A", "parent_category_id": b}, nil); err != nil {
		t.Fatalf("seed foreign cycle A->B: %v", err)
	}

	// C is in no cycle — only its own chain (C -> B -> A -> B -> ...)
	// passes through one. Renaming C, unrelated to any reference field,
	// must not be blocked by a cycle that isn't C's problem.
	if _, err := engine.Update(ctx, def, c, map[string]any{"name": "C renamed", "parent_category_id": b}, nil, actor); err != nil {
		t.Fatalf("expected an update unrelated to the foreign cycle to succeed, got %v", err)
	}
}

// TestEngine_Update_SoftDeletedAncestorStillDetectedInCycle is the
// regression test for a real bug an independent review found:
// data.RecordRepo.GetTx filters out soft-deleted records, so a cycle
// routed through a soft-deleted ancestor was invisible to the walk (it
// looked like "reached the top of the chain" instead of a real cycle).
// Fixed by walking via GetTxIncludingDeleted.
func TestEngine_Update_SoftDeletedAncestorStillDetectedInCycle(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", a) // B.parent = A

	if err := engine.records.Delete(ctx, def.EntityType, b); err != nil {
		t.Fatalf("soft-delete B: %v", err)
	}

	// A.parent = B would close A -> B -> A even though B is soft-deleted.
	_, err := engine.Update(ctx, def, a, map[string]any{"name": "A", "parent_category_id": b}, nil, actor)
	if !errors.Is(err, ErrReferenceCycle) {
		t.Fatalf("expected a cycle routed through a soft-deleted ancestor to still be rejected, got %v", err)
	}
}

// widgetDef references categoryDef's entity type but is a DIFFERENT
// entity type itself — proves selfReferenceFields' actual filter
// (Target == def.EntityType), not just "is a FieldReference at all".
// Without this, deleting the Target check from selfReferenceFields would
// break no test: categoryDef's own field happens to be self-referencing,
// so its tests can't tell the two conditions apart.
func widgetDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Widget",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "category_id", Type: entity.FieldReference, Target: "Category"},
		},
	}
}

func TestEngine_Update_ReferenceFieldToADifferentEntityTypeIsNotGuarded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	catDef := categoryDef()
	wDef := widgetDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	cat := createCategory(t, ctx, engine, catDef, "Electronics", "")
	widget, err := engine.Create(ctx, wDef, map[string]any{"name": "Lamp", "category_id": cat}, actor)
	if err != nil {
		t.Fatalf("create widget: %v", err)
	}

	// A Widget.category_id pointing at its OWN id would be nonsensical
	// (different entity types), but proves selfReferenceFields never
	// even considers this field a self-reference to walk in the first
	// place: Target is "Category", not "Widget", so nothing here should
	// touch checkSelfReferenceCycle at all.
	if _, err := engine.Update(ctx, wDef, widget.ID, map[string]any{"name": "Lamp", "category_id": widget.ID}, nil, actor); err != nil {
		t.Fatalf("expected a cross-entity-type reference field to never be treated as a self-reference, got %v", err)
	}
}

func TestEngine_Update_ClearingSelfReferenceAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := categoryDef()

	a := createCategory(t, ctx, engine, def, "A", "")
	b := createCategory(t, ctx, engine, def, "B", a)

	// Clearing a self-reference field (omitting it from a full-replacement
	// update) is a no-op for this guard, not a cycle.
	if _, err := engine.Update(ctx, def, b, map[string]any{"name": "B"}, nil, audit.Actor{Type: audit.ActorHuman, ID: "farshid"}); err != nil {
		t.Fatalf("expected clearing a self-reference field to be allowed, got %v", err)
	}
}
