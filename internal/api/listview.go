package api

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"

	"github.com/universaltill/universal-core/internal/kernel/formrender"
)

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
	entityType := r.PathValue("entityType")
	locale := localeFromRequest(w, r)

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	records, err := h.crud.List(r.Context(), def, rc.TenantID)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("list %s records for list page", entityType), err)
		return
	}

	view := recordListView{
		Name:       h.entityDisplayName(locale, entityType),
		Code:       entityType,
		NewHref:    "/forms/" + entityType + "/new",
		ImportHref: "/import/" + entityType,
		NewLabel:   h.catalog.T(locale, "dashboard.new_link"),
		ImportLink: h.catalog.T(locale, "dashboard.import_link"),
		Empty:      h.catalog.T(locale, "list.empty"),
	}
	for _, f := range def.Fields {
		view.Columns = append(view.Columns, f.Name)
	}
	for _, rec := range records {
		row := recordRowView{Href: "/forms/" + entityType + "/" + rec.ID}
		for _, f := range def.Fields {
			row.Cells = append(row.Cells, formrender.FormatFieldValue(rec.Data[f.Name]))
		}
		view.Rows = append(view.Rows, row)
	}

	var buf bytes.Buffer
	if err := recordListTmpl.Execute(&buf, view); err != nil {
		writeInternalError(w, fmt.Sprintf("render %s list", entityType), err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, fmt.Sprintf("render %s list shell", entityType), err)
	}
}

type recordListView struct {
	Name       string
	Code       string
	Columns    []string
	Rows       []recordRowView
	NewHref    string
	ImportHref string
	NewLabel   string
	ImportLink string
	Empty      string
}

type recordRowView struct {
	Href  string
	Cells []string
}

var recordListTmpl = template.Must(template.New("recordList").Parse(`
<div class="uc-list-toolbar">
<h1>{{.Name}} <span class="uc-menu-item-code">{{.Code}}</span></h1>
<div><a href="{{.NewHref}}">{{.NewLabel}}</a> · <a href="{{.ImportHref}}">{{.ImportLink}}</a></div>
</div>
{{if not .Rows}}
<p class="uc-empty">{{.Empty}}</p>
{{else}}
<table class="uc-table">
<thead><tr>{{range .Columns}}<th>{{.}}</th>{{end}}</tr></thead>
<tbody>
{{range .Rows}}
{{$row := .}}
<tr onclick="window.location='{{$row.Href}}'" style="cursor:pointer">
{{range $i, $cell := $row.Cells}}{{if eq $i 0}}<td><a href="{{$row.Href}}">{{$cell}}</a></td>{{else}}<td>{{$cell}}</td>{{end}}{{end}}
</tr>
{{end}}
</tbody>
</table>
{{end}}
`))
