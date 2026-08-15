#!/usr/bin/env bash
#
# run-full-gate.sh (no arguments)
#
# The one command to run for a real, CI-equivalent build+test+coverage
# result in a SINGLE Bash invocation — see uc-infra#215: the harness
# Claude Code runs in as this pipeline's cloud sessions does not persist
# shell state (env vars, `cd`) between separate Bash tool calls. Running
# `export TEST_DATABASE_URL=...` in one tool call and then `go test
# ./...` in a later, separate call silently drops every DB-backed test
# into its own `if base == "" { t.Skip(...) }` guard instead of running
# it for real. The result *looks* like a genuine green suite — every
# package reports `ok` — but ran almost nothing. This script exists so
# there is one command, run once, in one process, that structurally
# cannot produce that false-green shape: it hard-fails before running
# anything expensive if a precondition isn't met, and everything after
# that runs inside this one script, never split across a tool-call
# boundary a caller could get wrong.
#
# Preflight checks are ordered cheapest/local-only first, network last
# (independent review of this exact script recommended this — it also
# means every check except DB reachability itself is exercisable by
# run-full-gate_test.sh without a real Postgres).
#
# Mirrors .github/workflows/ci.yml's own Test/Coverage-gate steps as
# closely as makes sense outside a GitHub Actions runner: gofmt check,
# go build, go vet, apply migrations, go test with the same
# race/parallelism/timeout/coverage flags, then gate on COVERAGE_FLOOR —
# parsed LIVE out of ci.yml itself, never duplicated as a second
# literal (same single-source-of-truth discipline CLAUDE.md's Testing
# section already establishes for that value, and
# .github/scripts/check-coverage-floor.sh already enforces for PRs that
# try to lower it).
#
# Also checks for xmllint and a Chrome/Chromium binary, same as ci.yml's
# own dedicated steps — unlike ci.yml, this script cannot install them,
# so it hard-fails with a clear message instead. Independent review of
# this exact script (uc-infra#215) found the first draft's assumption
# that a missing browser "fails loudly on its own" was only true when
# `$CI` is set (internal/e2e/browser_discovery_test.go's own
# CI-env-gated behavior) — this script now sets `CI=1` itself so that
# behavior is unconditional here, and checks for xmllint explicitly
# since internal/kernel/saft's and internal/kernel/ubl's XSD validation
# tests have no such CI-env escape hatch at all and would otherwise skip
# unconditionally.
#
# Exit codes:
#   0 = full gate passed (build, vet, fmt, migrations, tests, coverage floor)
#   1 = gate failed (a real build/vet/fmt/test/coverage failure, or a
#       precondition this script can't safely proceed without — see the
#       specific ::error:: message)
#   2 = usage error (unrecognized argument)
#   3 = TEST_DATABASE_URL is unset or unreachable — nothing else ran
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# CI_YML_OVERRIDE exists only so run-full-gate_test.sh can point
# extract_coverage_floor at a fixture file without a real checkout —
# never set this when actually running the gate.
CI_YML="${CI_YML_OVERRIDE:-$REPO_ROOT/.github/workflows/ci.yml}"

# extract_coverage_floor <ci-yml-path>
#
# Same anchored-to-line-start, exactly-one-match discipline as
# .github/scripts/check-coverage-floor.sh's own extract_floor — an
# unanchored match could lock onto a decoy COVERAGE_FLOOR=<n> written in
# a comment rather than the real assignment. Deliberately duplicated
# here rather than sourcing that script's internals: that script is
# built around a fixed 2-argument (head, base) usage contract, not a
# reusable single-file extractor, so reaching into it would couple this
# script to an interface that isn't meant to be called that way.
extract_coverage_floor() {
  local file="$1"
  local matches
  local count
  matches=$(grep -oE '^[[:space:]]*COVERAGE_FLOOR=[0-9]+(\.[0-9]+)?' -- "$file" || true)
  count=$(printf '%s\n' "$matches" | grep -c . || true)
  if [ "$count" -eq 0 ]; then
    echo "::error::Could not find COVERAGE_FLOOR=<n> in $file — refusing to silently pass." >&2
    return 1
  fi
  if [ "$count" -gt 1 ]; then
    echo "::error::Found ${count} COVERAGE_FLOOR=<n> assignments in $file — expected exactly one, refusing to guess which is real." >&2
    return 1
  fi
  echo "$matches" | cut -d= -f2 | tr -d '[:space:]'
}

main() {
  if [ "$#" -ne 0 ]; then
    echo "usage: run-full-gate.sh (no arguments)" >&2
    exit 2
  fi

  cd "$REPO_ROOT"

  echo "=== run-full-gate.sh: preflight ==="

  echo "--- TEST_DATABASE_URL check ---"
  if [ -z "${TEST_DATABASE_URL:-}" ]; then
    echo "::error::TEST_DATABASE_URL is not set. Refusing to run any part of the gate without it — a partial run with DB-backed tests silently skipped is exactly the false-green result this script exists to prevent (uc-infra#215). Set it to a real, reachable Postgres, e.g.:" >&2
    echo "  export TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/universal_core_test?sslmode=disable" >&2
    exit 3
  fi

  # cmd/migrate and the server binary itself read DATABASE_URL, not
  # TEST_DATABASE_URL — ci.yml sets both to the same value for the same
  # reason. If the caller already has a DIFFERENT DATABASE_URL exported
  # (e.g. a leftover dev/staging value), refuse rather than guess which
  # one is meant: migrations would land on DATABASE_URL while tests run
  # against TEST_DATABASE_URL, silently leaving the test DB unmigrated
  # or mutating a database nobody meant to touch — found in independent
  # review of this exact script.
  echo "--- DATABASE_URL check ---"
  if [ -n "${DATABASE_URL:-}" ] && [ "$DATABASE_URL" != "$TEST_DATABASE_URL" ]; then
    echo "::error::DATABASE_URL is already set and differs from TEST_DATABASE_URL. Refusing to guess which one migrations should target — unset DATABASE_URL (or set it equal to TEST_DATABASE_URL) before running this script." >&2
    exit 1
  fi
  export DATABASE_URL="$TEST_DATABASE_URL"

  echo "--- coverage floor (parsed live from ci.yml) ---"
  # Parsed here, before the ~20-minute build/test run below, not after
  # it — a missing/renamed/duplicated COVERAGE_FLOOR in ci.yml is a
  # precondition failure, and this script's whole design principle is
  # failing fast on those rather than discovering them only once the
  # expensive part is already done. Independent review of this exact
  # script found the first draft parsed it only at the very end.
  local coverage_floor
  coverage_floor=$(extract_coverage_floor "$CI_YML") || exit 1
  echo "Coverage floor: ${coverage_floor}% (from ci.yml)"

  echo "--- xmllint check (internal/kernel/saft, internal/kernel/ubl XSD validation) ---"
  if ! command -v xmllint >/dev/null 2>&1; then
    echo "::error::xmllint not found on PATH. internal/kernel/saft's and internal/kernel/ubl's XSD validation tests skip unconditionally without it (no CI-env escape hatch, unlike the browser check below) — that's exactly the silent-skip shape this script exists to prevent. Install libxml2-utils (or equivalent) before running this script." >&2
    exit 1
  fi

  echo "--- browser check (internal/e2e real-browser tests) ---"
  # Mirrors internal/e2e/browser_discovery_test.go's own resolveBrowserPath:
  # PATH first (google-chrome/google-chrome-stable/chromium/chromium-
  # browser), then $PLAYWRIGHT_BROWSERS_PATH/chromium as a second,
  # independent source (Playwright-provisioned sandboxes put Chromium
  # there, off PATH entirely — this session's own environment is exactly
  # that case). Checking only PATH here would hard-fail in precisely the
  # environment this script is meant to run in.
  local browser_found=0
  local candidate
  for candidate in google-chrome-stable google-chrome chromium chromium-browser; do
    if command -v "$candidate" >/dev/null 2>&1; then
      browser_found=1
      break
    fi
  done
  if [ "$browser_found" -eq 0 ] && [ -n "${PLAYWRIGHT_BROWSERS_PATH:-}" ] && [ -x "${PLAYWRIGHT_BROWSERS_PATH}/chromium" ]; then
    browser_found=1
  fi
  if [ "$browser_found" -eq 0 ]; then
    echo "::error::No Chrome/Chromium binary found (checked PATH and \$PLAYWRIGHT_BROWSERS_PATH/chromium, same as internal/e2e/browser_discovery_test.go's own resolveBrowserPath) — internal/e2e's real-browser tests need one. Install one, or run somewhere it's already present." >&2
    exit 1
  fi
  # internal/e2e/browser_discovery_test.go only hard-fails a missing
  # browser when $CI is set — otherwise it silently skips, which is
  # exactly the false-green shape this script exists to prevent.
  # Forcing it here makes that failure unconditional for this script,
  # regardless of whether the caller's own shell happens to export CI.
  # Independent review of this exact script found the original draft's
  # header claimed this was already true unconditionally — it wasn't.
  export CI="${CI:-1}"

  echo "--- psql on PATH check ---"
  if ! command -v psql >/dev/null 2>&1; then
    echo "::error::psql not found on PATH — cannot verify TEST_DATABASE_URL is actually reachable before trusting it. Install psql, or run this script somewhere it's available." >&2
    exit 3
  fi

  echo "--- TEST_DATABASE_URL reachability (network) ---"
  # PGCONNECT_TIMEOUT: without it, a firewalled (rather than actively
  # refusing) host makes this "fail fast" check hang instead of failing
  # fast — found in independent review of this exact script.
  if ! PGCONNECT_TIMEOUT=5 psql "$TEST_DATABASE_URL" -c 'select 1' >/dev/null 2>&1; then
    echo "::error::TEST_DATABASE_URL is set but not reachable (psql \"\$TEST_DATABASE_URL\" -c 'select 1' failed within 5s). Refusing to proceed — every DB-backed test needs a real connection to run for real instead of skipping." >&2
    exit 3
  fi
  echo "TEST_DATABASE_URL is set and reachable."

  echo ""
  echo "=== run-full-gate.sh: gate ==="
  local gate_start
  gate_start=$(date +%s)

  echo "--- gofmt check ---"
  # Captured with an explicit exit-code check, not left to `set -e` to
  # propagate `gofmt`'s own status verbatim: a Go file with a genuine
  # *syntax* error (not just bad formatting) makes `gofmt -l` exit 2,
  # which this script's own documented contract reserves for "usage
  # error" — independent review of this exact script caught this
  # exit-code collision. Normalize any non-zero gofmt exit to this
  # script's exit 1 ("gate failed"), the same bucket a real build/vet/
  # test failure lands in.
  local fmt_out fmt_status
  set +e
  fmt_out="$(gofmt -l . 2>&1)"
  fmt_status=$?
  set -e
  if [ "$fmt_status" -ne 0 ]; then
    echo "::error::gofmt exited ${fmt_status} — likely a syntax error, not just missing formatting:" >&2
    echo "$fmt_out" >&2
    exit 1
  fi
  if [ -n "$fmt_out" ]; then
    echo "::error::The following files need gofmt:" >&2
    echo "$fmt_out" >&2
    exit 1
  fi

  echo "--- go build ./... ---"
  go build ./...

  echo "--- go vet ./... ---"
  go vet ./...

  echo "--- apply migrations (go run ./cmd/migrate) ---"
  go run ./cmd/migrate

  echo "--- go test ./... (race, -p 1, coverage) ---"
  local coverpkgs
  coverpkgs=$(go list ./... | grep -v '/cmd/' | tr '\n' ',' | sed 's/,$//')
  if [ -z "$coverpkgs" ]; then
    echo "::error::go list produced no non-cmd packages — refusing to run with an empty -coverpkg (would silently fall back to plain per-package coverage). Same guard as ci.yml's own Test step." >&2
    exit 1
  fi
  go test ./... -race -count=1 -p 1 -timeout=30m -coverprofile=coverage.out -coverpkg="$coverpkgs" -covermode=atomic

  echo "--- coverage gate ---"
  if [ ! -f coverage.out ]; then
    echo "::error::coverage.out not found — the test step above did not produce one." >&2
    exit 1
  fi

  local total
  total=$(go tool cover -func=coverage.out | tail -1 | awk '{print $NF}' | tr -d '%')
  echo "Whole-program coverage: ${total}% (floor: ${coverage_floor}%, from ci.yml)"
  awk -v t="$total" -v f="$coverage_floor" 'BEGIN { exit (t+0 >= f+0) ? 0 : 1 }' || {
    echo "::error::Coverage ${total}% is below the ${coverage_floor}% floor (parsed from ci.yml). This gate only ratchets up — add tests to close the gap, never lower the floor to pass." >&2
    exit 1
  }

  local gate_end total_elapsed
  gate_end=$(date +%s)
  total_elapsed=$((gate_end - gate_start))
  echo ""
  echo "=== run-full-gate.sh: PASSED in ${total_elapsed}s ==="
  if [ "$total_elapsed" -lt 30 ]; then
    echo "::warning::Full gate finished in under 30s. This repo's real DB-backed suite normally takes several minutes (internal/api alone runs ~5-8 minutes against a real Postgres). A run this fast is itself a signal something skipped rather than ran for real — check the go test output above for suspiciously-fast package times before trusting this result." >&2
  fi
}

# Only run the gate when executed directly — sourcing this file (as
# run-full-gate_test.sh does, to exercise extract_coverage_floor in
# isolation) must define the functions above without running any of
# main's side effects.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
