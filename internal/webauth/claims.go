package webauth

import "sort"

// zitadelProjectRolesClaim carries {role_name: {org_id: org_domain}} in
// the id_token, asserted by turning on project_role_assertion on
// Universal Core's Zitadel project (see uc-infra/infra/terraform's
// zitadel.tf) — the exact claim shape ut-cloud/internal/webauth's own
// rolesFromClaims already relies on and has running in production,
// reused here for a different purpose: this package doesn't have RBAC
// roles yet (every tenant member gets the one "tenant_member" role,
// granted once per user via zitadel_user_grant), it's using the claim
// purely as a reliable way to learn which Zitadel org a user
// authenticated as a member of — a separate, unverified org-scoped claim
// would be a second thing to get right instead of reusing one already
// proven to arrive in the id_token correctly.
const zitadelProjectRolesClaim = "urn:zitadel:iam:org:project:roles"

// humanAccessRoles are the project roles that grant a human browser
// session access to a tenant — the contract with uc-infra's
// zitadel_project_role resources. tenant_integration is deliberately
// absent (machine-to-machine only, see orgIDsFromClaims's filter).
var humanAccessRoles = map[string]bool{"tenant_member": true}

// orgIDsFromClaims extracts every distinct Zitadel organization id out
// of the project-roles claim, sorted (stable across Go's randomized map
// iteration — the predecessor of this function, orgIDFromClaims, had a
// real bug from exactly that randomness before it learned to fail
// closed on >1 org). Empty slice when the claim is absent, malformed,
// or carries no orgs — a real Zitadel user with zero role grants in
// Universal Core's project; handleCallback routes that to the
// "no tenant linked" page.
//
// Multiple orgs are now a first-class result, not a failure: the
// fail-closed guard existed "until there's an actual tenant-picker UI
// to resolve the ambiguity explicitly" (its own words), and
// handleCallback + /ui/auth/choose are that UI (#25) — the shared
// accountant with tenant_member in two customer orgs picks a tenant
// instead of being unable to sign in at all.
func orgIDsFromClaims(claims map[string]any) []string {
	val, present := claims[zitadelProjectRolesClaim]
	if !present {
		return nil
	}
	roles, isMap := val.(map[string]any)
	if !isMap {
		return nil
	}
	found := make(map[string]bool)
	for roleKey, orgs := range roles {
		// Only HUMAN-access roles confer browser access to a tenant:
		// tenant_integration is the machine/connector role, and
		// internal/svcauth deliberately rejects a tenant_member-only
		// token on the machine path — this is the same asymmetry
		// enforced in the opposite direction (independent review,
		// 2026-07-31: without this filter, a connector-role grant in a
		// second org would put that whole tenant in a human's switcher).
		if !humanAccessRoles[roleKey] {
			continue
		}
		orgMap, isMap := orgs.(map[string]any)
		if !isMap {
			continue
		}
		for id := range orgMap {
			found[id] = true
		}
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func stringClaim(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}
