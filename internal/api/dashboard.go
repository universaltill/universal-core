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

	modules, err := h.accessibleModules(r.Context(), rc.TenantID, locale)
	if err != nil {
		writeInternalError(w, "build accessible modules", err)
		return
	}

	var buf bytes.Buffer
	err = dashboardTmpl.Execute(&buf, dashboardView{
		Nodes:        hubLayout(modules),
		Title:        h.catalog.T(locale, "dashboard.title"),
		Empty:        h.catalog.T(locale, "dashboard.empty"),
		ContainerPx:  hubContainerPx,
		CenterPx:     hubCenterPx,
		CenterSizePx: hubCenterSizePx,
	})
	if err != nil {
		writeInternalError(w, "render dashboard", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
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
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
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
func (h *Handler) accessibleModules(ctx context.Context, tenantID, locale string) ([]moduleGroup, error) {
	entityTypes, err := h.entityDefs.ListPublishedEntityTypes(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list published entity types: %w", err)
	}

	byKey := map[string][]moduleEntity{}
	for _, entityType := range entityTypes {
		if _, err := h.formDefs.GetPublished(ctx, tenantID, entityType); err != nil {
			if errors.Is(err, data.ErrNotFound) {
				continue // no published form for this entity type — nothing to link to yet
			}
			return nil, fmt.Errorf("look up form for %s: %w", entityType, err)
		}
		def, err := h.entityDef(ctx, tenantID, entityType)
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
			Entities: entities,
		})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Key < modules[j].Key })
	return modules, nil
}

// dashboardView is the hub-and-spoke home page: Universal Core at the
// center, one connected node per accessible module around it — the
// graphical module switcher Farshid asked for after seeing the
// SAP-style "modules around a hub" diagram, instead of a flat tile
// grid. Node positions are computed server-side (evenly spaced around
// a circle, starting at 12 o'clock) since html/template has no
// trigonometry of its own — see hubLayout.
type dashboardView struct {
	Nodes        []hubNode
	Title        string
	Empty        string
	ContainerPx  int
	CenterPx     int
	CenterSizePx int
}

// hubNode is one positioned, colored module link.
type hubNode struct {
	Key        string
	Name       string
	X, Y       int
	ColorIndex int
}

// hubColorCount is how many distinct node colors static/app.css
// defines (.uc-hub-node-0 .. .uc-hub-node-9) — cycled by index,
// deterministic per module (not per request), same rationale as
// sorting modules by Key: a stable order so the same tenant sees the
// same layout/colors on every load, not a shuffled one.
const hubColorCount = 10

const (
	hubContainerPx  = 600
	hubRadiusPx     = 220
	hubCenterPx     = hubContainerPx / 2
	hubCenterSizePx = 180
	// hubNodeSizePx must match static/app.css's .uc-hub-node
	// width/height — kept in sync by hand (a plain CSS file has no way
	// to read a Go const), noted here and there so a future resize of
	// one doesn't silently drift from the other.
	hubNodeSizePx = 140
)

func hubLayout(modules []moduleGroup) []hubNode {
	nodes := make([]hubNode, len(modules))
	n := len(modules)
	for i, m := range modules {
		angleDeg := -90.0 + float64(i)*(360.0/float64(n))
		rad := angleDeg * math.Pi / 180
		nodes[i] = hubNode{
			Key:        m.Key,
			Name:       m.Name,
			X:          hubCenterPx + int(hubRadiusPx*math.Cos(rad)),
			Y:          hubCenterPx + int(hubRadiusPx*math.Sin(rad)),
			ColorIndex: i % hubColorCount,
		}
	}
	return nodes
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`
<h1>{{.Title}}</h1>
{{if not .Nodes}}
<p>{{.Empty}}</p>
{{else}}
<div class="uc-hub-wrap">
<div class="uc-hub" style="width:{{.ContainerPx}}px;height:{{.ContainerPx}}px;">
<svg class="uc-hub-lines" width="{{.ContainerPx}}" height="{{.ContainerPx}}">
{{$center := .CenterPx}}
{{range .Nodes}}<line x1="{{$center}}" y1="{{$center}}" x2="{{.X}}" y2="{{.Y}}"></line>{{end}}
</svg>
<div class="uc-hub-center" style="left:{{.CenterPx}}px;top:{{.CenterPx}}px;width:{{.CenterSizePx}}px;height:{{.CenterSizePx}}px;">{{.Title}}</div>
{{range .Nodes}}
<a class="uc-hub-node uc-hub-node-{{.ColorIndex}}" href="/modules/{{.Key}}" style="left:{{.X}}px;top:{{.Y}}px;">{{.Name}}</a>
{{end}}
</div>
</div>
{{end}}
`))
