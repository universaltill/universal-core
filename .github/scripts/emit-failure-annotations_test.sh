#!/usr/bin/env bash
#
# emit-failure-annotations_test.sh
#
# Standalone test harness for emit-failure-annotations.sh — no CI, no
# git, no Postgres. Run directly:
#   ./.github/scripts/emit-failure-annotations_test.sh
# Wired into ci.yml as its own always-run step (not gated on `failure()`,
# unlike the step that uses the script for real) — same
# "a script nothing runs is only ever read, not verified" discipline as
# check-coverage-floor_test.sh / check-help-allowlist_test.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EMIT="$SCRIPT_DIR/emit-failure-annotations.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0

# run_emit <file...> -> sets $LAST_EXIT and $LAST_OUTPUT
run_emit() {
  LAST_OUTPUT=$("$EMIT" "$@" 2>&1) && LAST_EXIT=0 || LAST_EXIT=$?
}

# report <name> <expected-exit>
# Prints the captured output only on an unexpected result — see
# check-coverage-floor_test.sh's own identical helper for why (a swallowed
# stderr/stdout means a test can pass for the wrong reason).
report() {
  local name="$1" expected="$2"
  if [ "$LAST_EXIT" -eq "$expected" ]; then
    echo "PASS: $name (exit $LAST_EXIT, expected $expected)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (exit $LAST_EXIT, expected $expected)"
    echo "  --- output ---"
    echo "$LAST_OUTPUT" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# assert_contains <name> <needle>
assert_contains() {
  local name="$1" needle="$2"
  if printf '%s' "$LAST_OUTPUT" | grep -qF -- "$needle"; then
    echo "PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name — expected output to contain: $needle"
    echo "  --- output ---"
    echo "$LAST_OUTPUT" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# assert_not_contains <name> <needle>
assert_not_contains() {
  local name="$1" needle="$2"
  if printf '%s' "$LAST_OUTPUT" | grep -qF -- "$needle"; then
    echo "FAIL: $name — expected output NOT to contain: $needle"
    echo "  --- output ---"
    echo "$LAST_OUTPUT" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $name"
    PASS=$((PASS + 1))
  fi
}

# assert_count <name> <needle> <expected-count>
assert_count() {
  local name="$1" needle="$2" expected="$3" actual
  actual=$(printf '%s' "$LAST_OUTPUT" | grep -cF -- "$needle" || true)
  if [ "$actual" -eq "$expected" ]; then
    echo "PASS: $name ($actual occurrences)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (found $actual occurrences of \"$needle\", expected $expected)"
    FAIL=$((FAIL + 1))
  fi
}

# assert_reconstruction <name> <expected-content>
# Concatenates every "(part N/M)" annotation's message body, in emission
# order, and compares it byte-for-byte against the content it should
# reproduce. Only reads "(part ...)" annotations — the leading
# "(earlier output omitted)" notice (when present) is a different title
# and doesn't match, so it's excluded automatically. Exists because a
# uniform-character fixture ('x' repeated) cannot catch a chunk-boundary
# bug: a mutant that drops or duplicates one character per chunk still
# "looks the same" when every character is identical. A fixture whose
# characters encode their own position (see marker_content below) turns
# any lost/duplicated/reordered character into a real mismatch.
assert_reconstruction() {
  local name="$1" expected="$2" actual
  actual=$(printf '%s\n' "$LAST_OUTPUT" \
    | grep -E '^::error title=Test failure detail \(part [0-9]+/[0-9]+\)::' \
    | sed -E 's/^::error title=Test failure detail \([^)]*\):://' \
    | tr -d '\n')
  if [ "$actual" = "$expected" ]; then
    echo "PASS: $name (reconstructed ${#actual} chars, matches)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (reconstructed ${#actual} chars, expected ${#expected} chars — content mismatch at a chunk boundary)"
    FAIL=$((FAIL + 1))
  fi
}

# marker_content <n> -> n characters, each the decimal digit (0-9) of its
# own position mod 10 — e.g. "0123456789012345...". Unlike a uniform
# fixture, dropping, duplicating, or reordering even one character breaks
# the repeating pattern in a way a byte-for-byte comparison catches.
# Contains no '%'/'\r'/'\n', so escape() is a no-op on it and the raw
# marker string can be compared directly against reconstructed output.
marker_content() {
  awk -v n="$1" 'BEGIN { for (i = 0; i < n; i++) printf "%d", i % 10 }'
}

# assert_line_count <name> <expected-embedded-newline-count>
# Counts newline characters embedded WITHIN $LAST_OUTPUT (via `wc -l` on
# a value that, per run_emit's `$(...)` capture, already had its own
# single trailing newline stripped — so N annotation lines joined by
# newlines contain N-1 embedded newlines, and the expected value passed
# in is always one less than the annotation-line count). Used to prove no
# *raw* newline escaped an annotation's escaping (a raw '\n' mid-message
# would silently split one workflow command into two lines — exactly the
# corruption uc-infra#251's escaping exists to prevent). Deliberately not
# implemented as `assert_not_contains` with a literal-newline needle: GNU
# grep -F treats a multi-line PATTERN as a list of alternatives (one per
# line), matching if ANY line matches — not "these two lines appear
# adjacent" — so it can't actually detect an embedded raw newline; a real
# newline count can.
assert_line_count() {
  local name="$1" expected="$2" actual
  actual=$(printf '%s' "$LAST_OUTPUT" | wc -l | tr -d '[:space:]')
  if [ "$actual" -eq "$expected" ]; then
    echo "PASS: $name ($actual lines)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (found $actual lines, expected $expected)"
    echo "  --- output ---"
    echo "$LAST_OUTPUT" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# --- Argument validation ---

run_emit
report "zero-args" 2

run_emit "a" "b"
report "two-args" 2

run_emit "$TMP_DIR/does-not-exist.txt"
report "nonexistent-file" 1

# --- Empty input: no captured output, but a real failure happened ---

empty_file="$TMP_DIR/empty.txt"
: > "$empty_file"
run_emit "$empty_file"
report "empty-input-exit-code" 0
assert_contains "empty-input-says-no-output" "no captured output to extract"

# --- Small input: fits in a single chunk, no truncation notice ---

small_file="$TMP_DIR/small.txt"
printf 'FAIL: TestSomething\n  expected 1, got 2\n' > "$small_file"
run_emit "$small_file"
report "small-input-exit-code" 0
assert_count "small-input-single-annotation" '::error title=Test failure detail (part 1/1)::' 1
assert_not_contains "small-input-no-truncation-notice" "truncated)"
assert_contains "small-input-content-present" "expected 1, got 2"
# Real newlines inside the extract must never reach stdout raw — a bare
# '\n' mid-annotation would silently cut the workflow command in two,
# which is exactly the failure mode uc-infra#251 exists to fix. The input
# has one embedded newline; if it survived unescaped, this would emit 2
# lines instead of 1.
assert_line_count "small-input-no-raw-newline-in-message" 0
assert_contains "small-input-escaped-newline" '%0A'

# --- Percent-sign escaping order: '%' must escape before '%0A'/'%0D' are
#     introduced, or the just-added percent signs get double-escaped.
#     The '%' and the newline must be in the SAME fixture, with the
#     newline NOT trailing (a trailing newline is stripped by $(cat ...)
#     before escape() ever sees it, which would make this fixture
#     indistinguishable from one with no newline at all — order can only
#     be verified with both characters present in the string escape()
#     actually receives). ---

percent_file="$TMP_DIR/percent.txt"
printf 'coverage 83.5%% is below the 90%% floor\nnext line\n' > "$percent_file"
run_emit "$percent_file"
report "percent-input-exit-code" 0
assert_contains "percent-escaped-correctly" '83.5%25 is below the 90%25 floor%0Anext line'
assert_not_contains "percent-not-double-escaped" '%250A'
assert_not_contains "percent-not-double-escaped-2" '%2525'

# --- Carriage-return escaping: untested by every other fixture above,
#     none of which contain a literal \r. ---

cr_file="$TMP_DIR/cr.txt"
printf 'line one\r\nline two\rEND' > "$cr_file"
run_emit "$cr_file"
report "cr-input-exit-code" 0
assert_contains "cr-escaped-correctly" 'line one%0D%0Aline two%0DEND'
assert_line_count "cr-input-no-raw-cr-or-newline-in-message" 0

# --- Chunk-boundary content continuity: concatenating every emitted
#     chunk must reproduce the source exactly, byte for byte — no
#     character lost, duplicated, or reordered at a seam. ---

continuity_small_file="$TMP_DIR/continuity-small.txt"
continuity_small_content="$(marker_content 8123)"   # several chunks, no truncation
printf '%s' "$continuity_small_content" > "$continuity_small_file"
run_emit "$continuity_small_file"
report "continuity-small-exit-code" 0
assert_reconstruction "continuity-small-reproduces-source-exactly" "$continuity_small_content"

# --- Large input: exceeds MAX_CHUNKS * CHUNK_SIZE, must truncate loudly,
#     never silently, AND must keep the TAIL of the content (not the
#     head) — a `go test -timeout` panic's own detail, and the whole
#     point of uc-infra#251, prints at the very end of a log this large,
#     not the start. ---

# CHUNK_SIZE=3000, MAX_CHUNKS=9 in emit-failure-annotations.sh -> 27000
# chars fits exactly in 9 chunks with nothing left over. One char more
# forces exactly 1 character of truncation, dropped from the START.
large_content="$(marker_content 27001)"
large_file="$TMP_DIR/large.txt"
printf '%s' "$large_content" > "$large_file"
run_emit "$large_file"
report "large-input-exit-code" 0
assert_count "large-input-emits-exactly-9-content-chunks" 'Test failure detail (part' 9
assert_contains "large-input-has-truncation-notice" "Test failure detail (earlier output omitted)"
assert_contains "large-input-truncation-count-is-1-char" "1 characters from the START"
# The notice must NOT send an API-only reader to a channel it structurally
# cannot reach — that was the bug this fixture pins (uc-infra#251's
# review, finding #2): the step summary is mentioned only as context for
# a human, never as actionable advice for the reader of this annotation.
assert_contains "large-input-explains-step-summary-is-human-only" "visible only to a human in the Actions UI"
# Reconstructing the emitted chunks must equal the LAST 27000 characters
# of the source (the tail, not the head) — the one-char-dropped case.
assert_reconstruction "large-input-keeps-the-tail-not-the-head" "${large_content:1:27000}"

# --- Exactly at the boundary: no truncation notice when content fits
#     precisely in the reserved chunk budget, and the full content
#     (nothing dropped) reproduces exactly. ---

boundary_content="$(marker_content 27000)"
boundary_file="$TMP_DIR/boundary.txt"
printf '%s' "$boundary_content" > "$boundary_file"
run_emit "$boundary_file"
report "boundary-input-exit-code" 0
assert_count "boundary-input-emits-exactly-9-content-chunks" 'Test failure detail (part' 9
assert_not_contains "boundary-input-no-truncation-notice" "omitted)"
assert_reconstruction "boundary-input-reproduces-source-exactly" "$boundary_content"

echo ""
echo "emit-failure-annotations_test.sh: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
