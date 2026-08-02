// Package ids is a leaf kernel package for the one thing several packages
// each need in isolation: recognizing whether a caller-supplied string
// could possibly be a records.id (every records/tenants primary key is a
// Postgres uuid column, `DEFAULT gen_random_uuid()`). It exists so that
// need doesn't keep getting reinvented per-package with a stricter check
// than Postgres's own uuid_in actually enforces (uc-infra#107's review:
// a regex requiring the canonical 8-4-4-4-12 hyphenated form rejects
// several forms uuid_in accepts — no hyphens, brace-wrapped, hyphens in
// other positions — so a value that IS a real records.id could be
// wrongly treated as "can never match", silently disabling whatever
// check was guarding a raw ::uuid cast with it).
package ids

import "strings"

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// Canonical normalizes s to the lowercase hyphenated form (8-4-4-4-12)
// Postgres's uuid_out always returns, accepting the same input shapes
// uuid_in does — optional surrounding braces, optional hyphens in any
// position, either case — and reports ok=false only for a string that
// can never be a valid uuid literal (records.id's actual column type),
// the same condition that would otherwise surface as a raw Postgres
// "invalid input syntax for type uuid" driver error. Two different
// spellings of the same id canonicalize to the same string, so a caller
// comparing IDs (equality, a visited-set, cycle detection) is comparing
// values, not spellings — see uc-infra#107's review for why that
// distinction is load-bearing, not cosmetic.
func Canonical(s string) (string, bool) {
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) != 32 {
		return "", false
	}
	for i := 0; i < len(hex); i++ {
		if !isHexDigit(hex[i]) {
			return "", false
		}
	}
	hex = strings.ToLower(hex)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32], true
}
