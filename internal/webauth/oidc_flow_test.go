package webauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/universaltill/universal-core/internal/data"
)

func testCookieKeyB64(t *testing.T) string {
	t.Helper()
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key[:])
}

const testClientID = "test-client"

// ==== New ====

// TestNew_DisabledWhenNotConfigured confirms an unconfigured deployment
// gets a disabled Authenticator with no error and no network call —
// Config.Enabled() gates everything else in New before any discovery is
// attempted.
func TestNew_DisabledWhenNotConfigured(t *testing.T) {
	a, err := New(context.Background(), Config{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Enabled() {
		t.Fatal("expected a disabled Authenticator")
	}
}

func TestNew_InvalidCookieKeyErrors(t *testing.T) {
	_, err := New(context.Background(), Config{
		ClientID: testClientID, RedirectURL: "https://erp.example/ui/auth/callback",
		CookieKeyB64: "not valid base64 at all!!",
	}, nil)
	if err == nil {
		t.Fatal("expected an error for an invalid cookie key")
	}
}

func TestNew_DiscoveryFailureErrors(t *testing.T) {
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	unreachable.Close() // guarantees discovery fails at the transport level

	_, err := New(context.Background(), Config{
		IssuerURL: unreachable.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, nil)
	if err == nil {
		t.Fatal("expected an error when the provider is unreachable")
	}
}

// TestNew_DiscoversProviderAndEndSession is the real end-to-end proof
// that discovery + JWKS wiring works against an actual OIDC discovery
// document shape (same "one test proves New itself" pattern as
// internal/svcauth's own TestNew_DiscoversIntrospectionEndpoint).
func TestNew_DiscoversProviderAndEndSession(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !a.Enabled() {
		t.Fatal("expected Enabled() once discovery succeeds")
	}
	if a.endSess != p.URL+"/oidc/v1/end_session" {
		t.Fatalf("expected end_session_endpoint to be discovered, got %q", a.endSess)
	}
}

// ==== handleLogin ====

func TestHandleLogin_SetsFlowCookieAndRedirectsToProvider(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/login?returnTo=/forms/Party/new", nil)
	a.handleLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), p.URL+"/oauth/v2/authorize") {
		t.Fatalf("expected a redirect to the provider's authorize endpoint, got %q", loc)
	}
	q := loc.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("expected response_type=code, got %q", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("expected PKCE S256 challenge, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("expected non-empty state and nonce")
	}

	var flowCookieSet *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == flowCookie {
			flowCookieSet = c
		}
	}
	if flowCookieSet == nil {
		t.Fatal("expected the flow cookie to be set")
	}
	if !flowCookieSet.HttpOnly {
		t.Fatal("expected the flow cookie to be HttpOnly")
	}

	// The sealed flow cookie must actually decode to the same state/nonce
	// the redirect URL carries — proving handleLogin's cookie and its
	// own redirect are for the same login attempt, not independently
	// generated values that happen to both look valid.
	fs, err := a.sealer.openFlow(flowCookieSet.Value)
	if err != nil {
		t.Fatalf("openFlow: %v", err)
	}
	if fs.State != q.Get("state") || fs.Nonce != q.Get("nonce") {
		t.Fatal("flow cookie state/nonce must match the redirect URL's own")
	}
	if fs.ReturnTo != "/forms/Party/new" {
		t.Fatalf("expected ReturnTo to carry through from ?returnTo=, got %q", fs.ReturnTo)
	}
}

// ==== handleCallback ====

// newTestAuthenticator builds a real Authenticator against a fake
// provider, with tenants wired to a real Postgres control-plane database
// — handleCallback's happy path genuinely resolves a Zitadel org id to a
// tenant via data.TenantRepo, so this needs the genuine repository, not a
// stub. Only for tests that actually reach that lookup (the happy path,
// and the unlinked-org case) — see newTestAuthenticatorNoDB for every
// other callback test, which fails before ever touching a.tenants and
// shouldn't need Postgres to run at all (independent review, 2026-07-29:
// the original version of this file made every callback test — including
// the real-RS256-signature-verification ones, the actual point of this
// batch of work — skip without TEST_DATABASE_URL, unnecessarily
// narrowing the proof to environments with a database configured).
func newTestAuthenticator(t *testing.T, p *fakeOIDCProvider) (*Authenticator, *data.TenantRepo) {
	t.Helper()
	db := testDB(t)
	tenants := data.NewTenantRepo(db)
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
		SessionTTL: time.Hour,
	}, tenants)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, tenants
}

// newTestAuthenticatorNoDB is newTestAuthenticator without a database —
// for callback tests that fail (wrong nonce/state, invalid signature, no
// id_token, token endpoint down) or hit the no-org-claim case, all of
// which return before handleCallback ever calls a.tenants.GetByZitadelOrgID.
// A nil *data.TenantRepo would panic if one of these tests' assumptions
// about where the handler stops ever became wrong — which is exactly the
// point: it turns "this test silently started exercising the DB lookup
// path" into a nil-pointer test failure instead of a passing test that
// quietly needs Postgres it didn't ask for.
func newTestAuthenticatorNoDB(t *testing.T, p *fakeOIDCProvider) *Authenticator {
	t.Helper()
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
		SessionTTL: time.Hour,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// sealedFlowCookie builds the flow cookie a real handleLogin would have
// set, so callback tests can drive handleCallback directly without going
// through the redirect round trip handleLogin itself already covers.
func sealedFlowCookie(t *testing.T, a *Authenticator, fs *flowState) *http.Cookie {
	t.Helper()
	sealed, err := a.sealer.sealFlow(fs)
	if err != nil {
		t.Fatalf("sealFlow: %v", err)
	}
	return &http.Cookie{Name: flowCookie, Value: sealed}
}

func callbackRequest(state, code string, flow *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/ui/auth/callback?state="+state+"&code="+code, nil)
	if flow != nil {
		req.AddCookie(flow)
	}
	return req
}

// TestHandleCallback_HappyPath drives the complete real flow: PKCE token
// exchange against the fake provider's /token endpoint, real RS256
// signature verification of the returned id_token, org-claim resolution
// to a real tenant, and a sealed session cookie set on success. This is
// the path this package's own doc comments used to say needed "a live
// Zitadel session" to exercise — closing that gap.
func TestHandleCallback_HappyPath(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, tenants := newTestAuthenticator(t, p)

	tenantID, err := tenants.Create(context.Background(), "Callback Test Tenant", "eu-west")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	orgID := "zitadel-org-" + tenantID
	if err := tenants.SetZitadelOrgID(context.Background(), tenantID, orgID); err != nil {
		t.Fatalf("link zitadel org: %v", err)
	}

	fs := &flowState{State: "state-1", Nonce: "nonce-1", Verifier: "verifier-1",
		ReturnTo: "/forms/Party/new", Expiry: time.Now().Add(10 * time.Minute)}
	claims := a.claimsForOrg(p, orgID, "zitadel-user-1", fs.Nonce)
	claims["name"] = "Ada Lovelace"
	claims["email"] = "ada@example.com"
	p.idToken = p.mintIDToken(t, claims)

	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest(fs.State, "fake-code", sealedFlowCookie(t, a, fs)))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != fs.ReturnTo {
		t.Fatalf("expected redirect to %q, got %q", fs.ReturnTo, got)
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "uc_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected a session cookie to be set")
	}
	sess, err := a.sealer.open(sessionCookie.Value)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if sess.Subject != "zitadel-user-1" || sess.TenantID != tenantID || sess.Name != "Ada Lovelace" || sess.Email != "ada@example.com" {
		t.Fatalf("unexpected session: %+v", sess)
	}
	if !sess.Valid() {
		t.Fatal("expected a valid session")
	}
}

// claimsForOrg builds the standard set of claims a happy-path id_token
// needs (base + the Zitadel project-roles claim carrying orgID), shared
// by every callback test that gets as far as real signature
// verification.
func (a *Authenticator) claimsForOrg(p *fakeOIDCProvider, orgID, subject, nonce string) map[string]any {
	c := p.baseClaims(a.cfg.ClientID, subject, nonce)
	c[zitadelProjectRolesClaim] = map[string]any{
		"tenant_member": map[string]any{orgID: "acme.id.universaltill.com"},
	}
	return c
}

func TestHandleCallback_NoFlowCookieOffersRetry(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s", "c", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestHandleCallback_ErrorParamRejected(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	req := httptest.NewRequest(http.MethodGet, "/ui/auth/callback?state=s1&error=access_denied", nil)
	req.AddCookie(sealedFlowCookie(t, a, fs))
	rec := httptest.NewRecorder()
	a.handleCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestHandleCallback_WrongStateRejected(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	fs := &flowState{State: "expected-state", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("wrong-state", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestHandleCallback_TokenEndpointDown(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	p.tokenErr = http.StatusInternalServerError
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 (Exchange failure), got %d", rec.Code)
	}
}

func TestHandleCallback_NoIDTokenInResponse(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	p.idToken = "" // /token responds successfully but with no id_token
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCallback_InvalidSignatureRejected(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	other := newFakeOIDCProvider(t) // a different key pair entirely
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	claims := other.baseClaims(testClientID, "u1", fs.Nonce)
	claims["iss"] = p.URL                    // claims to be from the right issuer...
	p.idToken = other.mintIDToken(t, claims) // ...but signed by the wrong key
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 (signature verification failure), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCallback_WrongNonceRejected(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	fs := &flowState{State: "s1", Nonce: "expected-nonce", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	claims := p.baseClaims(testClientID, "u1", "wrong-nonce")
	p.idToken = p.mintIDToken(t, claims)
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (nonce mismatch), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCallback_NoOrgClaimNotLinked(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a := newTestAuthenticatorNoDB(t, p)
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	claims := p.baseClaims(testClientID, "u1", fs.Nonce) // no zitadelProjectRolesClaim at all
	p.idToken = p.mintIDToken(t, claims)
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 (not linked), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No tenant linked") {
		t.Fatalf("expected the notLinked page, got: %s", rec.Body.String())
	}
}

// TestHandleCallback_TenantLookupErrorIsInternalError confirms a genuine
// database failure resolving the org claim (as opposed to Zitadel
// answering "no such org linked") surfaces as a 500, not the 403
// notLinked page — the org might be perfectly linked; Universal Core
// just couldn't check. Forces a real query error by closing the
// underlying connection right before the call, distinct from
// sql.ErrNoRows (data.ErrNotFound), which TestHandleCallback_UnlinkedOrgNotLinked
// already covers.
func TestHandleCallback_TenantLookupErrorIsInternalError(t *testing.T) {
	p := newFakeOIDCProvider(t)

	// A dedicated, already-closed connection: distinct from
	// newTestAuthenticator's shared testDB(t), so closing it here can't
	// break any other test sharing that connection.
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	brokenDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	brokenDB.Close() // closed immediately: guarantees the next query fails at the connection level

	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, data.NewTenantRepo(brokenDB))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	claims := a.claimsForOrg(p, "some-org", "u1", fs.Nonce)
	p.idToken = p.mintIDToken(t, claims)

	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the tenant lookup itself fails, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCallback_UnlinkedOrgNotLinked(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, _ := newTestAuthenticator(t, p)
	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	claims := a.claimsForOrg(p, "some-org-nobody-linked", "u1", fs.Nonce)
	p.idToken = p.mintIDToken(t, claims)
	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest("s1", "code", sealedFlowCookie(t, a, fs)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 (org has no linked tenant), got %d: %s", rec.Code, rec.Body.String())
	}
}

// ==== handleLogout ====

func TestHandleLogout_WithEndSessionRedirects(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID, PostLogoutURL: "https://erp.example/",
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	a.handleLogout(rec, httptest.NewRequest(http.MethodGet, "/ui/logout", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), p.URL+"/oidc/v1/end_session") {
		t.Fatalf("expected redirect to the provider's end_session endpoint, got %q", loc)
	}
	if loc.Query().Get("client_id") != testClientID {
		t.Fatalf("expected client_id=%q, got %q", testClientID, loc.Query().Get("client_id"))
	}
	if loc.Query().Get("post_logout_redirect_uri") != "https://erp.example/" {
		t.Fatalf("expected post_logout_redirect_uri to carry through, got %q", loc.Query().Get("post_logout_redirect_uri"))
	}

	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "uc_session" {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatal("expected the session cookie to be cleared (MaxAge < 0)")
	}
}

func TestHandleLogout_WithoutEndSessionRedirectsHome(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true, cfg: Config{ClientID: testClientID}}
	rec := httptest.NewRecorder()
	a.handleLogout(rec, httptest.NewRequest(http.MethodGet, "/ui/logout", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("expected redirect to /, got %q", got)
	}
}

// ==== small helpers ====

func TestSecureCookies(t *testing.T) {
	cases := map[string]bool{
		"https://erp.example/ui/auth/callback":   true,
		"HTTPS://ERP.EXAMPLE/ui/auth/callback":   true,
		"http://localhost:8080/ui/auth/callback": false,
		"":                                       false,
	}
	for redirectURL, want := range cases {
		a := &Authenticator{cfg: Config{RedirectURL: redirectURL}}
		if got := a.secureCookies(); got != want {
			t.Errorf("secureCookies() for %q = %v, want %v", redirectURL, got, want)
		}
	}
}

func TestNotLinked_RendersForbiddenPage(t *testing.T) {
	a := &Authenticator{}
	rec := httptest.NewRecorder()
	a.notLinked(rec)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No tenant linked") {
		t.Fatalf("expected the notLinked copy, got: %s", rec.Body.String())
	}
}

// TestRegister_DisabledIsNoOp confirms an unconfigured Authenticator
// mounts nothing, so a mux built for a deployment with no OIDC app
// configured has no /ui/login etc. routes at all rather than routes that
// would panic against a nil oauth config.
func TestRegister_DisabledIsNoOp(t *testing.T) {
	a := &Authenticator{}
	mux := http.NewServeMux()
	a.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected no routes mounted (404), got %d", rec.Code)
	}
}

// TestRegister_EnabledMountsRoutes confirms all three routes are wired
// and actually reach this Authenticator's own handlers (not just "some
// handler responds") — /ui/login redirects to the provider we built
// against, proving it's this Authenticator's handleLogin and not a
// different instance's.
func TestRegister_EnabledMountsRoutes(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, err := New(context.Background(), Config{
		IssuerURL: p.URL, ClientID: testClientID,
		RedirectURL: "https://erp.example/ui/auth/callback", CookieKeyB64: testCookieKeyB64(t),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/login", nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), p.URL) {
		t.Fatalf("expected /ui/login to redirect to this Authenticator's own provider, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	for _, path := range []string{"/ui/auth/callback", "/ui/logout"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("expected %s to be mounted, got 404", path)
		}
	}
}

// TestTryContext_DisabledReturnsNotOK exercises TryContext's own early
// return directly — Guard's disabled test (TestGuard_DisabledPassesThrough)
// never reaches this branch, since Guard checks Enabled() itself first
// and never calls TryContext at all when disabled.
func TestTryContext_DisabledReturnsNotOK(t *testing.T) {
	var a *Authenticator
	_, ok := a.TryContext(httptest.NewRequest(http.MethodGet, "/", nil))
	if ok {
		t.Fatal("expected ok=false for a disabled/nil Authenticator")
	}
}

func TestDedupe(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"no dupes", []string{"a", "b"}, []string{"a", "b"}},
		{"dupes removed, order preserved", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"empty strings dropped", []string{"", "a", ""}, []string{"a"}},
	}
	for _, tc := range cases {
		got := dedupe(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: dedupe(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: dedupe(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
				break
			}
		}
	}
}
