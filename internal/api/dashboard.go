package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
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
		// Canonicalize a real webauth browser session's dashboard to its
		// /t/{slug}/ form (#25, ADR-0011) — same narrow conditions as
		// canonicalPage (tenanturl.go), inlined here because this route
		// deliberately sits outside the auth() chain. DevAuth sessions
		// (TryContext misses, TryDevAuth hits) never redirect.
		if !dispatchedViaPrefix(r.Context()) && h.tenants != nil && h.auth.Enabled() {
			if _, viaWebauth := h.auth.TryContext(r); viaWebauth {
				if slug, err := h.tenants.SlugByID(r.Context(), rc.TenantID); err == nil {
					// RequestURI, not a bare "/": dropping the query here
					// silently broke ?lang= on the dashboard for real
					// logins (independent review, 2026-07-31) — the
					// language switcher's links carry their locale as a
					// query parameter.
					http.Redirect(w, r, "/t/"+slug+r.URL.RequestURI(), http.StatusFound)
					return
				}
			}
		}
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

// writeDashboard is the home page: one tile per MODULE the tenant has
// access to — not a flat list of entity types (that was the original
// version; Farshid pointed out after logging in for real that a flat
// list is actually "menus from one module", not a module switcher, and
// asked for the two-level structure every big ERP actually uses: modules
// on the first page, each module's own searchable menu of screens
// inside it). "Access" today means accessibleModules' definition of it
// (see that function's own doc comment) — there's no separate
// per-module entitlement/licensing system yet (BACKLOG.md's R13).
func (h *Handler) writeDashboard(w http.ResponseWriter, r *http.Request, rc httpx.RequestContext) {
	locale := localeFromRequest(w, r)

	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	modules, err := h.accessibleModules(r.Context(), ts, locale)
	if err != nil {
		writeInternalError(w, "build accessible modules", err)
		return
	}

	title := h.catalog.T(locale, "dashboard.title")
	hub := hubLayout(modules, locale, h.catalog, title, h.catalog.T(locale, "module.coming_soon"))

	var buf bytes.Buffer
	err = dashboardTmpl.Execute(&buf, dashboardView{
		Hub:   hub,
		Title: title,
		Empty: h.catalog.T(locale, "dashboard.empty"),
	})
	if err != nil {
		writeInternalError(w, "render dashboard", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render dashboard shell", err)
	}
}

// renderWelcome is the anonymous-visitor view of "/": what to show
// depends on which auth backend (if any) this deployment actually has
// configured, since "sign in" only means something when webauth is
// enabled — otherwise the honest message is that this deployment has no
// working sign-in yet, not a link that would just redirect nowhere.
func (h *Handler) renderWelcome(w http.ResponseWriter, r *http.Request) {
	locale := localeFromRequest(w, r)
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
	nav := h.renderNav(r, nil, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
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

// moduleEntity is one entity type within a module group — the unit
// accessibleModules groups, and modulemenu.go's own list items.
type moduleEntity struct {
	EntityType string
}

// moduleGroup is one module the tenant has access to: a key (matches
// entity.Definition.Module, e.g. "purchasing"), an i18n'd display name,
// and the entity types inside it.
type moduleGroup struct {
	Key      string
	Name     string
	Icon     string
	Entities []moduleEntity
}

// accessibleModules groups every entity type the tenant currently has
// BOTH a published entity Definition AND a published Form Definition
// for (same "nothing to link to yet otherwise" reasoning the original
// flat dashboard used) by entity.Definition.Module — reading the
// registry, not a hardcoded module list, so a new module shows up
// correctly grouped the moment its own package sets Module on its
// Definitions (CLAUDE.md's kernel boundary rule: no entity-type
// branching in a generic engine — this reads data, it doesn't special-
// case any specific module or entity type by name).
//
// "Access" here means exactly "this tenant has this published" — there
// is no separate per-module entitlement/licensing system yet
// (BACKLOG.md's R13 is that future work); until it exists, published-
// for-tenant is the only notion of access this kernel has.
//
// An entity type whose Definition never set Module (shouldn't happen
// for anything in this repo today, but degrades safely rather than
// panicking or dropping the entity) falls into a "general" bucket.
func (h *Handler) accessibleModules(ctx context.Context, ts tenantScope, locale string) ([]moduleGroup, error) {
	entityTypes, err := ts.entityDefs.ListPublishedEntityTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list published entity types: %w", err)
	}

	byKey := map[string][]moduleEntity{}
	for _, entityType := range entityTypes {
		if _, err := ts.formDefs.GetPublished(ctx, entityType); err != nil {
			if errors.Is(err, data.ErrNotFound) {
				continue // no published form for this entity type — nothing to link to yet
			}
			return nil, fmt.Errorf("look up form for %s: %w", entityType, err)
		}
		def, err := ts.entityDef(ctx, entityType)
		if err != nil {
			if errors.Is(err, data.ErrNotFound) {
				// entityType came from ListPublishedEntityTypes moments
				// ago, so this is a narrow race (a Rollback landing
				// between that call and this one), not a caller
				// mistake — same skip-don't-fail treatment as the form
				// lookup above. Found by review: the original version
				// hard-failed the whole function here, and since this
				// runs on every page via renderNav (not just the
				// dashboard), that race could have broken page chrome
				// anywhere, not just the home page.
				continue
			}
			return nil, fmt.Errorf("look up entity definition for %s: %w", entityType, err)
		}
		// Don't offer a link to something this user would only be refused
		// at (ADR-0006's field-level commit). Filtering here rather than
		// in each caller covers the nav bar, the dashboard hub, and a
		// module's own menu page in one place, and means a module whose
		// entity types are ALL denied disappears entirely instead of
		// becoming a tile leading to a 403.
		//
		// Affordable because the resolver loads the tenant's whole policy
		// set once per request and answers from memory (see
		// authz.Resolver.loadRules) — this runs on every page via
		// renderNav, so a per-entity-type query here would have been a
		// per-page N+1.
		readable, err := ts.crud.CanRead(ctx, entityType)
		if err != nil {
			return nil, fmt.Errorf("check read permission for %s: %w", entityType, err)
		}
		if !readable {
			continue
		}
		key := def.Module
		if key == "" {
			key = "general"
		}
		byKey[key] = append(byKey[key], moduleEntity{EntityType: entityType})
	}

	modules := make([]moduleGroup, 0, len(byKey))
	for key, entities := range byKey {
		sort.Slice(entities, func(i, j int) bool { return entities[i].EntityType < entities[j].EntityType })
		modules = append(modules, moduleGroup{
			Key:      key,
			Name:     h.catalog.T(locale, "module."+key+".name"),
			Icon:     moduleIcon(key),
			Entities: entities,
		})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Key < modules[j].Key })
	return modules, nil
}

// plannedModuleKeys are the standard ERP domains this kernel doesn't
// have a real module for yet (BACKLOG.md's R8 domain list) — shown on
// the hub as non-clickable "coming soon" nodes rather than left off
// entirely, per Farshid asking for "all ERP modules going to their
// area but just say coming soon." Purely presentational: unlike
// moduleGroup (built from the registry), these aren't tied to any
// Entity Definition or tenant access — a real module landing later
// under one of these keys (see foundation.go/purchasing.go's own
// Module field) automatically takes over its slot instead of
// duplicating it (see hubNodes' dedup below), no code change needed
// here.
var plannedModuleKeys = []string{
	"finance", "sales", "manufacturing", "inventory", "warehouse",
	"supplychain", "crm", "projects", "hr", "ecommerce", "marketing",
	"reporting",
}

// moduleIcons is a plain emoji per module key — zero-dependency
// (no icon font, no SVG sprite sheet, no CDN), matching this kernel's
// "vendor nothing" static-asset pattern elsewhere (htmxJS, appCSS).
// Good enough to make the hub read as colorful/graphical the way
// Farshid's SAP/ERP reference infographics did; a real icon set is a
// fair future upgrade, not a blocker for this.
var moduleIcons = map[string]string{
	"foundation":    "🧱",
	"purchasing":    "🛒",
	"general":       "📁",
	"finance":       "💰",
	"sales":         "📈",
	"manufacturing": "🏭",
	"inventory":     "📦",
	"warehouse":     "🏬",
	"supplychain":   "🚚",
	"assets":        "🏗️",
	"crm":           "🤝",
	"projects":      "📋",
	"hr":            "👥",
	"ecommerce":     "🛍️",
	"marketing":     "📣",
	"reporting":     "📊",
}

func moduleIcon(key string) string {
	if icon, ok := moduleIcons[key]; ok {
		return icon
	}
	return "🔷"
}

// dashboardView is the hub-and-spoke home page: Universal Core at the
// center, one connected node per module around it — real, clickable
// modules the tenant has access to, plus every other standard ERP
// domain as a muted, non-clickable "coming soon" node (plannedModuleKeys)
// — the graphical module switcher Farshid asked for after seeing
// SAP/ERP infographics, instead of a flat tile grid or a hub with only
// 1-2 nodes on it. Node positions/sizes are computed server-side (see
// hubGeometry/hubLayout) since html/template has no trigonometry of
// its own, and because the layout must adapt to however many nodes
// there actually are (2 real modules today, 14 total with placeholders)
// without nodes overlapping.
type dashboardView struct {
	Hub   hubView
	Title string
	Empty string
}

// hubNode is one positioned, colored node in a hub-and-spoke graphic — a
// real clickable target (Href set) or a muted placeholder (Placeholder
// true, no Href). Shared by two different hubs: the module switcher
// (dashboard.go, nodes = modules the tenant has access to) and, since
// Farshid asked for the same graphical/searchable treatment one level
// deeper ("purchasing is only a list of menu... put them in a graphical
// mode"), a module's own menu (modulemenu.go, nodes = entity types
// inside that module) — one visual language throughout the app instead
// of two implementations that could drift. Code/SearchKey are unused
// (zero value) by the module switcher itself; a module's own entity hub
// sets both (Code shown as a sub-label, SearchKey feeds the same
// data-search filtering the flat list already had).
type hubNode struct {
	Key         string
	Name        string
	Code        string
	Icon        string
	Href        string
	SearchKey   string
	Placeholder bool
	X, Y        int
	SizePx      int
	ColorIndex  int
}

// hubEntry is one not-yet-positioned candidate for layoutHubNodes —
// the common shape both hubLayout (dashboard.go) and modulemenu.go's
// own entity layout build before handing off to the shared geometry
// engine.
type hubEntry struct {
	Key, Name, Code, Icon, Href, SearchKey string
	Placeholder                            bool
}

// layoutHubNodes positions entries evenly around a circle sized to fit
// them all without overlapping (see hubGeometry) — the shared geometry
// engine behind both hubs described on hubNode's own doc comment.
func layoutHubNodes(entries []hubEntry) ([]hubNode, int) {
	n := len(entries)
	containerPx, radiusPx, nodeSizePx := hubGeometry(n)
	centerPx := containerPx / 2
	nodes := make([]hubNode, n)
	for i, e := range entries {
		angleDeg := -90.0 + float64(i)*(360.0/float64(n))
		rad := angleDeg * math.Pi / 180
		nodes[i] = hubNode{
			Key:         e.Key,
			Name:        e.Name,
			Code:        e.Code,
			Icon:        e.Icon,
			Href:        e.Href,
			SearchKey:   e.SearchKey,
			Placeholder: e.Placeholder,
			X:           centerPx + int(float64(radiusPx)*math.Cos(rad)),
			Y:           centerPx + int(float64(radiusPx)*math.Sin(rad)),
			SizePx:      nodeSizePx,
			ColorIndex:  i % hubColorCount,
		}
	}
	return nodes, containerPx
}

// hubView is the data every hub-graphic page (dashboard.go,
// modulemenu.go) renders via the shared "hub" template block —
// see hubNode's own doc comment on why this is factored out rather than
// duplicated. SearchPlaceholder is "" for the module-switcher hub (no
// search box there today — few enough real+planned modules that one
// isn't needed yet) and non-empty for a module's own entity hub.
type hubView struct {
	Nodes             []hubNode
	CenterLabel       string
	ComingSoon        string
	SearchPlaceholder string
	ContainerPx       int
	CenterPx          int
	CenterSizePx      int
}

// hubTmpl holds the one shared "hub" block both dashboardTmpl and
// modulemenu.go's own template render via {{template "hub" .Hub}} —
// html/template requires an associated template to already exist by
// name in the *same* template.Template value a Parse call is attached
// to, so this is parsed once here and reused as the base every other
// hub-rendering template.Must(...).Parse(...) call parses its own
// content into (Go's html/template.Parse *adds* to an existing
// template set rather than replacing it, which is exactly what makes
// this composition work).
var hubTmpl = template.Must(template.New("hub").Parse(`
{{define "hub"}}
{{if .SearchPlaceholder}}
<input type="text" class="uc-menu-search" placeholder="{{.SearchPlaceholder}}"
  oninput="document.querySelectorAll('[data-search]').forEach(function(el){
    el.style.display = el.dataset.search.indexOf(this.value.toLowerCase()) === -1 ? 'none' : '';
  }, this)">
{{end}}
<div class="uc-hub-wrap">
<div class="uc-hub" style="width:{{.ContainerPx}}px;height:{{.ContainerPx}}px;">
<svg class="uc-hub-lines" width="{{.ContainerPx}}" height="{{.ContainerPx}}">
{{$center := .CenterPx}}
{{range .Nodes}}<line data-search="{{.SearchKey}}" x1="{{$center}}" y1="{{$center}}" x2="{{.X}}" y2="{{.Y}}"></line>{{end}}
</svg>
<div class="uc-hub-center" style="left:{{.CenterPx}}px;top:{{.CenterPx}}px;width:{{.CenterSizePx}}px;height:{{.CenterSizePx}}px;">{{.CenterLabel}}</div>
{{$comingSoon := .ComingSoon}}
{{range .Nodes}}
{{if .Placeholder}}
<div class="uc-hub-node uc-hub-node-placeholder" data-search="{{.SearchKey}}" style="left:{{.X}}px;top:{{.Y}}px;width:{{.SizePx}}px;height:{{.SizePx}}px;">
<span class="uc-hub-node-icon">{{.Icon}}</span>{{.Name}}
<span class="uc-hub-node-badge">{{$comingSoon}}</span>
</div>
{{else}}
<a class="uc-hub-node uc-hub-node-{{.ColorIndex}}" data-search="{{.SearchKey}}" href="{{.Href}}" style="left:{{.X}}px;top:{{.Y}}px;width:{{.SizePx}}px;height:{{.SizePx}}px;">
<span class="uc-hub-node-icon">{{.Icon}}</span>{{.Name}}{{if .Code}}<span class="uc-hub-node-code">{{.Code}}</span>{{end}}
</a>
{{end}}
{{end}}
</div>
</div>
{{end}}
`))

// hubColorCount is how many distinct node colors static/app.css
// defines (.uc-hub-node-0 .. .uc-hub-node-9) — cycled by index,
// deterministic per module (not per request), same rationale as
// sorting modules by Key: a stable order so the same tenant sees the
// same layout/colors on every load, not a shuffled one.
const hubColorCount = 10

const hubCenterSizePx = 180

// hubGeometry sizes the hub to however many nodes there actually are —
// a fixed radius/node-size (the original version) works for 2 nodes
// but overlaps badly once placeholders bring the count to a dozen-plus.
// minRadius is derived from the chord-length between adjacent nodes
// (2*radius*sin(pi/n)) needing to exceed nodeSize+gap, so neighboring
// circles never overlap regardless of n.
func hubGeometry(n int) (containerPx, radiusPx, nodeSizePx int) {
	nodeSizePx = 140
	switch {
	case n > 12:
		nodeSizePx = 92
	case n > 8:
		nodeSizePx = 112
	}
	if n <= 1 {
		radiusPx = 180
	} else {
		const gapPx = 14
		minRadius := float64(nodeSizePx+gapPx) / (2 * math.Sin(math.Pi/float64(n)))
		radiusPx = int(math.Max(180, minRadius))
	}
	containerPx = radiusPx*2 + nodeSizePx + 80
	return
}

// hubLayout combines real modules with plannedModuleKeys (skipping any
// planned key a real module already owns — a real module always wins
// its slot, never duplicated as also-a-placeholder) and lays every node
// out evenly spaced around a circle starting at 12 o'clock, via the
// shared layoutHubNodes engine (see hubNode's own doc comment).
func hubLayout(modules []moduleGroup, locale string, catalog *i18n.Catalog, centerLabel, comingSoon string) hubView {
	real := make(map[string]bool, len(modules))
	for _, m := range modules {
		real[m.Key] = true
	}

	entries := make([]hubEntry, 0, len(modules)+len(plannedModuleKeys))
	for _, m := range modules {
		entries = append(entries, hubEntry{Key: m.Key, Name: m.Name, Icon: m.Icon, Href: "/modules/" + m.Key})
	}
	for _, key := range plannedModuleKeys {
		if real[key] {
			continue
		}
		entries = append(entries, hubEntry{
			Key:         key,
			Name:        catalog.T(locale, "module."+key+".name"),
			Icon:        moduleIcon(key),
			Placeholder: true,
		})
	}

	nodes, containerPx := layoutHubNodes(entries)
	return hubView{
		Nodes:        nodes,
		CenterLabel:  centerLabel,
		ComingSoon:   comingSoon,
		ContainerPx:  containerPx,
		CenterPx:     containerPx / 2,
		CenterSizePx: hubCenterSizePx,
	}
}

var dashboardTmpl = template.Must(hubTmpl.New("dashboard").Parse(`
<h1>{{.Title}}</h1>
{{if not .Hub.Nodes}}
<p>{{.Empty}}</p>
{{else}}
{{template "hub" .Hub}}
{{end}}
`))
