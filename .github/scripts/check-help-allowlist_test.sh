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

# make_fixture_empty_map <path> — writes a fixture whose
# undocumentedAllowlist map is DECLARED but holds zero keys (the
# single-line `map[string]bool{}` gofmt spelling — the real shape
# uc-infra#148 landed once every module slice's entries were removed).
# Deliberately distinct from make_fixture called with no keys, which
# writes NO map declaration at all (simulating the map never having
# existed yet) — these two are the exact pair independent review found
# check-help-allowlist.sh's original HEAD_COUNT/BASE_COUNT-only logic
# could not tell apart, both reading as zero keys.
make_fixture_empty_map() {
  local path="$1"
  {
    echo "package help_test"
    echo
    echo "var undocumentedAllowlist = map[string]bool{}"
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
# NOTE: expected 1 here — has_allowlist_map(head) is false, hitting the
# "could not find undocumentedAllowlist in HEAD_FILE" refusal, same as
# check-coverage-floor.sh refusing a head file with no COVERAGE_FLOOR at
# all. Deleting the whole map DECLARATION is exactly the "deleting the
# whole step" class of gap this script's header names as
# accepted-but-named, not silently passed — this assertion is what
# proves it's actually named, not just claimed. This is deliberately
# distinct from the map being declared with zero keys — see the next
# case — which must NOT hit this same refusal.

f_head="$TMP_DIR/head-declared-empty.go"; f_base="$TMP_DIR/base-declared-empty-source.go"
make_fixture_empty_map "$f_head"   # map DECLARED, zero keys: the real end state uc-infra#148 landed
make_fixture "$f_base" A B C
run_check "$f_head" "$f_base"
report "head-map-declared-with-zero-keys-is-a-legal-full-shrink-not-a-missing-map" 0
# NOTE: expected 0 — this is the exact case independent review of
# uc-infra#148 found broken: a head file that legitimately closes the
# ratchet all the way to zero keys (map[string]bool{} still present)
# must pass, not be indistinguishable from "map deleted" above. Both
# read as zero keys under HEAD_COUNT alone; has_allowlist_map is what
# tells them apart.

f_head="$TMP_DIR/head-grows-from-declared-empty-base.go"; f_base="$TMP_DIR/base-declared-empty.go"
make_fixture_empty_map "$f_base"       # base already fully closed the ratchet (declared, zero keys)
make_fixture "$f_head" NewEntity       # head re-adds a key against that zero-key base
run_check "$f_head" "$f_base"
report "base-declared-empty-is-a-real-comparison-base-not-a-second-bootstrap-case" 1
# NOTE: expected 1 — a base whose map is declared with zero keys is a
# real, comparable ratchet state (the allowlist is legitimately empty),
# not "the gate doesn't exist yet in base" (the actual bootstrap case,
# tested below, is base having NO map declaration at all). Treating a
# zero-key base as bootstrap would skip the real comparison and let this
# head's added key through uncaught.

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
