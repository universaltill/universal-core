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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/money"
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

// SaveActionCatalogKey is the single global i18n key for every Save
// button, mirroring form.field.required_suffix's own global-key
// precedent (RequiredSuffix above). It's global rather than per-entity
// because every OpSave action in every Definition this kernel ships
// uses the identical literal Label "Save" — there is no per-entity
// variation to key on. Other Action ops (workflow.start/report.render/
// navigate) are NOT resolved through the catalog: no production
// Definition uses one yet, and each carries a genuinely per-instance
// Label (e.g. "Submit for Approval"), so this same TOrDefault approach
// would need a per-entity-and-action key, not this constant — add that
// the same way SectionCatalogKey works below, when a real form actually
// needs one. Exported so i18n_coverage_test.go's external test package
// can enforce every locale actually translates it.
const SaveActionCatalogKey = "form.action.save"

// sectionSlugRe turns a Section.Title into the slug half of
// sectionCatalogKey's key. Any run of characters outside [a-z0-9]
// (spaces, hyphens, "and", punctuation) becomes a single underscore;
// slugifyTitle then trims a leading/trailing one so "Lead-time stages"
// -> "lead_time_stages", not "_lead_time_stages_" or two different
// slugs depending on which non-alnum character happened to separate
// two words.
var sectionSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyTitle is the deterministic Title->slug transform both this
// package's own lookup (sectionCatalogKey) and whatever authors a
// locale JSON's keys must agree on byte-for-byte — see this file's
// i18n_coverage_test.go sibling, which fails the build the moment the
// two drift apart for any real Definition.
func slugifyTitle(title string) string {
	return strings.Trim(sectionSlugRe.ReplaceAllString(strings.ToLower(title), "_"), "_")
}

// SectionCatalogKey mirrors internal/api/locale.go's entityDisplayName
// convention ("entity."+entityType+".name" with the raw value as
// TOrDefault's fallback): a per-entity-type key so the same English
// section title on two different entities ("Details" on both Item and
// POLine) can carry two independently-authored translations, exactly
// like field."+entityType+"."+fieldName already does for field labels.
// Exported so i18n_coverage_test.go's external test package can compute
// the same key this package's own render path looks up, and callers
// authoring/auditing locale JSON have one canonical way to derive it
// instead of re-deriving the slugify rule by hand.
func SectionCatalogKey(entityType, title string) string {
	return "form." + entityType + ".section." + slugifyTitle(title)
}

// ActionCatalogKey is SectionCatalogKey's counterpart for a non-Save
// Action's Label: a per-entity-type key ("form."+entityType+".action."+
// slug(label)") so two entities can independently translate an action
// with the same English label, same reasoning as field labels and
// section titles. This is the "add a per-entity-and-action key the same
// way SectionCatalogKey works... when a real form actually needs one"
// this file's own SaveActionCatalogKey doc comment already flagged as
// the next step — the UBL export download button (uc-infra#66,
// following up #27's independent review finding that the export was
// unreachable from the UI) is the first real form to need it. Exported
// so i18n_coverage_test.go's external test package can compute the same
// key this package's own render path looks up.
func ActionCatalogKey(entityType, label string) string {
	return "form." + entityType + ".action." + slugifyTitle(label)
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
	// ChildDefs is each child entity type's published Definition, keyed
	// the same way as Children. Needed so a child table's columns come
	// from the Definition's field order and its i18n_text cells resolve
	// to the viewer's locale rather than printing a raw map — see
	// buildChildRows. A missing entry degrades to per-row keys.
	ChildDefs map[string]*entity.Definition
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
	// RecordLabel is the current record's own human-readable label (the
	// caller's recordLabel logic — same "name"/"title"/LabelField
	// convention ReferenceOptions' labels already use), set only when
	// RecordID != "". It renders as a data-record-label attribute on the
	// form tag purely so the inline reference-picker quick-create modal
	// (part 2 of #24, see layout.go) can read the just-created record's
	// label straight off the swapped-in success fragment without a
	// second round trip — formrender has no other use for it.
	RecordLabel string
	// ReferenceCreateLabels carries the "+ Create new {Entity}" button
	// label for each FieldReference field whose viewer holds create
	// permission on the field's target entity, keyed by field name. A
	// field with no entry here renders no quick-create affordance at
	// all — the caller (internal/api) is the only thing with access to
	// the RBAC engine and the entity-display-name/i18n lookups needed to
	// build this text, so it is precomputed the same way CurrentLabel is.
	ReferenceCreateLabels map[string]string
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
	// RecordLabel mirrors Data.RecordLabel — see that field's doc comment.
	RecordLabel string
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
	Title     string
	Component form.Component
	Fields    []fieldView
	Target    string
	Children  []childRowView
	// Columns is the child table's column order, from the child
	// Definition — see buildChildRows on why it can't come per row.
	Columns []columnView
	// RollUpField is the raw target field name (stable for data-field,
	// same reasoning as cellView.Field/columnView.Field below).
	// RollUpLabel is that field's resolved display text — see
	// buildViewModel's ComponentMasterDetail case.
	RollUpField string
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
	// CreateNewLabel is the "+ Create new {Entity}" quick-create button's
	// text (part 2 of #24) — empty means the viewer has no create
	// permission on RefTarget (or the target has none configured) and no
	// button renders at all. See Data.ReferenceCreateLabels.
	CreateNewLabel string
	// MustMatchParentField mirrors the field's own declared
	// entity.Field.MustMatchParentField (uc-infra#78) — empty unless the
	// Definition declares one. Rendered as the combobox's own
	// data-must-match-field attribute so the picker's client-side script
	// knows WHICH sibling input on this same form to read before it
	// queries GET /api/references (the sibling's value is submitted as
	// the search's sibling_value, so the server can apply the
	// declared "target must share this field's value" constraint). Purely
	// declarative — formrender never inspects what "project_id" means,
	// it only relays what the Definition already said.
	MustMatchParentField string
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

// columnView is one child-table column header. Field is the raw,
// untranslated field name — kept in data-field so a stable selector
// survives across locales (the same reason cellView.Field stays raw).
// Label is Field resolved through the field.{EntityType}.{FieldName}
// catalog for display, falling back to Field itself when untranslated.
type columnView struct {
	Field string
	Label string
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
		RecordLabel:          data.RecordLabel,
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
		title := r.i18n.TOrDefault(locale, SectionCatalogKey(def.EntityType, s.Title), s.Title)
		sv := sectionView{Title: title, Component: s.Component, Target: s.Target}

		switch s.Component {
		case form.ComponentFields:
			fields, err := r.buildFields(s, ent, effective, data.ReferenceOptions, data.ReferenceCreateLabels, data.RedactedFields, locale)
			if err != nil {
				return viewModel{}, fmt.Errorf("section %q: %w", s.Title, err)
			}
			sv.Fields = fields
			for _, fv := range fields {
				rendered[fv.Name] = true
			}

		case form.ComponentMasterDetail:
			sv.Children, sv.Columns = buildChildRows(data.Children[s.Target], data.ChildDefs[s.Target], r.i18n, locale)
			sv.AddHref = "/api/records/" + url.PathEscape(s.Target) + "/new"
			if s.RollUp != "" {
				// RollUpTarget names a field on the PARENT entity (ent,
				// not the child section's Target) — see form.Section.
				// RollUp's doc comment: it sums into "a header field".
				// Keyed off ent.EntityType, not def.EntityType, to match
				// every other field.{EntityType}.{FieldName} lookup in
				// this file (buildFields, childColumns): field names
				// belong to the entity Definition, not the form one.
				sv.RollUpField = s.RollUpTarget
				sv.RollUpLabel = r.i18n.TOrDefault(locale, "field."+ent.EntityType+"."+s.RollUpTarget, s.RollUpTarget)
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
			sv.Children, sv.Columns = buildChildRows(data.Children[s.Target], data.ChildDefs[s.Target], r.i18n, locale)
		}

		vm.Sections = append(vm.Sections, sv)
	}
	vm.HiddenFields = buildHiddenFields(ent, effective, rendered, data.RedactedFields)

	for _, a := range def.Actions {
		// A navigate Route may carry a "{id}" placeholder for the
		// current record's own id (e.g. "/export/PurchaseOrder/{id}/
		// ubl") — the same per-record link shape ReportHref/WorkflowHref
		// already build from data.RecordID, just declared in the
		// Definition instead of hardcoded here, so it stays generic
		// (works for any entity/route, not a PurchaseOrder-specific
		// branch — CLAUDE.md's kernel/deterministic-core boundary
		// rule). A record that doesn't exist yet (data.RecordID == "",
		// the "new" form) has nothing to navigate to, so the action is
		// omitted entirely rather than rendering a dead link — the same
		// reasoning Data.Version's own doc comment gives for not
		// rendering "_version" on a new record.
		if a.Op == form.OpNavigate && strings.Contains(a.Route, "{id}") && data.RecordID == "" {
			continue
		}
		av := actionView{Op: a.Op, Route: a.Route}
		if a.Op == form.OpNavigate {
			av.Route = strings.ReplaceAll(a.Route, "{id}", url.PathEscape(data.RecordID))
		}
		if a.Op == form.OpSave {
			// One global key: every OpSave action in every Definition
			// this kernel ships uses the identical literal Label, so
			// there is nothing to key per entity on (SaveActionCatalogKey's
			// own doc comment).
			av.Label = r.i18n.TOrDefault(locale, SaveActionCatalogKey, a.Label)
		} else {
			// Every other op carries a genuinely per-instance Label, so
			// it resolves through the per-entity-and-action key
			// SaveActionCatalogKey's doc comment already anticipated
			// (ActionCatalogKey, above) rather than the one shared
			// Save key. TOrDefault's usual additive-fallback guarantee
			// applies: a Definition whose action has no catalog entry
			// yet (none did before this change) renders exactly as it
			// always has, in every locale.
			av.Label = r.i18n.TOrDefault(locale, ActionCatalogKey(def.EntityType, a.Label), a.Label)
		}
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
		out = append(out, hiddenFieldView{Name: ef.Name, Value: hiddenFieldValue(ef, record[ef.Name])})
	}
	return out
}

// hiddenFieldValue renders a preserved-but-not-shown field's value for a
// hidden input — the same value buildFields' fv.Value would carry had
// this field actually been on the form. A plain FormatFieldValue would
// be wrong for FieldMoney (independent review, uc-infra#68): the stored
// value is minor units (1050), but csvimport.Coerce's FieldMoney case
// (what parseRecordFields feeds a resubmitted hidden input back through)
// expects the human major-unit decimal ("10.50"), the same as a visible
// money input's value attribute. Without this, EVERY save of a form that
// has a money field declared on the entity but not shown — a real,
// reachable shape (a VisibleIf-hidden field, or a tenant's own form
// Definition simply omitting it) — would silently multiply that field's
// stored amount by 100.
func hiddenFieldValue(ef entity.Field, v any) string {
	if ef.Type == entity.FieldMoney {
		if m, err := money.FromAny(v); err == nil {
			return m.String()
		}
		return ""
	}
	return FormatFieldValue(v)
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

func (r *Renderer) buildFields(s form.Section, ent *entity.Definition, record map[string]any, referenceOptions map[string][]ReferenceOption, referenceCreateLabels map[string]string, redacted map[string]bool, locale string) ([]fieldView, error) {
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
		case entity.FieldMoney:
			// The stored value is minor units (1050); the input's value
			// attribute must be the human major-unit decimal ("10.50") a
			// step="0.01" number input expects — the generic
			// FormatFieldValue assignment above would otherwise render
			// the raw integer verbatim (fv.Value already set to "1050"
			// by then, overridden here). A record with no value yet
			// (create form) leaves record[ff.Name] nil, money.FromAny
			// errors, and fv.Value stays "" from FormatFieldValue(nil) —
			// an empty input, not "0.00", so a required money field's
			// browser-native validation still fires on an untouched
			// field.
			if m, err := money.FromAny(record[ff.Name]); err == nil {
				fv.Value = m.String()
			}
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
			fv.MustMatchParentField = ef.MustMatchParentField
			fv.CreateNewLabel = referenceCreateLabels[ff.Name]
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

// buildChildRows renders a child table's rows.
//
// Columns come from the child DEFINITION's field order, not from each
// row's own keys. Deriving them per row (the original approach, sorted
// alphabetically for determinism) produced ragged tables the moment an
// optional field was set on some rows and not others: the row with a
// value got an extra cell and every column after it shifted, so one
// row's "project" sat under the next row's "status". Optional fields
// are normal — Task.parent_task_id is set on exactly the subset of rows
// that are subtasks — so this was guaranteed, not an edge case
// (independent review, board #18). A row missing a value now emits an
// empty cell in the right column instead.
//
// i18n_text values are resolved to the viewer's locale here for the
// same reason internal/api's list view resolves them: the stored value
// is a per-locale JSON object (ADR-0009), and printing it raw leaks
// "map[en:Design tr:Tasarım]" into the page — Go internals shown to a
// user, identical in every language. No child entity in this repo had
// an i18n field until Task.title, which is why nothing caught it
// before.
//
// childDef may be nil (a Definition mismatch the caller already
// tolerates); the per-row key fallback keeps such a section rendering
// something rather than nothing.
func buildChildRows(children []map[string]any, childDef *entity.Definition, catalog *i18n.Catalog, locale string) ([]childRowView, []columnView) {
	names := childColumns(children, childDef)
	columns := make([]columnView, 0, len(names))
	for _, name := range names {
		label := name
		// Same "field.{EntityType}.{FieldName}" convention buildFields
		// already resolves field labels through — falls back to the raw
		// name (childDef == nil, or no translation yet) so an unkeyed
		// column still renders exactly as before.
		if childDef != nil && catalog != nil {
			label = catalog.TOrDefault(locale, "field."+childDef.EntityType+"."+name, name)
		}
		columns = append(columns, columnView{Field: name, Label: label})
	}
	rows := make([]childRowView, 0, len(children))
	for _, child := range children {
		row := childRowView{Cells: make([]cellView, 0, len(names))}
		for _, name := range names {
			row.Cells = append(row.Cells, cellView{
				Field: name,
				Value: childCellValue(child, name, childDef, catalog, locale),
			})
		}
		rows = append(rows, row)
	}
	return rows, columns
}

// childColumns is the child Definition's declared field order, falling
// back to the union of the rows' own keys (sorted, so output stays
// deterministic — Go map iteration is randomized) when no Definition is
// available.
func childColumns(children []map[string]any, childDef *entity.Definition) []string {
	if childDef != nil && len(childDef.Fields) > 0 {
		cols := make([]string, 0, len(childDef.Fields))
		for _, f := range childDef.Fields {
			cols = append(cols, f.Name)
		}
		return cols
	}
	seen := map[string]bool{}
	var cols []string
	for _, child := range children {
		for k := range child {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	sort.Strings(cols)
	return cols
}

func childCellValue(child map[string]any, name string, childDef *entity.Definition, catalog *i18n.Catalog, locale string) any {
	v, present := child[name]
	if !present {
		return nil
	}
	if childDef == nil || catalog == nil {
		return v
	}
	if f, ok := childDef.FieldByName(name); ok {
		switch f.Type {
		case entity.FieldI18nText:
			if s, ok := catalog.ResolveLocalized(v, locale); ok {
				return s
			}
			// An unresolvable i18n value renders blank rather than as a
			// raw map — a missing translation is not something to show
			// as Go syntax.
			return nil
		case entity.FieldMoney:
			// Same fix as buildFields'/buildHiddenFields' own FieldMoney
			// cases (uc-infra#68): the stored value is minor units
			// (1050), and a master-detail/related-list child cell must
			// show the major-unit decimal ("10.50"), not the raw
			// integer a naive text/template stringification of the
			// float64 would print. An un-coercible value (absent,
			// fractional-legacy pre-backfill) falls through to nil —
			// same "blank rather than garbage" choice as the i18n_text
			// case above.
			if m, err := money.FromAny(v); err == nil {
				return m.String()
			}
			return nil
		}
	}
	return v
}

const tmplSrc = `<form class="uc-form" data-entity-type="{{.EntityType}}"{{if .RecordID}} data-record-id="{{.RecordID}}" data-record-label="{{.RecordLabel}}"{{end}} hx-post="{{.PostHref}}" hx-target="this" hx-swap="outerHTML">
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
{{else if eq .Type "reference"}}<div class="uc-ref" data-target="{{.RefTarget}}" data-field="{{.Name}}"{{if .MustMatchParentField}} data-must-match-field="{{.MustMatchParentField}}"{{end}}>
<input type="hidden" name="{{.Name}}" value="{{.Value}}">
<input type="text" id="{{.Name}}" class="uc-ref-search" autocomplete="off" value="{{.CurrentLabel}}" placeholder="{{$.RefSearchPlaceholder}}"{{if .Required}} required{{end}}>
<div class="uc-ref-results" hidden></div>
{{if .CreateNewLabel}}<button type="button" class="uc-ref-create" data-target="{{.RefTarget}}">{{.CreateNewLabel}}</button>{{end}}
</div>
{{else if eq .Type "i18n_text"}}<div class="uc-i18n" data-field="{{.Name}}">
{{$fname := .Name}}{{range $i, $inp := .I18nInputs}}<div class="uc-i18n-row"><span class="uc-i18n-locale">{{$inp.Locale}}</span><input type="text"{{if eq $i 0}} id="{{$fname}}"{{end}} name="{{$fname}}.{{$inp.Locale}}" value="{{$inp.Value}}" autocomplete="off" aria-label="{{$fname}} {{$inp.Locale}}"{{if $inp.Required}} required{{end}}></div>
{{end}}</div>
{{else if eq .Type "enum"}}<select id="{{.Name}}" name="{{.Name}}"{{if .Required}} required{{end}}>
{{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
</select>
{{else if eq .Type "date"}}<input type="date" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else if eq .Type "number"}}<input type="number" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else if eq .Type "money"}}<input type="number" step="0.01" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else}}<input type="text" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{end}}
</div>
{{end}}
{{else if eq .Component "master_detail"}}
<table class="uc-master-detail" data-target="{{.Target}}">
<thead><tr>{{range .Columns}}<th data-field="{{.Field}}">{{.Label}}</th>{{end}}</tr></thead>
<tbody>
{{range .Children}}<tr>{{range .Cells}}<td data-field="{{.Field}}">{{.Value}}</td>{{end}}</tr>{{end}}
</tbody>
</table>
{{if not .Children}}<p class="uc-empty">{{$.MasterDetailEmpty}}</p>{{end}}
{{if .RollUpTotal}}<p class="uc-rollup" data-field="{{.RollUpField}}">{{.RollUpLabel}}: {{.RollUpTotal}}</p>{{end}}
<button type="button" hx-get="{{.AddHref}}" hx-target="closest table" hx-swap="beforeend">{{$.MasterDetailAdd}}</button>
{{else if eq .Component "related_list"}}
<div class="uc-related-list">
{{if .Columns}}<div class="uc-related-header">{{range .Columns}}<span class="uc-related-header-cell" data-field="{{.Field}}">{{.Label}}</span>{{end}}</div>{{end}}
{{if not .Children}}<p class="uc-empty">{{$.RelatedListEmpty}}</p>{{end}}
{{range .Children}}<div class="uc-related-row">{{range .Cells}}<span class="uc-related-cell" data-field="{{.Field}}">{{.Value}}</span>{{end}}</div>{{end}}
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
