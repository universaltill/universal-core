// Package svcauth implements machine-to-machine authentication for
// Universal Core's API: a Bearer access token issued by Universal Till
// ID (Zitadel, id.universaltill.com) to a per-tenant *machine user* — a
// connector's own service credential (Universal Till's connector plugin
// is the first, see ../../INTEGRATIONS.md and uc-infra/infra/terraform/
// zitadel/main.tf's zitadel_machine_user/zitadel_personal_access_token
// resources).
//
// Deliberately a separate package from internal/webauth, even though
// both talk to the same Zitadel issuer: the trust model differs enough
// to keep them apart. webauth trusts a browser's signed, short-lived
// session cookie derived from an interactive login; svcauth trusts a
// bearer credential presented directly by a backend service on every
// request, with no session and no human present, validated via
// Zitadel's token-introspection endpoint (RFC 7662) rather than local
// JWT verification — deliberately, so a revoked/expired credential is
// rejected on the very next request rather than only once a
// self-contained token's own expiry passes. That matters much more here
// than for webauth's short browser sessions: a service credential is
// typically long-lived.
//
// Every mutation still needs a real audit.Actor (CLAUDE.md: human or
// ai_agent, never a generic third "system" bucket). A machine-
// authenticated request is always attributed as a *human* actor — never
// a new bucket — because there is always a real, named, accountable
// party behind it: either the specific human the caller identifies via
// the optional On-Behalf-Of header (e.g. the till operator who actually
// completed a sale), or, absent that, the service credential's own
// stable Zitadel identity (svc:<subject> — the person/team accountable
// for that credential, same as any other named actor). The service
// credential itself is only ever an *authentication* mechanism, not a
// new actor-type bucket.
package svcauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// zitadelProjectRolesClaim is the same claim shape internal/webauth's
// own orgIDFromClaims reads out of an id_token — Zitadel's introspection
// response carries the identical shape for an access token when project
// role assertion is on (see uc-infra's zitadel_project.
// project_role_assertion). Not shared code with webauth: the selection
// semantics differ (role-specific here, "any single role" there — see
// orgIDForRole's own doc comment) and the two packages otherwise have
// nothing to do with each other.
const zitadelProjectRolesClaim = "urn:zitadel:iam:org:project:roles"

// tenantIntegrationRole is the Zitadel project role
// (uc-infra/infra/terraform/zitadel's tenant_integration) granted only
// to machine users, never humans — a contract with that Terraform, keep
// in sync if it ever changes.
const tenantIntegrationRole = "tenant_integration"

// OnBehalfOfHeader lets an already-authenticated connector assert which
// specific human actually initiated a given mutation (e.g. the till
// operator who completed a sale) — the service credential vouches for
// this identity; Universal Core trusts the assertion once the
// credential itself is verified, the same trust boundary a till already
// extends to its own logged-in cashier. Absent, the actor defaults to
// the service credential's own identity (see Config's doc comment) —
// the right default for a genuinely unattended call (e.g. a nightly
// catalog sync) with no human in the loop at all.
const OnBehalfOfHeader = "X-On-Behalf-Of"

// Config configures machine-to-machine API auth. The feature is OFF
// unless every field is set, so a deployment with no introspection
// credential configured (every environment before this ships) behaves
// exactly as before.
type Config struct {
	IssuerURL    string // https://id.universaltill.com — same issuer webauth uses
	ClientID     string // Zitadel API-application client id (introspection caller identity)
	ClientSecret string // that application's client secret
}

// Enabled reports whether machine-to-machine API auth is configured.
func (c Config) Enabled() bool {
	return c.IssuerURL != "" && c.ClientID != "" && c.ClientSecret != ""
}

// Authenticator validates Bearer access tokens via Zitadel token
// introspection. A nil or disabled Authenticator is safe to use: Guard
// falls through to whatever runs after it, same composability contract
// webauth.Authenticator and httpx.DevAuth both already give.
type Authenticator struct {
	cfg     Config
	tenants *data.TenantRepo

	enabled               bool
	introspectionEndpoint string
	httpClient            *http.Client
}

// New builds an Authenticator. When not configured it returns a
// disabled Authenticator (no error) so callers can wire it
// unconditionally, same pattern webauth.New already establishes.
func New(ctx context.Context, cfg Config, tenants *data.TenantRepo) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, tenants: tenants, httpClient: http.DefaultClient}
	if !cfg.Enabled() {
		return a, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("svcauth: discover issuer %s: %w", cfg.IssuerURL, err)
	}
	// oidc.Provider doesn't expose introspection_endpoint itself (it's
	// not part of the minimal OIDC-core metadata that package parses) —
	// same "read the raw discovery document ourselves" pattern webauth.
	// New already uses for end_session_endpoint.
	var meta struct {
		Introspection string `json:"introspection_endpoint"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("svcauth: parse discovery document: %w", err)
	}
	if meta.Introspection == "" {
		return nil, fmt.Errorf("svcauth: issuer %s has no introspection_endpoint", cfg.IssuerURL)
	}
	a.introspectionEndpoint = meta.Introspection
	a.enabled = true
	return a, nil
}

// Enabled reports whether machine-to-machine API auth is configured and ready.
func (a *Authenticator) Enabled() bool {
	return a != nil && a.enabled
}

// Guard authenticates a Bearer access token if one is present, else
// passes the request through unchanged — composed ahead of
// webauth.Guard in internal/api's route wrapper (see handlers.go's
// Routes), not behind it: a Bearer-carrying request is unambiguously an
// API client, which must get a clean 401 JSON body on failure, never
// webauth.Guard's own browser-oriented redirect to /ui/login.
//
// A disabled Authenticator is a straight pass-through, same contract
// webauth.Guard/httpx.DevAuth both already give — a deployment with no
// introspection credential configured behaves exactly as if this
// middleware weren't there at all.
func (a *Authenticator) Guard(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := httpx.FromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		claims, err := a.introspect(r.Context(), token)
		if err != nil {
			log.Printf("svcauth: introspect: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ident, ok := identityFromClaims(claims, r.Header.Get(OnBehalfOfHeader))
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid or expired API token")
			return
		}
		tenantID, err := a.tenants.GetByZitadelOrgID(r.Context(), ident.orgID)
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid or expired API token")
			return
		}
		if err != nil {
			log.Printf("svcauth: resolve org %s to a tenant: %v", ident.orgID, err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		rc := httpx.RequestContext{TenantID: tenantID, Actor: ident.actor}
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestContext(r.Context(), rc)))
	})
}

func bearerToken(r *http.Request) (token string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token = strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

// introspect calls Zitadel's token-introspection endpoint (RFC 7662),
// authenticating the call itself as a.cfg's own API-application
// credential (Basic auth) — the caller being introspected (the
// connector's token) never needs to be a client Zitadel recognizes on
// its own, only a's credential does. A non-nil error here means the
// introspection call itself failed (network/transport/Zitadel-side
// problem) — an internal error, distinct from the token simply being
// invalid/expired, which is a normal, successful (200) response with
// "active": false.
func (a *Authenticator) introspect(ctx context.Context, token string) (map[string]any, error) {
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", "access_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.introspectionEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build introspection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(a.cfg.ClientID, a.cfg.ClientSecret)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call introspection endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("introspection endpoint returned status %d", resp.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, fmt.Errorf("decode introspection response: %w", err)
	}
	return claims, nil
}

// tokenIdentity is what a valid, active, correctly-roled introspection
// response resolves to — everything Guard needs to build a
// httpx.RequestContext, short of the org->tenant lookup (which needs a
// DB round trip Guard itself performs).
type tokenIdentity struct {
	orgID string
	actor audit.Actor
}

// identityFromClaims validates an introspection response and extracts
// the caller's identity. ok=false covers every way a Bearer token can
// be genuinely invalid for this purpose: inactive/expired/revoked, no
// tenant_integration role at all (e.g. a human's tenant_member-only ID
// token replayed here by mistake — see local.universal_core_roles'
// own comment in uc-infra's main.tf for why that's a distinct role, not
// reused), granted in more than one org (refused the same way
// webauth's orgIDFromClaims refuses ambiguous multi-org membership,
// rather than guessing), or missing a subject — every one of these
// must fail closed as an ordinary 401, not be treated as a server
// error.
func identityFromClaims(claims map[string]any, onBehalfOf string) (tokenIdentity, bool) {
	active, _ := claims["active"].(bool)
	if !active {
		return tokenIdentity{}, false
	}
	orgID, ok := orgIDForRole(claims, tenantIntegrationRole)
	if !ok {
		return tokenIdentity{}, false
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return tokenIdentity{}, false
	}

	actorID := "svc:" + sub
	if trimmed := strings.TrimSpace(onBehalfOf); trimmed != "" {
		actorID = trimmed
	}
	return tokenIdentity{
		orgID: orgID,
		actor: audit.Actor{Type: audit.ActorHuman, ID: actorID},
	}, true
}

// orgIDForRole extracts the single Zitadel org id granted role in the
// project-roles claim. Near-identical in shape to internal/webauth's
// own orgIDFromClaims (same claim, same "exactly one distinct org or
// refuse" reasoning for ambiguous multi-org grants) but deliberately not
// shared code: this one is restricted to one specific role rather than
// "any role asserted at all" — the whole reason tenant_integration
// exists as its own role instead of reusing tenant_member (see
// zitadelProjectRolesClaim's own doc comment).
func orgIDForRole(claims map[string]any, role string) (orgID string, ok bool) {
	val, present := claims[zitadelProjectRolesClaim]
	if !present {
		return "", false
	}
	roles, isMap := val.(map[string]any)
	if !isMap {
		return "", false
	}
	orgsForRole, present := roles[role]
	if !present {
		return "", false
	}
	orgMap, isMap := orgsForRole.(map[string]any)
	if !isMap || len(orgMap) != 1 {
		return "", false
	}
	for id := range orgMap {
		return id, true
	}
	return "", false // unreachable
}
