// Tests for the multi-org login + tenant picker/switcher (#25,
// ADR-0011): Session's accessible-set methods, handleCallback's
// multi-org resolution against the fake OIDC provider + real Postgres,
// /ui/auth/choose, /ui/auth/switch, and Guard's third outcome
// (needs-choice → the picker, not login). orgIDsFromClaims itself is
// covered in claims_test.go and not duplicated here.
package webauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
)

// ==== Session methods (unit, no DB) ====

func TestAccessibleTenantIDs(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name string
		sess *Session
		want []string
	}{
		{"nil session", nil, nil},
		{"empty session", &Session{}, nil},
		{"single active tenant", &Session{Subject: "u1", TenantID: "t1", Expiry: future}, []string{"t1"}},
		{"set carried, choice pending", &Session{Subject: "u1", TenantIDs: []string{"t1", "t2"}, Expiry: future}, []string{"t1", "t2"}},
		{"set carried with active tenant", &Session{Subject: "u1", TenantID: "t1", TenantIDs: []string{"t1", "t2"}, Expiry: future}, []string{"t1", "t2"}},
	}
	for _, tc := range cases {
		if got := tc.sess.AccessibleTenantIDs(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: AccessibleTenantIDs() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCanAccess(t *testing.T) {
	multi := &Session{Subject: "u1", TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)}
	single := &Session{Subject: "u1", TenantID: "t1", Expiry: time.Now().Add(time.Hour)}
	cases := []struct {
		name   string
		sess   *Session
		target string
		want   bool
	}{
		{"in set", multi, "t2", true},
		{"not in set", multi, "t3", false},
		{"empty target", multi, "", false},
		{"single-tenant own id", single, "t1", true},
		{"single-tenant other id", single, "t2", false},
		{"nil session", nil, "t1", false},
	}
	for _, tc := range cases {
		if got := tc.sess.CanAccess(tc.target); got != tc.want {
			t.Errorf("%s: CanAccess(%q) = %v, want %v", tc.name, tc.target, got, tc.want)
		}
	}
}

func TestNeedsChoice(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name string
		sess *Session
		want bool
	}{
		{"authenticated multi-tenant, no active tenant", &Session{Subject: "u1", TenantIDs: []string{"t1", "t2"}, Expiry: future}, true},
		{"expired", &Session{Subject: "u1", TenantIDs: []string{"t1", "t2"}, Expiry: past}, false},
		{"active tenant already picked", &Session{Subject: "u1", TenantID: "t1", TenantIDs: []string{"t1", "t2"}, Expiry: future}, false},
		{"no tenant set at all", &Session{Subject: "u1", Expiry: future}, false},
		{"no subject", &Session{TenantIDs: []string{"t1", "t2"}, Expiry: future}, false},
		{"nil session", nil, false},
	}
	for _, tc := range cases {
		if got := tc.sess.NeedsChoice(); got != tc.want {
			t.Errorf("%s: NeedsChoice() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNeedsChoiceSessionIsNotValid pins the deliberate contract that a
// needs-choice session is authenticated but NOT usable: Valid() must
// stay false until /ui/auth/switch seals an active tenant, so no
// downstream handler can ever see an empty TenantID.
func TestNeedsChoiceSessionIsNotValid(t *testing.T) {
	sess := &Session{Subject: "u1", TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)}
	if !sess.NeedsChoice() {
		t.Fatal("expected NeedsChoice() = true")
	}
	if sess.Valid() {
		t.Fatal("a needs-choice session must not be Valid()")
	}
}

// ==== handleCallback multi-org resolution (fake provider + real DB) ====

// claimsForOrgs is claimsForOrg's multi-org sibling: the same
// project-roles claim shape carrying every org in orgIDs under the one
// tenant_member role.
func (a *Authenticator) claimsForOrgs(p *fakeOIDCProvider, orgIDs []string, subject, nonce string) map[string]any {
	c := p.baseClaims(a.cfg.ClientID, subject, nonce)
	orgs := map[string]any{}
	for _, id := range orgIDs {
		orgs[id] = id + ".id.universaltill.com"
	}
	c[zitadelProjectRolesClaim] = map[string]any{"tenant_member": orgs}
	return c
}

// uniqueTenantName appends a random token to base so repeated runs
// against a persistent database never collide on the tenants_slug_key
// unique index (slug is derived from the name) — the same
// never-collides convention tenant_resolution_test.go documents for
// zitadel_org_id, now needed for names too.
func uniqueTenantName(base string) string {
	return base + " " + randToken()
}

// seedLinkedTenant creates a tenant and links it to a Zitadel org id
// derived from its own generated id (same never-collides convention as
// tenant_resolution_test.go).
func seedLinkedTenant(t *testing.T, tenants *data.TenantRepo, name string) (tenantID, orgID string) {
	t.Helper()
	tenantID, err := tenants.Create(context.Background(), name, "eu-west")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	orgID = "zitadel-org-" + tenantID
	if err := tenants.SetZitadelOrgID(context.Background(), tenantID, orgID); err != nil {
		t.Fatalf("link zitadel org: %v", err)
	}
	return tenantID, orgID
}

func sessionCookieFromResponse(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

// TestHandleCallback_TwoLinkedOrgsDeferToChoose is the multi-org login
// the old fail-closed orgIDFromClaims guard was waiting for: both
// asserted orgs map to tenants, so the callback seals a needs-choice
// session (no active tenant, both in the set) and defers to the picker
// instead of guessing.
func TestHandleCallback_TwoLinkedOrgsDeferToChoose(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, tenants := newTestAuthenticator(t, p)

	tenant1, org1 := seedLinkedTenant(t, tenants, uniqueTenantName("Choose Tenant One"))
	tenant2, org2 := seedLinkedTenant(t, tenants, uniqueTenantName("Choose Tenant Two"))

	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1",
		ReturnTo: "/forms/Party/new", Expiry: time.Now().Add(10 * time.Minute)}
	p.idToken = p.mintIDToken(t, a.claimsForOrgs(p, []string{org1, org2}, "zitadel-user-multi", fs.Nonce))

	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest(fs.State, "fake-code", sealedFlowCookie(t, a, fs)))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Path != "/ui/auth/choose" {
		t.Fatalf("expected a redirect to /ui/auth/choose, got %q", rec.Header().Get("Location"))
	}
	if got := loc.Query().Get("returnTo"); got != fs.ReturnTo {
		t.Fatalf("expected returnTo=%q carried to the picker, got %q", fs.ReturnTo, got)
	}

	cookie := sessionCookieFromResponse(t, rec)
	if cookie == nil {
		t.Fatal("expected a session cookie to be set")
	}
	sess, err := a.sealer.open(cookie.Value)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if sess.TenantID != "" {
		t.Fatalf("expected no active tenant before the choice, got %q", sess.TenantID)
	}
	got := append([]string(nil), sess.TenantIDs...)
	want := []string{tenant1, tenant2}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("expected accessible set %v, got %v", want, sess.TenantIDs)
	}
	if !sess.NeedsChoice() {
		t.Fatal("expected a needs-choice session")
	}
	if sess.Valid() {
		t.Fatal("a needs-choice session must not be Valid()")
	}
}

// TestHandleCallback_OneLinkedOrgSignsStraightIn: an unmapped org
// alongside one linked org is skipped, not fatal — exactly one tenant
// resolves, so the login signs straight in with it active and no
// TenantIDs set carried in the cookie (no picker for no real choice).
func TestHandleCallback_OneLinkedOrgSignsStraightIn(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, tenants := newTestAuthenticator(t, p)

	tenantID, orgID := seedLinkedTenant(t, tenants, uniqueTenantName("Choose Tenant Solo"))

	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1",
		ReturnTo: "/forms/Party/new", Expiry: time.Now().Add(10 * time.Minute)}
	p.idToken = p.mintIDToken(t, a.claimsForOrgs(p, []string{orgID, "org-nobody-linked"}, "zitadel-user-solo", fs.Nonce))

	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest(fs.State, "fake-code", sealedFlowCookie(t, a, fs)))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != fs.ReturnTo {
		t.Fatalf("expected straight redirect to %q (no picker), got %q", fs.ReturnTo, got)
	}

	cookie := sessionCookieFromResponse(t, rec)
	if cookie == nil {
		t.Fatal("expected a session cookie to be set")
	}
	sess, err := a.sealer.open(cookie.Value)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if sess.TenantID != tenantID {
		t.Fatalf("expected active tenant %q, got %q", tenantID, sess.TenantID)
	}
	if len(sess.TenantIDs) != 0 {
		t.Fatalf("expected no TenantIDs set for a single-tenant login, got %v", sess.TenantIDs)
	}
	if !sess.Valid() {
		t.Fatal("expected a valid session")
	}
}

// TestHandleCallback_MultipleOrgsNoneMappedNotLinked: several asserted
// orgs, none with a tenants row — the notLinked terminal state, same as
// a single unmapped org.
func TestHandleCallback_MultipleOrgsNoneMappedNotLinked(t *testing.T) {
	p := newFakeOIDCProvider(t)
	a, _ := newTestAuthenticator(t, p)

	fs := &flowState{State: "s1", Nonce: "n1", Verifier: "v1", ReturnTo: "/", Expiry: time.Now().Add(time.Minute)}
	p.idToken = p.mintIDToken(t, a.claimsForOrgs(p, []string{"org-unmapped-a", "org-unmapped-b"}, "u1", fs.Nonce))

	rec := httptest.NewRecorder()
	a.handleCallback(rec, callbackRequest(fs.State, "fake-code", sealedFlowCookie(t, a, fs)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 (not linked), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No tenant linked") {
		t.Fatalf("expected the notLinked page, got: %s", rec.Body.String())
	}
	if sessionCookieFromResponse(t, rec) != nil {
		t.Fatal("no session cookie may be sealed for a not-linked login")
	}
}

// ==== GET /ui/auth/choose ====

// chooseAuthenticator is a real-Postgres Authenticator for the
// picker/switcher routes only — no fake provider needed (choose/switch
// never touch the oauth config), mounted via Register so these tests
// also prove the routes are actually wired.
func chooseAuthenticator(t *testing.T) (*Authenticator, *data.TenantRepo, *http.ServeMux) {
	t.Helper()
	tenants := data.NewTenantRepo(testDB(t))
	a := &Authenticator{sealer: testSealer(t), enabled: true, tenants: tenants}
	mux := http.NewServeMux()
	a.Register(mux)
	return a, tenants, mux
}

func addSessionCookie(t *testing.T, a *Authenticator, req *http.Request, sess *Session) {
	t.Helper()
	sealed, err := a.sealer.seal(sess)
	if err != nil {
		t.Fatalf("seal session: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
}

func TestHandleChoose_ListsAccessibleTenantsAsSwitchForms(t *testing.T) {
	a, tenants, mux := chooseAuthenticator(t)
	name1 := uniqueTenantName("Picker Tenant Alpha")
	name2 := uniqueTenantName("Picker Tenant Beta")
	tenant1, _ := seedLinkedTenant(t, tenants, name1)
	tenant2, _ := seedLinkedTenant(t, tenants, name2)

	req := httptest.NewRequest(http.MethodGet, "/ui/auth/choose?returnTo=/forms/Party/new", nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1",
		TenantIDs: []string{tenant1, tenant2}, Expiry: time.Now().Add(time.Hour)})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected an HTML page, got Content-Type %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		name1, name2,
		`action="/ui/auth/switch"`,
		`name="tenant_id" value="` + tenant1 + `"`,
		`name="tenant_id" value="` + tenant2 + `"`,
		`name="returnTo" value="/forms/Party/new"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("choose page missing %q; body: %s", want, body)
		}
	}
}

// TestHandleChoose_HostileReturnToSanitized: the hidden returnTo the
// switch forms carry must already be sanitized — the picker never
// launders an open redirect through to /ui/auth/switch.
func TestHandleChoose_HostileReturnToSanitized(t *testing.T) {
	a, tenants, mux := chooseAuthenticator(t)
	tenant1, _ := seedLinkedTenant(t, tenants, uniqueTenantName("Picker Tenant Hostile A"))
	tenant2, _ := seedLinkedTenant(t, tenants, uniqueTenantName("Picker Tenant Hostile B"))

	req := httptest.NewRequest(http.MethodGet, "/ui/auth/choose?returnTo="+url.QueryEscape("//evil.com"), nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1",
		TenantIDs: []string{tenant1, tenant2}, Expiry: time.Now().Add(time.Hour)})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="returnTo" value="/"`) {
		t.Fatalf("expected hostile returnTo sanitized to \"/\", body: %s", rec.Body.String())
	}
}

func TestHandleChoose_NoSessionRedirectsToLogin(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	mux := http.NewServeMux()
	a.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/auth/choose", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/login?returnTo=") {
		t.Fatalf("expected a redirect to /ui/login, got %q", loc)
	}
}

// TestHandleChoose_AllTenantsGoneNotLinked: every id in the sealed set
// deleted since login — nothing to offer, so the picker degrades to the
// notLinked page instead of rendering an empty form list.
func TestHandleChoose_AllTenantsGoneNotLinked(t *testing.T) {
	a, _, mux := chooseAuthenticator(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/auth/choose", nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1", TenantIDs: []string{
		"99999999-9999-9999-9999-999999999998",
		"99999999-9999-9999-9999-999999999999",
	}, Expiry: time.Now().Add(time.Hour)})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No tenant linked") {
		t.Fatalf("expected the notLinked page, got: %s", rec.Body.String())
	}
}

// ==== POST /ui/auth/switch (no DB — handleSwitch never touches tenants) ====

func switchRequest(tenantID, returnTo string) *http.Request {
	form := url.Values{"tenant_id": {tenantID}, "returnTo": {returnTo}}
	req := httptest.NewRequest(http.MethodPost, "/ui/auth/switch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestHandleSwitch_ActivatesTenantInSet(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}

	req := switchRequest("t2", "/forms/Party/new")
	addSessionCookie(t, a, req, &Session{Subject: "u1",
		TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})
	rec := httptest.NewRecorder()
	a.handleSwitch(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/forms/Party/new" {
		t.Fatalf("expected redirect to the sanitized returnTo, got %q", got)
	}
	cookie := sessionCookieFromResponse(t, rec)
	if cookie == nil {
		t.Fatal("expected the session cookie to be re-sealed")
	}
	sess, err := a.sealer.open(cookie.Value)
	if err != nil {
		t.Fatalf("open re-sealed session: %v", err)
	}
	if sess.TenantID != "t2" {
		t.Fatalf("expected active tenant t2, got %q", sess.TenantID)
	}
	if !slices.Equal(sess.TenantIDs, []string{"t1", "t2"}) {
		t.Fatalf("expected the accessible set preserved, got %v", sess.TenantIDs)
	}
	if !sess.Valid() {
		t.Fatal("expected the session to be Valid() once a tenant is active")
	}
}

// TestHandleSwitch_SwitchesAlreadyActiveSession is the mid-session
// switcher (not the post-login picker): an already-valid session moves
// its active tenant to another member of the set by re-sealing the
// cookie, never re-authenticating.
func TestHandleSwitch_SwitchesAlreadyActiveSession(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}

	req := switchRequest("t2", "/")
	addSessionCookie(t, a, req, &Session{Subject: "u1", TenantID: "t1",
		TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})
	rec := httptest.NewRecorder()
	a.handleSwitch(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	cookie := sessionCookieFromResponse(t, rec)
	if cookie == nil {
		t.Fatal("expected the session cookie to be re-sealed")
	}
	sess, err := a.sealer.open(cookie.Value)
	if err != nil {
		t.Fatalf("open re-sealed session: %v", err)
	}
	if sess.TenantID != "t2" {
		t.Fatalf("expected active tenant t2 after the switch, got %q", sess.TenantID)
	}
}

func TestHandleSwitch_HostileReturnToFallsBackToRoot(t *testing.T) {
	for _, hostile := range []string{"//evil.com", `/\evil.com`, "https://evil.com"} {
		a := &Authenticator{sealer: testSealer(t), enabled: true}
		req := switchRequest("t2", hostile)
		addSessionCookie(t, a, req, &Session{Subject: "u1",
			TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})
		rec := httptest.NewRecorder()
		a.handleSwitch(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("returnTo=%q: want 303, got %d", hostile, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("returnTo=%q: expected fallback to \"/\", got %q", hostile, got)
		}
	}
}

// TestHandleSwitch_OutsideSetForbidden: the sealed accessible set is
// the sole authority — a forged tenant_id outside it gets a 403 and,
// critically, no re-sealed cookie.
func TestHandleSwitch_OutsideSetForbidden(t *testing.T) {
	for _, target := range []string{"t3", ""} {
		a := &Authenticator{sealer: testSealer(t), enabled: true}
		req := switchRequest(target, "/")
		addSessionCookie(t, a, req, &Session{Subject: "u1",
			TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})
		rec := httptest.NewRecorder()
		a.handleSwitch(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant_id=%q: want 403, got %d", target, rec.Code)
		}
		if sessionCookieFromResponse(t, rec) != nil {
			t.Errorf("tenant_id=%q: the session cookie must not be re-issued on a refused switch", target)
		}
	}
}

func TestHandleSwitch_NoSessionRedirectsToLogin(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	rec := httptest.NewRecorder()
	a.handleSwitch(rec, switchRequest("t1", "/"))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/login?returnTo=") {
		t.Fatalf("expected a redirect to /ui/login, got %q", loc)
	}
}

// ==== Guard's needs-choice outcome ====

func TestGuard_NeedsChoiceRedirectsToChoose(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	req := httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1",
		TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})

	rec := httptest.NewRecorder()
	a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached with a needs-choice session")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	want := "/ui/auth/choose?returnTo=" + url.QueryEscape("/forms/Party/new")
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("expected redirect to %q (the picker, not login), got %q", want, got)
	}
}

// TestGuard_ExpiredNeedsChoiceRedirectsToLogin: an expired needs-choice
// cookie is just "logged out" — rawSessionFromCookie enforces expiry,
// so the picker never serves a stale authentication.
func TestGuard_ExpiredNeedsChoiceRedirectsToLogin(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	req := httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1",
		TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(-time.Minute)})

	rec := httptest.NewRecorder()
	a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached with an expired session")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/login?returnTo=") {
		t.Fatalf("expected a redirect to /ui/login, got %q", loc)
	}
}

// TestGuard_ValidMultiTenantSessionPassesThrough: once a tenant is
// active, carrying a TenantIDs set changes nothing for Guard — the
// request passes with the active tenant in the RequestContext, same as
// the single-tenant sessions webauth_test.go already covers.
func TestGuard_ValidMultiTenantSessionPassesThrough(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	req := httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil)
	addSessionCookie(t, a, req, &Session{Subject: "u1", TenantID: "t2",
		TenantIDs: []string{"t1", "t2"}, Expiry: time.Now().Add(time.Hour)})

	var gotRC httpx.RequestContext
	var reached bool
	rec := httptest.NewRecorder()
	a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		gotRC, _ = httpx.FromContext(r.Context())
	})).ServeHTTP(rec, req)

	if !reached {
		t.Fatal("a valid multi-tenant session must pass through Guard")
	}
	if gotRC.TenantID != "t2" {
		t.Fatalf("expected the active tenant t2 in the RequestContext, got %q", gotRC.TenantID)
	}
}

// ==== SessionTenants (the nav switcher's data source) ====

func TestSessionTenants(t *testing.T) {
	a, tenants, _ := chooseAuthenticator(t)
	name1 := uniqueTenantName("Switcher Tenant One")
	name2 := uniqueTenantName("Switcher Tenant Two")
	tenant1, _ := seedLinkedTenant(t, tenants, name1)
	tenant2, _ := seedLinkedTenant(t, tenants, name2)

	// No session cookie at all → no switcher.
	if got := a.SessionTenants(httptest.NewRequest(http.MethodGet, "/", nil)); got != nil {
		t.Fatalf("expected nil without a session, got %v", got)
	}

	// Single-tenant session → no switcher (no real choice to offer).
	single := httptest.NewRequest(http.MethodGet, "/", nil)
	addSessionCookie(t, a, single, &Session{Subject: "u1", TenantID: tenant1, Expiry: time.Now().Add(time.Hour)})
	if got := a.SessionTenants(single); got != nil {
		t.Fatalf("expected nil for a single-tenant session, got %v", got)
	}

	// Multi-tenant session → both options, the active one flagged.
	multi := httptest.NewRequest(http.MethodGet, "/", nil)
	addSessionCookie(t, a, multi, &Session{Subject: "u1", TenantID: tenant1,
		TenantIDs: []string{tenant1, tenant2}, Expiry: time.Now().Add(time.Hour)})
	opts := a.SessionTenants(multi)
	if len(opts) != 2 {
		t.Fatalf("expected 2 tenant options, got %v", opts)
	}
	byID := map[string]TenantOption{}
	for _, o := range opts {
		byID[o.ID] = o
	}
	if o := byID[tenant1]; o.Name != name1 || !o.Active {
		t.Fatalf("expected tenant1 named %q and active, got %+v", name1, o)
	}
	if o := byID[tenant2]; o.Name != name2 || o.Active {
		t.Fatalf("expected tenant2 named %q and inactive, got %+v", name2, o)
	}
}
