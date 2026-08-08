package help

import (
	"io/fs"
	"testing"
)

// Size budget for internal/help/content/assets/ (uc-infra#145: "Help
// screenshots... with a staleness check and a size budget"). Chosen with
// real headroom over what TestCaptureHelpScreenshots (internal/e2e)
// actually measured when this file was written: 8 files, 1280x800
// viewport JPEGs at helpScreenshotJPEGQuality=82, individually
// 32,818-42,818 bytes, ~304,927 bytes (~298 KB) total. See this test's
// own t.Logf output on every run for the current exact per-file sizes.
const (
	// helpAssetPerImageBudgetBytes: the largest real capture measured
	// ~42.8 KB — 100 KB is a little over 2x that, enough headroom for a
	// future page with visibly more content without being so loose it
	// would wave through a genuinely bloated capture (e.g. a PNG-quality
	// misconfiguration regenerating these at 10x the size).
	helpAssetPerImageBudgetBytes = 100 * 1024
	// helpAssetTotalBudgetBytes: 8 files at the per-image budget would be
	// 800 KB; the real total measured ~298 KB, so 500 KB leaves
	// comparable headroom on the total without permitting every file to
	// simultaneously max out its own individual budget at the same time.
	helpAssetTotalBudgetBytes = 500 * 1024
	// helpAssetExpectedCount: 4 layout families (list/form/help/wizard) x
	// 2 locales (en/ar) — the issue's own capture matrix.
	helpAssetExpectedCount = 8
)

// TestHelpAssetSizeBudget walks the REAL embedded content/assets tree
// (contentFS, not a synthetic fstest.MapFS) and fails LOUD — not skip —
// when it's missing, empty, or short of the expected 8 files: this is
// what turns "screenshots never got captured" (or "got captured once,
// then a later change deleted/renamed them") into a real CI failure
// instead of a silently-passing gap in coverage. It also asserts every
// individual file, and the total, stay under the budgets above.
func TestHelpAssetSizeBudget(t *testing.T) {
	entries, err := fs.ReadDir(contentFS, "content/assets")
	if err != nil {
		t.Fatalf("content/assets does not exist in the embedded tree — screenshots were never captured (run CAPTURE_HELP_SCREENSHOTS=1 go test ./internal/e2e -run TestCaptureHelpScreenshots -v): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("content/assets exists but is empty — screenshots were never captured")
	}

	var total int64
	var count int
	if err := fs.WalkDir(contentFS, "content/assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		count++
		total += info.Size()
		t.Logf("%s: %d bytes", path, info.Size())
		if info.Size() > helpAssetPerImageBudgetBytes {
			t.Errorf("%s is %d bytes, over the per-image budget of %d bytes", path, info.Size(), helpAssetPerImageBudgetBytes)
		}
		if info.Size() == 0 {
			t.Errorf("%s is 0 bytes — a truncated/empty capture", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk content/assets: %v", err)
	}

	if count < helpAssetExpectedCount {
		t.Fatalf("found %d file(s) under content/assets, want at least %d (4 layout families x 2 locales) — screenshots never got captured, or some are missing", count, helpAssetExpectedCount)
	}
	t.Logf("total: %d bytes across %d files (budget %d bytes)", total, count, helpAssetTotalBudgetBytes)
	if total > helpAssetTotalBudgetBytes {
		t.Errorf("total asset size %d bytes exceeds the total budget of %d bytes", total, helpAssetTotalBudgetBytes)
	}
}
