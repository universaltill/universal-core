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
func DevAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !DevAuthEnabled() {
			WriteError(w, http.StatusUnauthorized, "no auth backend configured")
			return
		}
		tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
		actorID := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
		if tenantID == "" || actorID == "" {
			WriteError(w, http.StatusUnauthorized, "X-Tenant-ID and X-Actor-ID headers are required (insecure dev-only auth stopgap)")
			return
		}
		if !tenantIDPattern.MatchString(tenantID) {
			WriteError(w, http.StatusUnauthorized, "X-Tenant-ID is not a valid tenant id")
			return
		}
		ctx := withRequestContext(r.Context(), RequestContext{
			TenantID: tenantID,
			Actor:    audit.Actor{Type: audit.ActorHuman, ID: actorID},
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
