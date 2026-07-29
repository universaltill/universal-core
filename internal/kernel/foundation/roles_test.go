package foundation

import (
	"context"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// TestRoleCodesForUser_ResolvesAssignedRoles confirms the real, end-to-
// end path: publish foundation, create two Role records and one Party
// (standing in for "the logged-in user," per UserRole's own doc comment
// on user_id carrying the Zitadel sub rather than a Core entity id),
// grant both roles via UserRole, and confirm RoleCodesForUser resolves
// both codes back — the actual building block ADR-0005 says this task
// ships (not permission enforcement itself, just this resolution).
func TestRoleCodesForUser_ResolvesAssignedRoles(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	entityDefs := data.NewEntityDefinitionRepo(tenantDB)
	def := func(entityType string) *entity.Definition {
		v, err := entityDefs.GetPublished(ctx, entityType)
		if err != nil {
			t.Fatalf("GetPublished(%s): %v", entityType, err)
		}
		d, err := entity.Unmarshal(v.Definition)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", entityType, err)
		}
		return d
	}
	engine := crud.NewEngine(tenantDB)

	financeRole, err := engine.Create(ctx, def("Role"), map[string]any{
		"code": "finance_manager", "name": "Finance Manager",
	}, actor)
	if err != nil {
		t.Fatalf("create Role finance_manager: %v", err)
	}
	warehouseRole, err := engine.Create(ctx, def("Role"), map[string]any{
		"code": "warehouse_supervisor", "name": "Warehouse Supervisor",
	}, actor)
	if err != nil {
		t.Fatalf("create Role warehouse_supervisor: %v", err)
	}
	// A third Role, deliberately never granted to anyone — proves
	// RoleCodesForUser filters by user_id, not just "every Role that
	// exists."
	if _, err := engine.Create(ctx, def("Role"), map[string]any{
		"code": "sales_rep", "name": "Sales Rep",
	}, actor); err != nil {
		t.Fatalf("create Role sales_rep: %v", err)
	}

	const zitadelSub = "zitadel-sub-abc123"
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": zitadelSub, "role_id": financeRole.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (finance): %v", err)
	}
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": zitadelSub, "role_id": warehouseRole.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (warehouse): %v", err)
	}
	// A different user, granted a role that must NOT show up in the
	// first user's result.
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": "zitadel-sub-someone-else", "role_id": financeRole.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (other user): %v", err)
	}

	codes, err := RoleCodesForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleCodesForUser: %v", err)
	}
	got := map[string]bool{}
	for _, c := range codes {
		got[c] = true
	}
	if len(got) != 2 || !got["finance_manager"] || !got["warehouse_supervisor"] {
		t.Fatalf("expected exactly [finance_manager warehouse_supervisor], got %v", codes)
	}
	if got["sales_rep"] {
		t.Fatal("expected sales_rep (never granted to this user) not to appear")
	}
}

// TestRoleCodesForUser_NoGrants_ReturnsEmptyNotError confirms a user
// with zero UserRole records (a real, valid state — a Zitadel
// tenant_member who hasn't been assigned any Core-side Role yet) is not
// an error, per RoleCodesForUser's own doc comment.
func TestRoleCodesForUser_NoGrants_ReturnsEmptyNotError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	codes, err := RoleCodesForUser(ctx, tenantDB, "zitadel-sub-nobody")
	if err != nil {
		t.Fatalf("expected no error for a user with zero grants, got %v", err)
	}
	if len(codes) != 0 {
		t.Fatalf("expected zero role codes, got %v", codes)
	}
}
