package csvimport

import (
	"io"
	"regexp"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// Headers reads just r's header row as CSV — the only thing a
// column-mapping UI needs before any mapping exists yet (ValidateMapping
// needs both headers and a mapping; there is no mapping until the
// headers are known). Reuses readCSV unchanged, so BOM-stripping and
// every other header-handling detail already reviewed there applies
// here too — this isn't a second, divergent way of reading a header row.
func Headers(r io.Reader) ([]string, error) {
	headers, _, err := readCSV(r)
	return headers, err
}

// HeadersXLSX is Headers for an .xlsx file — see readXLSX.
func HeadersXLSX(r io.Reader) ([]string, error) {
	headers, _, err := readXLSX(r)
	return headers, err
}

var normalizeNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalize maps a header or field name to a comparable key: lowercase,
// every run of non-alphanumeric characters (spaces, underscores,
// hyphens, ...) collapsed to nothing. "Vendor Name", "vendor_name", and
// "Vendor-Name" all normalize to "vendorname", so any of those column
// headers suggest-matches a field literally named vendor_name.
func normalize(s string) string {
	return normalizeNonAlnum.ReplaceAllString(strings.ToLower(s), "")
}

// SuggestMapping proposes a ColumnMapping from headers to def's fields by
// exact normalized-name match — "Vendor Name"/"vendor_name" both suggest
// the vendor_name field, but nothing fuzzier than that (no substring or
// edit-distance matching): a wrong guess a human doesn't notice is worse
// than an unmapped column a human has to fill in by hand, so this stays
// conservative. Not an AI-assisted guess (BACKLOG.md's R4 sync-engine
// vision is broader than this) — a plain, deterministic, explainable
// starting point a caller reviews and can override before Preview/Commit
// ever runs; it never bypasses ValidateMapping, which still runs on
// whatever mapping the caller ultimately submits, suggested or hand-
// edited. Only ever maps one header per field: if two headers would
// normalize-match the same field, the first one (in headers' order)
// wins and the rest are left unmapped, rather than silently producing a
// mapping ValidateMapping would reject as a duplicate-target-field error.
func SuggestMapping(headers []string, def *entity.Definition) ColumnMapping {
	fieldByNormalizedName := make(map[string]string, len(def.Fields))
	for _, f := range def.Fields {
		fieldByNormalizedName[normalize(f.Name)] = f.Name
	}

	mapping := make(ColumnMapping, len(headers))
	claimed := make(map[string]bool, len(def.Fields))
	for _, h := range headers {
		fieldName, ok := fieldByNormalizedName[normalize(h)]
		if !ok || claimed[fieldName] {
			continue
		}
		mapping[h] = fieldName
		claimed[fieldName] = true
	}
	return mapping
}
