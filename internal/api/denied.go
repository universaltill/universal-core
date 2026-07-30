package api

import (
	"bytes"
	"errors"
	"html/template"
	"log"
	"net/http"

	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/authz"
)

// The rendered-page half of RBAC denial (ADR-0006's field-level commit).
//
// httpx.WriteError is right for /api/* and for anything a client parses
// (an htmx swap, a CSV download) — a fixed-string JSON envelope the
// global error-toast already knows how to surface. It is wrong for a
// page a person navigated to: a browser showing `{"data":null,"error":
// "access denied"}` as the whole document tells that person nothing
// about what to do next, and does it in English regardless of the locale
// every other string on the site just rendered in. Every user-facing
// surface in this kernel is multilingual (CLAUDE.md); a permission
// refusal is not the one place that stops being true.

// writePageDenied renders a localized 403 page inside the normal shell —
// same nav, same language, same direction (RTL for ar/fa) as any other
// page. rc may be nil for a request with no resolved session, in which
// case the nav degrades to brand-and-language-switcher exactly as
// renderNav already does for an anonymous visitor.
func (h *Handler) writePageDenied(w http.ResponseWriter, r *http.Request, rc *httpx.RequestContext, locale string) {
	var buf bytes.Buffer
	err := deniedTmpl.Execute(&buf, deniedView{
		Title: h.catalog.T(locale, "error.access_denied.title"),
		Body:  h.catalog.T(locale, "error.access_denied.body"),
	})
	if err != nil {
		// The template is a compile-time constant with two plain string
		// fields, so this is unreachable short of a programming error —
		// fall back to the plain envelope rather than writing a 200 with
		// an empty body.
		log.Printf("api: render access-denied page: %v", err)
		httpx.WriteError(w, http.StatusForbidden, "access denied")
		return
	}
	// WriteHeader before renderShell, which writes the body immediately:
	// a 200 would otherwise be committed by the first Write and the
	// status could never be corrected.
	w.WriteHeader(http.StatusForbidden)
	if err := h.renderShell(w, locale, h.renderNav(r, rc, locale), template.HTML(buf.String())); err != nil {
		log.Printf("api: render access-denied shell: %v", err)
	}
}

// writeCrudPageError is writeCrudError for a server-rendered page: an
// authz denial becomes the localized 403 page above, anything else stays
// a logged, generic internal error (never the raw DB text — see
// writeInternalError).
func (h *Handler) writeCrudPageError(w http.ResponseWriter, r *http.Request, rc *httpx.RequestContext, locale, logContext string, err error) {
	if errors.Is(err, authz.ErrDenied) {
		h.writePageDenied(w, r, rc, locale)
		return
	}
	writeInternalError(w, logContext, err)
}

// denyPageUnless renders the 403 page and reports false when allowed is
// false, so a handler can gate itself in one line. The error return is
// the permission lookup's own failure (a DB problem), already turned
// into a 500 here — a handler that gets false has nothing left to do
// either way.
func (h *Handler) denyPageUnless(w http.ResponseWriter, r *http.Request, rc *httpx.RequestContext, locale string, allowed bool, err error, logContext string) bool {
	if err != nil {
		writeInternalError(w, logContext, err)
		return false
	}
	if !allowed {
		h.writePageDenied(w, r, rc, locale)
		return false
	}
	return true
}

type deniedView struct {
	Title string
	Body  string
}

var deniedTmpl = template.Must(template.New("denied").Parse(`
<div class="uc-denied">
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
</div>
`))
