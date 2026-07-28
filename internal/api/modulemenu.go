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
// Graphical since 2026-07-26: originally a plain <ul>, changed after
// Farshid compared it directly to the dashboard's own hub-and-spoke
// module switcher ("the main page menu is amazing but when I go to
// purchasing... that is only a list of menu... put them in graphical
// mode and searchable") — this now renders each entity type as its own
// node in the exact same hub graphic (dashboard.go's hubNode/hubView/
// layoutHubNodes/hubTmpl), centered on the module's own name instead of
// the app's, so "graphical, connected-node menu" reads as one visual
// language throughout the app rather than the dashboard being a special
// case. Search still works the same way it always did (data-search
// substring match, filtering which nodes stay visible) — see hubTmpl's
// own doc comment for why that JS moved there once two different pages
// needed the identical behavior.
//
// Each node shows both a translated display name (locale.go's
// entityDisplayName, via "entity.{EntityType}.name") and its technical
// code (the raw EntityType, e.g. "PurchaseOrder") as a sub-label —
// search matches either, in any locale, matching "search by name and
// code" the way a real ERP's quick-search does (SAP's transaction-code
// search, Odoo's app search) rather than only matching one language's
// label. A node's own New/Import/Export shortcuts were dropped in this
// change (the flat list used to show them inline per row) — the entity's
// own list page (listview.go) already has all three in its toolbar, the
// same one click further away a module-switcher node's own target page
// already requires, so nothing this menu previously linked to became
// unreachable.
func (h *Handler) renderModuleMenu(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc.TenantID)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	key := r.PathValue("key")
	locale := localeFromRequest(w, r)

	modules, err := h.accessibleModules(r.Context(), ts, locale)
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
		Name: group.Name,
		Hub:  entityHubLayout(h, locale, group.Name, group.Entities),
	}
	for _, link := range moduleReportLinks[key] {
		view.Reports = append(view.Reports, moduleMenuReport{
			Label: h.catalog.T(locale, link.LabelKey),
			Href:  link.Href,
		})
	}

	var buf bytes.Buffer
	if err := moduleMenuTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render module menu", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render module menu shell", err)
	}
}

// entityHubLayout builds the hub graphic for one module's entity types —
// the same layoutHubNodes geometry engine the dashboard's own module
// switcher uses, centered on the module's own name rather than the
// app's. No placeholder/"coming soon" nodes here (unlike the dashboard):
// there's no equivalent "planned but not built yet" concept at the
// entity-type level, every node is a real, clickable target.
func entityHubLayout(h *Handler, locale, centerLabel string, entities []moduleEntity) hubView {
	entries := make([]hubEntry, len(entities))
	for i, e := range entities {
		name := h.entityDisplayName(locale, e.EntityType)
		entries[i] = hubEntry{
			Key:       e.EntityType,
			Name:      name,
			Code:      e.EntityType,
			Icon:      entityIcon(e.EntityType),
			Href:      "/records/" + e.EntityType,
			SearchKey: strings.ToLower(name + " " + e.EntityType),
		}
	}
	nodes, containerPx := layoutHubNodes(entries)
	return hubView{
		Nodes:             nodes,
		CenterLabel:       centerLabel,
		SearchPlaceholder: h.catalog.T(locale, "modulemenu.search_placeholder"),
		ContainerPx:       containerPx,
		CenterPx:          containerPx / 2,
		CenterSizePx:      hubCenterSizePx,
	}
}

// entityIcons is a plain emoji per known entity type — same "zero-
// dependency, good enough to read as graphical/colorful" reasoning as
// dashboard.go's moduleIcons, one level deeper. An entity type with no
// mapping (a module this kernel doesn't have icons picked for yet) falls
// back to a generic document icon rather than an empty/broken node.
var entityIcons = map[string]string{
	"Party":                "👤",
	"PartyRole":            "🎭",
	"PartyRelationship":    "🔗",
	"Address":              "📍",
	"ContactMechanism":     "☎️",
	"Attachment":           "📎",
	"UnitOfMeasure":        "📏",
	"UomConversion":        "🔁",
	"Currency":             "💱",
	"ExchangeRate":         "💹",
	"StatusType":           "🚦",
	"Status":               "🏷️",
	"StatusTransition":     "➡️",
	"IssueReport":          "🐛",
	"AIProviderConnection": "🤖",
	"Item":                 "📦",
	"PurchaseOrder":        "🧾",
	"POLine":               "📝",
	"InventoryItem":        "🗄️",
	"GoodsReceipt":         "🚚",
	"GoodsReceiptLine":     "📥",
}

func entityIcon(entityType string) string {
	if icon, ok := entityIcons[entityType]; ok {
		return icon
	}
	return "📄"
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
	Name    string
	Hub     hubView
	Reports []moduleMenuReport
}

type moduleMenuReport struct {
	Label string
	Href  string
}

var moduleMenuTmpl = template.Must(hubTmpl.New("moduleMenu").Parse(`
<h1>{{.Name}}</h1>
{{if .Reports}}
<p class="uc-module-reports">
{{range .Reports}}<a href="{{.Href}}">{{.Label}}</a> {{end}}
</p>
{{end}}
{{template "hub" .Hub}}
`))
