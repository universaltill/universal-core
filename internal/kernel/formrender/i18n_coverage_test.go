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
	"github.com/universaltill/universal-core/internal/kernel/entity"
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

// entityByType indexes every module's All() (the same entity.Definition
// lists Publish seeds into a tenant) by EntityType, for #99's coverage
// gate below to resolve a master_detail/related_list section's Target
// (or a RollUp's parent entity) to the Definition whose Fields it needs
// to walk. Mirrors moduleFormSets' module list exactly — kept as a
// separate literal rather than derived from it because entity.All() and
// form.AllForms() are two different per-module functions with no shared
// registry to draw both from.
func entityByType() map[string]*entity.Definition {
	sets := [][]*entity.Definition{
		foundation.All(),
		sales.All(),
		projects.All(),
		crm.All(),
		hr.All(),
		assets.All(),
		finance.All(),
		purchasing.All(),
	}
	out := make(map[string]*entity.Definition)
	for _, defs := range sets {
		for _, d := range defs {
			out[d.EntityType] = d
		}
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

// Also covers every non-Save Action's Label (formrender.ActionCatalogKey),
// added for uc-infra#66's "Download UBL file" navigate action — the
// first production Definition to use a non-Save action, so this is the
// first run where that half of the gate has anything real to check.
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
	// labelForKey is ActionCatalogKey's own version of titleForKey's
	// collision guard, for the same reason: two DIFFERENT Labels on the
	// SAME EntityType could slug to one key and silently share a
	// translation. Checked alongside sections in one pass rather than a
	// second loop over allModuleForms() — same data, same reasoning.
	labelForKey := map[string]string{}
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
			if a.Op == form.OpSave {
				sawSaveAction = true
				continue
			}
			// Every non-Save action (uc-infra#66's "Download UBL file"
			// is the first production one) resolves through
			// ActionCatalogKey, formrender.ActionCatalogKey's own doc
			// comment — the per-entity-and-action key
			// SaveActionCatalogKey's doc comment flagged as the thing
			// to add "when a real form actually needs one". Without
			// this half of the gate, a future action could ship with a
			// Label and no translation and nothing would ever notice,
			// exactly the hole this whole test exists to close for
			// sections.
			key := formrender.ActionCatalogKey(def.EntityType, a.Label)
			if prev, ok := labelForKey[key]; ok && prev != a.Label {
				t.Errorf("action labels %q and %q of entity %q both slug to catalog key %q — "+
					"they would share one translation; rename one or give the slug rule more to work with",
					prev, a.Label, def.EntityType, key)
			}
			labelForKey[key] = a.Label
			for _, locale := range locales {
				if !hasRealTranslation(catalog, locale, key) {
					t.Errorf("no %s translation for action %q (op %s) of entity %q (missing catalog key %q)",
						locale, a.Label, a.Op, def.EntityType, key)
				}
			}
			checked++
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

// TestMasterDetailAndRollUpCatalogCoverage is #99's own coverage gate,
// the same reason TestSectionAndSaveActionCatalogCoverage exists for
// #53: without it, a module could add a master_detail/related_list
// section (or a RollUp) whose child fields or roll-up target have no
// field.{EntityType}.{FieldName} translation, and buildChildRows/the
// roll-up label would silently degrade to the raw field name in every
// locale with nothing to notice — exactly how GoodsReceiptLine's four
// fields shipped untranslated in the first version of this fix (caught
// by independent review, not by any test, which is why this exists now).
func TestMasterDetailAndRollUpCatalogCoverage(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	locales := []string{"en", "ar", "fa", "tr"}
	entities := entityByType()

	checkedFields, checkedRollUps := 0, 0
	seenKeys := map[string]bool{}
	for _, def := range allModuleForms() {
		for _, s := range def.Sections {
			if s.Component != form.ComponentMasterDetail && s.Component != form.ComponentRelatedList {
				continue
			}
			childDef, ok := entities[s.Target]
			if !ok {
				t.Errorf("section %q of entity %q targets %q, which has no Definition in entityByType — "+
					"either a stale Target or entityByType's module list needs updating", s.Title, def.EntityType, s.Target)
				continue
			}
			for _, f := range childDef.Fields {
				key := "field." + childDef.EntityType + "." + f.Name
				if !seenKeys[key] {
					seenKeys[key] = true
					for _, locale := range locales {
						if !hasRealTranslation(catalog, locale, key) {
							t.Errorf("no %s translation for child column %q of entity %q (missing catalog key %q, "+
								"rendered by a master_detail/related_list section on %q)",
								locale, f.Name, childDef.EntityType, key, def.EntityType)
						}
					}
					checkedFields++
				}
				// A FieldEnum child column's CELL value (not just its
				// header) resolves through "field.{Entity}.{Field}.
				// {Value}" too (formrender.childCellValue, uc-infra#79) —
				// same convention buildFields' <select> options and
				// listview.go's list-view cells already use. Uncaught
				// before this, this loop only checked the header key
				// above; a future module's enum child column could ship
				// with the header translated and every row's actual
				// value silently un-translated in every locale.
				if f.Type != entity.FieldEnum {
					continue
				}
				for _, ev := range f.EnumValues {
					key := "field." + childDef.EntityType + "." + f.Name + "." + ev
					if seenKeys[key] {
						continue
					}
					seenKeys[key] = true
					for _, locale := range locales {
						if !hasRealTranslation(catalog, locale, key) {
							t.Errorf("no %s translation for child column %q's enum value %q of entity %q (missing catalog key %q, "+
								"rendered by a master_detail/related_list section on %q)",
								locale, f.Name, ev, childDef.EntityType, key, def.EntityType)
						}
					}
					checkedFields++
				}
			}

			if s.RollUp == "" {
				continue
			}
			key := "field." + def.EntityType + "." + s.RollUpTarget
			if seenKeys[key] {
				continue
			}
			seenKeys[key] = true
			for _, locale := range locales {
				if !hasRealTranslation(catalog, locale, key) {
					t.Errorf("no %s translation for roll-up target %q of entity %q (missing catalog key %q)",
						locale, s.RollUpTarget, def.EntityType, key)
				}
			}
			checkedRollUps++
		}
	}
	if checkedFields == 0 {
		t.Fatal("no master_detail/related_list child columns found across any module's AllForms() — this test isn't checking anything")
	}
	if checkedRollUps == 0 {
		t.Fatal("no RollUp sections found across any module's AllForms() — the roll-up label check below never ran")
	}
}
