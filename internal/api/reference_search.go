// The searchable reference-field picker's backend (#24): a JSON endpoint
// that returns id+label options for one target entity type, filtered by a
// type-ahead query and capped, so a reference field no longer has to load
// EVERY record of its target into a <select>. That worked for a handful
// of demo customers and breaks — payload, render time, unusable UX — at
// real customer-list scale (Farshid, 2026-07-29: "selecting customer's
// name from dropdown ... will not work when there are 1000s or more").
//
// Reuses the same ListPageFiltered query the list page uses (#21), so
// there is one paged/filtered read path, not two.
package api

import (
	"net/http"
	"strconv"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
)

// referenceSearchLimit caps how many options one search returns. A picker
// shows a short list a human scans, then narrows by typing — returning
// hundreds would recreate the very payload problem this endpoint exists to
// remove. Deliberately small.
const referenceSearchLimit = 20

// searchReferenceOptions serves GET /api/references/{entityType}?q=... —
// the async source for the reference-field combobox. It searches the
// target entity's label field (name/title, the same convention the old
// full <select> used) and returns [{id, label}], capped.
func (h *Handler) searchReferenceOptions(w http.ResponseWriter, r *http.Request) {
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

	def, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	locale := localeFromRequest(w, r)

	// The field the picker labels by — name, then title, then the raw id.
	// Searching by id is meaningless to a human, so a target with no label
	// field is searchable only by listing (rare; every real entity
	// declares name).
	labelField := referenceLabelFieldFor(def)

	// Decide whether the label field can drive the sort/filter query. It
	// CAN'T when it's hidden from this viewer (a FieldPermission,
	// ADR-0006): the guarded engine's rejectHiddenSortFilter would 403 the
	// whole request, breaking the picker for a role that can read the
	// entity but not that one field. In that case we degrade to an
	// unsorted, unfiltered capped listing (the labels are still resolved
	// below) rather than failing — the same graceful degradation
	// referenceLabelFor uses.
	//
	// When the label field is an i18n_text field (ADR-0009), the stored
	// value is a locale->string JSON object, not plain text — sort/filter
	// use the viewer-locale-aware ListPageOptions.SortI18nLocales/
	// FilterI18nText mechanism instead of the plain data->>field path, so
	// this no longer needs to degrade. No per-locale JSONB index backs
	// this (uc-infra#95's separate, larger deferred "dynamic DDL" scope)
	// — FilterI18nText is a plain EXISTS/jsonb_each_text scan and
	// SortI18nLocales an unindexable COALESCE, on the SAME i18n_text
	// targets listview.go's own sortFilterableField comment already notes
	// are NOT rare, low-cardinality controlled vocabularies (Project/Task/
	// FixedAsset are unbounded transactional entities). Every debounced
	// keystroke against one of those pickers is therefore a sequential
	// scan today; accepted for now because the pre-fix degraded path was
	// already a sequential scan (ListPageFiltered's own unfiltered listing
	// still reads every row), not a regression — but real usage volume
	// should decide whether uc-infra#95's indexing work gets prioritized,
	// not this comment alone.
	//
	// FilterI18nText deliberately matches a query against EVERY locale's
	// translation (OR-across-locales), not just the viewer's own resolved
	// label — a scoped BA/Architect call for THIS endpoint (uc-infra#249),
	// not an oversight. This is a real, intentional divergence from
	// listview.go's referenceIDsMatching, which resolves matches
	// APPLICATION-side against only the viewer-locale label specifically
	// to avoid cross-locale matches (uc-infra#245's own review called that
	// exact behaviour a defect for the list-column filter). The two
	// surfaces now behave differently on purpose: a picker's whole job is
	// helping someone FIND a record from partial/uncertain knowledge of
	// its label (they may have typed the term they know it by in another
	// language), where a false-negative (a real match not surfacing)
	// costs more than an occasional label with no visible relationship to
	// what was typed; a list column's filter is narrowing an already-
	// visible column the user is reading top-to-bottom, where a result
	// with no visible relationship to the query reads as broken. If this
	// divergence ever gets "fixed" by a future change, that has to be a
	// deliberate call in one direction, not an accidental copy-paste from
	// whichever of the two this comment or listview.go's own is read
	// first. (The narrower "en" matches literally everything defect
	// #245 fixed does NOT recur here: jsonb_each_text yields each
	// locale's VALUE, never its key, so a query never matches against a
	// locale code by accident.)
	canSortFilter := labelField != ""
	i18nText := false
	if canSortFilter {
		hidden, err := ts.crud.HiddenFields(r.Context(), entityType)
		if err != nil {
			writeInternalError(w, "resolve hidden fields for "+entityType, err)
			return
		}
		if hidden[labelField] {
			canSortFilter = false
		}
		if f, ok := def.FieldByName(labelField); ok && f.Type == entity.FieldI18nText {
			i18nText = true
		}
	}

	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	opts := data.ListPageOptions{
		Limit:  referenceSearchLimit,
		Offset: (page - 1) * referenceSearchLimit,
	}
	if canSortFilter {
		opts.SortField = labelField
		if i18nText {
			// Dedupe here, not inside Catalog.FallbackChain itself —
			// FallbackChain's own contract (and its pinned test) is to
			// return ResolveLocalized's exact walked sequence, duplicates
			// included (e.g. FallbackChain("en") is
			// ["en","en","en","en"]: base language of "en" is "en" again,
			// and the catalog fallback commonly IS "en"). Left undeduped,
			// every COALESCE arm this drives would carry redundant
			// identical branches — harmless for correctness, but that's
			// 2 bind params and one extraction evaluated per row per
			// comparison for nothing. Order-preserving so precedence is
			// unchanged.
			opts.SortI18nLocales = dedupeLocales(h.catalog.FallbackChain(locale))
		}
		if q != "" {
			opts.FilterField = labelField
			opts.FilterValue = q
			opts.FilterI18nText = i18nText
		}
	}

	// Honour the field's own declared Field.TargetFilter/
	// MustMatchParentField (uc-infra#78) — the picker must itself filter
	// candidates, not just have crud.Engine reject a bad pick after the
	// fact. sourceEntityType/sourceField identify WHICH reference field
	// is searching (a Party picker serves many different fields —
	// TimeEntry.employee_id and SalesOrder.customer_id both target
	// Party with different constraints — so entityType, the TARGET
	// type, is not enough on its own to know which constraint applies).
	// Both are supplied by the picker's own rendered markup
	// (formrender's data-entity-type/data-field attributes), not typed
	// by the user, and siblingValue is the SOURCE record's own current
	// value of MustMatchParentField (formrender's data-must-match-field
	// attribute tells the client which sibling input to read) — empty
	// when unknown, in which case MustMatchParentField simply
	// contributes no narrowing yet (see ResolveReferenceFilter's own
	// doc comment). A source field that doesn't resolve, or isn't
	// actually a FieldReference targeting entityType, is ignored rather
	// than failing the whole search — the same graceful-degradation
	// posture the label/sort logic above already takes.
	if sourceEntityType := r.URL.Query().Get("source_entity_type"); sourceEntityType != "" {
		if sourceField := r.URL.Query().Get("source_field"); sourceField != "" {
			if sourceDef, err := ts.entityDef(r.Context(), sourceEntityType); err == nil {
				if f, ok := sourceDef.FieldByName(sourceField); ok && f.Type == entity.FieldReference && f.Target == entityType {
					constraintOpts, err := ts.crud.ResolveReferenceFilter(r.Context(), f, r.URL.Query().Get("sibling_value"))
					if err != nil {
						writeInternalError(w, "resolve reference filter for "+sourceEntityType+"."+sourceField, err)
						return
					}
					opts.EqualsFilters = constraintOpts.EqualsFilters
					opts.IDIn = constraintOpts.IDIn
					opts.JoinFilters = constraintOpts.JoinFilters
				}
			}
		}
	}

	records, err := ts.crud.ListPageFiltered(r.Context(), def, opts)
	if err != nil {
		writeCrudError(w, "search reference options for "+entityType, err)
		return
	}

	// recordLabel resolves the label for the viewer's locale (i18n_text
	// aware) and falls back to the raw id — one resolution path shared with
	// the form/list label loaders.
	out := make([]formrender.ReferenceOption, 0, len(records))
	for _, rec := range records {
		out = append(out, formrender.ReferenceOption{ID: rec.ID, Label: h.recordLabel(def, rec, locale)})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// dedupeLocales drops repeat locale codes from a Catalog.FallbackChain
// result while preserving order (and therefore precedence) — see its call
// site's own doc comment for why this belongs here rather than inside
// FallbackChain itself.
func dedupeLocales(locales []string) []string {
	seen := make(map[string]bool, len(locales))
	out := make([]string, 0, len(locales))
	for _, l := range locales {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
