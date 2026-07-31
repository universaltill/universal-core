package foundation

import (
	"context"
	"testing"
	"time"

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

// TestRoleCodesForUserInDepartment_ScopesGrantsByDepartment confirms the
// cases R17's future approval-routing resolver needs
// (RoleCodesForUserInDepartment, roles.go): a globally-granted role
// (no department_id on the UserRole row) counts in every department, and
// a role granted scoped to department A counts in A but not in a
// different department B. Also proves finance_manager can be granted
// twice to the same user at different department scopes (Sales and HR)
// without one grant clobbering the other — see
// TestRoleGrantsForUser_SameRoleGlobalAndDepartmentScoped below for the
// specific "same role, one global one scoped" case this comment used to
// claim was covered here but wasn't (caught by independent review).
func TestRoleCodesForUserInDepartment_ScopesGrantsByDepartment(t *testing.T) {
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

	salesDept, err := engine.Create(ctx, def("Department"), map[string]any{
		"code": "sales", "name": "Sales",
	}, actor)
	if err != nil {
		t.Fatalf("create Department sales: %v", err)
	}
	hrDept, err := engine.Create(ctx, def("Department"), map[string]any{
		"code": "hr", "name": "HR",
	}, actor)
	if err != nil {
		t.Fatalf("create Department hr: %v", err)
	}
	financeRole, err := engine.Create(ctx, def("Role"), map[string]any{
		"code": "finance_manager", "name": "Finance Manager",
	}, actor)
	if err != nil {
		t.Fatalf("create Role finance_manager: %v", err)
	}
	globalRole, err := engine.Create(ctx, def("Role"), map[string]any{
		"code": "auditor", "name": "Auditor",
	}, actor)
	if err != nil {
		t.Fatalf("create Role auditor: %v", err)
	}

	const zitadelSub = "zitadel-sub-dept-scoped"
	// finance_manager, scoped to Sales only.
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": zitadelSub, "role_id": financeRole.ID, "department_id": salesDept.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (finance, sales-scoped): %v", err)
	}
	// finance_manager AGAIN, this time scoped to HR — same user, same
	// role, different department; both grants must survive.
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": zitadelSub, "role_id": financeRole.ID, "department_id": hrDept.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (finance, hr-scoped): %v", err)
	}
	// auditor, granted globally (no department_id).
	if _, err := engine.Create(ctx, def("UserRole"), map[string]any{
		"user_id": zitadelSub, "role_id": globalRole.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (auditor, global): %v", err)
	}

	salesCodes, err := RoleCodesForUserInDepartment(ctx, tenantDB, zitadelSub, salesDept.ID)
	if err != nil {
		t.Fatalf("RoleCodesForUserInDepartment(sales): %v", err)
	}
	gotSales := map[string]bool{}
	for _, c := range salesCodes {
		gotSales[c] = true
	}
	if !gotSales["finance_manager"] {
		t.Fatalf("expected finance_manager (sales-scoped) to resolve in Sales, got %v", salesCodes)
	}
	if !gotSales["auditor"] {
		t.Fatalf("expected auditor (global grant) to resolve in Sales too, got %v", salesCodes)
	}

	otherDept, err := engine.Create(ctx, def("Department"), map[string]any{
		"code": "ops", "name": "Operations",
	}, actor)
	if err != nil {
		t.Fatalf("create Department ops: %v", err)
	}
	opsCodes, err := RoleCodesForUserInDepartment(ctx, tenantDB, zitadelSub, otherDept.ID)
	if err != nil {
		t.Fatalf("RoleCodesForUserInDepartment(ops): %v", err)
	}
	gotOps := map[string]bool{}
	for _, c := range opsCodes {
		gotOps[c] = true
	}
	if gotOps["finance_manager"] {
		t.Fatalf("expected finance_manager (scoped to sales/hr only) NOT to resolve in Operations, got %v", opsCodes)
	}
	if !gotOps["auditor"] {
		t.Fatalf("expected auditor (global grant) to resolve in Operations too, got %v", opsCodes)
	}

	hrCodes, err := RoleCodesForUserInDepartment(ctx, tenantDB, zitadelSub, hrDept.ID)
	if err != nil {
		t.Fatalf("RoleCodesForUserInDepartment(hr): %v", err)
	}
	gotHR := map[string]bool{}
	for _, c := range hrCodes {
		gotHR[c] = true
	}
	if !gotHR["finance_manager"] {
		t.Fatalf("expected finance_manager (hr-scoped grant) to resolve in HR, got %v", hrCodes)
	}
}

// TestRoleGrantsForUser_SameRoleGlobalAndDepartmentScoped confirms the
// specific case RoleGrantsForUser's own doc comment names: the same
// user holding the same Role twice — once globally, once scoped to a
// department — with both grants surviving as distinct RoleGrant entries
// (not deduplicated into one, which would silently drop whichever
// DepartmentID lost the collision) and RoleGrant.DepartmentID itself
// readable directly, not just through RoleCodesForUserInDepartment's
// filter.
func TestRoleGrantsForUser_SameRoleGlobalAndDepartmentScoped(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	salesDept, err := engine.Create(ctx, Department(), map[string]any{
		"code": "sales", "name": "Sales",
	}, actor)
	if err != nil {
		t.Fatalf("create Department sales: %v", err)
	}
	role, err := engine.Create(ctx, Role(), map[string]any{
		"code": "finance_manager", "name": "Finance Manager",
	}, actor)
	if err != nil {
		t.Fatalf("create Role finance_manager: %v", err)
	}

	const zitadelSub = "zitadel-sub-global-and-scoped"
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": role.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (global): %v", err)
	}
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": role.ID, "department_id": salesDept.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (sales-scoped): %v", err)
	}

	grants, err := RoleGrantsForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleGrantsForUser: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 distinct grants (global + sales-scoped), got %d: %+v", len(grants), grants)
	}
	var sawGlobal, sawScoped bool
	for _, g := range grants {
		if g.Code != "finance_manager" {
			t.Fatalf("expected both grants to be finance_manager, got %q", g.Code)
		}
		switch g.DepartmentID {
		case "":
			sawGlobal = true
		case salesDept.ID:
			sawScoped = true
		default:
			t.Fatalf("unexpected DepartmentID %q", g.DepartmentID)
		}
	}
	if !sawGlobal || !sawScoped {
		t.Fatalf("expected one global grant and one Sales-scoped grant, got %+v", grants)
	}
}

// TestRoleGrantsForUser_SkipsDanglingRoleID is the regression test for
// the bug independent review found: a UserRole row whose role_id points
// at a since-deleted Role record must not resolve as a live grant.
// crud.Engine.Delete soft-deletes with no cascade to referencing rows
// (accepted, documented elsewhere in this codebase), so a dangling
// role_id is a real, reachable state, not a hypothetical — and
// internal/kernel/authz.loadRoles keys its role-membership map off
// RoleGrant.ID regardless of whether Code resolved, so an earlier draft
// of RoleGrantsForUser that stopped filtering these out silently kept
// every Permission/FieldPermission grant alive after its Role was
// deleted, undoing the admin's revocation.
func TestRoleGrantsForUser_SkipsDanglingRoleID(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	role, err := engine.Create(ctx, Role(), map[string]any{
		"code": "clerk", "name": "Clerk",
	}, actor)
	if err != nil {
		t.Fatalf("create Role clerk: %v", err)
	}
	const zitadelSub = "zitadel-sub-dangling-role"
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": role.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole: %v", err)
	}

	// Confirm the grant resolves before deletion, so the assertion after
	// deletion actually proves something changed.
	before, err := RoleCodesForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleCodesForUser (before delete): %v", err)
	}
	if len(before) != 1 || before[0] != "clerk" {
		t.Fatalf("expected [clerk] before deleting the Role, got %v", before)
	}

	if err := engine.Delete(ctx, Role(), role.ID, actor); err != nil {
		t.Fatalf("delete Role clerk: %v", err)
	}

	grants, err := RoleGrantsForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleGrantsForUser (after delete): %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected zero grants after the Role was deleted, got %+v", grants)
	}
	codes, err := RoleCodesForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleCodesForUser (after delete): %v", err)
	}
	if len(codes) != 0 {
		t.Fatalf("expected zero role codes after the Role was deleted, got %v", codes)
	}
}

// TestRoleCodesForUser_DeduplicatesRepeatedCode confirms RoleCodesForUser
// stays a set of codes (its own documented contract: "which Role.code
// values userID holds") even though RoleGrantsForUser now deliberately
// returns one entry per UserRole row — a user holding the same Role
// globally and department-scoped must not see that code twice.
func TestRoleCodesForUser_DeduplicatesRepeatedCode(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	salesDept, err := engine.Create(ctx, Department(), map[string]any{
		"code": "sales", "name": "Sales",
	}, actor)
	if err != nil {
		t.Fatalf("create Department sales: %v", err)
	}
	role, err := engine.Create(ctx, Role(), map[string]any{
		"code": "clerk", "name": "Clerk",
	}, actor)
	if err != nil {
		t.Fatalf("create Role clerk: %v", err)
	}
	const zitadelSub = "zitadel-sub-dedup"
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": role.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (global): %v", err)
	}
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": role.ID, "department_id": salesDept.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (sales-scoped): %v", err)
	}

	codes, err := RoleCodesForUser(ctx, tenantDB, zitadelSub)
	if err != nil {
		t.Fatalf("RoleCodesForUser: %v", err)
	}
	if len(codes) != 1 || codes[0] != "clerk" {
		t.Fatalf("expected exactly [clerk] (deduplicated), got %v", codes)
	}
}

// TestRoleCodesForUserInDepartment_EmptyDepartmentIDIsFailClosed confirms
// the edge case a future approval-routing caller reaches when it
// couldn't resolve a requester's department at all: passing "" as
// departmentID must NOT be treated as "match every department" (that
// would make an unresolvable department silently as permissive as a
// global grant's own semantics from the other direction) — only
// genuinely global grants (DepartmentID == "") match, exactly as if ""
// were an ordinary, if unusual, department id with no scoped grants of
// its own.
func TestRoleCodesForUserInDepartment_EmptyDepartmentIDIsFailClosed(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()
	actor := humanActor()

	if err := Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	engine := crud.NewEngine(tenantDB)

	salesDept, err := engine.Create(ctx, Department(), map[string]any{
		"code": "sales", "name": "Sales",
	}, actor)
	if err != nil {
		t.Fatalf("create Department sales: %v", err)
	}
	globalRole, err := engine.Create(ctx, Role(), map[string]any{
		"code": "auditor", "name": "Auditor",
	}, actor)
	if err != nil {
		t.Fatalf("create Role auditor: %v", err)
	}
	scopedRole, err := engine.Create(ctx, Role(), map[string]any{
		"code": "finance_manager", "name": "Finance Manager",
	}, actor)
	if err != nil {
		t.Fatalf("create Role finance_manager: %v", err)
	}

	const zitadelSub = "zitadel-sub-empty-department"
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": globalRole.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (global): %v", err)
	}
	if _, err := engine.Create(ctx, UserRole(), map[string]any{
		"user_id": zitadelSub, "role_id": scopedRole.ID, "department_id": salesDept.ID,
	}, actor); err != nil {
		t.Fatalf("create UserRole (sales-scoped): %v", err)
	}

	codes, err := RoleCodesForUserInDepartment(ctx, tenantDB, zitadelSub, "")
	if err != nil {
		t.Fatalf("RoleCodesForUserInDepartment(\"\"): %v", err)
	}
	got := map[string]bool{}
	for _, c := range codes {
		got[c] = true
	}
	if !got["auditor"] {
		t.Fatalf("expected the global grant to still resolve with an empty departmentID, got %v", codes)
	}
	if got["finance_manager"] {
		t.Fatalf("expected the Sales-scoped grant NOT to resolve against an empty departmentID, got %v", codes)
	}
}

// TestRoleCodesForUserInDepartment_NoGrants_ReturnsEmptyNotError mirrors
// TestRoleCodesForUser_NoGrants_ReturnsEmptyNotError for the department-
// scoped resolver — same nil-not-error contract on its own doc comment.
func TestRoleCodesForUserInDepartment_NoGrants_ReturnsEmptyNotError(t *testing.T) {
	tenantDB := freshTenantDB(t)
	ctx := context.Background()

	if err := Publish(ctx, tenantDB, humanActor()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	codes, err := RoleCodesForUserInDepartment(ctx, tenantDB, "zitadel-sub-nobody", "dept-does-not-matter")
	if err != nil {
		t.Fatalf("expected no error for a user with zero grants, got %v", err)
	}
	if len(codes) != 0 {
		t.Fatalf("expected zero role codes, got %v", codes)
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

// Department-scoped resolution is what R17 routing consumes: a user may
// hold the same role in several departments, and an approval scoped to
// department X must only see the grant for X.
func TestRoleCodesForUserInDepartment_ScopesToTheGrantsDepartment(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	fin, err := eng.Create(ctx, Role(), map[string]any{"code": "finance_manager", "name": "FM"}, actor)
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	// Same user holds finance_manager in dept-A but NOT dept-B.
	if _, err := eng.Create(ctx, UserRole(), map[string]any{
		"user_id": "u1", "role_id": fin.ID, "department_id": "dept-A",
	}, actor); err != nil {
		t.Fatalf("grant: %v", err)
	}

	inA, err := RoleCodesForUserInDepartment(ctx, db, "u1", "dept-A")
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	if !containsCode(inA, "finance_manager") {
		t.Fatalf("u1 should hold finance_manager in dept-A, got %v", inA)
	}
	inB, err := RoleCodesForUserInDepartment(ctx, db, "u1", "dept-B")
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	if containsCode(inB, "finance_manager") {
		t.Fatalf("u1 must NOT hold finance_manager in dept-B — the grant was scoped to A, got %v", inB)
	}
}

func containsCode(xs []string, w string) bool {
	for _, x := range xs {
		if x == w {
			return true
		}
	}
	return false
}

// TestActiveDelegatorsFor_ResolvesActiveDelegation is the basic case: a
// Delegation with no ends_at and not revoked is active indefinitely, and
// ActiveDelegatorsFor(delegate) returns the delegator's id.
func TestActiveDelegatorsFor_ResolvesActiveDelegation(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob",
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "bob")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if !containsCode(delegators, "alice") {
		t.Fatalf("expected bob's active delegators to include alice, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_RevokedIsExcluded confirms revoked=true is an
// immediate off switch — the delegation row still exists (this
// package's history-preserving design, see Delegation's own doc
// comment) but grants nothing.
func TestActiveDelegatorsFor_RevokedIsExcluded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob", "revoked": true,
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "bob")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if containsCode(delegators, "alice") {
		t.Fatalf("a revoked delegation must not grant anything, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_ExpiredEndsAtIsExcluded confirms a delegation
// whose ends_at is in the past no longer counts as active.
func TestActiveDelegatorsFor_ExpiredEndsAtIsExcluded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob", "ends_at": yesterday,
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "bob")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if containsCode(delegators, "alice") {
		t.Fatalf("a delegation past its ends_at must not grant anything, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_TodayEndsAtIsStillActive confirms ends_at is
// inclusive through the end of that calendar day, not exclusive.
func TestActiveDelegatorsFor_TodayEndsAtIsStillActive(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	today := time.Now().Format("2006-01-02")
	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob", "ends_at": today,
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "bob")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if !containsCode(delegators, "alice") {
		t.Fatalf("a delegation ending today should still be active through end of day, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_MalformedEndsAtIsExcluded confirms a value that
// doesn't parse as a date fails closed (excluded, not indefinite) — the
// same "wrong direction to fail on an access grant" posture
// RoleCodesForUserInDepartment's own empty-department handling takes.
func TestActiveDelegatorsFor_MalformedEndsAtIsExcluded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob", "ends_at": "not-a-date",
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "bob")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if containsCode(delegators, "alice") {
		t.Fatalf("a malformed ends_at must fail closed (excluded), got %v", delegators)
	}
}

// TestActiveDelegatorsFor_SelfDelegationExcluded confirms a delegation
// naming the same user as both delegator and delegate never appears in
// the returned set — see Delegation's own doc comment on why this is
// inert rather than rejected.
func TestActiveDelegatorsFor_SelfDelegationExcluded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "alice",
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "alice")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if containsCode(delegators, "alice") {
		t.Fatalf("self-delegation must not appear in the returned set, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_NotTargetingThisUserIsExcluded confirms a
// delegation naming a different delegate doesn't leak into an unrelated
// user's result.
func TestActiveDelegatorsFor_NotTargetingThisUserIsExcluded(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()
	eng := crud.NewEngine(db)
	actor := humanActor()

	if _, err := eng.Create(ctx, Delegation(), map[string]any{
		"delegator_user_id": "alice", "delegate_user_id": "bob",
	}, actor); err != nil {
		t.Fatalf("create Delegation: %v", err)
	}

	delegators, err := ActiveDelegatorsFor(ctx, db, "carol")
	if err != nil {
		t.Fatalf("ActiveDelegatorsFor: %v", err)
	}
	if len(delegators) != 0 {
		t.Fatalf("carol has no delegations naming her, expected none, got %v", delegators)
	}
}

// TestActiveDelegatorsFor_NoDelegations_ReturnsEmptyNotError matches the
// nil-not-error-on-zero-grants contract every other resolver in this
// file establishes.
func TestActiveDelegatorsFor_NoDelegations_ReturnsEmptyNotError(t *testing.T) {
	db := freshTenantDB(t)
	ctx := context.Background()

	delegators, err := ActiveDelegatorsFor(ctx, db, "nobody-delegated-to")
	if err != nil {
		t.Fatalf("expected no error for a user with zero delegations, got %v", err)
	}
	if len(delegators) != 0 {
		t.Fatalf("expected zero delegators, got %v", delegators)
	}
}
