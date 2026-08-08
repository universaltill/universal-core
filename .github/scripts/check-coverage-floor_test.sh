#!/usr/bin/env bash
#
# check-coverage-floor_test.sh
#
# Standalone test harness for check-coverage-floor.sh — no CI, no git,
# no Postgres. Run directly: ./.github/scripts/check-coverage-floor_test.sh
# Wired into ci.yml as its own always-run step (not gated on
# pull_request, unlike the tamper-check step that uses the script for
# real) — a script nothing runs isn't actually verified, just read.
#
# This script is shell/CI tooling, not Go, so it never runs through
# `go test` and doesn't move ci.yml's own -coverpkg Go coverage number
# (see ci.yml's Test step comment on what -coverpkg does and doesn't
# cover). It exists so "the floor comparison logic works" is something
# this repo actually verified, not something a future cycle has to trust
# on read-through alone.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-coverage-floor.sh"
REAL_CI_YML="$SCRIPT_DIR/../workflows/ci.yml"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0

# make_fixture <path> <content-line>
make_fixture() {
  local path="$1"
  local line="$2"
  {
    echo "      - name: Coverage gate"
    echo "        run: |"
    if [ -n "$line" ]; then
      echo "          $line"
    fi
    echo "          echo done"
  } > "$path"
}

# run_check <head-file> <base-file> -> sets $LAST_EXIT and $LAST_OUTPUT
run_check() {
  LAST_OUTPUT=$("$CHECK" "$@" 2>&1) && LAST_EXIT=0 || LAST_EXIT=$?
}

# report <name> <expected-exit>
# Prints the captured output only on an unexpected result — a swallowed
# stderr/stdout means a test can pass for the wrong reason (e.g. exit 1
# from a usage error instead of the comparison failure it's meant to
# prove), which is exactly the class of thing a "just check the exit
# code" harness can hide.
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

# assert_case <name> <head-line> <base-line> <expected-exit>
assert_case() {
  local name="$1" head_line="$2" base_line="$3" expected="$4"
  local head_file="$TMP_DIR/head-$name.yml"
  local base_file="$TMP_DIR/base-$name.yml"
  make_fixture "$head_file" "$head_line"
  make_fixture "$base_file" "$base_line"
  run_check "$head_file" "$base_file"
  report "$name" "$expected"
}

assert_case "head-lower-than-base"          "COVERAGE_FLOOR=80.0" "COVERAGE_FLOOR=83.5" 1
assert_case "head-equal-to-base"            "COVERAGE_FLOOR=83.5" "COVERAGE_FLOOR=83.5" 0
assert_case "head-higher-than-base"         "COVERAGE_FLOOR=85.0" "COVERAGE_FLOOR=83.5" 0
assert_case "head-missing-floor"            ""                    "COVERAGE_FLOOR=83.5" 1
assert_case "base-missing-floor"            "COVERAGE_FLOOR=83.5" ""                    1
assert_case "head-lower-integer-vs-decimal" "COVERAGE_FLOOR=83"   "COVERAGE_FLOOR=83.5" 1

# Regression case for the bug independent review found in this exact
# change (uc-infra#113): an unanchored match could lock onto a decoy
# COVERAGE_FLOOR=<n> written in a comment above the real assignment and
# silently pass a real drop underneath it. This fixture puts a decoy
# comment value ahead of a genuinely lowered real value in head, and
# confirms the script still catches the drop rather than matching the
# decoy. Reproduced failing against the pre-fix (unanchored) script
# before the fix landed — see the code-review record for that run.
decoy_head="$TMP_DIR/head-decoy.yml"
decoy_base="$TMP_DIR/base-decoy.yml"
{
  echo "      - name: Coverage floor tamper check"
  echo "        run: |"
  echo "          # A future comment mentioning an example value like this one,"
  echo "          # COVERAGE_FLOOR=83.5, must never be mistaken for the real"
  echo "          # assignment below."
  echo "          echo noop"
  echo "      - name: Coverage gate"
  echo "        run: |"
  echo "          COVERAGE_FLOOR=70.0"
  echo "          echo done"
} > "$decoy_head"
make_fixture "$decoy_base" "COVERAGE_FLOOR=83.5"
run_check "$decoy_head" "$decoy_base"
report "decoy-comment-does-not-shadow-real-drop" 1

# Two anchored assignments in the same file is ambiguous, not a "pick
# the first one" situation — must fail loudly rather than guess.
ambiguous_head="$TMP_DIR/head-ambiguous.yml"
{
  echo "      - name: Coverage gate (old)"
  echo "        run: |"
  echo "          COVERAGE_FLOOR=83.5"
  echo "      - name: Coverage gate (new)"
  echo "        run: |"
  echo "          COVERAGE_FLOOR=85.0"
} > "$ambiguous_head"
make_fixture "$TMP_DIR/base-ambiguous.yml" "COVERAGE_FLOOR=83.5"
run_check "$ambiguous_head" "$TMP_DIR/base-ambiguous.yml"
report "two-anchored-assignments-is-ambiguous" 1

# The real ci.yml, compared against a copy of itself with only the real
# assignment's value edited down — the same scenario Tester and Reviewer
# both ran by hand this cycle, folded into the permanent suite so it
# isn't only a one-off manual check.
if [ -f "$REAL_CI_YML" ]; then
  real_base="$TMP_DIR/real-base-ci.yml"
  real_head_lowered="$TMP_DIR/real-head-lowered-ci.yml"
  cp "$REAL_CI_YML" "$real_base"
  sed 's/COVERAGE_FLOOR=83\.5/COVERAGE_FLOOR=70.0/' "$REAL_CI_YML" > "$real_head_lowered"
  run_check "$real_head_lowered" "$real_base"
  report "real-ci-yml-lowered-floor-is-caught" 1

  run_check "$REAL_CI_YML" "$real_base"
  report "real-ci-yml-unchanged-passes" 0
else
  echo "SKIP: real-ci.yml cases ($REAL_CI_YML not found — run from a checkout that has it)"
fi

# Nonexistent file path: fails loudly (same "could not find" path as a
# file with no COVERAGE_FLOOR line), not an uninformative crash.
nonexistent_base="$TMP_DIR/base-for-nonexistent-check.yml"
make_fixture "$nonexistent_base" "COVERAGE_FLOOR=83.5"
run_check "$TMP_DIR/does-not-exist.yml" "$nonexistent_base"
report "nonexistent-head-file" 1

# Argument-count validation — distinct exit code (2) from every
# comparison failure above (1), so a caller can tell "used it wrong"
# apart from "found a real regression."
run_check "only-one-arg"
report "one-arg" 2

run_check "a" "b" "c"
report "three-args" 2

run_check
report "zero-args" 2

echo ""
echo "check-coverage-floor_test.sh: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
