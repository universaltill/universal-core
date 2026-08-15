// The in-product help manual's viewer (ADR-0023 in uc-infra, uc-infra#144
// — slice 2 of uc-infra#141, on top of slice 1's uc-infra#143). Every
// generated form/list page already renders a "?" affordance pointing at
// help.Href(topicID) (formrender's viewModel.Help, listview.go's
// buildHelpView); this file is what that link finally lands on: a
// two-pane browsable manual (a topic tree on the left, the selected
// topic's rendered content on the right), server-rendered HTML/HTMX
// exactly like every other page in this package, driven entirely by
// internal/help.Index — no entity-type branching here, the same
// kernel/deterministic-core boundary rule every other generic surface
// in this package already follows (CLAUDE.md).
//
// This slice originally shipped the viewer only, not content:
// internal/help/content/ held nothing but its own README.md, so every
// page here rendered a topic-free "no results"/"select a topic" state.
// Real topic content has since shipped (uc-infra#147-152 — 69 topics per
// locale under internal/help/content/{locale}/{entity,route}/*.md), so
// production now renders a populated two-pane manual; only the "select a
// topic" right-pane state (no topic chosen yet) still shows that message.
//
// This page now documents itself, too (uc-infra#243): renderHelpPage
// wires its own "?" affordance the same way every other page's does,
// pointing at help.RouteTopicID("help") — see content/{locale}/route/
// help.md, which also finally gives the "help" family's own screenshot
// (previously captured/budgeted/staleness-gated but embedded nowhere) a
// real topic to live in.
package api

import (
	"bytes"
	"context"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
)

// renderHelpIndex is GET /help — the manual with no topic selected (the
// right pane's empty state).
func (h *Handler) renderHelpIndex(w http.ResponseWriter, r *http.Request) {
	h.renderHelpPage(w, r, "")
}

// renderHelpTopic is GET /help/{topicID...} — a Go 1.22+ trailing
// wildcard that captures the full topic id including its own internal
// slashes (e.g. "entity/PurchaseOrder"), the same shape help.Href
// already produces. Go's ServeMux prefers a literal registration
// ("/help/search", helpSearchFragment below) over an overlapping
// wildcard one at the same position regardless of registration order
// (net/http's own "more specific pattern wins" rule — see this
// package's Routes' own /export/saft-vs-{entityType} comment for the
// identical precedent) — TestHelpSearchRouteWinsOverTopicWildcard
// confirms this holds for these two routes specifically.
func (h *Handler) renderHelpTopic(w http.ResponseWriter, r *http.Request) {
	h.renderHelpPage(w, r, r.PathValue("topicID"))
}

// renderHelpPage is shared by the index and single-topic routes above.
// selectedTopicID == "" means the index/welcome state.
func (h *Handler) renderHelpPage(w http.ResponseWriter, r *http.Request, selectedTopicID string) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	idx, err := help.GetIndex()
	if err != nil {
		writeInternalError(w, "build help index", err)
		return
	}

	visible, err := h.visibleHelpTopics(r.Context(), ts, idx.Topics(locale))
	if err != nil {
		writeInternalError(w, "authz-filter help topics", err)
		return
	}

	// ADR-0023's "?" help affordance (uc-infra#243) — the manual documents
	// itself the same way every other page documents itself: a route
	// topic, wired the same way import.go's own importTopicID is (route/
	// isn't authz-filtered — see visibleHelpTopics above — so this is
	// never gated, matching every other page's own topic never being
	// gated on read access to *itself*).
	helpTopicID := help.RouteTopicID("help")
	view := helpPageView{
		Title:             h.catalog.T(locale, "help.viewer.title"),
		SearchPlaceholder: h.catalog.T(locale, "help.viewer.search_placeholder"),
		SearchHref:        "/help/search",
		SelectedTopicID:   selectedTopicID,
		TopicList:         buildHelpTopicListData(h.catalog, locale, visible, selectedTopicID),
		Help:              h.buildHelpView(locale, helpTopicID, help.HasContent(locale, helpTopicID)),
	}

	if selectedTopicID == "" {
		view.DetailEmpty = h.catalog.T(locale, "help.viewer.empty_state")
	} else {
		topic, found := findHelpTopic(visible, selectedTopicID)
		if !found {
			// Authz-filtered-out and genuinely-nonexistent look
			// identical here on purpose (ADR-0023: "must be invisible in
			// the list AND return 404, not 403, when requested directly
			// by URL — don't reveal it exists").
			h.writeHelpNotFound(w, r, &rc, locale)
			return
		}
		view.DetailSelected = true
		view.DetailTitle = topic.Title
		view.DetailHTML = topic.HTML
	}

	var buf bytes.Buffer
	if err := helpPageTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, "render help page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render help page shell", err)
	}
}

// helpSearchFragment is GET /help/search — an htmx partial returning
// ONLY the left-pane topic-list fragment, filtered by ?q=, for the
// search box's live-narrowing (hx-get/hx-trigger="keyup changed
// delay:300ms, search" on the input below, the same query-param-driven
// server round trip listview.go's own filter box already uses, adapted
// to htmx's live-narrowing trigger rather than a full form submit since
// this box has no "Go"/"Clear" buttons of its own to submit alongside).
// An empty ?q= shows the full (authz-filtered) topic list, not an empty
// result — help.Index.Search's own "empty query returns nil" contract is
// about Search itself (ADR-0023's acceptance criterion), not about what
// a cleared search box should display to a person using it.
func (h *Handler) helpSearchFragment(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	idx, err := help.GetIndex()
	if err != nil {
		writeInternalError(w, "build help index", err)
		return
	}

	q := r.URL.Query().Get("q")
	var results []help.Rendered
	if strings.TrimSpace(q) == "" {
		results = idx.Topics(locale)
	} else {
		results = idx.Search(locale, q)
	}

	visible, err := h.visibleHelpTopics(r.Context(), ts, results)
	if err != nil {
		writeInternalError(w, "authz-filter help search results", err)
		return
	}

	data := buildHelpTopicListData(h.catalog, locale, visible, r.URL.Query().Get("selected"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := helpTopicListTmpl.ExecuteTemplate(w, "topicList", data); err != nil {
		log.Printf("api: render help search fragment: %v", err)
	}
}

// visibleHelpTopics authz-filters topics the same way accessibleModules
// (dashboard.go) already filters the nav/dashboard: an entity/-prefixed
// topic id is only visible when ts.crud.CanRead the entity type it
// names, since the module/nav is already built by resolving read access
// per entity type through the same GuardedEngine — a topic about an
// entity type a viewer can't read must not be independently discoverable
// via the help index (ADR-0023). Any other id prefix (route/... today)
// is never authz-filtered — ADR-0023 only asks to gate entity-typed
// topics.
func (h *Handler) visibleHelpTopics(ctx context.Context, ts tenantScope, topics []help.Rendered) ([]help.Rendered, error) {
	out := make([]help.Rendered, 0, len(topics))
	for _, t := range topics {
		if entityType, ok := strings.CutPrefix(t.ID, "entity/"); ok {
			readable, err := ts.crud.CanRead(ctx, entityType)
			if err != nil {
				return nil, err
			}
			if !readable {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// findHelpTopic looks a topic id up in an already authz-filtered slice
// — the "post-authz-filter" lookup ADR-0023 requires for a direct-URL
// request, so a denied topic is absent from this search exactly the way
// it's absent from the rendered tree, not just hidden from the tree's
// markup.
func findHelpTopic(topics []help.Rendered, id string) (help.Rendered, bool) {
	for _, t := range topics {
		if t.ID == id {
			return t, true
		}
	}
	return help.Rendered{}, false
}

// writeHelpNotFound renders a localized 404 page inside the normal
// shell — same nav/language/direction as every other page, mirroring
// denied.go's writePageDenied (the only existing "real HTML error page
// inside the shell" precedent in this package; that one is a 403, this
// is help's own 404), rather than the bare {"data":null,"error":...}
// envelope a browser navigating here directly would otherwise see.
func (h *Handler) writeHelpNotFound(w http.ResponseWriter, r *http.Request, rc *httpx.RequestContext, locale string) {
	var buf bytes.Buffer
	err := helpNotFoundTmpl.Execute(&buf, helpNotFoundView{
		Title: h.catalog.T(locale, "help.viewer.not_found_title"),
		Body:  h.catalog.T(locale, "help.viewer.not_found_body"),
	})
	if err != nil {
		// The template is a compile-time constant with two plain string
		// fields (same reasoning writePageDenied's own doc comment
		// gives) — unreachable short of a programming error.
		log.Printf("api: render help not-found page: %v", err)
		httpx.WriteError(w, http.StatusNotFound, "help topic not found")
		return
	}
	// WriteHeader before renderShell, which writes the body immediately
	// — same ordering writePageDenied uses, for the same reason: a 200
	// would otherwise be committed by the first Write.
	w.WriteHeader(http.StatusNotFound)
	if err := h.renderShell(w, locale, h.renderNav(r, rc, locale), template.HTML(buf.String())); err != nil {
		log.Printf("api: render help not-found shell: %v", err)
	}
}

// helpTopicView is one topic's left-pane row.
type helpTopicView struct {
	ID     string
	Title  string
	Href   string
	Active bool
	Admin  bool
}

// helpModuleGroupView is one Module's worth of topics in the left pane
// — topics arrive from help.Index.Topics/Search already sorted by
// (Module, Order, Title), so a single sequential pass groups them
// without needing a map+resort the way accessibleModules (dashboard.go)
// does for modules that DON'T already arrive pre-sorted by key.
type helpModuleGroupView struct {
	ModuleLabel string
	Topics      []helpTopicView
}

// helpTopicListData is the left-pane topic tree — shared between the
// full page (helpPageView.TopicList) and the htmx search fragment
// (helpSearchFragment writes this exact shape via the "topicList" named
// template both helpPageTmpl and helpTopicListTmpl associate with), so
// the two never render the tree two different ways that could drift.
type helpTopicListData struct {
	Groups []helpModuleGroupView
	Empty  string
	// AdminLabel badges a topic whose Audience is "admin" (not "both"/
	// "user") — ADR-0023's "one manual, admin topics marked visually,
	// not access-restricted": audience never hides a topic, only labels
	// it.
	AdminLabel string
}

func buildHelpTopicListData(catalog *i18n.Catalog, locale string, topics []help.Rendered, selectedID string) helpTopicListData {
	data := helpTopicListData{
		Empty:      catalog.T(locale, "help.viewer.no_results"),
		AdminLabel: catalog.T(locale, "help.viewer.admin_badge"),
	}
	for _, t := range topics {
		tv := helpTopicView{
			ID:     t.ID,
			Title:  t.Title,
			Href:   help.Href(t.ID),
			Active: t.ID == selectedID,
			Admin:  t.Audience == "admin",
		}
		moduleLabel := catalog.TOrDefault(locale, "module."+t.Module+".name", t.Module)
		if len(data.Groups) == 0 || data.Groups[len(data.Groups)-1].ModuleLabel != moduleLabel {
			data.Groups = append(data.Groups, helpModuleGroupView{ModuleLabel: moduleLabel})
		}
		g := &data.Groups[len(data.Groups)-1]
		g.Topics = append(g.Topics, tv)
	}
	return data
}

type helpPageView struct {
	Title             string
	SearchPlaceholder string
	SearchHref        string
	// SelectedTopicID threads the currently-open topic id into the
	// search input's hx-vals (independent review, uc-infra#144): the
	// search box's hx-get swaps only #uc-help-topic-results, rebuilt
	// server-side from scratch — without telling the server which topic
	// is open, buildHelpTopicListData has no way to keep marking its row
	// "active", so a single keystroke silently dropped the highlight on
	// the very topic still showing in the right pane.
	SelectedTopicID string
	TopicList       helpTopicListData
	// DetailSelected is the render gate for the right pane — NOT
	// DetailHTML's truthiness (independent review, uc-infra#144): a
	// topic can have valid front matter and a genuinely empty body,
	// where DetailHTML is template.HTML(""), which is falsy in
	// html/template's {{if}} exactly like the true "nothing selected"
	// state — so gating on DetailHTML made selecting that topic look
	// identical to not having clicked anything, its own <h2> title
	// included. DetailTitle/DetailHTML are set together with
	// DetailSelected when a topic is selected; DetailEmpty is set
	// (DetailSelected left false) otherwise.
	DetailSelected bool
	DetailTitle    string
	DetailHTML     template.HTML
	DetailEmpty    string
	// Help is the page's own "?" affordance (uc-infra#243) — the manual
	// documenting itself, same shape every other page's Help field
	// already has (listview.go's helpView, buildHelpView).
	Help helpView
}

type helpNotFoundView struct {
	Title string
	Body  string
}

// helpTopicListTmpl defines the "topicList" block both helpPageTmpl
// (below) and helpSearchFragment execute — the same "parse the shared
// block once, reuse it from a second template.Must(...New(...).Parse)"
// composition dashboard.go's hubTmpl already establishes for its own
// shared "hub" block, for the identical reason: html/template requires
// an associated template to already exist by name in the same
// *template.Template value a Parse call is attached to.
//
// Stable ids/classes an e2e test can select on (ADR-0023/uc-infra#144):
// #uc-help-topics (the left-pane container), #uc-help-search (the
// input), #uc-help-detail (the right pane, in helpPageTmpl below),
// .uc-help-topic-active (the selected topic's row),
// .uc-help-topic-admin (the admin badge).
var helpTopicListTmpl = template.Must(template.New("helpTopicList").Parse(`
{{define "topicList"}}
{{if not .Groups}}
<p class="uc-help-empty">{{.Empty}}</p>
{{else}}
{{range .Groups}}
<div class="uc-help-module-group">
<h3 class="uc-help-module-label">{{.ModuleLabel}}</h3>
<ul class="uc-help-topic-list">
{{range .Topics}}
<li><a class="uc-help-topic{{if .Active}} uc-help-topic-active{{end}}" href="{{.Href}}" data-help-topic="{{.ID}}">{{.Title}}{{if .Admin}} <span class="uc-help-topic-admin">{{$.AdminLabel}}</span>{{end}}</a></li>
{{end}}
</ul>
</div>
{{end}}
{{end}}
{{end}}
`))

// helpPageTmpl is the full two-pane page. The search input's
// hx-target/hx-swap point at #uc-help-topic-results (an inner div, not
// the whole #uc-help-topics aside) so a live-narrowing keystroke swaps
// only the topic list, not the input itself — replacing the input every
// keystroke would drop its focus/cursor position mid-type.
var helpPageTmpl = template.Must(helpTopicListTmpl.New("helpPage").Parse(`
<div class="uc-help">
<h1>{{.Title}} <a class="uc-help-affordance" role="link" data-help-topic="{{.Help.TopicID}}"{{if .Help.Href}} href="{{.Help.Href}}"{{end}}{{if .Help.Disabled}} aria-disabled="true"{{end}} tabindex="0" aria-label="{{.Help.Label}}">?</a></h1>
<div class="uc-help-layout">
<aside id="uc-help-topics" class="uc-help-topics">
<input type="search" id="uc-help-search" class="uc-help-search" name="q" placeholder="{{.SearchPlaceholder}}" aria-label="{{.SearchPlaceholder}}" autocomplete="off"
 hx-get="{{.SearchHref}}" hx-trigger="keyup changed delay:300ms, search" hx-target="#uc-help-topic-results" hx-swap="innerHTML" hx-vals='{"selected": "{{.SelectedTopicID}}"}'>
<div id="uc-help-topic-results">
{{template "topicList" .TopicList}}
</div>
</aside>
<article id="uc-help-detail" class="uc-help-detail">
{{if .DetailSelected}}
<h2>{{.DetailTitle}}</h2>
{{.DetailHTML}}
{{else}}
<p class="uc-help-empty-state">{{.DetailEmpty}}</p>
{{end}}
</article>
</div>
</div>
`))

var helpNotFoundTmpl = template.Must(template.New("helpNotFound").Parse(`
<div class="uc-help-not-found">
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
</div>
`))
