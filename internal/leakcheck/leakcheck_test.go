// Package leakcheck guards specific source files against reintroducing
// private-deployment detail into comments. universal-core is
// public (CLAUDE.md); comments describing internal infra topology or
// quoting a private conversation verbatim have leaked into this repo's
// public history before (uc-infra#138 — hostnames, hardware detail,
// Key Vault secret *names*, and a verbatim owner quote; uc-infra#153 —
// the same class of leak in 4 more files found during #138's
// independent review; uc-infra#155 — the same class of leak in two
// more files, missed by both earlier sweeps; uc-infra#156 — the same
// class of leak in a CI YAML file (a node hostname, hardware/cluster
// topology, and a dangling pointer to a private-repo record), missed
// because #138/#153/#155's sweeps only looked at Go source doc comments;
// no credential values in any case). This is a narrow, source-text
// regression test scoped to the exact files #138/#153/#155/#156 fixed
// — it is not a general secret scanner, and it does not catch the same
// pattern reappearing in a file it doesn't watch (e.g. via copy-paste)
// or a file that already had the pattern before this test existed.
// Widening this to a repo-wide check is proposed in uc-infra#153's own
// follow-up notes.
//
// PR#119 (uc-infra#138) and PR#121 (uc-infra#153) both added a file at
// this exact path/package/function name, each with a disjoint
// bannedInFile map — merged here into one map covering all 8 files
// (both sides taken together) rather than picking either side, so
// neither PR's guard coverage was silently dropped by the merge.
// uc-infra#155 added a 9th and 10th file (internal/api/issuereport.go
// and internal/api/issuereport_test.go) on top of that merged map.
// uc-infra#156 added an 11th: the first non-Go (YAML) file this test
// covers — os.ReadFile works unchanged on any text file, so no
// mechanism change was needed, only a new map entry.
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
// re-typed capitalization variant doesn't slip past. Where the leaked
// text used both a hyphenated and a spaced form in different files,
// both forms are listed here so a reintroduction in the "wrong" file's
// style still gets caught, not just the exact original wording of that
// file.
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
		"queue.md",
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
	"internal/api/issuereport.go": {
		"farshid's ask",
		"open an issue for us maybe in the github issue",
		"homelab",
		"queue.md",
	},
	"internal/api/issuereport_test.go": {
		"farshid reported",
		"it works but only in english",
	},
	".github/workflows/build-and-push.yml": {
		"mini-pc",
		"raspberry-pi",
		"raspberry pi",
		"pi node",
		"homelab",
		"queue.md",
		"hairpin",
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
					t.Errorf("%s: reintroduced leaked detail %q (see uc-infra#138/#153/#155/#156) — describe the deployment generically instead of naming the cluster/hardware/DNS/secret name/private-repo path, or quoting private planning context verbatim", relPath, s)
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
