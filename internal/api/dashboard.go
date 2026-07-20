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
)

// renderDashboard is what a browser lands on at "/" — until this
// existed, the root path 404'd outright (there was no handler
// registered for it at all): a real user with a real, working login
// had nowhere to actually go without already knowing a specific
// /forms/{entityType}/... URL by heart. Lists every entity type the
// requesting tenant currently has BOTH a published entity Definition
// AND a published Form Definition for — reading the registry, not a
// hardcoded module list, so this never needs a code change when a new
// module is provisioned (CLAUDE.md's kernel/deterministic-core
// boundary rule: no entity-type branching in a generic engine).
//
// An entity type with a published entity Definition but no published
// form (e.g. a foundation entity nobody has built a screen for yet —
// see foundation.go's own doc comment on building forms "as each is
// actually needed") is deliberately left off the list: a link to
// /forms/{that type}/new would just 404, which is a worse experience
// than not offering it at all.
func (h *Handler) renderDashboard(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
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
	if err := renderShell(w, buf.String()); err != nil {
		writeInternalError(w, "render dashboard shell", err)
	}
}

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
<ul class="uc-dashboard-modules">
{{range .Modules}}
<li>
<strong>{{.EntityType}}</strong>
— <a href="/forms/{{.EntityType}}/new">{{$.NewLabel}}</a>
· <a href="/import/{{.EntityType}}">{{$.ImportLink}}</a>
</li>
{{end}}
</ul>
{{end}}
`))
