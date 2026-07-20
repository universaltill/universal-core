package webauth

import "testing"

func TestOrgIDFromClaims(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{"123456": "acme.id.universaltill.com"},
		},
	}
	orgID, ok := orgIDFromClaims(claims)
	if !ok {
		t.Fatal("expected orgIDFromClaims to succeed")
	}
	if orgID != "123456" {
		t.Fatalf("got org id %q, want 123456", orgID)
	}
}

// TestOrgIDFromClaims_MultipleDistinctOrgsFailsClosed is the regression
// test for a real bug independent review found: the original version
// returned whichever org id happened to come out of Go's randomized map
// iteration first, so a user granted tenant_member in two different
// customer orgs (a shared accountant, say) could silently land in a
// different tenant on every other sign-in. Run several times — a flaky
// pass here would mean it's still picking one at random rather than
// genuinely refusing to.
func TestOrgIDFromClaims_MultipleDistinctOrgsFailsClosed(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{
				"org-a": "acme.id.universaltill.com",
				"org-b": "beta.id.universaltill.com",
			},
		},
	}
	for range 20 {
		if _, ok := orgIDFromClaims(claims); ok {
			t.Fatal("expected ok=false when more than one distinct org is asserted, got ok=true")
		}
	}
}

// TestOrgIDFromClaims_SameOrgUnderTwoRolesStillResolves confirms the
// fix above doesn't over-correct: a user with two DIFFERENT roles that
// both happen to name the SAME org (not a real scenario yet — this
// package has only one role — but the claim shape allows it) must
// still resolve, since there's only one distinct org, not an ambiguity.
func TestOrgIDFromClaims_SameOrgUnderTwoRolesStillResolves(t *testing.T) {
	claims := map[string]any{
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{"org-a": "acme.id.universaltill.com"},
			"tenant_admin":  map[string]any{"org-a": "acme.id.universaltill.com"},
		},
	}
	orgID, ok := orgIDFromClaims(claims)
	if !ok {
		t.Fatal("expected ok=true when every asserted role points at the same single org")
	}
	if orgID != "org-a" {
		t.Fatalf("got %q, want org-a", orgID)
	}
}

func TestOrgIDFromClaims_MissingClaim(t *testing.T) {
	if _, ok := orgIDFromClaims(map[string]any{}); ok {
		t.Fatal("expected ok=false when the project-roles claim is absent (no role grants)")
	}
}

func TestOrgIDFromClaims_WrongShape(t *testing.T) {
	// A malformed/unexpected claim shape must fail closed (ok=false), not
	// panic or silently return a zero-value org id that would resolve to
	// some tenant's zitadel_org_id by coincidence.
	claims := map[string]any{zitadelProjectRolesClaim: "not a map"}
	if _, ok := orgIDFromClaims(claims); ok {
		t.Fatal("expected ok=false for a malformed claim shape")
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
