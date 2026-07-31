// Path-prefix tenancy (/t/{slug}/…, universal-core#25, ADR-0011): the
// browser-facing canonical URL states which tenant it shows, and the
// server cross-checks that statement against the session instead of
// silently trusting either side. Three moving parts, all here:
//
//   - tenantPrefixed: the /t/{slug}/ subtree — resolves the slug,
//     cross-checks it against the request's tenant, and re-dispatches
//     into the same mux with the prefix stripped (routes are registered
//     once, not twice).
//   - canonicalPage: authenticated browser page-GETs on un-prefixed
//     paths redirect to their prefixed form (bookmarks keep working,
//     the URL ends up stating the tenant).
//   - guardTenantHeader: HTMX/API calls stay un-prefixed and
//     session-resolved, but pages stamp their tenant into an
//     X-UC-Tenant request header (layout.go's configRequest hook), and
//     any mismatch with the session's active tenant is refused — the
//     stale-tab hazard (two tabs, two tenants, one cookie) can cost a
//     reload, never a mutation in the wrong tenant.
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
)

// SetTenantRepo wires the control-plane tenant directory the prefix
// router resolves slugs against — same post-construction-setter shape
// as SetMemberMgmt (which also sets it; either order works, last write
// wins with the same repo in practice).
func (h *Handler) SetTenantRepo(tenants *data.TenantRepo) {
	h.tenants = tenants
}

type prefixDispatchKey struct{}

// dispatchedViaPrefix marks a request tenantPrefixed has already
// resolved — canonicalPage must not re-redirect it (that would loop:
// strip prefix → canonicalize → prefix → …).
func dispatchedViaPrefix(ctx context.Context) bool {
	v, _ := ctx.Value(prefixDispatchKey{}).(bool)
	return v
}

// tenantPrefixed serves the /t/{slug}/ subtree. It runs behind the same
// auth chain as every other route, so rc is always present and already
// carries the token/session-resolved tenant — the cross-check compares
// that against what the URL claims (ADR-0011's rules):
//
//   - slug's tenant == request's tenant → strip the prefix and
//     re-dispatch into the same mux;
//   - GET for a tenant the webauth session may access but isn't active
//     → switch (re-seal the cookie) and redirect to the same URL, which
//     then hits the case above — this is how a shared /t/… link or the
//     switcher lands somewhere else;
//   - any other mismatch on GET → 404, indistinguishable from a slug
//     that doesn't exist (URLs must not confirm which tenants exist);
//   - mismatch on a non-GET → 409 with a localized message: a mutation
//     never silently crosses tenants.
func (h *Handler) tenantPrefixed(inner *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc, ok := requestContext(w, r)
		if !ok {
			return
		}
		if h.tenants == nil {
			http.NotFound(w, r)
			return
		}
		slug := r.PathValue("slug")
		tid, err := h.tenants.GetBySlug(r.Context(), slug)
		notFound := errors.Is(err, data.ErrNotFound)
		if err != nil && !notFound {
			writeInternalError(w, "resolve tenant slug", err)
			return
		}
		writeMismatch := func() {
			locale := localeFromRequest(w, r)
			httpx.WriteError(w, http.StatusConflict, h.catalog.T(locale, "tenanturl.mismatch"))
		}
		if notFound {
			if r.Method == http.MethodGet {
				http.NotFound(w, r)
				return
			}
			// Non-GET gets the SAME 409 an existing-but-inaccessible slug
			// gets below — an unknown slug must be indistinguishable from
			// an inaccessible one on every method, or the status split
			// becomes a tenant-name oracle (slugs derive from customer
			// names; independent review, 2026-07-31, caught the original
			// 404-here leaking exactly that).
			writeMismatch()
			return
		}
		if rc.TenantID == tid {
			rest, ok := strings.CutPrefix(r.URL.Path, "/t/"+slug)
			if !ok {
				// Should be unreachable (ServeMux matched the pattern),
				// but the failure mode of proceeding anyway would be
				// re-entering this handler with an identical path —
				// unbounded recursion. A 404 is the safe wrong answer.
				http.NotFound(w, r)
				return
			}
			if rest == "" {
				rest = "/"
			}
			r2 := r.Clone(context.WithValue(r.Context(), prefixDispatchKey{}, true))
			r2.URL.Path = rest
			// Preserve the original escaping for the re-dispatched
			// remainder (slugs are [a-z0-9-], so the prefix's escaped and
			// unescaped forms are identical and CutPrefix is valid on
			// either): clearing RawPath outright would silently decode
			// %2F-style segments differently than the un-prefixed route.
			if rawRest, rawOK := strings.CutPrefix(r.URL.EscapedPath(), "/t/"+slug); rawOK && rawRest != rest {
				r2.URL.RawPath = rawRest
			} else {
				r2.URL.RawPath = ""
			}
			inner.ServeHTTP(w, r2)
			return
		}
		if r.Method == http.MethodGet {
			// One auto-switch attempt only: if the cookie the previous
			// redirect set never came back (cookies disabled, Secure
			// cookie over plain HTTP), a second pass lands here again —
			// 404 instead of an infinite 302 loop.
			if r.URL.Query().Get("_ucswitched") == "" && h.auth.SwitchIfAccessible(w, r, tid) {
				u := *r.URL
				q := u.Query()
				q.Set("_ucswitched", "1")
				u.RawQuery = q.Encode()
				// The Set-Cookie from the switch rides this redirect; the
				// retried request's session then matches the slug.
				http.Redirect(w, r, u.RequestURI(), http.StatusFound)
				return
			}
			http.NotFound(w, r)
			return
		}
		writeMismatch()
	}
}

// canonicalPage redirects an authenticated browser's un-prefixed page
// GET to its /t/{slug}/… form. Deliberately narrow: real webauth
// sessions only (DevAuth header traffic and machine tokens keep their
// existing un-prefixed contract), full page loads only (HX-Request
// partials stay session-resolved), and never a request tenantPrefixed
// already dispatched. A slug-lookup failure just serves the page
// un-prefixed — canonicalization is presentation, not access control.
func (h *Handler) canonicalPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && !dispatchedViaPrefix(r.Context()) &&
			r.Header.Get("HX-Request") == "" && h.tenants != nil && h.auth.Enabled() {
			if rc, ok := h.auth.TryContext(r); ok {
				if slug, err := h.tenants.SlugByID(r.Context(), rc.TenantID); err == nil {
					http.Redirect(w, r, "/t/"+slug+r.URL.RequestURI(), http.StatusFound)
					return
				} else {
					log.Printf("api: canonicalize: slug for tenant %s: %v", rc.TenantID, err)
				}
			}
		}
		next(w, r)
	}
}

// guardTenantHeader refuses a request whose X-UC-Tenant stamp (set by
// the page that issued it — layout.go) disagrees with the tenant the
// request actually resolved to. Runs innermost in the auth chain so rc
// is final. No header means no claim (plain API clients, connectors,
// pre-#25 pages) — nothing to check.
func (h *Handler) guardTenantHeader(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if claimed := r.Header.Get("X-UC-Tenant"); claimed != "" {
			if rc, ok := httpx.FromContext(r.Context()); ok && rc.TenantID != claimed {
				locale := localeFromRequest(w, r)
				httpx.WriteError(w, http.StatusConflict, h.catalog.T(locale, "tenanturl.mismatch"))
				return
			}
		}
		next(w, r)
	}
}
