// The tenant picker and switcher (#25, ADR-0011): a login whose
// id_token asserts tenant_member in more than one linked tenant lands
// on /ui/auth/choose instead of being unable to sign in at all (the
// old orgIDFromClaims fail-closed guard, retired by this file), and an
// already-active session switches tenants via POST /ui/auth/switch —
// re-sealing the cookie, never re-authenticating.
package webauth

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/universaltill/universal-core/internal/i18n"
)

// SetI18n hands webauth the app's i18n catalog for its own pages — a
// post-construction setter (same shape api.Handler uses for optional
// wiring) so New's signature and every existing call site stay
// untouched; nil is fine and falls back to the built-in English copy,
// matching how this package's older inline pages (notLinked,
// loginExpired) behave today.
func (a *Authenticator) SetI18n(catalog *i18n.Catalog) {
	a.catalog = catalog
}

// chooseLocales mirrors internal/api's supportedLocales (and its RTL
// subset) — webauth deliberately doesn't import that package, so the
// contract is: a locale exists here iff a file exists in
// internal/i18n/locales. An unrecognized cookie value falls back to
// "en" instead of being trusted verbatim, same as localeFromRequest.
var (
	chooseLocales    = map[string]bool{"en": true, "tr": true, "fa": true, "ar": true}
	chooseRTLLocales = map[string]bool{"fa": true, "ar": true}
)

// requestLocale resolves the request's locale from the same uc_locale
// cookie internal/api's localeFromRequest persists, validated against
// the supported set.
func requestLocale(r *http.Request) string {
	if c, err := r.Cookie("uc_locale"); err == nil && chooseLocales[c.Value] {
		return c.Value
	}
	return "en"
}

// t resolves key for the request's locale, falling back to the built-in
// English copy when no catalog is wired or the key is missing.
func (a *Authenticator) t(r *http.Request, key, fallback string) string {
	if a.catalog == nil {
		return fallback
	}
	if v := a.catalog.T(requestLocale(r), key); v != key {
		return v
	}
	return fallback
}

// TenantOption is one switchable tenant, resolved for display.
type TenantOption struct {
	ID     string
	Name   string
	Active bool
}

// SessionTenants exposes the request's accessible-tenant set for the
// nav switcher (internal/api asks this instead of RequestContext
// growing a field — the accessible set is a session concern, same
// boundary as ShowLogout already respects). Empty when login is
// disabled, the session is absent, or it can access only one tenant —
// the switcher only renders when there is a real choice.
func (a *Authenticator) SessionTenants(r *http.Request) []TenantOption {
	sess := a.rawSessionFromCookie(r)
	if sess == nil {
		return nil
	}
	ids := sess.AccessibleTenantIDs()
	if len(ids) < 2 {
		return nil
	}
	names, err := a.tenants.NamesByIDs(r.Context(), ids)
	if err != nil {
		log.Printf("webauth: resolve tenant names for switcher: %v", err)
		return nil
	}
	opts := make([]TenantOption, 0, len(ids))
	for _, id := range ids {
		name := names[id]
		if name == "" {
			// A tenant deleted since login — skip rather than render an
			// unnamed switch target; the session's set self-corrects at
			// next login.
			continue
		}
		opts = append(opts, TenantOption{ID: id, Name: name, Active: id == sess.TenantID})
	}
	if len(opts) < 2 {
		return nil
	}
	return opts
}

// handleChoose renders the post-login tenant picker for a session that
// authenticated against more than one linked tenant. A session that
// already has an active tenant still gets the page (it doubles as a
// full-page switcher); one with no session at all goes to login.
func (a *Authenticator) handleChoose(w http.ResponseWriter, r *http.Request) {
	sess := a.rawSessionFromCookie(r)
	if sess == nil {
		a.redirectToLogin(w, r)
		return
	}
	ids := sess.AccessibleTenantIDs()
	names, err := a.tenants.NamesByIDs(r.Context(), ids)
	if err != nil {
		http.Error(w, "tenant lookup failed", http.StatusInternalServerError)
		return
	}
	locale := requestLocale(r)
	view := chooseView{
		Title:    a.t(r, "tenantpick.title", "Choose a tenant"),
		Intro:    a.t(r, "tenantpick.intro", "Your account has access to more than one tenant. Pick the one to work in — you can switch later from the top bar."),
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("returnTo")),
		Lang:     locale,
		Dir:      "ltr",
	}
	if chooseRTLLocales[locale] {
		view.Dir = "rtl"
	}
	for _, id := range ids {
		if names[id] == "" {
			continue
		}
		view.Tenants = append(view.Tenants, TenantOption{ID: id, Name: names[id], Active: id == sess.TenantID})
	}
	if len(view.Tenants) == 0 {
		a.notLinked(w)
		return
	}
	var buf bytes.Buffer
	if err := chooseTmpl.Execute(&buf, view); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// handleSwitch activates one of the session's accessible tenants and
// re-seals the cookie. Refuses (403) a tenant outside the sealed
// accessible set — the set was resolved from the verified id_token at
// login and is the sole authority here; no form value can widen it.
func (a *Authenticator) handleSwitch(w http.ResponseWriter, r *http.Request) {
	sess := a.rawSessionFromCookie(r)
	if sess == nil {
		a.redirectToLogin(w, r)
		return
	}
	target := r.FormValue("tenant_id")
	if !sess.CanAccess(target) {
		http.Error(w, a.t(r, "tenantpick.denied", "Your account does not have access to that tenant."), http.StatusForbidden)
		return
	}
	sess.TenantID = target
	if err := a.writeSession(w, sess); err != nil {
		http.Error(w, "switch failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, stripTenantPrefix(sanitizeReturnTo(r.FormValue("returnTo"))), http.StatusSeeOther)
}

// stripTenantPrefix drops a /t/{slug} prefix from a returnTo path: after
// a switch, the old tenant's prefixed URL must not be the landing target
// (its slug would just auto-switch the session straight back —
// ADR-0011). The un-prefixed remainder lands on the same page under the
// NEW tenant via internal/api's canonicalizing redirect.
func stripTenantPrefix(path string) string {
	rest, ok := strings.CutPrefix(path, "/t/")
	if !ok {
		return path
	}
	if _, sub, found := strings.Cut(rest, "/"); found {
		return "/" + sub
	}
	return "/"
}

// SwitchIfAccessible re-seals the session with tenantID active when the
// session may access it — the /t/{slug} auto-switch on GET (ADR-0011;
// internal/api's tenantPrefixed is the caller). Reports false, writing
// nothing, for no session / no webauth / an inaccessible tenant — the
// caller treats that as "the URL names a tenant this request can't
// have" and 404s. True with tenantID already active is a no-op success.
func (a *Authenticator) SwitchIfAccessible(w http.ResponseWriter, r *http.Request, tenantID string) bool {
	sess := a.rawSessionFromCookie(r)
	if sess == nil || !sess.CanAccess(tenantID) {
		return false
	}
	if sess.TenantID == tenantID {
		return true
	}
	sess.TenantID = tenantID
	if err := a.writeSession(w, sess); err != nil {
		log.Printf("webauth: re-seal session on tenant switch: %v", err)
		return false
	}
	return true
}

// guardChoice sends an authenticated-but-unresolved session to the
// picker — Guard's third outcome (#25): not a redirect to login (the
// user IS logged in; that loop would bounce through Zitadel forever)
// and not a pass-through (downstream must never see an empty TenantID).
func (a *Authenticator) guardChoice(w http.ResponseWriter, r *http.Request) bool {
	sess := a.rawSessionFromCookie(r)
	if sess == nil || !sess.NeedsChoice() {
		return false
	}
	http.Redirect(w, r, "/ui/auth/choose?returnTo="+url.QueryEscape(r.URL.Path), http.StatusFound)
	return true
}

var chooseTmpl = template.Must(template.New("choose").Parse(`<!doctype html><html lang="{{.Lang}}" dir="{{.Dir}}"><meta charset="utf-8"><title>{{.Title}}</title>
<body style="font-family:system-ui;max-width:32rem;margin:4rem auto;text-align:center">
<h1>{{.Title}}</h1>
<p>{{.Intro}}</p>
{{$rt := .ReturnTo}}
{{range .Tenants}}
<form method="post" action="/ui/auth/switch" style="margin:.5rem 0">
<input type="hidden" name="tenant_id" value="{{.ID}}">
<input type="hidden" name="returnTo" value="{{$rt}}">
<button type="submit" style="font-size:1.1rem;padding:.6rem 1.4rem;min-width:16rem">{{.Name}}{{if .Active}} ✓{{end}}</button>
</form>
{{end}}
</body>`))

type chooseView struct {
	Title    string
	Intro    string
	ReturnTo string
	Lang     string
	Dir      string
	Tenants  []TenantOption
}
