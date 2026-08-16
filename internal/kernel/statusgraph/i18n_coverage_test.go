package statusgraph_test

// i18n_coverage_test.go is the regression gate for uc-infra#244: it
// walks every module's StatusSpecs() and fails the moment one Spec is
// missing a real, non-blank translation for one of the three non-English
// locales this repo ships (ar/fa/tr — "en" comes from Spec.Name itself,
// required separately). Without this, a status code added to any
// module's *StatusSpecs var tomorrow — or a whole seventh module —
// ships English-only silently: Translations is optional by design
// (statusgraph.Spec's own doc comment), so nothing else stops that.
// This is the same "convention, not just a docstring" enforcement
// internal/kernel/formrender/i18n_coverage_test.go already does for form
// section titles/Save labels, and internal/help/coverage_test.go does
// for help topics — CLAUDE.md's i18n section is explicit that a
// convention without a test enforcing it isn't one.
//
// An external (_test) package, using only statusgraph's exported
// Spec/CopySpecs/StatusSpecs surface — mirrors formrender's own
// external-package choice for the identical reason: this file stands in
// for a future module's own coverage of its StatusSpecs, not something
// exercising statusgraph's internals.

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/kernel/statusgraph"
)

// allModuleStatusSpecs mirrors internal/modules.Publishers' module list
// (formrender/i18n_coverage_test.go's moduleFormSets carries the same
// note) — every module that calls PublishStatuses, keyed by
// StatusTypeCode exactly as cmd/backfill-status-name-translations'
// allStatusSpecs() merges them.
func allModuleStatusSpecs() map[string][]statusgraph.Spec {
	out := map[string][]statusgraph.Spec{}
	for _, specs := range []map[string][]statusgraph.Spec{
		purchasing.StatusSpecs(),
		sales.StatusSpecs(),
		hr.StatusSpecs(),
		projects.StatusSpecs(),
		crm.StatusSpecs(),
		assets.StatusSpecs(),
	} {
		for code, s := range specs {
			out[code] = s
		}
	}
	return out
}

// TestStatusSpecs_EveryCodeHasEveryShippedLocale is the coverage gate
// itself: every Spec across every module must have a real, non-blank
// Translations entry for each of ar/fa/tr — matching the four-locale set
// (en included, via Name) this repo ships everywhere else
// (formrender/i18n_coverage_test.go's own locales literal).
func TestStatusSpecs_EveryCodeHasEveryShippedLocale(t *testing.T) {
	locales := []string{"ar", "fa", "tr"}
	for statusTypeCode, specs := range allModuleStatusSpecs() {
		for _, s := range specs {
			if strings.TrimSpace(s.Name) == "" {
				t.Errorf("%s/%s: Name (the required English/fallback value) is blank", statusTypeCode, s.Code)
			}
			for _, locale := range locales {
				text, ok := s.Translations[locale]
				if !ok || strings.TrimSpace(text) == "" {
					t.Errorf("%s/%s: missing or blank %q translation", statusTypeCode, s.Code, locale)
				}
			}
		}
	}
}

// TestAllModuleStatusSpecs_CoversEveryStatusSpecsExportingModule keeps
// allModuleStatusSpecs (this file's own module list) from silently
// drifting behind the real set of modules calling PublishStatuses — the
// same drift hazard formrender/i18n_coverage_test.go's
// TestModuleFormSets_CoversEveryRegisteredModule and internal/modules'
// TestPublishers_MatchReservedModules both guard against. A count check
// against the six known StatusTypeCode groups (purchasing has 4, sales
// 2, hr 2, projects 2, crm 4, assets 2 — 16 total) is enough to catch a
// forgotten module without hand-listing every code again here.
func TestAllModuleStatusSpecs_CoversEveryStatusSpecsExportingModule(t *testing.T) {
	got := len(allModuleStatusSpecs())
	want := 16
	if got != want {
		t.Fatalf("expected %d StatusTypeCode groups across all modules' StatusSpecs(), got %d — a module's StatusSpecs() was added, removed, or renamed without updating this file's own module list", want, got)
	}
}
