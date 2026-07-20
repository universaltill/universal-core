package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
)

// renderRoot is what every browser lands on at "/" — until this existed,
// the root path either 404'd outright or, once auth middleware was added,
// hard-401'd every visitor (including one with no way to log in yet, on a
// deployment where webauth isn't configured) before anything ever
// rendered. It is registered unauthenticated (see handlers.go's Routes)
// and does its own optional session check instead: a visitor with a
// valid session (webauth cookie or, locally, DevAuth headers) gets the
// module dashboard; everyone else gets a real HTML welcome page — never
// the raw {"data":null,"error":...} JSON body an API 401 returns, which
// is correct for API clients but not for a browser landing on the site.
func (h *Handler) renderRoot(w http.ResponseWriter, r *http.Request) {
	if rc, ok := h.sessionContext(r); ok {
		h.writeDashboard(w, r, rc)
		return
	}
	h.renderWelcome(w, r)
}

// sessionContext checks both auth backends without enforcing either —
// webauth's cookie first (the real login path), then DevAuth's headers
// (the local/insecure stopgap) — mirroring the same fallback order
// Routes()'s auth() wrapper uses, just non-redirecting and non-401ing.
func (h *Handler) sessionContext(r *http.Request) (httpx.RequestContext, bool) {
	if rc, ok := h.auth.TryContext(r); ok {
		return rc, true
	}
	return httpx.TryDevAuth(r)
}

// writeDashboard lists every entity type the given tenant currently has
// BOTH a published entity Definition AND a published Form Definition
// for — reading the registry, not a hardcoded module list, so this never
// needs a code change when a new module is provisioned (CLAUDE.md's
// kernel/deterministic-core boundary rule: no entity-type branching in a
// generic engine).
//
// An entity type with a published entity Definition but no published
// form (e.g. a foundation entity nobody has built a screen for yet —
// see foundation.go's own doc comment on building forms "as each is
// actually needed") is deliberately left off the list: a link to
// /forms/{that type}/new would just 404, which is a worse experience
// than not offering it at all.
func (h *Handler) writeDashboard(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext) {
	locale := localeFromRequest(r)

	entityTypes, err := h.entityDefs.ListPublishedEntityTypes(r.Context(), rc.TenantID)
	if err != nil {
		writeInternalError(w, "list published entity types", err)
		return
	}

	modules, err := h.dashboardModules(r.Context(), rc.TenantID, entityTypes)
	if err != nil {
		writeInternalError(w, "build dashboard modules", err)
		return
	}

	var buf bytes.Buffer
	err = dashboardTmpl.Execute(&buf, dashboardView{
		Modules:    modules,
		Title:      h.catalog.T(locale, "dashboard.title"),
		Empty:      h.catalog.T(locale, "dashboard.empty"),
		NewLabel:   h.catalog.T(locale, "dashboard.new_link"),
		ImportLink: h.catalog.T(locale, "dashboard.import_link"),
	})
	if err != nil {
		writeInternalError(w, "render dashboard", err)
		return
	}
	nav := h.renderNav(r.Context(), &rc, locale)
	if err := renderShell(w, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render dashboard shell", err)
	}
}

// renderWelcome is the anonymous-visitor view of "/": what to show
// depends on which auth backend (if any) this deployment actually has
// configured, since "sign in" only means something when webauth is
// enabled — otherwise the honest message is that this deployment has no
// working sign-in yet, not a link that would just redirect nowhere.
func (h *Handler) renderWelcome(w http.ResponseWriter, r *http.Request) {
	locale := localeFromRequest(r)
	view := welcomeView{
		Title: h.catalog.T(locale, "dashboard.title"),
	}
	switch {
	case h.auth.Enabled():
		view.Message = h.catalog.T(locale, "welcome.sign_in_prompt")
		view.LoginURL = "/ui/login"
		view.LoginLabel = h.catalog.T(locale, "welcome.sign_in_link")
	case httpx.DevAuthEnabled():
		view.Message = h.catalog.T(locale, "welcome.dev_auth_hint")
	default:
		view.Message = h.catalog.T(locale, "welcome.no_auth_configured")
	}

	var buf bytes.Buffer
	if err := welcomeTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render welcome", err)
		return
	}
	nav := h.renderNav(r.Context(), nil, locale)
	if err := renderShell(w, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render welcome shell", err)
	}
}

type welcomeView struct {
	Title      string
	Message    string
	LoginURL   string
	LoginLabel string
}

var welcomeTmpl = template.Must(template.New("welcome").Parse(`
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .LoginURL}}
<p><a href="{{.LoginURL}}">{{.LoginLabel}}</a></p>
{{end}}
`))

// dashboardModules filters entityTypes down to the ones with a
// published form. A genuine lookup failure (anything other than "no
// form published for this entity type") is returned, not swallowed —
// found by independent review: the original version treated every
// GetPublished error identically, so a transient DB error would
// silently drop a module from the dashboard and still return 200, with
// no log line anywhere pointing at why.
func (h *Handler) dashboardModules(ctx context.Context, tenantID string, entityTypes []string) ([]dashboardModule, error) {
	var modules []dashboardModule
	for _, entityType := range entityTypes {
		_, err := h.formDefs.GetPublished(ctx, tenantID, entityType)
		if errors.Is(err, data.ErrNotFound) {
			continue // no published form for this entity type — nothing to link to yet
		}
		if err != nil {
			return nil, fmt.Errorf("look up form for %s: %w", entityType, err)
		}
		modules = append(modules, dashboardModule{EntityType: entityType})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].EntityType < modules[j].EntityType })
	return modules, nil
}

type dashboardModule struct {
	EntityType string
}

type dashboardView struct {
	Modules    []dashboardModule
	Title      string
	Empty      string
	NewLabel   string
	ImportLink string
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`
<h1>{{.Title}}</h1>
{{if not .Modules}}
<p>{{.Empty}}</p>
{{else}}
<ul class="uc-modules">
{{range .Modules}}
<li class="uc-module-card">
<strong><a href="/records/{{.EntityType}}">{{.EntityType}}</a></strong>
<span class="uc-module-actions"><a href="/forms/{{.EntityType}}/new">{{$.NewLabel}}</a> · <a href="/import/{{.EntityType}}">{{$.ImportLink}}</a></span>
</li>
{{end}}
</ul>
{{end}}
`))
