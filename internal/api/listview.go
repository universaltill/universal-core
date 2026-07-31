package api

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
)

// listPageSize is how many records one list-page shows before paginating
// — not user-configurable yet (no list_columns/page-size concept exists
// anywhere in a Definition), just a fixed value that turns "every record,
// unbounded" into something that stays usable once a tenant has more than
// a screenful of data (QUEUE.md, flagged "not built yet" on 2026-07-20).
const listPageSize = 25

// renderRecordList is the module's actual landing page — a table of
// every record the tenant has for entityType, one row per record,
// linking each row to its own form (GET /forms/{entityType}/{id}).
// Until this existed, the only HTML surfaces for an entity type were
// "New" (a blank form) and the import wizard — there was no way to
// actually see or browse existing records short of the JSON-only
// GET /api/records/{entityType} (listRecords). Requested directly by
// Farshid after logging in for the first time and finding the
// dashboard was just New/Import links with nowhere to go look at data.
//
// Columns are every field the Entity Definition declares, in
// declaration order — reading the registry, not a hardcoded column set
// per entity type (CLAUDE.md's kernel/deterministic-core boundary rule:
// no entity-type branching in a generic engine). Composition/
// related-list children don't get a column here (see entity.Definition
// vs Relationship — those are rendered inside a record's own form, not
// a flat list row).
func (h *Handler) renderRecordList(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	entityType := r.PathValue("entityType")
	locale := localeFromRequest(w, r)

	def, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	// The guarded engine already strips hidden fields from the rows
	// below, but a column has to disappear too: a header for a field
	// whose every cell is blank tells a user exactly which fields are
	// being kept from them, and reads as broken data rather than as
	// policy.
	redacted, err := ts.crud.HiddenFields(r.Context(), entityType)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("resolve hidden fields for %s list page", entityType), err)
		return
	}
	columns := visibleFields(def, redacted)

	// Sort/filter come from the URL and are VALIDATED against the visible
	// columns before reaching the query: an unknown or hidden field is
	// dropped, not passed through. The repo binds them as parameters
	// regardless (so structure is safe either way), but validating here
	// means a typo'd or hidden field silently falls back to the default
	// ordering rather than erroring or leaking that the field exists.
	opts := data.ListPageOptions{Limit: listPageSize}
	if sf := r.URL.Query().Get("sort"); sf != "" && isVisibleColumn(columns, sf) {
		opts.SortField = sf
		opts.SortDesc = r.URL.Query().Get("dir") == "desc"
		// A number field sorts numerically, not as text — otherwise a
		// list of purchase orders by total puts 100 before 90.
		if f, ok := def.FieldByName(sf); ok && f.Type == entity.FieldNumber {
			opts.SortNumeric = true
		}
	}
	filterField := r.URL.Query().Get("filter")
	if filterField == "" && len(columns) > 0 {
		// No explicit field: search the first visible column. A
		// single-box filter with no column picker (the current UI) needs a
		// default target, and the first column — conventionally name — is
		// the one a user means by "filter this list".
		filterField = columns[0].Name
	}
	filterValue := r.URL.Query().Get("q")
	if filterValue != "" && isVisibleColumn(columns, filterField) {
		opts.FilterField = filterField
		opts.FilterValue = filterValue
	}

	total, err := ts.crud.CountFiltered(r.Context(), def, opts)
	if err != nil {
		h.writeCrudPageError(w, r, &rc, locale, fmt.Sprintf("count %s records for list page", entityType), err)
		return
	}
	totalPages := (total + listPageSize - 1) / listPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page")) // 0/negative/unparsable all clamp to 1 below
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	opts.Offset = (page - 1) * listPageSize
	records, err := ts.crud.ListPageFiltered(r.Context(), def, opts)
	if err != nil {
		h.writeCrudPageError(w, r, &rc, locale, fmt.Sprintf("list %s records for list page", entityType), err)
		return
	}

	// Reference columns show the target record's own label, not the raw id
	// every list row used to show before this — a page of GUIDs a user
	// can't tell apart is exactly the gap Farshid pointed out after the
	// reference-dropdown fix only fixed the form view, not the list.
	// Resolves labels for only the reference ids actually on THIS page
	// (#24) — not every target record — so the list scales with page size,
	// not the referenced table's total size; indexed field -> id -> label
	// for O(1) lookup per cell. A stale id with no resolvable label (the
	// target was deleted, or is not readable by this viewer) falls back to
	// showing the raw id in the cell renderer — visible-but-broken beats
	// silently hiding that the reference is dangling.
	referenceLabels := h.pageReferenceLabels(r.Context(), ts, def, records)

	view := recordListView{
		Name:        h.entityDisplayName(locale, entityType),
		Code:        entityType,
		NewHref:     "/forms/" + entityType + "/new",
		ImportHref:  "/import/" + entityType,
		ExportHref:  "/export/" + entityType,
		NewLabel:    h.catalog.T(locale, "dashboard.new_link"),
		ImportLink:  h.catalog.T(locale, "dashboard.import_link"),
		ExportLink:  h.catalog.T(locale, "dashboard.export_link"),
		Empty:       h.catalog.T(locale, "list.empty"),
		FilterField: opts.FilterField,
		FilterValue: opts.FilterValue,
		FilterHref:  "/records/" + entityType,
		FilterLabel: h.catalog.T(locale, "list.filter_placeholder"),
		FilterGo:    h.catalog.T(locale, "list.filter_button"),
		FilterClear: h.catalog.T(locale, "list.filter_clear"),
	}
	// keepQuery preserves the active filter across sort and page links, so
	// sorting a filtered list doesn't silently clear the filter.
	keepQuery := func(extra url.Values) string {
		q := url.Values{}
		if opts.FilterField != "" {
			q.Set("filter", opts.FilterField)
			q.Set("q", opts.FilterValue)
		}
		for k, vs := range extra {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		if len(q) == 0 {
			return "/records/" + entityType
		}
		return "/records/" + entityType + "?" + q.Encode()
	}
	if totalPages > 1 {
		pageLabel := h.catalog.T(locale, "list.page_of")
		pageLabel = strings.ReplaceAll(pageLabel, "{page}", strconv.Itoa(page))
		pageLabel = strings.ReplaceAll(pageLabel, "{total}", strconv.Itoa(totalPages))
		view.PageLabel = pageLabel
		sortParams := func(base url.Values) url.Values {
			if opts.SortField != "" {
				base.Set("sort", opts.SortField)
				if opts.SortDesc {
					base.Set("dir", "desc")
				}
			}
			return base
		}
		if page > 1 {
			view.PrevHref = keepQuery(sortParams(url.Values{"page": {strconv.Itoa(page - 1)}}))
			view.PrevLabel = h.catalog.T(locale, "list.prev")
		}
		if page < totalPages {
			view.NextHref = keepQuery(sortParams(url.Values{"page": {strconv.Itoa(page + 1)}}))
			view.NextLabel = h.catalog.T(locale, "list.next")
		}
	}
	for _, f := range columns {
		// A header is a sort link: clicking it sorts by that column, and
		// clicking the already-sorted column flips the direction. The
		// arrow shows the current sort so the ordering is legible, not a
		// mystery the user has to test by reading rows.
		label := h.catalog.TOrDefault(locale, "field."+entityType+"."+f.Name, f.Name)
		nextDesc := opts.SortField == f.Name && !opts.SortDesc
		params := url.Values{"sort": {f.Name}}
		if nextDesc {
			params.Set("dir", "desc")
		}
		arrow := ""
		if opts.SortField == f.Name {
			arrow = " ↑"
			if opts.SortDesc {
				arrow = " ↓"
			}
		}
		view.Columns = append(view.Columns, columnView{
			Label: label,
			Href:  keepQuery(params),
			Arrow: arrow,
		})
	}
	for _, rec := range records {
		row := recordRowView{Href: "/forms/" + entityType + "/" + rec.ID}
		for _, f := range columns {
			row.Cells = append(row.Cells, h.cellText(entityType, f, rec.Data[f.Name], referenceLabels, locale))
		}
		view.Rows = append(view.Rows, row)
	}

	var buf bytes.Buffer
	if err := recordListTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, fmt.Sprintf("render %s list", entityType), err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, fmt.Sprintf("render %s list shell", entityType), err)
	}
}

// visibleFields returns def's fields minus the ones this viewer may not
// see, in declaration order. Used wherever a surface enumerates an
// entity's fields ITSELF rather than reading a record's data (list
// columns, CSV export columns) — those can't inherit the guarded
// engine's row-level redaction, because they're built from the
// Definition, which is metadata every user can already see the shape of.
//
// Returns def.Fields unchanged (no copy) in the overwhelmingly common
// case of nothing redacted.
// isVisibleColumn reports whether name is one of the columns this viewer
// may see — the gate on any sort/filter field before it reaches the
// query. A field the viewer can't see must not be sortable/filterable
// (the guarded engine refuses it too, but rejecting it here keeps the
// page working instead of erroring).
func isVisibleColumn(cols []entity.Field, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

func visibleFields(def *entity.Definition, redacted map[string]bool) []entity.Field {
	if len(redacted) == 0 {
		return def.Fields
	}
	out := make([]entity.Field, 0, len(def.Fields))
	for _, f := range def.Fields {
		if redacted[f.Name] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// cellText formats one list-row cell — a reference field resolves to
// its target's label via referenceLabels (falling back to the raw
// stored id for a dangling/unresolvable reference); an enum field
// resolves through the same "field.{EntityType}.{FieldName}.{Value}"
// i18n convention the form dropdown uses (see buildFields' identical
// lookup), so a status of "active"/"draft" reads in the visitor's own
// language on the list page too, not just inside the form. Every other
// field type uses the same formatting the form renderer already uses.
func (h *Handler) cellText(entityType string, f entity.Field, value any, referenceLabels map[string]map[string]string, locale string) string {
	switch f.Type {
	case entity.FieldReference:
		if id, ok := value.(string); ok && id != "" {
			if label, ok := referenceLabels[f.Name][id]; ok {
				return label
			}
		}
	case entity.FieldEnum:
		if v, ok := value.(string); ok && v != "" {
			return h.catalog.TOrDefault(locale, "field."+entityType+"."+f.Name+"."+v, v)
		}
	}
	return formrender.FormatFieldValue(value)
}

type columnView struct {
	Label string
	Href  string
	Arrow string
}

type recordListView struct {
	Name        string
	Code        string
	Columns     []columnView
	Rows        []recordRowView
	NewHref     string
	ImportHref  string
	ExportHref  string
	NewLabel    string
	ImportLink  string
	ExportLink  string
	Empty       string
	FilterField string
	FilterValue string
	FilterHref  string
	FilterLabel string
	FilterGo    string
	FilterClear string
	// PageLabel is "" when there's only one page (no pager to show) —
	// PrevHref/NextHref are independently "" at whichever boundary has
	// no such page (first/last), so the template can render each link
	// only when there's somewhere for it to actually go.
	PageLabel string
	PrevHref  string
	PrevLabel string
	NextHref  string
	NextLabel string
}

type recordRowView struct {
	Href  string
	Cells []string
}

var recordListTmpl = template.Must(template.New("recordList").Parse(`
<div class="uc-list-toolbar">
<h1>{{.Name}} <span class="uc-menu-item-code">{{.Code}}</span></h1>
<div><a href="{{.NewHref}}">{{.NewLabel}}</a> · <a href="{{.ImportHref}}">{{.ImportLink}}</a> · <a href="{{.ExportHref}}">{{.ExportLink}}</a></div>
</div>
<form class="uc-list-filter" method="get" action="{{.FilterHref}}">
{{if .FilterField}}<input type="hidden" name="filter" value="{{.FilterField}}">{{end}}
<input type="search" name="q" value="{{.FilterValue}}" placeholder="{{.FilterLabel}}" aria-label="{{.FilterLabel}}">
<button type="submit">{{.FilterGo}}</button>
{{if .FilterValue}}<a href="{{.FilterHref}}">{{.FilterClear}}</a>{{end}}
</form>
{{if not .Rows}}
<p class="uc-empty">{{.Empty}}</p>
{{else}}
<table class="uc-table">
<thead><tr>{{range .Columns}}<th><a class="uc-sort" href="{{.Href}}">{{.Label}}{{.Arrow}}</a></th>{{end}}</tr></thead>
<tbody>
{{range .Rows}}
{{$row := .}}
<tr onclick="window.location='{{$row.Href}}'" style="cursor:pointer">
{{range $i, $cell := $row.Cells}}{{if eq $i 0}}<td><a href="{{$row.Href}}">{{$cell}}</a></td>{{else}}<td>{{$cell}}</td>{{end}}{{end}}
</tr>
{{end}}
</tbody>
</table>
{{if .PageLabel}}
<div class="uc-list-pager">
{{if .PrevHref}}<a href="{{.PrevHref}}">{{.PrevLabel}}</a>{{end}}
<span>{{.PageLabel}}</span>
{{if .NextHref}}<a href="{{.NextHref}}">{{.NextLabel}}</a>{{end}}
</div>
{{end}}
{{end}}
`))
