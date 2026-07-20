package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
)

// htmxJS is a vendored, pinned copy of htmx.org 2.0.4
// (https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js, sha256
// e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447) —
// self-hosted rather than loaded from a CDN at runtime, matching this
// kernel's general preference for a minimal, controlled dependency
// footprint (see csvimport.go's own doc comment on the same principle)
// and, more directly, so the app has zero runtime dependency on any
// third-party host being reachable.
//
//go:embed static/htmx.min.js
var htmxJS []byte

// serveHTMX serves the vendored htmx.min.js. Registered unauthenticated
// (outside httpx.DevAuth) in Routes — it's a static asset with no
// tenant-specific content, gating it behind auth would only break the
// page that needs it before auth can even run.
func serveHTMX(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(htmxJS); err != nil {
		log.Printf("api: serve htmx.min.js: %v", err)
	}
}

// appCSS is the kernel's one global stylesheet — every page (dashboard,
// welcome, forms, list views, the import wizard) shares it via shellTmpl
// below, the same "one shared thing, not per-page styling" reasoning as
// htmxJS. Deliberately plain (no build step, no framework) — see
// static/app.css's own header comment.
//
//go:embed static/app.css
var appCSS []byte

// appCSSPath embeds a content hash into app.css's own URL
// (/static/app-{hash}.css), computed once at process start. Unlike
// htmxJS (a pinned, rarely-changing vendored file, where a stable
// filename + immutable cache header is correct), app.css changes on
// almost every deploy during active development — serving it from a
// fixed "/static/app.css" URL with a one-year immutable cache header
// (the bug this fixed) meant a browser that had ever loaded the page
// before kept serving a stale, pre-hub, pre-i18n-switcher stylesheet
// for up to a year, never even revalidating: exactly the "no circles,
// just plain text" symptom Farshid saw. Baking the hash into the URL
// means every content change is automatically a new URL, so the long
// immutable cache header becomes safe again rather than the bug.
var appCSSPath = func() string {
	sum := sha256.Sum256(appCSS)
	return "/static/app-" + hex.EncodeToString(sum[:])[:12] + ".css"
}()

// serveCSS serves the vendored app.css at its content-hashed path (see
// appCSSPath). Registered unauthenticated, same reasoning as serveHTMX:
// a static asset with no tenant-specific content, needed before auth
// can even render an error page.
func serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(appCSS); err != nil {
		log.Printf("api: serve app.css: %v", err)
	}
}

// shellTmpl wraps a page fragment in the minimal HTML document a real
// browser needs to actually run htmx: without a real <script> tag
// loading htmx.js somewhere, every hx-* attribute this kernel's
// templates render (formrender, the import wizard) is inert markup — a
// browser has no code to interpret them. Found by internal/e2e's first
// real-browser test: every "verified end-to-end" claim before that test
// existed was verified via curl, which doesn't execute JavaScript and
// so could never have caught this — the fragments were correct, nothing
// ever loaded the runtime that makes hx-post/hx-get/hx-target work.
//
// Only wraps the *first* page a browser navigates to for a given
// entityType (renderForm, importUploadPage) — every htmx-swap response
// (importPreview, importCommit, createRecord, etc.) must keep returning
// a bare fragment, since htmx replaces an existing element's innerHTML/
// outerHTML with exactly that response; wrapping a swap response in a
// full <html> document would break the swap, not fix anything.
//
// Nav is pre-rendered HTML (see nav.go's renderNav) so shellTmpl itself
// never needs to know about tenants/modules/auth state — it's just
// layout, same separation formrender already keeps between rendering
// and the registry lookups that feed it.
//
// lang/dir on <html> is not cosmetic: without dir="rtl" for Arabic, the
// page is still laid out left-to-right underneath Arabic text — i18n
// strings translated but the surrounding layout still reading the wrong
// direction is arguably worse than not translating at all. See
// locale.go's localeDir.
var shellTmpl = template.Must(template.New("shell").Parse(fmt.Sprintf(`<!doctype html>
<html lang="{{.Lang}}" dir="{{.Dir}}">
<head>
<meta charset="utf-8">
<link rel="stylesheet" href="%s">
<script src="/static/htmx.min.js"></script>
</head>
<body>
{{.Nav}}
<main class="uc-container">
{{.Body}}
</main>
</body>
</html>
`, appCSSPath)))

type shellView struct {
	Lang string
	Dir  string
	Nav  template.HTML
	Body template.HTML
}

// renderShell writes fragment wrapped in shellTmpl, with nav as the
// page's top chrome and locale driving the document's lang/dir. nav and
// fragment are already-rendered, already-escaped HTML (nav from
// renderNav, fragment from formrender/importTmpl/dashboardTmpl, all
// html/template output), not raw user input — passed as template.HTML
// deliberately, the same trust boundary formrender's own Render already
// crossed once for this exact content.
func renderShell(w http.ResponseWriter, locale string, nav, fragment template.HTML) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := shellView{Lang: locale, Dir: localeDir(locale), Nav: nav, Body: fragment}
	if err := shellTmpl.Execute(w, view); err != nil {
		return fmt.Errorf("render page shell: %w", err)
	}
	return nil
}
