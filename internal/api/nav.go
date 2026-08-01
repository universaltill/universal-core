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
		Regions:     h.regionOptions(r, locale),
		// The nav is rendered from inside a page handler that already
		// persisted any explicit ?region=, so this read-only resolution
		// shows the choice the page itself is formatting with.
		ActiveRegion: regionalLocale(r, locale).Tag(),
		RegionLabel:  h.catalog.T(locale, "nav.region"),
	}

	if rc != nil {
		// Feeds layout.go's htmx:configRequest hook: every HTMX call this
		// page issues carries the tenant it was rendered for (ADR-0011's
		// stale-tab guard).
		view.TenantID = rc.TenantID
		ts, err := h.scope(r.Context(), *rc)
		if err != nil {
			log.Printf("api: nav: resolve tenant scope: %v", err)
		} else if modules, err := h.accessibleModules(r.Context(), ts, locale); err != nil {
			log.Printf("api: nav: build accessible modules: %v", err)
		} else {
			view.Modules = modules
		}
		// Always shown for any authenticated visitor, not gated on any
		// module (approvals aren't module-scoped — a workflow can trigger
		// against any entity type a tenant has, per triggerWorkflows).
		view.ShowApprovals = true
		view.ApprovalsLabel = h.catalog.T(locale, "nav.approvals")
		// Same reasoning: reporting a problem shouldn't require any
		// particular module — IssueReport is a foundation entity, always
		// present (see foundation.go's own doc comment on it).
		view.ShowIssueReport = true
		view.IssueReportLabel = h.catalog.T(locale, "nav.report_issue")
		// Same reasoning: BYOK AI-provider configuration isn't
		// module-scoped either — AIProviderConnection is a foundation
		// entity, always present (see aiprovidersettings.go).
		view.ShowAIProviderSettings = true
		view.AIProviderSettingsLabel = h.catalog.T(locale, "nav.ai_provider_settings")
		// Same reasoning again: ExternalSQLSource is a foundation entity,
		// always present (see extsqlsource.go).
		view.ShowSQLSourceSettings = true
		view.SQLSourceSettingsLabel = h.catalog.T(locale, "nav.sql_sources")
		// Members is the one nav link that IS role-gated: the page
		// itself 403s non-admins (members.go's requireMembersAccess), so
		// showing it to everyone would be a guaranteed dead end, not a
		// discoverability win. The same tenant_admin check, one indexed
		// query; a failure degrades to link-hidden (logged), same as the
		// module lookup above — never a page failure. Deliberately NOT
		// gated on h.members.Enabled(): an admin on an unconfigured
		// deployment still reaches the page's explicit "unavailable"
		// explanation instead of wondering where the feature went.
		if err == nil {
			if admin, aerr := isTenantAdmin(r.Context(), ts, *rc); aerr != nil {
				log.Printf("api: nav: tenant_admin check: %v", aerr)
			} else if admin {
				view.ShowMembers = true
				view.MembersLabel = h.catalog.T(locale, "nav.members")
			}
		}
		if h.auth.Enabled() {
			view.ShowLogout = true
			view.LogoutLabel = h.catalog.T(locale, "nav.logout")
			// The tenant switcher (#25, ADR-0011) — only when the session
			// can actually access more than one tenant (SessionTenants
			// returns nil otherwise), so single-tenant users never see it.
			if opts := h.auth.SessionTenants(r); len(opts) > 0 {
				view.SwitcherLabel = h.catalog.T(locale, "nav.tenant_switcher")
				// RequestURI, not Path: switching from a filtered/sorted
				// list page should land on the same view, not reset it.
				view.SwitchReturnTo = r.URL.RequestURI()
				for _, o := range opts {
					view.Tenants = append(view.Tenants, navTenantOption{ID: o.ID, Name: o.Name, Active: o.Active})
					if o.Active {
						view.ActiveTenantName = o.Name
					}
				}
			}
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
	Brand                   string
	Locale                  string
	CurrentPath             string
	TenantID                string
	Locales                 []string
	Regions                 []regionOption
	ActiveRegion            string
	Modules                 []moduleGroup
	ShowApprovals           bool
	ApprovalsLabel          string
	ShowIssueReport         bool
	IssueReportLabel        string
	ShowAIProviderSettings  bool
	AIProviderSettingsLabel string
	ShowSQLSourceSettings   bool
	SQLSourceSettingsLabel  string
	ShowMembers             bool
	MembersLabel            string
	ShowLogout              bool
	LogoutLabel             string
	SwitcherLabel           string
	RegionLabel             string
	ActiveTenantName        string
	SwitchReturnTo          string
	Tenants                 []navTenantOption
}

type navTenantOption struct {
	ID     string
	Name   string
	Active bool
}

var navTmpl = template.Must(template.New("nav").Parse(`
<nav class="uc-nav"{{if .TenantID}} data-uc-tenant="{{.TenantID}}"{{end}}>
<a class="uc-nav-brand" href="/">{{.Brand}}</a>
{{range .Modules}}<a class="uc-nav-link" href="/modules/{{.Key}}">{{.Name}}</a>{{end}}
{{if .ShowApprovals}}<a class="uc-nav-link" href="/workflow-jobs">{{.ApprovalsLabel}}</a>{{end}}
{{if .ShowIssueReport}}<a class="uc-nav-link" href="/issue-report/new">{{.IssueReportLabel}}</a>{{end}}
{{if .ShowAIProviderSettings}}<a class="uc-nav-link" href="/settings/ai-provider">{{.AIProviderSettingsLabel}}</a>{{end}}
{{if .ShowSQLSourceSettings}}<a class="uc-nav-link" href="/settings/sql-sources">{{.SQLSourceSettingsLabel}}</a>{{end}}
{{if .ShowMembers}}<a class="uc-nav-link" href="/settings/members">{{.MembersLabel}}</a>{{end}}
<span class="uc-nav-spacer"></span>
{{if .Tenants}}<details class="uc-nav-tenant"><summary title="{{.SwitcherLabel}}">{{.ActiveTenantName}}</summary>
<div class="uc-nav-tenant-menu">
{{$rt := .SwitchReturnTo}}{{range .Tenants}}{{if not .Active}}<form method="post" action="/ui/auth/switch" class="uc-nav-tenant-form"><input type="hidden" name="tenant_id" value="{{.ID}}"><input type="hidden" name="returnTo" value="{{$rt}}"><button type="submit">{{.Name}}</button></form>{{end}}{{end}}
</div>
</details>{{end}}
{{if .ShowLogout}}<a class="uc-nav-link" href="/ui/logout">{{.LogoutLabel}}</a>{{end}}
{{$path := .CurrentPath}}
{{$active := .Locale}}
{{range .Locales}}<a class="uc-nav-lang{{if eq . $active}} uc-nav-lang-active{{end}}" href="{{$path}}?lang={{.}}">{{.}}</a>{{end}}
{{if gt (len .Regions) 1}}{{$ar := .ActiveRegion}}<select class="uc-nav-region" aria-label="{{.RegionLabel}}" onchange="if(this.value)window.location=this.value">{{range .Regions}}<option value="{{.Href}}"{{if eq .Tag $ar}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{end}}
</nav>
`))
