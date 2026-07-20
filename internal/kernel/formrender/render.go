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
	// ReferenceOptions holds every FieldReference field's picker options,
	// keyed by field name (not target entity type — two fields could
	// reference the same target with different filtering someday, the
	// same forward-looking reasoning Children's own doc comment gives
	// for keying by Target today). A field with no entry here (e.g. the
	// target entity has no records yet, or its lookup failed and was
	// skipped rather than failing the whole render — see
	// internal/api's loadReferenceOptions) simply renders an empty
	// dropdown, not an error.
	ReferenceOptions map[string][]ReferenceOption
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
	PostHref string
	// HiddenFields carries every entDef field the form doesn't show in
	// any fields section, at its current stored value — see this file's
	// package-level note above buildHiddenFields for why this exists:
	// without it, a deliberately partial form (foundation.go explicitly
	// encourages building one as each field is actually needed, not the
	// whole entity at once) would silently wipe every field it doesn't
	// show on every save, since the record-write path is a full
	// replacement, not a merge.
	HiddenFields      []hiddenFieldView
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

// hiddenFieldView is one entDef field the form doesn't visibly show —
// see viewModel.HiddenFields.
type hiddenFieldView struct {
	Name  string
	Value string
}

type fieldView struct {
	Name     string
	Label    string
	Type     entity.FieldType
	Required bool
	Value    any
	Checked  bool         // FieldBool only
	Options  []optionView // FieldEnum and FieldReference only
}

// optionView is one <option> — Label and Value differ for
// FieldReference (Value is the referenced record's id, Label is its
// display text, built by ReferenceOption below); for FieldEnum they're
// always the same (the enum value has no separate display text).
type optionView struct {
	Value    string
	Label    string
	Selected bool
}

// ReferenceOption is one selectable target record for a FieldReference
// field — ID is what's actually stored (the referenced record's id),
// Label is what the picker shows a human. Built by the caller (see
// internal/api's loadReferenceOptions) since fetching the target
// entity's records needs the registry/crud engine, which this package
// deliberately has no access to (Render only ever works with data
// already handed to it — same separation Data.Children already keeps
// for master-detail rows).
type ReferenceOption struct {
	ID    string
	Label string
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

	// rendered tracks every field name that actually produced a visible
	// input, across every ComponentFields section — not every field name
	// merely *listed* in the Definition. A field the Definition lists but
	// whose VisibleIf currently evaluates false (buildFields skips it,
	// below) is NOT in this set, and correctly falls through to
	// buildHiddenFields as if it were never on the form at all: a
	// conditionally-hidden field's value needs the exact same
	// preservation an always-off-form field does, or it's silently wiped
	// on save the moment its condition happens to be false (caught by
	// independent review re-verifying the off-form-field fix: the same
	// failure mode survives via visible_if if this set is built from the
	// Definition's listed fields instead of what actually rendered).
	rendered := make(map[string]bool, len(ent.Fields))

	for _, s := range def.Sections {
		sv := sectionView{Title: s.Title, Component: s.Component, Target: s.Target}

		switch s.Component {
		case form.ComponentFields:
			fields, err := r.buildFields(s, ent, effective, data.ReferenceOptions, locale)
			if err != nil {
				return viewModel{}, fmt.Errorf("section %q: %w", s.Title, err)
			}
			sv.Fields = fields
			for _, fv := range fields {
				rendered[fv.Name] = true
			}

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
	vm.HiddenFields = buildHiddenFields(ent, effective, rendered)

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

// buildHiddenFields is the fix for a real data-loss bug: the record-write
// path (internal/data.RecordRepo.UpdateTx) is a full replacement, not a
// merge — SET data = $1, not a per-field patch. A form only shows the
// fields it was built to show (foundation.go explicitly encourages
// building a form field-by-field, only "as each is actually needed by a
// real screen", not the whole entity up front), so without carrying
// every other entDef field through as a hidden input at its current
// value, saving a genuinely partial form would silently drop every field
// it doesn't display — found the hard way (independent review, opus, on
// internal/api's form-submit-htmx branch): an entity with a field not on
// its form lost that field's data on the very first real save.
//
// Trade-off worth knowing (flagged by that same review, not fixed here:
// no optimistic-locking/versioning exists anywhere in this kernel yet to
// fix it properly): this makes every save submit a full point-in-time
// snapshot of the whole record, not just the fields a given partial form
// actually edits. Two users with different partial forms open on the
// same record, saving around the same time, now race for the *entire*
// record (last write wins, including fields the loser's form never
// showed) rather than just the fields both happened to edit. Acceptable
// for now — no version/lock field exists to detect the conflict even if
// this function didn't do it this way — but a real gap if concurrent
// editing of the same record ever becomes a real scenario.
//
// rendered is the set of field names that actually produced a visible
// input this render — not every name merely listed in the Definition.
// The two differ exactly when a listed field's VisibleIf currently
// evaluates false: buildFields skips rendering it, so it's absent from
// rendered too, and correctly still gets a hidden fallback here. Building
// this set from the Definition's listed names instead (an earlier,
// incomplete version of this fix did) would leave a conditionally-hidden
// field neither visible nor preserved — caught by independent review
// re-verifying the off-form-field fix: the identical silent-data-loss
// failure mode survives via visible_if unless "shown" means "actually
// rendered for this record's current data", not "named somewhere in the
// form".
func buildHiddenFields(ent *entity.Definition, record map[string]any, rendered map[string]bool) []hiddenFieldView {
	var out []hiddenFieldView
	for _, ef := range ent.Fields {
		if rendered[ef.Name] {
			continue
		}
		out = append(out, hiddenFieldView{Name: ef.Name, Value: FormatFieldValue(record[ef.Name])})
	}
	return out
}

// FormatFieldValue renders a record field's stored Go value (whatever
// entity.ValidateRecord accepted — string, float64, bool, or nil for
// "not set") as the plain text an HTML attribute/hidden input carries,
// and internal/api.parseRecordFields's csvimport.Coerce round-trips back
// into the same Go type on the next submit. A nil/absent value becomes
// "" (matching csvimport's own "empty means absent" convention on the
// way back in), not the string "<nil>" text/template's default
// stringification would otherwise produce.
func FormatFieldValue(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case float64:
		// Matches rollup.go's own float formatting — avoids
		// strconv/fmt's default switch to scientific notation for large
		// or precise values, which csvimport.Coerce's strconv.ParseFloat
		// would round-trip correctly but is worth staying consistent
		// with anyway.
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	case string:
		return val
	default:
		return fmt.Sprint(val)
	}
}

func (r *Renderer) buildFields(s form.Section, ent *entity.Definition, record map[string]any, referenceOptions map[string][]ReferenceOption, locale string) ([]fieldView, error) {
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

		fv := fieldView{
			Name:     ff.Name,
			Label:    label,
			Type:     ef.Type,
			Required: ef.Required,
			Value:    FormatFieldValue(record[ff.Name]),
		}

		switch ef.Type {
		case entity.FieldBool:
			fv.Checked, _ = record[ff.Name].(bool)
		case entity.FieldEnum:
			current, _ := record[ff.Name].(string)
			if current == "" {
				// A new record with no explicit value honors the
				// Definition's own declared Default (e.g. Item.item_type's
				// Default: "stock") — found necessary after the empty-
				// option fix below regressed a real e2e test: Default was
				// declared on several Definitions but never actually
				// consulted anywhere before this, so it only ever "worked"
				// by the accident of a browser auto-selecting whichever
				// <option> happened to render first, which coincidentally
				// matched EnumValues[0] more often than not. Now it's
				// honored for the right reason, not by coincidence.
				if def, ok := ef.Default.(string); ok {
					current = def
				}
			}
			if current == "" {
				// A genuinely undefaulted, unset enum must stay a real,
				// selectable choice — see the identical reasoning on
				// FieldReference below. This also makes `required` on a
				// <select> actually mean something: an empty-string option
				// is what makes a browser's native "please select an item"
				// validation fire at all; without it, the browser's own
				// default (whichever option renders first) always counts
				// as a value present, so `required` never blocked anything.
				fv.Options = append(fv.Options, optionView{Value: "", Label: "", Selected: true})
			}
			for _, ev := range ef.EnumValues {
				label := r.i18n.TOrDefault(locale, "field."+ent.EntityType+"."+ff.Name+"."+ev, ev)
				fv.Options = append(fv.Options, optionView{Value: ev, Label: label, Selected: ev == current})
			}
		case entity.FieldReference:
			current, _ := record[ff.Name].(string)
			if current == "" {
				// An unset reference must stay a real, selectable choice
				// — without this, the browser's own <select> default
				// (whatever option happens to render first) would look
				// selected on an untouched new-record form even though
				// no value was actually chosen, and submitting it would
				// silently write that first option's id. Unconditional
				// on Required now (was only added for optional fields
				// originally): a *required* reference with no value is
				// exactly the case that most needs a real empty state,
				// so the browser's native validation can actually catch
				// an unmade choice instead of silently accepting
				// whichever option rendered first (found by review after
				// this field type first shipped as a usable dropdown —
				// see uc-infra's 2026-07-20 reference-dropdowns review).
				fv.Options = append(fv.Options, optionView{Value: "", Label: "", Selected: true})
			}
			for _, opt := range referenceOptions[ff.Name] {
				fv.Options = append(fv.Options, optionView{Value: opt.ID, Label: opt.Label, Selected: opt.ID == current})
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
{{range .HiddenFields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}
{{range .Sections}}
<section class="uc-section" data-component="{{.Component}}">
<h2>{{.Title}}</h2>
{{if eq .Component "fields"}}
{{range .Fields}}
<div class="uc-field">
<label for="{{.Name}}">{{.Label}}{{if .Required}}{{$.RequiredSuffix}}{{end}}</label>
{{if eq .Type "bool"}}<input type="hidden" name="{{.Name}}" value="false"><input type="checkbox" id="{{.Name}}" name="{{.Name}}" value="true" {{if .Checked}}checked{{end}}{{if .Required}} required{{end}}>
{{else if or (eq .Type "enum") (eq .Type "reference")}}<select id="{{.Name}}" name="{{.Name}}"{{if .Required}} required{{end}}>
{{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
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
