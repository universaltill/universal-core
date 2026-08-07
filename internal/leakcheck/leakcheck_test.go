// Package leakcheck guards specific source files against reintroducing
// private-deployment detail into doc comments. universal-core is
// public (CLAUDE.md); comments describing internal infra topology or
// quoting a private conversation verbatim have leaked into this repo's
// public history before (uc-infra#138 — hostnames, hardware detail,
// Key Vault secret *names*, and a verbatim owner quote, no credential
// values). This is a narrow, source-text regression test scoped to the
// exact files uc-infra#138 fixed — it is not a general secret scanner,
// and it does not catch the same pattern reappearing in a file it
// doesn't watch (e.g. via copy-paste) or a file that already had the
// pattern before this test existed. Both are real gaps, not oversights:
// the additional occurrences #138's own review found elsewhere in the
// tree are tracked separately as uc-infra#153, which is also where
// widening this guard to a repo-wide check is proposed.
package leakcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bannedInFile maps a repo-relative file path to substrings it must
// never contain again — matched case-insensitively so a re-typed
// capitalization variant doesn't slip past. Where the leaked text used
// both a hyphenated and a spaced form in different files, both forms are
// listed here so a reintroduction in the "wrong" file's style still gets
// caught, not just the exact original wording of that file.
var bannedInFile = map[string][]string{
	"internal/kernel/aiassist/aiassist.go": {
		"homelab-k8s",
		"raspberry-pi",
		"raspberry pi",
		"ollama.ollama.svc.cluster.local",
		"server version 0.32.0",
	},
	"cmd/universal-core/main.go": {
		"homelab-k8s",
		"raspberry-pi",
		"raspberry pi",
		"kubernetes/apps",
		"member-mgmt-zitadel-pat",
		"zitadel-project-id",
	},
	"internal/api/import.go": {
		"homelab-k8s",
		"raspberry-pi",
		"raspberry pi",
	},
	"internal/api/aiprovidersettings.go": {
		"farshid's ask",
		"farshid approved",
		"that is fine, go for the api and ollama",
		"see queue.md",
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
				if strings.Contains(content, s) {
					t.Errorf("%s: reintroduced leaked detail %q (see uc-infra#138) — describe the deployment generically instead of naming the cluster/hardware/DNS/secret name, or quoting private planning context verbatim", relPath, s)
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
