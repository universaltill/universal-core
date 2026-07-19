package httpx

import (
	"context"
	"net/http"
	"os"

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

func withRequestContext(ctx context.Context, rc RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey, rc)
}

// FromContext returns the RequestContext a preceding auth middleware
// (DevAuth, or its eventual Zitadel/OIDC replacement) attached to ctx.
func FromContext(ctx context.Context) (RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey).(RequestContext)
	return rc, ok
}

// DevAuthEnabled reports whether the insecure dev-auth stopgap is
// allowed to run at all — checked once, not per request, so a
// deployment's whole auth posture is visible from one env var rather
// than implied by request headers nobody's watching.
func DevAuthEnabled() bool {
	return os.Getenv("INSECURE_DEV_AUTH") == "true"
}

// DevAuth is a NOT-A-SECURITY-BOUNDARY stopgap: it trusts the X-Tenant-ID
// and X-Actor-ID request headers verbatim, with zero verification —
// anyone who can reach this server can claim to be any tenant as any
// actor. It exists only so the HTTP layer (internal/api) has something
// real to test/demo against before Zitadel/OIDC auth is wired in.
//
// It fails CLOSED, not open: unless the caller has already checked
// DevAuthEnabled() (main.go does, at startup, logging a loud warning and
// registering these routes only if true) every request 401s. A
// deployment that forgets to wire real auth therefore serves nothing
// through these routes rather than silently trusting spoofable headers —
// the opposite of what a permissive default would do.
func DevAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !DevAuthEnabled() {
			WriteError(w, http.StatusUnauthorized, "no auth backend configured")
			return
		}
		tenantID := r.Header.Get("X-Tenant-ID")
		actorID := r.Header.Get("X-Actor-ID")
		if tenantID == "" || actorID == "" {
			WriteError(w, http.StatusUnauthorized, "X-Tenant-ID and X-Actor-ID headers are required (insecure dev-only auth stopgap)")
			return
		}
		ctx := withRequestContext(r.Context(), RequestContext{
			TenantID: tenantID,
			Actor:    audit.Actor{Type: audit.ActorHuman, ID: actorID},
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
