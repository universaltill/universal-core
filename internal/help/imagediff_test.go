package help

import (
	"image"
	"image/color"
	"testing"
)

// newSolidImage builds a w x h image.Image filled entirely with c — the
// simplest possible synthetic fixture for exercising ImageDiffScore
// without needing any real screenshot or file I/O.
func newSolidImage(c color.Color, w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// TestImageDiffScore_IdenticalImagesNotStale proves the algorithm's
// zero-distance floor: the same content compared against itself (and two
// separately-built but pixel-identical images) must score exactly 0 and
// fall well under HelpScreenshotStaleThreshold.
func TestImageDiffScore_IdenticalImagesNotStale(t *testing.T) {
	a := newSolidImage(color.RGBA{R: 100, G: 150, B: 200, A: 255}, 40, 30)
	b := newSolidImage(color.RGBA{R: 100, G: 150, B: 200, A: 255}, 40, 30)

	if score := ImageDiffScore(a, a); score != 0 {
		t.Errorf("ImageDiffScore(a, a) = %v, want 0 (an image compared against itself)", score)
	}
	score := ImageDiffScore(a, b)
	if score != 0 {
		t.Errorf("ImageDiffScore(a, b) = %v, want 0 (two separately-built, pixel-identical images)", score)
	}
	if score >= HelpScreenshotStaleThreshold {
		t.Errorf("score %v unexpectedly >= threshold %v for identical images", score, HelpScreenshotStaleThreshold)
	}
}

// TestImageDiffScore_BlackVsWhiteIsMaximallyStale proves the algorithm's
// far end: two maximally different solid-color images score at (or very
// near) 1.0, comfortably over the threshold.
func TestImageDiffScore_BlackVsWhiteIsMaximallyStale(t *testing.T) {
	black := newSolidImage(color.RGBA{R: 0, G: 0, B: 0, A: 255}, 40, 30)
	white := newSolidImage(color.RGBA{R: 255, G: 255, B: 255, A: 255}, 40, 30)

	score := ImageDiffScore(black, white)
	if score < 0.95 {
		t.Errorf("ImageDiffScore(black, white) = %v, want close to 1.0", score)
	}
	if score < HelpScreenshotStaleThreshold {
		t.Errorf("score %v unexpectedly below threshold %v for maximally different images", score, HelpScreenshotStaleThreshold)
	}
}

// TestImageDiffScore_DifferentDimensionsIsMaximallyStale proves the
// dimension-mismatch short-circuit: a real layout regression that
// changes a page's rendered size must never be silently ignored by
// comparing only the overlapping region.
func TestImageDiffScore_DifferentDimensionsIsMaximallyStale(t *testing.T) {
	a := newSolidImage(color.RGBA{R: 10, G: 10, B: 10, A: 255}, 40, 30)
	b := newSolidImage(color.RGBA{R: 10, G: 10, B: 10, A: 255}, 20, 30)

	if score := ImageDiffScore(a, b); score != 1.0 {
		t.Errorf("ImageDiffScore of differently-sized images = %v, want 1.0", score)
	}
}

// TestImageDiffScore_SmallUniformNoiseStaysBelowThreshold is the
// requantization-noise case HelpScreenshotStaleThreshold's absolute
// sanity ceiling specifically needs to tolerate: a small, uniform
// per-channel offset (the kind of difference real JPEG re-encoding
// introduces even for visually-unchanged content) must not itself read
// as "wholesale mismatch." Independent review (uc-infra#145): this
// threshold is a generous backstop, not the primary staleness signal —
// see its own doc comment in imagediff.go for why (the same/different
// distributions overlap too much for one scalar threshold to separate
// them on its own; the real-browser staleness check's primary signal is
// a relative comparison instead).
func TestImageDiffScore_SmallUniformNoiseStaysBelowThreshold(t *testing.T) {
	a := newSolidImage(color.RGBA{R: 120, G: 120, B: 120, A: 255}, 40, 30)
	b := newSolidImage(color.RGBA{R: 121, G: 119, B: 121, A: 255}, 40, 30)

	score := ImageDiffScore(a, b)
	if score >= HelpScreenshotStaleThreshold {
		t.Errorf("ImageDiffScore for a small uniform +/-1 offset = %v, want below threshold %v (this is the exact class of re-encoding noise the threshold must tolerate)", score, HelpScreenshotStaleThreshold)
	}
}

// TestImageDiffScore_LargeUniformShiftIsStale is the complement: a large
// (half-range) uniform shift is a real content change (e.g. a light-mode
// screenshot vs a dark-mode one), not noise, and must be flagged.
func TestImageDiffScore_LargeUniformShiftIsStale(t *testing.T) {
	a := newSolidImage(color.RGBA{R: 20, G: 20, B: 20, A: 255}, 40, 30)
	b := newSolidImage(color.RGBA{R: 220, G: 220, B: 220, A: 255}, 40, 30)

	score := ImageDiffScore(a, b)
	if score < HelpScreenshotStaleThreshold {
		t.Errorf("ImageDiffScore for a large 200-value uniform shift = %v, want at/above threshold %v", score, HelpScreenshotStaleThreshold)
	}
}
