// Screenshot capture for the in-product help manual (ADR-0023,
// uc-infra#145 — "Help screenshots: capture from internal/e2e's real
// browser, with a staleness check and a size budget"). This file holds
// the shared navigate/viewport/capture code path
// (captureHelpScreenshotJPEG) plus the one-command capture tool
// (TestCaptureHelpScreenshots) that (re)generates every JPEG checked in
// under internal/help/content/assets/. help_screenshot_staleness_test.go
// reuses the exact same capture function to re-derive a fresh screenshot
// and diff it against the checked-in file — deliberately the same code
// path, not a second reimplementation that could silently drift from
// what actually produced the committed files.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/api"
	"github.com/universaltill/universal-core/internal/i18n"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/testexec"
)

// helpScreenshotFamily is one of the 4 layout families this card
// captures — list/form/help/wizard, each represented by one real
// generated page (Item is the entity every capture uses: testServer's
// own foundation+purchasing publish already seeds it, so it's a real
// entity type, not a fixture invented only for this test).
type helpScreenshotFamily struct {
	name         string // matches the asset storage directory: content/assets/<name>/
	pathTemplate string // formatted with the locale, e.g. "/records/Item?lang=%s"
	waitSelector string // a real, stable selector on that page — never guessed
}

// helpScreenshotFamilies is deliberately package-level, not local to
// TestCaptureHelpScreenshots: help_screenshot_staleness_test.go re-uses
// it (specifically helpScreenshotFamilies[0], "list") so the staleness
// check's re-capture targets exactly one of the same 8 combos the
// capture tool itself produces.
var helpScreenshotFamilies = []helpScreenshotFamily{
	{"list", "/records/Item?lang=%s", ".uc-list-toolbar"},
	{"form", "/forms/Item/new?lang=%s", ".uc-form"},
	{"help", "/help?lang=%s", "#uc-help-detail"},
	{"wizard", "/import/Item?lang=%s", "#uc-import-form"},
}

// helpScreenshotLocales is the issue's own locale-policy trade-off: en
// (default) plus ar (one RTL exemplar) per layout family — tr/fa are
// deliberately NOT captured here (out of scope per uc-infra#145).
var helpScreenshotLocales = []string{"en", "ar"}

const (
	helpScreenshotViewportWidth  = 1280
	helpScreenshotViewportHeight = 800

	// helpScreenshotJPEGQuality was tuned against a real capture run in
	// this sandbox (see this card's Dev handback for the measured
	// sizes): high enough that a screenshot documenting real UI stays
	// legible, low enough that all 8 files land comfortably under
	// internal/help/asset_budget_test.go's per-image budget.
	helpScreenshotJPEGQuality = 82
)

// captureHelpScreenshotJPEG navigates ctx's real browser to url, waits
// for waitSelector to be visible, settles briefly, captures a VIEWPORT
// screenshot (not full-page — this is what keeps every file's size
// bounded regardless of how tall a given page's content is), and
// re-encodes it from PNG (chromedp's own capture format) to JPEG at
// helpScreenshotJPEGQuality using only the standard library — no new
// image-processing dependency for a narrow, controlled need, the same
// reasoning this repo's other hand-rolled-over-dependency choices give
// (e.g. internal/help/markdown.go's own doc comment).
//
// Settle wait: this repo's own CSS transitions are all 0.12s/0.15s
// opacity/transform (checked against static/app.css directly), not
// layout-affecting, and WaitVisible above already blocks until real
// content is in the DOM — so a screenshot taken immediately after is not
// expected to be mid-transition in practice. The extra 150ms sleep below
// is deliberate margin past that, not a wait for something known to be
// async: cheap insurance against the one class of flake a fixed-viewport,
// fixed-content capture like this could still have (a font finishing
// its own layout pass, a still-settling CSS transition on a slower
// machine than this one was tuned against), not evidence such a race
// was ever observed here.
func captureHelpScreenshotJPEG(ctx context.Context, url, waitSelector string) ([]byte, error) {
	var pngData []byte
	actions := []chromedp.Action{
		chromedp.EmulateViewport(helpScreenshotViewportWidth, helpScreenshotViewportHeight),
		chromedp.Navigate(url),
		chromedp.WaitVisible(waitSelector, chromedp.ByQuery),
		chromedp.Sleep(150 * time.Millisecond),
		chromedp.CaptureScreenshot(&pngData),
	}
	if err := chromedp.Run(ctx, actions...); err != nil {
		return nil, fmt.Errorf("capture screenshot at %s (waiting on %q): %w", url, waitSelector, err)
	}
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode captured png for %s: %w", url, err)
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: helpScreenshotJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode jpeg for %s: %w", url, err)
	}
	return jpegBuf.Bytes(), nil
}

// writeAssetAtomic writes data to path via a temp-file-then-rename in
// the same directory, so a run killed mid-write (Ctrl-C, OOM, CI job
// cancellation) can never leave a half-written, truncated JPEG sitting
// where a later `git add` would pick it up as if it were a real capture.
func writeAssetAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-capture-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	// os.CreateTemp always creates its file 0600 (owner-only) regardless
	// of umask — fine for the temp file itself, but the renamed-into-
	// place result is a real checked-in asset every other reader of this
	// repo needs to read (and re-clone with), so it needs the ordinary
	// 0644 every other file in this tree gets from git (independent
	// review, uc-infra#145).
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// demoOrgTestServer is testServer's (csv_import_test.go) own wiring —
// foundation + purchasing published, api.Routes-backed httptest server —
// under the tenant name "Demo Organization" instead of testServer's own
// "E2E Tenant". A hard rule, both from CLAUDE.md's seed-data convention
// and from this card's own issue text: a screenshot that ships inside
// the product's own help manual must never show a real client/prospect
// name, so this capture tool gets its own tenant-provisioning variant
// rather than reusing testServer's name.
func demoOrgTestServer(t *testing.T) (srv *httptest.Server, tenantID string, tenantDB *sql.DB) {
	t.Helper()
	router := newTestRouter(t)
	ctx := context.Background()
	actor := humanActor()

	id, err := router.Create(ctx, "Demo Organization", "eu-west")
	if err != nil {
		t.Fatalf("router.Create: %v", err)
	}
	tenantDB, err = router.Get(ctx, id)
	if err != nil {
		t.Fatalf("router.Get: %v", err)
	}
	testexec.DropConnectedDatabase(t, tenantDB)

	if err := foundation.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("foundation.Publish: %v", err)
	}
	if err := purchasing.Publish(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.Publish: %v", err)
	}
	if err := purchasing.PublishForms(ctx, tenantDB, actor); err != nil {
		t.Fatalf("purchasing.PublishForms: %v", err)
	}

	catalog, err := i18n.Load("en")
	if err != nil {
		t.Fatalf("load i18n catalog: %v", err)
	}
	mux := http.NewServeMux()
	api.New(router, catalog, nil, nil, nil, nil, nil).Routes(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, id, tenantDB
}

// TestCaptureHelpScreenshots is the one command that (re)generates every
// screenshot this card ships under internal/help/content/assets/ —
// deterministic given a fixed 1280x800 viewport and a real "Demo
// Organization" tenant. Every family, including "help", is captured
// against the real production help.GetIndex() content (internal/help/
// content/ ships real topics as of uc-infra#147-152) — no fixture
// override (uc-infra#192: an earlier revision wired help.SetIndexForTesting
// in here purely so the "help" family's own screenshot showed a
// populated manual instead of the pre-#147-152 empty state; now that real
// content ships, the checked-in help/{en,ar}.jpg show genuine product
// topics like the other three families already did).
//
// Run it explicitly with:
//
//	CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v
//
// Gated behind CAPTURE_HELP_SCREENSHOTS=1 and skipped otherwise —
// deliberately NOT part of the normal `go test ./...` pass: this test's
// whole job is to overwrite real files under internal/help/content/
// assets/ that the rest of the suite (internal/help's own
// asset_budget_test.go, internal/e2e's own staleness check, the topic-
// image-render test in help_test.go) treats as checked-in fixtures, so
// an ordinary test run must never silently regenerate them. Otherwise
// subject to the exact same TEST_DATABASE_URL/Chrome skip-or-fail rules
// as every other test in this package (testServer/browserCtx/
// findBrowser, csv_import_test.go/browser_discovery_test.go).
func TestCaptureHelpScreenshots(t *testing.T) {
	if os.Getenv("CAPTURE_HELP_SCREENSHOTS") != "1" {
		t.Skip("set CAPTURE_HELP_SCREENSHOTS=1 to (re)generate internal/help/content/assets/ — see this test's own doc comment for the exact command")
	}
	withDevAuthEnabled(t)
	srv, tenantID, _ := demoOrgTestServer(t)
	// browserCtxWithTimeout, not the package's plain 30-second browserCtx
	// (independent review, uc-infra#145): this loop does 8 real
	// navigate+capture round trips through ONE shared browser context,
	// and 30 seconds is that context's single absolute deadline, not a
	// per-iteration budget — see browserCtxWithTimeout's own doc comment
	// (csv_import_test.go) for how this was found (a real `-race` run
	// failing on the loop's last combo).
	ctx := browserCtxWithTimeout(t, tenantID, 3*time.Minute)

	assetsRoot := filepath.Join("..", "help", "content", "assets")

	var totalBytes int
	for _, fam := range helpScreenshotFamilies {
		for _, locale := range helpScreenshotLocales {
			name := fam.name + "/" + locale
			t.Run(name, func(t *testing.T) {
				url := srv.URL + fmt.Sprintf(fam.pathTemplate, locale)
				data, err := captureHelpScreenshotJPEG(ctx, url, fam.waitSelector)
				if err != nil {
					t.Fatalf("capture %s: %v", name, err)
				}
				dest := filepath.Join(assetsRoot, fam.name, locale+".jpg")
				if err := writeAssetAtomic(dest, data); err != nil {
					t.Fatalf("write %s: %v", dest, err)
				}
				totalBytes += len(data)
				t.Logf("captured %s: %d bytes -> %s", name, len(data), dest)
			})
		}
	}
	t.Logf("total captured: %d bytes across %d files", totalBytes, len(helpScreenshotFamilies)*len(helpScreenshotLocales))
}
