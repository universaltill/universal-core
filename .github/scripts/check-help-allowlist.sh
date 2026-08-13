#!/usr/bin/env bash
#
# check-help-allowlist.sh <head-coverage-test.go> <base-coverage-test.go>
#
# Fails if the head file's undocumentedAllowlist contains an entity-type
# key that the base file's undocumentedAllowlist does not — the
# allowlist only ever shrinks (universal-core/CLAUDE.md's "In-product
# help manual" section), and nothing before this script stopped a PR
# from grandfathering a newly-shipped, undocumented Definition into it
# instead of writing its help topic (found in independent review of the
# gate this script protects, uc-infra#146 review record — an earlier
# version tried to enforce this with a second Go map literal in the same
# file/commit, which a PR editing that file could trivially edit
# alongside the first).
#
# Deliberately independent of git/CI context beyond the two file paths
# it's given: it only reads two files, so it can be exercised directly
# against fixtures (see check-help-allowlist_test.sh) without a real PR
# or checkout. Mirrors check-coverage-floor.sh's own shape exactly —
# same two-file interface, same base-ref-anchoring done by the caller
# (ci.yml), same "fails loud, never silently" discipline — for the same
# reason: a ratchet that can't be inspected/tested outside a real PR
# isn't trustworthy.
#
# Removing keys (undocumenting nothing further, or the entity type being
# renamed away) is always fine — this only ever compares "does head add
# something base didn't have", never flags a shrink. The allowlist
# emptying out entirely (map declaration present, zero keys — the last
# module slice's own end state, uc-infra#148) is the fullest legal case
# of that shrink, not a special one: has_allowlist_map (below) is what
# makes "declared with zero keys" distinguishable from "declaration
# deleted outright", which key-count alone cannot do (independent review
# of uc-infra#148 caught this: an earlier version of this script only
# ever checked HEAD_COUNT/BASE_COUNT, so a head file that closed the
# ratchet by writing `map[string]bool{}` was indistinguishable from one
# that deleted the map declaration entirely — both counted zero keys —
# and got the same "could not find undocumentedAllowlist" refusal either
# way, which made emptying the map out CI-unshippable).
#
# Bootstrap case: if base has no undocumentedAllowlist DECLARATION at
# all (this script's own introducing PR, uc-infra#146, whose base commit
# predates the map's existence), there is nothing to ratchet against
# yet — every key in head is accepted rather than flagged as "added".
# This is safe precisely because it only ever fires once, for the PR
# that first adds the map: every PR after that has a base commit where
# the map already exists (this script's own reviewed and merged text),
# so a real base key set is always available for every future
# comparison — including a base whose map is declared but already empty
# (BASE_COUNT=0 with the declaration present), which must still run the
# real head-vs-base comparison below, not be treated as a second
# bootstrap case: a base that legitimately has zero keys is a real,
# comparable ratchet state, not "the gate doesn't exist yet".
#
# Known, accepted gap, same shape as check-coverage-floor.sh's own
# documented one: this only catches a key ADDED to undocumentedAllowlist.
# It does not stop someone deleting the whole map, deleting
# TestHelpCoverage_EveryDefinitionHasATopic, or editing this very tamper-
# check step/script in the same PR that also grows the allowlist (a
# pull_request checkout includes the PR's own workflow changes) — this
# pipeline has no tool access to configure branch protection to close
# that structurally. reviewer/SKILL.md's own manual check is the
# defense-in-depth for what this script can't see, same role it already
# plays for the coverage floor.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: check-help-allowlist.sh <head-coverage-test.go> <base-coverage-test.go>" >&2
  exit 2
fi

HEAD_FILE="$1"
BASE_FILE="$2"

# has_allowlist_map <file> — true (exit 0) iff the file declares the
# undocumentedAllowlist map AT ALL, regardless of how many keys (zero
# included) it holds. This is deliberately a SEPARATE question from "how
# many keys does it have" (extract_keys/HEAD_COUNT/BASE_COUNT below):
# key-count alone cannot tell "declared with zero keys" apart from
# "declaration missing entirely" — both count zero — which is exactly
# the ambiguity independent review of uc-infra#148 found this script
# collapsing. Matches both the multi-line form (`map[string]bool{` alone
# on its line, closed by a later `}`) and the single-line empty form
# (`map[string]bool{}` fully on one line) — both are gofmt-legal
# spellings of the same declaration, so both must count as "present".
has_allowlist_map() {
  local file="$1"
  [ -f "$file" ] || return 1
  grep -qE '^var undocumentedAllowlist = map\[string\]bool\{' "$file"
}

# extract_keys <file> — prints one quoted-string map key per line, taken
# only from between the `var undocumentedAllowlist = map[string]bool{`
# line and its closing `}` (anchored at column 0 — the var is top-level
# Go source, gofmt keeps its closing brace unindented), so a quoted
# entity-type-shaped string appearing elsewhere in the file (a comment,
# a different map) is never mistaken for an allowlist entry. Prints
# nothing (not an error) if the file doesn't exist, has no such map, or
# the map is declared with zero keys — none of those are errors at this
# layer; has_allowlist_map (above) is what tells "no map" apart from
# "empty map" for the caller.
#
# The single-line empty form (`map[string]bool{}`) needs its own branch:
# the opening pattern below matches it too (a regex anchored on the line
# START doesn't care what follows), so without checking for a `}`
# already on that SAME line, the flag would stay set past `next` and the
# scan would run on into whatever column-0 `}` comes next in the file —
# a different top-level declaration's closing brace, not this map's own
# (independent review of uc-infra#148: this exact bug, though it never
# produced a wrong key list in practice here only because nothing after
# an emptied map happened to also start at column 0 before a real `}`
# turned up).
extract_keys() {
  local file="$1"
  [ -f "$file" ] || return 0
  # `|| true` at the end: grep exits 1 on "no matches" (a valid outcome
  # here — the file has no allowlist, or has one with zero keys), and
  # under `set -o pipefail` that would otherwise make the whole
  # pipeline's exit status 1, which `set -e` then treats as this
  # function failing outright when it's called from a `VAR=$(extract_keys
  # ...)` assignment.
  awk '
    /^var undocumentedAllowlist = map\[string\]bool\{/ {
      flag=1
      if ($0 ~ /}/) { flag=0 }
      next
    }
    flag && /^}/ { flag=0; next }
    flag
  ' "$file" | grep -oE '"[A-Za-z0-9_]+":' | sed -E 's/^"//; s/":$//' | sort -u || true
}

if ! has_allowlist_map "$HEAD_FILE"; then
  echo "::error::Could not find undocumentedAllowlist in $HEAD_FILE — refusing to silently pass (deleting the map declaration entirely is not a valid way to close it out; leave \`var undocumentedAllowlist = map[string]bool{}\` in place instead)." >&2
  exit 1
fi

HEAD_KEYS=$(extract_keys "$HEAD_FILE")
HEAD_COUNT=$(printf '%s\n' "$HEAD_KEYS" | grep -c . || true)

if ! has_allowlist_map "$BASE_FILE"; then
  echo "No undocumentedAllowlist found in $BASE_FILE — treating as this gate's introducing PR (bootstrap case, see script header); nothing to ratchet against yet."
  exit 0
fi

BASE_KEYS=$(extract_keys "$BASE_FILE")
BASE_COUNT=$(printf '%s\n' "$BASE_KEYS" | grep -c . || true)

# comm -23: lines only in HEAD_KEYS (sorted), i.e. keys head added that
# base didn't have. Both inputs are already sorted -u by extract_keys.
ADDED=$(comm -23 <(printf '%s\n' "$HEAD_KEYS") <(printf '%s\n' "$BASE_KEYS") || true)
ADDED_COUNT=$(printf '%s\n' "$ADDED" | grep -c . || true)

echo "undocumentedAllowlist: base=${BASE_COUNT} keys, head=${HEAD_COUNT} keys"

if [ "$ADDED_COUNT" -gt 0 ]; then
  echo "::error::undocumentedAllowlist grew — this PR adds ${ADDED_COUNT} entity type(s) not present at the PR's base ref: $(printf '%s ' $ADDED)" >&2
  echo "::error::The allowlist only ever shrinks (universal-core/CLAUDE.md). Write the help topic(s) in internal/help/content/{en,ar,fa,tr}/ instead of grandfathering a new/still-undocumented entity type in. If this is a genuine, reviewed exception, that's a Reviewer-level call, not something CI should silently allow." >&2
  exit 1
fi

echo "undocumentedAllowlist did not grow — OK."
