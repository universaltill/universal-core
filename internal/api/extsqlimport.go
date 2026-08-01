// The "import from a SQL source" flow (ADR-0019) — the import wizard's
// second source format, alongside the CSV/XLSX upload (import.go), for a
// target entity type: pick a registered ExternalSQLSource, browse its
// relations (tables/views), map the chosen relation's columns onto the
// entity's fields, preview, commit. Everything downstream of the fetch
// is csvimport's own machinery (PreviewRows/CommitRows — an external
// relation and an uploaded file are indistinguishable to it), plus
// sqlsource.ApplyConstants for template-supplied fixed values.
//
// The mapping pre-fill prefers a vendor template
// (sqlsource.MatchTemplate — NAV 2009's "$Item"/"$Customer" shapes) when
// one matches the relation AND targets this entity type, falling back to
// csvimport.SuggestMapping over the real column names. The AI-assisted
// suggestion the CSV path layers on top (SuggestMappingAI) is
// deliberately NOT wired here: it needs sample rows before the mapping
// UI exists, which for an external source would mean a second full
// fetch round-trip against a legacy database per preview — the
// deterministic suggestion is the right cost/benefit for this first
// slice, and the mapping stays fully hand-editable either way.
//
// Error hygiene: a driver error's text routinely embeds the DSN
// (host, database, credentials) — every connection/fetch failure renders
// only a generic translated message and logs the detail server-side,
// the same rule extsqlsource.go's "Test connection" follows.
package api

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/sqlsource"
)

// Fetch budgets. Browsing (relations/columns + a PreviewRowCount fetch)
// must stay interactive; commit moves up to MaxImportRows across the
// wire and gets the long budget the task's one-shot pull needs.
const (
	extSQLBrowseTimeout = 10 * time.Second
	extSQLFetchTimeout  = 15 * time.Second
	extSQLCommitTimeout = 30 * time.Second
)

// extSQLImportPage is step 1: pick one of the tenant's registered
// sources. Gated on write permission exactly like importUploadPage —
// import is bulk record creation whichever source feeds it.
func (h *Handler) extSQLImportPage(w http.ResponseWriter, r *http.Request) {
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

	if _, err := ts.entityDef(r.Context(), entityType); err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	allowed, err := ts.crud.CanWrite(r.Context(), entityType)
	if !h.denyPageUnless(w, r, &rc, locale, allowed, err, "check write permission for "+entityType+" SQL import page") {
		return
	}

	sources, ok := h.listExtSQLSources(w, r, ts)
	if !ok {
		return
	}

	view := extSQLImportPageView{
		EntityType:    entityType,
		RelationsHref: "/import/" + entityType + "/sql/relations",
		Heading:       h.catalog.T(locale, "extsql_import.heading"),
		Intro:         h.catalog.T(locale, "extsql_import.intro"),
		SourceLabel:   h.catalog.T(locale, "extsql_import.source_label"),
		BrowseLabel:   h.catalog.T(locale, "extsql_import.browse_button"),
		NoSources:     h.catalog.T(locale, "extsql_import.no_sources"),
		ConfigureLink: h.catalog.T(locale, "extsql_import.configure_link"),
	}
	for _, rec := range sources {
		name, _ := rec.Data["name"].(string)
		view.Sources = append(view.Sources, extSQLImportSourceOption{ID: rec.ID, Name: name})
	}

	var buf strings.Builder
	if err := extSQLImportTmpl.ExecuteTemplate(&buf, "page", view); err != nil {
		writeInternalError(w, "render SQL import page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render SQL import page shell", err)
	}
}

// extSQLImportRelations is step 2's first half: list the chosen source's
// tables and views, each one a button that fetches the mapping+preview
// for it.
func (h *Handler) extSQLImportRelations(w http.ResponseWriter, r *http.Request) {
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

	// Same write gate the page handler applies (importUploadPage's
	// reasoning): browsing a legacy database's relations is part of a
	// bulk-write flow, and without this a read-only user could use the
	// fragment endpoints to read arbitrary external tables the page
	// itself would have refused to show them. 403 as the fixed JSON
	// envelope, not the localized page — these are htmx fragments, the
	// same surface writeCrudError serves (review finding, this commit).
	if _, err := ts.entityDef(r.Context(), entityType); err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	if !h.denyFragmentUnlessWritable(w, r, ts, entityType) {
		return
	}

	src, ok := h.extSQLSourceFromRequest(w, r, ts, locale)
	if !ok {
		return
	}
	sourceID := r.PostForm.Get("source_id")

	ctx, cancel := context.WithTimeout(r.Context(), extSQLBrowseTimeout)
	defer cancel()
	ext, ok := h.openExtSQL(w, src, locale)
	if !ok {
		return
	}
	defer ext.Close()
	relations, err := ext.Relations(ctx)
	if err != nil {
		log.Printf("api: SQL import: list relations of source %s: %v", sourceID, err)
		h.writeExtSQLImportError(w, locale)
		return
	}

	view := extSQLImportRelationsView{
		Heading:     h.catalog.T(locale, "extsql_import.relations_heading"),
		NoRelations: h.catalog.T(locale, "extsql_import.no_relations"),
		SelectLabel: h.catalog.T(locale, "extsql_import.select_button"),
		PreviewHref: "/import/" + entityType + "/sql/preview",
		SourceID:    sourceID,
	}
	kindLabels := map[string]string{
		"table": h.catalog.T(locale, "extsql_import.kind_table"),
		"view":  h.catalog.T(locale, "extsql_import.kind_view"),
	}
	for _, rel := range relations {
		view.Relations = append(view.Relations, extSQLImportRelationView{
			Schema: rel.Schema,
			Name:   rel.Name,
			Kind:   kindLabels[rel.Kind],
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := extSQLImportTmpl.ExecuteTemplate(w, "relations", view); err != nil {
		writeInternalError(w, "render SQL import relations", err)
	}
}

// extSQLImportPreview is steps 2 (mapping) and 3 (preview) in one
// response, mirroring importPreview's shape: the mapping table is always
// rendered (template pre-fill, suggestion, or the user's own
// resubmission), and the row preview + Commit button appear only once
// the mapping validates — an incomplete mapping shows the validation
// error inline with a re-preview button instead.
func (h *Handler) extSQLImportPreview(w http.ResponseWriter, r *http.Request) {
	h.extSQLImportPreviewOrCommit(w, r, false)
}

// extSQLImportCommit is step 4: fetch up to sqlsource.MaxImportRows and
// write every row that passes validation through the same RBAC-guarded
// engine and audit actor the CSV commit uses.
func (h *Handler) extSQLImportCommit(w http.ResponseWriter, r *http.Request) {
	h.extSQLImportPreviewOrCommit(w, r, true)
}

func (h *Handler) extSQLImportPreviewOrCommit(w http.ResponseWriter, r *http.Request, commit bool) {
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
	// Same write gate as the page and the relations fragment — see
	// extSQLImportRelations. The guarded engine would refuse the commit
	// row by row anyway, but preview must not let a denied user read the
	// external table's contents at all.
	if !h.denyFragmentUnlessWritable(w, r, ts, entityType) {
		return
	}
	// Same field-metadata redaction importPreview applies: the mapping UI
	// enumerates the Definition's fields as <select> options, so a field
	// this user may not see must not be offered as a target (nor
	// validated against, nor previewed).
	redacted, err := ts.crud.HiddenFields(r.Context(), entityType)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("resolve hidden fields for %s SQL import", entityType), err)
		return
	}
	if len(redacted) > 0 {
		visible := *def
		visible.Fields = visibleFields(def, redacted)
		def = &visible
	}

	src, ok := h.extSQLSourceFromRequest(w, r, ts, locale)
	if !ok {
		return
	}
	sourceID := r.PostForm.Get("source_id")
	schema := r.PostForm.Get("schema")
	relation := r.PostForm.Get("relation")
	if schema == "" || relation == "" {
		httpx.WriteError(w, http.StatusBadRequest, "schema and relation are required")
		return
	}

	limit, timeout := sqlsource.PreviewRowCount, extSQLFetchTimeout
	if commit {
		limit, timeout = sqlsource.MaxImportRows, extSQLCommitTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	ext, ok := h.openExtSQL(w, src, locale)
	if !ok {
		return
	}
	defer ext.Close()

	// The requested (schema, relation) must be one Relations() would have
	// offered — schema/relation arrive raw from the form, and without
	// this check the discovery listing's system-schema exclusion is
	// cosmetic: a hand-built request could fetch pg_catalog.pg_shadow or
	// any other system relation directly (review finding, this commit).
	// One extra round trip per action is an acceptable price.
	relations, err := ext.Relations(ctx)
	if err != nil {
		log.Printf("api: SQL import: list relations of source %s: %v", sourceID, err)
		h.writeExtSQLImportError(w, locale)
		return
	}
	if !slices.ContainsFunc(relations, func(rel data.ExtRelation) bool {
		return rel.Schema == schema && rel.Name == relation
	}) {
		log.Printf("api: SQL import: refused %s.%s — not among source %s's discovered relations", schema, relation, sourceID)
		h.writeExtSQLImportError(w, locale)
		return
	}

	headers, rows, err := ext.FetchRows(ctx, schema, relation, limit)
	if err != nil {
		log.Printf("api: SQL import: fetch %s.%s from source %s: %v", schema, relation, sourceID, err)
		h.writeExtSQLImportError(w, locale)
		return
	}

	// Mapping: the user's own resubmission wins outright; otherwise a
	// matching vendor template for this entity type (its mapping filtered
	// to the columns this relation actually has — a customized legacy
	// schema may lack some), otherwise the plain name-match suggestion.
	mapping := extSQLMappingFromForm(r)
	tmpl, tmplEntity, tmplMatched := sqlsource.MatchTemplate(relation)
	templateApplies := tmplMatched && tmplEntity.EntityType == def.EntityType
	if len(mapping) == 0 {
		if templateApplies {
			headerSet := make(map[string]bool, len(headers))
			for _, hd := range headers {
				headerSet[hd] = true
			}
			mapping = csvimport.ColumnMapping{}
			for col, field := range tmplEntity.Mapping {
				if headerSet[col] {
					mapping[col] = field
				}
			}
		} else {
			mapping = csvimport.SuggestMapping(headers, def)
		}
	}
	var constants map[string]string
	if templateApplies {
		// A user's explicit mapping targeting a field WINS over the
		// template's constant for that same field: forcing the constant
		// alongside it would make ValidateMapping reject the pair with a
		// raw duplicate-source error naming the internal __const: sentinel
		// (review finding, this commit). The template-prefilled mapping
		// never overlaps its own constants, so this only bites — and
		// correctly resolves — a hand-edited mapping.
		targeted := make(map[string]bool, len(mapping))
		for _, field := range mapping {
			targeted[field] = true
		}
		constants = make(map[string]string, len(tmplEntity.Constants))
		for field, value := range tmplEntity.Constants {
			if !targeted[field] {
				constants[field] = value
			}
		}
	}

	augHeaders, augRows, augMapping, err := sqlsource.ApplyConstants(headers, rows, mapping, constants)
	if err != nil {
		// A source column colliding with the reserved constant prefix is
		// the caller's data, safe to describe (no DSN in this error).
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	view := h.extSQLImportPreviewView(locale, def, entityType, sourceID, schema, relation, headers, mapping, constants)
	if templateApplies {
		view.TemplateNote = strings.ReplaceAll(h.catalog.T(locale, "extsql_import.template_note"), "{vendor}", tmpl.Vendor)
	}

	if mappingErr := csvimport.ValidateMapping(def, augHeaders, augMapping); mappingErr != nil {
		view.MappingError = mappingErr.Error()
		h.writeExtSQLImportFragment(w, "preview", view)
		return
	}

	rowOK := h.catalog.T(locale, "import.row_status_ok")
	rowError := h.catalog.T(locale, "import.row_status_error")

	if !commit {
		results, err := csvimport.PreviewRows(augHeaders, augRows, def, augMapping)
		if err != nil {
			// ValidateMapping passed above, so this is unreachable in
			// practice — fail generically rather than leak anything odd.
			writeInternalError(w, fmt.Sprintf("preview %s SQL import rows", entityType), err)
			return
		}
		view.Rows = buildResultRows(results, rowOK, rowError)
		h.writeExtSQLImportFragment(w, "preview", view)
		return
	}

	results, err := csvimport.CommitRows(ctx, augHeaders, augRows, def, augMapping, ts.crud, rc.Actor)
	if err != nil {
		writeCrudError(w, fmt.Sprintf("commit %s SQL import rows", entityType), err)
		return
	}
	succeeded := 0
	for _, res := range results {
		if res.Err == nil {
			succeeded++
		}
	}
	h.writeExtSQLImportFragment(w, "result", extSQLImportResultView{
		Heading:        h.catalog.T(locale, "import.result_heading"),
		Succeeded:      succeeded,
		Failed:         len(results) - succeeded,
		Total:          len(results),
		SucceededLabel: h.catalog.T(locale, "import.result_succeeded"),
		FailedLabel:    h.catalog.T(locale, "import.result_failed"),
		RowCapNote:     extSQLRowCapNote(h, locale),
		Rows:           buildResultRows(results, rowOK, rowError),
	})
}

// denyFragmentUnlessWritable is denyPageUnless for this flow's htmx
// fragments: same CanWrite gate, but a 403 JSON envelope (the fixed
// "access denied" string writeCrudError already uses for non-page
// surfaces) instead of the localized denial page. Reports false when the
// response was already written.
func (h *Handler) denyFragmentUnlessWritable(w http.ResponseWriter, r *http.Request, ts tenantScope, entityType string) bool {
	allowed, err := ts.crud.CanWrite(r.Context(), entityType)
	if err != nil {
		writeInternalError(w, "check write permission for "+entityType+" SQL import", err)
		return false
	}
	if !allowed {
		httpx.WriteError(w, http.StatusForbidden, "access denied")
		return false
	}
	return true
}

// listExtSQLSources lists the tenant's registered sources. An
// ExternalSQLSource Definition that isn't published yet (a tenant
// provisioned before the foundation set included it) degrades to "no
// sources registered" rather than 404ing the whole import page.
func (h *Handler) listExtSQLSources(w http.ResponseWriter, r *http.Request, ts tenantScope) ([]data.Record, bool) {
	def, err := ts.entityDef(r.Context(), "ExternalSQLSource")
	if errors.Is(err, data.ErrNotFound) {
		return nil, true
	}
	if err != nil {
		writeInternalError(w, "look up definition for ExternalSQLSource", err)
		return nil, false
	}
	records, err := ts.crud.List(r.Context(), def)
	if err != nil {
		writeCrudError(w, "list ExternalSQLSource records", err)
		return nil, false
	}
	return records, true
}

// extSQLSourceFromRequest parses the submitted source_id, loads the
// record through the guarded engine, and shapes it into a ready-to-dial
// Source (password decrypted). ok=false means the response was written.
func (h *Handler) extSQLSourceFromRequest(w http.ResponseWriter, r *http.Request, ts tenantScope, locale string) (sqlsource.Source, bool) {
	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid form submission: "+err.Error())
		return sqlsource.Source{}, false
	}
	sourceID := r.PostForm.Get("source_id")
	if !isValidID(sourceID) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid source id")
		return sqlsource.Source{}, false
	}
	def, err := ts.entityDef(r.Context(), "ExternalSQLSource")
	if err != nil {
		writeDefinitionLookupError(w, "ExternalSQLSource", err)
		return sqlsource.Source{}, false
	}
	rec, err := ts.crud.Get(r.Context(), def, sourceID)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("ExternalSQLSource %q not found", sourceID))
		return sqlsource.Source{}, false
	}
	if err != nil {
		writeCrudError(w, "get ExternalSQLSource "+sourceID, err)
		return sqlsource.Source{}, false
	}
	src, err := h.extSQLSourceFromFields(rec.Data)
	if err != nil {
		// A stored password this deployment can't decrypt — the details
		// stay in the log, the user gets the generic failure.
		log.Printf("api: SQL import: build source %s: %v", sourceID, err)
		h.writeExtSQLImportError(w, locale)
		return sqlsource.Source{}, false
	}
	return src, true
}

// openExtSQL composes the DSN and opens the handle. Neither the DSN nor
// any driver error text ever reaches the response — see this file's doc
// comment.
func (h *Handler) openExtSQL(w http.ResponseWriter, src sqlsource.Source, locale string) (*data.ExtSQL, bool) {
	dsn, err := src.DSN()
	if err != nil {
		log.Printf("api: SQL import: compose DSN for source %q: %v", src.Name, err)
		h.writeExtSQLImportError(w, locale)
		return nil, false
	}
	ext, err := data.OpenExtSQL(src.Driver, dsn)
	if err != nil {
		log.Printf("api: SQL import: open source %q: %v", src.Name, err)
		h.writeExtSQLImportError(w, locale)
		return nil, false
	}
	return ext, true
}

// writeExtSQLImportError renders the generic translated failure fragment
// — an htmx swap target, so an HTML fragment, not a JSON envelope.
func (h *Handler) writeExtSQLImportError(w http.ResponseWriter, locale string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := extSQLImportTmpl.ExecuteTemplate(w, "error", h.catalog.T(locale, "extsql_import.connection_failed")); err != nil {
		writeInternalError(w, "render SQL import error", err)
	}
}

func (h *Handler) writeExtSQLImportFragment(w http.ResponseWriter, name string, view any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := extSQLImportTmpl.ExecuteTemplate(w, name, view); err != nil {
		writeInternalError(w, "render SQL import "+name, err)
	}
}

// extSQLMappingFromForm reconstructs the mapping from the preview
// fragment's "mapping.<column>" selects — mappingFromForm's urlencoded
// counterpart (these fragments carry no file, so htmx posts them
// application/x-www-form-urlencoded, and r.MultipartForm stays nil).
func extSQLMappingFromForm(r *http.Request) csvimport.ColumnMapping {
	mapping := csvimport.ColumnMapping{}
	const prefix = "mapping."
	for key, values := range r.PostForm {
		if !strings.HasPrefix(key, prefix) || len(values) == 0 || values[0] == "" {
			continue
		}
		mapping[strings.TrimPrefix(key, prefix)] = values[0]
	}
	return mapping
}

func extSQLRowCapNote(h *Handler, locale string) string {
	return strings.ReplaceAll(h.catalog.T(locale, "extsql_import.row_cap_note"), "{max}", strconv.Itoa(sqlsource.MaxImportRows))
}

func (h *Handler) extSQLImportPreviewView(locale string, def *entity.Definition, entityType, sourceID, schema, relation string, headers []string, mapping csvimport.ColumnMapping, constants map[string]string) extSQLImportPreviewView {
	view := extSQLImportPreviewView{
		EntityType:       entityType,
		PreviewHref:      "/import/" + entityType + "/sql/preview",
		CommitHref:       "/import/" + entityType + "/sql/commit",
		SourceID:         sourceID,
		Schema:           schema,
		Relation:         relation,
		MappingHeading:   h.catalog.T(locale, "import.mapping_heading"),
		RowsHeading:      h.catalog.T(locale, "import.rows_heading"),
		CommitLabel:      h.catalog.T(locale, "import.commit_button"),
		RepreviewLabel:   h.catalog.T(locale, "import.repreview_button"),
		ConstantsHeading: h.catalog.T(locale, "extsql_import.constants_heading"),
		FixedValueLabel:  h.catalog.T(locale, "extsql_import.fixed_value"),
		RowCapNote:       extSQLRowCapNote(h, locale),
		Mappings:         buildMappingRows(headers, mapping, nil, def, h.catalog.T(locale, "import.unmapped_option")),
	}
	for _, f := range sortedConstantFields(constants) {
		view.Constants = append(view.Constants, extSQLImportConstantView{Field: f, Value: constants[f]})
	}
	return view
}

// sortedConstantFields returns constants' field names in a stable order
// — the same sorted order sqlsource.ApplyConstants itself appends the
// synthetic columns in.
func sortedConstantFields(constants map[string]string) []string {
	return slices.Sorted(maps.Keys(constants))
}

// --- view models ---

type extSQLImportPageView struct {
	EntityType    string
	RelationsHref string
	Heading       string
	Intro         string
	SourceLabel   string
	BrowseLabel   string
	NoSources     string
	ConfigureLink string
	Sources       []extSQLImportSourceOption
}

type extSQLImportSourceOption struct {
	ID   string
	Name string
}

type extSQLImportRelationsView struct {
	Heading     string
	NoRelations string
	SelectLabel string
	PreviewHref string
	SourceID    string
	Relations   []extSQLImportRelationView
}

type extSQLImportRelationView struct {
	Schema string
	Name   string
	Kind   string
}

type extSQLImportPreviewView struct {
	EntityType  string
	PreviewHref string
	CommitHref  string
	SourceID    string
	Schema      string
	Relation    string

	MappingHeading   string
	RowsHeading      string
	CommitLabel      string
	RepreviewLabel   string
	ConstantsHeading string
	FixedValueLabel  string
	RowCapNote       string
	TemplateNote     string

	Mappings  []mappingRowView
	Constants []extSQLImportConstantView

	MappingError string
	Rows         []previewRowView
}

type extSQLImportConstantView struct {
	Field string
	Value string
}

type extSQLImportResultView struct {
	Heading        string
	Succeeded      int
	Failed         int
	Total          int
	SucceededLabel string
	FailedLabel    string
	RowCapNote     string
	Rows           []previewRowView
}

var extSQLImportTmpl = template.Must(template.New("ext-sql-import").Parse(`
{{define "page"}}
<div class="uc-extsql-import" data-entity-type="{{.EntityType}}">
<h1>{{.Heading}}</h1>
<p>{{.Intro}}</p>
{{if .Sources}}
<form id="uc-extsql-import-form">
<label for="uc-extsql-import-source">{{.SourceLabel}}</label>
<select id="uc-extsql-import-source" name="source_id">
{{range .Sources}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
</select>
<button type="button" hx-post="{{.RelationsHref}}" hx-include="#uc-extsql-import-form" hx-target="#uc-extsql-import-result">{{.BrowseLabel}}</button>
</form>
{{else}}
<p class="uc-extsql-none">{{.NoSources}}</p>
<a href="/settings/sql-sources">{{.ConfigureLink}}</a>
{{end}}
<div id="uc-extsql-import-result"></div>
</div>
{{end}}

{{define "relations"}}
<div id="uc-extsql-relations">
<h2>{{.Heading}}</h2>
{{if not .Relations}}<p>{{.NoRelations}}</p>{{end}}
{{$v := .}}
<table class="uc-extsql-relations">
<tbody>
{{range .Relations}}
<tr>
<td>{{.Schema}}.{{.Name}}</td>
<td>{{.Kind}}</td>
<td>
<form>
<input type="hidden" name="source_id" value="{{$v.SourceID}}">
<input type="hidden" name="schema" value="{{.Schema}}">
<input type="hidden" name="relation" value="{{.Name}}">
<button type="button" hx-post="{{$v.PreviewHref}}" hx-include="closest form" hx-target="#uc-extsql-import-result">{{$v.SelectLabel}}</button>
</form>
</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{end}}

{{define "preview"}}
<div id="uc-extsql-preview">
<form id="uc-extsql-preview-form">
<input type="hidden" name="source_id" value="{{.SourceID}}">
<input type="hidden" name="schema" value="{{.Schema}}">
<input type="hidden" name="relation" value="{{.Relation}}">
<h2>{{.MappingHeading}}</h2>
{{if .TemplateNote}}<p class="uc-extsql-template-note">{{.TemplateNote}}</p>{{end}}
<table class="uc-import-mapping">
<tbody>
{{range .Mappings}}
<tr>
<td>{{.Header}}</td>
<td>
<select name="mapping.{{.Header}}">
{{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
</select>
</td>
</tr>
{{end}}
</tbody>
</table>
{{if .Constants}}
<h3>{{.ConstantsHeading}}</h3>
<table class="uc-extsql-constants">
<tbody>
{{$fixed := .FixedValueLabel}}
{{range .Constants}}
<tr><td>{{.Field}}</td><td>{{.Value}}</td><td>{{$fixed}}</td></tr>
{{end}}
</tbody>
</table>
{{end}}
</form>

{{if .MappingError}}
<p class="uc-import-mapping-error">{{.MappingError}}</p>
<button type="button" hx-post="{{.PreviewHref}}" hx-include="#uc-extsql-preview-form" hx-target="#uc-extsql-import-result">{{.RepreviewLabel}}</button>
{{else}}
<h2>{{.RowsHeading}}</h2>
<table class="uc-import-rows">
<tbody>
{{range .Rows}}
<tr class="{{if .OK}}uc-row-ok{{else}}uc-row-error{{end}}">
<td>{{.RowNumber}}</td>
<td>{{.Status}}</td>
<td>{{.Data}}</td>
<td>{{.Error}}</td>
</tr>
{{end}}
</tbody>
</table>
<p class="uc-extsql-row-cap">{{.RowCapNote}}</p>
<button type="button" hx-post="{{.PreviewHref}}" hx-include="#uc-extsql-preview-form" hx-target="#uc-extsql-import-result">{{.RepreviewLabel}}</button>
<button type="button" hx-post="{{.CommitHref}}" hx-include="#uc-extsql-preview-form" hx-target="#uc-extsql-import-result">{{.CommitLabel}}</button>
{{end}}
</div>
{{end}}

{{define "result"}}
<div class="uc-extsql-import-result">
<h2>{{.Heading}}</h2>
<p>{{.Succeeded}} {{.SucceededLabel}}, {{.Failed}} {{.FailedLabel}} ({{.Total}} total)</p>
<p class="uc-extsql-row-cap">{{.RowCapNote}}</p>
<table class="uc-import-rows">
<tbody>
{{range .Rows}}
<tr class="{{if .OK}}uc-row-ok{{else}}uc-row-error{{end}}">
<td>{{.RowNumber}}</td>
<td>{{.Status}}</td>
<td>{{.Error}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{end}}

{{define "error"}}
<p class="uc-extsql-import-error">{{.}}</p>
{{end}}
`))
