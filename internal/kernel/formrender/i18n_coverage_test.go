package formrender_test

// i18n_coverage_test.go is the regression gate for #53 (form section
// titles / the Save action label are not localized): it enumerates every
// Section and OpSave Action across every module's AllForms() and fails
// the moment one has no real translation in one of the four locales this
// repo ships. Without this, a new module's forms.go could add a section
// with no catalog key and nothing would ever notice — TOrDefault's whole
// point is to degrade silently to the literal Title/Label, which is
// exactly the right behavior at render time but the wrong one to leave
// unchecked at CI time. This is the enforcement half of the convention
// documented in universal-core/CLAUDE.md's i18n section; that doc
// comment is the "what and why", this test is the "and it stays true".
//
// An external (_test) package, using only formrender's exported
// SectionCatalogKey/SaveActionCatalogKey — this test is standing in for
// a future module's own test asserting its forms are covered, not
// exercising anything internal to the renderer itself. No import cycle:
// neither the eight module packages it imports (foundation, sales,
// projects, crm, hr, assets, finance, purchasing) nor internal/modules
// (imported only to keep this file's module list honest — see
// TestModuleFormSets_CoversEveryRegisteredModule) imports formrender.

import (
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/modules"
)

// moduleFormSets mirrors internal/modules.Publishers (the registry a
// module must be in to be provisionable at all) plus foundation, which
// modules.Publishers deliberately excludes because it is never optional
// (modules.FoundationKey's doc comment). It calls each module's
// AllForms() directly rather than going through Publishers, whose
// PublishForms entry points write to a database this test has no use for
// — but TestModuleFormSets_CoversEveryRegisteredModule below keeps the
// two lists from drifting, the same hazard TestPublishers_
// MatchReservedModules exists for in internal/modules (those lists had
// already drifted by three modules once).
func moduleFormSets() [][]*form.Definition {
	return [][]*form.Definition{
		foundation.AllForms(),
		sales.AllForms(),
		projects.AllForms(),
		crm.AllForms(),
		hr.AllForms(),
		assets.AllForms(),
		finance.AllForms(),
		purchasing.AllForms(),
	}
}

func allModuleForms() []*form.Definition {
	var out []*form.Definition
	for _, forms := range moduleFormSets() {
		out = append(out, forms...)
	}
	return out
}

// TestModuleFormSets_CoversEveryRegisteredModule is what makes the
// coverage gate below actually binding on a FUTURE module. The module
// list in moduleFormSets is hand-written, so without this a ninth module
// could register in internal/modules.Publishers, ship an entirely
// untranslated forms.go, and TestSectionAndSaveActionCatalogCoverage
// would still pass green — it would simply never look at that module.
// A count comparison (rather than matching module keys) is all that is
// available: a form.Definition carries no module key, and AllForms is a
// per-package function, not something Publishers exposes.
func TestModuleFormSets_CoversEveryRegisteredModule(t *testing.T) {
	// +1 for foundation, which is not in Publishers.
	if got, want := len(moduleFormSets()), len(modules.Publishers)+1; got != want {
		t.Fatalf("moduleFormSets lists %d modules but internal/modules knows %d (%d optional + foundation): "+
			"a module missing from moduleFormSets is a module whose section titles this coverage gate silently never checks",
			got, want, len(modules.Publishers))
	}
}

// hasRealTranslation is HasOwn plus a non-blank check. HasOwn alone
// answers "is this key in this locale's own map", which an entry like
// "form.X.section.y": "" satisfies — and so does
// TestLocales_HaveIdenticalKeySets over in internal/i18n — while
// rendering an empty <h2> to that locale's users. That is the same shape
// of hole one level down from the one HasOwn itself exists to close, so
// close it here rather than leaving the gate able to bless a blank
// string as a translation.
func hasRealTranslation(c *i18n.Catalog, locale, key string) bool {
	return c.HasOwn(locale, key) && strings.TrimSpace(c.T(locale, key)) != ""
}

func TestSectionAndSaveActionCatalogCoverage(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	locales := []string{"en", "ar", "fa", "tr"}

	checked := 0
	// titleForKey is the collision guard: SectionCatalogKey is lossy
	// (every run of non-[a-z0-9] collapses to one underscore), so two
	// DIFFERENT Titles on the SAME EntityType — "Schedule and Budget" vs
	// "Schedule & Budget", "Lead-time stages" vs "Lead time stages" —
	// can land on one key. Nothing about that fails: both sections
	// resolve, both render the same translated text, and the wrong one
	// is only visible to someone reading the screen in a non-English
	// locale. No such pair exists today (verified across all 81 sections
	// of all 8 modules); this keeps it that way.
	titleForKey := map[string]string{}
	sawSaveAction := false
	for _, def := range allModuleForms() {
		for _, s := range def.Sections {
			if s.Title == "" {
				continue
			}
			key := formrender.SectionCatalogKey(def.EntityType, s.Title)
			if prev, ok := titleForKey[key]; ok && prev != s.Title {
				t.Errorf("section titles %q and %q of entity %q both slug to catalog key %q — "+
					"they would share one translation; rename one or give the slug rule more to work with",
					prev, s.Title, def.EntityType, key)
			}
			titleForKey[key] = s.Title
			for _, locale := range locales {
				// hasRealTranslation (HasOwn-based), not T/TOrDefault: T
				// would silently resolve through the "en" fallback the
				// moment english has the key (which the backfill always
				// gives it), so a T-based check here would never fail no
				// matter which non-English locale was missing its own
				// entry — exactly the gap this test exists to catch.
				if !hasRealTranslation(catalog, locale, key) {
					t.Errorf("no %s translation for section %q of entity %q (missing catalog key %q)",
						locale, s.Title, def.EntityType, key)
				}
			}
			checked++
		}
		for _, a := range def.Actions {
			if a.Op != form.OpSave {
				continue
			}
			sawSaveAction = true
		}
	}
	// One global key, so check it once rather than once per form per
	// locale (56 forms x 4 locales of identical failures otherwise) —
	// but only having established that a real form actually uses OpSave,
	// so this can't quietly become a check of a key nothing renders.
	if sawSaveAction {
		for _, locale := range locales {
			if !hasRealTranslation(catalog, locale, formrender.SaveActionCatalogKey) {
				t.Errorf("no %s translation for the global Save action key %q", locale, formrender.SaveActionCatalogKey)
			}
		}
	} else {
		t.Error("no OpSave action found on any module form — the Save label check below never ran")
	}
	if checked == 0 {
		t.Fatal("no sections found across any module's AllForms() — moduleFormSets is stale or every module returned nothing, either way this test isn't checking anything")
	}
}
