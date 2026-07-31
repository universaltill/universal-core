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
	// Version is the record's optimistic-locking counter (data.Record.
	// Version) at the moment this form was loaded — round-tripped through
	// a hidden "_version" field so the save this form eventually submits
	// can be rejected (409) if someone else saved a change in between,
	// instead of silently overwriting it. Meaningless for a new/unsaved
	// record (RecordID == ""), where it's simply not rendered.
	Version  int
	Record   map[string]any
	Children map[string][]map[string]any
	// ReferenceOptions carries only the label of each FieldReference
	// field's CURRENT value, keyed by field name — enough to show a name
	// instead of a raw id on an existing record's combobox (#24). It is no
	// longer the full candidate list: the searchable picker fetches
	// candidates on demand from internal/api's /api/references endpoint, so
	// a form render never loads every target record. A field with no entry
	// here (a new/unset field, or the current value's label couldn't be
	// resolved and was skipped rather than failing the whole render — see
	// internal/api's loadCurrentReferenceLabels) simply renders an empty
	// search box, not an error.
	ReferenceOptions map[string][]ReferenceOption
	// RedactedFields names the entity fields this form's viewer may not
	// see (internal/kernel/authz's FieldPermission rules, resolved by the
	// caller — this package deliberately has no access to the registry or
	// the permission model, same separation ReferenceOptions already
	// keeps). A redacted field renders neither as a visible input NOR as
	// one of viewModel.HiddenFields' preservation inputs, so its name
	// never reaches the DOM at all.
	//
	// Not to be confused with viewModel.HiddenFields, which means very
	// nearly the opposite: those are fields deliberately carried through
	// the form invisibly so a partial form doesn't wipe them. A redacted
	// field is preserved too, but server-side (authz.GuardedEngine's
	// EffectiveWriteFields restores its stored value on write) precisely
	// because sending it to a browser that must not see it is the thing
	// being prevented.
	RedactedFields map[string]bool
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
	// Version renders as a hidden "_version" input when RecordID != "" —
	// see Data.Version's doc comment. Zero value (0) for a new record,
	// but VersionKnown gates whether the template emits the input at all,
	// so a genuinely new record never submits a meaningless "_version=0"
	// that could be misread as "check against version 0".
	Version      int
	VersionKnown bool
	// PostHref is the form's own hx-post target, pre-built via
	// url.PathEscape the same way AddHref/WorkflowHref/
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
	HiddenFields         []hiddenFieldView
	Sections             []sectionView
	Actions              []actionView
	RequiredSuffix       string
	RelatedListEmpty     string
	MasterDetailEmpty    string
	MasterDetailAdd      string
	RefSearchPlaceholder string
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
	// AddHref is pre-built via net/url so a Target
	// name or record ID containing "&", "?", or similar can't get
	// interpreted as URL/query structure once the browser HTML-decodes the
	// attribute value (html/template doesn't URL-encode non-standard hx-*
	// attributes the way it does href/src).
	AddHref string
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
	// RefTarget is the referenced entity type — set only for
	// FieldReference, and it turns the field into a searchable combobox
	// backed by GET /api/references/{RefTarget} instead of a <select> of
	// every record. CurrentLabel is the label of the currently-selected
	// record, so an existing value shows its name, not a raw id, before
	// the user searches.
	RefTarget    string
	CurrentLabel string
	// I18nInputs is set only for FieldI18nText (ADR-0009): one entry per
	// supported locale, each rendered as its own text input named
	// "{Name}.{Locale}" so the form decoder can reassemble the per-locale
	// object. Ordered by locale for stable rendering.
	I18nInputs []i18nInputView
}

// i18nInputView is one locale's input within an i18n_text field. Required
// is set only on the fallback (primary) locale's input when the field is
// required — a required multilingual field must be named in the primary
// language at least, not in every language at once (ADR-0009).
type i18nInputView struct {
	Locale   string
	Value    string
	Required bool
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

// ReferenceOption is one target record for a FieldReference field — ID is
// what's actually stored (the referenced record's id), Label is what the
// picker shows a human. Used two ways: the caller pre-loads just the
// CURRENT value's option so an existing record's combobox shows a name on
// load (see internal/api's loadCurrentReferenceLabels), and the same type
// is the JSON shape internal/api's /api/references search endpoint returns
// for on-demand candidates. Either way it's built by the caller, since
// fetching target records needs the registry/crud engine this package
// deliberately has no access to (Render only ever works with data already
// handed to it — same separation Data.Children keeps for master-detail
// rows).
type ReferenceOption struct {
	// JSON tags are load-bearing: this type is marshalled directly by the
	// reference-search endpoint (internal/api/reference_search.go) and the
	// combobox JS reads o.id / o.label. Without the tags Go would emit
	// ID/Label and the picker would render blank options with no value.
	// snake_case also matches this repo's API convention (CLAUDE.md).
	ID    string `json:"id"`
	Label string `json:"label"`
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
		EntityType:           def.EntityType,
		RecordID:             data.RecordID,
		Version:              data.Version,
		VersionKnown:         data.RecordID != "",
		PostHref:             postHref,
		RequiredSuffix:       r.i18n.T(locale, "form.field.required_suffix"),
		RelatedListEmpty:     r.i18n.T(locale, "form.related_list.empty"),
		MasterDetailEmpty:    r.i18n.T(locale, "form.master_detail.empty"),
		MasterDetailAdd:      r.i18n.T(locale, "form.master_detail.add"),
		RefSearchPlaceholder: r.i18n.T(locale, "form.reference.search"),
		WorkflowStartVals:    string(valsJSON),
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
			fields, err := r.buildFields(s, ent, effective, data.ReferenceOptions, data.RedactedFields, locale)
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
			// Rendered server-side from Children, exactly like
			// master-detail. It previously carried an hx-get lazy-load
			// instead; that endpoint ignores the ref filter, so the
			// trigger replaced the section with a JSON dump of every
			// record of the target type. Removed rather than fixed at
			// the endpoint: the rows are already here, and a lazy-load
			// that fires on every form render is a request this page
			// does not need (independent review, board #20).
			sv.Children = buildChildRows(data.Children[s.Target])
		}

		vm.Sections = append(vm.Sections, sv)
	}
	vm.HiddenFields = buildHiddenFields(ent, effective, rendered, data.RedactedFields)

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
// redacted is the set of fields this viewer may not see (Data.
// RedactedFields). Those get NO hidden input either — emitting one would
// publish the field's name in the DOM and, worse, submit it back empty on
// every save. Their preservation happens server-side instead
// (authz.GuardedEngine.EffectiveWriteFields restores the stored value),
// which is the only place it can happen for a value the browser is never
// allowed to hold.
func buildHiddenFields(ent *entity.Definition, record map[string]any, rendered, redacted map[string]bool) []hiddenFieldView {
	var out []hiddenFieldView
	for _, ef := range ent.Fields {
		if rendered[ef.Name] || redacted[ef.Name] {
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

func (r *Renderer) buildFields(s form.Section, ent *entity.Definition, record map[string]any, referenceOptions map[string][]ReferenceOption, redacted map[string]bool, locale string) ([]fieldView, error) {
	var out []fieldView
	for _, ff := range s.Fields {
		if redacted[ff.Name] {
			// Skipped before evalVisibleIf, not after: a visible_if
			// expression can reference other fields' values, but this
			// field itself must not render regardless of what any
			// expression concludes. Skipping here also keeps it out of
			// the `rendered` set the caller builds, which is exactly what
			// makes buildHiddenFields' own redaction check below the only
			// other place this decision has to be made.
			continue
		}
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

		fallback := ff.Label
		if fallback == "" {
			fallback = ff.Name
		}
		// "field.{EntityType}.{FieldName}" mirrors the enum-value
		// convention below ("field.{EntityType}.{FieldName}.{Value}") —
		// falls back to the form.FormField.Label Go declared (or the raw
		// field name) when no translation exists yet, so this is
		// additive: an untranslated field still renders exactly as
		// before, it just isn't multilingual until a key is added.
		label := r.i18n.TOrDefault(locale, "field."+ent.EntityType+"."+ff.Name, fallback)

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
			// A reference now renders as a searchable combobox (#24) backed
			// by GET /api/references/{RefTarget}, not a <select> of every
			// record — so no option list is built here. The only thing the
			// server still needs to resolve is the CURRENT value's label, so
			// an existing record's picker shows the human-readable name
			// rather than a bare id on load; the caller pre-loads just that
			// one record's option in referenceOptions. Value (the id) is
			// already set from FormatFieldValue above and drives the hidden
			// input. An empty current value simply renders an empty search
			// box with an empty hidden id — the combobox's own empty state,
			// no synthetic blank <option> needed.
			current, _ := record[ff.Name].(string)
			fv.RefTarget = ef.Target
			for _, opt := range referenceOptions[ff.Name] {
				if opt.ID == current {
					fv.CurrentLabel = opt.Label
					break
				}
			}
		case entity.FieldI18nText:
			// One input per supported locale (ADR-0009), each pre-filled
			// from the stored per-locale object and named "{field}.{locale}"
			// so the API's form decoder can reassemble the object. Ordered
			// by locale (Available is sorted) for stable rendering.
			values, _ := record[ff.Name].(map[string]any)
			for _, loc := range r.i18n.Available() {
				val, _ := values[loc].(string)
				fv.I18nInputs = append(fv.I18nInputs, i18nInputView{
					Locale:   loc,
					Value:    val,
					Required: ef.Required && loc == r.i18n.Fallback(),
				})
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
{{if .VersionKnown}}<input type="hidden" name="_version" value="{{.Version}}">
{{end}}
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
{{else if eq .Type "reference"}}<div class="uc-ref" data-target="{{.RefTarget}}" data-field="{{.Name}}">
<input type="hidden" name="{{.Name}}" value="{{.Value}}">
<input type="text" id="{{.Name}}" class="uc-ref-search" autocomplete="off" value="{{.CurrentLabel}}" placeholder="{{$.RefSearchPlaceholder}}"{{if .Required}} required{{end}}>
<div class="uc-ref-results" hidden></div>
</div>
{{else if eq .Type "i18n_text"}}<div class="uc-i18n" data-field="{{.Name}}">
{{$fname := .Name}}{{range $i, $inp := .I18nInputs}}<div class="uc-i18n-row"><span class="uc-i18n-locale">{{$inp.Locale}}</span><input type="text"{{if eq $i 0}} id="{{$fname}}"{{end}} name="{{$fname}}.{{$inp.Locale}}" value="{{$inp.Value}}" autocomplete="off" aria-label="{{$fname}} {{$inp.Locale}}"{{if $inp.Required}} required{{end}}></div>
{{end}}</div>
{{else if eq .Type "enum"}}<select id="{{.Name}}" name="{{.Name}}"{{if .Required}} required{{end}}>
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
<div class="uc-related-list">
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
