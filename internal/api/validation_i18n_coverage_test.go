package api

// validation_i18n_coverage_test.go is entity.AllValidationErrorKinds'
// own enforcement half (uc-infra#96, independent review) — the same
// convention internal/kernel/formrender/i18n_coverage_test.go already
// established for form section titles and the Save action label
// (universal-core/CLAUDE.md's i18n section): Catalog.T degrades silently
// to the literal key when nothing matches, which is the right behavior
// at render time and the wrong one to leave unchecked at CI time. A
// typo'd or newly-added ValidationErrorKind with no matching
// entity.validation.{kind} entry in some locale would otherwise put that
// literal key on a user's screen with nothing catching it until someone
// actually hit that failure path in that language.

import (
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

func TestValidationErrorKinds_HaveCatalogKeysInEveryLocale(t *testing.T) {
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("i18n.Load: %v", err)
	}
	locales := catalog.Available()
	if len(locales) == 0 {
		t.Fatal("no locales loaded — cannot check coverage")
	}
	for _, kind := range entity.AllValidationErrorKinds() {
		key := "entity.validation." + string(kind)
		for _, locale := range locales {
			// HasOwn, not T: T's whole point is to hide a missing
			// translation behind the fallback chain, exactly the gap
			// this test exists to catch — see HasOwn's own doc comment.
			if !catalog.HasOwn(locale, key) {
				t.Errorf("locale %q has no translation for ValidationErrorKind %q (catalog key %q)", locale, kind, key)
			}
		}
	}
}
