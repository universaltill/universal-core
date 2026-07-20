// Package i18n is a small JSON-based translator. Ported from
// universal-till's internal/config/i18n.go — i18n is one of the explicit
// reuse-from-unitill items (erp/BACKLOG.md) rather than something to
// reinvent. This is deliberately the base-message subset only: locale
// files plus a fallback chain (BCP-47 tag -> base language -> fallback
// locale -> fallback's base language), same lookup order as unitill's T().
// unitill's I18n also layers manager "shop" overrides and plugin-supplied
// overlays on top of the base files; Universal Core has no translation
// editor and no plugin runtime wired up yet (BACKLOG.md), so those layers
// are intentionally left out here rather than guessed at — add them when
// the plugin runtime actually lands, following the same precedence unitill
// uses (shop > base > overlay).
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// Catalog holds every locale's messages, loaded once at startup.
type Catalog struct {
	messages map[string]map[string]string
	fallback string
}

// Load reads every embedded locales/*.json file. fallback is the locale
// used when a requested locale (or its base language) has no messages at
// all, e.g. "en".
func Load(fallback string) (*Catalog, error) {
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		return nil, fmt.Errorf("i18n: read locales dir: %w", err)
	}
	c := &Catalog{messages: make(map[string]map[string]string), fallback: fallback}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		locale := strings.TrimSuffix(name, ".json")
		b, err := localeFS.ReadFile("locales/" + name)
		if err != nil {
			return nil, fmt.Errorf("i18n: read %s: %w", name, err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("i18n: parse %s: %w", name, err)
		}
		c.messages[locale] = m
	}
	if _, ok := c.messages[fallback]; !ok {
		return nil, fmt.Errorf("i18n: fallback locale %q has no locale file", fallback)
	}
	return c, nil
}

// T returns the translation for key in the given locale, falling back to
// the base language, then the default locale, then its base language.
// Returns key itself if nothing matches, so a missing translation is
// visible (a literal key on screen) rather than silently blank.
func (c *Catalog) T(locale, key string) string {
	for _, loc := range []string{locale, baseLang(locale), c.fallback, baseLang(c.fallback)} {
		if m, ok := c.messages[loc]; ok {
			if v, ok := m[key]; ok {
				return v
			}
		}
	}
	return key
}

// TOrDefault is T for a key built from data rather than authored UI
// copy — an entity type name, a module key, an enum value — where T's
// own fallback (the literal key string, e.g. "field.Item.item_type.stock")
// would be a meaningless string to show a user, not a legible English
// default. Returns fallback (the raw underlying value) instead of the
// key whenever no locale in T's chain has a translation for it.
func (c *Catalog) TOrDefault(locale, key, fallback string) string {
	if v := c.T(locale, key); v != key {
		return v
	}
	return fallback
}

// Available returns the sorted locale codes that have at least one message.
func (c *Catalog) Available() []string {
	out := make([]string, 0, len(c.messages))
	for loc, m := range c.messages {
		if len(m) > 0 {
			out = append(out, loc)
		}
	}
	sort.Strings(out)
	return out
}

// baseLang strips the region from a BCP-47 tag: "en-US" -> "en".
func baseLang(locale string) string {
	if idx := strings.IndexAny(locale, "-_"); idx > 0 {
		return locale[:idx]
	}
	return locale
}
