#!/usr/bin/env bash
#
# check-help-allowlist_test.sh
#
# Standalone test harness for check-help-allowlist.sh — no CI, no git,
# no Postgres. Run directly:
#   ./.github/scripts/check-help-allowlist_test.sh
# Wired into ci.yml as its own always-run step (not gated on
# pull_request, unlike the tamper-check step that uses the script for
# real) — a script nothing runs is only ever read, not verified. Same
# shape as check-coverage-floor_test.sh, which this file is a direct
# copy of the idiom of.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-help-allowlist.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0

# make_fixture <path> <key>...
# Writes a minimal but structurally real coverage_test.go fixture with
# one undocumentedAllowlist entry per remaining argument. No keys at all
# (called with just a path) writes a file with no map, simulating "this
# gate doesn't exist yet in this revision" (the bootstrap case).
make_fixture() {
  local path="$1"
  shift
  {
    echo "package help_test"
    echo
    if [ "$#" -gt 0 ]; then
      echo "var undocumentedAllowlist = map[string]bool{"
      for key in "$@"; do
        echo "	\"${key}\": true,"
      done
      echo "}"
    fi
  } > "$path"
}

# run_check <head-file> <base-file> -> sets $LAST_EXIT and $LAST_OUTPUT
run_check() {
  LAST_OUTPUT=$("$CHECK" "$@" 2>&1) && LAST_EXIT=0 || LAST_EXIT=$?
}

# report <name> <expected-exit>
# Prints the captured output only on an unexpected result — a swallowed
# stderr/stdout means a test can pass for the wrong reason.
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

# --- same key set: never fails, removal-only or no-op edits ---

f_head="$TMP_DIR/head-same.go"; f_base="$TMP_DIR/base-same.go"
make_fixture "$f_head" A B C
make_fixture "$f_base" A B C
run_check "$f_head" "$f_base"
report "identical-key-sets" 0

f_head="$TMP_DIR/head-shrunk.go"; f_base="$TMP_DIR/base-shrunk.go"
make_fixture "$f_head" A
make_fixture "$f_base" A B C
run_check "$f_head" "$f_base"
report "head-only-removes-keys" 0

f_head="$TMP_DIR/head-emptied.go"; f_base="$TMP_DIR/base-emptied.go"
make_fixture "$f_head"          # no map at all: fully documented, gate removed the whole thing
make_fixture "$f_base" A
run_check "$f_head" "$f_base"
report "head-map-entirely-removed-not-flagged-as-missing" 1
# NOTE: expected 1 here — extract_keys(head)=0 keys hits the "could not
# find undocumentedAllowlist in HEAD_FILE" refusal, same as
# check-coverage-floor.sh refusing a head file with no COVERAGE_FLOOR at
# all. Deleting the whole map is exactly the "deleting the whole step"
# class of gap this script's header names as accepted-but-named, not
# silently passed — this assertion is what proves it's actually named,
# not just claimed.

# --- growth: the actual thing this script exists to catch ---

f_head="$TMP_DIR/head-grew.go"; f_base="$TMP_DIR/base-grew.go"
make_fixture "$f_head" A B NewEntity
make_fixture "$f_base" A B
run_check "$f_head" "$f_base"
report "head-adds-a-key-fails" 1

f_head="$TMP_DIR/head-grew-multi.go"; f_base="$TMP_DIR/base-grew-multi.go"
make_fixture "$f_head" A B C D
make_fixture "$f_base" A
run_check "$f_head" "$f_base"
report "head-adds-multiple-keys-fails" 1

f_head="$TMP_DIR/head-swap.go"; f_base="$TMP_DIR/base-swap.go"
make_fixture "$f_head" A NewOne
make_fixture "$f_base" A B
run_check "$f_head" "$f_base"
report "head-removes-one-adds-another-still-fails-on-the-addition" 1

# --- bootstrap: base predates the map entirely ---

f_head="$TMP_DIR/head-bootstrap.go"; f_base="$TMP_DIR/base-bootstrap.go"
make_fixture "$f_head" A B C
make_fixture "$f_base"   # no map: simulates this gate's own introducing PR
run_check "$f_head" "$f_base"
report "base-has-no-allowlist-yet-is-the-bootstrap-case-not-a-failure" 0

# --- usage / missing files ---

run_check "$TMP_DIR/does-not-exist-head.go" "$TMP_DIR/does-not-exist-base.go"
report "head-file-does-not-exist-fails" 1

run_check "only-one-arg.go"
report "wrong-arg-count-fails" 2

echo
echo "check-help-allowlist_test.sh: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
