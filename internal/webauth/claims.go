package webauth

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

// orgIDFromClaims extracts a Zitadel organization id out of the
// project-roles claim. Returns ok=false if the claim is absent or
// empty — a real Zitadel user with zero role grants in Universal
// Core's project (never added to any tenant) — Authenticator.
// handleCallback treats that the same as "no matching tenant".
//
// Also ok=false if MORE than one distinct org id is asserted (a user
// legitimately granted tenant_member in two different customer orgs —
// a shared accountant, say). Found by independent review: the original
// version returned whichever org happened to come out of Go's
// randomized map iteration first, so the same user could silently land
// in a different customer's tenant on every other sign-in — not a
// cross-tenant *leak* (they're authorized for both), but landing in
// the wrong company's data with no warning is still a real bug.
// Failing closed here (same "no matching tenant" page a zero-grant
// user gets) is the safe default until there's an actual tenant-picker
// UI to resolve the ambiguity explicitly; this package doesn't assume
// away multi-org membership, it refuses to guess.
func orgIDFromClaims(claims map[string]any) (orgID string, ok bool) {
	val, present := claims[zitadelProjectRolesClaim]
	if !present {
		return "", false
	}
	roles, isMap := val.(map[string]any)
	if !isMap {
		return "", false
	}
	found := make(map[string]bool)
	for _, orgs := range roles {
		orgMap, isMap := orgs.(map[string]any)
		if !isMap {
			continue
		}
		for id := range orgMap {
			found[id] = true
		}
	}
	if len(found) != 1 {
		return "", false
	}
	for id := range found {
		return id, true
	}
	return "", false // unreachable
}

func stringClaim(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}
