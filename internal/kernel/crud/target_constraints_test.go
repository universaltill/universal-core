package crud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// groupedItemOptionalGroupDef is groupedItemDef with group_id NOT
// Required — used only by the "target has nothing to compare"
// consistency test below (finding #9), which needs a target record that
// legitimately has no stored value for its own MustMatchParentField
// field at all.
func groupedItemOptionalGroupDef() *entity.Definition {
	def := groupedItemDef()
	fields := make([]entity.Field, len(def.Fields))
	copy(fields, def.Fields)
	for i, f := range fields {
		if f.Name == "group_id" {
			f.Required = false
			fields[i] = f
		}
	}
	def.Fields = fields
	return def
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

// TestEngine_Create_TargetFilterEntityJoin_NonUUIDReferenceValueAllowed
// is the regression test for independent review finding #3: a
// non-UUID-shaped reference value (a validation-layer concern
// entity.validateFieldValue only checks is a string, not that it's
// UUID-shaped) used to reach records.GetTx's `WHERE id = $1` against a
// uuid column, producing a raw Postgres "invalid input syntax for type
// uuid" error that fell through to an internal 500 — a regression from
// the pre-existing "tolerated dangling reference" behaviour, which the
// existing dangling-reference tests above don't catch because
// "00000000-0000-0000-0000-000000000000" IS valid UUID syntax. A
// non-UUID string must be tolerated exactly the same way.
func TestEngine_Create_TargetFilterEntityJoin_NonUUIDReferenceValueAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	_, err := engine.Create(ctx, assignmentDef(), map[string]any{
		"title": "Ghost task", "assignee_id": "junk",
	}, actor)
	if err != nil {
		t.Fatalf("expected a non-UUID assignee reference to be tolerated as dangling, got %v", err)
	}
}

// TestEngine_Create_MustMatchParentField_NonUUIDReferenceValueAllowed is
// the MustMatchParentField twin of the test above — the direct-field
// (Entity == "") TargetFilter shape and the MustMatchParentField
// mechanism share the same loadTarget closure, so both need the same
// non-UUID tolerance proven independently. Uses Create, deliberately,
// not Update: groupedItemDef's parent_item_id is a SELF-reference, and
// Update additionally runs checkSelfReferenceCycle first (crud.go),
// which has this exact same class of bug in its own GetTxIncludingDeleted
// call (independent review found this while writing this test) — a
// real, separate, PRE-EXISTING gap this PR did not introduce and did
// not touch, out of scope for this fix pass, flagged in the code review
// doc instead. Create never runs that check at all (a new record's id
// cannot yet appear in any chain — crud.go's own doc comment), so it
// isolates target_constraints.go's own fix cleanly.
func TestEngine_Create_MustMatchParentField_NonUUIDReferenceValueAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	if _, err := engine.Create(ctx, def, map[string]any{
		"name": "orphan", "group_id": "group-a", "parent_item_id": "not-a-uuid",
	}, actor); err != nil {
		t.Fatalf("expected a non-UUID parent_item_id to be tolerated as dangling, got %v", err)
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

// TestEngine_Create_MustMatchParentField_TargetMissingOwnValueAllowed is
// the regression test for independent review finding #9: a target
// record that EXISTS but has nothing stored for its own
// MustMatchParentField value (never set — group_id is optional on this
// throwaway Definition) used to always fail valueMatches(nil, ownValue)
// and reject, making the field permanently unusable against any such
// target — inconsistent with the field-omitted-on-THIS-record case just
// above, which already skips rather than rejects. Both "nothing to
// compare" cases must now behave the same way: skip, not reject.
func TestEngine_Create_MustMatchParentField_TargetMissingOwnValueAllowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemOptionalGroupDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A parent created with no group_id at all — nothing for the child's
	// own group_id to compare against.
	parent, err := engine.Create(ctx, def, map[string]any{"name": "ungrouped-root"}, actor)
	if err != nil {
		t.Fatalf("create parent with no group_id: %v", err)
	}

	if _, err := engine.Create(ctx, def, map[string]any{
		"name": "child", "group_id": "group-a", "parent_item_id": parent.ID,
	}, actor); err != nil {
		t.Fatalf("expected a parent with no stored group_id to be treated as not-comparable (allowed), got %v", err)
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
	opts, err := engine.ResolveReferenceFilter(ctx, assignmentDef(), assigneeField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if len(opts.JoinFilters) != 1 {
		t.Fatalf("expected exactly one JoinFilters entry for an entity-join TargetFilter, got %+v", opts.JoinFilters)
	}
	jf := opts.JoinFilters[0]
	if jf.Entity != "PersonRole" || jf.EntityField != "person_id" || jf.Field != "role_type" || jf.Value != "employee" {
		t.Fatalf("unexpected JoinFilters entry: %+v", jf)
	}

	// Applying opts against the real query must actually filter the
	// list down, not just describe the intent — this is the correlated
	// EXISTS narrowing, evaluated by Postgres itself, not an id list
	// resolved and intersected in Go (independent review, uc-infra#78
	// follow-up: the original DistinctFieldValues-based shape did the
	// latter, unboundedly).
	recs, err := engine.ListPageFiltered(ctx, personDef(), data.ListPageOptions{Limit: 10, JoinFilters: opts.JoinFilters})
	if err != nil {
		t.Fatalf("ListPageFiltered with resolved JoinFilters: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != employee.ID {
		t.Fatalf("expected exactly the employee record, got %+v", recs)
	}
	for _, rec := range recs {
		if rec.ID == vendor.ID {
			t.Fatalf("expected vendor %s to be excluded from the employee-only candidate set, got %+v", vendor.ID, recs)
		}
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
	opts, err := engine.ResolveReferenceFilter(ctx, def, parentField, "group-a")
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
	opts, err := engine.ResolveReferenceFilter(ctx, def, parentField, "")
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

// --- status_id auto-scoping to sourceDef's own StatusType (ADR-0032,
// uc-infra#250) — no declared TargetFilter involved at all; the
// narrowing is derived purely from sourceDef.StatusTypeCode, the exact
// fixtures status_test.go's ValidateStatusTransition tests already use
// for the equivalent write-path scoping. ---

// partyDef is purchaseOrderDef's counterpart for a SECOND, distinct
// StatusTypeCode ("party_status") — used below to prove the narrowing
// is actually keyed on sourceDef.StatusTypeCode, not incidentally
// narrowed to whichever StatusType happens to be seeded/created first
// (independent review, ADR-0032/uc-infra#250: the original version of
// this test used only ONE real sourceDef, so a bug that hardcoded
// "purchase_order_status" — or "the first StatusType row in the
// tenant" — instead of actually reading sourceDef.StatusTypeCode would
// still have passed it).
func partyDef() *entity.Definition {
	return &entity.Definition{
		EntityType:     "Party",
		Version:        1,
		StatusTypeCode: "party_status",
		Fields: []entity.Field{
			{Name: "status_id", Type: entity.FieldReference, Required: true, Target: "Status"},
		},
	}
}

func TestEngine_ResolveReferenceFilter_StatusIDScopedToOwnStatusType(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	fx := seedStatusFixture(t, ctx, engine)

	// A second StatusType/Status pair, seeded AFTER purchase_order_status
	// (so ordering can't accidentally make it "the narrowing" either) —
	// party_status's own Active status.
	otherType, err := engine.Create(ctx, statusTypeDef(), map[string]any{
		"entity_type": "Party", "code": "party_status", "name": "Party Status",
	}, actor)
	if err != nil {
		t.Fatalf("seed other status type: %v", err)
	}
	activeStatus, err := engine.Create(ctx, statusDef(), map[string]any{
		"status_type_id": otherType.ID, "code": "active", "name": "Active", "is_initial": true,
	}, actor)
	if err != nil {
		t.Fatalf("seed other status: %v", err)
	}

	poDef := purchaseOrderDef()
	poStatusField, ok := poDef.FieldByName("status_id")
	if !ok {
		t.Fatal("status_id field not found on purchaseOrderDef")
	}
	partyD := partyDef()
	partyStatusField, ok := partyD.FieldByName("status_id")
	if !ok {
		t.Fatal("status_id field not found on partyDef")
	}

	// PurchaseOrder's own picker: draft + submitted only.
	poOpts, err := engine.ResolveReferenceFilter(ctx, poDef, poStatusField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter(purchaseOrderDef): %v", err)
	}
	if len(poOpts.EqualsFilters) != 1 || poOpts.EqualsFilters[0].Field != "status_type_id" {
		t.Fatalf("expected exactly one status_type_id EqualsFilters entry, got %+v", poOpts.EqualsFilters)
	}
	poRecs, err := engine.ListPageFiltered(ctx, statusDef(), data.ListPageOptions{Limit: 10, EqualsFilters: poOpts.EqualsFilters})
	if err != nil {
		t.Fatalf("ListPageFiltered with resolved EqualsFilters (PurchaseOrder): %v", err)
	}
	if len(poRecs) != 2 {
		t.Fatalf("expected exactly the 2 purchase_order_status statuses (draft, submitted), got %+v", poRecs)
	}
	gotIDs := map[string]bool{poRecs[0].ID: true, poRecs[1].ID: true}
	if !gotIDs[fx.draftID] || !gotIDs[fx.submittedID] {
		t.Fatalf("expected draft (%s) and submitted (%s) in results, got %+v", fx.draftID, fx.submittedID, poRecs)
	}
	for _, rec := range poRecs {
		if rec.Data["code"] == "active" {
			t.Fatalf("expected party_status's Active status excluded from PurchaseOrder's picker, got %+v", poRecs)
		}
	}

	// SAME target field shape, DIFFERENT sourceDef (Party, StatusTypeCode
	// "party_status") — must narrow to the OPPOSITE set: Active only,
	// draft/submitted excluded. This is the actual proof the EqualsFilters
	// value tracks sourceDef.StatusTypeCode rather than being constant.
	partyOpts, err := engine.ResolveReferenceFilter(ctx, partyD, partyStatusField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter(partyDef): %v", err)
	}
	if len(partyOpts.EqualsFilters) != 1 || partyOpts.EqualsFilters[0].Field != "status_type_id" {
		t.Fatalf("expected exactly one status_type_id EqualsFilters entry, got %+v", partyOpts.EqualsFilters)
	}
	if partyOpts.EqualsFilters[0].Value == poOpts.EqualsFilters[0].Value {
		t.Fatalf("expected a DIFFERENT status_type_id than PurchaseOrder's own — got the same value %q for both sourceDefs", partyOpts.EqualsFilters[0].Value)
	}
	partyRecs, err := engine.ListPageFiltered(ctx, statusDef(), data.ListPageOptions{Limit: 10, EqualsFilters: partyOpts.EqualsFilters})
	if err != nil {
		t.Fatalf("ListPageFiltered with resolved EqualsFilters (Party): %v", err)
	}
	if len(partyRecs) != 1 || partyRecs[0].ID != activeStatus.ID {
		t.Fatalf("expected exactly party_status's Active status, got %+v", partyRecs)
	}
}

func TestEngine_ResolveReferenceFilter_StatusIDFieldWithoutStatusTypeCodeNotNarrowed(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)

	// A field shaped exactly like a real status_id field (name, type,
	// target) but declared on a Definition with NO StatusTypeCode —
	// entity.Definition.Validate() would never let a real module publish
	// this shape (it only REQUIRES status_id when StatusTypeCode is set,
	// it doesn't forbid the field existing without it), but
	// ResolveReferenceFilter must not infer scoping from field
	// name/target alone, only from sourceDef.StatusTypeCode explicitly —
	// this proves the guard checks both, not just the field's own shape.
	def := &entity.Definition{
		EntityType: "Widget",
		Version:    1,
		Fields: []entity.Field{
			{Name: "status_id", Type: entity.FieldReference, Target: "Status"},
		},
	}
	statusField, ok := def.FieldByName("status_id")
	if !ok {
		t.Fatal("status_id field not found")
	}

	opts, err := engine.ResolveReferenceFilter(ctx, def, statusField, "")
	if err != nil {
		t.Fatalf("ResolveReferenceFilter: %v", err)
	}
	if len(opts.EqualsFilters) != 0 {
		t.Fatalf("expected no auto-narrowing for a Definition with no StatusTypeCode, got %+v", opts.EqualsFilters)
	}
}

func TestEngine_ResolveReferenceFilter_StatusIDUnresolvableStatusTypeCodeReturnsError(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	engine := NewEngine(db)
	// Deliberately no seedStatusFixture call — purchase_order_status is
	// not published for this tenant, matching
	// TestValidateStatusTransition_CreateRequiresStatusID's sibling
	// "not published" gap on the write-path check this mirrors.

	def := purchaseOrderDef()
	statusField, ok := def.FieldByName("status_id")
	if !ok {
		t.Fatal("status_id field not found")
	}

	_, err := engine.ResolveReferenceFilter(ctx, def, statusField, "")
	if err == nil {
		t.Fatal("expected an error resolving picker narrowing for an unpublished StatusTypeCode, not a silent skip")
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected the error to wrap ErrInvalidTransition (same sentinel statusTypeIDByCode already uses), got %v", err)
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

// --- CountTargetConstraintViolations (uc-infra#112): the read-only
// reporting counterpart to the write-path enforcement above — the
// FieldReference equivalent of RecordRepo.CountMissingField, used to warn
// an operator about EXISTING records that already fail a
// TargetFilter/MustMatchParentField constraint newly added or tightened
// on a live tenant, before they hit a 400 on next edit. ---

// The three early-return tests below deliberately pass nil for BOTH db
// and records: the guard at the top of CountTargetConstraintViolations
// must return before touching either. Independent review verified by
// mutation that calling them against an empty live table (the original
// shape) made all three pass even with the guard's conditions deleted
// entirely — 0 came back either way, since there was nothing in the
// table regardless of whether the guard fired. Against nil, a removed
// or weakened guard panics on the first dereference instead of silently
// returning 0, and these need no TEST_DATABASE_URL at all.

func TestCountTargetConstraintViolations_FieldNotFound(t *testing.T) {
	n, err := CountTargetConstraintViolations(context.Background(), nil, nil, assignmentDef(), "no_such_field")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 for an unknown field name, got %d", n)
	}
}

func TestCountTargetConstraintViolations_NotAFieldReference(t *testing.T) {
	n, err := CountTargetConstraintViolations(context.Background(), nil, nil, assignmentDef(), "title")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 for a non-FieldReference field, got %d", n)
	}
}

func TestCountTargetConstraintViolations_NoConstraintConfigured(t *testing.T) {
	// A FieldReference field with neither TargetFilter nor
	// MustMatchParentField declared has nothing this function can report.
	def := &entity.Definition{
		EntityType: "Assignment",
		Fields: []entity.Field{
			{Name: "plain_ref", Type: entity.FieldReference, Target: "Person"},
		},
	}
	n, err := CountTargetConstraintViolations(context.Background(), nil, nil, def, "plain_ref")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 when the field declares no constraint, got %d", n)
	}
}

// TestCountTargetConstraintViolations_NoRecordsAtAll is the true "empty
// page" case (line 314's `if len(page) == 0 { break }`) as distinct from
// the early-return tests above: this DOES reach ListPage — the
// constrained field is real and configured, there just happen to be no
// records of the entity type yet.
func TestCountTargetConstraintViolations_NoRecordsAtAll(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)

	n, err := CountTargetConstraintViolations(context.Background(), db, records, assignmentDef(), "assignee_id")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 when no records exist yet, got %d", n)
	}
}

// TestCountTargetConstraintViolations_CountsOnlyLiveViolations is the main
// case: records inserted via the raw repo (records.Create), bypassing
// Engine.Create's own enforcement — modeling data that predates a
// TargetFilter being added/tightened on a live tenant, exactly the
// scenario this reporting function exists to surface. Each fixture is
// asserted as one contribution to a single count (same "deltas cannot
// cancel" discipline as data.TestCountMissingField): a satisfying record,
// two violating records, a dangling reference, a record with no value
// set, and a soft-deleted violation that must NOT be counted.
func TestCountTargetConstraintViolations_CountsOnlyLiveViolations(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	employee, err := engine.Create(ctx, personDef(), map[string]any{"name": "Jamie Employee"}, actor)
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	if _, err := engine.Create(ctx, personRoleDef(), map[string]any{"person_id": employee.ID, "role_type": "employee"}, actor); err != nil {
		t.Fatalf("create employee role: %v", err)
	}
	nonEmployee, err := engine.Create(ctx, personDef(), map[string]any{"name": "Acme Vendor"}, actor)
	if err != nil {
		t.Fatalf("create non-employee person: %v", err)
	}

	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "valid", "assignee_id": employee.ID}); err != nil {
		t.Fatalf("create valid assignment: %v", err)
	}
	violatingToDelete, err := records.Create(ctx, "Assignment", map[string]any{"title": "violating, later deleted", "assignee_id": nonEmployee.ID})
	if err != nil {
		t.Fatalf("create violating assignment: %v", err)
	}
	// TWO violating records stay live, deliberately, not one: a count that
	// merely SET a "found a violation" flag to 1 instead of actually
	// incrementing would still pass a single-live-violation assertion —
	// this needs at least two live violations to distinguish "count" from
	// "detect".
	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "violating, stays live 1", "assignee_id": nonEmployee.ID}); err != nil {
		t.Fatalf("create second violating assignment: %v", err)
	}
	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "violating, stays live 2", "assignee_id": nonEmployee.ID}); err != nil {
		t.Fatalf("create third violating assignment: %v", err)
	}
	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "dangling", "assignee_id": "00000000-0000-0000-0000-000000000000"}); err != nil {
		t.Fatalf("create dangling assignment: %v", err)
	}
	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "no assignee set"}); err != nil {
		t.Fatalf("create assignment with no assignee: %v", err)
	}
	if err := engine.Delete(ctx, assignmentDef(), violatingToDelete.ID, actor); err != nil {
		t.Fatalf("soft-delete violating assignment: %v", err)
	}

	n, err := CountTargetConstraintViolations(ctx, db, records, assignmentDef(), "assignee_id")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 live violations (the soft-deleted one excluded), got %d", n)
	}
}

// TestCountTargetConstraintViolations_MustMatchParentField_CountsAcrossGroups
// is the MustMatchParentField twin of the TargetFilter-based main case
// above — the same reporting function, exercised via the OTHER
// constraint mechanism, using groupedItemDef's self-reference shape.
func TestCountTargetConstraintViolations_MustMatchParentField_CountsAcrossGroups(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)
	ctx := context.Background()
	engine := NewEngine(db)
	def := groupedItemDef()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	parentA, err := engine.Create(ctx, def, map[string]any{"name": "A-root", "group_id": "group-a"}, actor)
	if err != nil {
		t.Fatalf("create parent in group A: %v", err)
	}
	parentB, err := engine.Create(ctx, def, map[string]any{"name": "B-root", "group_id": "group-b"}, actor)
	if err != nil {
		t.Fatalf("create parent in group B: %v", err)
	}

	// Same-group children: satisfy MustMatchParentField, not counted.
	if _, err := records.Create(ctx, "GroupedItem", map[string]any{"name": "same-group child", "group_id": "group-a", "parent_item_id": parentA.ID}); err != nil {
		t.Fatalf("create same-group child: %v", err)
	}
	// Cross-group children: violate MustMatchParentField.
	if _, err := records.Create(ctx, "GroupedItem", map[string]any{"name": "cross-group child 1", "group_id": "group-b", "parent_item_id": parentA.ID}); err != nil {
		t.Fatalf("create cross-group child 1: %v", err)
	}
	if _, err := records.Create(ctx, "GroupedItem", map[string]any{"name": "cross-group child 2", "group_id": "group-a", "parent_item_id": parentB.ID}); err != nil {
		t.Fatalf("create cross-group child 2: %v", err)
	}

	n, err := CountTargetConstraintViolations(ctx, db, records, def, "parent_item_id")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 cross-group violations, got %d", n)
	}
}

// TestCountTargetConstraintViolations_PaginatesAcrossMultiplePages seeds
// more records than the function's internal pageSize (200) so the second
// ListPage call (offset=200) and the loop's continuation past a full
// first page are actually exercised, not just the single-page path every
// other test above takes. Also exercises the direct-field (Entity=="")
// TargetFilter shape, which the tests above never routed through Count.
func TestCountTargetConstraintViolations_PaginatesAcrossMultiplePages(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	active, err := engine.Create(ctx, personDef(), map[string]any{"name": "Live Co", "status": "active"}, actor)
	if err != nil {
		t.Fatalf("create active supplier: %v", err)
	}
	inactive, err := engine.Create(ctx, personDef(), map[string]any{"name": "Retired Co", "status": "inactive"}, actor)
	if err != nil {
		t.Fatalf("create inactive supplier: %v", err)
	}

	const total = 205 // > pageSize (200): forces a second ListPage call
	const wantViolations = 7
	for i := 0; i < total; i++ {
		supplier := active.ID
		if i < wantViolations {
			supplier = inactive.ID
		}
		if _, err := records.Create(ctx, "GizmoOrder", map[string]any{
			"name": fmt.Sprintf("Order %d", i), "supplier_id": supplier,
		}); err != nil {
			t.Fatalf("create order %d: %v", i, err)
		}
	}

	n, err := CountTargetConstraintViolations(ctx, db, records, widgetWithStatusFilterDef(), "supplier_id")
	if err != nil {
		t.Fatalf("CountTargetConstraintViolations: %v", err)
	}
	if n != wantViolations {
		t.Fatalf("expected %d violations across %d records spanning multiple pages, got %d", wantViolations, total, n)
	}
}

// erroringQueryable wraps a real *sql.DB but ignores every query it is
// given, substituting one against a table that does not exist — a
// genuine, deterministic driver error on every call, without needing a
// hand-rolled driver. Used only to reach CountTargetConstraintViolations'
// non-ErrTargetConstraintViolation error branch (line 326): records.ListPage
// itself runs against the RecordRepo's OWN connection, entirely separate
// from this stub, so the page still lists successfully — only the
// per-record target-loading query below (routed through this stub) fails.
type erroringQueryable struct{ real *sql.DB }

func (e erroringQueryable) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	return e.real.QueryRowContext(ctx, "SELECT 1 FROM uc_test_table_that_does_not_exist")
}

func (e erroringQueryable) QueryContext(ctx context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return e.real.QueryContext(ctx, "SELECT 1 FROM uc_test_table_that_does_not_exist")
}

func (e erroringQueryable) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	return e.real.ExecContext(ctx, "SELECT 1 FROM uc_test_table_that_does_not_exist")
}

func TestCountTargetConstraintViolations_NonViolationErrorPropagates(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)
	ctx := context.Background()
	engine := NewEngine(db)
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}

	// A live record with a real, resolvable target — loadTarget must
	// actually attempt the query below for the stub's failure to be
	// reached; a dangling/absent reference would short-circuit first.
	person, err := engine.Create(ctx, personDef(), map[string]any{"name": "Jamie"}, actor)
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	if _, err := records.Create(ctx, "Assignment", map[string]any{"title": "t", "assignee_id": person.ID}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	stub := erroringQueryable{real: db}
	_, err = CountTargetConstraintViolations(ctx, stub, records, assignmentDef(), "assignee_id")
	if err == nil {
		t.Fatal("expected a genuine DB error to propagate, got nil")
	}
	if errors.Is(err, ErrTargetConstraintViolation) {
		t.Fatalf("expected a genuine DB error, not ErrTargetConstraintViolation: %v", err)
	}
}

// TestCountTargetConstraintViolations_ListPageErrorPropagates covers the
// ListPage error path: a canceled context makes the underlying query fail
// deterministically, without needing to sabotage the connection itself.
func TestCountTargetConstraintViolations_ListPageErrorPropagates(t *testing.T) {
	db := freshTenantDB(t)
	records := data.NewRecordRepo(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := CountTargetConstraintViolations(ctx, db, records, assignmentDef(), "assignee_id")
	if err == nil {
		t.Fatal("expected an error from ListPage when the context is already canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the error to wrap context.Canceled, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "Assignment") || !strings.Contains(got, "assignee_id") {
		t.Fatalf("expected the wrapped error to name Assignment.assignee_id, got %q", got)
	}
}
