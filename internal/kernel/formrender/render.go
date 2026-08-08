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

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	uclocale "github.com/universaltill/universal-core/internal/kernel/locale"
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

// HelpAffordanceCatalogKey is the single global i18n key for the "?"
// help affordance every generated form page carries (ADR-0023 in
// uc-infra, uc-infra#143), the same "one global key, not per-entity"
// reasoning as SaveActionCatalogKey above: the affordance's aria-label/
// tooltip text is identical across every entity type, so there is
// nothing to key per-entity on. Exported so i18n_coverage_test.go's
// external test package can enforce every locale actually translates
// it.
const HelpAffordanceCatalogKey = "help.affordance.label"

// HelpAffordanceDefaultLabel is the TOrDefault fallback for
// HelpAffordanceCatalogKey — exported so internal/api's listview.go
// (which builds an independent, identically-shaped helpView for list
// pages, see that file's own doc comment) uses this literal rather than
// its own copy that could silently drift from this one (found by
// independent review, uc-infra#143).
const HelpAffordanceDefaultLabel = "Help"

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
	// ChildReferenceLabels resolves each child row's FieldReference cells
	// to their target's label, keyed the same way as Children (by section
	// Target), then by the child entity's field name, then by the stored
	// id — the same field->id->label shape internal/api's own
	// pageReferenceLabels already returns for the top-level list view, so
	// the caller can reuse that helper per section rather than inventing a
	// second lookup convention. This is what makes a related-list/
	// master-detail column showing a reference (e.g. Lead.campaign_id)
	// print a name instead of a raw id — see childCellValue. A missing
	// section entry, field entry, or id entry all degrade the same way
	// cellText's referenceLabels does: the cell falls back to the raw
	// stored value rather than failing the render, since an unresolvable
	// reference (dangling id, or this viewer lacking read access to the
	// target) is a real, visible-but-broken state worth showing rather
	// than hiding.
	ChildReferenceLabels map[string]map[string]map[string]string
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
	// UnavailableSections names master_detail/related_list sections (keyed
	// by section Target, same convention as Children/ChildDefs) whose
	// child entity type has no published Definition for this tenant —
	// typically because that module isn't licensed here. Such a section
	// is omitted from the rendered form entirely (buildViewModel skips
	// it) rather than rendered as a misleadingly-empty table with a
	// broken "add" link, or allowed to fail the whole render: the parent
	// record and every other section are still perfectly renderable, and
	// a missing related module is a normal, supported tenant
	// configuration (foundation.go's Attachment/Item split, CRM without
	// Purchasing, etc.), not an error condition (uc-infra#132). The
	// caller (internal/api) is the only thing with access to the
	// Definition registry needed to tell "not licensed" apart from "list
	// query failed", so it resolves this the same way it precomputes
	// RedactedFields/ReferenceCreateLabels.
	UnavailableSections map[string]bool
	// DegradedSections names every master-detail/related-list section
	// (keyed by section.Target, same key space as Children) whose child
	// rows could NOT be loaded because the viewer lacks read access to
	// the child entity type (internal/api's loadMasterDetailChildren
	// degrading authz.ErrDenied rather than failing the whole render —
	// uc-infra#131). Distinct both from a section simply having zero real
	// children (absent from DegradedSections but still meant to roll up
	// to 0) and from UnavailableSections above (unlicensed at the
	// Definition level, omitted from the form entirely) — a degraded
	// section IS licensed and IS rendered, just without any rows this
	// viewer is allowed to see. A section IN this set must not be
	// treated as "zero children" by the roll-up computation below — see
	// that code's own comment for why conflating the two silently
	// corrupts a writable header field.
	DegradedSections map[string]bool
	// ChildRedactedFields is RedactedFields' equivalent for a
	// master_detail/related_list section's CHILD entity type, keyed the
	// same way as Children/ChildDefs (by section Target), then by the
	// child entity's field name. RedactedFields only ever governs the
	// parent entity's own Fields sections and HiddenFields inputs; before
	// this, a FieldPermission hiding a field on a child entity type had
	// no effect at all on a related-list/master-detail table — the child
	// Definition's full field order (childColumns) went straight into
	// both the column headers and every row's cells, leaking a hidden
	// field's name AND, once uc-infra#103 made column headers real text,
	// its human-readable label too — visible even with zero child rows,
	// since childColumns derives columns from the Definition, not the
	// rows (uc-infra#127). A field named here is dropped from both the
	// column set and every row's cells for that section — see
	// childColumns/buildChildRows. Missing/nil is treated as "nothing
	// redacted for this child type", same graceful-degrade convention as
	// ChildDefs/ChildReferenceLabels.
	ChildRedactedFields map[string]map[string]bool
	// RegionalLocale is the viewer's resolved regional-formatting
	// preference (internal/api's regionalLocale — language plus region
	// plus calendar, not just the bare language tag Render's own locale
	// parameter carries), used by a master_detail/related_list child
	// cell's FieldDate/FieldNumber formatting (see childCellValue) to
	// match internal/api/listview.go's cellText behaviour for the exact
	// same field types on the top-level list page (uc-infra#133).
	//
	// Precomputed by the caller, same reasoning as ChildReferenceLabels/
	// RedactedFields above: resolving a region needs the incoming HTTP
	// request (a "?region=" query param, a cookie) that this generic
	// kernel package deliberately has no access to. internal/kernel/
	// locale's own regionRules table is very much per-country (GB/US/
	// TR/IR/...), not jurisdiction-invariant — CLAUDE.md's kernel/
	// deterministic-core rule isn't about jurisdiction-invariance, it's
	// about no ENTITY-TYPE-specific business logic in the generic
	// engine. Depending on locale.Locale here doesn't cross that line:
	// the package is metadata/request-agnostic and doesn't branch on
	// entity type, same as entity/form/money/i18n, the other peer
	// kernel-tier packages this file already imports. Only resolving a
	// Locale FROM a request is genuinely internal/api's concern, which
	// is why the VALUE still has to come from the caller.
	//
	// The zero value (Locale{}) is not a usable preference — see that
	// type's own doc comment — so an unset RegionalLocale (most of this
	// package's own unit tests, or any future caller with no
	// request-scoped region to resolve) falls back to
	// uclocale.Default(locale) in buildViewModel rather than formatting
	// against a broken zero Locale.
	RegionalLocale uclocale.Locale
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
	// Help is the page-level "?" affordance (ADR-0023, uc-infra#143) —
	// one per page, since a form page documents exactly one topic
	// (def.EntityType's), not one per section.
	Help helpView
}

// helpView is the "?" affordance's render data. NOT shared with
// internal/api's recordListView — that package declares its own,
// independently, for the same reason columnView/recordRowView aren't
// shared either (see listview.go's own helpView doc comment); this
// comment previously claimed otherwise, which independent review
// flagged as a real hazard in a codebase where doc comments are the
// design record — someone editing this type or buildHelpView below
// could reasonably believe they'd fixed listview.go's copy too.
type helpView struct {
	// TopicID is the derived help.TopicID/RouteTopicID this affordance
	// points at, rendered unconditionally as a data-help-topic attribute
	// regardless of Disabled — independent review found that without
	// this, the actual entityType->TopicID wiring at this package's own
	// buildViewModel call site was unobservable by any test in this
	// slice: Href (below) only renders once HasContent is true, which is
	// false for every topic today, so a wrong call (e.g. a typo'd field)
	// would have shipped with every test still green.
	TopicID string
	// Label is the affordance's aria-label/tooltip text, resolved via
	// HelpAffordanceCatalogKey.
	Label string
	// Href is the topic's URL once it has content, "" otherwise. The
	// template renders no href attribute at all when this is "" — an
	// <a> without href is not a link and not in the tab order by
	// default, which is why the template also always sets tabindex="0"
	// regardless of this field (ADR-0023's "stay focusable even while
	// inert" decision).
	Href string
	// Disabled is true whenever Href == "" — kept as its own field
	// rather than the template testing Href directly so the template's
	// {{if .Help.Disabled}} reads as intent ("render the inert state"),
	// not as an accidental empty-string check.
	Disabled bool
}

// buildHelpView computes a page's help affordance. Split out from
// buildViewModel as a small, pure function taking hasContent as an
// explicit bool (rather than calling help.HasContent itself) so both of
// this function's branches — content exists vs. doesn't yet — are
// directly unit-testable without needing real embedded help content to
// exist for the "content exists" branch to mean anything (ADR-0023).
func buildHelpView(catalog *i18n.Catalog, locale, topicID string, hasContent bool) helpView {
	hv := helpView{
		TopicID:  topicID,
		Label:    catalog.TOrDefault(locale, HelpAffordanceCatalogKey, HelpAffordanceDefaultLabel),
		Disabled: !hasContent,
	}
	if hasContent {
		hv.Href = help.Href(topicID)
	}
	return hv
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
	// Min/Max mirror the field's own declared entity.Field Min/Max
	// (ADR-0018 §4), rendered as the number input's min/max HTML
	// attributes — the same "validation is defined once, applied
	// identically client- and server-side" principle Required's own
	// attribute already follows. Pre-formatted strings, not *float64:
	// text/template prints a non-nil *float64 as its pointer address, not
	// its value (fmt's %v on a pointer to a scalar doesn't dereference),
	// so formatting happens once here, matching FormatFieldValue's own
	// 'f'-verb convention. Empty means unset — same idiom CurrentLabel/
	// MustMatchParentField already use, no separate bool needed since an
	// HTML attribute is either present or absent.
	Min string
	Max string
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
	// See Data.RegionalLocale's doc comment: a caller that never resolved
	// one (this package's own unit tests, mostly) gets the plain
	// language default rather than formatting child-cell dates/numbers
	// against a useless zero Locale. Usable(), not a bare Language == ""
	// check: a hand-built Locale{Language: "..."} literal that never
	// went through Parse/Default/build would pass a Language check but
	// still carry zero formatting rules, silently corrupting output
	// (independent review, uc-infra#133 — e.g. FormatNumber(1234.5, -1)
	// on an unusable Locale drops the decimal separator entirely,
	// printing "12345").
	regionalLoc := data.RegionalLocale
	if !regionalLoc.Usable() {
		regionalLoc = uclocale.Default(locale)
	}

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
	helpTopicID := help.TopicID(def.EntityType)
	vm.Help = buildHelpView(r.i18n, locale, helpTopicID, help.HasContent(locale, helpTopicID))

	// Roll-ups are computed in a first pass, before any fields section is
	// built, because form.Section's RollUp/RollUpTarget sums a
	// master-detail section's child records "into a header field"
	// (form/definition.go) — a fields section elsewhere in the form (in
	// either slice order) must see the freshly computed total, not
	// whatever was last saved for that field.
	effective := make(map[string]any, len(data.Record))
	maps.Copy(effective, data.Record)
	rollUpTotals := make(map[string]float64)
	// rollUpComputed tracks which RollUpTarget fields actually got a
	// fresh total this render, as opposed to being skipped below — see
	// its use at the ComponentMasterDetail case, where sv.RollUpTotal
	// must stay "" (not "0") for a skipped section, since
	// rollUpTotals' own zero-value default is indistinguishable from a
	// genuinely-computed total of zero.
	rollUpComputed := make(map[string]bool)
	for _, s := range def.Sections {
		if s.Component != form.ComponentMasterDetail || s.RollUp == "" {
			continue
		}
		if data.UnavailableSections[s.Target] || data.DegradedSections[s.Target] {
			// Don't recompute (and don't zero) the roll-up target field
			// from a section we can't actually read children for right
			// now — leave whatever was last saved on the record alone,
			// the same "preserve, don't silently wipe" treatment an
			// off-form field already gets elsewhere in this file, rather
			// than confidently showing 0 for a total that may well be
			// nonzero. This covers two distinct cases: UnavailableSections
			// (unlicensed — the section is omitted from the form entirely,
			// so this is largely moot for display) and DegradedSections
			// (licensed and rendered, but this viewer lacks read access to
			// its children — Children[s.Target] being empty/absent here
			// means "couldn't load," not "zero rows," and summing it would
			// silently produce 0). RollUpTarget is an ordinary writable
			// field on the parent (e.g. PurchaseOrder.total, edited like
			// any other header field), not a read-only computed one, so a
			// wrong 0 here doesn't just mis-render — saving this form
			// persists it over the real total (independent review,
			// uc-infra#131: found live-corrupting internal/kernel/
			// purchasing/ledger.go's GL posting, which reads the stored
			// total directly). Leave effective[s.RollUpTarget] exactly as
			// maps.Copy already set it above (the record's last stored
			// value) and surface that same number as the displayed total
			// below (relevant for the DegradedSections case, since that
			// section IS still rendered), rather than a total this render
			// cannot actually verify. That value can itself be stale by
			// whatever changed since it was last saved — an accepted,
			// bounded gap: a viewer denied read on the child type
			// structurally cannot verify a live total either way, and
			// "possibly stale" is a materially smaller failure than
			// "silently zeroed."
			if n, ok := effective[s.RollUpTarget].(float64); ok {
				rollUpTotals[s.RollUpTarget] = n
				// A DegradedSection IS still rendered (unlike an
				// UnavailableSection, which buildViewModel omits entirely
				// below — see UnavailableSections' own doc comment), so it
				// needs rollUpComputed set too, or the ComponentMasterDetail
				// case's `s.RollUp != "" && rollUpComputed[...]` guard
				// suppresses the RollUpTotal line entirely instead of
				// showing the preserved value this comment promises
				// (independent review of this merge, 2026-08-07: an earlier
				// version of this fix populated rollUpTotals but never
				// rollUpComputed, silently dropping PR#123's reviewed
				// "surface the last-saved total" behavior for a degraded
				// section — display-only, effective[s.RollUpTarget] was
				// never at risk, but no review had approved the
				// suppression and no test caught it).
				rollUpComputed[s.RollUpTarget] = true
			}
			continue
		}
		if data.ChildRedactedFields[s.Target][s.RollUp] {
			// The roll-up SOURCE field itself is hidden from this viewer
			// on the child entity. authz.GuardedEngine.ListByField has
			// already stripped it from every child row before this ever
			// runs, so computeRollUp would silently sum a pile of
			// missing keys into 0 — and that 0 would then land in
			// `effective[s.RollUpTarget]` below, which is exactly what
			// feeds the PARENT's own visible field (e.g. PurchaseOrder.
			// total) and what a subsequent Save actually persists. A
			// viewer who can't see POLine.line_total would silently zero
			// out a real, nonzero PurchaseOrder.total on save (uc-infra
			// #127 independent review) — the same class of silent data
			// loss the UnavailableSections case just above already
			// guards against, just reached through redaction instead of
			// licensing. Same fix: leave `effective` (and rollUpTotals)
			// untouched, rollUpComputed stays false for this target.
			continue
		}
		// isMoney: RollUpTarget names a field on the PARENT entity (see
		// this loop's own later comment on that convention) — its
		// declared Type, not the child RollUp field's, is what a
		// subsequent Save round-trips through entity.ValidateRecord
		// against, so that's the type computeRollUp must sum as. By
		// construction (Architect-level design, uc-infra#136) a
		// coherent Definition never mismatches the two: a FieldMoney
		// header total is always rolled up from a FieldMoney child
		// field (PurchaseOrder.total <- POLine.line_total), never a
		// plain FieldNumber one.
		targetField, _ := ent.FieldByName(s.RollUpTarget)
		total, err := computeRollUp(data.Children[s.Target], s.RollUp, targetField.Type == entity.FieldMoney)
		if err != nil {
			return viewModel{}, fmt.Errorf("section %q: %w", s.Title, err)
		}
		rollUpTotals[s.RollUpTarget] = total
		rollUpComputed[s.RollUpTarget] = true
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
		// A master_detail/related_list section whose Target has no
		// published Definition here (module not licensed for this
		// tenant) is omitted entirely — see UnavailableSections' doc
		// comment. Checked before building anything else for this
		// section, and guarded on s.Target != "": UnavailableSections is
		// keyed by Target, and a ComponentFields section's Target is
		// conventionally empty (form.Definition.Validate only REQUIRES
		// Target on the other two component kinds, it doesn't forbid one
		// here), so without the guard a stray same-named entry could
		// wrongly hide a fields section too.
		if s.Target != "" && data.UnavailableSections[s.Target] {
			continue
		}
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
			sv.Children, sv.Columns = buildChildRows(data.Children[s.Target], data.ChildDefs[s.Target], data.ChildReferenceLabels[s.Target], data.ChildRedactedFields[s.Target], r.i18n, locale, regionalLoc)
			sv.AddHref = "/api/records/" + url.PathEscape(s.Target) + "/new"
			// rollUpComputed guards this, not just s.RollUp != "": a
			// skipped roll-up (UnavailableSections, or ChildRedactedFields
			// hiding the RollUp source field itself, both above) must
			// leave RollUpTotal at its zero value ("") so the template's
			// {{if .RollUpTotal}} suppresses the line entirely — checking
			// rollUpTotals[s.RollUpTarget] alone can't tell "skipped" from
			// "computed and genuinely zero", since a Go map miss and a
			// real 0.0 total are the same value.
			if s.RollUp != "" && rollUpComputed[s.RollUpTarget] {
				// RollUpTarget names a field on the PARENT entity (ent,
				// not the child section's Target) — see form.Section.
				// RollUp's doc comment: it sums into "a header field".
				// Keyed off ent.EntityType, not def.EntityType, to match
				// every other field.{EntityType}.{FieldName} lookup in
				// this file (buildFields, childColumns): field names
				// belong to the entity Definition, not the form one.
				sv.RollUpField = s.RollUpTarget
				sv.RollUpLabel = r.i18n.TOrDefault(locale, "field."+ent.EntityType+"."+s.RollUpTarget, s.RollUpTarget)
				if targetField, ok := ent.FieldByName(s.RollUpTarget); ok && targetField.Type == entity.FieldMoney {
					// The stored/summed total is minor units (uc-infra#136,
					// computeRollUp's own isMoney branch) — the SAME fix
					// buildFields'/childCellValue's own FieldMoney cases
					// already apply (uc-infra#68): a plain FormatNumber
					// call would print the raw integer ("2000") instead of
					// the major-unit decimal a human reads as money
					// ("20.00").
					//
					// STILL ungrouped (m.String()) here, unlike
					// childCellValue's own FieldMoney case, which is now
					// regionally grouped (uc-infra#166) — this total sits
					// directly below the child rows' line_total cells and
					// as of #166 now visually DISAGREES with them for a
					// grouped locale. Tracked, not fixed here, as
					// uc-infra#189 (#166's own "Non-goals" deliberately
					// deferred this rather than folding it into that diff).
					if m, err := money.FromAny(rollUpTotals[s.RollUpTarget]); err == nil {
						sv.RollUpTotal = m.String()
					} else {
						// A PRESERVED value (DegradedSections/
						// UnavailableSections above leave rollUpTotals at
						// whatever was last stored, never recomputed) can
						// itself be a legacy, not-yet-backfilled fractional
						// amount — money.FromAny rejecting it must not
						// suppress this line entirely (independent review
						// of uc-infra#136's first pass): that reintroduces
						// the exact silent-display-regression class
						// TestRender_DegradedSectionRollUpFieldKeepsSavedValueAndDisplaysIt
						// exists to catch, just reached through a legacy
						// money value instead of a missing rollUpComputed
						// flag. The raw number isn't a trustworthy
						// major-unit decimal yet, but showing it beats
						// silently hiding the total a form's own visible
						// "total" field still displays right above it.
						sv.RollUpTotal = regionalLoc.FormatNumber(rollUpTotals[s.RollUpTarget], -1)
					}
				} else {
					// Regionally formatted (uc-infra#133, independent review):
					// the child rows summed into this total now render their
					// own line_total/qty cells through regionalLoc — leaving
					// the total on raw strconv.FormatFloat would show a
					// grouped/Jalali-digit line item one row above an
					// ungrouped Latin-digit total that doesn't visually agree
					// with what it's the sum of, the same mismatch this
					// package's own history already flagged in the opposite
					// direction (childCellValue's doc comment, "a line_total
					// column could show 1e+06 one row above a roll-up total
					// reading 1000000").
					sv.RollUpTotal = regionalLoc.FormatNumber(rollUpTotals[s.RollUpTarget], -1)
				}
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
			sv.Children, sv.Columns = buildChildRows(data.Children[s.Target], data.ChildDefs[s.Target], data.ChildReferenceLabels[s.Target], data.ChildRedactedFields[s.Target], r.i18n, locale, regionalLoc)
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
		// Min/Max become the browser-native min=/max= attributes on a
		// number input (uc-infra#80). They are NOT emitted for a
		// FieldMoney input, even though entity.Field now accepts a bound
		// on one: the bound is declared in MINOR units while a money
		// input displays and submits MAJOR units, so putting the raw
		// bound on that input would enforce a value 100x off. Server-side
		// ValidateRecord still applies it — see the FieldMoney case in
		// entity/validate.go. Converting the bound for display needs the
		// currency's exponent, which this renderer is not given.
		if ef.Min != nil {
			fv.Min = strconv.FormatFloat(*ef.Min, 'f', -1, 64)
		}
		if ef.Max != nil {
			fv.Max = strconv.FormatFloat(*ef.Max, 'f', -1, 64)
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
// FieldReference and FieldEnum cells are resolved the same way
// internal/api's own list view (cellText) resolves its top-level
// columns — a reference through referenceLabels (falling back to the
// raw stored id when unresolved), an enum through the
// "field.{EntityType}.{FieldName}.{Value}" catalog convention. Before
// this, childCellValue only special-cased FieldI18nText, so every
// related-list/master-detail reference or enum column printed the raw
// stored UUID/code — invisible on a FORM's own fields, which already
// went through buildFields' resolution, and easy to miss because of
// that asymmetry (uc-infra#85).
//
// childDef may be nil (a Definition mismatch the caller already
// tolerates); the per-row key fallback keeps such a section rendering
// something rather than nothing.
//
// redacted names the child entity's fields this viewer may not see
// (Data.ChildRedactedFields[section.Target]) — filtered out of names
// before columns/rows are built, so a redacted field reaches neither
// the header (column NAME or its resolved LABEL) nor any row's cell.
// nil/empty means nothing is redacted for this child type, the same
// graceful-degrade convention RedactedFields itself follows.
//
// loc is the viewer's resolved regional-formatting preference
// (Data.RegionalLocale, or uclocale.Default(locale) when unset) —
// threaded through to childCellValue so a FieldDate/FieldNumber cell in
// a child row formats the same way the top-level list view's cellText
// does for the identical field types (uc-infra#133).
func buildChildRows(children []map[string]any, childDef *entity.Definition, referenceLabels map[string]map[string]string, redacted map[string]bool, catalog *i18n.Catalog, locale string, loc uclocale.Locale) ([]childRowView, []columnView) {
	names := childColumns(children, childDef, redacted)
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
	if len(names) == 0 {
		// Nothing to show per row either — without this, a child type
		// whose every field got redacted (or that genuinely has zero
		// declared fields) still emitted one content-free <tr>/
		// <div class="uc-related-row"> per child record: visually
		// broken, AND it discloses exactly how many child records exist
		// to a viewer permitted to see none of their fields (uc-infra#127
		// independent review). Falling through to the section's own
		// {{if not .Children}} empty-state message instead — the same
		// outcome a genuinely childless section already gets.
		return nil, columns
	}
	rows := make([]childRowView, 0, len(children))
	for _, child := range children {
		row := childRowView{Cells: make([]cellView, 0, len(names))}
		for _, name := range names {
			row.Cells = append(row.Cells, cellView{
				Field: name,
				Value: childCellValue(child, name, childDef, referenceLabels, catalog, locale, loc),
			})
		}
		rows = append(rows, row)
	}
	return rows, columns
}

// childColumns is the child Definition's declared field order, falling
// back to the union of the rows' own keys (sorted, so output stays
// deterministic — Go map iteration is randomized) when no Definition is
// available. Either way, any name redacted (see buildChildRows' own
// redacted param) is dropped before it's returned — this runs whether or
// not there are any rows, which is what makes a redacted column disappear
// even from an otherwise-empty related-list/master-detail table.
func childColumns(children []map[string]any, childDef *entity.Definition, redacted map[string]bool) []string {
	if childDef != nil && len(childDef.Fields) > 0 {
		cols := make([]string, 0, len(childDef.Fields))
		for _, f := range childDef.Fields {
			if redacted[f.Name] {
				continue
			}
			cols = append(cols, f.Name)
		}
		return cols
	}
	seen := map[string]bool{}
	var cols []string
	for _, child := range children {
		for k := range child {
			if !seen[k] && !redacted[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	sort.Strings(cols)
	return cols
}

// childCellValue resolves one child row's cell for column name. Every
// return path except a resolved FieldI18nText/FieldReference/FieldEnum
// value goes through FormatFieldValue, not the raw stored value — the
// same formatter cellText's own default case (internal/api/listview.go)
// falls back to, and for the same reason: a bare `any` printed by
// html/template's default verb switches a large/precise float64 to
// scientific notation ("1e+06" instead of "1000000"), which
// FormatFieldValue exists specifically to avoid (see its own doc
// comment; sv.RollUpTotal already routes through the equivalent
// strconv.FormatFloat call for exactly this reason). Before this, only
// the FieldI18nText path in this function got that treatment, so a
// PurchaseOrder Lines table's line_total column could show "1e+06" one
// row above a roll-up total reading "1000000" (independent review,
// uc-infra#85 follow-up).
//
// This DOES give a child cell the regional locale formatting
// (loc.FormatDate's Jalali calendar, loc.FormatNumber's digit grouping)
// cellText's own FieldDate/FieldNumber cases apply — loc is a
// pre-resolved uclocale.Locale, threaded down from Data.RegionalLocale
// via buildViewModel/buildChildRows exactly the way referenceLabels
// itself is precomputed by the caller (uc-infra#133; this function
// previously fell back to FormatFieldValue's raw, locale-agnostic
// formatting for both types, unlike cellText's identical top-level list
// cell).
func childCellValue(child map[string]any, name string, childDef *entity.Definition, referenceLabels map[string]map[string]string, catalog *i18n.Catalog, locale string, loc uclocale.Locale) any {
	v, present := child[name]
	if !present {
		return nil
	}
	if childDef == nil || catalog == nil {
		return FormatFieldValue(v)
	}
	f, ok := childDef.FieldByName(name)
	if !ok {
		return FormatFieldValue(v)
	}
	switch f.Type {
	case entity.FieldI18nText:
		if s, ok := catalog.ResolveLocalized(v, locale); ok {
			return s
		}
		// An unresolvable i18n value renders blank rather than as a raw
		// map — a missing translation is not something to show as Go
		// syntax.
		return nil
	case entity.FieldDate:
		// Same regional formatting cellText's FieldDate case applies to
		// a top-level list column (Jalali calendar for a Farsi loc,
		// otherwise Gregorian with the region's own field order/
		// separator) — the stored value is always ISO 8601, display
		// only, same "the form INPUT keeps the raw ISO value" scope
		// boundary cellText's own comment documents (this function has
		// no form-input path to worry about; every child cell here is
		// read-only table text).
		if s, ok := v.(string); ok && s != "" {
			return loc.FormatDate(s)
		}
		return FormatFieldValue(v)
	case entity.FieldNumber:
		// Same reasoning as cellText's FieldNumber case: locale-aware
		// grouping/decimal separator and digit shapes, precision left at
		// -1 (this column has no currency context to fix a scale from —
		// a FieldMoney child cell is its own case below, fixed at
		// money.Decimals instead; see that case's own comment).
		if n, ok := v.(float64); ok {
			return loc.FormatNumber(n, -1)
		}
		return FormatFieldValue(v)
	case entity.FieldMoney:
		// Same fix as buildFields'/buildHiddenFields' own FieldMoney
		// cases (uc-infra#68): the stored value is minor units (1050),
		// and a master-detail/related-list child cell must show the
		// major-unit decimal ("10.50"), not the raw integer a naive
		// stringification of the float64 would print. An un-coercible
		// value (absent, fractional-legacy pre-backfill) falls through
		// to nil — same "blank rather than garbage" choice as the
		// i18n_text case above.
		//
		// Regionally grouped via loc.FormatNumber(m.Major(),
		// money.Decimals), matching cellText's own FieldMoney case on
		// the top-level list page (uc-infra#166) — this used to be
		// m.String() (ungrouped), a real, separate gap from this case's
		// FieldDate/FieldNumber fix above, filed rather than folded in
		// here (out of uc-infra#133's stated scope) and now closed.
		if m, err := money.FromAny(v); err == nil {
			return loc.FormatNumber(m.Major(), money.Decimals)
		}
		return nil
	case entity.FieldReference:
		if id, ok := v.(string); ok && id != "" {
			if label, ok := referenceLabels[name][id]; ok {
				return label
			}
		}
		// Unresolved (dangling id, or the caller never populated a
		// label for it — same "not this viewer's fault" cases
		// referenceLabels' own doc comment lists) falls back to the raw
		// stored id, exactly like cellText's top-level list columns:
		// visible-but-broken beats silently hiding a reference.
		return FormatFieldValue(v)
	case entity.FieldEnum:
		// Same "field.{EntityType}.{FieldName}.{Value}" convention
		// buildFields' <select> option labels (see the FieldEnum case
		// there) and internal/api/listview.go's top-level list-view
		// cells already resolve through — not a new convention, just a
		// third caller of it.
		//
		// This closes a real gap, but only for COMPOSITION children:
		// until this, no composition child anywhere declared a FieldEnum
		// column (POLine/SOLine/GoodsReceiptLine/RequestForQuotationLine/
		// RequestForQuotationVendor/DepreciationSchedule/Task — none of
		// them have one), so a master-detail table cell holding an enum
		// value had always silently rendered the raw stored string (e.g.
		// "labour") regardless of viewer locale. ProjectBudgetLine.category
		// (uc-infra#79) is the first, and this closes the gap for that
		// case generically rather than shipping a table only correct in
		// English.
		//
		// It is NOT the first enum column in a RELATED LIST, though —
		// this function serves both buildChildRows callers (master_detail
		// and related_list use the identical helper), and three existing
		// production related lists already show an enum child column:
		// assets.FixedAssetForm's Maintenance History
		// (MaintenanceOrder.maintenance_type), crm.CampaignForm's Leads
		// (Lead.source), hr.EmployeeForm's Leave History
		// (LeaveRequest.leave_type). Their CELL values (not just their
		// headers, which already resolved via childColumns) start
		// rendering translated the moment this lands — a real,
		// previously-untested behavior change to those three screens,
		// caught by independent review of this change. See
		// TestRelatedList_ColumnHeadersAreLocalized_RealBrowser's
		// leave_type CELL-text assertion (internal/e2e/related_list_
		// test.go) for the regression pin — it already exercised this
		// exact cell for geometry; that test now also asserts the text.
		if s, ok := v.(string); ok && s != "" {
			return catalog.TOrDefault(locale, "field."+childDef.EntityType+"."+name+"."+s, s)
		}
		return FormatFieldValue(v)
	}
	return FormatFieldValue(v)
}

const tmplSrc = `<form class="uc-form" data-entity-type="{{.EntityType}}"{{if .RecordID}} data-record-id="{{.RecordID}}" data-record-label="{{.RecordLabel}}"{{end}} hx-post="{{.PostHref}}" hx-target="this" hx-swap="outerHTML">
<div class="uc-form-toolbar"><a class="uc-help-affordance" role="link" data-help-topic="{{.Help.TopicID}}"{{if .Help.Href}} href="{{.Help.Href}}"{{end}}{{if .Help.Disabled}} aria-disabled="true"{{end}} tabindex="0" aria-label="{{.Help.Label}}">?</a></div>
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
{{else if eq .Type "number"}}<input type="number" id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if .Min}} min="{{.Min}}"{{end}}{{if .Max}} max="{{.Max}}"{{end}}{{if .Required}} required{{end}}>
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
