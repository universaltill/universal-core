package api

import (
	_ "embed"
	"fmt"
	"html/template"
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
	w.Write(htmxJS)
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
var shellTmpl = template.Must(template.New("shell").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<script src="/static/htmx.min.js"></script>
</head>
<body>
{{.}}
</body>
</html>
`))

// renderShell writes fragment wrapped in shellTmpl. fragment is already-
// rendered, already-escaped HTML (from formrender or importTmpl, both of
// which use html/template themselves), not raw user input — passed as
// template.HTML deliberately, the same trust boundary formrender's own
// Render already crossed once for this exact content.
func renderShell(w http.ResponseWriter, fragment string) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := shellTmpl.Execute(w, template.HTML(fragment)); err != nil { //nolint:gosec // fragment is our own already-escaped template output, not raw user input
		return fmt.Errorf("render page shell: %w", err)
	}
	return nil
}
