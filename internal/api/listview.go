package api

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"

	"github.com/universaltill/universal-core/internal/kernel/entity"
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

	// Reference columns show the target record's own label (the same
	// resolution the form's dropdown already uses, see
	// loadReferenceOptions's own doc comment), not the raw id every
	// list row used to show before this — a page of GUIDs a user can't
	// tell apart is exactly the gap Farshid pointed out after the
	// reference-dropdown fix only fixed the form view, not the list.
	// referenceLabels indexes each reference field's options by id for
	// O(1) lookup per cell; a stale id with no matching option (the
	// target record was deleted after this one referenced it) falls
	// back to showing the raw id — visible-but-broken beats silently
	// hiding that the reference is dangling.
	refOptions := h.loadReferenceOptions(r.Context(), rc.TenantID, def)
	referenceLabels := make(map[string]map[string]string, len(refOptions))
	for field, opts := range refOptions {
		byID := make(map[string]string, len(opts))
		for _, opt := range opts {
			byID[opt.ID] = opt.Label
		}
		referenceLabels[field] = byID
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
		// Same "field.{EntityType}.{FieldName}" convention formrender
		// uses for form labels (falls back to the raw field name when no
		// translation exists yet) — previously every list page showed
		// raw snake_case column headers regardless of locale (QUEUE.md,
		// flagged "not built yet" on 2026-07-20).
		view.Columns = append(view.Columns, h.catalog.TOrDefault(locale, "field."+entityType+"."+f.Name, f.Name))
	}
	for _, rec := range records {
		row := recordRowView{Href: "/forms/" + entityType + "/" + rec.ID}
		for _, f := range def.Fields {
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
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, fmt.Sprintf("render %s list shell", entityType), err)
	}
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
