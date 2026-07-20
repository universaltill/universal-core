package api

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"sort"
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
	if err := dashboardTmpl.Execute(&buf, dashboardView{Modules: modules}); err != nil {
		writeInternalError(w, "render dashboard", err)
		return
	}
	if err := renderShell(w, buf.String()); err != nil {
		writeInternalError(w, "render dashboard shell", err)
	}
}

func (h *Handler) dashboardModules(ctx context.Context, tenantID string, entityTypes []string) ([]dashboardModule, error) {
	var modules []dashboardModule
	for _, entityType := range entityTypes {
		if _, err := h.formDefs.GetPublished(ctx, tenantID, entityType); err != nil {
			continue // no published form for this entity type — nothing to link to yet
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
	Modules []dashboardModule
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`
<h1>Universal Core</h1>
{{if not .Modules}}
<p>No modules are available yet for this tenant.</p>
{{else}}
<ul class="uc-dashboard-modules">
{{range .Modules}}
<li>
<strong>{{.EntityType}}</strong>
— <a href="/forms/{{.EntityType}}/new">New</a>
· <a href="/import/{{.EntityType}}">Import</a>
</li>
{{end}}
</ul>
{{end}}
`))
