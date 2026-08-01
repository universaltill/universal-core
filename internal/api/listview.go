package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	uclocale "github.com/universaltill/universal-core/internal/kernel/locale"
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
	loc := regionalLocale(r, locale)

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
	if sf := r.URL.Query().Get("sort"); sf != "" && isVisibleColumn(columns, sf) && sortFilterableField(def, sf) {
		opts.SortField = sf
		opts.SortDesc = r.URL.Query().Get("dir") == "desc"
		// A number field sorts numerically, not as text — otherwise a
		// list of purchase orders by total puts 100 before 90.
		if f, ok := def.FieldByName(sf); ok && f.Type == entity.FieldNumber {
			opts.SortNumeric = true
		}
	}
	filterField := r.URL.Query().Get("filter")
	if filterField == "" {
		// No explicit field: the single filter box (no column picker in the
		// current UI) targets the first visible column that can actually be
		// filtered — conventionally name. An i18n_text column is skipped
		// because it stores a JSON object, not a searchable string
		// (ADR-0009): `data->>'name'` on an object matches its raw JSON
		// text, which is meaningless — the same reason the reference-search
		// endpoint degrades. Localized filtering needs a per-locale JSONB
		// expression index and is deferred (ties into #64).
		for _, c := range columns {
			if sortFilterableField(def, c.Name) {
				filterField = c.Name
				break
			}
		}
	}
	filterValue := r.URL.Query().Get("q")
	if filterValue != "" && isVisibleColumn(columns, filterField) && sortFilterableField(def, filterField) {
		opts.FilterField = filterField
		opts.FilterValue = filterValue
		// A reference column stores the target's id, so matching what a
		// human typed against it finds nothing: searching an attendance
		// list for "EMP-0001" returned zero rows even though the cells
		// plainly showed EMP-0001, because the cell is a resolved label
		// and the stored value is a UUID (independent review, board
		// #17). Resolve the text against the target's own label field
		// and filter by the ids it matches. Generic — it reads the
		// Definition, never a specific entity type — so every
		// reference-first list gets it, not just this one.
		if f, ok := def.FieldByName(filterField); ok && f.Type == entity.FieldReference {
			ids, err := h.referenceIDsMatching(r.Context(), ts, f.Target, filterValue)
			if err != nil {
				h.writeCrudPageError(w, r, &rc, locale, fmt.Sprintf("resolve %s filter", filterField), err)
				return
			}
			// Non-nil even when empty: "no target matched" must return no
			// rows, not silently drop the filter.
			opts.FilterIn = ids
		}
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
	referenceLabels := h.pageReferenceLabels(r.Context(), ts, def, records, locale)

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
		// An i18n_text column can't be sorted (its value is a JSON object,
		// not orderable text — ADR-0009), so it renders as a plain header
		// with no sort link rather than a link that would order by raw JSON
		// text. Empty Href signals the template to drop the <a>.
		if !sortFilterableField(def, f.Name) {
			view.Columns = append(view.Columns, columnView{Label: label})
			continue
		}
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
			row.Cells = append(row.Cells, h.cellText(entityType, f, rec.Data[f.Name], referenceLabels, locale, loc))
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

// sortFilterableField reports whether a field can back a SQL sort/filter.
// An i18n_text field (ADR-0009) can't: its value is a JSON object, so
// `data->>'name'` would order/match by the object's raw JSON text, which is
// meaningless — the same reason the reference-search endpoint degrades for
// such a label. Localized sort/filter needs a per-locale JSONB expression
// index and is deferred (#64). A field the Definition doesn't declare is
// not sort/filterable either (defensive; callers already gate on
// isVisibleColumn).
func sortFilterableField(def *entity.Definition, name string) bool {
	f, ok := def.FieldByName(name)
	return ok && f.Type != entity.FieldI18nText
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

// referenceIDsMatching resolves free text typed into the filter box
// into the ids of target records whose label matches it — the bridge
// between what a list cell shows and what the column actually stores.
// Uses the same label-field convention the picker and the cell renderer
// use (referenceLabelFieldFor, which honours an explicit
// Definition.LabelField), so the three always agree about what a record
// is called.
//
// A target with no label field at all resolves to no matches rather
// than to everything: silently widening a filter to the whole table is
// worse than an empty result a user can see and correct.
func (h *Handler) referenceIDsMatching(ctx context.Context, ts tenantScope, targetType, text string) ([]string, error) {
	targetDef, err := ts.entityDef(ctx, targetType)
	if errors.Is(err, data.ErrNotFound) {
		// The target's module is not licensed in this tenant — a real,
		// supported configuration (crm references Item and SalesOrder,
		// and can be licensed without purchasing or sales). Filtering by
		// it matches nothing, which is the truthful answer; 500ing the
		// whole list page is not. Every neighbouring resolution path
		// (loadCurrentReferenceLabels, pageReferenceLabels) already
		// degrades rather than failing — this one was the odd one out
		// (independent review).
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up %s definition: %w", targetType, err)
	}
	labelField := referenceLabelFieldFor(targetDef)
	if labelField == "" {
		return []string{}, nil
	}
	matches, err := ts.crud.ListPageFiltered(ctx, targetDef, data.ListPageOptions{
		FilterField: labelField,
		FilterValue: text,
		// Bounded like the reference picker's own search: a filter that
		// matched thousands of targets would build an enormous IN list
		// for no practical gain.
		Limit: referenceFilterMatchLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("search %s by %s: %w", targetType, labelField, err)
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// referenceFilterMatchLimit caps how many target records one filter
// term may resolve to.
const referenceFilterMatchLimit = 500

// cellText formats one list-row cell — a reference field resolves to
// its target's label via referenceLabels (falling back to the raw
// stored id for a dangling/unresolvable reference); an enum field
// resolves through the same "field.{EntityType}.{FieldName}.{Value}"
// i18n convention the form dropdown uses (see buildFields' identical
// lookup), so a status of "active"/"draft" reads in the visitor's own
// language on the list page too, not just inside the form. Every other
// field type uses the same formatting the form renderer already uses.
func (h *Handler) cellText(entityType string, f entity.Field, value any, referenceLabels map[string]map[string]string, locale string, loc uclocale.Locale) string {
	switch f.Type {
	case entity.FieldDate:
		// Regional formatting (board #22): the stored value is always
		// ISO 8601, but 03/04/2026 and 04/03/2026 are the same day to
		// different readers, and a Farsi reader expects a Jalali date
		// entirely. Display only — the form's date INPUT keeps rendering
		// the raw ISO value, because that round-trips back through
		// csvimport.Coerce on submit.
		if s, ok := value.(string); ok && s != "" {
			return loc.FormatDate(s)
		}
	case entity.FieldNumber:
		// Same reasoning for grouping/decimal separators and digit
		// shapes. Precision is left as the value's own (-1) — this
		// column has no currency context to fix a scale from; a money
		// column formatted to its Currency's minor_unit is follow-up
		// work, filed on the board.
		if v, ok := value.(float64); ok {
			return loc.FormatNumber(v, -1)
		}
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
	case entity.FieldI18nText:
		// A multilingual field (ADR-0009) is a JSON object, not a string —
		// resolve it to the viewer's locale so its OWN list column shows a
		// readable value, not a raw map dump. An empty/absent value falls
		// through to FormatFieldValue's blank.
		if s, ok := h.catalog.ResolveLocalized(value, locale); ok {
			return s
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
<thead><tr>{{range .Columns}}<th>{{if .Href}}<a class="uc-sort" href="{{.Href}}">{{.Label}}{{.Arrow}}</a>{{else}}{{.Label}}{{end}}</th>{{end}}</tr></thead>
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
