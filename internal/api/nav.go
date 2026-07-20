package api

import (
	"bytes"
	"context"
	"html/template"
	"log"

	"github.com/universaltill/universal-core/internal/httpx"
)

// renderNav builds the top nav bar shared by every page (see layout.go's
// shellTmpl) — a brand link back to "/" plus one link per module the
// requesting tenant currently has both a published entity and form
// Definition for (reusing dashboardModules — the module list a nav
// switches between is exactly the same list the dashboard itself
// tiles, so there's one source of the registry lookup, not two).
//
// rc is nil for an anonymous visitor (the welcome page, see
// renderWelcome) — nav degrades to brand-only rather than erroring,
// since there's no tenant to list modules for. A registry lookup
// failure degrades the same way (logged, not surfaced): nav is page
// chrome, not the content the visitor actually came for, so a transient
// DB hiccup building it must never turn into a hard failure for the
// whole page.
func (h *Handler) renderNav(ctx context.Context, rc *httpx.RequestContext, locale string) template.HTML {
	view := navView{Brand: h.catalog.T(locale, "dashboard.title")}

	if rc != nil {
		entityTypes, err := h.entityDefs.ListPublishedEntityTypes(ctx, rc.TenantID)
		if err != nil {
			log.Printf("api: nav: list published entity types: %v", err)
		} else {
			modules, err := h.dashboardModules(ctx, rc.TenantID, entityTypes)
			if err != nil {
				log.Printf("api: nav: build modules: %v", err)
			} else {
				view.Modules = modules
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
	Brand   string
	Modules []dashboardModule
}

var navTmpl = template.Must(template.New("nav").Parse(`
<nav class="uc-nav">
<a class="uc-nav-brand" href="/">{{.Brand}}</a>
{{range .Modules}}<a class="uc-nav-link" href="/records/{{.EntityType}}">{{.EntityType}}</a>{{end}}
</nav>
`))
