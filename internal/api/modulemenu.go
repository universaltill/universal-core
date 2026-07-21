package api

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/httpx"
)

// renderModuleMenu is what a click on one of the dashboard's hub nodes
// (or a nav link) lands on: a searchable menu of the entity types inside
// one module, each linking to its own record list page. Farshid asked
// for this after seeing the flat nav/dashboard was really "menus from
// one module" with nowhere to search — big ERPs put a search-by-name-
// or-code box inside a module's own menu (SAP's transaction-code search,
// Odoo's app search), not just a flat unfiltered list.
//
// Each row shows both a translated display name (locale.go's
// entityDisplayName, via "entity.{EntityType}.name") and its technical
// code (the raw EntityType, e.g. "PurchaseOrder") — search matches
// either, in any locale, matching "search by name and code" the way a
// real ERP's quick-search does (SAP's transaction-code search, Odoo's
// app search) rather than only matching one language's label. Filtering
// is a few lines of plain JS in the page itself (see moduleMenuTmpl) —
// no framework, matching this kernel's general dependency-light
// preference — not a server round trip, since the whole list is already
// on the page.
func (h *Handler) renderModuleMenu(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	locale := localeFromRequest(w, r)

	modules, err := h.accessibleModules(r.Context(), rc.TenantID, locale)
	if err != nil {
		writeInternalError(w, "build accessible modules", err)
		return
	}
	var group *moduleGroup
	for i := range modules {
		if modules[i].Key == key {
			group = &modules[i]
			break
		}
	}
	if group == nil {
		// Either a typo'd/unknown key, or a real module this tenant
		// just doesn't have access to (same "don't link to it if you
		// can't reach it" reasoning accessibleModules already applies
		// to individual entities) — both are a 404, not a 401/403:
		// this route never distinguishes "doesn't exist" from
		// "exists but you can't see it," matching every other
		// registry-lookup 404 in this package (writeDefinitionLookupError).
		httpx.WriteError(w, http.StatusNotFound, "module not found")
		return
	}

	view := moduleMenuView{
		Name:              group.Name,
		SearchPlaceholder: h.catalog.T(locale, "modulemenu.search_placeholder"),
		NewLabel:          h.catalog.T(locale, "dashboard.new_link"),
		ImportLink:        h.catalog.T(locale, "dashboard.import_link"),
	}
	for _, link := range moduleReportLinks[key] {
		view.Reports = append(view.Reports, moduleMenuReport{
			Label: h.catalog.T(locale, link.LabelKey),
			Href:  link.Href,
		})
	}
	for _, e := range group.Entities {
		name := h.entityDisplayName(locale, e.EntityType)
		view.Entities = append(view.Entities, moduleMenuEntity{
			Name:       name,
			Code:       e.EntityType,
			SearchKey:  strings.ToLower(name + " " + e.EntityType),
			ListHref:   "/records/" + e.EntityType,
			NewHref:    "/forms/" + e.EntityType + "/new",
			ImportHref: "/import/" + e.EntityType,
		})
	}

	var buf bytes.Buffer
	if err := moduleMenuTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render module menu", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render module menu shell", err)
	}
}

// moduleReportLinks are hand-declared report pages that live one level
// inside a module's menu rather than being a browsable/importable entity
// type of their own (a report is a read-only view over other entities'
// data, not a Definition an admin publishes) — same reasoning
// dashboard.go's moduleIcons gives for a plain hardcoded map: exactly
// one small, stable piece of presentation data, not worth a registry of
// its own for a single report per module today.
var moduleReportLinks = map[string][]struct{ LabelKey, Href string }{
	"purchasing": {{"report.purchasing.nav_label", "/reports/purchasing"}},
}

type moduleMenuView struct {
	Name              string
	SearchPlaceholder string
	NewLabel          string
	ImportLink        string
	Reports           []moduleMenuReport
	Entities          []moduleMenuEntity
}

type moduleMenuReport struct {
	Label string
	Href  string
}

type moduleMenuEntity struct {
	Name       string
	Code       string
	SearchKey  string
	ListHref   string
	NewHref    string
	ImportHref string
}

var moduleMenuTmpl = template.Must(template.New("moduleMenu").Parse(`
<h1>{{.Name}}</h1>
{{if .Reports}}
<p class="uc-module-reports">
{{range .Reports}}<a href="{{.Href}}">{{.Label}}</a> {{end}}
</p>
{{end}}
<input type="text" class="uc-menu-search" placeholder="{{.SearchPlaceholder}}"
  oninput="document.querySelectorAll('.uc-menu-item').forEach(function(el){
    el.style.display = el.dataset.search.indexOf(this.value.toLowerCase()) === -1 ? 'none' : '';
  }, this)">
<ul class="uc-menu-list">
{{range .Entities}}
<li class="uc-menu-item" data-search="{{.SearchKey}}">
<a class="uc-menu-item-link" href="{{.ListHref}}">{{.Name}} <span class="uc-menu-item-code">{{.Code}}</span></a>
<span class="uc-module-actions"><a href="{{.NewHref}}">{{$.NewLabel}}</a> · <a href="{{.ImportHref}}">{{$.ImportLink}}</a></span>
</li>
{{end}}
</ul>
`))
