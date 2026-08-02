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

	// Decide whether the label field can drive the sort/filter query.
	// It CAN'T when:
	//   - it's hidden from this viewer (a FieldPermission, ADR-0006): the
	//     guarded engine's rejectHiddenSortFilter would 403 the whole
	//     request, breaking the picker for a role that can read the entity
	//     but not that one field.
	//   - it's an i18n_text field (ADR-0009): the stored value is a JSON
	//     object, so `data->>'name'` sorts/filters by the whole object's
	//     text, which is meaningless. Localized filtering needs a per-locale
	//     JSONB expression index and is deferred (ties into #64).
	// In both cases we degrade to an unsorted, unfiltered capped listing
	// (the labels are still resolved below) rather than failing — the same
	// graceful degradation referenceLabelFor uses. Controlled-vocabulary
	// targets, the ones that get i18n labels, have few rows, so an unfiltered
	// capped list is still usable.
	canSortFilter := labelField != ""
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
			canSortFilter = false
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
		if q != "" {
			opts.FilterField = labelField
			opts.FilterValue = q
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
