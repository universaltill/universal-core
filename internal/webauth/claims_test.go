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
