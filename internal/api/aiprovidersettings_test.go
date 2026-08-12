package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/audit"
	"github.com/universaltill/universal-core/internal/kernel/authz"
	"github.com/universaltill/universal-core/internal/kernel/crud"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
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

// seedAIProviderAdmin grants "farshid" — the actor every test in this
// file authenticates as — the tenant_admin role, so aiProviderRequireAdmin
// (aiprovidersettings.go) lets it through. Every settings-route test below
// needs this since uc-infra#180's independent review: AIProviderConnection
// joined authz.systemOnlyWriteTypes, so RBAC's ordinary opt-in-by-default
// posture no longer applies to it — a fresh tenant in the bootstrap window
// (no roles configured at all, every other test fixture in this package's
// default state) resolves "farshid" to NOT an admin, not "no rules ->
// open" the way an ordinary entity type would.
func seedAIProviderAdmin(t *testing.T, db *sql.DB) {
	t.Helper()
	seedRBAC(t, db, map[string][]string{authz.AdminRoleCode: {"farshid"}}, nil)
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

// TestAIProviderSettings_Page_NonAdminForbidden confirms the tenant_admin
// gate (aiProviderRequireAdmin, uc-infra#180 independent review) is
// actually enforced: an authenticated but non-admin member reaching
// /settings/ai-provider gets the localized 403 page, not the settings
// form. This is the regression test for the gap the review caught —
// switching AIProviderConnection's writes to a raw, RBAC-bypassing
// engine (required once it joined authz.systemOnlyWriteTypes) had
// silently left these bare auth(...)-registered routes with no
// authorization check at all until this gate was added.
func TestAIProviderSettings_Page_NonAdminForbidden(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	// Deliberately NOT seedAIProviderAdmin: "farshid" stays an ordinary,
	// unprivileged member.

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("GET", "/settings/ai-provider", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin GET, got %d: %s", rec.Code, rec.Body.String())
	}

	saveRec := postAIProviderSave(mux, tenantID, "ollama", "http://attacker.example.com:11434", "llama3.2:3b", "")
	if saveRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin Save, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	clearReq := newRequest("POST", "/settings/ai-provider/clear", tenantID, "farshid", nil)
	clearRec := httptest.NewRecorder()
	mux.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin Clear, got %d: %s", clearRec.Code, clearRec.Body.String())
	}

	// And, just as important: none of the three denied attempts left a
	// row behind.
	listReq := newRequest("GET", "/api/records/AIProviderConnection", tenantID, "farshid", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), "attacker.example.com") {
		t.Fatalf("expected no record to have been created by a denied non-admin Save, got:\n%s", listRec.Body.String())
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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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

// TestAIProviderSettings_Save_AnthropicWithoutCryptorConfiguredFails
// confirms this deployment refuses to store a plaintext API key when no
// SECRET_ENCRYPTION_KEY exists to encrypt it with, rather than silently
// falling back to storing it unencrypted.
func TestAIProviderSettings_Save_AnthropicWithoutCryptorConfiguredFails(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

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
	seedAIProviderAdmin(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	req := newRequest("POST", "/settings/ai-provider/clear", tenantID, "farshid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAIProviderSettings_DirectAPIWriteIsDenied is the end-to-end proof
// for uc-infra#180's authz fix: an ordinary authenticated user can no
// longer POST straight to /api/records/AIProviderConnection and create a
// second, settings-page-invisible row — AIProviderConnection joined
// authz.systemOnlyWriteTypes specifically to close this, and this is the
// generic RBAC-guarded route (not internal/api/aiprovidersettings.go's
// own raw-engine bypass) that must now refuse it. Run for BOTH a
// tenant_admin and an ordinary member: systemOnlyWriteTypes denies this
// route unconditionally, so the admin case is the sharper proof — an
// admin can still reach the settings page (a different route) but must
// not be able to shortcut it via the generic API.
func TestAIProviderSettings_DirectAPIWriteIsDenied(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admin bool
	}{
		{"OrdinaryMember", false},
		{"TenantAdmin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t)
			withDevAuthEnabled(t)
			tenantID, db := newTestTenant(t, router)
			publishFoundation(t, db)
			if tc.admin {
				seedAIProviderAdmin(t, db)
			}

			mux := http.NewServeMux()
			testHandler(t, router).Routes(mux)

			body := []byte(`{"provider":"ollama","base_url":"http://attacker.example.com:11434","model":"llama3.2:3b","singleton_key":"singleton"}`)
			req := newRequest("POST", "/api/records/AIProviderConnection", tenantID, "farshid", body)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 denying a direct write to AIProviderConnection, got %d: %s", rec.Code, rec.Body.String())
			}

			// The denial must be real, not merely reported: no row landed.
			listReq := newRequest("GET", "/api/records/AIProviderConnection", tenantID, "farshid", nil)
			listRec := httptest.NewRecorder()
			mux.ServeHTTP(listRec, listReq)
			if strings.Contains(listRec.Body.String(), "attacker.example.com") {
				t.Fatalf("expected no record to exist after a denied direct write, got:\n%s", listRec.Body.String())
			}
		})
	}
}

// TestAIProviderSettings_SingletonEnforced_DuplicateCreateRejected is the
// crud-layer belt-and-suspenders proof: even calling the raw engine
// directly with the settings handler's own marker field set correctly (as
// if a second legitimate-looking write path existed), a second Create
// for the same tenant is rejected by the Unique constraint on
// singleton_key — not merely "the settings handler's upsert logic
// happens not to attempt it." Uses the raw engine deliberately (the same
// system-path bypass aiProviderSettingsSave itself uses, since
// AIProviderConnection is systemOnlyWriteTypes) to isolate the DB-level
// guarantee from RBAC. Also confirms the two informative negatives an
// independent review asked for: an omitted singleton_key is rejected at
// the entity-validation layer (Required, not even reaching the Unique
// check), and — the actual remaining exposure, since only a
// machine/service-token caller or a bug elsewhere could reach this in
// practice, per authz_test.go's own machine-bypass-ordering coverage — a
// DIFFERENT singleton_key value is NOT rejected: the Unique mechanism
// only ever protects exact marker collisions, never "any second row",
// which is exactly why authz.systemOnlyWriteTypes (not this constraint
// alone) is what makes "one legitimate row" hold.
func TestAIProviderSettings_SingletonEnforced_DuplicateCreateRejected(t *testing.T) {
	router := newTestRouter(t)
	_, db := newTestTenant(t, router)
	publishFoundation(t, db)

	engine := crud.NewEngine(db)
	def := foundation.AIProviderConnection()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	ctx := context.Background()

	if _, err := engine.Create(ctx, def, map[string]any{
		"provider": "ollama", "base_url": "http://first.example.com:11434", "model": "llama3.2:3b", "singleton_key": "singleton",
	}, actor); err != nil {
		t.Fatalf("create first AIProviderConnection row: %v", err)
	}

	// Same marker value: rejected by the Unique constraint.
	if _, err := engine.Create(ctx, def, map[string]any{
		"provider": "ollama", "base_url": "http://second.example.com:11434", "model": "llama3.2:3b", "singleton_key": "singleton",
	}, actor); !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected ErrUniqueConstraintViolation for a second row with the same singleton_key, got %v", err)
	}

	// Omitted marker: rejected at entity validation (Required), before
	// the Unique check is even reached.
	if _, err := engine.Create(ctx, def, map[string]any{
		"provider": "ollama", "base_url": "http://third.example.com:11434", "model": "llama3.2:3b",
	}, actor); err == nil {
		t.Fatal("expected an error for a row missing singleton_key entirely")
	}

	// Different marker value: NOT rejected — the residual exposure this
	// test's own doc comment documents. This is expected/current
	// behavior, not a bug to fix here: closing it is authz's job
	// (systemOnlyWriteTypes), not this Definition's Unique declaration's.
	if _, err := engine.Create(ctx, def, map[string]any{
		"provider": "ollama", "base_url": "http://fourth.example.com:11434", "model": "llama3.2:3b", "singleton_key": "not-the-real-marker",
	}, actor); err != nil {
		t.Fatalf("expected a row with a DIFFERENT singleton_key to succeed (documenting the mechanism's real boundary), got %v", err)
	}
}

// TestAIProviderSettings_UpgradePath_PreV2RowSelfHealsOnNextSave confirms
// the migration story an independent review asked to see tested: a row
// created before uc-infra#180 (Version 1, no singleton_key at all —
// simulated here via the raw engine bypassing entity.ValidateRecord's
// Required check the same way a row written against the still-published
// v1 Definition would have) is not permanently exempt from the Unique
// constraint. The very next Save through the real handler must both (a)
// update that same pre-existing row (not silently create a second one
// alongside it) and (b) backfill singleton_key onto it, so a subsequent
// direct-engine attempt at a genuine second row is rejected exactly as
// TestAIProviderSettings_SingletonEnforced_DuplicateCreateRejected proves
// for a row that started out v2-shaped.
func TestAIProviderSettings_UpgradePath_PreV2RowSelfHealsOnNextSave(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	seedAIProviderAdmin(t, db)

	def := foundation.AIProviderConnection()
	actor := audit.Actor{Type: audit.ActorHuman, ID: "farshid"}
	ctx := context.Background()

	// Simulate a pre-Version-2 row (no singleton_key at all) via the
	// record repo directly, bypassing crud.Engine.Create's
	// entity.ValidateRecord call — the only way to reproduce a shape
	// that predates a field that's Required in the CURRENT Definition.
	// A real such row would have been written before singleton_key
	// existed at all; production code never has a legitimate reason to
	// skip validation itself.
	preV2, err := data.NewRecordRepo(db).Create(ctx, def.EntityType, map[string]any{
		"provider": "ollama", "base_url": "http://pre-migration.example.com:11434", "model": "llama3.2:3b",
	})
	if err != nil {
		t.Fatalf("simulate a pre-v2 row with no singleton_key: %v", err)
	}

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	rec := postAIProviderSave(mux, tenantID, "ollama", "http://post-migration.example.com:11434", "llama3.2:3b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on save over a pre-v2 row, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "http://post-migration.example.com:11434") {
		t.Fatalf("expected the new base_url to be saved, got:\n%s", body)
	}

	// Exactly one row must exist — the pre-v2 row was UPDATED, not left
	// behind alongside a new one.
	rows, err := crud.NewEngine(db).List(ctx, def)
	if err != nil {
		t.Fatalf("list AIProviderConnection after save: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 AIProviderConnection row after saving over a pre-v2 row, got %d", len(rows))
	}
	if rows[0].ID != preV2.ID {
		t.Fatalf("expected the pre-v2 row (%s) to have been updated in place, got a different id %s", preV2.ID, rows[0].ID)
	}
	if rows[0].Data["singleton_key"] != aiProviderConnectionSingletonKey {
		t.Fatalf("expected singleton_key to be backfilled onto the pre-v2 row, got %v", rows[0].Data["singleton_key"])
	}

	// And the constraint now actually holds: a genuine second row is
	// rejected, exactly as it would be for a row that started out
	// v2-shaped.
	if _, err := crud.NewEngine(db).Create(ctx, def, map[string]any{
		"provider": "ollama", "base_url": "http://intruder.example.com:11434", "model": "llama3.2:3b", "singleton_key": aiProviderConnectionSingletonKey,
	}, actor); !errors.Is(err, crud.ErrUniqueConstraintViolation) {
		t.Fatalf("expected the backfilled constraint to reject a genuine second row, got %v", err)
	}
}

// TestAIProviderSettings_ConcurrentSaves_ProduceExactlyOneRow is the
// real-race proof for the recovery path an independent review asked for
// (createOrRecoverAIProviderConnection, aiprovidersettings.go): two
// genuinely concurrent first-time Saves for the same tenant — the actual
// scenario the review named, a double form submit or two browser tabs —
// must not have either one surface a raw, internal-marker-naming error;
// both must return 200, and exactly one AIProviderConnection row must
// exist afterward.
//
// Deliberately n=2, not a larger stress-test fan-out: at higher
// concurrency, requests that observe an ALREADY-existing row (not a
// Create conflict) can also race each other on the plain
// aiProviderSettingsSave Update branch — ordinary optimistic-locking
// contention (data.ErrVersionConflict) every Update in this kernel has,
// unmodified by and out of scope for this fix, which only recovers the
// Create-conflict path this test exercises.
func TestAIProviderSettings_ConcurrentSaves_ProduceExactlyOneRow(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishFoundation(t, db)
	seedAIProviderAdmin(t, db)

	mux := http.NewServeMux()
	testHandler(t, router).Routes(mux)

	const n = 2
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postAIProviderSave(mux, tenantID, "ollama", "http://race.example.com:11434", "llama3.2:3b", "")
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("save %d: expected 200, got %d: %s", i, code, bodies[i])
		}
		if strings.Contains(bodies[i], "singleton_key") {
			t.Fatalf("save %d: expected the internal marker field to never surface to the caller, got:\n%s", i, bodies[i])
		}
	}

	rows, err := crud.NewEngine(db).List(context.Background(), foundation.AIProviderConnection())
	if err != nil {
		t.Fatalf("list AIProviderConnection after concurrent saves: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 AIProviderConnection row after %d concurrent saves, got %d", n, len(rows))
	}
}
