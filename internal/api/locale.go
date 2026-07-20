package api

import "net/http"

const localeCookie = "uc_locale"

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

// rtlLocales names this kernel's right-to-left locales — Arabic and
// Farsi today, kept as a lookup (not a single hardcoded check) so a
// further RTL locale (Hebrew, Urdu, …) is one line to add, not a
// rewrite.
var rtlLocales = map[string]bool{"ar": true, "fa": true}

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
	if rtlLocales[locale] {
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
