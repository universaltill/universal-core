package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/secretcrypt"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// testHandlerWithSecretCryptor is testHandler plus a secretcrypt.Cryptor
// — kept separate rather than adding a cryptor parameter to testHandler
// itself, same reasoning testHandlerWithAI/testHandlerWithSpeech already
// establish (every other test in this package stays exactly as it was:
// secret encryption disabled, matching a deployment with no
// SECRET_ENCRYPTION_KEY configured).
func testHandlerWithSecretCryptor(t *testing.T, router *tenantdb.Router, cryptor *secretcrypt.Cryptor) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(router, catalog, nil, nil, nil, nil, cryptor)
}

func testCryptor(t *testing.T) *secretcrypt.Cryptor {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	c, err := secretcrypt.NewCryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	return c
}

func aiProviderSaveForm(provider, baseURL, model, apiKey string) []byte {
	form := "provider=" + provider + "&base_url=" + baseURL + "&model=" + model + "&api_key=" + apiKey
	return []byte(form)
}

func postAIProviderSave(mux *http.ServeMux, tenantID, provider, baseURL, model, apiKey string) *httptest.ResponseRecorder {
	req := newRequest("POST", "/settings/ai-provider", tenantID, "farshid", aiProviderSaveForm(provider, baseURL, model, apiKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAIProviderSettings_Page_RequiresAuth(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := httptest.NewRequest("GET", "/settings/ai-provider", nil) // no auth headers
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAIProviderSettings_Page_DefaultsToPlatformAI confirms a tenant with
// no AIProviderConnection row yet sees the "using the platform default"
// note, not an error — this is the expected, common state for every
// tenant until they explicitly opt into BYOK.
func TestAIProviderSettings_Page_DefaultsToPlatformAI(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/settings/ai-provider", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "currently using the platform") {
		t.Fatalf("expected the default-AI note, got:\n%s", body)
	}
	if strings.Contains(body, "Revert to platform default") {
		t.Fatalf("expected no clear/revert button with no override configured, got:\n%s", body)
	}
}

// TestAIProviderSettings_Save_OllamaRequiresBaseURL confirms
// buildAIProviderFields' Ollama branch is actually enforced end-to-end,
// not just at the unit level.
func TestAIProviderSettings_Save_OllamaRequiresBaseURL(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postAIProviderSave(mux, tenantID, "ollama", "", "llama3.2:3b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error, not an HTTP failure), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "base_url is required") {
		t.Fatalf("expected an inline base_url validation error, got:\n%s", rec.Body.String())
	}
}

// TestAIProviderSettings_Save_OllamaSucceeds is the core create-then-read
// proof for the Ollama path: a tenant pointing at their own Ollama server
// (not the platform default) round-trips through Save and back onto the
// page correctly, with no API key ever in play.
func TestAIProviderSettings_Save_OllamaSucceeds(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postAIProviderSave(mux, tenantID, "ollama", "http://my-own-ollama.example.com:11434", "llama3.2:3b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "http://my-own-ollama.example.com:11434") {
		t.Fatalf("expected the saved base_url to be reflected back, got:\n%s", body)
	}
	if !strings.Contains(body, "Revert to platform default") {
		t.Fatalf("expected a clear/revert button once an override exists, got:\n%s", body)
	}

	// A second GET (fresh page load, not the Save response) must show the
	// same thing was actually persisted, not just echoed in this one
	// response.
	getReq := newRequest("GET", "/settings/ai-provider", tenantID, "farshid", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "http://my-own-ollama.example.com:11434") {
		t.Fatalf("expected the override to persist across requests, got:\n%s", getRec.Body.String())
	}
}

// TestAIProviderSettings_Save_AnthropicWithoutCryptorConfiguredFails
// confirms this deployment refuses to store a plaintext API key when no
// SECRET_ENCRYPTION_KEY exists to encrypt it with, rather than silently
// falling back to storing it unencrypted.
// TestAIProviderSettings_Save_OllamaRejectsLinkLocalBaseURL is
// validateOllamaBaseURL's own end-to-end proof: a tenant pointing
// base_url at the well-known cloud-metadata address must be rejected,
// not silently accepted and later fetched server-side on every import
// preview.
func TestAIProviderSettings_Save_OllamaRejectsLinkLocalBaseURL(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postAIProviderSave(mux, tenantID, "ollama", "http://169.254.169.254/latest/meta-data/", "llama3.2:3b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error, not an HTTP failure), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "link-local") {
		t.Fatalf("expected an inline link-local base_url validation error, got:\n%s", rec.Body.String())
	}
}

func TestAIProviderSettings_Save_AnthropicWithoutCryptorConfiguredFails(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux) // no secret cryptor

	rec := postAIProviderSave(mux, tenantID, "anthropic", "", "claude-sonnet-5", "sk-ant-fake-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SECRET_ENCRYPTION_KEY") {
		t.Fatalf("expected an inline error naming the missing configuration, got:\n%s", rec.Body.String())
	}
}

// TestAIProviderSettings_Save_AnthropicEncryptsKeyAndNeverEchoesIt is the
// core BYOK proof for a real vendor provider: the key reaches storage
// encrypted (never appears verbatim anywhere in the response, and isn't
// the ciphertext-equals-plaintext no-op either), and the settings page
// never renders it back out, only whether one is set.
func TestAIProviderSettings_Save_AnthropicEncryptsKeyAndNeverEchoesIt(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)

	const plainKey = "sk-ant-api03-super-secret-value-should-never-appear"
	rec := postAIProviderSave(mux, tenantID, "anthropic", "", "claude-sonnet-5", plainKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plainKey) {
		t.Fatalf("expected the plaintext API key to never appear in the response, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already saved") {
		t.Fatalf("expected the page to report a key is already saved, got:\n%s", rec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/AIProviderConnection", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 listing AIProviderConnection records, got %d: %s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), plainKey) {
		t.Fatalf("expected the stored record to never contain the plaintext key, got:\n%s", listRec.Body.String())
	}
}

// TestAIProviderSettings_Save_BlankAPIKeyKeepsExistingForSameProvider
// confirms the "leave blank to keep it" convention the settings page's
// own hint text promises actually holds: resubmitting with a new model
// but no api_key must not silently drop the previously stored key.
func TestAIProviderSettings_Save_BlankAPIKeyKeepsExistingForSameProvider(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)

	if rec := postAIProviderSave(mux, tenantID, "anthropic", "", "claude-sonnet-5", "sk-ant-fake-key"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on first save, got %d: %s", rec.Code, rec.Body.String())
	}

	rec := postAIProviderSave(mux, tenantID, "anthropic", "", "claude-opus-4-8", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on second save with a blank key, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "claude-opus-4-8") {
		t.Fatalf("expected the new model to be saved, got:\n%s", body)
	}
	if !strings.Contains(body, "already saved") {
		t.Fatalf("expected the existing key to still be reported as saved, got:\n%s", body)
	}
}

// TestAIProviderSettings_Save_BlankAPIKeySwitchingProviderFails confirms
// a key stored under one vendor is never silently carried forward under
// a different one — buildAIProviderFields' own doc comment on why that
// would be a confusing, silent bug.
func TestAIProviderSettings_Save_BlankAPIKeySwitchingProviderFails(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandlerWithSecretCryptor(t, router, testCryptor(t)).Routes(mux)

	if rec := postAIProviderSave(mux, tenantID, "anthropic", "", "claude-sonnet-5", "sk-ant-fake-key"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on first save, got %d: %s", rec.Code, rec.Body.String())
	}

	rec := postAIProviderSave(mux, tenantID, "openai", "", "gpt-5.6-mini", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API key is required for the openai provider") {
		t.Fatalf("expected an inline error requiring a fresh key for the new provider, got:\n%s", rec.Body.String())
	}
}

func TestAIProviderSettings_Save_UnknownProviderIsRejected(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postAIProviderSave(mux, tenantID, "gemini", "", "gemini-pro", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown provider") {
		t.Fatalf("expected an inline unknown-provider error, got:\n%s", rec.Body.String())
	}
}

// TestAIProviderSettings_Clear_RemovesOverride is the revert-to-default
// round trip: after Clear, the page must show the default-AI note again
// and the underlying record must actually be gone (not just hidden),
// confirmed via the generic /api/records listing.
func TestAIProviderSettings_Clear_RemovesOverride(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	if rec := postAIProviderSave(mux, tenantID, "ollama", "http://my-own-ollama.example.com:11434", "llama3.2:3b", ""); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on save, got %d: %s", rec.Code, rec.Body.String())
	}

	clearReq := newRequest("POST", "/settings/ai-provider/clear", tenantID, "farshid", nil)
	clearRec := httptest.NewRecorder()
	mux.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", clearRec.Code, clearRec.Body.String())
	}
	if !strings.Contains(clearRec.Body.String(), "currently using the platform") {
		t.Fatalf("expected the default-AI note after clearing, got:\n%s", clearRec.Body.String())
	}

	listReq := newRequest("GET", "/api/records/AIProviderConnection", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), "my-own-ollama") {
		t.Fatalf("expected the override record to be gone after Clear, got:\n%s", listRec.Body.String())
	}
}

// TestAIProviderSettings_Clear_WithNoOverrideIsHarmlessNoOp confirms
// Clear on a tenant with nothing to clear doesn't error — the end state
// ("no override on file") is identical either way, per
// aiProviderSettingsClear's own doc comment.
func TestAIProviderSettings_Clear_WithNoOverrideIsHarmlessNoOp(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/settings/ai-provider/clear", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
