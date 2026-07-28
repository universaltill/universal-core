package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/svcauth"
	"github.com/universaltill/universal-core/internal/tenantdb"
)

// testHandlerWithSvc is testHandler plus a svcauth.Authenticator — kept
// separate rather than adding a svc parameter to testHandler itself,
// same reasoning testHandlerWithAI/testHandlerWithSpeech/
// testHandlerWithSecretCryptor already establish (every other test in
// this package stays exactly as it was: machine-to-machine auth
// disabled, matching a deployment with no SVC_INTROSPECTION_CLIENT_ID
// configured).
func testHandlerWithSvc(t *testing.T, router *tenantdb.Router, svc *svcauth.Authenticator) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(router, catalog, nil, svc, nil, nil, nil)
}

// fakeZitadelServer fakes just enough of Zitadel for svcauth.New's own
// discovery (a minimal .well-known/openid-configuration) plus token
// introspection: responses is keyed by the exact bearer token a test
// presents, so a single fake server can answer differently per token —
// the same way real Zitadel would for two different connector
// credentials. An unrecognized token gets "active": false, matching how
// real introspection answers a token it's never heard of, rather than
// panicking the test.
func fakeZitadelServer(t *testing.T, responses map[string]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/oauth/v2/authorize",
			"token_endpoint":                        srv.URL + "/oauth/v2/token",
			"introspection_endpoint":                srv.URL + "/oauth/v2/introspect",
			"jwks_uri":                              srv.URL + "/oauth/v2/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/oauth/v2/introspect", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("fake zitadel: parse introspection form: %v", err)
		}
		resp, ok := responses[r.PostForm.Get("token")]
		if !ok {
			resp = map[string]any{"active": false}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tenantIntegrationResponse(sub, orgID string) map[string]any {
	return map[string]any{
		"active": true,
		"sub":    sub,
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"tenant_integration": map[string]any{orgID: "acme.id.universaltill.com"},
		},
	}
}

// bearerTenant provisions a real tenant (via router.Create, same as
// newTestTenant) and links it to a freshly-made-up Zitadel org id —
// what a connector's own Bearer token needs to resolve to a tenant.
func bearerTenant(t *testing.T, router *tenantdb.Router, control *sql.DB) (tenantID, orgID string, db *sql.DB) {
	t.Helper()
	tenantID, db = newTestTenant(t, router)
	orgID = "zitadel-org-" + tenantID
	if err := data.NewTenantRepo(control).SetZitadelOrgID(context.Background(), tenantID, orgID); err != nil {
		t.Fatalf("link zitadel org: %v", err)
	}
	return tenantID, orgID, db
}

// TestAPI_BearerToken_CreateAndListRecords is the core end-to-end proof
// this whole mechanism exists for: a connector authenticates with
// nothing but a real Bearer access token — no X-Tenant-ID/X-Actor-ID
// headers at all — and both tenant and actor resolve correctly from the
// token itself, all the way through a real create + list against the
// generic records API.
func TestAPI_BearerToken_CreateAndListRecords(t *testing.T) {
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	_, orgID, db := bearerTenant(t, router, control)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	const token = "real-connector-token"
	zitadel := fakeZitadelServer(t, map[string]map[string]any{
		token: tenantIntegrationResponse("connector-1", orgID),
	})
	svc, err := svcauth.New(context.Background(), svcauth.Config{
		IssuerURL: zitadel.URL, ClientID: "introspection-client", ClientSecret: "introspection-secret",
	}, data.NewTenantRepo(control))
	if err != nil {
		t.Fatalf("svcauth.New: %v", err)
	}

	mux := http.NewServeMux()
	testHandlerWithSvc(t, router, svc).Routes(mux)

	createReq := httptest.NewRequest("POST", "/api/records/Vendor", strings.NewReader(`{"name":"Acme Textiles"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating a Vendor via Bearer token, got %d: %s", createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest("GET", "/api/records/Vendor", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 listing Vendor via Bearer token, got %d: %s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "Acme Textiles") {
		t.Fatalf("expected the created record in the Bearer-token list response, got:\n%s", listRec.Body.String())
	}

	// The audit trail's actor defaults to the service credential's own
	// stable identity (svc:<sub>) when no On-Behalf-Of header asserts a
	// specific human — see TestAPI_BearerToken_OnBehalfOfAttributesAudit
	// for the override.
	var actorID, actorType string
	if err := db.QueryRow(`SELECT actor_id, actor_type FROM audit_log WHERE entity_type = 'Vendor' AND action = 'create'`).Scan(&actorID, &actorType); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if actorID != "svc:connector-1" {
		t.Fatalf("audit actor_id = %q, want %q", actorID, "svc:connector-1")
	}
	if actorType != "human" {
		t.Fatalf("audit actor_type = %q, want \"human\" — a service credential is an auth mechanism, never a third actor bucket", actorType)
	}
}

// TestAPI_BearerToken_OnBehalfOfAttributesAudit confirms a connector can
// assert which specific human actually initiated a mutation (e.g. the
// till operator who completed a sale) — see svcauth.OnBehalfOfHeader's
// own doc comment on the trust boundary this represents.
func TestAPI_BearerToken_OnBehalfOfAttributesAudit(t *testing.T) {
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	_, orgID, db := bearerTenant(t, router, control)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())

	const token = "real-connector-token"
	zitadel := fakeZitadelServer(t, map[string]map[string]any{
		token: tenantIntegrationResponse("connector-1", orgID),
	})
	svc, err := svcauth.New(context.Background(), svcauth.Config{
		IssuerURL: zitadel.URL, ClientID: "introspection-client", ClientSecret: "introspection-secret",
	}, data.NewTenantRepo(control))
	if err != nil {
		t.Fatalf("svcauth.New: %v", err)
	}

	mux := http.NewServeMux()
	testHandlerWithSvc(t, router, svc).Routes(mux)

	createReq := httptest.NewRequest("POST", "/api/records/Vendor", strings.NewReader(`{"name":"Acme Textiles"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set(svcauth.OnBehalfOfHeader, "cashier-42")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	var actorID string
	if err := db.QueryRow(`SELECT actor_id FROM audit_log WHERE entity_type = 'Vendor' AND action = 'create'`).Scan(&actorID); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if actorID != "cashier-42" {
		t.Fatalf("audit actor_id = %q, want the On-Behalf-Of value %q", actorID, "cashier-42")
	}
}

// TestAPI_BearerToken_TenantIsolation confirms two different connector
// credentials, for two different tenants, each backed by its own real
// per-tenant database (ADR-0003), never cross — the same cross-tenant-
// leak class of bug every other auth path in this kernel is held to.
func TestAPI_BearerToken_TenantIsolation(t *testing.T) {
	control, dsn := freshControlDB(t)
	router, err := tenantdb.NewRouter(control, dsn)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { router.Close() })

	_, orgA, dbA := bearerTenant(t, router, control)
	_, orgB, dbB := bearerTenant(t, router, control)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	publishEntityAndForm(t, dbB, vendorEntityDef(), vendorFormDef())

	const tokenA, tokenB = "connector-token-a", "connector-token-b"
	zitadel := fakeZitadelServer(t, map[string]map[string]any{
		tokenA: tenantIntegrationResponse("connector-a", orgA),
		tokenB: tenantIntegrationResponse("connector-b", orgB),
	})
	svc, err := svcauth.New(context.Background(), svcauth.Config{
		IssuerURL: zitadel.URL, ClientID: "introspection-client", ClientSecret: "introspection-secret",
	}, data.NewTenantRepo(control))
	if err != nil {
		t.Fatalf("svcauth.New: %v", err)
	}

	mux := http.NewServeMux()
	testHandlerWithSvc(t, router, svc).Routes(mux)

	createReq := httptest.NewRequest("POST", "/api/records/Vendor", strings.NewReader(`{"name":"Tenant A Only Vendor"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+tokenA)
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating tenant A's Vendor, got %d: %s", createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest("GET", "/api/records/Vendor", nil)
	listReq.Header.Set("Authorization", "Bearer "+tokenB)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), "Tenant A Only Vendor") {
		t.Fatalf("expected tenant B's connector token to never see tenant A's data, got:\n%s", listRec.Body.String())
	}
}
