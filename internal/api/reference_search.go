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

	// The field the picker labels by — name, then title, then the raw id.
	// Searching by id is meaningless to a human, so a target with no label
	// field is searchable only by listing (rare; every real entity
	// declares name).
	labelField := referenceLabelFieldFor(def)

	// If this viewer may not SEE the label field (a FieldPermission hides
	// it, ADR-0006), we must not sort or filter by it: the guarded engine's
	// rejectHiddenSortFilter would 403 the whole request, breaking the
	// picker for a role that can legitimately read the entity but not that
	// one field. Degrade the same way referenceLabelFor does — fall back to
	// an unsorted, unfiltered listing labelled by raw id (the redacted
	// label field comes back empty anyway) rather than failing entirely.
	if labelField != "" {
		hidden, err := ts.crud.HiddenFields(r.Context(), entityType)
		if err != nil {
			writeInternalError(w, "resolve hidden fields for "+entityType, err)
			return
		}
		if hidden[labelField] {
			labelField = ""
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
	if labelField != "" {
		opts.SortField = labelField
		if q != "" {
			opts.FilterField = labelField
			opts.FilterValue = q
		}
	}

	records, err := ts.crud.ListPageFiltered(r.Context(), def, opts)
	if err != nil {
		writeCrudError(w, "search reference options for "+entityType, err)
		return
	}

	out := make([]formrender.ReferenceOption, 0, len(records))
	for _, rec := range records {
		label := rec.ID
		if labelField != "" {
			if s, ok := rec.Data[labelField].(string); ok && s != "" {
				label = s
			}
		}
		out = append(out, formrender.ReferenceOption{ID: rec.ID, Label: label})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
