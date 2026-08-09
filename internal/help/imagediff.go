package help

import "image"

// HelpScreenshotStaleThreshold is a generous absolute sanity ceiling —
// NOT the primary staleness signal. It exists only to catch a totally
// broken/corrupt capture (e.g. a blank white page, or a completely
// unrelated image checked in by mistake), never to distinguish "the same
// page, re-rendered" from "a materially different page" on its own.
//
// Independent review (uc-infra#145) found the original design used this
// same value (0.007) AS the sole "is this page's own re-capture too
// different from what's checked in" signal, and — measured for real
// against all 28 pairs of the 8 shipped captures, plus synthetic 1px/2px
// layout-shift perturbations of one capture — that doesn't work: the two
// smallest genuinely-different-page pairs (list/en vs wizard/en at
// 0.0060, list/ar vs wizard/ar at 0.0056) scored BELOW a plain 1-pixel
// vertical shift of an otherwise-unchanged page (0.0068). The "same
// page, different renderer noise" and "different page" distributions
// overlap, so no single scalar threshold on this metric can separate
// them reliably — the original doc comment's claim that 0.007 sat with
// "real headroom" between those two bands was arithmetically wrong (0.007
// is not between 0.0000 and 0.006, it's above both).
//
// internal/e2e's staleness check (TestHelpScreenshot_StalenessCheck_
// RealBrowser) now uses this metric differently: a fresh re-capture of
// page P must score closer to P's own checked-in file than to ANY other
// page's checked-in file (a relative, renderer-noise-robust comparison —
// see that test's own doc comment). HelpScreenshotStaleThreshold backs
// only that test's secondary sanity check (the fresh/checked-in score
// itself must stay under some generous absolute ceiling, catching a
// wholesale mismatch the relative comparison alone wouldn't), and this
// package's own unit tests (imagediff_test.go) proving the metric's
// shape in isolation. 0.05 is comfortably above every real same-page
// measurement taken so far (0.0000-0.0068) and comfortably below a
// genuinely different image (>=0.3 for anything but near-identical
// content, ~1.0 for solid black vs white).
const HelpScreenshotStaleThreshold = 0.05

// ImageDiffScore returns a normalized [0,1] dissimilarity score between a
// and b: 0.0 means pixel-identical, 1.0 means maximally different (e.g.
// solid black vs solid white). Used by the help-screenshot staleness
// check to tell "the page's real rendered content changed" apart from
// ordinary JPEG-requantization/font-hinting noise between two captures
// of the same, unchanged page.
//
// Metric: mean absolute per-channel (R,G,B — alpha ignored) difference
// over every pixel, normalized by the channel range — a plain,
// easy-to-verify-by-inspection metric, not a perceptual/structural one
// (SSIM etc.): this package has no image-processing dependency today,
// and a metric anyone reading this file can hand-check against a small
// example serves this slice's own honesty requirement (see the
// staleness test's doc comment) better than a more "accurate" one this
// codebase has no way to validate against ground truth.
//
// Images of different pixel dimensions score 1.0 (maximally different)
// rather than comparing only their overlapping region: a real layout
// regression that changes a page's rendered size is itself exactly the
// kind of change staleness should catch, not silently ignore by cropping
// around it.
func ImageDiffScore(a, b image.Image) float64 {
	ab := a.Bounds()
	bb := b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return 1.0
	}
	if ab.Dx() == 0 || ab.Dy() == 0 {
		return 0.0
	}

	var total uint64
	var count uint64
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			r1, g1, b1, _ := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			total += absDiffU32(r1, r2) + absDiffU32(g1, g2) + absDiffU32(b1, b2)
			count += 3
		}
	}
	// image.Color.RGBA() returns components scaled to the 0..65535
	// range regardless of the underlying image's own bit depth —
	// normalizing by that range (not 255) is what keeps this correct for
	// both an 8-bit JPEG decode and any other image.Image implementation.
	return float64(total) / float64(count) / 65535.0
}

func absDiffU32(x, y uint32) uint64 {
	if x > y {
		return uint64(x - y)
	}
	return uint64(y - x)
}
