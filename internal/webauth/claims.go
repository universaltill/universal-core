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
// project-roles claim — the first (only, in Universal Core's case: one
// role, one grant) org key under any asserted role. Returns ok=false if
// the claim is absent or empty, which happens for a real Zitadel user
// with zero role grants in Universal Core's project (never added to any
// tenant) — Authenticator.handleCallback treats that the same as "no
// matching tenant", not a different error, since neither case has
// anywhere to route the user.
func orgIDFromClaims(claims map[string]any) (orgID string, ok bool) {
	val, present := claims[zitadelProjectRolesClaim]
	if !present {
		return "", false
	}
	roles, isMap := val.(map[string]any)
	if !isMap {
		return "", false
	}
	for _, orgs := range roles {
		orgMap, isMap := orgs.(map[string]any)
		if !isMap {
			continue
		}
		for id := range orgMap {
			return id, true
		}
	}
	return "", false
}

func stringClaim(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}
