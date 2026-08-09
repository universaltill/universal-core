// The help manual's static-asset route (uc-infra#145 — screenshots
// captured from internal/e2e's real browser, ADR-0023). A screenshot
// referenced from a topic's ![alt](url) markdown (internal/help/
// markdown.go) needs somewhere to actually load from; this is that
// somewhere.
package api

import (
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/httpx"
)

// helpAssetContentTypes is an explicit allowlist, not mime.TypeByExtension
// (independent review, uc-infra#145): this route serves whatever's under
// internal/help/content/assets/, and nothing today enforces that tree only
// ever holds the JPEGs TestCaptureHelpScreenshots writes — deriving
// Content-Type from "whatever extension happens to be in the tree" would
// mean a stray .html file dropped there some day gets served as
// same-origin text/html with no auth, not just a wrong image type. A
// fixed map keeps this route incapable of serving anything but an image,
// regardless of what future content ever lands under content/assets/.
var helpAssetContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
}

// serveHelpAsset is GET /help/assets/{path...} — a Go 1.22+ trailing
// wildcard resolved via help.Asset, the same "content/assets/<path>"
// embedded-tree lookup that package documents.
//
// Registered UNAUTHENTICATED, in Routes' unauthenticated block alongside
// layout.go's serveHTMX/serveCSS — deliberately, not an oversight:
//   - This content is generic, not tenant-specific. Every captured
//     screenshot (internal/e2e's TestCaptureHelpScreenshots) is taken
//     against a tenant literally named "Demo Organization", never a real
//     tenant, so there is nothing here for auth to protect — the same
//     reasoning that lets htmx.min.js/app.css skip auth ("a static asset
//     with no tenant-specific content").
//   - This is NOT the same call as the help TOPICS themselves
//     (help.go's renderHelpTopic/renderHelpIndex, which DO stay
//     authz-gated): a topic id can reveal which entity types/features a
//     tenant has access to, and ADR-0023 requires a denied topic to 404
//     rather than reveal it exists. A screenshot's own filename
//     (list/en.jpg, form/en.jpg, ...) carries no such information — it
//     names a generic layout family, not a tenant's data.
//   - Gating this behind auth would only break the image on a page
//     nothing has authenticated to fetch yet, for zero confidentiality
//     benefit, mirroring serveHTMX/serveCSS's own reasoning exactly.
//
// Cache-Control is "public, max-age=3600", NOT the
// "immutable, max-age=31536000" htmx.min.js/app.css use: those are
// either content-hashed into their own URL (app.css) or a pinned,
// rarely-changing vendored file (htmx.min.js), so a change is always a
// new URL. A screenshot's URL is stable
// (content/assets/<family>/<locale>.jpg) and DOES change in place
// whenever TestCaptureHelpScreenshots is re-run — an immutable header
// here would leave a stale screenshot cached for up to a year past a
// real UI change, the exact bug appCSSPath's own doc comment describes
// app.css having had before it was content-hashed.
func serveHelpAsset(w http.ResponseWriter, r *http.Request) {
	assetPath := r.PathValue("path")
	ctype, ok := helpAssetContentTypes[strings.ToLower(path.Ext(assetPath))]
	if !ok {
		// An extension outside the allowlist above is rejected before
		// even touching help.Asset — this route must never become a way
		// to serve an arbitrary file type just because something with an
		// unexpected extension ended up under content/assets/.
		httpx.WriteError(w, http.StatusNotFound, "help asset not found")
		return
	}
	data, ok := help.Asset(assetPath)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "help asset not found")
		return
	}
	w.Header().Set("Content-Type", ctype)
	// nosniff: this route's Content-Type is already a fixed allowlist
	// (helpAssetContentTypes above), so there's nothing here for a
	// browser to usefully "sniff" — this only forecloses a browser ever
	// reinterpreting the response as something other than what this
	// route declared, defense in depth on an unauthenticated route
	// (independent review, uc-infra#145).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if _, err := w.Write(data); err != nil {
		log.Printf("api: serve help asset %s: %v", assetPath, err)
	}
}
