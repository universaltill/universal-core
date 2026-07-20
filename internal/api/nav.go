package api

import (
	"bytes"
	"html/template"
	"log"
	"net/http"

	"github.com/universaltill/universal-core/internal/httpx"
)

// renderNav builds the top nav bar shared by every page (see layout.go's
// shellTmpl) — a brand link back to "/", one link per module the tenant
// has access to (reusing accessibleModules — the module list a nav
// switches between is exactly the same list the dashboard's hub tiles
// and each module's own menu page use, so there's one source of the
// registry lookup, not three), and a language switcher (see locale.go —
// this is the one visible control that actually makes the app
// multilingual, not just its i18n catalog).
//
// rc is nil for an anonymous visitor (the welcome page, see
// renderWelcome) — nav degrades to brand-and-language-switcher-only
// rather than erroring, since there's no tenant to list modules for. A
// registry lookup failure degrades the same way (logged, not
// surfaced): nav is page chrome, not the content the visitor actually
// came for, so a transient DB hiccup building it must never turn into a
// hard failure for the whole page.
//
// Log out only shows when there's both a session (rc != nil) AND real
// login is enabled: /ui/logout is only ever registered by
// webauth.Authenticator.Register when Enabled() (see webauth.go) — on a
// dev-auth-only deployment the route doesn't exist at all, so showing
// the link there would be a dead link to a 404, not a working control.
func (h *Handler) renderNav(r *http.Request, rc *httpx.RequestContext, locale string) template.HTML {
	view := navView{
		Brand:       h.catalog.T(locale, "dashboard.title"),
		Locale:      locale,
		CurrentPath: r.URL.Path,
		Locales:     supportedLocaleList,
	}

	if rc != nil {
		modules, err := h.accessibleModules(r.Context(), rc.TenantID, locale)
		if err != nil {
			log.Printf("api: nav: build accessible modules: %v", err)
		} else {
			view.Modules = modules
		}
		if h.auth.Enabled() {
			view.ShowLogout = true
			view.LogoutLabel = h.catalog.T(locale, "nav.logout")
		}
	}

	var buf bytes.Buffer
	if err := navTmpl.Execute(&buf, view); err != nil {
		log.Printf("api: render nav: %v", err)
		return ""
	}
	return template.HTML(buf.String())
}

type navView struct {
	Brand       string
	Locale      string
	CurrentPath string
	Locales     []string
	Modules     []moduleGroup
	ShowLogout  bool
	LogoutLabel string
}

var navTmpl = template.Must(template.New("nav").Parse(`
<nav class="uc-nav">
<a class="uc-nav-brand" href="/">{{.Brand}}</a>
{{range .Modules}}<a class="uc-nav-link" href="/modules/{{.Key}}">{{.Name}}</a>{{end}}
<span class="uc-nav-spacer"></span>
{{if .ShowLogout}}<a class="uc-nav-link" href="/ui/logout">{{.LogoutLabel}}</a>{{end}}
{{$path := .CurrentPath}}
{{$active := .Locale}}
{{range .Locales}}<a class="uc-nav-lang{{if eq . $active}} uc-nav-lang-active{{end}}" href="{{$path}}?lang={{.}}">{{.}}</a>{{end}}
</nav>
`))
