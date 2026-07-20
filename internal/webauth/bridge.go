package webauth

import (
	"net/http"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/audit"
)

// Guard enforces a real session on every request it wraps: a valid
// session cookie populates the exact same httpx.RequestContext
// DevAuth's spoofable headers do (internal/api's handlers call
// requestContext() and never see which middleware actually ran), and a
// missing/invalid one redirects a browser to /ui/login rather than
// 401ing — the request is retried automatically once login completes
// (ReturnTo), matching how a real product's auth wall behaves.
//
// A nil or disabled Authenticator is a straight pass-through to next —
// this is what makes it safe to wrap unconditionally in Routes()
// alongside DevAuth (see internal/api/handlers.go): a deployment with no
// OIDC app configured behaves exactly as if Guard weren't there at all,
// falling through to DevAuth's own independent fail-closed check.
func (a *Authenticator) Guard(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := a.sessionFromCookie(r)
		if sess == nil {
			a.redirectToLogin(w, r)
			return
		}
		ctx := httpx.WithRequestContext(r.Context(), httpx.RequestContext{
			TenantID: sess.TenantID,
			Actor:    audit.Actor{Type: audit.ActorHuman, ID: sess.Subject},
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
