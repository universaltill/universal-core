package statusgraph_test

// catalog_parity_test.go is the regression gate for uc-infra#256:
// internal/i18n/locales/{en,ar,fa,tr}.json each carry a
// "field.{EntityType}.status_id.{code}" entry for 10 entity types
// (Campaign/Case/Employee/FixedAsset/Lead/LeaveRequest/MaintenanceOrder/
// Opportunity/Project/Task) — content that looks, by its naming
// convention, like it should feed the "field.{EntityType}.{FieldName}.
// {Value}" enum-label lookup this repo uses in several places
// (internal/api/listview.go's cellText, internal/kernel/formrender/
// render.go's two dropdown-option-label sites, internal/api/handlers.go's
// uniqueConstraintMessage). None of them ever read it for status_id:
// status_id is always entity.FieldReference on every module's
// Definition, never entity.FieldEnum, so every one of those lookup
// sites' "case entity.FieldEnum" branch is unreachable for it (and
// handlers.go's site is further gated on a UniqueWhen violation, which
// no tracked entity type declares on status_id). The actual, consumed
// source of truth for a Status record's translated label is each
// module's *StatusSpecs() (Name for "en", Translations for ar/fa/tr),
// which uc-infra#244 hand-copied from this same catalog content into Go
// literals — a second, independently-maintained copy of the same 49
// strings/locale, with nothing before this file checking they stay
// identical.
//
// This intentionally does NOT wire StatusSpecs() to read the catalog at
// runtime, or delete the now-dead catalog keys — see uc-infra#256's own
// close-out for why: the catalog content is correct and reviewed, just
// currently unread, and building a live catalog dependency into the
// generic kernel modules for a caller that doesn't need it would be
// exactly the "generalize against evidence, not speculatively" mistake
// this repo's own backlog (uc-infra#31) already calls out. A test that
// fails loudly on drift is the bounded fix for "nothing stays these two
// in sync" without taking on either of those.

import (
	"regexp"
	"sort"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// entityTypeToStatusTypeCode maps each of the 10 entity types that carry
// a field.{EntityType}.status_id.{code} catalog entry to the module
// StatusTypeCode whose StatusSpecs() is that content's real, consumed
// source today. Six of allModuleStatusSpecs()'s 16 StatusTypeCode groups
// (purchasing's 4, sales' 2) are deliberately absent — those entities
// have no field.*.status_id.* catalog keys at all (confirmed by grep
// while filing uc-infra#256), so there is nothing for this test to check
// them against.
var entityTypeToStatusTypeCode = map[string]string{
	"Employee":         "employee_status",
	"LeaveRequest":     "leave_request_status",
	"Project":          "project_status",
	"Task":             "task_status",
	"Campaign":         "campaign_status",
	"Case":             "case_status",
	"Lead":             "lead_status",
	"Opportunity":      "opportunity_stage",
	"FixedAsset":       "fixed_asset_status",
	"MaintenanceOrder": "maintenance_order_status",
}

// fieldStatusIDKeyPattern matches exactly the dead-for-rendering key
// shape uc-infra#256 is about — "field.{EntityType}.status_id.{code}" —
// not the separate, live "field.{EntityType}.status_id" field-label key
// (no trailing ".{code}") that every status_id field also has and which
// this test has nothing to say about.
var fieldStatusIDKeyPattern = regexp.MustCompile(`^field\.([A-Za-z]+)\.status_id\.([a-z0-9_]+)$`)

// TestFieldStatusIDCatalogKeysMatchStatusSpecs checks both directions:
// every field.*.status_id.* catalog entry's text must match its
// StatusSpecs() counterpart in all 4 locales (forward), and every
// StatusSpecs() code for a tracked entity type must actually appear in
// the catalog (reverse) — a code added to StatusSpecs() without a
// matching catalog key would silently pass a forward-only check, since
// that check only ever looks at keys the catalog already has.
func TestFieldStatusIDCatalogKeysMatchStatusSpecs(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	allSpecs := allModuleStatusSpecs()

	specsByEntity := make(map[string]map[string]statusgraph.Spec, len(entityTypeToStatusTypeCode))
	for entityType, statusTypeCode := range entityTypeToStatusTypeCode {
		specs, ok := allSpecs[statusTypeCode]
		if !ok {
			t.Fatalf("%s: no StatusSpecs() group for StatusTypeCode %q — entityTypeToStatusTypeCode is stale", entityType, statusTypeCode)
		}
		byCode := make(map[string]statusgraph.Spec, len(specs))
		for _, s := range specs {
			byCode[s.Code] = s
		}
		specsByEntity[entityType] = byCode
	}

	// The real shipped locale set, not a hardcoded literal — a fifth
	// locale (e.g. "de") added to internal/i18n/locales/ automatically
	// joins this check rather than silently going unverified. Available()
	// requires at least one message present, which every locale file
	// here always has; Keys("en")'s scan below is still the source of
	// which entityType+code pairs exist, since TestLocales_HaveIdentical
	// KeySets (internal/i18n/i18n_test.go) already guarantees every
	// locale carries the same key set as "en".
	locales := catalog.Available()
	seen := map[string]map[string]bool{} // entityType -> code -> catalog has it

	for _, key := range catalog.Keys("en") {
		m := fieldStatusIDKeyPattern.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		entityType, code := m[1], m[2]

		byCode, tracked := specsByEntity[entityType]
		if !tracked {
			t.Errorf("catalog has %s but %q isn't in entityTypeToStatusTypeCode — a new entity type grew field.*.status_id.* keys; add it here or confirm the key is a mistake", key, entityType)
			continue
		}
		spec, ok := byCode[code]
		if !ok {
			t.Errorf("catalog has %s but StatusSpecs()[%q] has no status code %q", key, entityTypeToStatusTypeCode[entityType], code)
			continue
		}

		if seen[entityType] == nil {
			seen[entityType] = map[string]bool{}
		}
		seen[entityType][code] = true

		for _, locale := range locales {
			want := spec.Name
			if locale != "en" {
				want = spec.Translations[locale]
			}
			// HasOwn, not T: T falls back through baseLang/the catalog
			// fallback/its own base language, so a key missing from
			// THIS locale's own file would silently compare against
			// English (or another locale) instead of failing — catching
			// that gap only by accident, whenever a translation happens
			// to differ from the fallback text it would've been
			// compared against instead. HasOwn asks the real question:
			// does this locale's own file have this key at all.
			if !catalog.HasOwn(locale, "field."+entityType+".status_id."+code) {
				t.Errorf("field.%s.status_id.%s: catalog has no %s translation of its own (checked HasOwn, not T's fallback chain)", entityType, code, locale)
				continue
			}
			got := catalog.T(locale, "field."+entityType+".status_id."+code)
			if got != want {
				t.Errorf("field.%s.status_id.%s[%s]: catalog says %q, StatusSpecs()[%q] says %q — the two copies have drifted",
					entityType, code, locale, got, entityTypeToStatusTypeCode[entityType], want)
			}
		}
	}

	// Sorted iteration so a multi-mismatch failure's error order is
	// stable across runs, not whatever Go's randomized map order gives
	// this time — a non-deterministic t.Errorf order doesn't change
	// pass/fail, but makes a CI failure harder to diff against a rerun.
	trackedEntityTypes := make([]string, 0, len(specsByEntity))
	for entityType := range specsByEntity {
		trackedEntityTypes = append(trackedEntityTypes, entityType)
	}
	sort.Strings(trackedEntityTypes)

	for _, entityType := range trackedEntityTypes {
		codes := make([]string, 0, len(specsByEntity[entityType]))
		for code := range specsByEntity[entityType] {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			if !seen[entityType][code] {
				t.Errorf("StatusSpecs()[%q] has code %q but the catalog's en.json has no field.%s.status_id.%s key",
					entityTypeToStatusTypeCode[entityType], code, entityType, code)
			}
		}
	}
}
