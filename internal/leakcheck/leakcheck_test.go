// Package leakcheck guards specific source files against reintroducing
// private-deployment detail into doc comments. universal-core is
// public (CLAUDE.md); comments describing internal infra topology have
// leaked into this repo's public history before (uc-infra#138 —
// hostnames, hardware detail, Key Vault secret *names*, a verbatim
// owner quote; uc-infra#153 — the same class of leak in 4 more files
// found during #138's independent review, no credential values in
// either case). This is a narrow, source-text regression test scoped
// to the exact files uc-infra#153 fixed — it is not a general secret
// scanner, and it does not catch the same pattern reappearing in a
// file it doesn't watch (e.g. via copy-paste) or a file that already
// had the pattern before this test existed.
//
// MERGE HAZARD: universal-core PR#119 (uc-infra#138's fix, open as of
// this writing) adds a file at this exact same path, same package, same
// TestNoLeakedInfraDetailInDocComments function name, same bannedInFile
// var and repoRoot helper — but with a disjoint map covering the other
// 4 files. Whichever of the two PRs merges second will hit an
// add/add conflict here. Resolve it by taking BOTH sides' bannedInFile
// entries into one merged map — never "ours"/"theirs", which compiles
// and passes green while silently dropping 4 files' worth of guard
// coverage. The repo-wide-substring-set widening this leaves on the
// table is proposed on whichever of #138/PR#119 or this card's PR
// merges first; don't duplicate the proposal in both.
package leakcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bannedInFile maps a repo-relative file path to substrings it must
// never contain again — matched case-insensitively (both the file
// content and each needle are lowercased before comparing) so a
// re-typed capitalization variant doesn't slip past.
var bannedInFile = map[string][]string{
	"internal/zitadelmgmt/zitadelmgmt.go": {
		"member-mgmt-zitadel-pat",
		"zitadel-project-id",
		"universal-core-member-mgmt",
		"key vault",
		"keyvault",
	},
	"internal/kernel/speechassist/speechassist.go": {
		"homelab-k8s",
		"kubernetes/apps",
	},
	"internal/kernel/csvimport/ai.go": {
		"homelab",
	},
	"internal/api/import_test.go": {
		"homelab",
	},
}

func TestNoLeakedInfraDetailInDocComments(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	// One subtest per file: an error reading (or a hit in) any single
	// file must not stop the others from being checked — every file
	// gets reported every run, not just the first one map iteration
	// (randomized order) happens to reach.
	for relPath, banned := range bannedInFile {
		relPath, banned := relPath, banned
		t.Run(relPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			content := strings.ToLower(string(data))
			for _, s := range banned {
				if strings.Contains(content, strings.ToLower(s)) {
					t.Errorf("%s: reintroduced leaked detail %q (see uc-infra#153) — describe the deployment generically instead of naming the cluster/hardware/secret name/private-repo path", relPath, s)
				}
			}
		})
	}
}

// repoRoot walks up from the working directory to the module root, so
// this test behaves the same whether run via `go test ./...` from the
// repo root or `go test .` from this package's own directory.
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
