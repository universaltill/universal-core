package svcauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// linkedTenant provisions a control-plane tenant row and links it to a
// freshly-made-up Zitadel org id — mirrors internal/webauth's own
// tenant_resolution_test.go fixture exactly (same table, same
// SetZitadelOrgID call); svcauth doesn't need a tenant's own per-tenant
// database (unlike internal/tenantdb.Router's tests), only the
// control-plane row GetByZitadelOrgID reads.
func linkedTenant(t *testing.T, db *sql.DB) (tenantID, orgID string) {
	t.Helper()
	ctx := context.Background()
	tenants := data.NewTenantRepo(db)
	tenantID, err := tenants.Create(ctx, "Svcauth Test Tenant", "eu-west")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	orgID = "zitadel-org-" + tenantID
	if err := tenants.SetZitadelOrgID(ctx, tenantID, orgID); err != nil {
		t.Fatalf("link zitadel org: %v", err)
	}
	return tenantID, orgID
}

// introspectionServer fakes Zitadel's POST /oauth/v2/introspect: every
// call returns response verbatim as JSON, regardless of what token/auth
// was actually presented — good enough for exercising Guard's own
// claims-handling logic, which is what these tests are actually proving
// (not whatever real Zitadel does server-side).
func introspectionServer(t *testing.T, response map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func activeResponse(sub, orgID string) map[string]any {
	return map[string]any{
		"active": true,
		"sub":    sub,
		zitadelProjectRolesClaim: map[string]any{
			tenantIntegrationRole: map[string]any{orgID: "acme.id.universaltill.com"},
		},
	}
}

func TestGuard_DisabledPassesThrough(t *testing.T) {
	var a *Authenticator // nil — the zero value Routes() gets when svcauth.New was never called
	called := false
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("disabled Guard must pass through to next, even with an Authorization header present")
	}
}

func TestGuard_NoBearerHeaderPassesThrough(t *testing.T) {
	a := &Authenticator{enabled: true, httpClient: http.DefaultClient}
	called := false
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/records/Item", nil))
	if !called {
		t.Fatal("Guard with no Authorization header must pass through to next (leaves room for DevAuth/webauth)")
	}
}

func TestGuard_ValidTokenPopulatesRequestContext(t *testing.T) {
	db := testDB(t)
	tenantID, orgID := linkedTenant(t, db)
	srv := introspectionServer(t, activeResponse("machine-user-1", orgID))

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}

	var gotRC httpx.RequestContext
	var gotOK bool
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRC, gotOK = httpx.FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer real-connector-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("expected a RequestContext to be attached")
	}
	if gotRC.TenantID != tenantID {
		t.Fatalf("TenantID = %q, want %q", gotRC.TenantID, tenantID)
	}
	if gotRC.Actor.ID != "svc:machine-user-1" {
		t.Fatalf(`Actor.ID = %q, want "svc:machine-user-1"`, gotRC.Actor.ID)
	}
	if gotRC.Actor.Type != "human" {
		t.Fatalf("Actor.Type = %q, want human — a service credential is an auth mechanism, never a third actor bucket", gotRC.Actor.Type)
	}
}

// TestGuard_OnBehalfOfOverridesActor proves the connector's own
// assertion of which human actually initiated the mutation (e.g. the
// till operator who completed a sale) wins over the service
// credential's own default identity.
func TestGuard_OnBehalfOfOverridesActor(t *testing.T) {
	db := testDB(t)
	_, orgID := linkedTenant(t, db)
	srv := introspectionServer(t, activeResponse("machine-user-1", orgID))

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}

	var gotActorID string
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, _ := httpx.FromContext(r.Context())
		gotActorID = rc.Actor.ID
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer real-connector-token")
	req.Header.Set(OnBehalfOfHeader, "cashier-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotActorID != "cashier-42" {
		t.Fatalf("Actor.ID = %q, want the On-Behalf-Of value %q", gotActorID, "cashier-42")
	}
}

func TestGuard_InactiveTokenIs401(t *testing.T) {
	db := testDB(t)
	srv := introspectionServer(t, map[string]any{"active": false})

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be called for an inactive token")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer revoked-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an inactive token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGuard_MissingTenantIntegrationRoleIs401 is the regression test for
// the whole reason tenant_integration is its own role rather than
// reusing tenant_member: a token that only carries tenant_member (e.g.
// a human's id_token/access_token, replayed here as a Bearer token by
// mistake) must be rejected by the machine-to-machine path, not silently
// accepted as if it were a connector's own credential.
func TestGuard_MissingTenantIntegrationRoleIs401(t *testing.T) {
	db := testDB(t)
	_, orgID := linkedTenant(t, db)
	srv := introspectionServer(t, map[string]any{
		"active": true,
		"sub":    "a-human-user",
		zitadelProjectRolesClaim: map[string]any{
			"tenant_member": map[string]any{orgID: "acme.id.universaltill.com"},
		},
	})

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be called for a token with no tenant_integration role")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer a-human-id-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token with no tenant_integration role, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGuard_UnlinkedOrgIs401(t *testing.T) {
	db := testDB(t)
	srv := introspectionServer(t, activeResponse("machine-user-1", "some-org-nobody-linked"))

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be called for an org with no linked tenant")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer real-connector-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unlinked org, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGuard_IntrospectionEndpointDownIsInternalError confirms a
// transport-level failure calling Zitadel itself (as opposed to Zitadel
// answering "this token is invalid") surfaces as a generic 500, not a
// 401 — the caller's credential might be perfectly good; Universal Core
// just couldn't check it.
func TestGuard_IntrospectionEndpointDownIsInternalError(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close() // closed immediately: guarantees the request fails at the transport level

	a := &Authenticator{
		enabled:               true,
		introspectionEndpoint: srv.URL,
		httpClient:            srv.Client(),
		tenants:               data.NewTenantRepo(db),
	}
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be called when introspection itself fails")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/records/Item", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when introspection itself is unreachable, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNew_DiscoversIntrospectionEndpoint is the one test that exercises
// New itself (every other test above constructs an Authenticator
// directly against a fake introspection server, same pattern
// internal/webauth's own Guard tests use) — proving the OIDC-discovery
// wiring that finds introspection_endpoint in the first place actually
// works end to end against a real discovery document shape.
func TestNew_DiscoversIntrospectionEndpoint(t *testing.T) {
	var issuerURL string
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuerURL,
			"authorization_endpoint":                issuerURL + "/oauth/v2/authorize",
			"token_endpoint":                        issuerURL + "/oauth/v2/token",
			"introspection_endpoint":                issuerURL + "/oauth/v2/introspect",
			"jwks_uri":                              issuerURL + "/oauth/v2/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	defer discovery.Close()
	issuerURL = discovery.URL

	a, err := New(context.Background(), Config{
		IssuerURL:    issuerURL,
		ClientID:     "introspection-client",
		ClientSecret: "introspection-secret",
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !a.Enabled() {
		t.Fatal("expected Enabled() once discovery succeeds")
	}
	if a.introspectionEndpoint != issuerURL+"/oauth/v2/introspect" {
		t.Fatalf("introspectionEndpoint = %q, want %q", a.introspectionEndpoint, issuerURL+"/oauth/v2/introspect")
	}
}

func TestConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"zero value", Config{}, false},
		{"missing secret", Config{IssuerURL: "https://id.universaltill.com", ClientID: "x"}, false},
		{"fully configured", Config{IssuerURL: "https://id.universaltill.com", ClientID: "x", ClientSecret: "y"}, true},
	}
	for _, c := range cases {
		if got := c.cfg.Enabled(); got != c.want {
			t.Errorf("%s: Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}
