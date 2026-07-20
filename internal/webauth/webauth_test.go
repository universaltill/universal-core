package webauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/universaltill/universal-core/internal/httpx"
)

func TestSanitizeReturnTo(t *testing.T) {
	cases := map[string]string{
		"":                     "/",
		"/forms/Party/new":     "/forms/Party/new",
		"//evil.example.com":   "/",
		"https://evil.example": "/",
		"not-a-path":           "/",
		// Regression case for a confirmed open redirect (independent
		// review): a leading backslash passes the plain "/" prefix +
		// not-"//" check, but browsers normalize \ to / per the WHATWG
		// URL spec before following a Location header — turning this
		// into a protocol-relative "//evil.example.com" once the
		// browser actually navigates there.
		`/\evil.example.com`:  "/",
		`/\/evil.example.com`: "/",
	}
	for in, want := range cases {
		if got := sanitizeReturnTo(in); got != want {
			t.Errorf("sanitizeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGuard_DisabledPassesThrough confirms a nil/disabled Authenticator
// is a pure pass-through — the property that makes it safe to wrap
// unconditionally in internal/api's Routes() regardless of whether
// login is configured for this deployment.
func TestGuard_DisabledPassesThrough(t *testing.T) {
	var a *Authenticator // nil — the zero value Routes() gets when webauth.New was never called
	called := false
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil))
	if !called {
		t.Fatal("disabled Guard must pass through to next")
	}
}

// TestGuard_ValidSessionPopulatesRequestContext confirms a valid session
// cookie produces the exact same httpx.RequestContext shape DevAuth
// does — internal/api's handlers must not need to know which auth
// middleware actually ran.
func TestGuard_ValidSessionPopulatesRequestContext(t *testing.T) {
	s := testSealer(t)
	a := &Authenticator{sealer: s, enabled: true}

	sealed, err := s.seal(&Session{Subject: "zitadel-user-1", TenantID: "tenant-abc", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	var gotRC httpx.RequestContext
	var gotOK bool
	h := a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRC, gotOK = httpx.FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("expected a RequestContext to be attached")
	}
	if gotRC.TenantID != "tenant-abc" {
		t.Fatalf("expected tenant-abc, got %q", gotRC.TenantID)
	}
	if gotRC.Actor.ID != "zitadel-user-1" {
		t.Fatalf("expected actor id zitadel-user-1, got %q", gotRC.Actor.ID)
	}
}

// TestGuard_NoCookieRedirectsToLogin confirms an unauthenticated browser
// request gets a real redirect to the login flow, not a bare 401 — the
// whole point of Guard vs. DevAuth's JSON error for this case.
func TestGuard_NoCookieRedirectsToLogin(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	rec := httptest.NewRecorder()
	a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached without a valid session")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/login?returnTo=") {
		t.Fatalf("expected a redirect to /ui/login, got %q", loc)
	}
}

// TestGuard_TenantlessSessionRedirectsToLogin: a session sealed with no
// TenantID (Session.Valid()'s Universal-Core-specific check) must be
// treated exactly like no session at all — never let a request through
// with an empty tenant scope.
func TestGuard_TenantlessSessionRedirectsToLogin(t *testing.T) {
	s := testSealer(t)
	a := &Authenticator{sealer: s, enabled: true}
	sealed, _ := s.seal(&Session{Subject: "u1", TenantID: "", Expiry: time.Now().Add(time.Hour)})

	req := httptest.NewRequest(http.MethodGet, "/forms/Party/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
	rec := httptest.NewRecorder()
	a.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached with a tenantless session")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
}

// TestCallbackWithoutFlowCookieOffersRetry: a callback with no (or an
// expired) flow cookie must render a retry page pointing back at
// /ui/login, not a dead-end error string. Doesn't exercise the actual
// token exchange (that needs a live Zitadel session — see this branch's
// review doc for what's deliberately verified manually instead).
func TestCallbackWithoutFlowCookieOffersRetry(t *testing.T) {
	a := &Authenticator{sealer: testSealer(t), enabled: true}
	rec := httptest.NewRecorder()
	a.handleCallback(rec, httptest.NewRequest(http.MethodGet, "/ui/auth/callback?code=x&state=y", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/ui/login"`) || !strings.Contains(body, "Sign-in expired") {
		t.Fatalf("retry page missing login link: %q", body)
	}
}

// TestConfig_Enabled confirms the feature is genuinely OFF unless all
// three required fields are set — the property that lets Guard be wired
// unconditionally into every deployment, including ones with no OIDC
// app configured at all.
func TestConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"all set", Config{ClientID: "c", RedirectURL: "https://x/callback", CookieKeyB64: "k"}, true},
		{"missing client id", Config{RedirectURL: "https://x/callback", CookieKeyB64: "k"}, false},
		{"missing redirect url", Config{ClientID: "c", CookieKeyB64: "k"}, false},
		{"missing cookie key", Config{ClientID: "c", RedirectURL: "https://x/callback"}, false},
		{"zero value", Config{}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.Enabled(); got != tc.want {
			t.Errorf("%s: Enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
