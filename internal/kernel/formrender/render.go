// Package formrender is the form renderer from ADR-0001 §6's rollout: it
// turns a form.Definition, an entity.Definition, and a record's data into
// HTML/HTMX output. Like every package under internal/kernel, it is a
// generic engine — behaviour comes only from the two Definitions and the
// record data passed in, never a per-entity-type branch (CLAUDE.md's
// kernel/deterministic-core boundary rule). Generated markup is never
// hand-patched (same rule): a fix belongs in the Form/Entity Definition or
// in this renderer, not in a one-off edit to rendered output.
package formrender

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/url"
	"sort"
	"strconv"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// Renderer renders form.Definitions. One Renderer serves every entity
// type and locale; the catalog and the Definitions passed per call are
// what make each render distinct.
type Renderer struct {
	i18n *i18n.Catalog
	tmpl *template.Template
}

func New(catalog *i18n.Catalog) *Renderer {
	return &Renderer{i18n: catalog, tmpl: template.Must(template.New("form").Parse(tmplSrc))}
}

// Data is everything the renderer needs beyond the two Definitions: the
// record's current field values (nil/empty for a new, unsaved record) and,
// for master_detail/related_list sections, each section's child records
// keyed by the section's Target entity type. Keying by Target rather than
// by section means two sections in the same form pointing at the same
// Target (e.g. two differently filtered related_lists) would collide and
// show identical rows — not a shape the current form schema examples use,
// but worth a key change (e.g. by section Title) if that need shows up.
type Data struct {
	RecordID string // empty for a new/unsaved record
	Record   map[string]any
	Children map[string][]map[string]any
}

// Render writes the HTML/HTMX form for def against ent's field shapes and
// data, in locale. It returns an error rather than guessing when a form
// field names an entity field that doesn't exist (Definition drift between
// form and entity) or when a visible_if/roll_up expression is malformed —
// the same "fail loud on schema drift" discipline crud.Engine applies to
// record validation.
func (r *Renderer) Render(w io.Writer, def *form.Definition, ent *entity.Definition, data Data, locale string) error {
	vm, err := r.buildViewModel(def, ent, data, locale)
	if err != nil {
		return err
	}
	return r.tmpl.Execute(w, vm)
}

type viewModel struct {
	EntityType string
	RecordID   string
	// PostHref is the form's own hx-post target, pre-built via
	// url.PathEscape the same way AddHref/RelatedListHref/WorkflowHref/
	// ReportHref are — EntityType/RecordID must not be interpolated
	// directly into the template's hx-post, since hx-post isn't a
	// URL-context attribute html/template auto-escapes for that purpose
	// (only attribute-context escaping applies), so a raw RecordID could
	// otherwise inject query structure into the form's own submit target.
	PostHref          string
	Sections          []sectionView
	Actions           []actionView
	RequiredSuffix    string
	RelatedListEmpty  string
	MasterDetailEmpty string
	MasterDetailAdd   string
	// WorkflowStartVals is the pre-built JSON body for every workflow.start
	// action's hx-vals. Built once via encoding/json (never by
	// hand-concatenating field values into a JSON-looking string) so a
	// record ID or entity type containing a quote can't break out of the
	// JSON structure — html/template's attribute-context escaping of the
	// already-valid JSON text round-trips losslessly in the browser, but
	// only because the JSON itself was built correctly first.
	WorkflowStartVals string
}

type sectionView struct {
	Title       string
	Component   form.Component
	Fields      []fieldView
	Target      string
	Children    []childRowView
	RollUpLabel string
	RollUpTotal string // empty when the section has no roll-up
	// AddHref and RelatedListHref are pre-built via net/url so a Target
	// name or record ID containing "&", "?", or similar can't get
	// interpreted as URL/query structure once the browser HTML-decodes the
	// attribute value (html/template doesn't URL-encode non-standard hx-*
	// attributes the way it does href/src).
	AddHref         string
	RelatedListHref string
}

type fieldView struct {
	Name     string
	Label    string
	Type     entity.FieldType
	Required bool
	Value    any
	Checked  bool         // FieldBool only
	Options  []optionView // FieldEnum only
}

type optionView struct {
	Value    string
	Selected bool
}

type childRowView struct {
	Cells []cellView
}

type cellView struct {
	Field string
	Value any
}

type actionView struct {
	Label        string
	Op           form.ActionOp
	WorkflowHref string // pre-built for OpWorkflowStart, see WorkflowStartVals
	ReportHref   string // pre-built for OpReportRender
	Route        string
}

func (r *Renderer) buildViewModel(def *form.Definition, ent *entity.Definition, data Data, locale string) (viewModel, error) {
	valsJSON, err := json.Marshal(map[string]string{
		"entity_type": def.EntityType,
		"record_id":   data.RecordID,
	})
	if err != nil {
		return viewModel{}, fmt.Errorf("build workflow.start hx-vals: %w", err)
	}

	postHref := "/api/records/" + url.PathEscape(def.EntityType)
	if data.RecordID != "" {
		postHref += "/" + url.PathEscape(data.RecordID)
	}

	vm := viewModel{
		EntityType:        def.EntityType,
		RecordID:          data.RecordID,
		PostHref:          postHref,
		RequiredSuffix:    r.i18n.T(locale, "form.field.required_suffix"),
		RelatedListEmpty:  r.i18n.T(locale, "form.related_list.empty"),
		MasterDetailEmpty: r.i18n.T(locale, "form.master_detail.empty"),
		MasterDetailAdd:   r.i18n.T(locale, "form.master_detail.add"),
		WorkflowStartVals: string(valsJSON),
	}

	// Roll-ups are computed in a first pass, before any fields section is
	// built, because form.Section's RollUp/RollUpTarget sums a
	// master-detail section's child records "into a header field"
	// (form/definition.go) — a fields section elsewhere in the form (in
	// either slice order) must see the freshly computed total, not
	// whatever was last saved for that field.
	effective := make(map[string]any, len(data.Record))
	maps.Copy(effective, data.Record)
	rollUpTotals := make(map[string]float64)
	for _, s := range def.Sections {
		if s.Component != form.ComponentMasterDetail || s.RollUp == "" {
			continue
		}
		total, err := computeRollUp(data.Children[s.Target], s.RollUp)
		if err != nil {
			return viewModel{}, fmt.Errorf("section %q: %w", s.Title, err)
		}
		rollUpTotals[s.RollUpTarget] = total
		effective[s.RollUpTarget] = total
	}

	for _, s := range def.Sections {
		sv := sectionView{Title: s.Title, Component: s.Component, Target: s.Target}

		switch s.Component {
		case form.ComponentFields:
			fields, err := buildFields(s, ent, effective)
			if err != nil {
				return viewModel{}, fmt.Errorf("section %q: %w", s.Title, err)
			}
			sv.Fields = fields

		case form.ComponentMasterDetail:
			sv.Children = buildChildRows(data.Children[s.Target])
			sv.AddHref = "/api/records/" + url.PathEscape(s.Target) + "/new"
			if s.RollUp != "" {
				sv.RollUpLabel = s.RollUpTarget
				sv.RollUpTotal = strconv.FormatFloat(rollUpTotals[s.RollUpTarget], 'f', -1, 64)
			}

		case form.ComponentRelatedList:
			sv.Children = buildChildRows(data.Children[s.Target])
			q := url.Values{}
			q.Set("ref", def.EntityType+":"+data.RecordID)
			sv.RelatedListHref = "/api/records/" + url.PathEscape(s.Target) + "?" + q.Encode()
		}

		vm.Sections = append(vm.Sections, sv)
	}

	for _, a := range def.Actions {
		av := actionView{Label: a.Label, Op: a.Op, Route: a.Route}
		switch a.Op {
		case form.OpWorkflowStart:
			av.WorkflowHref = "/api/workflows/" + url.PathEscape(a.Workflow) + "/start"
		case form.OpReportRender:
			q := url.Values{}
			q.Set("record_id", data.RecordID)
			av.ReportHref = "/api/reports/" + url.PathEscape(a.Report) + "?" + q.Encode()
		}
		vm.Actions = append(vm.Actions, av)
	}

	return vm, nil
}

func buildFields(s form.Section, ent *entity.Definition, record map[string]any) ([]fieldView, error) {
	var out []fieldView
	for _, ff := range s.Fields {
		visible, err := evalVisibleIf(ff.VisibleIf, record)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}

		ef, ok := ent.FieldByName(ff.Name)
		if !ok {
			return nil, fmt.Errorf("form field %q has no matching field on entity %q", ff.Name, ent.EntityType)
		}

		label := ff.Label
		if label == "" {
			label = ff.Name
		}

		value := record[ff.Name]
		if n, ok := value.(float64); ok {
			// Format explicitly (matching rollup.go's display format)
			// rather than relying on text/template's default float
			// printing, which can switch to scientific notation.
			value = strconv.FormatFloat(n, 'f', -1, 64)
		}

		fv := fieldView{
			Name:     ff.Name,
			Label:    label,
			Type:     ef.Type,
			Required: ef.Required,
			Value:    value,
		}

		switch ef.Type {
		case entity.FieldBool:
			fv.Checked, _ = record[ff.Name].(bool)
		case entity.FieldEnum:
			current, _ := record[ff.Name].(string)
			for _, ev := range ef.EnumValues {
				fv.Options = append(fv.Options, optionView{Value: ev, Selected: ev == current})
			}
		}

		out = append(out, fv)
	}
	return out, nil
}

// buildChildRows renders each child record's fields sorted by name for
// deterministic output — map iteration order in Go is randomized, and
// this render must be stable across calls (tested, and a stable diff for
// anyone reviewing rendered output).
func buildChildRows(children []map[string]any) []childRowView {
	rows := make([]childRowView, 0, len(children))
	for _, child := range children {
		names := make([]string, 0, len(child))
		for k := range child {
			names = append(names, k)
		}
		sort.Strings(names)

		row := childRowView{Cells: make([]cellView, 0, len(names))}
		for _, name := range names {
			row.Cells = append(row.Cells, cellView{Field: name, Value: child[name]})
		}
		rows = append(rows, row)
	}
	return rows
}

const tmplSrc = `<form class="uc-form" data-entity-type="{{.EntityType}}" hx-post="{{.PostHref}}" hx-target="this" hx-swap="outerHTML">
{{range .Sections}}
<section class="uc-section" data-component="{{.Component}}">
<h2>{{.Title}}</h2>
{{if eq .Component "fields"}}
{{range .Fields}}
<div class="uc-field">
<label for="{{.Name}}">{{.Label}}{{if .Required}}{{$.RequiredSuffix}}{{end}}</label>
{{if eq .Type "bool"}}<input type="checkbox" id="{{.Name}}" name="{{.Name}}" {{if .Checked}}checked{{end}}{{if .Required}} required{{end}}>
{{else if eq .Type "enum"}}<select id="{{.Name}}" name="{{.Name}}"{{if .Required}} required{{end}}>
{{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Value}}</option>{{end}}
</select>
{{else if eq .Type "date"}}<input type="date" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else if eq .Type "number"}}<input type="number" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else}}<input type="text" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{end}}
</div>
{{end}}
{{else if eq .Component "master_detail"}}
<table class="uc-master-detail" data-target="{{.Target}}">
<tbody>
{{range .Children}}<tr>{{range .Cells}}<td data-field="{{.Field}}">{{.Value}}</td>{{end}}</tr>{{end}}
</tbody>
</table>
{{if not .Children}}<p class="uc-empty">{{$.MasterDetailEmpty}}</p>{{end}}
{{if .RollUpTotal}}<p class="uc-rollup" data-field="{{.RollUpLabel}}">{{.RollUpLabel}}: {{.RollUpTotal}}</p>{{end}}
<button type="button" hx-get="{{.AddHref}}" hx-target="closest table" hx-swap="beforeend">{{$.MasterDetailAdd}}</button>
{{else if eq .Component "related_list"}}
<div class="uc-related-list" hx-get="{{.RelatedListHref}}" hx-trigger="load" hx-swap="innerHTML">
{{if not .Children}}<p class="uc-empty">{{$.RelatedListEmpty}}</p>{{end}}
{{range .Children}}<div class="uc-related-row">{{range .Cells}}<span data-field="{{.Field}}">{{.Value}}</span>{{end}}</div>{{end}}
</div>
{{end}}
</section>
{{end}}
<div class="uc-actions">
{{range .Actions}}
{{if eq .Op "save"}}<button type="submit">{{.Label}}</button>
{{else if eq .Op "workflow.start"}}<button type="button" hx-post="{{.WorkflowHref}}" hx-vals='{{$.WorkflowStartVals}}' hx-target="closest form" hx-swap="outerHTML">{{.Label}}</button>
{{else if eq .Op "report.render"}}<a href="{{.ReportHref}}" target="_blank">{{.Label}}</a>
{{else if eq .Op "navigate"}}<a href="{{.Route}}">{{.Label}}</a>
{{end}}
{{end}}
</div>
</form>`
