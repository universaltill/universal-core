// The CSV export endpoint — the mirror of /import/{entityType} and the
// other half of Farshid's original "dynamic importer exporter plugins"
// ask (QUEUE.md: import shipped first, export was explicitly left as a
// known gap). Reuses csvimport.ExportCSV, which is itself built as the
// reverse of the same Coerce convention import already uses, so a file
// this endpoint produces re-imports through /import/{entityType} with
// the same field names and values — not byte-for-byte in every case,
// though (see ExportCSV's own doc comment on whitespace/empty-string
// trimming and its deliberate formula-injection escaping) — no separate
// export-specific format or mapping concept to maintain either way.
package api

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/csvimport"
)

// exportRecordsCSV streams every def record the tenant has, in
// declaration-field-order columns, as a CSV attachment. Unlike the
// import wizard, there's no preview/confirm step: reading your own
// tenant's own data out has no destructive effect to confirm before, the
// same reasoning GET /api/records/{entityType} already gets away with
// needing no confirmation either.
//
// Once csvimport.ExportCSV starts writing to w, the response's 200
// status and headers are already committed (the first Write flushes
// them) — a failure partway through a large export can't be turned into
// a clean error response at that point, only logged server-side. Same
// accepted tradeoff any streaming HTTP export has; a failure this far in
// is a genuine internal/DB error, not a client mistake to explain.
func (h *Handler) exportRecordsCSV(w http.ResponseWriter, r *http.Request) {
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
	records, err := ts.crud.List(r.Context(), def)
	if err != nil {
		writeCrudError(w, fmt.Sprintf("list %s records for export", entityType), err)
		return
	}
	// A hidden field's VALUES are already stripped from records above by
	// the guarded engine, but ExportCSV builds its header and column
	// order from the Definition, so the column itself has to go too —
	// otherwise the file names every field being withheld and pads it
	// with blanks. Exported through a filtered copy of the Definition
	// rather than a filtered result set, so header and cells can't drift
	// apart. (A file produced this way still re-imports cleanly: the
	// import wizard maps by header name, and a column that isn't there
	// is simply a field the import doesn't set.)
	redacted, err := ts.crud.HiddenFields(r.Context(), entityType)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("resolve hidden fields for %s export", entityType), err)
		return
	}
	if len(redacted) > 0 {
		visible := *def
		visible.Fields = visibleFields(def, redacted)
		def = &visible
	}
	rows := make([]map[string]any, len(records))
	for i, rec := range records {
		rows[i] = rec.Data
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// entityType is sanitized before landing in this header, not used
	// verbatim: it's a raw URL path segment, so — independent of whether
	// anything in this kernel could actually get an entity type
	// containing a CR/LF or quote published today — a header-injection
	// sink built on unsanitized user-influenceable input is exactly the
	// kind of mistake worth closing at the point it's introduced, not
	// deferring on "nothing currently reaches it."
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, safeFilenameComponent(entityType)))
	if err := csvimport.ExportCSV(w, def, rows); err != nil {
		log.Printf("api: export %s records: %v", entityType, err)
	}
}

// safeFilenameComponent keeps only ASCII letters/digits/underscore/
// hyphen, replacing everything else with "_" — enough to make s safe to
// embed in a Content-Disposition header value (no quote to close the
// filename="..." string early, no CR/LF to inject a second header) and
// safe as a literal filename on every mainstream OS (no path separator,
// no reserved character).
func safeFilenameComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
