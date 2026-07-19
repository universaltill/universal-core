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
	"net/http"

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
}

// New builds a Handler. catalog is the i18n.Catalog forms render against
// (internal/i18n.Load).
func New(db *sql.DB, catalog *i18n.Catalog) *Handler {
	return &Handler{
		entityDefs: data.NewEntityDefinitionRepo(db),
		formDefs:   data.NewFormDefinitionRepo(db),
		crud:       crud.NewEngine(db),
		renderer:   formrender.New(catalog),
	}
}

// Routes registers every handler onto mux, wrapped in httpx.DevAuth (the
// insecure stopgap — see that package's doc comment; main.go only calls
// Routes when httpx.DevAuthEnabled()).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/records/{entityType}", httpx.DevAuth(http.HandlerFunc(h.listRecords)))
	mux.Handle("POST /api/records/{entityType}", httpx.DevAuth(http.HandlerFunc(h.createRecord)))
	mux.Handle("GET /api/records/{entityType}/{id}", httpx.DevAuth(http.HandlerFunc(h.getRecord)))
	mux.Handle("GET /forms/{entityType}/new", httpx.DevAuth(http.HandlerFunc(h.renderNewForm)))
	mux.Handle("GET /forms/{entityType}/{id}", httpx.DevAuth(http.HandlerFunc(h.renderRecordForm)))
}

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
	rc, _ := httpx.FromContext(r.Context())
	entityType := r.PathValue("entityType")

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	records, err := h.crud.List(r.Context(), def, rc.TenantID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "list records: "+err.Error())
		return
	}
	out := make([]recordResponse, len(records))
	for i, rec := range records {
		out[i] = toRecordResponse(rec)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) getRecord(w http.ResponseWriter, r *http.Request) {
	rc, _ := httpx.FromContext(r.Context())
	entityType := r.PathValue("entityType")
	id := r.PathValue("id")

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
		httpx.WriteError(w, http.StatusInternalServerError, "get record: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRecordResponse(rec))
}

func (h *Handler) createRecord(w http.ResponseWriter, r *http.Request) {
	rc, _ := httpx.FromContext(r.Context())
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

	rec, err := h.crud.Create(r.Context(), def, rc.TenantID, fields, rc.Actor)
	if err != nil {
		// entity.ValidateRecord failures and DB errors both land here;
		// crud.Engine wraps validation failures with "validation
		// failed: " so this is at least distinguishable in the message
		// without the handler needing to know entity's error types.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
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
	rc, _ := httpx.FromContext(r.Context())
	entityType := r.PathValue("entityType")
	locale := r.URL.Query().Get("lang")
	if locale == "" {
		locale = "en"
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
			httpx.WriteError(w, http.StatusInternalServerError, "get record: "+err.Error())
			return
		}
		renderData.RecordID = rec.ID
		renderData.Record = rec.Data
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.Render(w, formDef, entDef, renderData, locale); err != nil {
		// Rendering only fails on a schema-drift/malformed-expression
		// bug in the Definitions themselves (formrender's own "fail
		// loud" contract) — by the time headers are already written for
		// a streaming response this can't cleanly become a JSON error,
		// so it's logged server-side; there is no good client-facing
		// recovery from a half-written HTML page.
		httpx.WriteError(w, http.StatusInternalServerError, "render form: "+err.Error())
	}
}

func writeDefinitionLookupError(w http.ResponseWriter, entityType string, err error) {
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("no published definition for entity type %q", entityType))
		return
	}
	httpx.WriteError(w, http.StatusInternalServerError, "look up definition: "+err.Error())
}
