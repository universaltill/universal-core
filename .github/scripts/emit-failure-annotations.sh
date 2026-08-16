#!/usr/bin/env bash
#
# emit-failure-annotations.sh <extract-file>
#
# Reads the failing-test extract ci.yml's "Publish failing test output to
# step summary" step already computes (uc-infra#240) and ALSO emits it as
# ::error:: workflow-command annotations, chunked to fit GitHub Actions'
# own constraints. uc-infra#251: $GITHUB_STEP_SUMMARY has no REST/GraphQL
# API exposure, so a cloud pipeline session reading a CI failure through
# the API alone (no browser, no blob-storage log access — every other
# channel available to such a session was tried and failed, see that
# issue's own writeup) had no way to see *why* a build failed. The Checks
# API's annotations, unlike the step summary, ARE reachable that way
# (`pull_request_read`'s `get_check_runs`, or
# `GET .../check-runs/{id}/annotations`) — this script is what actually
# populates them with the same detail, not just the generic
# "Process completed with exit code 1." GitHub already attaches.
#
# GitHub Actions only reliably surfaces the first 10 error annotations
# per step (undocumented in GitHub's own reference docs, but consistently
# reported and empirically reproduced — see this change's review record
# for sources). This reserves the last of those 10 slots for an explicit
# "N characters truncated, see the step summary" notice instead of
# silently dropping the tail — the same "no silent caps" discipline this
# repo already applies elsewhere (e.g. CLAUDE.md's coverage-floor
# ratchet), just for an annotation budget instead of a test count.
#
# Deliberately independent of any CI/git context: it only reads one file
# path and writes to stdout, so it can be exercised directly against
# fixture files (see emit-failure-annotations_test.sh) without a real
# `go test` failure or checkout.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: emit-failure-annotations.sh <extract-file>" >&2
  exit 2
fi

FILE="$1"

# No GitHub-documented per-message character cap was found for ::error::
# (checked: workflow-commands reference, Checks API reference, and
# several community reports of the *count* limits below — none state a
# message-length limit) — 3000 is a deliberately conservative guess that
# stays well under every limit actually observed in practice, not a
# figure taken from an official source. If GitHub ever documents (or this
# repo ever hits) a different real limit, update this constant with a
# comment pointing at the evidence, the same way the 10-per-step constant
# below is justified.
CHUNK_SIZE=3000

# GitHub Actions annotations: 10 error + 10 warning per step, 50 per job,
# 50 per run (community-confirmed across multiple independent reports;
# GitHub's own reference docs don't state this number, but it's been
# empirically consistent). Reserve the 10th slot for the truncation
# notice rather than emitting 10 content chunks and silently dropping
# anything beyond the 11th.
MAX_CHUNKS=9

if [ ! -f "$FILE" ]; then
  echo "::error::emit-failure-annotations.sh: input file not found: $FILE" >&2
  exit 1
fi

# Escape per GitHub's own documented workflow-command data escaping,
# applied in this exact order: '%' must be escaped BEFORE the '%0D'/'%0A'
# this introduces for '\r'/'\n' — escaping '%' second would re-escape the
# percent signs just added into '%250D'/'%250A', corrupting them.
#
# Message-body escaping ONLY — this does not also escape ':' -> '%3A' or
# ',' -> '%2C', which GitHub's workflow-command syntax additionally
# requires for *property* values (e.g. the `title=` attribute below).
# That's fine today because `title=` is always a static literal
# containing neither character; if a future change interpolates anything
# dynamic (a test name, a file path) into `title=`, it needs its own
# escaping, not this function — reusing `escape()` there would emit an
# unparseable command the moment the interpolated value contained a ':'.
escape() {
  local s="$1"
  s="${s//%/%25}"
  s="${s//$'\r'/%0D}"
  s="${s//$'\n'/%0A}"
  printf '%s' "$s"
}

# Known, accepted gap: a NUL byte in the extract is silently dropped by
# the `$(cat "$FILE")` command substitution above (bash warns on stderr,
# doesn't fail) — `-race`/e2e output can rarely carry stray binary. The
# annotation still emits, just missing that byte; not worth a binary-safe
# rewrite for a cosmetic, extremely rare edge case.
CONTENT="$(cat "$FILE")"
TOTAL_LEN=${#CONTENT}

if [ "$TOTAL_LEN" -eq 0 ]; then
  echo "::error::Test step failed but produced no captured output to extract — check the job's other steps for which one actually failed."
  exit 0
fi

# Tail-biased, not head-biased: EXTRACT can be the *entire* test-output
# log verbatim (ci.yml's own "no '--- FAIL:' line at all" fallback, for a
# `-timeout` panic — `go test` never prints '--- FAIL:' on a timeout, so
# the whole log becomes EXTRACT). A timeout panic and its goroutine dump
# print at the very END of that log, after potentially thousands of
# `ok`/`--- PASS:` lines — chunking from the START, as an earlier version
# of this script did, filled every annotation with passing-test noise and
# emitted the actual failure NEVER, for exactly the failure shape
# uc-infra#240's own independent review already flagged the step-summary
# extraction as blind to. Keeping the LAST MAX_CHUNKS*CHUNK_SIZE
# characters instead means the failure survives truncation in both
# branches: the timeout case (detail at the end of the whole log) and the
# multi-`--- FAIL:` case (the per-package `^FAIL` summary lines ci.yml's
# own extraction appends last, after every -B60/-A30 block).
KEEP_LEN=$((MAX_CHUNKS * CHUNK_SIZE))
DROPPED=0
START=0
if [ "$TOTAL_LEN" -gt "$KEEP_LEN" ]; then
  DROPPED=$((TOTAL_LEN - KEEP_LEN))
  START=$DROPPED
fi
KEPT_LEN=$((TOTAL_LEN - START))
EMIT_COUNT=$(( (KEPT_LEN + CHUNK_SIZE - 1) / CHUNK_SIZE ))

# Emitted first (not last): a reader scanning annotations top-to-bottom
# sees immediately that this is a partial view, before any content, not
# after. Deliberately does NOT point at $GITHUB_STEP_SUMMARY as if it
# were a viable next step — this script's whole reason to exist is that
# an API-only cloud session (uc-infra#251) cannot reach that channel; a
# human reading the Actions UI already has the complete extract there
# without needing this notice at all.
if [ "$DROPPED" -gt 0 ]; then
  echo "::error title=Test failure detail (earlier output omitted)::${DROPPED} characters from the START of this extract were omitted — GitHub Actions caps annotations per step. The chunks below cover the MOST RECENT portion, which is where a Go test panic/timeout's own detail (and the per-package FAIL summary) appear. This is the complete detail reachable via the Checks API; a full copy also exists in this job's step summary, but that is visible only to a human in the Actions UI, not through this API channel."
fi

# Honest, accepted gap: chunking by raw byte/character offset can split a
# multi-byte UTF-8 sequence across two annotations if the extract contains
# non-ASCII text right at a chunk boundary — cosmetic (one chunk's tail or
# the next chunk's head renders as a replacement character), not a
# correctness or security issue, and Go test output is overwhelmingly
# ASCII. Not worth the complexity of a UTF-8-aware splitter for that.
i=0
offset=$START
while [ "$i" -lt "$EMIT_COUNT" ]; do
  PART="${CONTENT:$offset:$CHUNK_SIZE}"
  ESCAPED="$(escape "$PART")"
  echo "::error title=Test failure detail (part $((i + 1))/${EMIT_COUNT})::${ESCAPED}"
  offset=$((offset + CHUNK_SIZE))
  i=$((i + 1))
done
