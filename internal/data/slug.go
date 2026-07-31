package data

import (
	"regexp"
	"strings"
)

// reservedSlugs are top-level path segments a tenant slug must never
// collide with (they'd shadow real routes once un-prefixed browser URLs
// redirect to /t/{slug}/… — ADR-0011). Kept in sync with the reserved
// list in migrations/control/0002_tenant_slug.sql's backfill.
var reservedSlugs = map[string]bool{
	"api": true, "ui": true, "t": true, "static": true, "settings": true,
	"healthz": true, "records": true, "forms": true, "modules": true,
	"import": true, "export": true, "reports": true,
	"issue-report": true, "workflow-jobs": true,
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify derives a URL-safe tenant slug from a display name — the same
// rules the 0002 backfill applies in SQL: lowercase, non-alphanumerics
// collapsed to '-', trimmed, empty → "tenant", reserved segments
// suffixed. Uniqueness is NOT handled here — CreateWithDatabase retries
// with numeric suffixes against the unique index, the only authority.
func Slugify(name string) string {
	s := strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "tenant"
	}
	if reservedSlugs[s] {
		s += "-tenant"
	}
	return s
}
