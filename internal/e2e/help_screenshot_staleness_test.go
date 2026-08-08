// The real-browser half of the help-screenshot staleness check
// (uc-infra#145). internal/help/imagediff_test.go already proves
// ImageDiffScore's own math in isolation (identical images score 0,
// solid-black-vs-white scores ~1, a small uniform offset stays below
// HelpScreenshotStaleThreshold) — pure Go, no browser, always runs.
// What only a real browser adds is: does a FRESH re-capture of an
// UNCHANGED page, taken right now in this sandbox, actually read as
// "the same page" — and does the same check have real power to flag a
// genuinely different page.
//
// Design (independent review, uc-infra#145 — replaces this file's
// original design): a single absolute ImageDiffScore threshold cannot
// separate "same page, different renderer/JPEG noise" from "genuinely
// different page" on its own. Measured for real against all 28 pairs of
// the 8 shipped captures plus synthetic 1px/2px layout-shift
// perturbations: the two smallest genuinely-different-page pairs
// (list/en vs wizard/en at 0.0060, list/ar vs wizard/ar at 0.0056)
// scored BELOW a plain 1-pixel vertical shift of an otherwise-unchanged
// page (0.0068) — the two distributions overlap, so no single scalar
// threshold on this metric is a reliable separator.
//
// The primary check here is therefore RELATIVE, not absolute: a fresh
// re-capture of page P must score closer to P's own checked-in file than
// to any of the other 7 checked-in files. This is renderer-noise-robust
// by construction — it only requires that ordinary re-rendering noise
// (JPEG requantization, font hinting) stays smaller than the difference
// between two actually-different pages, which held for every one of the
// 8 combos measured here, not that any specific noise level stays under
// a fixed number tuned against one environment. What this trades away:
// it catches "this page now looks like a DIFFERENT one of the other 7"
// (a gross regression — wrong content, broken layout collapsing to
// another page's shape, a blank/error page) but does NOT catch a subtle
// change confined to one page (e.g. a single field's label changing).
// HelpScreenshotStaleThreshold (internal/help/imagediff.go) backs a
// secondary, generous absolute sanity ceiling on top of that — the
// fresh/own-checked-in score itself must also stay low in absolute
// terms, catching a wholesale mismatch (e.g. the wrong file checked in)
// that the relative comparison alone wouldn't if every one of the 8
// checked-in files were somehow equally wrong.
//
// Honesty note (do not remove, per this card's own acceptance
// criteria): this design is spot-checked against one real run in this
// sandbox, covering all 8 combos. It has NOT been validated across
// repeated runs in this sandbox, nor against a different CI runner's own
// font rendering (GitHub Actions' ubuntu-latest, where this suite also
// runs in CI). If it proves too noisy there — a false "stale" on an
// unchanged page — that will show up as a real CI failure in THIS test
// to investigate and retune, not a silently swallowed skip: this test is
// gated the same TEST_DATABASE_URL/Chrome way as every other test in
// this package (testServer/browserCtx/findBrowser), NOT behind
// CAPTURE_HELP_SCREENSHOTS — it runs as part of the normal
// `go test ./... -p 1` pass.
package e2e

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/universaltill/universal-core/internal/help"
)

// loadCheckedInHelpScreenshots decodes every checked-in capture under
// internal/help/content/assets/<family>/<locale>.jpg, keyed by
// "<family>/<locale>" — the fixed comparison set both legs of
// TestHelpScreenshot_StalenessCheck_RealBrowser measure against.
func loadCheckedInHelpScreenshots(t *testing.T) map[string]image.Image {
	t.Helper()
	out := map[string]image.Image{}
	for _, fam := range helpScreenshotFamilies {
		for _, locale := range helpScreenshotLocales {
			key := fam.name + "/" + locale
			p := filepath.Join("..", "help", "content", "assets", fam.name, locale+".jpg")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read checked-in %s: %v (run CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v first)", p, err)
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("decode checked-in %s: %v", p, err)
			}
			out[key] = img
		}
	}
	return out
}

// TestHelpScreenshot_StalenessCheck_RealBrowser is described in this
// file's own header comment above. For every one of the 8 shipped
// combos: re-captures it fresh (the exact same captureHelpScreenshotJPEG
// function TestCaptureHelpScreenshots itself calls — comparing two
// DIFFERENT capture pipelines would prove nothing about whether the
// checked-in file is stale, only that the pipelines differ), then
// asserts it scores closer to its own checked-in file than to any other
// combo's checked-in file, AND stays under the generous absolute sanity
// ceiling.
func TestHelpScreenshot_StalenessCheck_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, _ := demoOrgHelpServer(t)
	ctx := browserCtx(t, tenantID)

	checkedIn := loadCheckedInHelpScreenshots(t)

	for _, fam := range helpScreenshotFamilies {
		for _, locale := range helpScreenshotLocales {
			key := fam.name + "/" + locale
			t.Run(key, func(t *testing.T) {
				url := srv.URL + fmt.Sprintf(fam.pathTemplate, locale)
				fresh, err := captureHelpScreenshotJPEG(ctx, url, fam.waitSelector)
				if err != nil {
					t.Fatalf("re-capture %s: %v", key, err)
				}
				freshImg, err := jpeg.Decode(bytes.NewReader(fresh))
				if err != nil {
					t.Fatalf("decode fresh capture of %s: %v", key, err)
				}

				ownScore := help.ImageDiffScore(freshImg, checkedIn[key])
				t.Logf("%s: fresh vs own checked-in = %.4f (sanity ceiling %.4f)", key, ownScore, help.HelpScreenshotStaleThreshold)
				if ownScore >= help.HelpScreenshotStaleThreshold {
					t.Errorf("%s: fresh re-capture (unchanged page) scored %.4f against its own checked-in file — over the %.4f sanity ceiling, either a real UI regression or a wholesale mismatch", key, ownScore, help.HelpScreenshotStaleThreshold)
				}

				var minOtherScore float64 = -1
				var minOtherKey string
				for otherKey, otherImg := range checkedIn {
					if otherKey == key {
						continue
					}
					s := help.ImageDiffScore(freshImg, otherImg)
					if minOtherScore < 0 || s < minOtherScore {
						minOtherScore = s
						minOtherKey = otherKey
					}
				}
				t.Logf("%s: closest OTHER checked-in combo = %s at %.4f", key, minOtherKey, minOtherScore)
				if ownScore >= minOtherScore {
					t.Errorf("%s: fresh re-capture scored closer to (or tied with) a DIFFERENT page (%s, %.4f) than to its own checked-in file (%.4f) — this is the real staleness signal, and it just failed: either %s genuinely changed to resemble %s, or the checked-in file for %s is wrong", key, minOtherKey, minOtherScore, ownScore, key, minOtherKey, key)
				}
			})
		}
	}
}
