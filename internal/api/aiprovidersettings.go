// The BYOK AI-provider settings page — Farshid's ask: a tenant should be
// able to plug in their own Anthropic or OpenAI API key, or point at
// their own (self-hosted, on their own machine or cloud) Ollama
// instance, and have the app use it in place of the platform's own
// default self-hosted Ollama. Subscription-login (OAuth against a
// tenant's own Claude Pro/Max or ChatGPT Plus account) was researched
// and ruled out: Anthropic bans third-party use of that flow outright,
// and OpenAI's equivalent only works inside their own first-party
// tooling — see QUEUE.md. This is the API-key/endpoint alternative
// Farshid approved instead ("that is fine, go for the api and ollama").
//
// One AIProviderConnection record per tenant (an upsert, not a list —
// see foundation.go's own doc comment on that entity): this file is
// solely responsible for looking that row up, validating it against
// whichever provider is selected, encrypting the API key before it's
// ever written, and never echoing a stored key back to the browser.
// internal/kernel/aiprovider's doc comment covers the resolver
// (aiProviderFor, import.go) that actually turns this stored config into
// a live client for a request — a separate concern from managing the
// config itself.
package api

import (
	"bytes"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// aiProviderConnectionRecord looks up tenantID's single AIProviderConnection
// row, if any — nil, nil when none exists yet (the tenant is on the
// platform default). AIProviderConnection has no unique constraint of its
// own (same reasoning as PurchaseOrder.po_number's precedent, see
// foundation.go): a tenant is only ever expected to have zero or one, and
// this is the one place in the kernel that reads/writes it, so nothing
// else could have created a second row to disagree with. If more than one
// somehow exists, the first (List's own, unspecified-beyond-insertion-
// order) is treated as authoritative rather than erroring — a settings
// page must always be able to render something to fix the situation from,
// not 500.
func (h *Handler) aiProviderConnectionRecord(w http.ResponseWriter, r *http.Request, tenantID string) (def *entity.Definition, id string, fields map[string]any, version int, ok bool) {
	def, err := h.entityDef(r.Context(), tenantID, "AIProviderConnection")
	if err != nil {
		writeDefinitionLookupError(w, "AIProviderConnection", err)
		return nil, "", nil, 0, false
	}
	records, err := h.crud.List(r.Context(), def, tenantID)
	if err != nil {
		writeInternalError(w, "list AIProviderConnection records", err)
		return nil, "", nil, 0, false
	}
	if len(records) == 0 {
		return def, "", nil, 0, true
	}
	rec := records[0]
	return def, rec.ID, rec.Data, rec.Version, true
}

// aiProviderSettingsPage renders the current configuration (an API key,
// if one is stored, is never read back out of Data for display — only
// whether one exists, see view.HasAPIKey) alongside a form to change it.
func (h *Handler) aiProviderSettingsPage(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)

	_, id, fields, _, ok := h.aiProviderConnectionRecord(w, r, rc.TenantID)
	if !ok {
		return
	}

	var buf bytes.Buffer
	err := aiProviderSettingsTmpl.ExecuteTemplate(&buf, "page", h.aiProviderSettingsPageView(locale, id, fields))
	if err != nil {
		writeInternalError(w, "render AI provider settings page", err)
		return
	}
	nav := h.renderNav(r, &rc, locale)
	if err := renderShell(w, locale, nav, template.HTML(buf.String())); err != nil {
		writeInternalError(w, "render AI provider settings page shell", err)
	}
}

// aiProviderSettingsPageView shapes fields (nil when the tenant has no
// override configured yet) into the form's view model.
func (h *Handler) aiProviderSettingsPageView(locale, id string, fields map[string]any) aiProviderSettingsView {
	view := aiProviderSettingsView{
		SaveHref:        "/settings/ai-provider",
		ClearHref:       "/settings/ai-provider/clear",
		Heading:         h.catalog.T(locale, "ai_provider.heading"),
		Intro:           h.catalog.T(locale, "ai_provider.intro"),
		ProviderLabel:   h.catalog.T(locale, "ai_provider.provider_label"),
		BaseURLLabel:    h.catalog.T(locale, "ai_provider.base_url_label"),
		BaseURLHint:     h.catalog.T(locale, "ai_provider.base_url_hint"),
		ModelLabel:      h.catalog.T(locale, "ai_provider.model_label"),
		APIKeyLabel:     h.catalog.T(locale, "ai_provider.api_key_label"),
		APIKeyHintSet:   h.catalog.T(locale, "ai_provider.api_key_hint_set"),
		APIKeyHintUnset: h.catalog.T(locale, "ai_provider.api_key_hint_unset"),
		SaveLabel:       h.catalog.T(locale, "ai_provider.save_button"),
		ClearLabel:      h.catalog.T(locale, "ai_provider.clear_button"),
		DefaultNote:     h.catalog.T(locale, "ai_provider.default_note"),
		HasOverride:     id != "",
	}
	if fields == nil {
		return view
	}
	provider, _ := fields["provider"].(string)
	baseURL, _ := fields["base_url"].(string)
	model, _ := fields["model"].(string)
	apiKeyEncrypted, _ := fields["api_key_encrypted"].(string)
	view.Provider = provider
	view.BaseURL = baseURL
	view.Model = model
	view.HasAPIKey = apiKeyEncrypted != ""
	view.ProviderIsOllama = provider == "ollama"
	view.ProviderIsAnthropic = provider == "anthropic"
	view.ProviderIsOpenAI = provider == "openai"
	return view
}

// aiProviderSettingsSave validates the submission against whichever
// provider was selected and upserts the tenant's single
// AIProviderConnection row.
//
// The API key field is deliberately treated as "leave unchanged" when
// submitted blank and a key of the same provider is already stored —
// this page never echoes a stored key back into the form (see
// aiProviderSettingsPageView), so an empty field on a resubmission isn't
// distinguishable from "I have nothing new to say about the key" unless
// this handler makes that convention explicit. Switching provider away
// from whichever one a stored key belongs to always requires a fresh
// key: an Anthropic key and an OpenAI key are never interchangeable, so
// silently carrying one forward under a new provider would be a
// confusing, silent bug the first time that provider's API call failed
// with an auth error nobody could explain from the settings page alone.
func (h *Handler) aiProviderSettingsSave(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)

	def, id, existing, version, ok := h.aiProviderConnectionRecord(w, r, rc.TenantID)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid form submission: "+err.Error())
		return
	}
	provider := r.PostForm.Get("provider")
	baseURL := r.PostForm.Get("base_url")
	model := r.PostForm.Get("model")
	apiKeyPlain := r.PostForm.Get("api_key")

	fields, err := h.buildAIProviderFields(provider, baseURL, model, apiKeyPlain, existing)
	if err != nil {
		h.rerenderAIProviderSettingsWithError(w, locale, id, existing, err)
		return
	}

	if id == "" {
		rec, err := h.crud.Create(r.Context(), def, rc.TenantID, fields, rc.Actor)
		if err != nil {
			writeInternalError(w, "create AIProviderConnection", err)
			return
		}
		id = rec.ID
	} else {
		v := version
		if _, err := h.crud.Update(r.Context(), def, rc.TenantID, id, fields, &v, rc.Actor); err != nil {
			writeInternalError(w, "update AIProviderConnection", err)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := aiProviderSettingsTmpl.ExecuteTemplate(w, "page", h.aiProviderSettingsPageView(locale, id, fields)); err != nil {
		writeInternalError(w, "render AI provider settings result", err)
	}
}

// validateOllamaBaseURL rejects a base_url this server would otherwise
// POST to unconditionally on a tenant's behalf during every import
// preview (aiProviderFor -> aiassist.NewClient(baseURL, model) ->
// POST baseURL+"/api/generate"), an SSRF surface a malicious or
// compromised tenant admin could otherwise point at cloud metadata
// services (AWS/Azure/GCP all serve their instance-metadata API from the
// link-local 169.254.0.0/16 range, e.g. http://169.254.169.254/, often
// with no auth of its own). Only that link-local range is blocked, not
// private networks in general: "point at your own Ollama on a local
// machine or your own cloud" (this settings page's whole reason for
// existing — see this file's own doc comment) routinely means a
// loopback, LAN, or other RFC1918 address, none of which are link-local
// and all of which stay allowed here. A metadata endpoint reached only
// via a DNS name (not a literal link-local IP) isn't caught by this
// check — accepted as a residual gap consistent with this whole feature
// treating AI assistance as advisory/best-effort, not a hard security
// boundary the vendor call itself is trusted to enforce.
func validateOllamaBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url must be http or https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("base_url must include a host")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLinkLocalUnicast() {
		return fmt.Errorf("base_url may not point at a link-local address")
	}
	return nil
}

// buildAIProviderFields validates provider/baseURL/model/apiKeyPlain and
// returns the full field set to persist — Update replaces a record's data
// wholesale (crud.Engine.Update's own doc comment), so every field this
// connection should end up with must be present here, not just the ones
// that changed.
func (h *Handler) buildAIProviderFields(provider, baseURL, model, apiKeyPlain string, existing map[string]any) (map[string]any, error) {
	switch provider {
	case "ollama", "anthropic", "openai":
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	fields := map[string]any{"provider": provider, "model": model}

	if provider == "ollama" {
		if baseURL == "" {
			return nil, fmt.Errorf("base_url is required for the Ollama provider")
		}
		if err := validateOllamaBaseURL(baseURL); err != nil {
			return nil, err
		}
		fields["base_url"] = baseURL
		// api_key_encrypted intentionally omitted: an Ollama connection
		// carries no vendor API key regardless of what a prior
		// anthropic/openai configuration on this same tenant may have
		// stored, so a switch to Ollama always drops it.
		return fields, nil
	}

	// anthropic or openai: an API key is required, either freshly
	// submitted or already on file for this exact provider.
	if apiKeyPlain != "" {
		if !h.secretCryptor.Enabled() {
			return nil, fmt.Errorf("this deployment has no secret encryption key configured — an administrator must set SECRET_ENCRYPTION_KEY before an API key can be stored")
		}
		encrypted, err := h.secretCryptor.Encrypt(apiKeyPlain)
		if err != nil {
			return nil, fmt.Errorf("encrypt API key: %w", err)
		}
		fields["api_key_encrypted"] = encrypted
		return fields, nil
	}
	existingProvider, _ := existing["provider"].(string)
	existingKey, _ := existing["api_key_encrypted"].(string)
	if existingProvider == provider && existingKey != "" {
		fields["api_key_encrypted"] = existingKey
		return fields, nil
	}
	return nil, fmt.Errorf("an API key is required for the %s provider", provider)
}

// rerenderAIProviderSettingsWithError re-renders the form with saveErr's
// message shown inline and whatever was already on file left untouched —
// the same "never lose what the user was looking at, never silently
// swallow a validation failure" shape import.go's own mapping-error path
// established for this kernel's HTMX fragments.
func (h *Handler) rerenderAIProviderSettingsWithError(w http.ResponseWriter, locale, id string, existing map[string]any, saveErr error) {
	view := h.aiProviderSettingsPageView(locale, id, existing)
	view.SaveError = saveErr.Error()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := aiProviderSettingsTmpl.ExecuteTemplate(w, "page", view); err != nil {
		writeInternalError(w, "render AI provider settings error", err)
	}
}

// aiProviderSettingsClear deletes the tenant's AIProviderConnection
// override entirely, reverting to the platform default AI (aiprovider
// resolution falls back to h.ai — see import.go's aiProviderFor). A
// request for a tenant with no override to clear is a harmless no-op,
// not an error — the end state ("no override on file") is identical
// either way.
func (h *Handler) aiProviderSettingsClear(w http.ResponseWriter, r *http.Request) {
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	locale := localeFromRequest(w, r)

	def, id, _, _, ok := h.aiProviderConnectionRecord(w, r, rc.TenantID)
	if !ok {
		return
	}
	if id != "" {
		if err := h.crud.Delete(r.Context(), def, rc.TenantID, id, rc.Actor); err != nil {
			writeInternalError(w, "delete AIProviderConnection", err)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := aiProviderSettingsTmpl.ExecuteTemplate(w, "page", h.aiProviderSettingsPageView(locale, "", nil)); err != nil {
		writeInternalError(w, "render AI provider settings after clear", err)
	}
}

// --- view model ---

type aiProviderSettingsView struct {
	SaveHref  string
	ClearHref string

	Heading         string
	Intro           string
	ProviderLabel   string
	BaseURLLabel    string
	BaseURLHint     string
	ModelLabel      string
	APIKeyLabel     string
	APIKeyHintSet   string
	APIKeyHintUnset string
	SaveLabel       string
	ClearLabel      string
	DefaultNote     string

	HasOverride bool
	Provider    string
	BaseURL     string
	Model       string
	HasAPIKey   bool

	ProviderIsOllama    bool
	ProviderIsAnthropic bool
	ProviderIsOpenAI    bool

	SaveError string
}

var aiProviderSettingsTmpl = template.Must(template.New("ai-provider-settings").Parse(`
{{define "page"}}
<div class="uc-ai-provider-settings">
<h1>{{.Heading}}</h1>
<p>{{.Intro}}</p>
{{if not .HasOverride}}<p class="uc-ai-provider-default-note">{{.DefaultNote}}</p>{{end}}
{{if .SaveError}}<p class="uc-ai-provider-error">{{.SaveError}}</p>{{end}}

<form method="post" action="{{.SaveHref}}">
<label for="uc-ai-provider">{{.ProviderLabel}}</label>
<select id="uc-ai-provider" name="provider">
<option value="ollama" {{if .ProviderIsOllama}}selected{{end}}>Ollama</option>
<option value="anthropic" {{if .ProviderIsAnthropic}}selected{{end}}>Anthropic (Claude)</option>
<option value="openai" {{if .ProviderIsOpenAI}}selected{{end}}>OpenAI (ChatGPT)</option>
</select>

<label for="uc-ai-provider-base-url">{{.BaseURLLabel}}</label>
<input type="text" id="uc-ai-provider-base-url" name="base_url" value="{{.BaseURL}}">
<p class="uc-ai-provider-hint">{{.BaseURLHint}}</p>

<label for="uc-ai-provider-model">{{.ModelLabel}}</label>
<input type="text" id="uc-ai-provider-model" name="model" value="{{.Model}}" required>

<label for="uc-ai-provider-api-key">{{.APIKeyLabel}}</label>
<input type="password" id="uc-ai-provider-api-key" name="api_key" autocomplete="off">
<p class="uc-ai-provider-hint">{{if .HasAPIKey}}{{.APIKeyHintSet}}{{else}}{{.APIKeyHintUnset}}{{end}}</p>

<button type="submit">{{.SaveLabel}}</button>
</form>
{{if .HasOverride}}
<form method="post" action="{{.ClearHref}}">
<button type="submit">{{.ClearLabel}}</button>
</form>
{{end}}
</div>
{{end}}
`))
