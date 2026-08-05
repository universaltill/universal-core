// The External SQL sources settings page (ADR-0019): a tenant registers
// the external databases (a legacy ERP's SQL Server, any Postgres) the
// import wizard's SQL-source flow (extsqlimport.go) can pull records
// from. Unlike AIProviderConnection this is a LIST — a tenant may
// register several sources and each import run picks one — but the
// secret discipline is aiprovidersettings.go's exactly: the password is
// encrypted with the handler's secretcrypt Cryptor BEFORE the record
// ever reaches crud Create/Update, it is never echoed back to a browser
// (only whether one is stored), and a blank password on edit means
// "keep the stored one". ExternalSQLSource deliberately has no generic
// form Definition (see foundation.go/forms.go) — this bespoke page is
// the only way the entity is meant to be edited, precisely because of
// the encrypted secret.
//
// "Test connection" dials the source with the stored credentials and a
// short timeout. Driver errors routinely embed the DSN (host, database,
// even credentials), so a failure NEVER renders the driver error's text
// — only a generic translated message, with the detail logged
// server-side (same reasoning as writeInternalError).
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/sqlsource"
)

// extSQLTestTimeout bounds the "Test connection" dial — a settings page
// button must fail fast on an unreachable host, not tie up a request
// goroutine for the driver's own (much longer) default dial timeout.
const extSQLTestTimeout = 5 * time.Second

// extSQLSourceRecords lists the tenant's ExternalSQLSource records via
// the guarded engine. ok=false means the response was already written.
func (h *Handler) extSQLSourceRecords(w http.ResponseWriter, r *http.Request, ts tenantScope) (def *entity.Definition, records []data.Record, ok bool) {
	def, err := ts.entityDef(r.Context(), "ExternalSQLSource")
	if err != nil {
		writeDefinitionLookupError(w, "ExternalSQLSource", err)
		return nil, nil, false
	}
	records, err = ts.crud.List(r.Context(), def)
	if err != nil {
		writeCrudError(w, "list ExternalSQLSource records", err)
		return nil, nil, false
	}
	return def, records, true
}

// extSQLSourcesPage renders the list of registered sources (never the
// password — only whether one is stored) plus per-source edit/test/
// delete forms and a create form.
func (h *Handler) extSQLSourcesPage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	_, records, ok := h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}

	var buf bytes.Buffer
	if err := extSQLSourcesTmpl.ExecuteTemplate(&buf, "page", h.extSQLSourcesPageView(locale, records)); err != nil {
		writeInternalError(w, "render external SQL sources page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := h.renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render external SQL sources page shell", err)
	}
}

// extSQLSourceCreate handles the "add a source" form.
func (h *Handler) extSQLSourceCreate(w http.ResponseWriter, r *http.Request) {
	h.extSQLSourceSave(w, r, "")
}

// extSQLSourceUpdate handles one existing source's edit form.
func (h *Handler) extSQLSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}
	h.extSQLSourceSave(w, r, id)
}

// extSQLSourceSave is the shared create/update path. id == "" creates.
// A validation failure (bad driver, unparsable port, a password with no
// encryption key configured) re-renders the page with the error inline
// and nothing written — the same never-lose-the-form shape
// aiprovidersettings.go established.
func (h *Handler) extSQLSourceSave(w http.ResponseWriter, r *http.Request, id string) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)

	def, records, ok := h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}

	var existing map[string]any
	var version int
	if id != "" {
		rec, found := recordByID(records, id)
		if !found {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("ExternalSQLSource %q not found", id))
			return
		}
		existing = rec.Data
		version = rec.Version
	}

	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid form submission: "+err.Error())
		return
	}

	fields, err := h.buildExtSQLSourceFields(def, locale, r.PostForm, existing)
	if err != nil {
		view := h.extSQLSourcesPageView(locale, records)
		h.attachExtSQLError(&view, id, h.validationErrorMessage(locale, err))
		h.writeExtSQLSourcesFragment(w, view)
		return
	}

	if id == "" {
		if _, err := ts.crud.Create(r.Context(), def, fields, rc.Actor); err != nil {
			h.writeCrudErrorLocalized(w, r, "create ExternalSQLSource", err)
			return
		}
	} else {
		v := version
		if _, err := ts.crud.Update(r.Context(), def, id, fields, &v, rc.Actor); err != nil {
			if errors.Is(err, data.ErrNotFound) {
				httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("ExternalSQLSource %q not found", id))
				return
			}
			h.writeCrudErrorLocalized(w, r, "update ExternalSQLSource "+id, err)
			return
		}
	}

	_, records, ok = h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}
	h.writeExtSQLSourcesFragment(w, h.extSQLSourcesPageView(locale, records))
}

// extSQLSourceDelete removes one source. Deleting a source a concurrent
// request already removed is treated as done, not an error — the end
// state is identical.
func (h *Handler) extSQLSourceDelete(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	def, records, ok := h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}
	if _, found := recordByID(records, id); found {
		if err := ts.crud.Delete(r.Context(), def, id, rc.Actor); err != nil {
			// Localized: the guarded Delete refuses a record a read_only
			// system of record owns (uc-infra#102) — translated 409, not
			// a generic failure.
			h.writeCrudErrorLocalized(w, r, "delete ExternalSQLSource "+id, err)
			return
		}
	}

	_, records, ok = h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}
	h.writeExtSQLSourcesFragment(w, h.extSQLSourcesPageView(locale, records))
}

// extSQLSourceTest dials one stored source and reports reachable / not
// reachable inline. Only ever renders this page's own translated
// messages: a driver error's text can embed the DSN (credentials
// included), so it goes to the server log and nowhere else.
func (h *Handler) extSQLSourceTest(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}
	locale := localeFromRequest(w, r)
	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	_, records, ok := h.extSQLSourceRecords(w, r, ts)
	if !ok {
		return
	}
	rec, found := recordByID(records, id)
	if !found {
		httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("ExternalSQLSource %q not found", id))
		return
	}

	testOK := h.pingExtSQLSource(r.Context(), rec.Data, id)

	view := h.extSQLSourcesPageView(locale, records)
	for i := range view.Sources {
		if view.Sources[i].ID == id {
			view.Sources[i].TestDone = true
			view.Sources[i].TestOK = testOK
			if testOK {
				view.Sources[i].TestMessage = h.catalog.T(locale, "extsql.test_ok")
			} else {
				view.Sources[i].TestMessage = h.catalog.T(locale, "extsql.test_failed")
			}
		}
	}
	h.writeExtSQLSourcesFragment(w, view)
}

// pingExtSQLSource dials the source described by fields and reports
// success. Every failure detail is logged, never returned — see
// extSQLSourceTest.
func (h *Handler) pingExtSQLSource(ctx context.Context, fields map[string]any, id string) bool {
	src, err := h.extSQLSourceFromFields(fields)
	if err != nil {
		log.Printf("api: test ExternalSQLSource %s: %v", id, err)
		return false
	}
	dsn, err := src.DSN()
	if err != nil {
		log.Printf("api: test ExternalSQLSource %s: compose DSN: %v", id, err)
		return false
	}
	ext, err := data.OpenExtSQL(src.Driver, dsn)
	if err != nil {
		log.Printf("api: test ExternalSQLSource %s: open: %v", id, err)
		return false
	}
	defer ext.Close()
	pingCtx, cancel := context.WithTimeout(ctx, extSQLTestTimeout)
	defer cancel()
	if err := ext.Ping(pingCtx); err != nil {
		log.Printf("api: test ExternalSQLSource %s: ping: %v", id, err)
		return false
	}
	return true
}

// extSQLSourceFromFields shapes one stored ExternalSQLSource record into
// the runtime sqlsource.Source, decrypting the stored password. The
// returned Source's Password is plaintext and must never be rendered or
// persisted (sqlsource.Source's own doc comment).
func (h *Handler) extSQLSourceFromFields(fields map[string]any) (sqlsource.Source, error) {
	name, _ := fields["name"].(string)
	driver, _ := fields["driver"].(string)
	host, _ := fields["host"].(string)
	database, _ := fields["database"].(string)
	username, _ := fields["username"].(string)
	options, _ := fields["options"].(string)
	// Both re-validated at dial time, not just at form-save time: a
	// record written through the generic JSON API (/api/records/
	// ExternalSQLSource) never went through buildExtSQLSourceFields, so
	// the funnel every dial passes through has to enforce the same
	// invariants (review finding, this commit). Failures here reach the
	// user only as the generic test/import failure message; the detail
	// is logged by the caller.
	if err := validateExtSQLHost(host); err != nil {
		return sqlsource.Source{}, fmt.Errorf("source %q: %w", name, err)
	}
	port := 0
	if p, ok := fields["port"].(float64); ok { // JSONB numbers decode as float64
		// int(p) on an out-of-range float is undefined-ish (platform
		// truncation) — refuse instead of converting.
		if p != math.Trunc(p) || p < 0 || p > 65535 {
			return sqlsource.Source{}, fmt.Errorf("source %q: stored port %v is not a valid port number", name, p)
		}
		port = int(p)
	}
	src := sqlsource.Source{
		Name:     name,
		Driver:   driver,
		Host:     host,
		Port:     port,
		Database: database,
		Username: username,
		Options:  options,
	}
	if encrypted, _ := fields["password_encrypted"].(string); encrypted != "" {
		if !h.secretCryptor.Enabled() {
			return src, fmt.Errorf("source %q has a stored password but this deployment has no SECRET_ENCRYPTION_KEY configured", name)
		}
		password, err := h.secretCryptor.Decrypt(encrypted)
		if err != nil {
			return src, fmt.Errorf("decrypt stored password for source %q: %w", name, err)
		}
		src.Password = password
	}
	return src, nil
}

// validateExtSQLHost rejects a link-local host — the same cloud-metadata
// SSRF defense validateOllamaBaseURL (aiprovidersettings.go) documents:
// this server dials the host with stored credentials on a tenant admin's
// behalf, and AWS/Azure/GCP all serve their instance-metadata APIs from
// the link-local range (169.254.0.0/16, fe80::/10). ONLY link-local is
// blocked, never private/RFC1918 ranges — "a legacy NAV on the tenant's
// own LAN" is this feature's primary use case (ADR-0019), so loopback
// and LAN addresses stay allowed. A metadata endpoint reached via a DNS
// name rather than a literal link-local IP isn't caught, the same
// accepted residual gap validateOllamaBaseURL records.
func validateExtSQLHost(host string) error {
	if ip := net.ParseIP(host); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return errors.New("host may not point at a link-local address")
	}
	return nil
}

// buildExtSQLSourceFields validates the submitted form and returns the
// full field set to persist (Update replaces record data wholesale —
// crud.Engine.Update's contract — so every field must be present, not
// just the changed ones). The password field follows
// buildAIProviderFields' convention: blank on edit keeps the stored
// ciphertext, non-blank requires the Cryptor and is encrypted here,
// before Create/Update ever sees it.
func (h *Handler) buildExtSQLSourceFields(def *entity.Definition, locale string, form url.Values, existing map[string]any) (map[string]any, error) {
	fields := map[string]any{
		"name":     form.Get("name"),
		"driver":   form.Get("driver"),
		"host":     form.Get("host"),
		"database": form.Get("database"),
	}
	if username := form.Get("username"); username != "" {
		fields["username"] = username
	}
	if options := form.Get("options"); options != "" {
		fields["options"] = options
	}
	if err := validateExtSQLHost(form.Get("host")); err != nil {
		return nil, errors.New(h.catalog.T(locale, "extsql.error_link_local_host"))
	}
	if rawPort := form.Get("port"); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port <= 0 || port > 65535 {
			return nil, errors.New(h.catalog.T(locale, "extsql.error_bad_port"))
		}
		fields["port"] = float64(port)
	}

	password := form.Get("password")
	if password != "" {
		if !h.secretCryptor.Enabled() {
			return nil, errors.New(h.catalog.T(locale, "extsql.error_no_cryptor"))
		}
		encrypted, err := h.secretCryptor.Encrypt(password)
		if err != nil {
			return nil, fmt.Errorf("encrypt password: %w", err)
		}
		fields["password_encrypted"] = encrypted
	} else if existingEncrypted, _ := existing["password_encrypted"].(string); existingEncrypted != "" {
		fields["password_encrypted"] = existingEncrypted
	}

	// Validated here so a bad submission is an inline form error, not an
	// opaque engine failure — the driver enum and the required
	// name/driver/host/database fields are all enforced by the entity
	// Definition itself (foundation.ExternalSQLSource), kept generic.
	if err := entity.ValidateRecord(def, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// recordByID finds one record in a listed slice — the settings page
// always lists first, so a second indexed Get would be a wasted round
// trip.
func recordByID(records []data.Record, id string) (data.Record, bool) {
	for _, rec := range records {
		if rec.ID == id {
			return rec, true
		}
	}
	return data.Record{}, false
}

// writeExtSQLSourcesFragment writes the page template bare (no shell) —
// the POST responses here are full-page form submissions in the same
// style aiprovidersettings.go uses.
func (h *Handler) writeExtSQLSourcesFragment(w http.ResponseWriter, view extSQLSourcesView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := extSQLSourcesTmpl.ExecuteTemplate(w, "page", view); err != nil {
		writeInternalError(w, "render external SQL sources result", err)
	}
}

// attachExtSQLError places msg on the form it belongs to: the matching
// source's edit form, or the create form when id is "" (or unknown).
func (h *Handler) attachExtSQLError(view *extSQLSourcesView, id, msg string) {
	if id != "" {
		for i := range view.Sources {
			if view.Sources[i].ID == id {
				view.Sources[i].SaveError = msg
				return
			}
		}
	}
	view.CreateError = msg
}

// extSQLSourcesPageView shapes the listed records into the template's
// view model. The password never leaves storage: only HasPassword does.
func (h *Handler) extSQLSourcesPageView(locale string, records []data.Record) extSQLSourcesView {
	view := extSQLSourcesView{
		Heading:           h.catalog.T(locale, "extsql.heading"),
		Intro:             h.catalog.T(locale, "extsql.intro"),
		NoneYet:           h.catalog.T(locale, "extsql.none_yet"),
		AddHeading:        h.catalog.T(locale, "extsql.add_heading"),
		NameLabel:         h.catalog.T(locale, "extsql.name_label"),
		DriverLabel:       h.catalog.T(locale, "extsql.driver_label"),
		HostLabel:         h.catalog.T(locale, "extsql.host_label"),
		PortLabel:         h.catalog.T(locale, "extsql.port_label"),
		DatabaseLabel:     h.catalog.T(locale, "extsql.database_label"),
		UsernameLabel:     h.catalog.T(locale, "extsql.username_label"),
		PasswordLabel:     h.catalog.T(locale, "extsql.password_label"),
		PasswordHintSet:   h.catalog.T(locale, "extsql.password_hint_set"),
		PasswordHintUnset: h.catalog.T(locale, "extsql.password_hint_unset"),
		OptionsLabel:      h.catalog.T(locale, "extsql.options_label"),
		OptionsHint:       h.catalog.T(locale, "extsql.options_hint"),
		AddLabel:          h.catalog.T(locale, "extsql.add_button"),
		SaveLabel:         h.catalog.T(locale, "extsql.save_button"),
		DeleteLabel:       h.catalog.T(locale, "extsql.delete_button"),
		TestLabel:         h.catalog.T(locale, "extsql.test_button"),
	}
	for _, rec := range records {
		s := extSQLSourceView{ID: rec.ID}
		s.Name, _ = rec.Data["name"].(string)
		s.Driver, _ = rec.Data["driver"].(string)
		s.Host, _ = rec.Data["host"].(string)
		s.Database, _ = rec.Data["database"].(string)
		s.Username, _ = rec.Data["username"].(string)
		s.Options, _ = rec.Data["options"].(string)
		if p, ok := rec.Data["port"].(float64); ok {
			s.Port = strconv.Itoa(int(p))
		}
		encrypted, _ := rec.Data["password_encrypted"].(string)
		s.HasPassword = encrypted != ""
		view.Sources = append(view.Sources, s)
	}
	return view
}

// --- view model ---

type extSQLSourcesView struct {
	Heading           string
	Intro             string
	NoneYet           string
	AddHeading        string
	NameLabel         string
	DriverLabel       string
	HostLabel         string
	PortLabel         string
	DatabaseLabel     string
	UsernameLabel     string
	PasswordLabel     string
	PasswordHintSet   string
	PasswordHintUnset string
	OptionsLabel      string
	OptionsHint       string
	AddLabel          string
	SaveLabel         string
	DeleteLabel       string
	TestLabel         string

	Sources     []extSQLSourceView
	CreateError string
}

type extSQLSourceView struct {
	ID          string
	Name        string
	Driver      string
	Host        string
	Port        string
	Database    string
	Username    string
	Options     string
	HasPassword bool

	SaveError   string
	TestDone    bool
	TestOK      bool
	TestMessage string
}

// extSQLFormCtx pairs the page-level labels with one source's values so
// the shared "form-fields" template block renders both the edit forms
// and the create form (a zero-value source) from one definition.
type extSQLFormCtx struct {
	V extSQLSourcesView
	S extSQLSourceView
}

// The driver <option> labels are proper nouns (product names), not
// translatable copy — the same treatment aiprovidersettings.go gives
// "Ollama"/"Anthropic (Claude)"/"OpenAI (ChatGPT)".
var extSQLSourcesTmpl = template.Must(template.New("ext-sql-sources").Funcs(template.FuncMap{
	"formctx": func(v extSQLSourcesView, s extSQLSourceView) extSQLFormCtx {
		// The nested view would recurse Sources into every form needlessly;
		// the form block only reads labels, so drop the slice.
		v.Sources = nil
		return extSQLFormCtx{V: v, S: s}
	},
	"newsource": func() extSQLSourceView { return extSQLSourceView{Driver: "postgres"} },
}).Parse(`
{{define "form-fields"}}
<label>{{.V.NameLabel}}
<input type="text" name="name" value="{{.S.Name}}" required></label>
<label>{{.V.DriverLabel}}
<select name="driver">
<option value="postgres" {{if eq .S.Driver "postgres"}}selected{{end}}>PostgreSQL</option>
<option value="mssql" {{if eq .S.Driver "mssql"}}selected{{end}}>Microsoft SQL Server</option>
</select></label>
<label>{{.V.HostLabel}}
<input type="text" name="host" value="{{.S.Host}}" required></label>
<label>{{.V.PortLabel}}
<input type="text" name="port" value="{{.S.Port}}" inputmode="numeric"></label>
<label>{{.V.DatabaseLabel}}
<input type="text" name="database" value="{{.S.Database}}" required></label>
<label>{{.V.UsernameLabel}}
<input type="text" name="username" value="{{.S.Username}}"></label>
<label>{{.V.PasswordLabel}}
<input type="password" name="password" autocomplete="off"></label>
<p class="uc-extsql-hint">{{if .S.HasPassword}}{{.V.PasswordHintSet}}{{else}}{{.V.PasswordHintUnset}}{{end}}</p>
<label>{{.V.OptionsLabel}}
<input type="text" name="options" value="{{.S.Options}}"></label>
<p class="uc-extsql-hint">{{.V.OptionsHint}}</p>
{{end}}

{{define "page"}}
<div class="uc-extsql-sources">
<h1>{{.Heading}}</h1>
<p>{{.Intro}}</p>
{{if not .Sources}}<p class="uc-extsql-none">{{.NoneYet}}</p>{{end}}
{{$v := .}}
{{range .Sources}}
<details class="uc-extsql-source" open>
<summary>{{.Name}} — {{.Driver}} {{.Host}}/{{.Database}}</summary>
{{if .SaveError}}<p class="uc-extsql-error">{{.SaveError}}</p>{{end}}
{{if .TestDone}}<p class="{{if .TestOK}}uc-extsql-test-ok{{else}}uc-extsql-test-failed{{end}}">{{.TestMessage}}</p>{{end}}
<form method="post" action="/settings/sql-sources/{{.ID}}">
{{template "form-fields" (formctx $v .)}}
<button type="submit">{{$v.SaveLabel}}</button>
</form>
<form method="post" action="/settings/sql-sources/{{.ID}}/test">
<button type="submit">{{$v.TestLabel}}</button>
</form>
<form method="post" action="/settings/sql-sources/{{.ID}}/delete">
<button type="submit">{{$v.DeleteLabel}}</button>
</form>
</details>
{{end}}

<h2>{{.AddHeading}}</h2>
{{if .CreateError}}<p class="uc-extsql-error">{{.CreateError}}</p>{{end}}
<form method="post" action="/settings/sql-sources">
{{template "form-fields" (formctx $v (newsource))}}
<button type="submit">{{.AddLabel}}</button>
</form>
</div>
{{end}}
`))
