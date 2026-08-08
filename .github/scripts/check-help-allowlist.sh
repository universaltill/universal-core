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
# something base didn't have", never flags a shrink.
#
# Bootstrap case: if base has no undocumentedAllowlist at all (this
# script's own introducing PR, uc-infra#146, whose base commit predates
# the map's existence), there is nothing to ratchet against yet — every
# key in head is accepted rather than flagged as "added". This is safe
# precisely because it only ever fires once, for the PR that first adds
# the map: every PR after that has a base commit where the map already
# exists (this script's own reviewed and merged text), so a real base
# key set is always available for every future comparison.
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

# extract_keys <file> — prints one quoted-string map key per line, taken
# only from between the `var undocumentedAllowlist = map[string]bool{`
# line and its closing `}` (anchored at column 0 — the var is top-level
# Go source, gofmt keeps its closing brace unindented), so a quoted
# entity-type-shaped string appearing elsewhere in the file (a comment,
# a different map) is never mistaken for an allowlist entry. Prints
# nothing (not an error) if the file doesn't exist or has no such map —
# unlike check-coverage-floor.sh's extract_floor, a missing map is the
# expected, valid state for the base file the one time this script's own
# introducing PR runs (see the bootstrap-case comment above), so this
# function can't tell "missing on purpose" from "missing by mistake" —
# only the caller (main, below) knows which file is head vs. base and
# treats the two differently.
extract_keys() {
  local file="$1"
  [ -f "$file" ] || return 0
  # `|| true` at the end: grep exits 1 on "no matches" (a valid outcome
  # here — the file has no allowlist), and under `set -o pipefail` that
  # would otherwise make the whole pipeline's exit status 1, which `set
  # -e` then treats as this function failing outright when it's called
  # from a `VAR=$(extract_keys ...)` assignment.
  awk '
    /^var undocumentedAllowlist = map\[string\]bool\{/ { flag=1; next }
    flag && /^}/ { flag=0 }
    flag
  ' "$file" | grep -oE '"[A-Za-z0-9_]+":' | sed -E 's/^"//; s/":$//' | sort -u || true
}

HEAD_KEYS=$(extract_keys "$HEAD_FILE")
BASE_KEYS=$(extract_keys "$BASE_FILE")

HEAD_COUNT=$(printf '%s\n' "$HEAD_KEYS" | grep -c . || true)
BASE_COUNT=$(printf '%s\n' "$BASE_KEYS" | grep -c . || true)

if [ "$HEAD_COUNT" -eq 0 ]; then
  echo "::error::Could not find undocumentedAllowlist in $HEAD_FILE — refusing to silently pass (deleting the map entirely is not a valid way to close it out)." >&2
  exit 1
fi

if [ "$BASE_COUNT" -eq 0 ]; then
  echo "No undocumentedAllowlist found in $BASE_FILE — treating as this gate's introducing PR (bootstrap case, see script header); nothing to ratchet against yet."
  exit 0
fi

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
