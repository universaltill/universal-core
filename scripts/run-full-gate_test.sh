#!/usr/bin/env bash
#
# run-full-gate_test.sh
#
# Standalone test harness for run-full-gate.sh — no Go build, no real
# test suite run, and (per uc-infra#215's own independent review) no
# real Postgres either: run-full-gate.sh's preflight checks are
# deliberately ordered cheapest/local-only first, network (DB
# reachability) last, so every check except the DB-reachability one
# itself is exercisable here via plain env var / PATH manipulation.
# Run directly: ./scripts/run-full-gate_test.sh
#
# Covers what uc-infra#215 asked for, plus the gaps its own independent
# review found:
#   (a) TEST_DATABASE_URL-unset hard-fail (the original false-green root
#       cause) and DATABASE_URL-mismatch hard-fail — both real subprocess
#       invocations, so the exit-code *and* message contract is verified
#       end-to-end, not just read.
#   (b) extract_coverage_floor's parsing logic, against fixture ci.yml
#       files — exercised by sourcing run-full-gate.sh (the
#       `[[ "${BASH_SOURCE[0]}" == "${0}" ]]` guard at its end means
#       sourcing only defines functions, it never runs the gate itself).
#   (c) the psql-missing vs. TEST_DATABASE_URL-unreachable distinction —
#       these are different failure causes that must not collapse into
#       an indistinguishable "exit 3", found by independent review.
#
# Deliberately does NOT spin up a real Postgres or run the full gate
# end-to-end here — that's exactly what a real `TEST_DATABASE_URL`-set
# invocation of run-full-gate.sh against this repo already does, and
# duplicating it in a second harness would just be the full gate again,
# not a unit test of this script's own logic. The xmllint/browser
# preflight checks and the gofmt-syntax-error exit-code fix are verified
# by manual exercise instead (see uc-infra/docs/code-reviews for this
# card's record) — mocking them here would mean faking PATH lookups for
# tools this harness itself needs (grep/cut/tr live in the same
# directory as xmllint on every image checked), which is more fragile
# than the manual check it would replace.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/run-full-gate.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0

# report_exit <name> <expected-exit> <actual-exit>
report_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    echo "PASS: $name (exit $actual, expected $expected)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (exit $actual, expected $expected)"
    FAIL=$((FAIL + 1))
  fi
}

# assert_contains <name> <file> <needle>
assert_contains() {
  local name="$1" file="$2" needle="$3"
  if grep -q -- "$needle" "$file"; then
    echo "PASS: $name (found '$needle')"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (did not find '$needle' in $file)"
    echo "  --- output ---"
    sed 's/^/  /' "$file"
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# assert_not_contains <name> <file> <needle>
assert_not_contains() {
  local name="$1" file="$2" needle="$3"
  if grep -q -- "$needle" "$file"; then
    echo "FAIL: $name (unexpectedly found '$needle' — a gate step ran that should not have)"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $name ('$needle' correctly absent)"
    PASS=$((PASS + 1))
  fi
}

# --- (a) TEST_DATABASE_URL unset hard-fails before running anything ---

out="$TMP_DIR/unset.out"
set +e
env -u TEST_DATABASE_URL -u DATABASE_URL "$GATE" >"$out" 2>&1
actual=$?
set -e
report_exit "unset-TEST_DATABASE_URL-exits-3" 3 "$actual"
assert_contains "unset-TEST_DATABASE_URL-message" "$out" "TEST_DATABASE_URL is not set"
assert_not_contains "unset-TEST_DATABASE_URL-no-gate-step-ran" "$out" "=== run-full-gate.sh: gate ==="

# --- DATABASE_URL set and differs from TEST_DATABASE_URL: hard-fails, distinct from the DB checks ---

out="$TMP_DIR/mismatch.out"
set +e
env TEST_DATABASE_URL="postgres://a/db1" DATABASE_URL="postgres://b/db2" "$GATE" >"$out" 2>&1
actual=$?
set -e
report_exit "DATABASE_URL-mismatch-exits-1" 1 "$actual"
assert_contains "DATABASE_URL-mismatch-message" "$out" "DATABASE_URL is already set and differs"
assert_not_contains "DATABASE_URL-mismatch-no-gate-step-ran" "$out" "=== run-full-gate.sh: gate ==="

# DATABASE_URL equal to TEST_DATABASE_URL must NOT trip the mismatch
# check — it should proceed past it (and fail later, on the coverage
# floor or psql check, which is fine; this only asserts the mismatch
# message is absent).
out="$TMP_DIR/equal.out"
set +e
env TEST_DATABASE_URL="postgres://a/db1" DATABASE_URL="postgres://a/db1" CI_YML_OVERRIDE="$TMP_DIR/does-not-exist.yml" "$GATE" >"$out" 2>&1
actual=$?
set -e
assert_not_contains "DATABASE_URL-equal-does-not-trip-mismatch" "$out" "DATABASE_URL is already set and differs"

# --- (c) psql-missing vs. unreachable: distinct causes, must not collapse silently ---

if command -v psql >/dev/null 2>&1; then
  # psql is present in this environment: exercise the "set but
  # unreachable" branch for real, and confirm the message names
  # unreachability, not a missing binary.
  out="$TMP_DIR/unreachable.out"
  set +e
  env -u DATABASE_URL TEST_DATABASE_URL="postgres://nobody:nobody@127.0.0.1:1/does-not-exist?sslmode=disable" "$GATE" >"$out" 2>&1
  actual=$?
  set -e
  report_exit "unreachable-TEST_DATABASE_URL-exits-3" 3 "$actual"
  assert_contains "unreachable-TEST_DATABASE_URL-message" "$out" "not reachable"
else
  # psql itself is absent here: the only reachable branch is the
  # missing-binary one — assert that specifically, so this environment
  # doesn't silently skip testing exit 3 altogether.
  out="$TMP_DIR/no-psql.out"
  set +e
  env -u DATABASE_URL TEST_DATABASE_URL="postgres://irrelevant" "$GATE" >"$out" 2>&1
  actual=$?
  set -e
  report_exit "missing-psql-exits-3" 3 "$actual"
  assert_contains "missing-psql-message" "$out" "psql not found on PATH"
fi

# --- Usage: unexpected argument is a distinct exit code from the DB/usage checks above ---

out="$TMP_DIR/usage.out"
set +e
env TEST_DATABASE_URL="postgres://irrelevant" "$GATE" extra-arg >"$out" 2>&1
actual=$?
set -e
report_exit "unexpected-argument-exits-2" 2 "$actual"

# --- (b) extract_coverage_floor parsing, via sourcing (no gate execution) ---

# shellcheck source=./run-full-gate.sh
source "$GATE"

make_fixture() {
  local path="$1" line="$2"
  {
    echo "      - name: Coverage gate"
    echo "        run: |"
    if [ -n "$line" ]; then
      echo "          $line"
    fi
    echo "          echo done"
  } > "$path"
}

f="$TMP_DIR/simple.yml"
make_fixture "$f" "COVERAGE_FLOOR=83.5"
got="$(extract_coverage_floor "$f")"
if [ "$got" = "83.5" ]; then
  echo "PASS: extract-simple-floor (got $got)"
  PASS=$((PASS + 1))
else
  echo "FAIL: extract-simple-floor (got '$got', expected 83.5)"
  FAIL=$((FAIL + 1))
fi

f="$TMP_DIR/missing.yml"
make_fixture "$f" ""
set +e
extract_coverage_floor "$f" >/dev/null 2>"$TMP_DIR/missing.err"
actual=$?
set -e
report_exit "extract-missing-floor-fails" 1 "$actual"

f="$TMP_DIR/decoy.yml"
{
  echo "      - name: Coverage floor tamper check"
  echo "        run: |"
  echo "          # example: COVERAGE_FLOOR=83.5 (a decoy in a comment)"
  echo "          echo noop"
  echo "      - name: Coverage gate"
  echo "        run: |"
  echo "          COVERAGE_FLOOR=70.0"
} > "$f"
got="$(extract_coverage_floor "$f")"
if [ "$got" = "70.0" ]; then
  echo "PASS: extract-does-not-shadow-on-decoy-comment (got $got)"
  PASS=$((PASS + 1))
else
  echo "FAIL: extract-does-not-shadow-on-decoy-comment (got '$got', expected 70.0)"
  FAIL=$((FAIL + 1))
fi

f="$TMP_DIR/ambiguous.yml"
{
  echo "          COVERAGE_FLOOR=83.5"
  echo "          COVERAGE_FLOOR=85.0"
} > "$f"
set +e
extract_coverage_floor "$f" >/dev/null 2>"$TMP_DIR/ambiguous.err"
actual=$?
set -e
report_exit "extract-two-assignments-is-ambiguous" 1 "$actual"

# The real ci.yml in this checkout, if present — confirms the live
# parsing actually matches this repo's own file, not just fixtures.
# Asserts the result is a well-formed numeric percentage, not a fixed
# literal: COVERAGE_FLOOR only ever ratchets up (CLAUDE.md's Testing
# section), so hardcoding today's value here would make this test fail
# the next time the floor legitimately rises — the same trap
# .github/scripts/check-coverage-floor_test.sh's own real-ci.yml cases
# avoid by comparing relative (lowered/unchanged), not absolute, values.
REAL_CI_YML="$SCRIPT_DIR/../.github/workflows/ci.yml"
if [ -f "$REAL_CI_YML" ]; then
  set +e
  got="$(extract_coverage_floor "$REAL_CI_YML" 2>"$TMP_DIR/real.err")"
  actual=$?
  set -e
  if [ "$actual" -eq 0 ] && [[ "$got" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    echo "PASS: extract-real-ci-yml (got ${got}%, well-formed)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: extract-real-ci-yml (exit $actual, got '$got')"
    FAIL=$((FAIL + 1))
  fi
else
  echo "SKIP: real-ci.yml case ($REAL_CI_YML not found — run from a checkout that has it)"
fi

echo ""
echo "run-full-gate_test.sh: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
