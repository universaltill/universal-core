// Package webauth implements real browser login for Universal Core:
// OpenID Connect Authorization Code + PKCE against Universal Till ID
// (Zitadel, id.universaltill.com), replicating the mechanics already
// proven in production by ut-cloud's own internal/webauth (ADR-0012) —
// not a fresh design. The one genuine difference: ut-cloud maps a
// Zitadel id_token onto app-wide RBAC roles for a single console;
// Universal Core is multi-tenant (separate customer companies with
// isolated data, CLAUDE.md's multi-tenancy rule), and Zitadel's own
// multi-tenancy primitive — organizations — maps onto that concept
// directly: one customer company is one Zitadel org, and one
// tenants row (tenants.zitadel_org_id, 004_tenant_zitadel_org.sql).
// A login resolves the org claim to a tenant_id once, at sign-in
// (data.TenantRepo.GetByZitadelOrgID); every later request reads the
// already-resolved tenant_id out of the sealed session cookie, the same
// way ut-cloud's sessions carry already-resolved roles.
//
// The feature is OFF unless configured (Config.Enabled()); a deployment
// with no OIDC app behaves exactly as before — still gated behind
// httpx.DevAuth's own fail-closed default. See bridge.go for how a
// verified Session becomes the same httpx.RequestContext DevAuth
// produces, so internal/api's handlers never need to know which
// middleware actually ran.
//
// Deliberately simpler than ut-cloud's version in one respect:
// single-hostname only (erp.universaltill.com), no multi-host
// redirect-URL registry — ut-cloud's canonicalLoginURL/redirectMap exist
// because that console answers on several public hostnames
// (marketplace.*, cloud.*, ADR-0018); Universal Core doesn't have that
// problem yet, so that whole mechanism is left out rather than ported
// speculatively.
package webauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/universaltill/universal-core/internal/data"
)

const (
	sessionCookie = "uc_session"
	flowCookie    = "uc_oidc_flow"
)

// Config configures the browser OIDC login. The feature is OFF unless
// ClientID, RedirectURL and CookieKeyB64 are all set, so a deployment
// with no OIDC app configured (every environment before this ships,
// and any without id.universaltill.com set up) behaves exactly as
// before.
type Config struct {
	IssuerURL     string // https://id.universaltill.com
	ClientID      string // Zitadel public PKCE web-app client id
	RedirectURL   string // https://erp.universaltill.com/ui/auth/callback
	PostLogoutURL string // where Zitadel returns the browser after logout
	CookieKeyB64  string // base64-encoded 32-byte key sealing the session cookie
	Scopes        []string
	SessionTTL    time.Duration
}

// Enabled reports whether browser OIDC login is configured.
func (c Config) Enabled() bool {
	return c.ClientID != "" && c.RedirectURL != "" && c.CookieKeyB64 != ""
}

// Authenticator drives the OIDC login. A nil or disabled Authenticator
// is safe to use: Guard falls through to whatever runs after it (in
// practice, httpx.DevAuth, which still fails closed on its own).
type Authenticator struct {
	cfg     Config
	tenants *data.TenantRepo

	enabled  bool
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	endSess  string // provider end_session_endpoint (may be empty)
	sealer   *sealer
}

// New builds an Authenticator. When login is not configured it returns
// a disabled Authenticator (no error) so callers can wire it
// unconditionally.
func New(ctx context.Context, cfg Config, tenants *data.TenantRepo) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, tenants: tenants}
	if !cfg.Enabled() {
		return a, nil
	}
	seal, err := newSealer(cfg.CookieKeyB64)
	if err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	scopes := dedupe(append([]string{oidc.ScopeOpenID, "profile", "email"}, cfg.Scopes...))
	a.sealer = seal
	a.oauth = &oauth2.Config{
		ClientID:    cfg.ClientID,
		Endpoint:    provider.Endpoint(),
		RedirectURL: cfg.RedirectURL,
		Scopes:      scopes,
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	a.enabled = true

	// Discover the end-session endpoint for RP-initiated logout
	// (optional — logout still clears the local cookie without it).
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&meta); err == nil {
		a.endSess = meta.EndSession
	}
	return a, nil
}

// Enabled reports whether browser login is configured and ready.
func (a *Authenticator) Enabled() bool {
	return a != nil && a.enabled
}

func (a *Authenticator) secureCookies() bool {
	return strings.HasPrefix(strings.ToLower(a.cfg.RedirectURL), "https://")
}

// Register mounts /ui/login, /ui/auth/callback and /ui/logout. No-op
// when login is disabled. These paths must not sit behind DevAuth/Guard
// themselves — the login flow is how a request gets a session in the
// first place.
func (a *Authenticator) Register(mux *http.ServeMux) {
	if !a.Enabled() {
		return
	}
	mux.HandleFunc("GET /ui/login", a.handleLogin)
	mux.HandleFunc("GET /ui/auth/callback", a.handleCallback)
	mux.HandleFunc("GET /ui/logout", a.handleLogout)
}

func (a *Authenticator) sessionFromCookie(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	sess, err := a.sealer.open(c.Value)
	if err != nil || !sess.Valid() {
		return nil
	}
	return sess
}

func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	verifier := oauth2.GenerateVerifier()
	fs := flowState{
		State:    randToken(),
		Nonce:    randToken(),
		Verifier: verifier,
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("returnTo")),
		Expiry:   time.Now().Add(10 * time.Minute),
	}
	sealed, err := a.sealer.sealFlow(&fs)
	if err != nil {
		http.Error(w, "login init failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: sealed, Path: "/ui", HttpOnly: true,
		Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	authURL := a.oauth.AuthCodeURL(fs.State, oidc.Nonce(fs.Nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Always clear the flow cookie.
	defer http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/ui", MaxAge: -1})

	fc, err := r.Cookie(flowCookie)
	if err != nil {
		a.loginExpired(w)
		return
	}
	fs, err := a.sealer.openFlow(fc.Value)
	if err != nil || time.Now().After(fs.Expiry) {
		a.loginExpired(w)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "sign-in was cancelled or failed", http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Get("state") != fs.State {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		http.Error(w, "sign-in failed", http.StatusUnauthorized)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		http.Error(w, "sign-in failed (no id_token)", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "sign-in failed", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != fs.Nonce {
		http.Error(w, "invalid login nonce", http.StatusBadRequest)
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}

	orgID, ok := orgIDFromClaims(claims)
	if !ok {
		a.notLinked(w)
		return
	}
	tenantID, err := a.tenants.GetByZitadelOrgID(r.Context(), orgID)
	if errors.Is(err, data.ErrNotFound) {
		a.notLinked(w)
		return
	}
	if err != nil {
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}

	sess := &Session{
		Subject:  idToken.Subject,
		Name:     stringClaim(claims, "name"),
		Email:    stringClaim(claims, "email"),
		TenantID: tenantID,
		Expiry:   time.Now().Add(a.cfg.SessionTTL),
	}
	sealed, err := a.sealer.seal(sess)
	if err != nil {
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sealed, Path: "/", HttpOnly: true,
		Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode,
		Expires: sess.Expiry, MaxAge: int(time.Until(sess.Expiry).Seconds()),
	})
	http.Redirect(w, r, fs.ReturnTo, http.StatusFound)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	if a.endSess != "" {
		if u, err := url.Parse(a.endSess); err == nil {
			q := u.Query()
			q.Set("client_id", a.cfg.ClientID)
			if a.cfg.PostLogoutURL != "" {
				q.Set("post_logout_redirect_uri", a.cfg.PostLogoutURL)
			}
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Authenticator) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/login?returnTo="+url.QueryEscape(r.URL.Path), http.StatusFound)
}

// loginExpired mirrors ut-cloud's own page for the same failure mode:
// the flow cookie is missing or its 10-minute window passed (an old tab
// finishing a stale sign-in, or sitting on Zitadel's account picker too
// long).
func (a *Authenticator) loginExpired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Sign-in expired</title>` +
		`<body style="font-family:system-ui;max-width:32rem;margin:4rem auto;text-align:center">` +
		`<h1>Sign-in expired</h1><p>This sign-in attempt is no longer valid.</p>` +
		`<p><a href="/ui/login">Try signing in again</a></p></body>`))
}

// notLinked is the Universal-Core-specific failure ut-cloud has no
// equivalent of: a real, successfully-authenticated Zitadel user with no
// tenants row linked to their org (orgIDFromClaims found nothing, or it
// found an org id that isn't in tenants.zitadel_org_id — both routed
// here, see handleCallback). Tenant linking is a manual, out-of-band
// step for now (matches cmd/provision-tenant's own current scope, no
// self-serve onboarding exists yet) — this page is the correct terminal
// state, not a bug to route around.
func (a *Authenticator) notLinked(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>No tenant linked</title>` +
		`<body style="font-family:system-ui;max-width:32rem;margin:4rem auto;text-align:center">` +
		`<h1>No tenant linked</h1><p>Your account signed in successfully, but isn't linked to a Universal Core tenant yet.</p></body>`))
}

// sanitizeReturnTo prevents open redirects: only a same-site absolute
// path is honoured.
func sanitizeReturnTo(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
