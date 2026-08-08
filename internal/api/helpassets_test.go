package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/universaltill/universal-core/internal/help"
	"github.com/universaltill/universal-core/internal/i18n"
)

// helpAssetTestHandler builds a Handler with no router at all (nil) — GET
// /help/assets/{path...} is registered unauthenticated in Routes and
// never touches h.router (see helpassets.go's serveHelpAsset, a free
// function with no Handler receiver), so this route's own tests don't
// need a real Postgres the way most of this package's tests do (they
// exercise entity/form CRUD, which does).
func helpAssetTestHandler(t *testing.T) *Handler {
	t.Helper()
	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	return New(nil, catalog, nil, nil, nil, nil, nil)
}

// TestAPI_HelpAsset_ServesExistingAssetUnauthenticated is the real proof
// (not just a doc-comment claim) that GET /help/assets/{path...} is
// reachable with NO auth headers set at all — unlike every other page
// route in this package, which needs X-Tenant-ID/X-Actor-ID (or a real
// session) to get past auth()/authPage(). httptest.NewRequest sets no
// headers beyond what's passed explicitly, so the plain request built
// here already IS the unauthenticated case; this test also checks the
// response is byte-identical to help.Asset's own read of the same path,
// and that Content-Type/Cache-Control are set correctly.
func TestAPI_HelpAsset_ServesExistingAssetUnauthenticated(t *testing.T) {
	wantData, ok := help.Asset("list/en.jpg")
	if !ok {
		t.Fatal("help.Asset(\"list/en.jpg\") not found — run CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v first")
	}

	h := helpAssetTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	// Deliberately built with httptest.NewRequest directly, NOT this
	// package's own newRequest helper (which would still leave headers
	// unset here, but the point is to make "no auth headers at all" the
	// visibly obvious thing about this request, not an accidental
	// byproduct of which helper was picked).
	req := httptest.NewRequest(http.MethodGet, "/help/assets/list/en.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/assets/list/en.jpg (no auth headers) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want %q", ct, "image/jpeg")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=3600")
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", nosniff, "nosniff")
	}
	if !bytes.Equal(rec.Body.Bytes(), wantData) {
		t.Errorf("response body (%d bytes) does not match help.Asset's own %d bytes for the same path", rec.Body.Len(), len(wantData))
	}
}

// TestAPI_HelpAsset_MissingIs404 confirms a path with no backing
// embedded file 404s via the normal JSON error envelope, rather than
// panicking or falling through to some other route.
func TestAPI_HelpAsset_MissingIs404(t *testing.T) {
	h := helpAssetTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/help/assets/list/does-not-exist.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /help/assets/list/does-not-exist.jpg = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_HelpAsset_EncodedTraversalRejected is the regression proof
// independent review (uc-infra#145) asked for: a LITERAL ".." in the URL
// never reaches this handler at all (net/http's ServeMux path-cleans and
// redirects before routing), so the real vector worth testing is a
// PERCENT-ENCODED "../" — net/http unescapes {path...} wildcard captures
// AFTER cleaning has already run, so an encoded form reaches
// r.PathValue("path") un-cleaned. help.Asset's own doc comment explains
// why this is still safe (embed.FS.Open validates the joined name via
// fs.ValidPath) — this test proves it through the real route + the real
// embedded content, not a synthetic fs.FS, and for both an encoded ".."
// segment and an encoded "../" separator.
func TestAPI_HelpAsset_EncodedTraversalRejected(t *testing.T) {
	h := helpAssetTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	for _, raw := range []string{
		"/help/assets/%2e%2e/%2e%2e/help/help.go",
		"/help/assets/..%2f..%2fhelp/help.go",
		"/help/assets/%2e%2e/README.md",
	} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, raw, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404 (a percent-encoded \"../\" must never escape content/assets): %s", raw, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAPI_HelpAsset_UnexpectedExtensionRejected confirms the
// Content-Type allowlist (helpassets.go's helpAssetContentTypes,
// independent review uc-infra#145) rejects an extension outside it
// before ever calling help.Asset — this route must never become a way
// to serve an arbitrary file type based on whatever happens to be under
// content/assets/.
func TestAPI_HelpAsset_UnexpectedExtensionRejected(t *testing.T) {
	h := helpAssetTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/help/assets/list/en.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /help/assets/list/en.html = %d, want 404 (extension outside helpAssetContentTypes)", rec.Code)
	}
}

// TestAPI_HelpAsset_RouteWinsOverTopicWildcard mirrors
// TestAPI_HelpSearchRouteWinsOverTopicWildcard (help_test.go): the
// literal "assets" segment in "/help/assets/{path...}" must outrank the
// overlapping "/help/{topicID...}" wildcard registered elsewhere in
// Routes, the same net/http "more specific pattern wins" rule that test
// already confirms for /help/search. A regression here would silently
// route every /help/assets/* request into renderHelpTopic instead
// (topicID="assets/list/en.jpg"), which 404s for an entirely different,
// unintended reason (no such help TOPIC, not "no such asset").
func TestAPI_HelpAsset_RouteWinsOverTopicWildcard(t *testing.T) {
	if _, ok := help.Asset("list/en.jpg"); !ok {
		t.Fatal("help.Asset(\"list/en.jpg\") not found — run CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v first")
	}

	h := helpAssetTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/help/assets/list/en.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// renderHelpTopic (behind authPage) would redirect/401 an
	// unauthenticated request rather than ever reaching 200 with an
	// image/jpeg body — so a 200 with the right content type is itself
	// proof this landed on serveHelpAsset, not renderHelpTopic.
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help/assets/list/en.jpg = %d, want 200 from serveHelpAsset (would 401/redirect if it fell through to authPage(renderHelpTopic)): %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want %q (would be text/html from the page shell if this fell through to renderHelpTopic)", ct, "image/jpeg")
	}
}
