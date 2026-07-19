// Package api is the first real HTTP surface for a Definition-driven
// entity: it looks Definitions up from the registry (internal/data),
// drives crud.Engine and formrender.Renderer with them, and shapes the
// result through internal/httpx. Like every generic engine in this
// kernel, it must never branch on a specific entity type — behaviour
// comes only from the Definition the registry hands back (CLAUDE.md).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
)

// Handler wires the registry, crud.Engine, and formrender.Renderer
// together behind HTTP. One Handler serves every entity/form type.
type Handler struct {
	entityDefs *data.EntityDefinitionRepo
	formDefs   *data.FormDefinitionRepo
	crud       *crud.Engine
	renderer   *formrender.Renderer
	catalog    *i18n.Catalog
}

// New builds a Handler. catalog is the i18n.Catalog forms (and the
// import wizard, import.go) render against (internal/i18n.Load).
func New(db *sql.DB, catalog *i18n.Catalog) *Handler {
	return &Handler{
		entityDefs: data.NewEntityDefinitionRepo(db),
		formDefs:   data.NewFormDefinitionRepo(db),
		crud:       crud.NewEngine(db),
		renderer:   formrender.New(catalog),
		catalog:    catalog,
	}
}

// Routes registers every handler onto mux, wrapped in httpx.DevAuth (the
// insecure stopgap — see that package's doc comment; main.go always
// registers Routes, relying on DevAuth itself to fail closed).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/records/{entityType}", httpx.DevAuth(http.HandlerFunc(h.listRecords)))
	mux.Handle("POST /api/records/{entityType}", httpx.DevAuth(http.HandlerFunc(h.createRecord)))
	mux.Handle("GET /api/records/{entityType}/{id}", httpx.DevAuth(http.HandlerFunc(h.getRecord)))
	mux.Handle("GET /forms/{entityType}/new", httpx.DevAuth(http.HandlerFunc(h.renderNewForm)))
	mux.Handle("GET /forms/{entityType}/{id}", httpx.DevAuth(http.HandlerFunc(h.renderRecordForm)))
	mux.Handle("GET /import/{entityType}", httpx.DevAuth(http.HandlerFunc(h.importUploadPage)))
	mux.Handle("POST /import/{entityType}/preview", httpx.DevAuth(http.HandlerFunc(h.importPreview)))
	mux.Handle("POST /import/{entityType}/commit", httpx.DevAuth(http.HandlerFunc(h.importCommit)))
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

func (h *Handler) createRecord(w http.ResponseWriter, r *http.Request) {
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

	var fields map[string]any
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
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
	if err := entity.ValidateRecord(def, fields); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	rec, err := h.crud.Create(r.Context(), def, rc.TenantID, fields, rc.Actor)
	if err != nil {
		writeInternalError(w, fmt.Sprintf("create %s record", entityType), err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toRecordResponse(rec))
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
// Known scope limitation for this first HTTP increment: master_detail/
// related_list sections render empty (formrender.Data.Children is never
// populated here) — RecordRepo has no "list records where field X ==
// this id" query yet, only List-by-entity-type. Revisit once a real
// caller needs master-detail forms to actually show their child rows
// over HTTP (formrender itself already supports it; this handler
// doesn't wire it yet).
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

	renderData := formrender.Data{}
	if id != "" {
		rec, err := h.crud.Get(r.Context(), entDef, rc.TenantID, id)
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
			return
		}
		if err != nil {
			writeInternalError(w, fmt.Sprintf("get %s %s for form render", entityType, id), err)
			return
		}
		renderData.RecordID = rec.ID
		renderData.Record = rec.Data
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.Render(w, formDef, entDef, renderData, locale); err != nil {
		// Rendering only fails on a schema-drift/malformed-expression bug
		// in the Definitions themselves (formrender's own "fail loud"
		// contract), never on attacker-controlled record data — but by
		// the time headers are already written for a streaming response
		// this can't cleanly become a different status code either way,
		// so it's logged server-side with the real detail and the client
		// just sees a generic failure, same as every other 500 here.
		log.Printf("api: render %s form (id=%q): %v", entityType, id, err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeDefinitionLookupError(w http.ResponseWriter, entityType string, err error) {
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("no published definition for entity type %q", entityType))
		return
	}
	writeInternalError(w, fmt.Sprintf("look up definition for %s", entityType), err)
}
