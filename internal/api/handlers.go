// Package api is the first real HTTP surface for a Definition-driven
// entity: it looks Definitions up from the registry (internal/data),
// drives crud.Engine and formrender.Renderer with them, and shapes the
// result through internal/httpx. Like every generic engine in this
// kernel, it must never branch on a specific entity type — behaviour
// comes only from the Definition the registry hands back (CLAUDE.md).
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	"github.com/universaltill/universal-core/internal/webauth"
)

// Handler wires the registry, crud.Engine, and formrender.Renderer
// together behind HTTP. One Handler serves every entity/form type.
type Handler struct {
	entityDefs *data.EntityDefinitionRepo
	formDefs   *data.FormDefinitionRepo
	crud       *crud.Engine
	renderer   *formrender.Renderer
	catalog    *i18n.Catalog
	auth       *webauth.Authenticator
}

// New builds a Handler. catalog is the i18n.Catalog forms (and the
// import wizard, import.go) render against (internal/i18n.Load). auth
// may be nil or disabled (webauth.Config.Enabled() == false) — Routes
// wires it unconditionally either way, since Guard/Register are both
// safe no-ops on a disabled Authenticator (see webauth's own doc
// comments).
func New(db *sql.DB, catalog *i18n.Catalog, auth *webauth.Authenticator) *Handler {
	return &Handler{
		entityDefs: data.NewEntityDefinitionRepo(db),
		formDefs:   data.NewFormDefinitionRepo(db),
		crud:       crud.NewEngine(db),
		renderer:   formrender.New(catalog),
		catalog:    catalog,
		auth:       auth,
	}
}

// Routes registers every handler onto mux, wrapped in
// h.auth.Guard(httpx.DevAuth(...)) — real login (webauth) is tried
// first; DevAuth (the insecure stopgap — see that package's doc
// comment) only ever runs when webauth is disabled entirely for this
// deployment, since Guard either populates the request context itself
// or redirects before DevAuth gets a chance (see DevAuth's own doc
// comment on why that composition is safe either way main.go always
// registers Routes, relying on DevAuth's own fail-closed default when
// neither is configured).
func (h *Handler) Routes(mux *http.ServeMux) {
	// Unauthenticated: a static asset with no tenant-specific content —
	// gating it behind auth would only break the page that needs it
	// (a 401/redirect for the very script tag meant to make that page
	// itself interactive) before auth can even run.
	mux.HandleFunc("GET /static/htmx.min.js", serveHTMX)
	// webauth's own /ui/login, /ui/auth/callback, /ui/logout — never
	// wrapped in Guard themselves; that's how a request gets a session
	// in the first place. No-op registration when webauth is disabled.
	h.auth.Register(mux)

	auth := func(handler http.HandlerFunc) http.Handler {
		return h.auth.Guard(httpx.DevAuth(handler))
	}
	// "/{$}" — the Go 1.22+ ServeMux exact-match wildcard — not plain
	// "/", which would act as a catch-all subtree match and silently
	// swallow every unmatched path into the dashboard instead of a real
	// 404. Until this route existed at all, "/" 404'd outright: a real
	// user with a real, working login had nowhere to land without
	// already knowing a specific /forms/{entityType}/... URL by heart.
	mux.Handle("GET /{$}", auth(h.renderDashboard))
	mux.Handle("GET /api/records/{entityType}", auth(h.listRecords))
	mux.Handle("POST /api/records/{entityType}", auth(h.createRecord))
	mux.Handle("GET /api/records/{entityType}/{id}", auth(h.getRecord))
	// POST, not PUT: formrender's own <form> tag always submits via
	// hx-post regardless of new vs. existing record (see render.go's
	// tmplSrc) — until this route existed at all, saving an existing
	// record's form 404'd outright (found via internal/e2e's real-browser
	// testing, not curl — no existing test ever exercised editing a
	// record that already existed).
	mux.Handle("POST /api/records/{entityType}/{id}", auth(h.updateRecord))
	mux.Handle("GET /forms/{entityType}/new", auth(h.renderNewForm))
	mux.Handle("GET /forms/{entityType}/{id}", auth(h.renderRecordForm))
	mux.Handle("GET /import/{entityType}", auth(h.importUploadPage))
	mux.Handle("POST /import/{entityType}/preview", auth(h.importPreview))
	mux.Handle("POST /import/{entityType}/commit", auth(h.importCommit))
}

// requestContext fetches the httpx.RequestContext a preceding DevAuth (or
// its eventual Zitadel/OIDC replacement) attached to the request, and
// refuses the request if one isn't present — a handler reachable without
// ever going through auth middleware (e.g. registered directly on a mux
// without the httpx.DevAuth wrapper, a mistake a future change could
// make) must not silently proceed with a zero-value TenantID, it must
// refuse. Every handler below calls this first, not
// httpx.FromContext directly.
func requestContext(w http.ResponseWriter, r *http.Request) (httpx.RequestContext, bool) {
	rc, ok := httpx.FromContext(r.Context())
	if !ok {
		log.Printf("api: no RequestContext on %s %s — handler reachable without auth middleware?", r.Method, r.URL.Path)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return httpx.RequestContext{}, false
	}
	return rc, true
}

// writeInternalError logs the real error server-side (with enough
// context to find it in logs) and returns only a generic message to the
// client — an internal/DB error's text can contain SQLSTATE codes, table
// or column names, or query fragments, none of which belong in an HTTP
// response.
func writeInternalError(w http.ResponseWriter, logContext string, err error) {
	log.Printf("api: %s: %v", logContext, err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal error")
}

// idPattern matches the shape records.id/tenants.id actually are
// (Postgres gen_random_uuid()). Rejecting a malformed id here means a
// client typo becomes a clean 400 before ever reaching a query, instead
// of a Postgres "invalid input syntax for type uuid" driver error
// surfacing as a 500 with raw SQLSTATE text in the response.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isValidID(s string) bool { return idPattern.MatchString(s) }

// entityDef looks up entityType's published Definition for the
// requesting tenant. Every handler below calls this first — a request
// for an entity type with no published Definition 404s here, before
// touching crud.Engine or formrender at all.
func (h *Handler) entityDef(ctx context.Context, tenantID, entityType string) (*entity.Definition, error) {
	v, err := h.entityDefs.GetPublished(ctx, tenantID, entityType)
	if err != nil {
		return nil, err
	}
	return entity.Unmarshal(v.Definition)
}

func (h *Handler) formDef(ctx context.Context, tenantID, entityType string) (*form.Definition, error) {
	v, err := h.formDefs.GetPublished(ctx, tenantID, entityType)
	if err != nil {
		return nil, err
	}
	return form.Unmarshal(v.Definition)
}

func (h *Handler) listRecords(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	records, err := h.crud.List(r.Context(), def, rc.TenantID)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("list %s records", entityType), err)
		return
	}
	out := make([]recordResponse, len(records))
	for i, rec := range records {
		out[i] = toRecordResponse(rec)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) getRecord(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	rec, err := h.crud.Get(r.Context(), def, rc.TenantID, id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		writeInternalError(w, fmt.Sprintf("get %s %s", entityType, id), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRecordResponse(rec))
}

// createRecord and updateRecord both content-negotiate two ways:
//
//   - Request body: formrender's <form> submits via a real browser as
//     application/x-www-form-urlencoded (or multipart/form-data once a
//     form has a file field) — plain JSON only when a caller sets that
//     Content-Type explicitly (every existing test does, and every JSON
//     API client should keep working exactly as before; see
//     parseRecordFields). Found via internal/e2e's real-browser testing:
//     the JSON-only decoder here used to reject every real htmx form
//     submission outright with "invalid JSON body", before the request
//     even reached validation.
//   - Response body: an htmx request (HX-Request: true, set automatically
//     by htmx on every request it issues) gets back the re-rendered form
//     fragment as HTML, matching formrender's own hx-target="this"
//     hx-swap="outerHTML" contract on the <form> tag — the JSON envelope
//     every non-htmx caller (the API client tests, curl, a future real
//     API consumer) still gets is not HTML a browser can swap in.
func (h *Handler) createRecord(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")

	entDef, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	fields, err := parseRecordFields(r, entDef)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validated explicitly here, ahead of crud.Create (which validates
	// again internally — cheap, no DB round trip, and Create doesn't
	// expose a way to distinguish "your input was invalid" from "the
	// database failed" other than by pre-checking the same thing this
	// handler needs the answer to before it's committed to a status
	// code): a validation failure is unambiguously the caller's fault
	// (400, safe to describe exactly what's wrong), so anything Create
	// itself still fails on past this point is a genuine internal/DB
	// error (500, generic message, logged).
	if err := entity.ValidateRecord(entDef, fields); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	rec, err := h.crud.Create(r.Context(), entDef, rc.TenantID, fields, rc.Actor)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("create %s record", entityType), err)
		return
	}

	if isHTMXRequest(r) {
		h.writeRecordFormFragment(w, r, rc.TenantID, entDef, entityType, rec.ID)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toRecordResponse(rec))
}

func (h *Handler) updateRecord(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	entDef, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	fields, err := parseRecordFields(r, entDef)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Same reasoning as createRecord: validated explicitly first so a
	// bad update is unambiguously a 400, not indistinguishable from a
	// genuine 500.
	if err := entity.ValidateRecord(entDef, fields); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = h.crud.Update(r.Context(), entDef, rc.TenantID, id, fields, rc.Actor)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		writeInternalError(w, fmt.Sprintf("update %s %s", entityType, id), err)
		return
	}

	if isHTMXRequest(r) {
		h.writeRecordFormFragment(w, r, rc.TenantID, entDef, entityType, id)
		return
	}
	rec, err := h.crud.Get(r.Context(), entDef, rc.TenantID, id)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("get %s %s after update", entityType, id), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRecordResponse(rec))
}

// isHTMXRequest reports whether r was issued by htmx itself (set
// automatically on every request htmx makes — see
// https://htmx.org/reference/#request_headers) rather than a plain API
// client. Deciding the response shape (HTML fragment vs. JSON envelope)
// on this header, not on Accept or a query param, matches exactly what
// actually distinguishes "formrender's own form just submitted" from
// "some other caller hit this same URL".
func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// parseRecordFields reads entDef's fields out of r's body, dispatching on
// Content-Type: a form-encoded body (what a real browser's htmx-driven
// <form> submission actually sends — see this file's doc comment on
// createRecord/updateRecord) is decoded field-by-field via
// csvimport.Coerce, the same raw-string-to-typed-value conversion CSV
// import already uses (identical problem: a form field, like a CSV cell,
// is never anything but text). A missing Content-Type, or
// application/json, is decoded as a plain JSON body — the default that
// preserves every existing API-client test unchanged, none of which set
// Content-Type explicitly today.
func parseRecordFields(r *http.Request, entDef *entity.Definition) (map[string]any, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data") {
		// ParseMultipartForm calls ParseForm first regardless of content
		// type, so r.PostForm ends up populated either way; the
		// ErrNotMultipart it returns for a plain urlencoded body is
		// expected and safely ignored.
		if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			return nil, fmt.Errorf("parse form: %w", err)
		}
		fields := make(map[string]any, len(entDef.Fields))
		for _, f := range entDef.Fields {
			vals := r.PostForm[f.Name]
			if len(vals) == 0 {
				// Absent entirely, not just empty: formrender always
				// submits every entDef field now (either a visible
				// input, or one of its own hidden fallbacks — see
				// formrender.buildHiddenFields), so a field genuinely
				// missing from the submission means a non-formrender
				// caller (or a hand-built request) chose not to send it,
				// same "absent means don't touch it" reading a JSON
				// caller already gets by omitting a key.
				continue
			}
			// The LAST value wins, not the first: a FieldBool renders as
			// <input type=hidden value=false><input type=checkbox
			// value=true> in that order, so an unchecked box submits
			// only "false" but a checked one submits "false" then
			// "true" — the browser preserves DOM order in the request
			// body, and the checkbox's real state is whichever value
			// came last.
			raw := vals[len(vals)-1]
			if raw == "" {
				// Empty means "explicitly present, but blank" for a
				// formrender-submitted field (a real value cleared to
				// nothing) — treated as absent (not stored as an empty
				// string) matching csvimport.buildRowData's identical
				// convention for a blank CSV cell.
				continue
			}
			v, err := csvimport.Coerce(f.Type, raw)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f.Name, err)
			}
			fields[f.Name] = v
		}
		return fields, nil
	}
	var fields map[string]any
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return fields, nil
}

// writeRecordFormFragment re-renders entityType/id's form (bare fragment,
// no page shell — this is an htmx-swap response, not a page navigation;
// wrapping it in layout.go's full <html> document would break the swap
// the same way wrapping importPreview's response would) and writes it to
// w. Called after a successful create/update when isHTMXRequest(r).
func (h *Handler) writeRecordFormFragment(w http.ResponseWriter, r *http.Request, tenantID string, entDef *entity.Definition, entityType, id string) {
	formDef, err := h.formDef(r.Context(), tenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	renderData, err := h.buildFormRenderData(r.Context(), tenantID, entDef, formDef, id)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("build %s form render data (id=%q)", entityType, id), err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.Render(w, formDef, entDef, renderData, localeFromRequest(r)); err != nil {
		log.Printf("api: render %s form fragment (id=%q): %v", entityType, id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
	}
}

// recordResponse is the wire shape for a data.Record — data.Record has no
// json tags (internal/data isn't coupled to any particular wire format;
// see internal/data/definitions.go's own doc comment on staying generic),
// so this package owns the snake_case mapping CLAUDE.md's API-format
// rule requires. TenantID is deliberately omitted: the caller already
// knows their own tenant, and never echoing it back means a
// cross-tenant bug here can't leak which tenant a record actually
// belongs to.
type recordResponse struct {
	ID         string         `json:"id"`
	EntityType string         `json:"entity_type"`
	Data       map[string]any `json:"data"`
}

func toRecordResponse(r data.Record) recordResponse {
	return recordResponse{ID: r.ID, EntityType: r.EntityType, Data: r.Data}
}

// renderNewForm renders def/entityType's form for a not-yet-saved
// record — empty Record, no RecordID.
func (h *Handler) renderNewForm(w http.ResponseWriter, r *http.Request) {
	h.renderForm(w, r, "")
}

func (h *Handler) renderRecordForm(w http.ResponseWriter, r *http.Request) {
	h.renderForm(w, r, r.PathValue("id"))
}

// renderForm is shared by the "new" and "existing record" routes; id =="" means new.
//
// master_detail sections are populated below via loadMasterDetailChildren
// (RecordRepo.ListByField, added once a real caller — PurchaseOrder's
// Lines section — needed it; formrender itself already supported
// Data.Children, this handler just didn't fetch anything to put there
// before). related_list sections still render empty: unlike
// master_detail, the template already lazy-loads a related_list's rows
// itself via a separate hx-trigger="load" request to
// /api/records/{Target}?ref=..., but nothing serves that ref-filtered
// query yet (no form.Section field says which field on Target points
// back to this record for an arbitrary related-list, the way
// entity.Relationship.ParentField does for a composition/master-detail
// child) — still a real gap, just not one any Definition in this kernel
// exercises yet (QUEUE.md).
func (h *Handler) renderForm(w http.ResponseWriter, r *http.Request, id string) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")
	locale := r.URL.Query().Get("lang")
	if locale == "" {
		locale = "en"
	}
	if id != "" && !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	entDef, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	formDef, err := h.formDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	renderData, err := h.buildFormRenderData(r.Context(), rc.TenantID, entDef, formDef, id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		writeInternalError(w, fmt.Sprintf("build %s form render data (id=%q)", entityType, id), err)
		return
	}

	// Rendered into a buffer first, not straight to w: this is a
	// top-level page navigation (GET /forms/{entityType}/new|{id}), not
	// an htmx-swap response, so it needs the real <html><head> shell
	// that actually loads htmx.js (see layout.go's doc comment) — a
	// browser navigating here directly gets nothing but inert markup
	// otherwise, exactly the gap internal/e2e's first real-browser test
	// exists to catch.
	var buf bytes.Buffer
	if err := h.renderer.Render(&buf, formDef, entDef, renderData, locale); err != nil {
		// Rendering only fails on a schema-drift/malformed-expression bug
		// in the Definitions themselves (formrender's own "fail loud"
		// contract), never on attacker-controlled record data.
		log.Printf("api: render %s form (id=%q): %v", entityType, id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := renderShell(w, buf.String()); err != nil {
		log.Printf("api: render %s form shell (id=%q): %v", entityType, id, err)
	}
}

// buildFormRenderData assembles the formrender.Data for entityType/id —
// id == "" means a not-yet-saved record (empty Record, no RecordID,
// obviously no children). Shared by renderForm (a page navigation) and
// writeRecordFormFragment (an htmx-swap response after create/update) so
// both build the exact same data shape the same way, rather than two
// copies that could silently drift (e.g. one remembering to populate
// master-detail children, the other not).
func (h *Handler) buildFormRenderData(ctx context.Context, tenantID string, entDef *entity.Definition, formDef *form.Definition, id string) (formrender.Data, error) {
	renderData := formrender.Data{}
	if id == "" {
		return renderData, nil
	}
	rec, err := h.crud.Get(ctx, entDef, tenantID, id)
	if err != nil {
		return formrender.Data{}, err
	}
	renderData.RecordID = rec.ID
	renderData.Record = rec.Data

	children, err := h.loadMasterDetailChildren(ctx, tenantID, entDef, formDef, id)
	if err != nil {
		return formrender.Data{}, fmt.Errorf("load master-detail children: %w", err)
	}
	renderData.Children = children
	return renderData, nil
}

// loadMasterDetailChildren fetches the child rows for every
// ComponentMasterDetail section in formDef, keyed by section.Target — the
// shape formrender.Data.Children expects. For each such section it finds
// entDef's own entity.Relationship naming that Target (ParentField is
// declared on the parent, not the child — see entity.Relationship's doc
// comment) and lists every child record whose ParentField equals the
// current record's id. A section with no matching Relationship is
// skipped (formrender.buildChildRows treats a missing key as "no
// children", the same as an explicitly empty slice) rather than erroring
// — a Definition mismatch here is a data-modeling bug to fix in the
// Definition, not something that should 500 every form render for it.
func (h *Handler) loadMasterDetailChildren(ctx context.Context, tenantID string, entDef *entity.Definition, formDef *form.Definition, recordID string) (map[string][]map[string]any, error) {
	children := make(map[string][]map[string]any)
	for _, section := range formDef.Sections {
		if section.Component != form.ComponentMasterDetail {
			continue
		}
		var rel *entity.Relationship
		for i := range entDef.Relationships {
			if entDef.Relationships[i].Target == section.Target {
				rel = &entDef.Relationships[i]
				break
			}
		}
		if rel == nil || rel.ParentField == "" {
			continue
		}
		childDef, err := h.entityDef(ctx, tenantID, section.Target)
		if err != nil {
			return nil, fmt.Errorf("look up %s definition for master-detail section: %w", section.Target, err)
		}
		records, err := h.crud.ListByField(ctx, childDef, tenantID, rel.ParentField, recordID)
		if err != nil {
			return nil, fmt.Errorf("list %s children: %w", section.Target, err)
		}
		rows := make([]map[string]any, len(records))
		for i, rec := range records {
			rows[i] = rec.Data
		}
		children[section.Target] = rows
	}
	return children, nil
}

func writeDefinitionLookupError(w http.ResponseWriter, entityType string, err error) {
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("no published definition for entity type %q", entityType))
		return
	}
	writeInternalError(w, fmt.Sprintf("look up definition for %s", entityType), err)
}
