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
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/formrender"
	"github.com/universaltill/universal-core/internal/kernel/secretcrypt"
	"github.com/universaltill/universal-core/internal/kernel/speechassist"
	"github.com/universaltill/universal-core/internal/kernel/workflow"
	"github.com/universaltill/universal-core/internal/svcauth"
	"github.com/universaltill/universal-core/internal/tenantdb"
	"github.com/universaltill/universal-core/internal/webauth"
	"github.com/universaltill/universal-core/internal/zitadelmgmt"
)

// Handler wires the registry, crud.Engine, and formrender.Renderer
// together behind HTTP. One Handler serves every tenant/entity/form type
// — router resolves each request's own tenant database (ADR-0003) via
// scope, rather than this Handler holding a single shared *sql.DB.
type Handler struct {
	router   *tenantdb.Router
	renderer *formrender.Renderer
	catalog  *i18n.Catalog
	auth     *webauth.Authenticator
	// svc is nil (Enabled() == false) unless machine-to-machine API auth
	// is configured (SVC_INTROSPECTION_CLIENT_ID/SECRET) — see
	// internal/svcauth's own doc comment. A connector's Bearer token is
	// checked ahead of auth (webauth) in Routes, not behind it: see
	// svcauth.Authenticator.Guard's own doc comment on why.
	svc *svcauth.Authenticator
	// ai is nil (Enabled() == false) unless OLLAMA_URL is configured —
	// see aiassist's own doc comment on why every caller can treat that
	// as "AI assistance unavailable" without a separate nil check.
	ai *aiassist.Client
	// speech is nil (Enabled() == false) unless WHISPER_URL is
	// configured — same nil-safe contract as ai, see speechassist's own
	// doc comment.
	speech *speechassist.Client
	// secretCryptor is nil (Enabled() == false) unless
	// SECRET_ENCRYPTION_KEY is configured — the AI-provider settings page
	// (aiprovidersettings.go) refuses to store a tenant's own API key at
	// all while this is disabled, rather than ever writing one to the
	// database unencrypted (see secretcrypt's own doc comment).
	secretCryptor *secretcrypt.Cryptor
	// hooks is applied to every freshly-built crud.Engine in scope()
	// (nil/empty is the common case — most tenants/entity types register
	// none). Populated via RegisterHook by the real composition root
	// (cmd/universal-core's main, the only caller so far — never by this
	// package itself), which is what keeps this file from needing to
	// import a specific kernel module (purchasing/sales/...) just to
	// wire a ledger-posting hook: this package's own doc comment says it
	// must stay entity-agnostic, and importing concrete modules by name
	// here would quietly break that even though the *dispatch* inside
	// crud.Engine itself stays generic either way. Independent review
	// caught this coupling in an earlier draft that called SetHook
	// directly with purchasing/sales imports in this file.
	hooks []hookRegistration
	// members is nil (Enabled() == false) unless the Zitadel
	// member-management credential is configured (ZITADEL_MGMT_PAT etc.,
	// ADR-0010) — the /settings/members page renders its unavailable
	// state rather than erroring, same nil-safe contract as ai/speech.
	// tenants (the control-plane TenantRepo) resolves the current
	// tenant's linked Zitadel org for it; both are set together by the
	// composition root via SetMemberMgmt.
	members *zitadelmgmt.Client
	tenants *data.TenantRepo
}

// SetMemberMgmt wires the member-management client and the tenant
// registry lookup it scopes by — a post-construction setter for the
// same reason RegisterHook is one (New already has seven parameters,
// and this is optional wiring done exactly once at startup).
func (h *Handler) SetMemberMgmt(client *zitadelmgmt.Client, tenants *data.TenantRepo) {
	h.members = client
	h.tenants = tenants
}

type hookRegistration struct {
	entityType string
	hook       crud.Hook
}

// RegisterHook queues hook to be applied (via crud.Engine.SetHook) to
// every tenantScope's Engine going forward — called once per hook by the
// real composition root after New, before Routes/ListenAndServe. Not a
// constructor parameter: New already has seven, and every hook this
// Handler will ever need is registered exactly once at startup, not
// per-request, so a post-construction setter (the same shape
// crud.Engine.SetHook itself already uses) is the simpler fit.
func (h *Handler) RegisterHook(entityType string, hook crud.Hook) {
	h.hooks = append(h.hooks, hookRegistration{entityType, hook})
}

// New builds a Handler. catalog is the i18n.Catalog forms (and the
// import wizard, import.go) render against (internal/i18n.Load). auth
// and svc may each be nil or disabled — Routes wires both
// unconditionally either way, since Guard/Register are safe no-ops on
// a disabled Authenticator (see webauth's and svcauth's own doc
// comments). ai/speech may be nil — every caller of either (the import
// wizard's mapping suggestion; the issue logger's voice transcription)
// treats a disabled client as "AI assistance unavailable," never an
// error. secretCryptor may also be nil — see the Handler field's own
// doc comment.
func New(router *tenantdb.Router, catalog *i18n.Catalog, auth *webauth.Authenticator, svc *svcauth.Authenticator, ai *aiassist.Client, speech *speechassist.Client, secretCryptor *secretcrypt.Cryptor) *Handler {
	return &Handler{
		router:        router,
		renderer:      formrender.New(catalog),
		catalog:       catalog,
		auth:          auth,
		svc:           svc,
		ai:            ai,
		speech:        speech,
		secretCryptor: secretCryptor,
	}
}

// tenantScope bundles every per-tenant repo/engine a request handler
// needs, resolved once per request against that tenant's own database
// (ADR-0003) — the replacement for Handler holding these long-lived
// against one shared *sql.DB. Cheap to construct (each wraps the same
// already-open, router-cached *sql.DB pointer; no I/O here beyond
// router.Get's own cache lookup), so building one per request is not a
// performance concern.
type tenantScope struct {
	db           *sql.DB
	entityDefs   *data.EntityDefinitionRepo
	formDefs     *data.FormDefinitionRepo
	workflowDefs *data.WorkflowDefinitionRepo
	// crud is the RBAC-guarded engine (ADR-0006): every handler CRUD
	// call goes through authz.GuardedEngine's read/write checks
	// structurally, so no individual handler carries (or can forget) a
	// permission check. System paths that must not be subject to a
	// user's permissions (workflow steps, seeding, provisioning) don't
	// run through tenantScope at all — they build their own raw
	// crud.Engine.
	crud          *authz.GuardedEngine
	workflowQueue *workflow.Queue
	reporting     *data.ReportingRepo
}

// scope resolves rc's tenant database via h.router and builds a
// tenantScope against it. Every request handler calls this immediately
// after requestContext. It takes the full RequestContext, not just the
// tenant id, because the CRUD engine it hands back is guarded per-actor
// (ADR-0006) — who is asking is part of the scope now.
func (h *Handler) scope(ctx context.Context, rc httpx.RequestContext) (tenantScope, error) {
	db, err := h.router.Get(ctx, rc.TenantID)
	if err != nil {
		return tenantScope{}, fmt.Errorf("resolve tenant database: %w", err)
	}
	// nil handlers: same default no-op notify handler internal/worker's
	// Runner gets from workflow.NewQueue — this Handler only ever calls
	// Enqueue/ResumeAfterApproval, never ProcessOne, so no StepHandler of
	// its own is needed here regardless.
	workflowQueue, err := workflow.NewQueue(db, nil)
	if err != nil {
		// Only returns an error for a caller-supplied require_approval
		// handler, which nil (no handlers at all) can never trigger —
		// unreachable in practice, but fail loud rather than silently
		// leaving workflowQueue nil for something later to panic on.
		return tenantScope{}, fmt.Errorf("build workflow queue: %w", err)
	}
	engine := crud.NewEngine(db)
	// Apply whatever the real composition root registered via
	// RegisterHook (Handler's own doc comment on the hooks field) — this
	// loop has zero knowledge of what any registered entityType/hook
	// actually is, same generic-dispatch shape crud.Engine.SetHook
	// itself already is.
	for _, hr := range h.hooks {
		engine.SetHook(hr.entityType, hr.hook)
	}
	guarded := authz.Guard(engine, authz.NewResolver(db, rc.Actor, rc.Machine))

	return tenantScope{
		db:            db,
		entityDefs:    data.NewEntityDefinitionRepo(db),
		formDefs:      data.NewFormDefinitionRepo(db),
		workflowDefs:  data.NewWorkflowDefinitionRepo(db),
		crud:          guarded,
		workflowQueue: workflowQueue,
		reporting:     data.NewReportingRepo(db),
	}, nil
}

// entityDef looks up entityType's published Definition. Every handler
// calls this first — a request for an entity type with no published
// Definition 404s here, before touching crud.Engine or formrender at all.
func (ts tenantScope) entityDef(ctx context.Context, entityType string) (*entity.Definition, error) {
	v, err := ts.entityDefs.GetPublished(ctx, entityType)
	if err != nil {
		return nil, err
	}
	return entity.Unmarshal(v.Definition)
}

func (ts tenantScope) formDef(ctx context.Context, entityType string) (*form.Definition, error) {
	v, err := ts.formDefs.GetPublished(ctx, entityType)
	if err != nil {
		return nil, err
	}
	return form.Unmarshal(v.Definition)
}

// Routes registers every handler onto mux, wrapped in
// h.svc.Guard(h.auth.Guard(httpx.DevAuth(...))) — a connector's Bearer
// access token (svcauth) is checked FIRST, ahead of real browser login
// (webauth), ahead of DevAuth (the insecure stopgap — see that
// package's doc comment): a Bearer-carrying request is unambiguously an
// API client and must get a clean 401 JSON body on failure, never
// webauth.Guard's own browser-oriented redirect to /ui/login (see
// svcauth.Authenticator.Guard's own doc comment). Whichever of the
// three actually authenticates a given request, all three populate the
// exact same httpx.RequestContext shape — internal/api's handlers never
// need to know which one ran (main.go always registers Routes, relying
// on DevAuth's own fail-closed default when none of the three are
// configured).
func (h *Handler) Routes(mux *http.ServeMux) {
	// Unauthenticated: a static asset with no tenant-specific content —
	// gating it behind auth would only break the page that needs it
	// (a 401/redirect for the very script tag meant to make that page
	// itself interactive) before auth can even run.
	mux.HandleFunc("GET /static/htmx.min.js", serveHTMX)
	// Content-hashed path (see layout.go's appCSSPath) — not the plain
	// "/static/app.css" a stale bookmark/cache might still request, since
	// serving *this app's* CSS at that fixed URL is exactly the bug this
	// fixed. shellTmpl only ever links to the hashed path.
	mux.HandleFunc("GET "+appCSSPath, serveCSS)
	// webauth's own /ui/login, /ui/auth/callback, /ui/logout — never
	// wrapped in Guard themselves; that's how a request gets a session
	// in the first place. No-op registration when webauth is disabled.
	h.auth.Register(mux)

	// guardTenantHeader runs innermost on every authed route: a page's
	// X-UC-Tenant stamp that disagrees with the session refuses rather
	// than mutating the wrong tenant (tenanturl.go, ADR-0011). authPage
	// additionally canonicalizes an authenticated browser's un-prefixed
	// page GET to its /t/{slug}/… form — page routes only, never API/
	// HTMX endpoints (their un-prefixed contract is load-bearing for
	// connectors and partials).
	auth := func(handler http.HandlerFunc) http.Handler {
		return h.svc.Guard(h.auth.Guard(httpx.DevAuth(h.guardTenantHeader(handler))))
	}
	authPage := func(handler http.HandlerFunc) http.Handler {
		return h.svc.Guard(h.auth.Guard(httpx.DevAuth(h.guardTenantHeader(h.canonicalPage(handler)))))
	}
	// "/{$}" — the Go 1.22+ ServeMux exact-match wildcard — not plain
	// "/", which would act as a catch-all subtree match and silently
	// swallow every unmatched path into the dashboard instead of a real
	// 404. Deliberately NOT wrapped in auth(): a hard 401 here (the JSON
	// error body auth() produces) is right for an API route but wrong for
	// the page a browser actually lands on — renderRoot does its own
	// optional session check and renders a real welcome page for a
	// visitor with no session at all, including on a deployment where no
	// auth backend is configured yet.
	mux.HandleFunc("GET /{$}", h.renderRoot)
	mux.Handle("GET /api/records/{entityType}", auth(h.listRecords))
	mux.Handle("POST /api/records/{entityType}", auth(h.createRecord))
	mux.Handle("GET /api/records/{entityType}/{id}", auth(h.getRecord))
	// Searchable reference-field picker source (#24) — see reference_search.go.
	mux.Handle("GET /api/references/{entityType}", auth(h.searchReferenceOptions))
	// POST, not PUT: formrender's own <form> tag always submits via
	// hx-post regardless of new vs. existing record (see render.go's
	// tmplSrc) — until this route existed at all, saving an existing
	// record's form 404'd outright (found via internal/e2e's real-browser
	// testing, not curl — no existing test ever exercised editing a
	// record that already existed).
	mux.Handle("POST /api/records/{entityType}/{id}", auth(h.updateRecord))
	mux.Handle("GET /forms/{entityType}/new", authPage(h.renderNewForm))
	mux.Handle("GET /forms/{entityType}/{id}", authPage(h.renderRecordForm))
	// The module's actual landing page — a table of existing records,
	// not just New/Import links (see listview.go's doc comment: found
	// missing the first time a real login actually reached the
	// dashboard, since New/Import alone give nowhere to go look at data
	// that already exists).
	mux.Handle("GET /records/{entityType}", authPage(h.renderRecordList))
	// A module's searchable menu of its own entity types — the page
	// each dashboard hub node/nav link actually lands on (see
	// modulemenu.go's doc comment).
	mux.Handle("GET /modules/{key}", authPage(h.renderModuleMenu))
	mux.Handle("GET /import/{entityType}", authPage(h.importUploadPage))
	mux.Handle("POST /import/{entityType}/preview", auth(h.importPreview))
	mux.Handle("POST /import/{entityType}/commit", auth(h.importCommit))
	// Import from a registered external SQL source (ADR-0019) — see
	// extsqlimport.go. Same gating as the file-based import routes above:
	// the page behind authPage, the htmx fragments behind auth, and the
	// commit writing through the same RBAC-guarded engine (ADR-0006).
	// The literal "sql" segment outranks the file flow's wildcard-free
	// patterns by segment count, so there's no collision.
	mux.Handle("GET /import/{entityType}/sql", authPage(h.extSQLImportPage))
	mux.Handle("POST /import/{entityType}/sql/relations", auth(h.extSQLImportRelations))
	mux.Handle("POST /import/{entityType}/sql/preview", auth(h.extSQLImportPreview))
	mux.Handle("POST /import/{entityType}/sql/commit", auth(h.extSQLImportCommit))
	// The exporter half of Farshid's original "importer exporter
	// plugins" ask — see export.go's own doc comment.
	mux.Handle("GET /export/{entityType}", auth(h.exportRecordsCSV))
	// SAF-T statutory export (saftexport.go). The literal "saft"
	// segment outranks the {entityType} wildcard above under the mux's
	// most-specific-pattern-wins rule, so no CSV export for an entity
	// type that happens to be named "saft" — an acceptable collision:
	// PascalCase entity type names can never be the lowercase "saft".
	mux.Handle("GET /export/saft", auth(h.exportSAFT))
	mux.Handle("GET /export/saft/form", authPage(h.saftExportPage))
	// UBL 2.1 single-document export (ublexport.go). Three path
	// segments, so no overlap with the two CSV/SAF-T patterns above.
	mux.Handle("GET /export/{entityType}/{id}/ubl", auth(h.exportUBL))
	// The in-app issue logger — see issuereport.go's own doc comment.
	// Not entity-scoped: IssueReport is one fixed foundation entity, not
	// a generic-per-entity-type route the way /import is.
	mux.Handle("GET /issue-report/new", authPage(h.issueReportNewPage))
	mux.Handle("POST /issue-report/transcribe", auth(h.issueReportTranscribe))
	mux.Handle("POST /issue-report/submit", auth(h.issueReportSubmit))
	// Resumes a job halted at a require_approval step — see workflow.go's
	// doc comment. Not entity-scoped in the URL: a workflow_jobs row is
	// tenant+id addressed, same as workflow_definitions being keyed by
	// name rather than entity_type.
	mux.Handle("POST /api/workflow-jobs/{id}/approve", auth(h.approveWorkflowJob))
	// The read side of the same loop — see listWorkflowJobs' doc comment.
	mux.Handle("GET /api/workflow-jobs", auth(h.listWorkflowJobs))
	// The human-facing page on top of the two routes above — see
	// renderWorkflowInbox's doc comment.
	mux.Handle("GET /workflow-jobs", authPage(h.renderWorkflowInbox))
	// The mgmt reporting workbench (QUEUE.md's design-partner opportunity
	// entry) — see reporting.go's doc comment.
	mux.Handle("GET /reports/purchasing", authPage(h.renderPurchasingReport))
	// The RFQ vendor-comparison report (#9) — see rfq_report.go's doc
	// comment. {id} is the RequestForQuotation record's own id, not an
	// entity type — this report is scoped to exactly one RFQ, unlike the
	// purchasing report above which aggregates across every record.
	mux.Handle("GET /reports/rfq/{id}", authPage(h.renderRFQComparisonReport))
	// The BYOK AI-provider settings page — see aiprovidersettings.go's own
	// doc comment. Not entity-scoped in the URL, same reasoning as
	// /issue-report/*: AIProviderConnection is one fixed foundation
	// entity a tenant upserts a single row of, not a generic
	// per-entity-type route.
	mux.Handle("GET /settings/ai-provider", authPage(h.aiProviderSettingsPage))
	mux.Handle("POST /settings/ai-provider", auth(h.aiProviderSettingsSave))
	mux.Handle("POST /settings/ai-provider/clear", auth(h.aiProviderSettingsClear))
	// The external SQL sources settings page (ADR-0019) — see
	// extsqlsource.go's own doc comment. Same shape as /settings/
	// ai-provider (a fixed foundation entity's bespoke settings page,
	// never a generic form), except ExternalSQLSource is a list, so the
	// mutating routes are per-record.
	mux.Handle("GET /settings/sql-sources", authPage(h.extSQLSourcesPage))
	mux.Handle("POST /settings/sql-sources", auth(h.extSQLSourceCreate))
	mux.Handle("POST /settings/sql-sources/{id}", auth(h.extSQLSourceUpdate))
	mux.Handle("POST /settings/sql-sources/{id}/delete", auth(h.extSQLSourceDelete))
	mux.Handle("POST /settings/sql-sources/{id}/test", auth(h.extSQLSourceTest))
	// The self-service tenant member management page (universal-core#3,
	// ADR-0010) — see members.go's own doc comment. Every route is
	// additionally gated to the tenant_admin role code server-side
	// (requireMembersAccess), on top of auth()'s session gate.
	mux.Handle("GET /settings/members", authPage(h.membersPage))
	mux.Handle("POST /settings/members/invite", auth(h.membersInvite))
	mux.Handle("POST /settings/members/remove", auth(h.membersRemove))
	mux.Handle("POST /settings/members/password-link", auth(h.membersPasswordLink))
	mux.Handle("POST /settings/members/password-email", auth(h.membersPasswordEmail))
	mux.Handle("POST /settings/members/roles/assign", auth(h.membersAssignRole))
	mux.Handle("POST /settings/members/roles/revoke", auth(h.membersRevokeRole))
	// Path-prefix tenancy (#25, ADR-0011) — the whole /t/{slug}/ subtree
	// resolves + cross-checks the slug, then re-dispatches into this same
	// mux with the prefix stripped (see tenanturl.go). Wrapped in the raw
	// auth chain, NOT auth(): guardTenantHeader and canonicalPage belong
	// to the inner, re-dispatched routes.
	mux.Handle("/t/{slug}/", h.svc.Guard(h.auth.Guard(httpx.DevAuth(h.tenantPrefixed(mux)))))
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

// writeCrudError is writeInternalError for errors coming back from a
// guarded CRUD call (tenantScope.crud): an RBAC denial (ADR-0006) is
// the requester's 403, not a server fault, mapped to a fixed "access
// denied" string (the localized denial surface on rendered pages is the
// field-level enforcement commit's work, alongside menu filtering). A
// rejected self-reference cycle (ADR-0007) is likewise the requester's
// own bad input, not a server fault, so it gets crud.ErrReferenceCycle's
// own text (entity type + field name — the same information the caller
// already submitted in its own request) rather than a fixed string —
// the same "safe to describe exactly what's wrong" reasoning this file's
// other 400-mapped kernel errors (crud.ErrInvalidTransition) already use.
// crud.ErrHookRejected is the same shape for a Hook's own business-rule
// rejection (crud.ErrHookRejected's own doc comment) — this file stays
// entity-agnostic either way, checking one generic kernel sentinel
// rather than importing purchasing/sales to check an entity-specific one.
// crud.ErrTargetConstraintViolation (uc-infra#78) is the same family
// again: a FieldReference value that fails its Definition-declared
// TargetFilter/MustMatchParentField is the caller's own bad input, not
// a server fault, so it too gets its own descriptive text rather than a
// fixed string.
func writeCrudError(w http.ResponseWriter, logContext string, err error) {
	if errors.Is(err, authz.ErrDenied) {
		httpx.WriteError(w, http.StatusForbidden, "access denied")
		return
	}
	if errors.Is(err, crud.ErrReferenceCycle) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, crud.ErrHookRejected) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, crud.ErrTargetConstraintViolation) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeInternalError(w, logContext, err)
}

// writeCrudErrorLocalized is writeCrudError plus the system-of-record
// mappings (uc-infra#102), used on the user-driven mutation paths
// (create/update/delete call sites) where authz's SoR checks can fire.
// A Handler method, unlike writeCrudError, because these two refusals
// MUST render translated: authz.SoRReadOnlyError's Error() string is
// explicitly logs-only (sor.go's own doc comment), and the envelope's
// error string is exactly what the global error toast shows a browser
// user on a failed form save — so the translated text has to live in the
// envelope itself, for the JSON API client and the form-save toast alike.
//
//   - SoRReadOnlyError → 409: the record exists and the request was
//     well-formed; it conflicts with the source's ownership of the data
//     (the same "state disagrees with your request" family as the
//     version-conflict 409 above it in updateRecord). The message names
//     the owning source when the engine could resolve its name, and
//     falls back to a no-source variant otherwise.
//   - ErrSystemOfRecordModeReserved → 400: the submitted value itself is
//     unacceptable today ("bidirectional" is reserved for the sync
//     engine), same family as every other validation 400.
func (h *Handler) writeCrudErrorLocalized(w http.ResponseWriter, r *http.Request, logContext string, err error) {
	if errors.Is(err, authz.ErrSystemOfRecordReadOnly) {
		// Logged server-side: the engine's Error() text (entity type,
		// source id) is exactly the "for logs only" detail sor.go
		// promises, and the translated envelope deliberately carries less.
		log.Printf("api: %s: %v", logContext, err)
		httpx.WriteError(w, http.StatusConflict, h.sorReadOnlyMessage(localeFromRequest(w, r), err))
		return
	}
	if errors.Is(err, authz.ErrSystemOfRecordModeReserved) {
		log.Printf("api: %s: %v", logContext, err)
		locale := localeFromRequest(w, r)
		httpx.WriteError(w, http.StatusBadRequest, h.catalog.T(locale, "sor.mode_reserved"))
		return
	}
	// crud.TargetConstraintError (uc-infra#78) is a routine data-entry
	// mistake a real end user hits, not a rare admin-only action, so it
	// gets the same translated-envelope treatment as the two SoR cases
	// above rather than writeCrudError's untranslated ErrReferenceCycle/
	// ErrHookRejected precedent (independent review: that precedent was
	// the wrong one to follow here — CLAUDE.md's "no hardcoded
	// user-facing strings"). Detail (entity/field/value-naming, possibly
	// already generalized by GuardedEngine.sanitizeTargetConstraintError)
	// is logged in full server-side; only the field name is surfaced to
	// the caller, resolved through the same field-label catalog key
	// (field.{EntityType}.{FieldName}) listview.go's column headers use.
	var tce *crud.TargetConstraintError
	if errors.As(err, &tce) {
		log.Printf("api: %s: %v", logContext, err)
		locale := localeFromRequest(w, r)
		fieldLabel := h.catalog.TOrDefault(locale, "field."+tce.EntityType+"."+tce.FieldName, tce.FieldName)
		msg := strings.ReplaceAll(h.catalog.T(locale, "crud.error.target_constraint_violation"), "{field}", fieldLabel)
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	writeCrudError(w, logContext, err)
}

// writeValidationErrorLocalized translates an entity.ValidateRecord
// failure into the viewer's locale before it reaches a form-save toast
// or the JSON API's error envelope (uc-infra#96) — CLAUDE.md's "no
// hardcoded user-facing strings" applies to *entity.ValidationError
// exactly like it does to crud.TargetConstraintError
// (writeCrudErrorLocalized above): the field name is resolved through
// the same field.{EntityType}.{FieldName} label catalog form rendering
// and the target-constraint mapping already use, substituted into a
// per-Kind entity.validation.* template.
func (h *Handler) writeValidationErrorLocalized(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, http.StatusBadRequest, h.validationErrorMessage(localeFromRequest(w, r), err))
}

// validationErrorMessage is writeValidationErrorLocalized's pure
// message-building half, split out so a caller that already has the
// viewer's locale, or that renders into a page fragment rather than a
// fresh error envelope (extSQLSourceSave's inline form error), can reuse
// the same translation. Falls back to err.Error() (the untranslated,
// English Detail) for any error that isn't an *entity.ValidationError,
// so it's safe to call unconditionally on anything ValidateRecord might
// return.
func (h *Handler) validationErrorMessage(locale string, err error) string {
	var verr *entity.ValidationError
	if !errors.As(err, &verr) {
		return err.Error()
	}
	fieldLabel := h.catalog.TOrDefault(locale, "field."+verr.EntityType+"."+verr.FieldName, verr.FieldName)
	msg := strings.ReplaceAll(h.catalog.T(locale, "entity.validation."+string(verr.Kind)), "{field}", fieldLabel)
	if verr.Kind == entity.KindNotBefore {
		otherLabel := h.catalog.TOrDefault(locale, "field."+verr.EntityType+"."+verr.OtherField, verr.OtherField)
		msg = strings.ReplaceAll(msg, "{other_field}", otherLabel)
	}
	return msg
}

// sorReadOnlyMessage builds the translated system-of-record refusal for
// the viewer's locale, naming the owning source when the engine resolved
// its name — shared by the single-record mapping above and the import
// result table's per-row rendering (extsqlimport.go).
func (h *Handler) sorReadOnlyMessage(locale string, err error) string {
	var sorErr *authz.SoRReadOnlyError
	if errors.As(err, &sorErr) && sorErr.SourceName != "" {
		return strings.ReplaceAll(h.catalog.T(locale, "sor.read_only_blocked"), "{source}", sorErr.SourceName)
	}
	return h.catalog.T(locale, "sor.read_only_blocked_no_source")
}

// idPattern matches the shape records.id/tenants.id actually are
// (Postgres gen_random_uuid()). Rejecting a malformed id here means a
// client typo becomes a clean 400 before ever reaching a query, instead
// of a Postgres "invalid input syntax for type uuid" driver error
// surfacing as a 500 with raw SQLSTATE text in the response.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isValidID(s string) bool { return idPattern.MatchString(s) }

func (h *Handler) listRecords(w http.ResponseWriter, r *http.Request) {
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
		writeCrudError(w, fmt.Sprintf("list %s records", entityType), err)
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
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	entityType := r.PathValue("entityType")
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	def, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	rec, err := ts.crud.Get(r.Context(), def, id)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		writeCrudError(w, fmt.Sprintf("get %s %s", entityType, id), err)
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
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	entityType := r.PathValue("entityType")

	entDef, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	fields, err := parseRecordFields(r, entDef)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Field-level RBAC is applied BEFORE the validation below, not left
	// entirely to crud.Create's own guarded call: a field this user may
	// not see is stripped from the form they were served, so validating
	// their raw submission would report a hidden required field as a
	// missing one (a 400 naming a field they aren't allowed to know
	// exists) instead of letting the write proceed on the stored value.
	// The guarded engine repeats this check on the way through — it is
	// idempotent, and the structural guarantee must not depend on this
	// handler having remembered to call it (see EffectiveWriteFields).
	fields, err = ts.crud.EffectiveWriteFields(r.Context(), entDef, "", fields)
	if err != nil {
		writeCrudError(w, fmt.Sprintf("apply field permissions for new %s", entityType), err)
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
		h.writeValidationErrorLocalized(w, r, err)
		return
	}
	// isCreate=true, id/version both ignored: Create generates the
	// record's id itself, and a create's only requirement is starting in
	// an is_initial status — there's no prior state to race against, so
	// no expectedVersion is needed (see ValidateStatusTransition's doc
	// comment).
	if err := ts.crud.ValidateStatusTransition(r.Context(), entDef, "", fields, true, nil); err != nil {
		if errors.Is(err, crud.ErrInvalidTransition) {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeInternalError(w, fmt.Sprintf("validate status transition for new %s", entityType), err)
		return
	}

	rec, err := ts.crud.Create(r.Context(), entDef, fields, rc.Actor)
	if err != nil {
		// Localized variant: the guarded Create can refuse a reserved
		// SystemOfRecord mode (authz.ErrSystemOfRecordModeReserved).
		h.writeCrudErrorLocalized(w, r, fmt.Sprintf("create %s record", entityType), err)
		return
	}
	h.triggerWorkflows(r.Context(), ts, entityType, rec.ID, workflow.TriggerOnCreate, rc.Actor)

	if isHTMXRequest(r) {
		h.writeRecordFormFragment(w, r, ts, entDef, entityType, rec.ID)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toRecordResponse(rec))
}

func (h *Handler) updateRecord(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	entDef, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	fields, err := parseRecordFields(r, entDef)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Same reasoning as createRecord's own call: resolve what this user
	// is actually allowed to write before validating it. On an update
	// this also RESTORES every hidden field's stored value, without which
	// the record-write path's full-replacement semantics would erase
	// each one on every save (see EffectiveWriteFields).
	fields, err = ts.crud.EffectiveWriteFields(r.Context(), entDef, id, fields)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		writeCrudError(w, fmt.Sprintf("apply field permissions for %s %s", entityType, id), err)
		return
	}

	// Same reasoning as createRecord: validated explicitly first so a
	// bad update is unambiguously a 400, not indistinguishable from a
	// genuine 500.
	if err := entity.ValidateRecord(entDef, fields); err != nil {
		h.writeValidationErrorLocalized(w, r, err)
		return
	}
	// Extracted before ValidateStatusTransition, not after: a real status
	// transition requires expectedVersion to be non-nil (see that
	// method's doc comment on why an unversioned update can't safely
	// validate a transition), so the version has to be known first.
	expectedVersion, err := extractVersion(r, fields)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ts.crud.ValidateStatusTransition(r.Context(), entDef, id, fields, false, expectedVersion); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			// Matches the 404 crud.Update itself would have returned for
			// this id — the status check runs first, so it has to report
			// the same "no such record" outcome, not a generic 500.
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
			return
		}
		if errors.Is(err, crud.ErrInvalidTransition) {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeInternalError(w, fmt.Sprintf("validate status transition for %s %s", entityType, id), err)
		return
	}

	_, err = ts.crud.Update(r.Context(), entDef, id, fields, expectedVersion, rc.Actor)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if errors.Is(err, data.ErrVersionConflict) {
		// 409: the record is real, just not at the version this request
		// was based on — someone else (or the same user, another tab)
		// saved a change since this caller last read it. Not surfaced as
		// a friendlier in-form message yet (QUEUE.md) — closing the
		// actual data-loss gap (a stale save no longer silently wins)
		// takes priority over that polish.
		httpx.WriteError(w, http.StatusConflict, fmt.Sprintf("%s %q was changed by someone else — reload and try again", entityType, id))
		return
	}
	if err != nil {
		// Localized variant: the guarded Update can refuse a hand-edit of
		// a record a read_only system of record owns (409, uc-infra#102)
		// or a reserved SystemOfRecord mode (400) — both translated.
		h.writeCrudErrorLocalized(w, r, fmt.Sprintf("update %s %s", entityType, id), err)
		return
	}
	h.triggerWorkflows(r.Context(), ts, entityType, id, workflow.TriggerOnUpdate, rc.Actor)

	if isHTMXRequest(r) {
		h.writeRecordFormFragment(w, r, ts, entDef, entityType, id)
		return
	}
	rec, err := ts.crud.Get(r.Context(), entDef, id)
	if err != nil {
		writeCrudError(w, fmt.Sprintf("get %s %s after update", entityType, id), err)
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
			if f.Type == entity.FieldI18nText {
				// An i18n_text field (ADR-0009) is submitted as one input per
				// locale named "{field}.{locale}", not a single "{field}"
				// value — reassemble them into the locale->string object the
				// record stores. Only non-empty locales are kept (a cleared
				// locale drops that translation on a full-replacement update);
				// an all-empty field is treated as absent, exactly like a
				// blank plain field below.
				obj := map[string]any{}
				prefix := f.Name + "."
				for key, vals := range r.PostForm {
					if len(vals) == 0 || !strings.HasPrefix(key, prefix) {
						continue
					}
					if val := vals[len(vals)-1]; val != "" {
						obj[key[len(prefix):]] = val
					}
				}
				if len(obj) > 0 {
					fields[f.Name] = obj
				}
				continue
			}
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

// extractVersion pulls the optimistic-locking "_version" value out of an
// update request, reading whichever channel parseRecordFields itself used
// — r.PostForm for a form-encoded submission (formrender's own
// "_version" hidden field, see render.go's viewModel.VersionKnown; never
// collected into fields since it isn't a declared entity field), or the
// fields map itself for a JSON body (parseRecordFields's JSON branch
// decodes the whole body unfiltered, so "_version" would otherwise leak
// into fields and get stored as bogus record data — deleted here either
// way). Returns nil when absent: no check requested, matching this
// endpoint's original unconditional-update behaviour for any caller that
// predates versioning (every existing JSON API client/test included).
func extractVersion(r *http.Request, fields map[string]any) (*int, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data") {
		raw := r.PostForm.Get("_version")
		if raw == "" {
			return nil, nil
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("_version: %w", err)
		}
		return &v, nil
	}
	raw, ok := fields["_version"]
	delete(fields, "_version")
	if !ok {
		return nil, nil
	}
	n, ok := raw.(float64) // encoding/json decodes any JSON number as float64
	if !ok {
		return nil, fmt.Errorf("_version: expected a number")
	}
	v := int(n)
	return &v, nil
}

// writeRecordFormFragment re-renders entityType/id's form (bare fragment,
// no page shell — this is an htmx-swap response, not a page navigation;
// wrapping it in layout.go's full <html> document would break the swap
// the same way wrapping importPreview's response would) and writes it to
// w. Called after a successful create/update when isHTMXRequest(r).
func (h *Handler) writeRecordFormFragment(w http.ResponseWriter, r *http.Request, ts tenantScope, entDef *entity.Definition, entityType, id string) {
	formDef, err := ts.formDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	locale := localeFromRequest(w, r)
	renderData, err := h.buildFormRenderData(r.Context(), ts, entDef, formDef, id, locale)
	if err != nil {
		writeCrudError(w, fmt.Sprintf("build %s form render data (id=%q)", entityType, id), err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.Render(w, formDef, entDef, renderData, locale); err != nil {
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
	// Version is the optimistic-locking counter (data.Record.Version) —
	// a JSON API client round-trips this back as "_version" on its next
	// update to get the same conflict protection formrender's own hidden
	// field gives a browser-driven save (see extractVersion).
	Version int `json:"version"`
}

func toRecordResponse(r data.Record) recordResponse {
	return recordResponse{ID: r.ID, EntityType: r.EntityType, Data: r.Data, Version: r.Version}
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
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	entityType := r.PathValue("entityType")
	locale := localeFromRequest(w, r)
	if id != "" && !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	if id == "" {
		// The blank "new record" form is the one page in this package that
		// reaches a user without making a single CRUD call of its own (an
		// entity with no reference fields loads nothing), so the guarded
		// engine never gets a chance to refuse it — ADR-0006 recorded that
		// as an accepted leak for its first commit, and this closes it. A
		// create form is a write surface, so CanWrite is the right gate:
		// a read-only role has no business being handed one, even though
		// submitting it would have been refused anyway.
		allowed, err := ts.crud.CanWrite(r.Context(), entityType)
		if !h.denyPageUnless(w, r, &rc, locale, allowed, err, "check write permission for new "+entityType+" form") {
			return
		}
	}

	entDef, err := ts.entityDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}
	formDef, err := ts.formDef(r.Context(), entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	renderData, err := h.buildFormRenderData(r.Context(), ts, entDef, formDef, id, locale)
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", entityType, id))
		return
	}
	if err != nil {
		h.writeCrudPageError(w, r, &rc, locale, fmt.Sprintf("build %s form render data (id=%q)", entityType, id), err)
		return
	}

	// isHTMXRequest(r) here means this GET was itself htmx-issued, not a
	// real browser navigation — today that only happens when the inline
	// reference-picker quick-create modal (part 2 of #24) fetches
	// /forms/{RefTarget}/new to embed inside its <dialog> (see layout.go's
	// uc-ref-create handler). A bare fragment is exactly what a modal
	// body swap needs, the same reasoning writeRecordFormFragment already
	// applies to a post-create/update swap — wrapping it in the full
	// <html> shell below would nest a second <head>/<body>/nav inside the
	// dialog instead of just the form. No existing caller of this route
	// sends HX-Request (grepped: no hx-boost, every current link here is
	// a plain <a>), so this is purely additive for real browser
	// navigation.
	// Vary: HX-Request — this URL now serves two different
	// representations of the same resource depending on that request
	// header (found by this feature's own independent review): without
	// it, a cache sitting in front of this route could serve a bare
	// fragment to a real browser navigation (inert markup — the exact
	// bug layout.go's shellTmpl doc comment says internal/e2e's first
	// real-browser test exists to catch) or the full shelled page into
	// the quick-create dialog.
	w.Header().Set("Vary", "HX-Request")
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := h.renderer.Render(w, formDef, entDef, renderData, locale); err != nil {
			log.Printf("api: render %s form fragment (id=%q): %v", entityType, id, err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		}
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
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
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
func (h *Handler) buildFormRenderData(ctx context.Context, ts tenantScope, entDef *entity.Definition, formDef *form.Definition, id, locale string) (formrender.Data, error) {
	// Resolved before anything else: a field this viewer may not see must
	// be absent from the form whether or not there's a record to load,
	// since a blank "new" form leaks a hidden field's NAME just as
	// readily as a populated one leaks its value.
	redacted, err := ts.crud.HiddenFields(ctx, entDef.EntityType)
	if err != nil {
		return formrender.Data{}, fmt.Errorf("resolve hidden fields for %s: %w", entDef.EntityType, err)
	}
	renderData := formrender.Data{RedactedFields: redacted}
	if id != "" {
		rec, err := ts.crud.Get(ctx, entDef, id)
		if err != nil {
			return formrender.Data{}, err
		}
		renderData.RecordID = rec.ID
		renderData.Version = rec.Version
		renderData.Record = rec.Data
		renderData.RecordLabel = h.recordLabel(entDef, rec, locale)

		children, childDefs, err := h.loadMasterDetailChildren(ctx, ts, entDef, formDef, id)
		if err != nil {
			return formrender.Data{}, fmt.Errorf("load master-detail children: %w", err)
		}
		renderData.Children = children
		renderData.ChildDefs = childDefs
	}
	// Resolved AFTER the record is loaded, because it depends on the
	// record's current values: the combobox (#24) only needs each
	// reference field's CURRENT selection labelled, so an existing record
	// shows a name rather than a raw id on load. A new/unset field needs
	// nothing pre-loaded — the picker fetches candidates on demand from
	// /api/references. This is the whole point of the task: form render no
	// longer lists every target record.
	renderData.ReferenceOptions = h.loadCurrentReferenceLabels(ctx, ts, entDef, renderData.Record, locale)
	// Independent of whether there's a current value: the quick-create
	// affordance (part 2 of #24) is offered per reference FIELD, not per
	// selection — even a brand-new, unset picker on a "new record" form
	// should offer to create the target inline if the viewer is allowed
	// to.
	renderData.ReferenceCreateLabels, err = h.loadReferenceCreateLabels(ctx, ts, entDef, locale)
	if err != nil {
		return formrender.Data{}, fmt.Errorf("resolve reference create labels for %s: %w", entDef.EntityType, err)
	}
	return renderData, nil
}

// loadReferenceCreateLabels builds the "+ Create new {Entity}" button text
// for each of entDef's FieldReference fields whose target BOTH (a) opts
// into entity.Definition.QuickCreatable and (b) the viewer holds create
// (write) permission on (part 2 of #24) — RBAC-gated the same way
// renderForm gates the "new record" page itself (CanWrite), just per
// target entity instead of per page. QuickCreatable is checked first and
// is the narrower gate in practice: this pipeline's own independent
// review found that CanWrite alone offers quick-create on every
// FieldReference target with no explicit Permission row denying it —
// including lookup/lifecycle entities like Status, which are wide open
// by RBAC default (not in authz's controlPlaneTypes) but whose fields
// (is_initial, is_terminal, status_type_id) are graph-shaping, not
// something to improvise from inside an unrelated form's modal.
// Two or more fields pointing at the same target (e.g. two Party
// references) resolve both checks once, not once per field: both are
// properties of the target entity type, not of which field points at it.
// A target with no published Definition (a dangling/misconfigured
// Target string) degrades to no button rather than failing the whole
// form render — the same graceful-degradation CanWrite failures below
// don't get, deliberately: a missing Definition is exactly the
// "unresolvable" case loadCurrentReferenceLabels already treats as
// non-fatal for the sibling reference-label lookup, for the same reason.
func (h *Handler) loadReferenceCreateLabels(ctx context.Context, ts tenantScope, entDef *entity.Definition, locale string) (map[string]string, error) {
	out := map[string]string{}
	type targetState struct {
		quickCreatable bool
		canWrite       bool
	}
	cache := map[string]targetState{}
	for _, f := range entDef.Fields {
		if f.Type != entity.FieldReference {
			continue
		}
		state, ok := cache[f.Target]
		if !ok {
			targetDef, err := ts.entityDef(ctx, f.Target)
			if err != nil {
				log.Printf("api: resolve quick-create eligibility for %s.%s -> %s: %v", entDef.EntityType, f.Name, f.Target, err)
				cache[f.Target] = targetState{}
				continue
			}
			state.quickCreatable = targetDef.QuickCreatable
			if state.quickCreatable {
				state.canWrite, err = ts.crud.CanWrite(ctx, f.Target)
				if err != nil {
					return nil, fmt.Errorf("check create permission for %s (referenced by %s): %w", f.Target, f.Name, err)
				}
			}
			cache[f.Target] = state
		}
		if !state.quickCreatable || !state.canWrite {
			continue
		}
		if hasJoinTargetFilter(f) {
			// The quick-create dialog (layout.go's .uc-ref-create handler)
			// can only POST a bare new record of f.Target — it has no way
			// to ALSO create the separate joined-entity row (e.g. a
			// PartyRole) an entity-join TargetFilter condition requires
			// (TimeEntry.employee_id's "must hold the employee PartyRole"),
			// so offering the button here would guarantee the very next
			// save 400s with no way to fix it from that dialog
			// (independent review, uc-infra#78 follow-up, finding #10).
			// Suppressed rather than offered-then-broken.
			continue
		}
		out[f.Name] = h.catalog.T(locale, "form.reference.create_new") + " " + h.entityDisplayName(locale, f.Target)
	}
	return out, nil
}

// hasJoinTargetFilter reports whether f declares an entity-join
// TargetFilter condition (Field.TargetFilter with Entity set) — see
// loadReferenceCreateLabels' own use above for why this specific shape
// is what a quick-create dialog cannot satisfy. Direct-field TargetFilter
// (Entity == "") and MustMatchParentField are deliberately NOT gated
// here: MustMatchParentField's own record-target-side value being unset
// on a freshly quick-created target is already tolerated as "not
// comparable, skip" (checkReferenceTargetConstraints' own posture, see
// its doc comment), so a quick-created Task with no project_id yet does
// not 400 the way a quick-created Party with no PartyRole row would.
func hasJoinTargetFilter(f entity.Field) bool {
	for _, cond := range f.TargetFilter {
		if cond.Entity != "" {
			return true
		}
	}
	return false
}

// loadCurrentReferenceLabels resolves ONLY the label of each reference
// field's currently-selected value — one indexed Get per set reference,
// not a full List of every candidate record. This is #24's actual scaling
// fix: the reference field renders as a searchable combobox
// (formrender's uc-ref) that fetches candidates on demand from
// /api/references, so a form render must no longer load every target
// record just to populate a <select> (which fell over at real
// customer-list scale — Farshid, 2026-07-29). The only thing the server
// still resolves eagerly is the current selection's human label, so an
// existing record's picker shows a name instead of a raw id on load.
//
// record == nil (a brand-new, unsaved form) needs nothing at all. A
// target lookup failure — including this viewer legitimately lacking read
// access to the referenced record — degrades to no pre-loaded label
// (logged, not surfaced) rather than failing the whole form render: the
// picker then simply opens empty, the same graceful degradation the old
// full-list loader applied. Labels are cached per target+id so two fields
// pointing at the same record resolve it once.
func (h *Handler) loadCurrentReferenceLabels(ctx context.Context, ts tenantScope, entDef *entity.Definition, record map[string]any, locale string) map[string][]formrender.ReferenceOption {
	if record == nil {
		return nil
	}
	out := map[string][]formrender.ReferenceOption{}
	cache := map[string]string{} // "target\x00id" -> label
	for _, f := range entDef.Fields {
		if f.Type != entity.FieldReference {
			continue
		}
		current, _ := record[f.Name].(string)
		if current == "" {
			continue
		}
		key := f.Target + "\x00" + current
		label, ok := cache[key]
		if !ok {
			var err error
			label, err = h.referenceLabelFor(ctx, ts, f.Target, current, locale)
			if err != nil {
				log.Printf("api: resolve reference label for %s.%s -> %s(%s): %v", entDef.EntityType, f.Name, f.Target, current, err)
				continue
			}
			cache[key] = label
		}
		out[f.Name] = []formrender.ReferenceOption{{ID: current, Label: label}}
	}
	return out
}

// referenceLabelFieldCandidates is the generic, entity-agnostic order of
// preference for picking which field labels a record in a reference
// picker/list cell: "name" is the overwhelming convention in this
// kernel's own Definitions (Party, Item, and most other entities), but
// an entity without one still deserves a human-readable label instead of
// falling straight to a raw id — "title" (e.g. Position) is the next
// most obvious "the thing a human calls this record" field already used
// elsewhere in this kernel (IssueReport). Deliberately NOT extended to
// "code" here: several existing entities (Account, TaxCode, CostCenter,
// Currency, UnitOfMeasure) declare "code" as a short identifier rather
// than a human-facing label, an existing test
// (TestAPI_RenderForm_ReferenceFieldWithoutNameFieldFallsBackToID) pins
// today's id-fallback behavior for a "code"-only entity, and widening
// the label convention that far is a bigger behavior change than this
// task's Position.reports_to_position_id picker needs. This is a
// data-driven field-name lookup, not entity-type-specific branching — it
// never checks targetType, only which of these field names the
// Definition happens to declare.
var referenceLabelFieldCandidates = []string{"name", "title"}

// referenceLabelFieldFor returns the field targetDef declares that should
// label its records in a picker/list cell — the first of
// referenceLabelFieldCandidates present, or "" if the entity declares
// none of them (in which case callers fall back to the raw id).
// An explicit Definition.LabelField wins over the name/title
// convention — see that field's own doc comment for why an entity may
// legitimately have neither.
func referenceLabelFieldFor(targetDef *entity.Definition) string {
	if targetDef.LabelField != "" {
		if _, ok := targetDef.FieldByName(targetDef.LabelField); ok {
			return targetDef.LabelField
		}
	}
	for _, candidate := range referenceLabelFieldCandidates {
		if _, ok := targetDef.FieldByName(candidate); ok {
			return candidate
		}
	}
	return ""
}

// recordLabel is the single place a referenced record's human label is
// derived from a LOADED record — shared by referenceLabelFor (which Gets
// the record) and searchReferenceOptions (which already has it). Labeled by
// the first field in referenceLabelFieldCandidates the target declares. If
// that field is an i18n_text (ADR-0009), the label is resolved for the
// viewer's locale via the i18n catalog's fallback chain; otherwise it's the
// plain string. Falls back to the raw id when there is no label field, the
// value is empty, or the i18n object has no usable translation. Ordinary
// (non-i18n) record data is still not translated — only a field explicitly
// declared i18n_text is (see ADR-0009 and locale.go's entityDisplayName,
// which translates the "PurchaseOrder" identifier, a different concern).
func (h *Handler) recordLabel(def *entity.Definition, rec data.Record, locale string) string {
	labelField := referenceLabelFieldFor(def)
	if labelField == "" {
		return rec.ID
	}
	if f, ok := def.FieldByName(labelField); ok && f.Type == entity.FieldI18nText {
		if s, ok := h.catalog.ResolveLocalized(rec.Data[labelField], locale); ok && s != "" {
			return s
		}
		return rec.ID
	}
	if s, ok := rec.Data[labelField].(string); ok && s != "" {
		return s
	}
	return rec.ID
}

// referenceLabelFor resolves ONE referenced record's human label by id —
// an indexed Get, not a full-table List — for the viewer's locale.
func (h *Handler) referenceLabelFor(ctx context.Context, ts tenantScope, targetType, id, locale string) (string, error) {
	targetDef, err := ts.entityDef(ctx, targetType)
	if err != nil {
		return "", fmt.Errorf("look up target entity %s: %w", targetType, err)
	}
	rec, err := ts.crud.Get(ctx, targetDef, id)
	if err != nil {
		return "", fmt.Errorf("get %s record %s: %w", targetType, id, err)
	}
	return h.recordLabel(targetDef, rec, locale), nil
}

// pageReferenceLabels resolves the labels of just the reference ids that
// actually appear in `records` (one list page) — field -> id -> label. Like
// the form combobox (#24), this deliberately does NOT list every target
// record: a page of 20 purchase orders resolves at most 20 distinct vendor
// ids, not the whole vendor table, so the list view scales with page size
// rather than with the referenced entity's total row count. A label that
// can't be resolved (dangling id, or this viewer lacking read access to
// the target) is simply omitted; the cell renderer falls back to the raw
// id — visible-but-broken beats silently hiding a dangling reference.
func (h *Handler) pageReferenceLabels(ctx context.Context, ts tenantScope, def *entity.Definition, records []data.Record, locale string) map[string]map[string]string {
	out := map[string]map[string]string{}
	cache := map[string]string{} // "target\x00id" -> label
	for _, f := range def.Fields {
		if f.Type != entity.FieldReference {
			continue
		}
		byID := map[string]string{}
		for _, rec := range records {
			id, _ := rec.Data[f.Name].(string)
			if id == "" {
				continue
			}
			if _, done := byID[id]; done {
				continue
			}
			key := f.Target + "\x00" + id
			label, ok := cache[key]
			if !ok {
				var err error
				label, err = h.referenceLabelFor(ctx, ts, f.Target, id, locale)
				if err != nil {
					log.Printf("api: resolve list reference label for %s.%s -> %s(%s): %v", def.EntityType, f.Name, f.Target, id, err)
					continue
				}
				cache[key] = label
			}
			byID[id] = label
		}
		out[f.Name] = byID
	}
	return out
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
func (h *Handler) loadMasterDetailChildren(ctx context.Context, ts tenantScope, entDef *entity.Definition, formDef *form.Definition, recordID string) (map[string][]map[string]any, map[string]*entity.Definition, error) {
	children := make(map[string][]map[string]any)
	// The child Definitions travel with the rows: the renderer needs
	// them for column order and to resolve i18n_text cells to the
	// viewer's locale (formrender.buildChildRows).
	childDefs := make(map[string]*entity.Definition)
	for _, section := range formDef.Sections {
		// Both component kinds resolve their rows the same way — parent
		// id matched against the child's ParentField. They differ in what
		// the RENDERER does with them (master-detail is editable and
		// rolls up; a related list is read-only), not in how they are
		// found. Related lists were previously skipped here, which left
		// the section empty and let its lazy-load fall through to an
		// endpoint that ignores the ref filter and returned every record
		// of the target type in the tenant — an asset's "Maintenance
		// History" listing other assets' work orders (independent
		// review, board #20).
		if section.Component != form.ComponentMasterDetail && section.Component != form.ComponentRelatedList {
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
		childDef, err := ts.entityDef(ctx, section.Target)
		if err != nil {
			return nil, nil, fmt.Errorf("look up %s definition for master-detail section: %w", section.Target, err)
		}
		records, err := ts.crud.ListByField(ctx, childDef, rel.ParentField, recordID)
		if err != nil {
			return nil, nil, fmt.Errorf("list %s children: %w", section.Target, err)
		}
		rows := make([]map[string]any, len(records))
		for i, rec := range records {
			rows[i] = rec.Data
		}
		children[section.Target] = rows
		childDefs[section.Target] = childDef
	}
	return children, childDefs, nil
}

func writeDefinitionLookupError(w http.ResponseWriter, entityType string, err error) {
	if errors.Is(err, data.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("no published definition for entity type %q", entityType))
		return
	}
	writeInternalError(w, fmt.Sprintf("look up definition for %s", entityType), err)
}
