package foundation

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/universaltill/universal-core/internal/data"
)

// RoleCodesForUser resolves which Role.code values userID (the Zitadel
// OIDC "sub" — the same identifier this kernel already uses as
// audit.Actor.ID, see UserRole's own doc comment and ADR-0005) currently
// holds in this tenant. Read-only.
//
// The UserRole lookup uses RecordRepo.ListByField (WHERE data->>'user_id'
// = $1, pushed into SQL) — not a full-tenant scan filtered in Go —
// because, unlike ledger.checkPeriodOpen/purchasing.
// receivedValueForPurchaseOrder (which run inside an existing *sql.Tx
// with no ListByFieldTx equivalent), this runs on a plain *sql.DB with
// no such constraint, and it's meant to become the building block a
// future per-request permission check calls (ADR-0005) — scanning every
// UserRole grant in the tenant on every call would be a real, needless
// scalability cost once that happens. Found by independent review: an
// earlier version of this function did exactly that full scan, on a
// stale "no field-level query exists yet" premise that ListByField's
// own existence (used elsewhere in this same codebase, e.g.
// cmd/seed-demo-data's seedVendorInvoices) already contradicted.
//
// The second Role lookup stays a full List + Go-side filter — the
// resolved role_id set from the first pass is already small (however
// many roles this one user holds), so pushing a second filter into SQL
// wouldn't meaningfully help.
//
// Deliberately not wired into any permission check yet (ADR-0005's own
// scoping note, uc-infra/docs/adr/0005-role-permission-model-core-
// owned.md) — this is the building block a future enforcement mechanism
// calls, not the mechanism itself. Returns nil, not an error, for a user
// with zero role grants — a real, valid state (they're a tenant member
// via Zitadel but haven't been assigned any Core-side Role yet), not a
// failure.
func RoleCodesForUser(ctx context.Context, db *sql.DB, userID string) ([]string, error) {
	grants, err := RoleGrantsForUser(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(grants))
	var codes []string
	for _, g := range grants {
		if g.Code == "" || seen[g.Code] {
			continue
		}
		seen[g.Code] = true
		codes = append(codes, g.Code)
	}
	return codes, nil
}

// RoleGrant is one resolved Role a user holds: the Role record's id
// (what Permission.role_id / FieldPermission.role_id reference) plus
// its code (what conventions like ADR-0006's tenant_admin override key
// on). internal/kernel/authz needs both per request, so they resolve
// together rather than in two round-trip sets of queries.
//
// DepartmentID is the UserRole grant row's own department_id (v2, see
// UserRole's doc comment) — empty means a global grant (every
// department), not "department unknown". Populated here so a caller
// that already has RoleGrantsForUser's result can filter by department
// itself without a second query; RoleCodesForUserInDepartment below is
// the same filter as a convenience wrapper for the common case.
type RoleGrant struct {
	ID           string
	Code         string
	DepartmentID string
}

// RoleGrantsForUser resolves every Role userID holds in this tenant —
// same contract as RoleCodesForUser (which is now a thin projection of
// this): read-only, nil-not-error for a user with zero grants.
//
// Returns one RoleGrant per UserRole row whose role_id still resolves to
// a live Role record — not deduplicated by role_id, so a user can hold
// the same Role twice with different department scope (e.g. "Finance
// Manager" granted globally via one UserRole row and again scoped to a
// specific department via another) without collapsing those into one
// grant and silently dropping whichever DepartmentID lost the
// collision. A UserRole row whose role_id no longer resolves (the Role
// record was deleted; crud.Engine.Delete soft-deletes with no cascade,
// so this is a real, reachable state) is skipped entirely, same as the
// pre-department-scoping version of this function did by construction —
// found by independent review: an earlier draft of this rewrite dropped
// that filter, which meant deleting a Role silently stopped revoking
// the Permission/FieldPermission grants it had authorized, since
// internal/kernel/authz.loadRoles keys off RoleGrant.ID regardless of
// whether Code resolved. Existing callers (authz's r.roleIDs map,
// RoleCodesForUser's linear scans) are dedup-tolerant for the
// same-role-twice case this function deliberately allows through — that
// part of the original claim was verified correct, just not the
// dangling-role-id case, which this filter closes.
func RoleGrantsForUser(ctx context.Context, db *sql.DB, userID string) ([]RoleGrant, error) {
	records := data.NewRecordRepo(db)

	userRoles, err := records.ListByField(ctx, "UserRole", "user_id", userID)
	if err != nil {
		return nil, fmt.Errorf("list UserRole by user_id: %w", err)
	}
	type seed struct {
		roleID       string
		departmentID string
	}
	roleIDs := make(map[string]bool)
	seeds := make([]seed, 0, len(userRoles))
	for _, ur := range userRoles {
		rid, _ := ur.Data["role_id"].(string)
		if rid == "" {
			continue
		}
		deptID, _ := ur.Data["department_id"].(string)
		roleIDs[rid] = true
		seeds = append(seeds, seed{roleID: rid, departmentID: deptID})
	}
	if len(seeds) == 0 {
		return nil, nil
	}

	roles, err := records.List(ctx, "Role")
	if err != nil {
		return nil, fmt.Errorf("list Role: %w", err)
	}
	codeByID := make(map[string]string, len(roleIDs))
	for _, r := range roles {
		if roleIDs[r.ID] {
			code, _ := r.Data["code"].(string)
			codeByID[r.ID] = code
		}
	}

	var grants []RoleGrant
	for _, s := range seeds {
		code, ok := codeByID[s.roleID]
		if !ok {
			// role_id points at a Role record that no longer exists —
			// exclude it rather than emit a grant with an empty Code,
			// the same filter records.List's roleIDs check applied
			// implicitly before this function tracked per-row
			// department scope.
			continue
		}
		grants = append(grants, RoleGrant{ID: s.roleID, Code: code, DepartmentID: s.departmentID})
	}
	return grants, nil
}

// RoleCodesForUserInDepartment resolves which Role.code values userID
// holds either globally (a UserRole grant with no department_id) or
// scoped to departmentID specifically — the filter R17's department-
// based approval routing needs once it can resolve "the requester's
// department" (still separate, open work — see UserRole's doc comment).
// A grant scoped to a *different* department does not count. Same
// nil-not-error-on-zero-grants contract as RoleCodesForUser.
func RoleCodesForUserInDepartment(ctx context.Context, db *sql.DB, userID, departmentID string) ([]string, error) {
	grants, err := RoleGrantsForUser(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	var codes []string
	for _, g := range grants {
		if g.Code == "" {
			continue
		}
		if g.DepartmentID == "" || g.DepartmentID == departmentID {
			codes = append(codes, g.Code)
		}
	}
	return codes, nil
}

// ActiveDelegatorsFor resolves every userID who currently has an active
// Delegation (foundation.go) naming delegateUserID as delegate_user_id —
// i.e. every user whose own approval standing delegateUserID may also
// act with. The only caller is internal/api/workflow.go's
// userMeetsApproval, which checks a delegator's OWN role grants for
// each id this returns — this function decides WHO counts as an active
// delegator, not whether that standing actually satisfies any particular
// approval requirement.
//
// "Active" means not revoked and, when ends_at is set, not yet past it
// (ends_at is inclusive through the end of that calendar day, evaluated
// against the server's own local calendar — a delegation ending
// "2026-08-05" is still active for any check made before local midnight
// rolls over into the 6th). An absent ends_at is an indefinite
// delegation until explicitly revoked (see Delegation's own doc
// comment). A malformed ends_at value is treated as expired (excluded),
// not indefinite — fail-closed, same posture RoleCodesForUserInDepartment's
// own "unset department routes to nobody" rule takes for a grant that
// can't be resolved cleanly: the wrong direction to fail on an access
// grant is the direction that grants more. The cutoff is deliberately
// parsed IN the server's local location (time.ParseInLocation, not the
// UTC time.Parse would default to) so it lines up with the time.Now()
// it's compared against — independent review caught the first draft
// comparing a UTC midnight cutoff against a local-clock "now," which
// fails open by up to ~14 hours in any tenant deployed east of UTC.
//
// Self-delegation (delegator_user_id == delegateUserID) is filtered out
// here too — seeing it in the returned set would cost the caller nothing
// incorrect (checking a user's own codes against themselves a second
// time is a harmless no-op), but excluding it keeps this function's
// contract — "who ELSE delegated to this user" — honest. Duplicate
// delegator ids (two active Delegation rows from the same delegator) are
// also deduplicated, so a caller resolving each one's own role grants
// never pays for the same lookup twice.
func ActiveDelegatorsFor(ctx context.Context, db *sql.DB, delegateUserID string) ([]string, error) {
	if delegateUserID == "" {
		return nil, nil
	}
	records := data.NewRecordRepo(db)
	rows, err := records.ListByField(ctx, "Delegation", "delegate_user_id", delegateUserID)
	if err != nil {
		return nil, fmt.Errorf("list Delegation by delegate_user_id: %w", err)
	}
	now := time.Now()
	seen := make(map[string]bool, len(rows))
	var delegators []string
	for _, row := range rows {
		if revoked, _ := row.Data["revoked"].(bool); revoked {
			continue
		}
		if endsAt, _ := row.Data["ends_at"].(string); endsAt != "" {
			t, err := time.ParseInLocation("2006-01-02", endsAt, time.Local)
			if err != nil || !now.Before(t.AddDate(0, 0, 1)) {
				continue
			}
		}
		delegatorID, _ := row.Data["delegator_user_id"].(string)
		if delegatorID == "" || delegatorID == delegateUserID || seen[delegatorID] {
			continue
		}
		seen[delegatorID] = true
		delegators = append(delegators, delegatorID)
	}
	return delegators, nil
}
