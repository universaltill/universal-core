package authz

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
)

// The field-level half of ADR-0006 (commit 2). Semantics under test:
// a field is hidden only when EVERY role the actor holds hides it; a
// hidden field is stripped from every read; and a write of one is
// refused while an omitted one keeps its stored value rather than being
// erased.

func (f *fixture) hide(roleID, entityType, fieldName string) {
	f.create("FieldPermission", map[string]any{
		"role_id": roleID, "entity_type": entityType,
		"field_name": fieldName, "hidden": true,
	})
}

// guardedFor builds the engine a request handler would get for userID.
func guardedFor(f *fixture, userID string) *GuardedEngine {
	return Guard(crud.NewEngine(f.db), humanResolver(f, userID))
}

func mustHidden(t *testing.T, got map[string]bool, err error, want []string, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	wantSet := map[string]bool{}
	for _, n := range want {
		wantSet[n] = true
	}
	if len(got) == 0 && len(wantSet) == 0 {
		return
	}
	if !reflect.DeepEqual(got, wantSet) {
		t.Fatalf("%s = %v, want %v", what, got, wantSet)
	}
}

// A single role hiding a field hides it — the base case.
func TestHiddenFields_SingleRoleHides(t *testing.T) {
	f := newFixture(t)
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "name")

	got, err := humanResolver(f, "user-clerk").HiddenFields(context.Background(), "Party")
	mustHidden(t, got, err, []string{"name"}, "clerk HiddenFields(Party)")
}

// Union of visibility: a field is hidden only when EVERY held role hides
// it. One role that says nothing about the field is enough to see it —
// the same additive reading entity-level grants get, since a role with
// no rule for a field implicitly sees it.
func TestHiddenFields_UnionOfVisibilityAcrossRoles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	sales := f.role("sales")
	f.grant("user-both", clerk.ID)
	f.grant("user-both", sales.ID)
	f.grant("user-clerk-only", clerk.ID)

	// Only "clerk" hides Party.name; only "clerk" AND "sales" both hide
	// Party.description.
	f.hide(clerk.ID, "Party", "name")
	f.hide(clerk.ID, "Party", "tax_id")
	f.hide(sales.ID, "Party", "tax_id")

	got, err := humanResolver(f, "user-both").HiddenFields(ctx, "Party")
	mustHidden(t, got, err, []string{"tax_id"}, "user-both HiddenFields(Party)")

	got, err = humanResolver(f, "user-clerk-only").HiddenFields(ctx, "Party")
	mustHidden(t, got, err, []string{"name", "tax_id"}, "user-clerk-only HiddenFields(Party)")
}

// Two duplicate rows for the SAME role must not stand in for a second
// role's agreement — the count is over distinct held roles, not rows.
// Written because counting rows is the obvious wrong implementation and
// silently over-hides for any tenant that double-authored a rule.
func TestHiddenFields_DuplicateRowsForOneRoleDoNotSatisfyOtherRoles(t *testing.T) {
	f := newFixture(t)
	clerk := f.role("clerk")
	sales := f.role("sales")
	f.grant("user-both", clerk.ID)
	f.grant("user-both", sales.ID)
	f.hide(clerk.ID, "Party", "name")
	f.hide(clerk.ID, "Party", "name") // same role again

	got, err := humanResolver(f, "user-both").HiddenFields(context.Background(), "Party")
	mustHidden(t, got, err, nil, "user-both HiddenFields(Party) with duplicate clerk rows")
}

// hidden=false is not a rule — it neither hides nor blocks another
// role's hiding row from counting. (There are no deny rows in this
// model; "hidden" false is simply the default state written down.)
func TestHiddenFields_HiddenFalseRowIsNotARule(t *testing.T) {
	f := newFixture(t)
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.create("FieldPermission", map[string]any{
		"role_id": clerk.ID, "entity_type": "Party",
		"field_name": "name", "hidden": false,
	})

	got, err := humanResolver(f, "user-clerk").HiddenFields(context.Background(), "Party")
	mustHidden(t, got, err, nil, "clerk HiddenFields(Party) with hidden=false row")
}

// tenant_admin, machine actors, and a user holding NO roles all get
// nothing hidden. The roleless case is the subtle one: "every role you
// hold hides it" is unsatisfiable over an empty set, and treating it as
// vacuously true would hide every ruled field from every roleless user,
// including on entity types their tenant never opted into RBAC at all.
func TestHiddenFields_AdminMachineAndRolelessSeeEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	admin := f.role(AdminRoleCode)
	f.grant("user-clerk", clerk.ID)
	f.grant("user-admin", admin.ID)
	f.hide(clerk.ID, "Party", "name")
	f.hide(admin.ID, "Party", "name") // even an explicit rule on the admin role

	got, err := humanResolver(f, "user-admin").HiddenFields(ctx, "Party")
	mustHidden(t, got, err, nil, "admin HiddenFields(Party)")

	got, err = humanResolver(f, "user-roleless").HiddenFields(ctx, "Party")
	mustHidden(t, got, err, nil, "roleless HiddenFields(Party)")

	machine := NewResolver(f.db, f.actor, true)
	got, err = machine.HiddenFields(ctx, "Party")
	mustHidden(t, got, err, nil, "machine HiddenFields(Party)")
}

// Rules are scoped to their own entity_type — a rule on Party must not
// leak onto Address.
func TestHiddenFields_ScopedToEntityType(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "name")

	r := humanResolver(f, "user-clerk")
	got, err := r.HiddenFields(ctx, "Address")
	mustHidden(t, got, err, nil, "clerk HiddenFields(Address)")
	got, err = r.HiddenFields(ctx, "Party")
	mustHidden(t, got, err, []string{"name"}, "clerk HiddenFields(Party)")
}

// Every read path strips the hidden field: Get, List, ListPage and
// ListByField. Asserted on all four rather than one, because each is a
// separate delegation in GuardedEngine and a missed one is a silent leak
// on whichever surface happens to call it.
func TestGuardedEngine_ReadsStripHiddenFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	partyDef := f.def("Party")
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "Visible Co", "tax_id": "SECRET-TAX-ID"})

	g := guardedFor(f, "user-clerk")
	assertStripped := func(what string, recs []data.Record) {
		t.Helper()
		if len(recs) == 0 {
			t.Fatalf("%s returned no records", what)
		}
		for _, got := range recs {
			if _, present := got.Data["tax_id"]; present {
				t.Fatalf("%s leaked hidden field: %v", what, got.Data)
			}
			if got.Data["name"] != "Visible Co" {
				t.Fatalf("%s dropped a visible field: %v", what, got.Data)
			}
		}
	}

	one, err := g.Get(ctx, partyDef, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertStripped("Get", []data.Record{one})

	all, err := g.List(ctx, partyDef)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertStripped("List", all)

	page, err := g.ListPage(ctx, partyDef, 10, 0)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	assertStripped("ListPage", page)

	byField, err := g.ListByField(ctx, partyDef, "name", "Visible Co")
	if err != nil {
		t.Fatalf("ListByField: %v", err)
	}
	assertStripped("ListByField", byField)

	// The unguarded engine still sees everything — redaction is a
	// property of the guarded view, not of the stored record.
	raw, err := crud.NewEngine(f.db).Get(ctx, partyDef, rec.ID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if raw.Data["tax_id"] != "SECRET-TAX-ID" {
		t.Fatalf("raw read lost the hidden value: %v", raw.Data)
	}
}

// The whole point of the write half: a form that legitimately omits a
// hidden field must NOT erase it. crud.Update replaces the record's
// whole data blob, so without restoration "hide this field" would mean
// "let this user delete this field."
func TestGuardedEngine_UpdateOmittingHiddenFieldPreservesStoredValue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	partyDef := f.def("Party")
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "Before", "tax_id": "KEEP-ME"})

	g := guardedFor(f, "user-clerk")
	// Exactly what a redacted form submits: every field it was allowed to
	// render, and nothing at all for the one it wasn't.
	if _, err := g.Update(ctx, partyDef, rec.ID, map[string]any{"party_type": "organization", "name": "After"}, nil, f.actor); err != nil {
		t.Fatalf("Update: %v", err)
	}

	stored, err := crud.NewEngine(f.db).Get(ctx, partyDef, rec.ID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if stored.Data["name"] != "After" {
		t.Fatalf("visible field not updated: %v", stored.Data)
	}
	if stored.Data["tax_id"] != "KEEP-ME" {
		t.Fatalf("hidden field was erased by a save that omitted it: %v", stored.Data)
	}
}

// Writing a hidden field is refused, on both create and update — a user
// must not be able to set a value they are not allowed to read.
func TestGuardedEngine_WritingHiddenFieldDenied(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	partyDef := f.def("Party")
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "Target", "tax_id": "ORIGINAL"})
	g := guardedFor(f, "user-clerk")

	_, err := g.Update(ctx, partyDef, rec.ID, map[string]any{"party_type": "organization", "name": "Target", "tax_id": "OVERWRITTEN"}, nil, f.actor)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("update of hidden field: expected ErrDenied, got %v", err)
	}
	_, err = g.Create(ctx, partyDef, map[string]any{"party_type": "organization", "name": "New", "tax_id": "SNEAKY"}, f.actor)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("create setting hidden field: expected ErrDenied, got %v", err)
	}

	// The refused update wrote nothing.
	stored, err := crud.NewEngine(f.db).Get(ctx, partyDef, rec.ID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if stored.Data["tax_id"] != "ORIGINAL" {
		t.Fatalf("denied update still changed the hidden field: %v", stored.Data)
	}
}

// Re-submitting the hidden field at exactly its stored value is allowed,
// not denied — this is what makes EffectiveWriteFields idempotent, which
// is in turn what lets internal/api call it before its own validation
// while the engine still calls it again on the way through.
func TestGuardedEngine_EffectiveWriteFieldsIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	partyDef := f.def("Party")
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "Target", "tax_id": "ORIGINAL"})
	g := guardedFor(f, "user-clerk")

	once, err := g.EffectiveWriteFields(ctx, partyDef, rec.ID, map[string]any{"party_type": "organization", "name": "Renamed"})
	if err != nil {
		t.Fatalf("EffectiveWriteFields: %v", err)
	}
	if once["tax_id"] != "ORIGINAL" {
		t.Fatalf("stored value not restored: %v", once)
	}
	twice, err := g.EffectiveWriteFields(ctx, partyDef, rec.ID, once)
	if err != nil {
		t.Fatalf("EffectiveWriteFields (second pass): %v", err)
	}
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("not idempotent: %v then %v", once, twice)
	}
	// And it did not mutate the caller's map.
	input := map[string]any{"party_type": "organization", "name": "Renamed"}
	if _, err := g.EffectiveWriteFields(ctx, partyDef, rec.ID, input); err != nil {
		t.Fatalf("EffectiveWriteFields: %v", err)
	}
	if _, leaked := input["tax_id"]; leaked {
		t.Fatalf("EffectiveWriteFields mutated the caller's map: %v", input)
	}
}

// A field with no stored value stays absent rather than being written
// back as an explicit nil — an update must not invent a key the record
// never had.
func TestGuardedEngine_UnsetHiddenFieldStaysAbsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	partyDef := f.def("Party")
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "No Description"})
	g := guardedFor(f, "user-clerk")

	if _, err := g.Update(ctx, partyDef, rec.ID, map[string]any{"party_type": "organization", "name": "Still None"}, nil, f.actor); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err := crud.NewEngine(f.db).Get(ctx, partyDef, rec.ID)
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if _, present := stored.Data["tax_id"]; present {
		t.Fatalf("unset hidden field was materialized: %v", stored.Data)
	}
}

// A create echoes the new record straight back to the caller, so it goes
// through the same redaction a read would — a create must not become a
// way to observe a hidden field.
func TestGuardedEngine_CreateEchoIsRedacted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	g := guardedFor(f, "user-clerk")
	rec, err := g.Create(ctx, f.def("Party"), map[string]any{"party_type": "organization", "name": "Fresh"}, f.actor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, present := rec.Data["tax_id"]; present {
		t.Fatalf("create echo leaked the hidden field: %v", rec.Data)
	}
}

// Field rules apply independently of entity-level opt-in: hiding a field
// on a type nobody wrote a Permission row for still works (that is the
// stated reason FieldPermission is a standalone entity, not a
// composition under Permission).
func TestHiddenFields_AppliesWithoutEntityLevelOptIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	g := guardedFor(f, "user-clerk")
	// No Permission row for Party anywhere — reading is still allowed...
	rec := f.create("Party", map[string]any{"party_type": "organization", "name": "Open", "tax_id": "STILL-HIDDEN"})
	got, err := g.Get(ctx, f.def("Party"), rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// ...but the field rule holds.
	if _, present := got.Data["tax_id"]; present {
		t.Fatalf("field rule ignored on a type with no entity-level rules: %v", got.Data)
	}
}

// A FieldPermission row is enough to count as "this tenant has
// configured RBAC," which closes the control plane's bootstrap window
// even with no Permission row anywhere — otherwise a tenant that only
// ever authored field rules would leave self-granting wide open.
func TestFieldPermissionAloneClosesControlPlaneBootstrap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	r := humanResolver(f, "user-clerk")
	got, err := r.CanWrite(ctx, "Role")
	mustCan(t, got, err, false, "clerk CanWrite(Role) once field rules exist")
}

// Sorting or filtering by a field the actor may not read is refused at the
// guard layer — independent review flagged this had no direct test (the
// handler's own pre-filter reached it first). A hidden field used as a
// sort key would otherwise let page order bisect its values.
func TestGuardedEngine_RejectsHiddenSortFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clerk := f.role("clerk")
	f.grant("user-clerk", clerk.ID)
	f.hide(clerk.ID, "Party", "tax_id")

	g := guardedFor(f, "user-clerk")
	def := f.def("Party")

	if _, err := g.ListPageFiltered(ctx, def, data.ListPageOptions{SortField: "tax_id", Limit: 10}); !errors.Is(err, ErrDenied) {
		t.Fatalf("sorting by a hidden field should be denied, got %v", err)
	}
	if _, err := g.ListPageFiltered(ctx, def, data.ListPageOptions{FilterField: "tax_id", FilterValue: "x", Limit: 10}); !errors.Is(err, ErrDenied) {
		t.Fatalf("filtering by a hidden field should be denied, got %v", err)
	}
	if _, err := g.CountFiltered(ctx, def, data.ListPageOptions{FilterField: "tax_id", FilterValue: "x"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("counting by a hidden field should be denied, got %v", err)
	}
	// A VISIBLE field is fine.
	if _, err := g.ListPageFiltered(ctx, def, data.ListPageOptions{SortField: "name", Limit: 10}); err != nil {
		t.Fatalf("sorting by a visible field should work: %v", err)
	}
}
