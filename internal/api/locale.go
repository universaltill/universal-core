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
// paginated list doesn't silently reset all three — EXCEPT "q" (see
// below), which is dropped deliberately.
//
// "q" can hold text that isn't locale-invariant: since board #74, a
// FieldNumber/FieldDate filter's "q" is the RAW text the viewer typed
// under the PREVIOUS region's grouping/decimal/date-order rules, and
// listview.go re-parses that same raw text under whatever region is now
// active. Carrying it across a region switch can silently reinterpret it
// as a DIFFERENT valid value under the new rules (en-GB's day-first
// "04/03/2026" reads as en-US's month-first 4 April, not 3 April — no
// error, no empty result, just a different match, with no indication to
// the viewer that the filter's meaning changed) or stop it matching
// altogether — either way, the viewer never asked for that (uc-infra#128).
//
// This drops "q" unconditionally, for every region switch and every
// field type — not only when the two regions actually disagree, and not
// only for FieldNumber/FieldDate. regionOptions only has the request and
// the language: it has no Definition to check the active filter field's
// type against (nav renders on every page, not just a list page, and
// doing a tenant-scoped lookup here just to maybe skip clearing a search
// box is not IO this shared chrome should own — see renderNav's own
// "must never turn into a hard failure" framing). The honest tradeoff:
// switching between two regions that format identically (e.g. any two of
// en-GB/en-IE/en-AU/en-NZ, or any two "ar" choices), or while filtering a
// plain FieldString, still clears a search box that didn't need clearing
// — a re-type, not a silent surprise. "filter" (which FIELD is targeted)
// has no locale dependence and always stays, matching the
// already-supported bookmarked-"?filter=weight"-with-no-"q" case
// (uc-infra#129) — a region switch just leaves that same box empty
// instead of guessing what the old text now means.
//
// What this did NOT originally close: "q" traveling back in via a route
// other than this picker's own generated link — the Back button, a
// bookmark, or a link shared to someone whose region cookie differs —
// since neither the filter form nor the sort/page links pinned the
// region they were rendered under. That was a real, separate gap (filed
// as uc-infra#164) — closed by listview.go's keepQuery/sortParams and
// the filter form's own hidden field, which now pin the rendering
// region into every link/submit this handler generates via the
// qRegionParam ("qregion") this file defines below — deliberately NOT
// "region" itself, so that pin is never mistaken for an explicit switch
// and persisted over the viewer's real preference (see qRegionParam's
// own doc comment). Only the region a stale "q" is interpreted under is
// pinned this way; language is not (a pinned link can still be
// reinterpreted if the active LANGUAGE differs from the one "q" was
// typed under — a narrower, milder residual gap than the one this fix
// closes, since the shipped locale set degrades that case to "no
// match" rather than a silent wrong match).
// qRegionParam is the query parameter that pins the region a filter's
// "q" text was actually typed/rendered under (uc-infra#164) — read-only,
// and DELIBERATELY not "region": persistRegionPreference above only
// ever scans "region" itself when deciding what to persist. This pin is
// present on nearly every link listview.go generates (every sort/pager/
// Clear link, and the filter form's own hidden field), so if it reused
// "region" it would make EVERY one of those an "explicit switch" as far
// as persistRegionPreference is concerned — silently overwriting the
// viewer's real saved preference on an ordinary click, or hijacking a
// recipient's preference the moment they open a shared link (independent
// review of an earlier version of this fix caught exactly that: it
// reused "region" for both purposes). Keeping this a separate,
// differently-named parameter means persistRegionPreference never even
// looks at it — the pin can ride along on every generated link without
// ever being mistaken for a deliberate region switch.
const qRegionParam = "qregion"

// pinnedRegion resolves qRegionParam against language, returning the
// Locale it names only if it's a real, offered choice for that language
// — the same validation offeredRegion already gives "region", reused
// rather than duplicated (a pin can't claim a region the picker itself
// wouldn't offer). ok is false for a missing, tampered, or
// cross-language value; the caller's own fallback (the request's real
// active region) governs in that case — a bad or absent pin is never a
// hard failure, just "nothing to pin".
func pinnedRegion(r *http.Request, language string) (locale.Locale, bool) {
	tag := r.URL.Query().Get(qRegionParam)
	if tag == "" {
		return locale.Locale{}, false
	}
	return offeredRegion(tag, language)
}

func (h *Handler) regionOptions(r *http.Request, language string) []regionOption {
	choices := regionChoices[language]
	if len(choices) < 2 {
		return nil
	}
	// One stripped base, reused across choices: Set below overwrites just
	// the "region" key each iteration, so there's no need to re-parse (or
	// copy) the query string per choice.
	base := r.URL.Query()
	base.Del("q")
	out := make([]regionOption, 0, len(choices))
	for _, tag := range choices {
		base.Set("region", tag)
		out = append(out, regionOption{
			Tag:   tag,
			Label: h.catalog.TOrDefault(language, "region."+strings.NewReplacer("-", "_").Replace(tag), tag),
			Href:  r.URL.Path + "?" + base.Encode(),
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
