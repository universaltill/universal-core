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
