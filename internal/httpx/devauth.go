package httpx

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// RequestContext is what a real auth layer populates per request. Real
// Zitadel/OIDC auth is not built yet (see QUEUE.md) — until it exists,
// DevAuth below is the only way to populate one, and it is not a
// security boundary.
type RequestContext struct {
	TenantID string
	Actor    audit.Actor
}

type ctxKey int

const requestContextKey ctxKey = 0

// WithRequestContext attaches rc to ctx — exported so internal/webauth's
// real-login middleware can populate the same RequestContext DevAuth
// does, from a verified session instead of trusted-verbatim headers.
// Both middlewares produce the exact same downstream shape, so
// internal/api's handlers never need to know which one actually ran.
func WithRequestContext(ctx context.Context, rc RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey, rc)
}

// FromContext returns the RequestContext a preceding auth middleware
// (DevAuth, webauth, or a future replacement) attached to ctx.
func FromContext(ctx context.Context) (RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey).(RequestContext)
	return rc, ok
}

// DevAuthEnabled reports whether the insecure dev-auth stopgap is
// allowed to run at all. Re-read from the environment on every call
// (DevAuth checks it per request, not once at startup) — main.go also
// checks it once at startup purely to decide what to log, not to gate
// anything; DevAuth's own per-request check is what actually enforces
// fail-closed behavior.
func DevAuthEnabled() bool {
	return os.Getenv("INSECURE_DEV_AUTH") == "true"
}

// tenantIDPattern matches the shape tenants.id actually is (Postgres
// gen_random_uuid()) — rejecting a malformed X-Tenant-ID here, as a 401,
// means a garbage header value never reaches a query as a malformed
// parameter (which would otherwise surface as a 500 with a raw Postgres
// "invalid input syntax for type uuid" error leaking into the response).
var tenantIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DevAuth is a NOT-A-SECURITY-BOUNDARY stopgap: it trusts the X-Tenant-ID
// and X-Actor-ID request headers verbatim, with zero verification —
// anyone who can reach this server can claim to be any tenant as any
// actor. It exists only so the HTTP layer (internal/api) has something
// real to test/demo against before Zitadel/OIDC auth is wired in.
//
// It fails CLOSED, not open: unless INSECURE_DEV_AUTH=true, every
// request 401s regardless of what headers it carries. A deployment that
// forgets to wire real auth therefore serves nothing through these
// routes rather than silently trusting spoofable headers — the opposite
// of what a permissive default would do.
//
// Composes as a fallback behind a real auth middleware (internal/api
// wraps every route as webauth.Guard(DevAuth(handler))): if a
// RequestContext is already attached — webauth.Guard already verified a
// real session — DevAuth does nothing and passes the request straight
// through, never re-checking headers on top of an already-authenticated
// request. This is also what makes DevAuth's own headers genuinely
// inert the moment real login is configured for a deployment: Guard
// either populates the context itself or redirects before DevAuth ever
// runs, so INSECURE_DEV_AUTH has no effect once webauth.Config.Enabled().
func DevAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		rc, ok := TryDevAuth(r)
		if !ok {
			if !DevAuthEnabled() {
				WriteError(w, http.StatusUnauthorized, "no auth backend configured")
				return
			}
			tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
			if tenantID != "" && !tenantIDPattern.MatchString(tenantID) {
				WriteError(w, http.StatusUnauthorized, "X-Tenant-ID is not a valid tenant id")
				return
			}
			WriteError(w, http.StatusUnauthorized, "X-Tenant-ID and X-Actor-ID headers are required (insecure dev-only auth stopgap)")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithRequestContext(r.Context(), rc)))
	})
}

// TryDevAuth is DevAuth's own header check exposed as a non-enforcing
// peek: it returns ok=false (never writes a response) whenever
// INSECURE_DEV_AUTH isn't set or the headers are missing/malformed,
// instead of 401ing. It exists for callers like the public "/" landing
// page (internal/api/dashboard.go) that want to show an authenticated
// view *if* a session already exists — via either DevAuth's headers or
// webauth's cookie — but must never demand one to render at all. Trust
// model is identical to DevAuth: still gated by DevAuthEnabled(), still
// trusts the headers verbatim, still not a security boundary.
func TryDevAuth(r *http.Request) (RequestContext, bool) {
	if !DevAuthEnabled() {
		return RequestContext{}, false
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	actorID := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
	if tenantID == "" || actorID == "" || !tenantIDPattern.MatchString(tenantID) {
		return RequestContext{}, false
	}
	return RequestContext{
		TenantID: tenantID,
		Actor:    audit.Actor{Type: audit.ActorHuman, ID: actorID},
	}, true
}
