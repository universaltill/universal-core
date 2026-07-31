package api

import (
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/locale"
)

const localeCookie = "uc_locale"

// regionCookie holds the REGIONAL formatting preference — a full
// locale.Locale tag ("en-US", "fa-IR-u-ca-gregory"), separate from the
// language cookie above because the two are genuinely different
// choices (board #22): which language the words are in, versus how
// dates and numbers are written. A visitor reading English in New York
// and one reading English in London share every translated string and
// disagree about what 03/04/2026 means.
const regionCookie = "uc_locale_region"

// persistRegionPreference writes the region cookie when the request
// carries an explicit, offered ?region=. Called from localeFromRequest
// — i.e. from EVERY page handler — rather than from the one page that
// happens to render formatted cells: the picker is in the nav on every
// page, and a picker that appears to work (the reloaded page shows the
// new option selected, because the query param is honoured for that
// request) while silently persisting nothing is worse than no picker.
// Independent review caught exactly that: choosing a region on the
// dashboard left every list page still formatting as en-GB.
func persistRegionPreference(w http.ResponseWriter, r *http.Request, language string) {
	tag := r.URL.Query().Get("region")
	if tag == "" {
		return
	}
	loc, ok := offeredRegion(tag, language)
	if !ok {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     regionCookie,
		Value:    loc.Tag(),
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
}

// offeredRegion parses a preference and accepts it only if it is one of
// the choices this language's picker actually offers. Parsing alone is
// not enough: locale.Parse happily accepts any language/region/calendar
// crossing, so "en-GB-u-ca-jalali" or "tr-US" would otherwise be
// persisted for a year — and for a single-region language like Turkish
// the picker is suppressed, leaving no way to undo it through the UI
// (independent review). Restricting to the offered set also guarantees
// the picker's selected option always matches what the page renders.
func offeredRegion(tag, language string) (locale.Locale, bool) {
	loc, err := locale.Parse(tag)
	if err != nil || loc.Language != language {
		return locale.Locale{}, false
	}
	canonical := loc.Tag()
	for _, choice := range regionChoices[language] {
		if choice == canonical {
			return loc, true
		}
	}
	return locale.Locale{}, false
}

// regionalLocale resolves the request's regional Locale without writing
// anything — the read side, used by every renderer. Persistence is
// localeFromRequest's job (see persistRegionPreference).
func regionalLocale(r *http.Request, language string) locale.Locale {
	if tag := r.URL.Query().Get("region"); tag != "" {
		if loc, ok := offeredRegion(tag, language); ok {
			return loc
		}
	}
	if c, err := r.Cookie(regionCookie); err == nil {
		// The stored preference only applies while it still matches the
		// active language: switching the UI to Turkish should not keep
		// formatting dates as en-US. The language switch simply falls
		// back to that language's conventional region.
		if loc, ok := offeredRegion(c.Value, language); ok {
			return loc
		}
	}
	return locale.Default(language)
}

// regionChoices lists the selectable regional variants per language for
// the nav's region picker — the regions locale.go's own rule table
// actually has conventions for, in a fixed order (a picker whose
// options shuffled between requests would be its own small bug, same
// reasoning as supportedLocaleList).
var regionChoices = map[string][]string{
	"en": {"en-GB", "en-US", "en-IE", "en-AU", "en-NZ", "en-IN"},
	"ar": {"ar-AE", "ar-SA", "ar-QA", "ar-KW", "ar-BH", "ar-OM", "ar-EG"},
	"tr": {"tr-TR"},
	// A Farsi reader may legitimately want Gregorian dates (an
	// export-facing role, an international team) — the calendar is part
	// of the regional preference, so it belongs in this picker.
	"fa": {"fa-IR", "fa-AF", "fa-IR-u-ca-gregory"},
}

// regionOption is one entry in the nav's region picker: the canonical
// tag (to mark the active one), a translated label, and the URL that
// selects it.
type regionOption struct {
	Tag   string
	Label string
	Href  string
}

// regionOptions builds the picker's entries for this language. The
// label goes through the catalog — a raw BCP 47 tag like
// "fa-IR-u-ca-gregory" is an extension subtag, not something to show an
// accountant (the two-letter language codes in the language switcher
// are a recognised convention; this is not). The href preserves the
// current query string, so choosing a region on a filtered, sorted,
// paginated list doesn't silently reset all three.
func (h *Handler) regionOptions(r *http.Request, language string) []regionOption {
	choices := regionChoices[language]
	if len(choices) < 2 {
		return nil
	}
	out := make([]regionOption, 0, len(choices))
	for _, tag := range choices {
		q := r.URL.Query()
		q.Set("region", tag)
		out = append(out, regionOption{
			Tag:   tag,
			Label: h.catalog.TOrDefault(language, "region."+strings.NewReplacer("-", "_").Replace(tag), tag),
			Href:  r.URL.Path + "?" + q.Encode(),
		})
	}
	return out
}

// supportedLocales matches internal/i18n's actual locale files
// (en.json, ar.json, tr.json, fa.json) — the only ones the catalog can
// serve. An unrecognized ?lang= value is ignored rather than persisted,
// so a typo or a stale bookmark doesn't silently pin a visitor to
// English forever via the cookie below. Kept in sync with
// internal/i18n/locales/*.json by hand — see that package's own
// Catalog.Available() if this needs to become dynamic later.
var supportedLocales = map[string]bool{"en": true, "ar": true, "tr": true, "fa": true}

// supportedLocaleList is supportedLocales in a fixed, deterministic
// order — used by nav.go's language switcher; a Go map has no order of
// its own, and a switcher whose link order shuffled between requests
// would be a strange, distracting kind of bug.
var supportedLocaleList = []string{"en", "ar", "tr", "fa"}

// rtlLocales was a second table answering "is this locale RTL"; the
// Locale type now owns that (locale.Locale.RTL), so direction travels
// with the same value that decides formatting instead of being looked
// up separately and drifting from it.
func localeIsRTL(language string) bool { return locale.Default(language).RTL() }

// localeFromRequest resolves the request's locale and, when ?lang=
// explicitly names a supported one, persists it in a cookie so it
// survives past this one request. Without this, a locale chosen once
// silently reverted to English on the very next click: every page this
// kernel renders navigates via a plain <a href> with no ?lang= of its
// own (nav links, hub nodes, module menu items, form actions), so
// "multilingual" meant nothing a visitor could actually keep using —
// found when Farshid pointed out the i18n catalog existing server-side
// wasn't the same thing as the app actually being usable in Arabic.
func localeFromRequest(w http.ResponseWriter, r *http.Request) string {
	lang := languageFromRequest(w, r)
	// The regional preference rides along: every page renders the nav's
	// region picker, so every page must be able to persist a choice
	// made from it.
	persistRegionPreference(w, r, lang)
	return lang
}

func languageFromRequest(w http.ResponseWriter, r *http.Request) string {
	if l := r.URL.Query().Get("lang"); l != "" && supportedLocales[l] {
		http.SetCookie(w, &http.Cookie{
			Name:     localeCookie,
			Value:    l,
			Path:     "/",
			MaxAge:   365 * 24 * 60 * 60,
			SameSite: http.SameSiteLaxMode,
			Secure:   requestIsHTTPS(r),
		})
		return l
	}
	if c, err := r.Cookie(localeCookie); err == nil && supportedLocales[c.Value] {
		return c.Value
	}
	return "en"
}

// requestIsHTTPS reports whether the original client request was HTTPS
// — matching internal/webauth's own secureCookies() reasoning (that
// package's session cookie is Secure iff its configured RedirectURL is
// https://), except keyed off the actual request instead of a static
// config value, since locale.go has no equivalent URL to check. TLS
// terminates at the Traefik ingress in every real deployment (the app
// itself only ever sees plain HTTP, so r.TLS is always nil there) —
// X-Forwarded-Proto is what actually carries the original scheme
// through; r.TLS is still checked first for the local-dev/no-ingress
// case where the app terminates TLS itself.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// localeDir returns the HTML `dir` attribute value for locale — "rtl"
// for Arabic, "ltr" for everything else. Threaded into shellTmpl (see
// layout.go) so the whole document actually mirrors for a right-to-left
// language instead of just swapping words inside a left-to-right layout.
func localeDir(locale string) string {
	if localeIsRTL(locale) {
		return "rtl"
	}
	return "ltr"
}

// entityDisplayName resolves an entity type's human label via
// "entity.{EntityType}.name" in the i18n catalog, falling back to the
// raw technical EntityType (e.g. "PurchaseOrder") when no translation
// exists — a real usable label instead of catalog.T's own fallback,
// which would otherwise be the literal, meaningless lookup key
// "entity.PurchaseOrder.name". Every entity this kernel actually ships
// (foundation.go, purchasing.go) has both an en and ar label; a
// Definition that doesn't declare one (a test fixture, or a future
// module's entity nobody has translated yet) still renders something
// legible rather than a blank or a raw i18n key.
func (h *Handler) entityDisplayName(locale, entityType string) string {
	return h.catalog.TOrDefault(locale, "entity."+entityType+".name", entityType)
}
