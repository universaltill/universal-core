package ids

import "testing"

// TestCanonical is a table test over the input shapes Postgres's uuid_in
// actually accepts/rejects (verified against a live Postgres during
// uc-infra#107's review) — the format-coverage gap that review flagged
// as where a too-strict regex should have been caught.
func TestCanonical(t *testing.T) {
	const want = "550e8400-e29b-41d4-a716-446655440000"

	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"canonical lowercase", "550e8400-e29b-41d4-a716-446655440000", want, true},
		{"uppercase", "550E8400-E29B-41D4-A716-446655440000", want, true},
		{"mixed case", "550e8400-E29B-41d4-A716-446655440000", want, true},
		{"brace-wrapped", "{550e8400-e29b-41d4-a716-446655440000}", want, true},
		{"no hyphens", "550e8400e29b41d4a716446655440000", want, true},
		{"hyphens in other positions", "550e8400-e29b41d4-a716446655440000", want, true},
		{"empty string", "", "", false},
		{"urn-prefixed (Postgres also rejects this)", "urn:uuid:550e8400-e29b-41d4-a716-446655440000", "", false},
		{"too short", "550e8400-e29b-41d4-a716-44665544000", "", false},
		{"too long", "550e8400-e29b-41d4-a716-4466554400001", "", false},
		{"non-hex characters", "zzze8400-e29b-41d4-a716-446655440000", "", false},
		{"arbitrary garbage", "not-a-uuid-at-all", "", false},
		{"legacy external-import key", "legacy-key-42", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Canonical(tc.input)
			if ok != tc.ok {
				t.Fatalf("Canonical(%q) ok = %v, want %v", tc.input, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("Canonical(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
