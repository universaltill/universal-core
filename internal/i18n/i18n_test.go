package i18n

import "testing"

func TestLoad_ReadsEmbeddedLocales(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.T("en", "form.field.required_suffix"); got != " *" {
		t.Fatalf("expected en required_suffix ' *', got %q", got)
	}
}

func TestLoad_MissingFallbackLocaleErrors(t *testing.T) {
	if _, err := Load("xx"); err == nil {
		t.Fatal("expected error when the fallback locale has no locale file")
	}
}

func TestT_FallsBackToBaseLanguage(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// ar-SA has no locale file, only ar.json — must fall back to the base language.
	got := c.T("ar-SA", "form.related_list.empty")
	want := c.T("ar", "form.related_list.empty")
	if got != want || got == "form.related_list.empty" {
		t.Fatalf("expected ar-SA to fall back to ar's message, got %q want %q", got, want)
	}
}

func TestT_FallsBackToFallbackLocale(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.T("de", "form.field.required_suffix")
	if got != " *" {
		t.Fatalf("expected unknown locale 'de' to fall back to fallback 'en', got %q", got)
	}
}

func TestT_ReturnsKeyWhenNothingMatches(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.T("en", "no.such.key")
	if got != "no.such.key" {
		t.Fatalf("expected missing key to return itself, got %q", got)
	}
}

// TestHasOwn_DoesNotFollowFallbackChain is what makes HasOwn a different
// tool from T for a completeness check: T("ar-SA", ...) or T("de", ...)
// happily resolves through ar/en respectively (TestT_FallsBackToBaseLanguage,
// TestT_FallsBackToFallbackLocale above), which is exactly right for
// rendering but exactly wrong for a test asking "does THIS locale have
// its own entry" — a T-based check can never observe a single locale
// missing a key once the fallback locale has it.
func TestHasOwn_DoesNotFollowFallbackChain(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.HasOwn("en", "form.field.required_suffix") {
		t.Fatal("expected en to have its own required_suffix entry")
	}
	// "de" has no locale file at all, only borrows en via T's fallback
	// chain — HasOwn must say no, not silently agree with T.
	if c.HasOwn("de", "form.field.required_suffix") {
		t.Fatal("HasOwn must not report a fallback-only locale as having its own entry")
	}
	if c.HasOwn("en", "no.such.key") {
		t.Fatal("HasOwn must not report a genuinely absent key as present")
	}
}

// TestLocales_HaveIdenticalKeySets guards against exactly the kind of
// drift these hand-maintained JSON files are prone to: a key added to
// en.json (or fixed/renamed) while updating the other three locales gets
// forgotten. A missing key doesn't break anything visibly — T just falls
// back through the chain to en, or to the literal key string — which is
// precisely why it needs an explicit test rather than relying on someone
// noticing a stray English string in an Arabic screenshot.
func TestLocales_HaveIdenticalKeySets(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reference := "en"
	want := c.messages[reference]
	for locale, got := range c.messages {
		if locale == reference {
			continue
		}
		for key := range want {
			if _, ok := got[key]; !ok {
				t.Errorf("%s.json is missing key %q (present in %s.json)", locale, key, reference)
			}
		}
		for key := range got {
			if _, ok := want[key]; !ok {
				t.Errorf("%s.json has key %q not present in %s.json", locale, key, reference)
			}
		}
	}
}

func TestKeys_ReturnsSortedKeysForThatLocaleOnly(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Keys("en")
	if len(got) == 0 {
		t.Fatal("expected en.json to have at least one key")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Keys() not sorted: %q before %q", got[i-1], got[i])
		}
	}
	want := c.messages["en"]
	if len(got) != len(want) {
		t.Fatalf("expected %d keys (len of en.json's own map), got %d", len(want), len(got))
	}
	for _, k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("Keys() returned %q, not present in en.json's own map", k)
		}
	}
}

func TestKeys_UnknownLocaleReturnsNil(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Keys("xx"); got != nil {
		t.Errorf("expected nil for an unknown locale, got %v", got)
	}
}

func TestAvailable_ListsBothLocales(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	locales := c.Available()
	want := []string{"ar", "en", "fa", "tr"}
	if len(locales) != len(want) {
		t.Fatalf("expected %v, got %v", want, locales)
	}
	for i, l := range want {
		if locales[i] != l {
			t.Fatalf("expected %v, got %v", want, locales)
		}
	}
}

// TestResolveLocalized covers the multilingual-data resolver (ADR-0009):
// exact-locale hit, base-language fall-through, catalog-fallback, the
// "any translation rather than blank" last resort, and the two negative
// cases (a non-object value, and an all-empty object).
func TestResolveLocalized(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	full := map[string]any{"en": "Each", "tr": "Adet", "fa": "عدد", "ar": "قطعة"}

	// Exact locale.
	if got, ok := c.ResolveLocalized(full, "tr"); !ok || got != "Adet" {
		t.Fatalf("exact locale: got (%q,%v), want (Adet,true)", got, ok)
	}
	// Region tag falls through to base language: "en-US" -> "en".
	if got, ok := c.ResolveLocalized(full, "en-US"); !ok || got != "Each" {
		t.Fatalf("base-lang fall-through: got (%q,%v), want (Each,true)", got, ok)
	}
	// Missing locale falls back to the catalog fallback ("en").
	if got, ok := c.ResolveLocalized(map[string]any{"en": "Each"}, "tr"); !ok || got != "Each" {
		t.Fatalf("fallback locale: got (%q,%v), want (Each,true)", got, ok)
	}
	// No requested/base/fallback match: any non-empty translation, chosen
	// deterministically (sorted key order → "ar" before "fa").
	if got, ok := c.ResolveLocalized(map[string]any{"fa": "عدد", "ar": "قطعة"}, "tr"); !ok || got != "قطعة" {
		t.Fatalf("last-resort: got (%q,%v), want (قطعة,true)", got, ok)
	}
	// A plain string field (not an i18n object) → (\"\", false) so callers
	// keep their existing string handling.
	if got, ok := c.ResolveLocalized("plain", "en"); ok || got != "" {
		t.Fatalf("non-object: got (%q,%v), want (\"\",false)", got, ok)
	}
	// An all-empty object is a valid i18n value with nothing to show.
	if got, ok := c.ResolveLocalized(map[string]any{"en": "", "tr": ""}, "en"); !ok || got != "" {
		t.Fatalf("all-empty: got (%q,%v), want (\"\",true)", got, ok)
	}
}

// TestFallbackChain_ReturnsSamePrecedenceResolveLocalizedUses pins the
// exact ordered candidate list ResolveLocalized's own (formerly inline)
// fallback chain walks — exact locale, base language, catalog fallback,
// fallback's base language — so a caller building its own locale-aware
// query (internal/data's SortI18nLocales) can mirror ResolveLocalized's
// precedence exactly, without duplicating the logic.
func TestFallbackChain_ReturnsSamePrecedenceResolveLocalizedUses(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.FallbackChain("tr-TR")
	want := []string{"tr-TR", "tr", "en", "en"}
	if len(got) != len(want) {
		t.Fatalf("FallbackChain(tr-TR): got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FallbackChain(tr-TR): got %v, want %v", got, want)
		}
	}
}

func TestFallback_ReturnsConfiguredLocale(t *testing.T) {
	c, err := Load("en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Fallback() != "en" {
		t.Fatalf("Fallback: got %q, want en", c.Fallback())
	}
}
