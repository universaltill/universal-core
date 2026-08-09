// Package repohygiene guards repo-root housekeeping conventions that
// have no other enforcement mechanism — nothing in Go, the Makefile, or
// CI reads .gitignore, so a gap there is invisible until someone
// accidentally `git add`s the exact file it should have excluded.
//
// This test exists because that already happened twice: a 13 MB
// seed-demo-data binary was one `git add -A` away from landing in this
// public repo's history (independent review, 2026-07-31, caught before
// it shipped) and a 15.8 MB sync-tenant-modules binary actually did
// land (uc-infra#184) — .gitignore's own binaries block had a line for
// every other cmd/ binary except that one. Both times the fix was
// manually adding one more line to .gitignore; neither time closed the
// class, so both recurred. This test closes the class: every directory
// under cmd/ must have a matching `/<name>` line in .gitignore, checked
// by walking cmd/ itself rather than hardcoding the list, so a future
// `cmd/whatever-new-tool` is covered automatically and a missing entry
// fails the build instead of waiting for the next accidental commit.
package repohygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdBinariesAreGitignored(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	cmdEntries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}

	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	ignoredLines := make(map[string]bool)
	for _, line := range strings.Split(string(gitignore), "\n") {
		ignoredLines[strings.TrimSpace(line)] = true
	}

	for _, entry := range cmdEntries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		want := "/" + name
		if !ignoredLines[want] {
			t.Errorf("cmd/%s has no matching %q line in .gitignore — a locally-built `go build ./cmd/%s` from the repo root would be one `git add -A` away from landing in this public repo's history (see uc-infra#184, where exactly this happened)", name, want, name)
		}
	}
}

// repoRoot walks up from the working directory to the module root, so
// this test behaves the same whether run via `go test ./...` from the
// repo root or `go test .` from this package's own directory. Mirrors
// internal/leakcheck's identical helper — small enough (and specific
// enough to "the module root for *this* package's own test run") that
// sharing it isn't worth a new exported dependency between two
// otherwise-unrelated guard packages.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
