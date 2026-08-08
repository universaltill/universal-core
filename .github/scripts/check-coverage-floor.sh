#!/usr/bin/env bash
#
# check-coverage-floor.sh <head-ci-yml> <base-ci-yml>
#
# Fails if the head file's COVERAGE_FLOOR=<n> is lower than the base
# file's — the floor only ever ratchets up (CLAUDE.md's "Testing"
# section), and nothing before this script stopped a PR from lowering it
# in the same commit that needed it lowered. Deliberately independent of
# git/CI context: it only reads two file paths, so it can be exercised
# directly against fixture files (see check-coverage-floor_test.sh)
# without a real PR or checkout.
#
# Fails loudly, never silently, if COVERAGE_FLOOR can't be found in
# either file — same discipline as this repo's own ci.yml, which refuses
# to fall back to a default rather than risk gating on a value nobody
# intended (see ci.yml's "empty -coverpkg" guard for the same idiom).
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: check-coverage-floor.sh <head-ci-yml> <base-ci-yml>" >&2
  exit 2
fi

HEAD_FILE="$1"
BASE_FILE="$2"

extract_floor() {
  local file="$1"
  local matches
  local count
  # Anchored to the start of the line (allowing leading whitespace, since
  # this is a shell `run:` block indented inside YAML) — NOT a bare
  # substring match. An unanchored `COVERAGE_FLOOR=<n>` grep matches the
  # first occurrence anywhere in the file, including inside a comment
  # (e.g. a future edit writing "example: COVERAGE_FLOOR=83.5" in prose
  # above the real assignment) — that would lock onto the comment on
  # both sides and let the real value drop underneath it with a green
  # check. Found in independent review of this exact change
  # (uc-infra#113): reproduced by adding a decoy commented-out value
  # ahead of a lowered real one and confirming the unanchored version
  # passed when it should have failed. Anchoring plus asserting exactly
  # one match closes that: a decoy line elsewhere in the file no longer
  # counts as the assignment, and if it ever produced a second anchored
  # match, that also fails loudly rather than silently picking one.
  matches=$(grep -oE '^[[:space:]]*COVERAGE_FLOOR=[0-9]+(\.[0-9]+)?' -- "$file" || true)
  count=$(printf '%s\n' "$matches" | grep -c . || true)
  if [ "$count" -eq 0 ]; then
    echo "::error::Could not find COVERAGE_FLOOR=<n> in $file — refusing to silently pass." >&2
    exit 1
  fi
  if [ "$count" -gt 1 ]; then
    echo "::error::Found ${count} COVERAGE_FLOOR=<n> assignments in $file — expected exactly one, refusing to guess which is real." >&2
    exit 1
  fi
  echo "$matches" | cut -d= -f2 | tr -d '[:space:]'
}

HEAD_FLOOR=$(extract_floor "$HEAD_FILE")
BASE_FLOOR=$(extract_floor "$BASE_FILE")

echo "Coverage floor: base=${BASE_FLOOR}% head=${HEAD_FLOOR}%"

# Same awk numeric-compare idiom ci.yml's own Coverage gate step already
# uses to compare TOTAL against COVERAGE_FLOOR — kept consistent rather
# than reaching for a different comparison mechanism (bc, python, etc.)
# for what's structurally the same "is X at least Y" check.
awk -v h="$HEAD_FLOOR" -v b="$BASE_FLOOR" 'BEGIN { exit (h+0 >= b+0) ? 0 : 1 }' || {
  echo "::error::COVERAGE_FLOOR lowered from ${BASE_FLOOR}% to ${HEAD_FLOOR}% — this gate only ever ratchets up. Add tests to close the gap instead of lowering the floor. If this is a genuine, reviewed exception, that's a Reviewer-level call (universal-core/CLAUDE.md), not something CI should silently allow." >&2
  exit 1
}
