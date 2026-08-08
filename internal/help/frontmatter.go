package help

import (
	"fmt"
	"strconv"
	"strings"
)

// frontMatter is one topic file's parsed `---`-delimited header — see
// content/README.md's "Front matter" section for the shape this
// mirrors, and ADR-0023 (uc-infra) §Scope for why exactly these four
// keys are required. Every field here is required; there is
// deliberately no optional/default-valued field, so a topic author
// omitting one gets a loud parse error (index.go's BuildIndexFrom fails
// the whole build) rather than a silently incomplete topic slipping
// into the manual.
type frontMatter struct {
	Title    string
	Audience string // "user" | "admin" | "both"
	Module   string
	Order    int
}

// requiredFrontMatterKeys is the exact key set parseTopicFile demands,
// in the order they're checked — order only matters for which missing
// key a multi-missing-key file's error names first, not for parsing
// itself (all lines are read before any key is validated).
var requiredFrontMatterKeys = []string{"title", "audience", "module", "order"}

// parseTopicFile splits a topic file's raw bytes into its front matter
// and markdown body. The shape is a hand-rolled, deliberately narrow
// flat-scalar parser — no YAML library — the same "no new dependency
// for a narrow, controlled need" reasoning this package's own
// slugifyRoute (help.go) and formrender's slugifyTitle already follow
// (uc-infra#31/#95): every topic file is authored by this pipeline or
// Farshid, never arbitrary user input, and the shape needed is nothing
// more than `key: value` lines between two `---` delimiters.
//
// Fails loud on anything malformed — a missing delimiter, a missing
// required key, or an unparseable `order` — rather than degrading:
// BuildIndexFrom (index.go) propagates this error and fails the whole
// index build, so a malformed topic breaks the build/tests that
// exercise it instead of silently vanishing from the manual (ADR-0023).
func parseTopicFile(raw []byte) (frontMatter, string, error) {
	lines := strings.Split(string(raw), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return frontMatter{}, "", fmt.Errorf("help: missing opening --- front-matter delimiter")
	}

	values := map[string]string{}
	i := 1
	closed := false
	for ; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "---" {
			closed = true
			i++
			break
		}
		key, val, ok := splitFrontMatterLine(line)
		if !ok {
			// A blank (or otherwise key:-less) line inside the front
			// matter block is tolerated rather than treated as a parse
			// error — the only hard requirements are the two `---`
			// delimiters and the four required keys, checked below.
			continue
		}
		values[key] = unquote(val)
	}
	if !closed {
		return frontMatter{}, "", fmt.Errorf("help: missing closing --- front-matter delimiter")
	}

	for _, key := range requiredFrontMatterKeys {
		if _, ok := values[key]; !ok {
			return frontMatter{}, "", fmt.Errorf("help: missing required front-matter key %q", key)
		}
	}

	fm := frontMatter{
		Title:    values["title"],
		Audience: values["audience"],
		Module:   values["module"],
	}
	if fm.Title == "" {
		return frontMatter{}, "", fmt.Errorf("help: front-matter key %q must not be empty", "title")
	}
	if fm.Module == "" {
		return frontMatter{}, "", fmt.Errorf("help: front-matter key %q must not be empty", "module")
	}
	switch fm.Audience {
	case "user", "admin", "both":
	default:
		return frontMatter{}, "", fmt.Errorf("help: front-matter key %q must be one of user|admin|both, got %q", "audience", fm.Audience)
	}
	order, err := strconv.Atoi(values["order"])
	if err != nil {
		return frontMatter{}, "", fmt.Errorf("help: front-matter key %q must be an integer: %w", "order", err)
	}
	fm.Order = order

	// The body is everything after the closing delimiter line, with at
	// most one leading blank line (the conventional blank line straight
	// after `---`) trimmed — not every leading blank line, so an author
	// deliberately starting the body with extra vertical space isn't
	// silently collapsed.
	body := strings.Join(lines[i:], "\n")
	body = strings.TrimPrefix(body, "\n")
	return fm, body, nil
}

// splitFrontMatterLine splits one front-matter line on its first ":" —
// the flat-scalar grammar this parser supports has no nested
// structures, so the first colon unambiguously separates key from
// value. A line with no colon, or an empty key, isn't a key:value line
// at all (ok=false) — parseTopicFile tolerates it as a blank/comment-
// like line rather than erroring, since the only hard requirements are
// the delimiters and the required keys.
func splitFrontMatterLine(line string) (key, val string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	val = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// unquote strips one leading/trailing matching quote character (" or ')
// from a scalar value, so `title: "Purchase Orders"` and
// `title: Purchase Orders` parse identically — otherwise verbatim,
// trimmed of surrounding whitespace (already done by
// splitFrontMatterLine before this runs).
func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
