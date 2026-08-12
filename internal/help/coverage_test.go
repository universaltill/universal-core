package help_test

// coverage_test.go is the regression gate for uc-infra#146: it walks
// every shipped Entity and Form Definition across every module and
// fails when one has no help-manual topic — in en, or (checked
// separately, per locale, no fallback) in any of the other three
// shipped locales — in the same shape
// internal/kernel/formrender/i18n_coverage_test.go already uses for
// section-title/Save-label translation coverage.
//
// This is deliberately an external (_test) package: internal/help
// itself cannot import the eight kernel module packages here without
// creating an import cycle (formrender, which those modules'
// generic-renderer call paths run through, already imports
// internal/help — see help.go's own TopicID doc comment on why the
// dependency only runs that one direction). A _test package is a
// separate compiled test binary, not something formrender or the
// modules import, so it can safely import all eight without cycling
// anything — the same reasoning i18n_coverage_test.go's own header
// already documents for its own import list.
//
// Turning this gate on cold would fail the build for all 61 currently-
// shipped entity types at once — no content exists yet (help.go's own
// HasContent doc comment). undocumentedAllowlist below is the
// documented, shrinking exception list that lets the gate ship live
// today without that mass failure: uc-infra#147-152's content slices
// each remove their module's entries as real topics land.
//
// The allowlist may only ever shrink — but that direction-of-travel
// rule is enforced OUTSIDE this file, not by a second copy of the map
// living in it: .github/scripts/check-help-allowlist.sh diffs this
// file's undocumentedAllowlist key set against the same file at the
// PR's base ref (ci.yml's "Help allowlist tamper check" step), the
// exact base-ref-anchored mechanism ci.yml's own Coverage floor tamper
// check already uses for COVERAGE_FLOOR (CLAUDE.md: "the floor only
// ever ratchets up — never lower it to make a change pass"). An earlier
// version of this gate tried to enforce the ratchet with a second Go
// map literal in this same file/commit — that is not a ratchet, since
// a PR editing this file can trivially edit both literals together;
// independent review caught it (uc-infra#146 review record). Do not
// reintroduce that pattern; if a Go-level check is wanted for fast
// local feedback, it would need to read the git history itself, which
// is exactly what the shell script already does properly.

import (
	"sort"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/modules"
)

// moduleEntityDefs is derived from internal/modules' own registry
// (modules.FoundationDefinitions plus every modules.Publishers entry's
// Definitions), not a hand-maintained list — unlike moduleFormDefs
// below, entity Definitions already have a registry to draw from, so a
// ninth module registering in Publishers is structurally covered here
// with nothing to keep in sync (no drift class for
// TestModuleFormDefs_CoverEveryRegisteredModule to guard against on
// this half, which is why that test only checks the forms list).
func moduleEntityDefs() [][]*entity.Definition {
	defs := [][]*entity.Definition{modules.FoundationDefinitions()}
	for _, p := range modules.Publishers {
		defs = append(defs, p.Definitions())
	}
	return defs
}

// moduleFormDefs mirrors i18n_coverage_test.go's own moduleFormSets
// list byte-for-byte (foundation, which modules.Publishers deliberately
// excludes per modules.FoundationKey's doc comment, plus each of
// Publishers' seven optional modules) — hand-maintained because, unlike
// entity Definitions, form Definitions have no registry accessor to
// derive this from (AllForms is a per-package function Publisher
// doesn't expose). Kept as a second literal rather than shared with
// formrender_test's own copy because the two files live in different
// packages with no third package either could import from without
// creating a cycle. TestModuleFormDefs_CoverEveryRegisteredModule below
// is what keeps this one honest against future drift, the same role
// i18n_coverage_test.go's own TestModuleFormSets_CoversEveryRegisteredModule
// plays for its copy.
func moduleFormDefs() [][]*form.Definition {
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

// TestModuleFormDefs_CoverEveryRegisteredModule is what makes the
// coverage gate below binding on a FUTURE module's forms, not just
// today's eight. Without it, a ninth module could register in
// internal/modules.Publishers, ship entirely undocumented forms, and
// every test below would still pass green — it would simply never look
// at that module. Same count-comparison reasoning (not matching module
// keys) as i18n_coverage_test.go's own version, and the same underlying
// limitation: AllForms is a per-package function, not something
// Publishers exposes, so a count is what's available. (moduleEntityDefs
// above needs no equivalent test — it's derived from Publishers
// directly, so it cannot drift from it.)
func TestModuleFormDefs_CoverEveryRegisteredModule(t *testing.T) {
	// +1 for foundation, which is not in Publishers.
	if got, want := len(moduleFormDefs()), len(modules.Publishers)+1; got != want {
		t.Fatalf("moduleFormDefs lists %d modules but internal/modules knows %d (%d optional + foundation): "+
			"a module missing here is a module whose forms this help-coverage gate silently never checks",
			got, want, len(modules.Publishers))
	}
}

// requiredEntityTypes is the set of entity types this gate holds to a
// help-topic requirement: the union of every module's entity.Definition
// EntityType and every module's form.Definition EntityType. It's a
// union, not just the entity set, because a handful of entity types
// (AIProviderConnection, ExternalIdentity, ExternalSQLSource, as of this
// slice) have an entity.Definition but no form.Definition — configured
// via API/admin surfaces rather than a generated form page — and the
// reverse never happens in this kernel today (every form's EntityType
// names a real entity.Definition), but the union is what's actually
// correct to require: formrender.buildViewModel and
// internal/api/listview.go both derive their "?" affordance from
// help.TopicID(EntityType) regardless of which side registered that
// entity type.
func requiredEntityTypes() []string {
	seen := map[string]bool{}
	for _, defs := range moduleEntityDefs() {
		for _, d := range defs {
			seen[d.EntityType] = true
		}
	}
	for _, defs := range moduleFormDefs() {
		for _, d := range defs {
			seen[d.EntityType] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// undocumentedAllowlist is the shrinking exception list uc-infra#146
// asks for: every entity type in it is exempt from the coverage check
// below because its help topic hasn't been written yet. Edit this map
// only to REMOVE a key once its module's content slice (uc-infra#147-
// 152) has shipped real topics in all four locales — never to add one.
// This file does not enforce that "only shrinks" rule itself (see the
// package doc comment above for why a same-commit Go check can't) —
// .github/scripts/check-help-allowlist.sh, run against the PR's base
// ref by ci.yml, does.
var undocumentedAllowlist = map[string]bool{
	"Account":                      true,
	"AttendanceRecord":             true,
	"Campaign":                     true,
	"Case":                         true,
	"CostCenter":                   true,
	"CustomerInvoice":              true,
	"DepreciationSchedule":         true,
	"Employee":                     true,
	"Facility":                     true,
	"FiscalYear":                   true,
	"FixedAsset":                   true,
	"GoodsReceipt":                 true,
	"GoodsReceiptLine":             true,
	"InventoryItem":                true,
	"Item":                         true,
	"Lead":                         true,
	"LeaveRequest":                 true,
	"MaintenanceOrder":             true,
	"Opportunity":                  true,
	"POLine":                       true,
	"Period":                       true,
	"Project":                      true,
	"ProjectBudgetLine":            true,
	"PurchaseOrder":                true,
	"ReorderRule":                  true,
	"RequestForQuotation":          true,
	"RequestForQuotationLine":      true,
	"RequestForQuotationQuoteLine": true,
	"RequestForQuotationVendor":    true,
	"SOLine":                       true,
	"SalesOrder":                   true,
	"StockTransfer":                true,
	"Task":                         true,
	"TaxCode":                      true,
	"TimeEntry":                    true,
	"VendorInvoice":                true,
}

// missingTopic is one (entity type, locale) pair with no real content —
// undocumentedTopics' return shape, factored out so the failure path
// itself has a fast, hand-driven unit test independent of the real
// embedded content ever changing shape. As of this slice's landing
// every required entity type is allowlisted, so
// TestHelpCoverage_EveryDefinitionHasATopic's own loop body never
// executes anything that would fail — without a table test exercising
// undocumentedTopics directly, the gate's failure path would be
// entirely unrun (and therefore unverified) until uc-infra#147 first
// shrinks the allowlist, which independent review flagged as exactly
// the kind of untested-by-construction gate CLAUDE.md's testing
// discipline exists to prevent.
type missingTopic struct {
	entityType string
	locale     string
}

// undocumentedTopics is TestHelpCoverage_EveryDefinitionHasATopic's pure
// core: for every entity type in required that isn't in allow, check
// hasReal(entityType, locale) for every locale, and collect the ones
// that come back false. hasReal is injected (rather than this function
// calling help.GetIndex()/OwnPlainText directly) so
// TestUndocumentedTopics_TableDriven below can exercise every branch —
// all-missing, one-locale-missing, allowlisted-skips, fully-documented —
// without needing real embedded content to exist for any of them to be
// true.
func undocumentedTopics(required []string, allow map[string]bool, locales []string, hasReal func(entityType, locale string) bool) []missingTopic {
	var out []missingTopic
	for _, entityType := range required {
		if allow[entityType] {
			continue
		}
		for _, locale := range locales {
			if !hasReal(entityType, locale) {
				out = append(out, missingTopic{entityType, locale})
			}
		}
	}
	return out
}

func TestUndocumentedTopics_TableDriven(t *testing.T) {
	locales := []string{"en", "tr"}

	t.Run("allowlisted entity is skipped entirely, even with no content", func(t *testing.T) {
		got := undocumentedTopics([]string{"A"}, map[string]bool{"A": true}, locales, func(string, string) bool { return false })
		if len(got) != 0 {
			t.Fatalf("undocumentedTopics = %v, want none (A is allowlisted)", got)
		}
	})

	t.Run("non-allowlisted entity with no content at all fails every locale", func(t *testing.T) {
		got := undocumentedTopics([]string{"B"}, map[string]bool{}, locales, func(string, string) bool { return false })
		want := []missingTopic{{"B", "en"}, {"B", "tr"}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("undocumentedTopics = %v, want %v", got, want)
		}
	})

	t.Run("en documented but tr missing fails only tr — the actual per-locale requirement", func(t *testing.T) {
		hasReal := func(_, locale string) bool { return locale == "en" }
		got := undocumentedTopics([]string{"C"}, map[string]bool{}, locales, hasReal)
		want := []missingTopic{{"C", "tr"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("undocumentedTopics = %v, want %v", got, want)
		}
	})

	t.Run("fully documented in every locale produces nothing", func(t *testing.T) {
		got := undocumentedTopics([]string{"D"}, map[string]bool{}, locales, func(string, string) bool { return true })
		if len(got) != 0 {
			t.Fatalf("undocumentedTopics = %v, want none (fully documented)", got)
		}
	})

	t.Run("mixed set only reports the actually-undocumented ones", func(t *testing.T) {
		hasReal := func(entityType, _ string) bool { return entityType == "F" }
		got := undocumentedTopics([]string{"E", "F"}, map[string]bool{}, locales, hasReal)
		want := []missingTopic{{"E", "en"}, {"E", "tr"}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("undocumentedTopics = %v, want %v (F is fully documented, only E should be reported)", got, want)
		}
	})
}

// TestHelpCoverage_EveryDefinitionHasATopic is the actual gate
// uc-infra#146 asks for, wired to the real embedded content via
// help.GetIndex()/OwnPlainText. As of this slice landing every one of
// the 61 current entity types is still in undocumentedAllowlist, so
// this test passes today by construction (see
// TestUndocumentedTopics_TableDriven above for proof the failure logic
// itself works) — it becomes binding the moment uc-infra#147's first
// content slice removes its module's entries, which is the whole point:
// the gate ships live, not toggled on later.
func TestHelpCoverage_EveryDefinitionHasATopic(t *testing.T) {
	required := requiredEntityTypes()
	if len(required) == 0 {
		t.Fatal("requiredEntityTypes() returned nothing — moduleEntityDefs/moduleFormDefs are stale or every module returned nothing, either way this gate isn't checking anything")
	}

	idx, err := help.GetIndex()
	if err != nil {
		t.Fatalf("help.GetIndex(): %v — a malformed topic file fails loud here rather than silently vanishing from the manual (ADR-0023)", err)
	}
	hasReal := func(entityType, locale string) bool {
		text, ok := idx.OwnPlainText(help.TopicID(entityType), locale)
		return ok && strings.TrimSpace(text) != ""
	}

	missing := undocumentedTopics(required, undocumentedAllowlist, help.ContentLocales(), hasReal)
	for _, m := range missing {
		t.Errorf("entity %q has no real %s help topic (missing or blank-body internal/help/content/%s/%s.md) — "+
			"add real content in all four locales. undocumentedAllowlist only ever shrinks (CLAUDE.md's "+
			"\"In-product help manual\" section): adding this entity type to it here will not make CI pass — "+
			".github/scripts/check-help-allowlist.sh fails a PR that adds any allowlist entry not already present at the PR's base ref.",
			m.entityType, m.locale, m.locale, help.TopicID(m.entityType))
	}

	allAllowlisted := true
	for _, entityType := range required {
		if !undocumentedAllowlist[entityType] {
			allAllowlisted = false
			break
		}
	}
	if allAllowlisted && len(missing) == 0 {
		t.Skip("every required entity type is currently in undocumentedAllowlist — expected as of uc-infra#146 landing, before any content slice has shipped")
	}
}

// staleEntry is one undocumentedAllowlist key that no longer belongs
// there, and why — staleAllowlistEntries' return shape.
type staleEntry struct {
	entityType string
	reason     string
}

// staleAllowlistEntries is TestUndocumentedAllowlist_NoStaleEntries' pure
// core, injected the same way undocumentedTopics is so its branches have
// a fast table test independent of real embedded content or a real
// module registry. Two distinct ways an entry goes stale:
//   - the entity type it names is no longer in required at all (the
//     Definition was renamed/removed from the kernel) — it should never
//     have stayed in the allowlist once that happened, and worse, a
//     later PR reintroducing that exact entity type would silently find
//     it pre-exempted, one more quiet way a "new" Definition could ship
//     undocumented (uc-infra#146 review finding).
//   - the entity type is still required but is now fully documented in
//     every locale — the content slice that documented it forgot to
//     remove the now-stale exemption.
func staleAllowlistEntries(allow map[string]bool, required map[string]bool, locales []string, hasReal func(entityType, locale string) bool) []staleEntry {
	var out []staleEntry
	keys := make([]string, 0, len(allow))
	for k := range allow {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, entityType := range keys {
		if !required[entityType] {
			out = append(out, staleEntry{entityType, "no longer a required entity type — remove it from undocumentedAllowlist"})
			continue
		}
		fullyDocumented := true
		for _, locale := range locales {
			if !hasReal(entityType, locale) {
				fullyDocumented = false
				break
			}
		}
		if fullyDocumented {
			out = append(out, staleEntry{entityType, "fully documented in every shipped locale — remove it now that its content has shipped"})
		}
	}
	return out
}

func TestStaleAllowlistEntries_TableDriven(t *testing.T) {
	locales := []string{"en", "tr"}

	t.Run("still required and still undocumented is not stale", func(t *testing.T) {
		got := staleAllowlistEntries(map[string]bool{"A": true}, map[string]bool{"A": true}, locales, func(string, string) bool { return false })
		if len(got) != 0 {
			t.Fatalf("staleAllowlistEntries = %v, want none", got)
		}
	})

	t.Run("still required but fully documented is stale", func(t *testing.T) {
		got := staleAllowlistEntries(map[string]bool{"B": true}, map[string]bool{"B": true}, locales, func(string, string) bool { return true })
		if len(got) != 1 || got[0].entityType != "B" {
			t.Fatalf("staleAllowlistEntries = %v, want one stale entry for B", got)
		}
	})

	t.Run("no longer required at all is stale, even with no content", func(t *testing.T) {
		got := staleAllowlistEntries(map[string]bool{"C": true}, map[string]bool{}, locales, func(string, string) bool { return false })
		if len(got) != 1 || got[0].entityType != "C" {
			t.Fatalf("staleAllowlistEntries = %v, want one stale entry for C (no longer required)", got)
		}
	})

	t.Run("partially documented (one locale still missing) is not stale", func(t *testing.T) {
		hasReal := func(_, locale string) bool { return locale == "en" }
		got := staleAllowlistEntries(map[string]bool{"D": true}, map[string]bool{"D": true}, locales, hasReal)
		if len(got) != 0 {
			t.Fatalf("staleAllowlistEntries = %v, want none (tr still missing)", got)
		}
	})
}

// TestUndocumentedAllowlist_NoStaleEntries is the real (index-wired)
// counterpart to TestStaleAllowlistEntries_TableDriven above, the same
// split TestHelpCoverage_EveryDefinitionHasATopic/undocumentedTopics
// uses. Not a hard requirement uc-infra#146 spelled out, but cheap and
// directly operationalizes its "each content slice removes its entries"
// line, and closes a real gap independent review found in an earlier
// version of this file (an entry could go stale by its entity type
// being deleted from the kernel entirely, silently forever, because the
// old version only ever looked at entries that were still required).
func TestUndocumentedAllowlist_NoStaleEntries(t *testing.T) {
	requiredSet := map[string]bool{}
	for _, entityType := range requiredEntityTypes() {
		requiredSet[entityType] = true
	}

	idx, err := help.GetIndex()
	if err != nil {
		t.Fatalf("help.GetIndex(): %v", err)
	}
	hasReal := func(entityType, locale string) bool {
		text, ok := idx.OwnPlainText(help.TopicID(entityType), locale)
		return ok && strings.TrimSpace(text) != ""
	}

	for _, s := range staleAllowlistEntries(undocumentedAllowlist, requiredSet, help.ContentLocales(), hasReal) {
		t.Errorf("undocumentedAllowlist[%q]: %s", s.entityType, s.reason)
	}
}
