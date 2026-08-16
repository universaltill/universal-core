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

// HasOwn reports whether locale itself (not a fallback language/locale
// T would otherwise walk to) has its own message for key. Unlike T,
// this never consults baseLang(locale), c.fallback, or
// baseLang(c.fallback) — a coverage check that asked T (or TOrDefault)
// "does this locale have key?" would always answer yes the moment the
// fallback locale has it, since T's whole point is to hide that gap
// from a viewer. HasOwn exists for the opposite audience: a build-time
// completeness check (internal/kernel/formrender's i18n_coverage_test.go)
// that needs to know THIS locale's own file actually carries the entry,
// not that some other locale's translation is quietly standing in for
// it.
func (c *Catalog) HasOwn(locale, key string) bool {
	m, ok := c.messages[locale]
	if !ok {
		return false
	}
	_, ok = m[key]
	return ok
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

// FallbackChain returns the ordered locale candidates ResolveLocalized
// tries before falling back to "any translation" — exact locale, its
// base language, the catalog's fallback locale, and the fallback's own
// base language. Exported so callers building their own locale-aware
// query (e.g. internal/api's i18n-aware sort) can mirror the same
// precedence ResolveLocalized uses, without duplicating it.
func (c *Catalog) FallbackChain(locale string) []string {
	return []string{locale, baseLang(locale), c.fallback, baseLang(c.fallback)}
}

// ResolveLocalized resolves a record's multilingual value (an entity
// FieldI18nText, ADR-0009) for the given viewer locale. v is the raw
// value straight off the JSONB data column: for an i18n_text field it is a
// map[string]any of locale -> string (that is how encoding/json unmarshals
// the stored object).
//
// Returns (resolved, true) when v is such an object, so callers can tell a
// genuine i18n value apart from an ordinary field — a plain string field
// yields (\"\", false) and the caller keeps its existing string handling
// untouched. The fallback chain mirrors T's: exact locale, its base
// language, the catalog fallback and its base language, then — so a value
// that exists in *some* language is never shown blank — the first
// non-empty entry in deterministic (sorted) locale order. All-empty or a
// non-object yields \"\".
func (c *Catalog) ResolveLocalized(v any, locale string) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	str := func(loc string) string {
		s, _ := m[loc].(string)
		return s
	}
	for _, loc := range c.FallbackChain(locale) {
		if s := str(loc); s != "" {
			return s, true
		}
	}
	// Last resort: any translation at all, chosen deterministically so the
	// same record always resolves the same way regardless of map order.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s := str(k); s != "" {
			return s, true
		}
	}
	return "", true
}

// Fallback returns the catalog's fallback locale (the primary language a
// required multilingual field must at least be filled in — see ADR-0009).
func (c *Catalog) Fallback() string { return c.fallback }

// Keys returns the sorted set of message keys locale's own file
// carries. Like HasOwn (and unlike T/TOrDefault), this only sees what
// that locale's own file actually has — no fallback chain. Returns nil
// only if locale has no locale file at all — a locale whose file exists
// but is empty ("{}") returns a non-nil, zero-length slice, since that's
// a real (if degenerate) locale, not an absent one. Available()'s own
// "has messages" check is a different, stricter test (len(m) > 0) for
// a different question (which locales are usable at all) — don't read
// the two as the same definition of "empty."
//
// Exists for a caller that needs to enumerate a whole family of
// data-driven keys (e.g. every "field.{EntityType}.status_id.{code}"
// entry, uc-infra#256) to check them against another source of truth —
// T/TOrDefault/HasOwn all require the caller to already know the key,
// which doesn't help when the question is "what keys exist."
func (c *Catalog) Keys(locale string) []string {
	m, ok := c.messages[locale]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
