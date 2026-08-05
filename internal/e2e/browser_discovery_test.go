package e2e

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// findBrowser looks for a Chrome/Chromium binary in the handful of
// places it's actually installed across this repo's real environments —
// GitHub Actions' ubuntu-latest runners (google-chrome-stable on PATH),
// a Playwright-provisioned sandbox (Chromium under
// $PLAYWRIGHT_BROWSERS_PATH, not on PATH — see resolveBrowserPath), and
// a local macOS dev machine (Google Chrome.app, not on PATH by
// default). chromedp can locate a browser itself on Linux/PATH, but not
// the other two, so those branches are only load-bearing outside real
// CI.
func findBrowser(t *testing.T) string {
	t.Helper()
	if path := resolveBrowserPath(exec.LookPath, os.Stat, runtime.GOOS, os.Getenv("PLAYWRIGHT_BROWSERS_PATH")); path != "" {
		return path
	}
	if fail, msg := browserMissingAction(os.Getenv("CI")); fail {
		t.Fatal(msg)
	} else {
		t.Skip(msg)
	}
	return ""
}

// resolveBrowserPath is findBrowser's discovery logic pulled out as a
// pure function (lookPath/stat/goos/playwrightBrowsersPath all
// injected) so the fallback ordering — and specifically the
// Playwright-sandbox branch — is unit-testable without depending on an
// actual PATH or filesystem. Returns "" when nothing usable is found.
//
// Order matters: PATH-based candidates first (never worse than the old
// behavior), then the Playwright sandbox's known install location, then
// macOS. Found 2026-08-02 (uc-infra#122): a Playwright-provisioned
// cloud sandbox installs Chromium under $PLAYWRIGHT_BROWSERS_PATH and
// deliberately keeps it off PATH, so every exec.LookPath candidate
// above always failed there and every one of this package's 49
// real-browser tests silently skipped instead of actually running.
//
// The Playwright-sandbox candidate must be a regular, executable file —
// not just present — so a directory (Playwright's usual on-disk layout
// is versioned, e.g. chromium-<rev>/chrome-linux/chrome; a flat
// "chromium" entry is this particular image's own convenience symlink,
// not a guarantee every Playwright install has one) or a stray
// non-executable file at that path falls through to the macOS branch
// or the missing-browser path instead of being handed to chromedp as a
// binary to exec.
func resolveBrowserPath(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error), goos string, playwrightBrowsersPath string) string {
	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	for _, name := range candidates {
		if path, err := lookPath(name); err == nil {
			return path
		}
	}
	if playwrightBrowsersPath != "" {
		candidate := filepath.Join(playwrightBrowsersPath, "chromium")
		if fi, err := stat(candidate); err == nil && isExecutableFile(fi) {
			return candidate
		}
	}
	if goos == "darwin" {
		macPath := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := stat(macPath); err == nil {
			return macPath
		}
	}
	return ""
}

// isExecutableFile reports whether fi describes a regular file with at
// least one executable bit set — true for an ordinary binary, false for
// a directory or a non-executable file.
func isExecutableFile(fi os.FileInfo) bool {
	return fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// browserMissingAction decides what happens when no Chrome/Chromium
// binary is found: a hard failure when CI is set (GitHub Actions and
// most CI systems set this by default), never a silent skip — a runner
// image that quietly drops its browser must not let all 49 of this
// package's tests quietly skip along with it (CLAUDE.md's "CI must
// never silently skip" rule, applied to the browser dependency itself,
// not just the tests that need it). Skips locally so a laptop without
// Chrome installed doesn't block an ordinary `go test ./...`. A pure
// function so the branch is unit-testable without needing a real
// missing-browser environment.
func browserMissingAction(ciEnv string) (fail bool, message string) {
	const notFound = "no Chrome/Chromium binary found (checked PATH, PLAYWRIGHT_BROWSERS_PATH, and the macOS Chrome.app path)"
	if ciEnv != "" {
		return true, notFound + " in CI — internal/e2e's real-browser tests must never silently skip"
	}
	return false, notFound + "; skipping browser E2E test"
}

func TestBrowserMissingAction_CIEnvironment_Fails(t *testing.T) {
	fail, msg := browserMissingAction("true")
	if !fail {
		t.Fatal("expected fail=true when the CI env var is set, got fail=false")
	}
	if msg == "" {
		t.Fatal("expected a non-empty failure message")
	}
}

func TestBrowserMissingAction_NoCIEnvironment_Skips(t *testing.T) {
	fail, msg := browserMissingAction("")
	if fail {
		t.Fatal("expected fail=false when the CI env var is unset, got fail=true")
	}
	if msg == "" {
		t.Fatal("expected a non-empty skip message")
	}
}

// lookPathAlwaysMiss simulates every findBrowser PATH candidate being
// absent — the state a Playwright-only sandbox is actually in: none of
// google-chrome/google-chrome-stable/chromium/chromium-browser resolve
// when the real Chromium lives off PATH under PLAYWRIGHT_BROWSERS_PATH
// instead.
func lookPathAlwaysMiss(string) (string, error) {
	return "", exec.ErrNotFound
}

// fakeFileInfo is a minimal os.FileInfo stub carrying only the fs.FileMode
// resolveBrowserPath actually inspects (via isExecutableFile) — the rest
// of the interface is never consulted by the code under test.
type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// statRegularExecutable returns a stat func reporting existingPath as a
// regular, executable file (mode 0o755 — no ModeDir/other type bits, so
// IsRegular() is true, and the owner-exec bit is set) and everything
// else as missing.
func statRegularExecutable(existingPath string) func(string) (os.FileInfo, error) {
	return statWithMode(existingPath, 0o755)
}

// statWithMode returns a stat func reporting existingPath as present
// with exactly the given mode (letting tests simulate a directory or a
// non-executable file at that path) and everything else as missing.
func statWithMode(existingPath string, mode fs.FileMode) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if path == existingPath {
			return fakeFileInfo{mode: mode}, nil
		}
		return nil, os.ErrNotExist
	}
}

func TestResolveBrowserPath_SandboxChromiumOffPath_FoundViaPlaywrightBrowsersPath(t *testing.T) {
	// Regression test for uc-infra#122: before this fix, findBrowser had
	// no fallback beyond PATH and the macOS .app path, so a Playwright
	// sandbox with Chromium installed at
	// $PLAYWRIGHT_BROWSERS_PATH/chromium — off PATH by design — silently
	// skipped every real-browser test instead of finding it.
	got := resolveBrowserPath(lookPathAlwaysMiss, statRegularExecutable("/opt/pw-browsers/chromium"), "linux", "/opt/pw-browsers")
	want := "/opt/pw-browsers/chromium"
	if got != want {
		t.Fatalf("resolveBrowserPath() = %q, want %q (sandbox Chromium under PLAYWRIGHT_BROWSERS_PATH should have been found)", got, want)
	}
}

func TestResolveBrowserPath_PathCandidateStillWinsOverSandboxFallback(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", exec.ErrNotFound
	}
	// Even with a (wrong/decoy) PLAYWRIGHT_BROWSERS_PATH chromium also
	// present, an ordinary PATH hit must still win — no behavior change
	// for GitHub Actions' runners, which already resolve via PATH.
	got := resolveBrowserPath(lookPath, statRegularExecutable("/opt/pw-browsers/chromium"), "linux", "/opt/pw-browsers")
	want := "/usr/bin/chromium"
	if got != want {
		t.Fatalf("resolveBrowserPath() = %q, want %q (PATH candidate should take priority)", got, want)
	}
}

func TestResolveBrowserPath_PlaywrightBrowsersPathSetButNoChromiumThere_NotFound(t *testing.T) {
	got := resolveBrowserPath(lookPathAlwaysMiss, statRegularExecutable("/some/other/file"), "linux", "/opt/pw-browsers")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (no PATH hit and no chromium under PLAYWRIGHT_BROWSERS_PATH)", got)
	}
}

func TestResolveBrowserPath_PlaywrightBrowsersPathUnset_SkipsThatBranch(t *testing.T) {
	got := resolveBrowserPath(lookPathAlwaysMiss, statRegularExecutable("/opt/pw-browsers/chromium"), "linux", "")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (PLAYWRIGHT_BROWSERS_PATH unset should not fall back to a fixed path)", got)
	}
}

func TestResolveBrowserPath_PlaywrightBrowsersPathIsADirectory_NotAccepted(t *testing.T) {
	// A Playwright install's real on-disk layout is versioned
	// (chromium-<rev>/chrome-linux/chrome); a bare "chromium" entry at
	// $PLAYWRIGHT_BROWSERS_PATH that turns out to be a directory (not
	// this image's convenience symlink to a binary) must not be handed
	// to chromedp as an executable.
	got := resolveBrowserPath(lookPathAlwaysMiss, statWithMode("/opt/pw-browsers/chromium", fs.ModeDir|0o755), "linux", "/opt/pw-browsers")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (a directory at the candidate path must be rejected)", got)
	}
}

func TestResolveBrowserPath_PlaywrightBrowsersPathIsNonExecutable_NotAccepted(t *testing.T) {
	got := resolveBrowserPath(lookPathAlwaysMiss, statWithMode("/opt/pw-browsers/chromium", 0o644), "linux", "/opt/pw-browsers")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (a non-executable file at the candidate path must be rejected)", got)
	}
}

func TestResolveBrowserPath_DarwinFallback_StillWorksAfterSandboxBranch(t *testing.T) {
	macPath := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	got := resolveBrowserPath(lookPathAlwaysMiss, statRegularExecutable(macPath), "darwin", "")
	if got != macPath {
		t.Fatalf("resolveBrowserPath() = %q, want %q (macOS fallback must survive the new sandbox branch)", got, macPath)
	}
}

func TestResolveBrowserPath_DarwinWithNoChromeAppEither_ReturnsEmpty(t *testing.T) {
	got := resolveBrowserPath(lookPathAlwaysMiss, statWithMode("/nope", 0o755), "darwin", "")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (darwin with no PATH hit, no sandbox path, and no Chrome.app)", got)
	}
}

func TestResolveBrowserPath_DarwinWithPlaywrightSandboxAlsoSet_SandboxWinsByOrder(t *testing.T) {
	// The documented order is PATH, then Playwright sandbox, then
	// macOS — so a mac dev machine that also has a Playwright Chromium
	// installed resolves the sandbox one first. Locking in the
	// documented order rather than leaving it an untested interaction.
	got := resolveBrowserPath(lookPathAlwaysMiss, statRegularExecutable("/opt/pw-browsers/chromium"), "darwin", "/opt/pw-browsers")
	want := "/opt/pw-browsers/chromium"
	if got != want {
		t.Fatalf("resolveBrowserPath() = %q, want %q (Playwright sandbox branch should be checked before the macOS branch)", got, want)
	}
}

func TestResolveBrowserPath_NothingFoundAnywhere_ReturnsEmpty(t *testing.T) {
	got := resolveBrowserPath(lookPathAlwaysMiss, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, "linux", "")
	if got != "" {
		t.Fatalf("resolveBrowserPath() = %q, want \"\" (nothing found on PATH, sandbox path, or macOS)", got)
	}
}

// TestResolveBrowserPath_RealSandboxDependencies exercises
// resolveBrowserPath with the real, non-injected exec.LookPath, os.Stat
// and PLAYWRIGHT_BROWSERS_PATH this session actually has, with the PATH
// branch forced to miss so a genuine hit can only come from the
// Playwright-sandbox fallback this fix adds. Unlike calling findBrowser
// itself, this asserts on resolveBrowserPath's return value directly —
// findBrowser's t.Skip/t.Fatal calls unwind the goroutine
// (runtime.Goexit) before any assertion after them would run, which
// would make a test built around findBrowser pass unconditionally
// whenever the browser goes missing. That is exactly the silent-skip
// failure mode uc-infra#122 is about, so this test is written to fail
// loudly instead: reverting the PLAYWRIGHT_BROWSERS_PATH branch above
// would make it fail here, not skip.
func TestResolveBrowserPath_RealSandboxDependencies(t *testing.T) {
	pwPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if pwPath == "" {
		t.Skip("PLAYWRIGHT_BROWSERS_PATH not set in this environment")
	}
	want := filepath.Join(pwPath, "chromium")
	if fi, err := os.Stat(want); err != nil || !isExecutableFile(fi) {
		t.Skipf("no executable chromium under PLAYWRIGHT_BROWSERS_PATH in this environment (stat: %v)", err)
	}
	got := resolveBrowserPath(lookPathAlwaysMiss, os.Stat, runtime.GOOS, pwPath)
	if got != want {
		t.Fatalf("resolveBrowserPath() = %q, want %q", got, want)
	}
}
