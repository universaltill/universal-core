package crud

import (
	"context"
	"errors"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// --- pure helper unit tests (no database required) ---

func TestValueMatches(t *testing.T) {
	cases := []struct {
		name string
		a    any
		b    string
		want bool
	}{
		{"nil never matches", nil, "employee", false},
		{"equal strings", "employee", "employee", true},
		{"different strings", "employee", "vendor", false},
		{"float64 stringifies to compare", float64(42), "42", true},
		{"bool stringifies to compare", true, "true", true},
		{"empty string target value", "", "employee", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := valueMatches(tc.a, tc.b); got != tc.want {
				t.Fatalf("valueMatches(%v, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestIntersectIDSets(t *testing.T) {
	cases := []struct {
		name string
		sets [][]string
		want []string
	}{
		{"no sets", nil, nil},
		{"one set passes through deduped", [][]string{{"a", "b", "a"}}, []string{"a", "b"}},
		{"two sets intersect", [][]string{{"a", "b", "c"}, {"b", "c", "d"}}, []string{"b", "c"}},
		{"disjoint sets produce empty, not nil", [][]string{{"a"}, {"b"}}, []string{}},
		{"empty set collapses whole intersection", [][]string{{"a", "b"}, {}}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectIDSets(tc.sets)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil %v, got nil", tc.want)
			}
			gotSet := map[string]bool{}
			for _, id := range got {
				gotSet[id] = true
			}
			if len(gotSet) != len(tc.want) {
				t.Fatalf("intersectIDSets(%v) = %v, want %v", tc.sets, got, tc.want)
			}
			for _, id := range tc.want {
				if !gotSet[id] {
					t.Fatalf("intersectIDSets(%v) = %v, missing %q", tc.sets, got, id)
				}
			}
		})
	}
}

// --- integration tests against a real Postgres (TargetFilter's
// entity-join shape — the PartyRole "does a Party hold this role"
// pattern, modeled here with throwaway Person/PersonRole entities so
// these tests exercise the GENERIC mechanism, not one real module's
// Definition; same reasoning cycle_test.go's categoryDef gives) ---

func personDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Person",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "status", Type: entity.FieldString},
		},
	}
}

func personRoleDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "PersonRole",
		Version:    1,
		Fields: []entity.Field{
			{Name: "person_id", Type: entity.FieldReference, Required: true, Target: "Person"},
			{Name: "role_type", Type: entity.FieldEnum, Required: true,
				EnumValues: []string{"employee", "vendor", "customer"}},
		},
	}
}

// assignmentDef is the TimeEntry.employee_id/Task.assignee_id shape: a
// reference to Person constrained to only a Person holding the
// "employee" PersonRole, via the entity-join TargetFilter condition.
func assignmentDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "Assignment",
		Version:    1,
		Fields: []entity.Field{
			{Name: "title", Type: entity.FieldString, Required: true},
			{Name: "assignee_id", Type: entity.FieldReference, Target: "Person", TargetFilter: []entity.TargetFilterCondition{
				{Entity: "PersonRole", EntityField: "person_id", Field: "role_type", Value: "employee"},
			}},
		},
	}
}

// widgetWithStatusFilterDef is the direct-field TargetFilter shape
// (Entity == ""): the target's OWN field must equal the declared value,
// no join required.
func widgetWithStatusFilterDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "GizmoOrder",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "supplier_id", Type: entity.FieldReference, Target: "Person", TargetFilter: []entity.TargetFilterCondition{
				{Field: "status", Value: "active"},
			}},
		},
	}
}

// groupedItemDef is the Task.parent_task_id shape: a self-reference
// whose target must share this record's own group_id value.
func groupedItemDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "GroupedItem",
		Version:    1,
		Fields: []entity.Field{
			{Name: "name", Type: entity.FieldString, Required: true},
			{Name: "group_id", Type: entity.FieldString, Required: true},
			{Name: "parent_item_id", Type: entity.FieldReference, Target: "GroupedItem", MustMatchParentField: "group_id"},
		},
	}
}

func TestEngine_Create_TargetFilterEntityJoin_RejectsWhenTargetLacksRole(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	personDefV := personDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A Person with no PersonRole at all — not an employee, not a vendor.
	vendor, err := engine.Create(ctx, personDefV, map[string]any{"name": "Acme Vendor Co"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}

	_, err = engine.Create(ctx, assignmentDef(), map[string]any{"title": "Fix the roof", "assignee_id": vendor.ID}, actor)
	if !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for a Person with no employee role, got %v", err)
	}
}

func TestEngine_Create_TargetFilterEntityJoin_RejectsWhenTargetHasWrongRole(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	personDefV := personDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	vendor, err := engine.Create(ctx, personDefV, map[string]any{"name": "Vendor Only"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": vendor.ID, "role_type": "vendor"}, actor); err != nil {
		t.Fatalf("create vendor role: %v", err)
	}

	_, err = engine.Create(ctx, assignmentDef(), map[string]any{"title": "Fix the roof", "assignee_id": vendor.ID}, actor)
	if !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for a Person holding only the vendor role, got %v", err)
	}
}

func TestEngine_Create_TargetFilterEntityJoin_AllowsWhenTargetHoldsRequiredRole(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	personDefV := personDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	person, err := engine.Create(ctx, personDefV, map[string]any{"name": "Jamie Employee"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	// A person can hold multiple roles simultaneously — same
	// foundation.PartyRole many-to-many shape the real kernel uses.
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": person.ID, "role_type": "vendor"}, actor); err != nil {
		t.Fatalf("create vendor role: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": person.ID, "role_type": "employee"}, actor); err != nil {
		t.Fatalf("create employee role: %v", err)
	}

	rec, err := engine.Create(ctx, assignmentDef(), map[string]any{"title": "Fix the roof", "assignee_id": person.ID}, actor)
	if err != nil {
		t.Fatalf("expected assigning to a Person holding the employee role to succeed, got %v", err)
	}
	if rec.Data["assignee_id"] != person.ID {
		t.Fatalf("expected assignee_id %s, got %v", person.ID, rec.Data["assignee_id"])
	}
}

func TestEngine_Update_TargetFilterEntityJoin_RejectsReassigningToNonEmployee(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	personDefV := personDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	employee, err := engine.Create(ctx, personDefV, map[string]any{"name": "Jamie Employee"}, actor)
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": employee.ID, "role_type": "employee"}, actor); err != nil {
		t.Fatalf("create employee role: %v", err)
	}
	vendor, err := engine.Create(ctx, personDefV, map[string]any{"name": "Acme Vendor"}, actor)
	if err != nil {
		t.Fatalf("create vendor person: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": vendor.ID, "role_type": "vendor"}, actor); err != nil {
		t.Fatalf("create vendor role: %v", err)
	}

	assignment, err := engine.Create(ctx, assignmentDef(), map[string]any{"title": "Fix the roof", "assignee_id": employee.ID}, actor)
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	_, err = engine.Update(ctx, assignmentDef(), assignment.ID, map[string]any{"title": "Fix the roof", "assignee_id": vendor.ID}, nil, actor)
	if !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation reassigning to a vendor, got %v", err)
	}
}

func TestEngine_Create_TargetFilterEntityJoin_DanglingTargetAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A target id that doesn't exist at all is a dangling reference, not
	// a role-constraint failure — this guard shouldn't newly reject
	// something the rest of the system already tolerates (same posture
	// checkSelfReferenceCycle's dangling-reference test takes).
	_, err := engine.Create(ctx, assignmentDef(), map[string]any{
		"title": "Ghost task", "assignee_id": "00000000-0000-0000-0000-000000000000",
	}, actor)
	if err != nil {
		t.Fatalf("expected a dangling assignee reference to be allowed, got %v", err)
	}
}

func TestEngine_Create_TargetFilterDirectField_RejectsMismatch(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	inactive, err := engine.Create(ctx, personDef(), map[string]any{"name": "Retired Co", "status": "inactive"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}

	_, err = engine.Create(ctx, widgetWithStatusFilterDef(), map[string]any{"name": "Order 1", "supplier_id": inactive.ID}, actor)
	if !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for an inactive supplier, got %v", err)
	}
}

func TestEngine_Create_TargetFilterDirectField_AllowsMatch(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	active, err := engine.Create(ctx, personDef(), map[string]any{"name": "Live Co", "status": "active"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}

	if _, err := engine.Create(ctx, widgetWithStatusFilterDef(), map[string]any{"name": "Order 1", "supplier_id": active.ID}, actor); err != nil {
		t.Fatalf("expected an active supplier to be allowed, got %v", err)
	}
}

func TestEngine_Update_MustMatchParentField_RejectsCrossGroupParent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	parentInGroupA, err := engine.Create(ctx, def, map[string]any{"name": "A-root", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create parent in group A: %v", err)
	}
	childInGroupB, err := engine.Create(ctx, def, map[string]any{"name": "B-child", "group_id": "group-b"}, actor)
	if err != nil {
		t.Fatalf("create child in group B: %v", err)
	}

	_, err = engine.Update(ctx, def, childInGroupB.ID, map[string]any{
		"name": "B-child", "group_id": "group-b", "parent_item_id": parentInGroupA.ID,
	}, nil, actor)
	if !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected ErrTargetConstraintViolation for a cross-group parent, got %v", err)
	}
}

func TestEngine_Create_MustMatchParentField_AllowsSameGroupParent(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	parent, err := engine.Create(ctx, def, map[string]any{"name": "root", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := engine.Create(ctx, def, map[string]any{
		"name": "child", "group_id": "group-a", "parent_item_id": parent.ID,
	}, actor)
	if err != nil {
		t.Fatalf("expected a same-group parent to be allowed, got %v", err)
	}
	if child.Data["parent_item_id"] != parent.ID {
		t.Fatalf("expected parent_item_id %s, got %v", parent.ID, child.Data["parent_item_id"])
	}
}

func TestEngine_Create_MustMatchParentField_DanglingParentAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	_, err := engine.Create(ctx, def, map[string]any{
		"name": "orphan", "group_id": "group-a", "parent_item_id": "00000000-0000-0000-0000-000000000000",
	}, actor)
	if err != nil {
		t.Fatalf("expected a dangling parent reference to be allowed, got %v", err)
	}
}

func TestEngine_Update_MustMatchParentField_ClearingReferenceAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	parent, err := engine.Create(ctx, def, map[string]any{"name": "root", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := engine.Create(ctx, def, map[string]any{
		"name": "child", "group_id": "group-a", "parent_item_id": parent.ID,
	}, actor)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Clearing the reference field (omitting it from a full-replacement
	// update) is a no-op for this guard, same as the cycle guard's own
	// clearing test.
	if _, err := engine.Update(ctx, def, child.ID, map[string]any{"name": "child renamed", "group_id": "group-a"}, nil, actor); err != nil {
		t.Fatalf("expected clearing parent_item_id to be allowed, got %v", err)
	}
}

// --- ResolveReferenceFilter (the reference-picker's own narrowing path) ---

func TestEngine_ResolveReferenceFilter_EntityJoinCondition(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	employee, err := engine.Create(ctx, personDef(), map[string]any{"name": "Jamie"}, actor)
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": employee.ID, "role_type": "employee"}, actor); err != nil {
		t.Fatalf("create employee role: %v", err)
	}
	vendor, err := engine.Create(ctx, personDef(), map[string]any{"name": "Acme"}, actor)
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": vendor.ID, "role_type": "vendor"}, actor); err != nil {
		t.Fatalf("create vendor role: %v", err)
	}

	assigneeField, ok := assignmentDef().FieldByName("assignee_id")
	if !ok {
		t.Fatal("assignee_id field not found")
	}
	opts, err := engine.ResolveReferenceFilter(ctx, assigneeField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if opts.IDIn == nil {
		t.Fatal("expected IDIn to be resolved (non-nil) for an entity-join TargetFilter")
	}
	found := false
	for _, id := range opts.IDIn {
		if id == vendor.ID {
			t.Fatalf("expected vendor %s to be excluded from the employee-only candidate set, got IDIn=%v", vendor.ID, opts.IDIn)
		}
		if id == employee.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected employee %s to be included in the candidate set, got IDIn=%v", employee.ID, opts.IDIn)
	}

	// Applying opts against the real query must actually filter the
	// list down, not just describe the intent.
	recs, err := engine.ListPageFiltered(ctx, personDef(), data.ListPageOptions{Limit: 10, IDIn: opts.IDIn})
	if err != nil {
		t.Fatalf("ListPageFiltered with resolved IDIn: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != employee.ID {
		t.Fatalf("expected exactly the employee record, got %+v", recs)
	}
}

func TestEngine_ResolveReferenceFilter_MustMatchParentField(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	inGroupA, err := engine.Create(ctx, def, map[string]any{"name": "A-item", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create item in group A: %v", err)
	}
	if _, err := engine.Create(ctx, def, map[string]any{"name": "B-item", "group_id": "group-b"}, actor); err != nil {
		t.Fatalf("create item in group B: %v", err)
	}

	parentField, ok := def.FieldByName("parent_item_id")
	if !ok {
		t.Fatal("parent_item_id field not found")
	}
	opts, err := engine.ResolveReferenceFilter(ctx, parentField, "group-a")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if len(opts.EqualsFilters) != 1 || opts.EqualsFilters[0].Field != "group_id" || opts.EqualsFilters[0].Value != "group-a" {
		t.Fatalf("expected an EqualsFilters entry for group_id=group-a, got %+v", opts.EqualsFilters)
	}

	recs, err := engine.ListPageFiltered(ctx, def, data.ListPageOptions{Limit: 10, EqualsFilters: opts.EqualsFilters})
	if err != nil {
		t.Fatalf("ListPageFiltered with resolved EqualsFilters: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != inGroupA.ID {
		t.Fatalf("expected exactly the group-a item, got %+v", recs)
	}
}

func TestEngine_ResolveReferenceFilter_EmptySiblingValueAppliesNoRestriction(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()

	parentField, ok := def.FieldByName("parent_item_id")
	if !ok {
		t.Fatal("parent_item_id field not found")
	}
	opts, err := engine.ResolveReferenceFilter(ctx, parentField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if len(opts.EqualsFilters) != 0 {
		t.Fatalf("expected no EqualsFilters when the sibling value is unknown, got %+v", opts.EqualsFilters)
	}
	if opts.IDIn != nil {
		t.Fatalf("expected no IDIn restriction for a field with no TargetFilter, got %v", opts.IDIn)
	}
}

// --- edge cases an adversarial review should probe: soft-deleted
// targets, multiple ANDed conditions, and a field combining both
// mechanisms at once ---

// combinedConstraintDef declares BOTH TargetFilter (entity-join) and
// MustMatchParentField on the SAME field — not a shape any real module
// wires up today, but the two mechanisms are independent per-field
// checks sharing one memoized target load (checkReferenceTargetConstraints'
// own loadTarget closure), so this proves they compose correctly rather
// than one silently masking the other.
func combinedConstraintDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "CombinedRef",
		Version:    1,
		Fields: []entity.Field{
			{Name: "group_id", Type: entity.FieldString, Required: true},
			{Name: "target_id", Type: entity.FieldReference, Target: "CombinedTarget",
				MustMatchParentField: "group_id",
				TargetFilter: []entity.TargetFilterCondition{
					{Entity: "PersonRole", EntityField: "person_id", Field: "role_type", Value: "employee"},
				}},
		},
	}
}

func combinedTargetDef() *entity.Definition {
	return &entity.Definition{
		EntityType: "CombinedTarget",
		Version:    1,
		Fields: []entity.Field{
			{Name: "group_id", Type: entity.FieldString, Required: true},
		},
	}
}

func TestEngine_Create_CombinedTargetFilterAndMustMatchParentField_BothEnforced(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A target that satisfies MustMatchParentField (same group) but NOT
	// the entity-join TargetFilter (no PersonRole at all).
	targetSameGroupNoRole, err := engine.Create(ctx, combinedTargetDef(), map[string]any{"group_id": "g1"}, actor)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := engine.Create(ctx, combinedConstraintDef(), map[string]any{
		"group_id": "g1", "target_id": targetSameGroupNoRole.ID,
	}, actor); !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected TargetFilter to still reject a same-group target with no role, got %v", err)
	}

	// A target that satisfies the entity-join TargetFilter but NOT
	// MustMatchParentField (different group).
	targetDifferentGroupWithRole, err := engine.Create(ctx, combinedTargetDef(), map[string]any{"group_id": "g2"}, actor)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{
		"person_id": targetDifferentGroupWithRole.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("create role for target: %v", err)
	}
	if _, err := engine.Create(ctx, combinedConstraintDef(), map[string]any{
		"group_id": "g1", "target_id": targetDifferentGroupWithRole.ID,
	}, actor); !errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected MustMatchParentField to still reject a cross-group target despite holding the role, got %v", err)
	}

	// A target satisfying BOTH must be accepted.
	targetBoth, err := engine.Create(ctx, combinedTargetDef(), map[string]any{"group_id": "g1"}, actor)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{
		"person_id": targetBoth.ID, "role_type": "employee",
	}, actor); err != nil {
		t.Fatalf("create role for target: %v", err)
	}
	if _, err := engine.Create(ctx, combinedConstraintDef(), map[string]any{
		"group_id": "g1", "target_id": targetBoth.ID,
	}, actor); err != nil {
		t.Fatalf("expected a target satisfying both constraints to be accepted, got %v", err)
	}
}

// TestEngine_Create_TargetFilterEntityJoin_SoftDeletedTargetAllowed:
// dangling-reference exemption must ALSO cover a target that once
// existed but was soft-deleted — checkReferenceTargetConstraints uses
// records.GetTx (deleted_at IS NULL), the same "soft-deleted is absent"
// view as every other non-cycle-guard read in this kernel, so this must
// NOT be rejected as a constraint violation.
func TestEngine_Create_TargetFilterEntityJoin_SoftDeletedTargetAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	person, err := engine.Create(ctx, personDef(), map[string]any{"name": "Departed Employee"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": person.ID, "role_type": "employee"}, actor); err != nil {
		t.Fatalf("create employee role: %v", err)
	}
	if err := engine.Delete(ctx, personDef(), person.ID, actor); err != nil {
		t.Fatalf("soft-delete person: %v", err)
	}

	if _, err := engine.Create(ctx, assignmentDef(), map[string]any{
		"title": "Orphaned assignment", "assignee_id": person.ID,
	}, actor); err != nil {
		t.Fatalf("expected a soft-deleted target to be treated as dangling (allowed), got %v", err)
	}
}

// TestEngine_Update_MustMatchParentField_SoftDeletedParentAllowed is the
// MustMatchParentField twin of the test above.
func TestEngine_Update_MustMatchParentField_SoftDeletedParentAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	parent, err := engine.Create(ctx, def, map[string]any{"name": "root", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := engine.Create(ctx, def, map[string]any{"name": "child", "group_id": "group-b"}, actor)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := engine.Delete(ctx, def, parent.ID, actor); err != nil {
		t.Fatalf("soft-delete parent: %v", err)
	}

	if _, err := engine.Update(ctx, def, child.ID, map[string]any{
		"name": "child", "group_id": "group-b", "parent_item_id": parent.ID,
	}, nil, actor); err != nil {
		t.Fatalf("expected a soft-deleted (dangling) parent to be allowed regardless of group mismatch, got %v", err)
	}
}
