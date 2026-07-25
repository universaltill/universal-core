package api

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/aiassist"
	"github.com/universaltill/universal-core/internal/kernel/aiprovider"
	"github.com/universaltill/universal-core/internal/kernel/claudeassist"
	"github.com/universaltill/universal-core/internal/kernel/csvimport"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/openaiassist"
)

// maxUploadBytes bounds the whole multipart request body for an import
// upload. csvimport's XLSX reader already caps itself internally (see
// xlsx.go's maxXLSXFileSize), but the CSV path has no size cap of its
// own — csvimport.Preview/Commit read a CSV file to completion
// unconditionally, which was an accepted, low-risk gap while nothing
// exposed it over HTTP. Wiring an upload endpoint is exactly the moment
// that gap goes live, so the cap belongs here, at the HTTP boundary,
// where it protects both formats uniformly regardless of what either
// engine does internally.
const maxUploadBytes = 50 << 20 // 50 MiB

// importUploadPage renders the upload form shell — a file input plus a
// Preview button, and an empty result container. The file input's own
// form (#uc-import-form) is never itself replaced by any later HTMX
// swap in this flow (only #uc-import-result's contents are), so the
// browser keeps the selected file available for the Commit step's own
// request without the server ever needing to hold the uploaded bytes
// between requests.
func (h *Handler) importUploadPage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")

	if _, err := h.entityDef(r.Context(), rc.TenantID, entityType); err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	locale := localeFromRequest(w, r)
	// Rendered into a buffer first, not straight to w — this is a
	// top-level page navigation, not an htmx-swap response, so it needs
	// the real <html><head> shell that loads htmx.js (see layout.go);
	// without it the Preview button's hx-post is inert markup, a real
	// browser has nothing to execute it. The preview/commit fragments
	// this page's own htmx swaps into #uc-import-result stay bare —
	// only the very first page load gets the shell.
	var buf bytes.Buffer
	err := importTmpl.ExecuteTemplate(&buf, "page", importPageView{
		EntityType:   entityType,
		PreviewHref:  "/import/" + entityType + "/preview",
		ChooseFile:   h.catalog.T(locale, "import.choose_file"),
		PreviewLabel: h.catalog.T(locale, "import.preview_button"),
	})
	if err != nil {
		writeInternalError(w, "render import upload page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render import upload page shell", err)
	}
}

// importPreview parses the uploaded file and renders a fragment showing
// an editable column mapping alongside a validation preview of every
// row — nothing is written yet.
//
// The mapping comes from mappingFromForm(r) when the request already
// carries mapping.* fields (the user is re-submitting after adjusting
// the <select> dropdowns this same handler rendered), falling back to
// csvimport.SuggestMapping's name-match guess on the very first call for
// a given file. A real-world CSV's headers routinely don't exactly
// name-match the entity's field names for every required field (e.g.
// "Item Name" vs. the "name" field) — SuggestMapping's guess is only a
// starting point, not a guarantee. When the resulting mapping still
// doesn't satisfy csvimport.ValidateMapping, this used to fail the whole
// request with a raw JSON 400 and never show the mapping table at all,
// making the wizard unusable for any file the auto-guess couldn't fully
// resolve (found by actually driving the wizard against a real CSV, not
// by the unit tests, which always passed a pre-completed mapping in
// directly). Now an incomplete mapping still renders the mapping table
// (whatever was resolved, plus the unresolved fields blank) with the
// validation error surfaced inline and no rows/Commit button — only a
// "re-preview" button that resubmits the same file together with
// whatever the user just picked in the dropdowns.
func (h *Handler) importPreview(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")
	locale := localeFromRequest(w, r)

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	data, xlsx, ok := readUploadedFile(w, r)
	if !ok {
		return // readUploadedFile already wrote the response
	}

	submitted := mappingFromForm(r)

	ai := h.aiProviderFor(r.Context(), rc.TenantID)
	headers, mapping, aiSuggested, results, mappingErr, err := previewUpload(r.Context(), ai, data, xlsx, def, submitted)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	view := importPreviewView{
		EntityType:      entityType,
		PreviewHref:     "/import/" + entityType + "/preview",
		CommitHref:      "/import/" + entityType + "/commit",
		MappingHeading:  h.catalog.T(locale, "import.mapping_heading"),
		RowsHeading:     h.catalog.T(locale, "import.rows_heading"),
		CommitLabel:     h.catalog.T(locale, "import.commit_button"),
		RepreviewLabel:  h.catalog.T(locale, "import.repreview_button"),
		AISuggestedNote: h.catalog.T(locale, "import.ai_suggested_note"),
		Mappings:        buildMappingRows(headers, mapping, aiSuggested, def, h.catalog.T(locale, "import.unmapped_option")),
	}
	if mappingErr != nil {
		view.MappingError = mappingErr.Error()
	} else {
		rowOK := h.catalog.T(locale, "import.row_status_ok")
		rowError := h.catalog.T(locale, "import.row_status_error")
		view.Rows = buildResultRows(results, rowOK, rowError)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := importTmpl.ExecuteTemplate(w, "preview", view); err != nil {
		writeInternalError(w, "render import preview", err)
	}
}

// importCommit re-parses the same uploaded file (the browser resubmits
// it fresh — see importUploadPage's comment) together with the mapping
// the preview step's <select> elements now carry, and actually writes
// the rows that pass validation.
func (h *Handler) importCommit(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	entityType := r.PathValue("entityType")
	locale := localeFromRequest(w, r)

	def, err := h.entityDef(r.Context(), rc.TenantID, entityType)
	if err != nil {
		writeDefinitionLookupError(w, entityType, err)
		return
	}

	data, xlsx, ok := readUploadedFile(w, r)
	if !ok {
		return
	}
	mapping := mappingFromForm(r)

	var results []csvimport.RowResult
	if xlsx {
		results, err = csvimport.CommitXLSX(r.Context(), bytes.NewReader(data), def, mapping, h.crud, rc.TenantID, rc.Actor)
	} else {
		results, err = csvimport.Commit(r.Context(), bytes.NewReader(data), def, mapping, h.crud, rc.TenantID, rc.Actor)
	}
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	succeeded := 0
	for _, res := range results {
		if res.Err == nil {
			succeeded++
		}
	}

	rowOK := h.catalog.T(locale, "import.row_status_ok")
	rowError := h.catalog.T(locale, "import.row_status_error")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = importTmpl.ExecuteTemplate(w, "result", importResultView{
		Heading:        h.catalog.T(locale, "import.result_heading"),
		Succeeded:      succeeded,
		Failed:         len(results) - succeeded,
		Total:          len(results),
		SucceededLabel: h.catalog.T(locale, "import.result_succeeded"),
		FailedLabel:    h.catalog.T(locale, "import.result_failed"),
		Rows:           buildResultRows(results, rowOK, rowError),
	})
	if err != nil {
		writeInternalError(w, "render import result", err)
	}
}

// readUploadedFile parses the multipart request (capped at
// maxUploadBytes), extracts the "file" field, and reads it fully into
// memory. On any failure it writes the HTTP response itself and returns
// ok=false purely as a "stop, I already responded" signal to the caller.
func readUploadedFile(w http.ResponseWriter, r *http.Request) (data []byte, xlsx bool, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid or oversized upload (max %d MiB): %s", maxUploadBytes>>20, err.Error()))
		return nil, false, false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, `no file uploaded (expected a "file" form field)`)
		return nil, false, false
	}
	defer file.Close()

	data, err = io.ReadAll(file)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "could not read uploaded file: "+err.Error())
		return nil, false, false
	}
	xlsx = strings.HasSuffix(strings.ToLower(header.Filename), ".xlsx")
	return data, xlsx, true
}

// previewUpload reads data's headers, resolves a mapping (submitted if
// non-empty, otherwise a guess), and — only if that mapping passes
// csvimport.ValidateMapping — runs the row-level preview. A failing
// mapping is reported via mappingErr, not err: err is reserved for the
// file itself being unreadable (a real 400, nothing left to show the
// user); mappingErr is recoverable, the caller still has headers+
// mapping to render the editable mapping table with.
//
// The initial guess (submitted empty — this is the very first preview
// of a freshly uploaded file, not the user resubmitting after editing
// the mapping dropdowns) is csvimport.SuggestMapping's exact
// normalized-name match, then — only for whatever SuggestMapping
// couldn't resolve, and only when ai is enabled — csvimport.
// SuggestMappingAI's turn to fill the rest by actually looking at
// sample values, not just header text. AI never runs on a resubmission
// (submitted non-empty): once a human has started editing the mapping
// table, only their own choices matter, an AI guess overwriting an
// edit mid-review would be exactly the kind of silent surprise this
// wizard's whole "confirm or edit before anything is written" design
// exists to prevent. aiSuggested names which of the returned mapping's
// entries came from the AI pass, purely for the UI to visibly flag
// those (still fully editable) rows as worth a closer look — see
// buildMappingRows.
func previewUpload(ctx context.Context, ai aiprovider.Provider, data []byte, xlsx bool, def *entity.Definition, submitted csvimport.ColumnMapping) (headers []string, mapping csvimport.ColumnMapping, aiSuggested map[string]bool, results []csvimport.RowResult, mappingErr error, err error) {
	var sampleRows [][]string
	if xlsx {
		headers, sampleRows, err = csvimport.SampleRowsXLSX(bytes.NewReader(data))
	} else {
		headers, sampleRows, err = csvimport.SampleRows(bytes.NewReader(data))
	}
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("read uploaded file: %w", err)
	}

	aiSuggested = map[string]bool{}
	if len(submitted) > 0 {
		mapping = submitted
	} else {
		mapping = csvimport.SuggestMapping(headers, def)
		if ai.Enabled() {
			// aiassist's own default timeout (90s) is a ceiling meant for
			// a caller with no tighter budget of its own — this one has
			// one: an interactive HTMX preview request shouldn't be able
			// to tie up a request goroutine anywhere near that long just
			// because Ollama is cold or overloaded. 25s comfortably
			// covers the ~13-18s this took against the real homelab-k8s
			// instance (small model, 2-node Raspberry Pi hardware) with
			// headroom, while still failing fast enough that a slow
			// model degrades to "the heuristic-only mapping, a beat
			// later" rather than "the page hangs."
			aiCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			var aiErr error
			mapping, aiSuggested, aiErr = csvimport.SuggestMappingAI(aiCtx, ai, headers, sampleRows, def, mapping)
			cancel()
			if aiErr != nil {
				// Advisory only (see SuggestMappingAI's own doc comment)
				// — mapping is always still whatever SuggestMapping alone
				// produced, never blocked or failed by an AI hiccup.
				log.Printf("api: AI mapping suggestion for %s: %v", def.EntityType, aiErr)
			}
		}
	}

	if mappingErr = csvimport.ValidateMapping(def, headers, mapping); mappingErr != nil {
		return headers, mapping, aiSuggested, nil, mappingErr, nil
	}

	if xlsx {
		results, err = csvimport.PreviewXLSX(bytes.NewReader(data), def, mapping)
	} else {
		results, err = csvimport.Preview(bytes.NewReader(data), def, mapping)
	}
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return headers, mapping, aiSuggested, results, nil, nil
}

// aiProviderFor resolves which aiprovider.Provider a request for
// tenantID should use for AI assistance: the tenant's own
// AIProviderConnection override if one is configured (BYOK — see
// aiprovidersettings.go), falling back to h.ai (the platform's own
// default self-hosted Ollama) otherwise. Never returns nil: a caller can
// always call .Enabled()/.GenerateJSON() on what comes back exactly like
// it always could on h.ai alone (every concrete client's nil-safe
// receiver contract — see aiassist/claudeassist/openaiassist's own doc
// comments — means even a disabled fallback is safe to call through).
//
// Any failure resolving the override (a definition/list lookup error, or
// a stored ciphertext this deployment's current SECRET_ENCRYPTION_KEY
// can't decrypt — e.g. the key rotated since the tenant saved it)
// degrades to the platform default, logged not surfaced: SuggestMappingAI
// treats AI assistance as advisory-only regardless of which provider
// backs it, so a tenant's broken BYOK config must never take down AI
// assistance entirely when a perfectly good platform default is right
// there.
func (h *Handler) aiProviderFor(ctx context.Context, tenantID string) aiprovider.Provider {
	def, err := h.entityDef(ctx, tenantID, "AIProviderConnection")
	if err != nil {
		return h.ai
	}
	records, err := h.crud.List(ctx, def, tenantID)
	if err != nil {
		log.Printf("api: resolve AI provider for tenant %s: list AIProviderConnection: %v", tenantID, err)
		return h.ai
	}
	if len(records) == 0 {
		return h.ai
	}
	fields := records[0].Data
	provider, _ := fields["provider"].(string)
	model, _ := fields["model"].(string)
	switch provider {
	case "ollama":
		baseURL, _ := fields["base_url"].(string)
		return aiassist.NewClient(baseURL, model)
	case "anthropic":
		key, ok := h.decryptStoredAPIKey(tenantID, fields)
		if !ok {
			return h.ai
		}
		return claudeassist.NewClient(key, model)
	case "openai":
		key, ok := h.decryptStoredAPIKey(tenantID, fields)
		if !ok {
			return h.ai
		}
		return openaiassist.NewClient(key, model)
	default:
		return h.ai
	}
}

// decryptStoredAPIKey decrypts fields' api_key_encrypted for tenantID —
// ok is false (never a stop-the-request error, see aiProviderFor's own
// doc comment) when there's nothing to decrypt, this deployment has no
// SECRET_ENCRYPTION_KEY configured, or decryption itself fails.
func (h *Handler) decryptStoredAPIKey(tenantID string, fields map[string]any) (key string, ok bool) {
	encrypted, _ := fields["api_key_encrypted"].(string)
	if encrypted == "" || !h.secretCryptor.Enabled() {
		return "", false
	}
	key, err := h.secretCryptor.Decrypt(encrypted)
	if err != nil {
		log.Printf("api: resolve AI provider for tenant %s: decrypt stored API key: %v", tenantID, err)
		return "", false
	}
	return key, true
}

// mappingFromForm reconstructs a csvimport.ColumnMapping from the
// preview fragment's "mapping.<header>" <select> fields (see
// buildMappingRows) — the counterpart of how they were named when
// rendered. A select left at the unmapped option submits an empty
// value, which is skipped here the same way an absent mapping entry
// would be.
func mappingFromForm(r *http.Request) csvimport.ColumnMapping {
	mapping := csvimport.ColumnMapping{}
	if r.MultipartForm == nil {
		return mapping
	}
	const prefix = "mapping."
	for key, values := range r.MultipartForm.Value {
		if !strings.HasPrefix(key, prefix) || len(values) == 0 || values[0] == "" {
			continue
		}
		header := strings.TrimPrefix(key, prefix)
		mapping[header] = values[0]
	}
	return mapping
}

// --- view models ---

type importPageView struct {
	EntityType   string
	PreviewHref  string
	ChooseFile   string
	PreviewLabel string
}

type importPreviewView struct {
	EntityType     string
	PreviewHref    string
	CommitHref     string
	MappingHeading string
	RowsHeading    string
	CommitLabel    string
	RepreviewLabel string
	// AISuggestedNote is the label shown next to any mapping row
	// buildMappingRows flags AISuggested — only ever rendered when at
	// least one row actually is (see the template), so a deployment
	// with no OLLAMA_URL configured never shows AI-related copy at all.
	AISuggestedNote string
	Mappings        []mappingRowView
	// MappingError is set instead of Rows when the current mapping fails
	// csvimport.ValidateMapping (e.g. a required field still unmapped) —
	// the template shows the mapping table plus this message and a
	// re-preview button, never a Commit button, until it's empty.
	MappingError string
	Rows         []previewRowView
}

type mappingRowView struct {
	Header string
	// AISuggested marks a row whose currently-selected option came from
	// csvimport.SuggestMappingAI rather than SuggestMapping's exact
	// name match — still just pre-filling the same editable <select>
	// every row already has, per the "confirm or edit" design: this is
	// purely a visible hint to look twice, never a different trust
	// level in what the wizard will actually do with the row.
	AISuggested bool
	Options     []mappingOptionView
}

type mappingOptionView struct {
	Value    string
	Label    string
	Selected bool
}

type previewRowView struct {
	RowNumber int
	Status    string
	OK        bool
	Data      string
	Error     string
}

type importResultView struct {
	Heading        string
	Succeeded      int
	Failed         int
	Total          int
	SucceededLabel string
	FailedLabel    string
	Rows           []previewRowView
}

func buildMappingRows(headers []string, mapping csvimport.ColumnMapping, aiSuggested map[string]bool, def *entity.Definition, unmappedLabel string) []mappingRowView {
	mappings := make([]mappingRowView, 0, len(headers))
	for _, header := range headers {
		selectedField := mapping[header]
		options := make([]mappingOptionView, 0, len(def.Fields)+1)
		options = append(options, mappingOptionView{Value: "", Label: unmappedLabel, Selected: selectedField == ""})
		for _, f := range def.Fields {
			options = append(options, mappingOptionView{Value: f.Name, Label: f.Name, Selected: f.Name == selectedField})
		}
		mappings = append(mappings, mappingRowView{Header: header, AISuggested: aiSuggested[header], Options: options})
	}
	return mappings
}

func buildResultRows(results []csvimport.RowResult, okLabel, errorLabel string) []previewRowView {
	rows := make([]previewRowView, len(results))
	for i, res := range results {
		row := previewRowView{RowNumber: res.RowNumber, Data: fmt.Sprintf("%v", res.Data)}
		if res.Err == nil {
			row.OK = true
			row.Status = okLabel
		} else {
			row.Status = errorLabel
			row.Error = res.Err.Error()
		}
		rows[i] = row
	}
	return rows
}

var importTmpl = template.Must(template.New("import").Parse(`
{{define "page"}}
<div class="uc-import" data-entity-type="{{.EntityType}}">
<form id="uc-import-form" enctype="multipart/form-data">
<label for="uc-import-file">{{.ChooseFile}}</label>
<input type="file" id="uc-import-file" name="file" accept=".csv,.xlsx" required>
<button type="button" hx-post="{{.PreviewHref}}" hx-include="#uc-import-form" hx-target="#uc-import-result" hx-encoding="multipart/form-data">{{.PreviewLabel}}</button>
</form>
<div id="uc-import-result"></div>
</div>
{{end}}

{{define "preview"}}
<div id="uc-import-preview">
<h2>{{.MappingHeading}}</h2>
<table class="uc-import-mapping">
<tbody>
{{range .Mappings}}
<tr>
<td>{{.Header}}{{if .AISuggested}} <span class="uc-ai-suggested">{{$.AISuggestedNote}}</span>{{end}}</td>
<td>
<select name="mapping.{{.Header}}">
{{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
</select>
</td>
</tr>
{{end}}
</tbody>
</table>

{{if .MappingError}}
<p class="uc-import-mapping-error">{{.MappingError}}</p>
<button type="button" hx-post="{{.PreviewHref}}" hx-include="#uc-import-form, #uc-import-preview" hx-target="#uc-import-result" hx-encoding="multipart/form-data">{{.RepreviewLabel}}</button>
{{else}}
<h2>{{.RowsHeading}}</h2>
<table class="uc-import-rows">
<tbody>
{{range .Rows}}
<tr class="{{if .OK}}uc-row-ok{{else}}uc-row-error{{end}}">
<td>{{.RowNumber}}</td>
<td>{{.Status}}</td>
<td>{{.Data}}</td>
<td>{{.Error}}</td>
</tr>
{{end}}
</tbody>
</table>

<button type="button" hx-post="{{.CommitHref}}" hx-include="#uc-import-form, #uc-import-preview" hx-target="#uc-import-result" hx-encoding="multipart/form-data">{{.CommitLabel}}</button>
{{end}}
</div>
{{end}}

{{define "result"}}
<div class="uc-import-result">
<h2>{{.Heading}}</h2>
<p>{{.Succeeded}} {{.SucceededLabel}}, {{.Failed}} {{.FailedLabel}} ({{.Total}} total)</p>
<table class="uc-import-rows">
<tbody>
{{range .Rows}}
<tr class="{{if .OK}}uc-row-ok{{else}}uc-row-error{{end}}">
<td>{{.RowNumber}}</td>
<td>{{.Status}}</td>
<td>{{.Error}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{end}}
`))
