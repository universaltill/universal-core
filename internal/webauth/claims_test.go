package webauth

import (
	"slices"
	"testing"
)

func TestOrgIDsFromClaims(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{"123456": "acme.id.universaltill.com"},
		},
	}
	if got := orgIDsFromClaims(claims); !slices.Equal(got, []string{"123456"}) {
		t.Fatalf("got %v, want [123456]", got)
	}
}

// TestOrgIDsFromClaims_MultipleDistinctOrgsStableOrder is the successor
// of the old fail-closed regression test: the predecessor
// (orgIDFromClaims) refused to pick between two orgs because Go's
// randomized map iteration once made it land users in a different
// tenant on alternate sign-ins. Multi-org is now a first-class result
// feeding the tenant picker (#25) — the property that survives is
// DETERMINISM: the same claims must yield the same slice, in the same
// (sorted) order, every time. Run several times, same reasoning as the
// original: a flaky pass would mean map-iteration order is leaking
// through again.
func TestOrgIDsFromClaims_MultipleDistinctOrgsStableOrder(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{
				"org-b": "beta.id.universaltill.com",
				"org-a": "acme.id.universaltill.com",
			},
		},
	}
	want := []string{"org-a", "org-b"}
	for range 20 {
		if got := orgIDsFromClaims(claims); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v (stable sorted order)", got, want)
		}
	}
}

// TestOrgIDsFromClaims_SameOrgUnderTwoRolesDeduplicates: a human role
// plus another (here filtered-anyway) role naming the same org is one
// accessible org, not two picker entries.
func TestOrgIDsFromClaims_SameOrgUnderTwoRolesDeduplicates(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member":      map[string]any{"org-a": "acme.id.universaltill.com"},
			"tenant_integration": map[string]any{"org-a": "acme.id.universaltill.com"},
		},
	}
	if got := orgIDsFromClaims(claims); !slices.Equal(got, []string{"org-a"}) {
		t.Fatalf("got %v, want [org-a]", got)
	}
}

func TestOrgIDsFromClaims_MissingClaim(t *testing.T) {
	if got := orgIDsFromClaims(map[string]any{}); len(got) != 0 {
		t.Fatalf("expected no orgs when the project-roles claim is absent, got %v", got)
	}
}

func TestOrgIDsFromClaims_WrongShape(t *testing.T) {
	// A malformed/unexpected claim shape must yield nothing (routed to
	// the not-linked page), not panic or return a zero-value org id that
	// might resolve to some tenant's zitadel_org_id by coincidence.
	claims := map[string]any{zitadelProjectRolesClaim: "not a map"}
	if got := orgIDsFromClaims(claims); len(got) != 0 {
		t.Fatalf("expected no orgs for a malformed claim shape, got %v", got)
	}
}

// TestOrgIDsFromClaims_RoleValueWrongShape: the outer claim is a real
// map, but one role's own value isn't the expected {org_id: domain}
// map — skipped, not a panic; a well-formed sibling role still counts.
func TestOrgIDsFromClaims_RoleValueWrongShape(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{"tenant_member": "not a map either"},
	}
	if got := orgIDsFromClaims(claims); len(got) != 0 {
		t.Fatalf("expected no orgs when a role's own value isn't a map, got %v", got)
	}
}

// TestOrgIDsFromClaims_MachineRoleFiltered: a tenant_integration grant
// (the machine/connector role) must NOT put its org into a human's
// accessible set — the browser-path mirror of svcauth rejecting a
// tenant_member-only token on the machine path (independent review,
// 2026-07-31).
func TestOrgIDsFromClaims_MachineRoleFiltered(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member":      map[string]any{"org-a": "acme.id.universaltill.com"},
			"tenant_integration": map[string]any{"org-b": "beta.id.universaltill.com"},
		},
	}
	if got := orgIDsFromClaims(claims); !slices.Equal(got, []string{"org-a"}) {
		t.Fatalf("tenant_integration's org must be filtered out; got %v, want [org-a]", got)
	}
	onlyMachine := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_integration": map[string]any{"org-b": "beta.id.universaltill.com"},
		},
	}
	if got := orgIDsFromClaims(onlyMachine); len(got) != 0 {
		t.Fatalf("a machine-role-only claim must yield no human-accessible orgs, got %v", got)
	}
}

func TestStringClaim(t *testing.T) {
	claims := map[string]any{"name": "Ada Lovelace", "count": 5}
	if got := stringClaim(claims, "name"); got != "Ada Lovelace" {
		t.Fatalf("got %q", got)
	}
	if got := stringClaim(claims, "count"); got != "" {
		t.Fatalf("expected empty string for a non-string claim, got %q", got)
	}
	if got := stringClaim(claims, "missing"); got != "" {
		t.Fatalf("expected empty string for a missing claim, got %q", got)
	}
}
