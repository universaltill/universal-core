#!/usr/bin/env bash
#
# check-commit-identity_test.sh
#
# Standalone test harness for check-commit-identity.sh — no CI, no real
# GitHub push event. Run directly:
#   ./.github/scripts/check-commit-identity_test.sh
# Wired into commit-identity-guard.yml (same "a script nothing runs is
# only ever read, not verified" reasoning as check-coverage-floor_test.sh).
#
# Builds real throwaway git repos with real commits under controlled
# GIT_AUTHOR_EMAIL/GIT_COMMITTER_EMAIL, then asserts the script's exit
# code and behavior against them — same style as
# check-coverage-floor_test.sh's fixture-based assertions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-commit-identity.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0

# report <name> <expected-exit> <actual-exit> <output>
report() {
  local name="$1" expected="$2" actual="$3" output="$4"
  if [ "$actual" -eq "$expected" ]; then
    echo "PASS: $name (exit $actual, expected $expected)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (exit $actual, expected $expected)"
    echo "  --- output ---"
    echo "$output" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# assert_contains <name> <haystack> <needle>
assert_contains() {
  local name="$1" haystack="$2" needle="$3"
  if echo "$haystack" | grep -qF -- "$needle"; then
    echo "PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name — expected output to contain: $needle"
    echo "  --- output ---"
    echo "$haystack" | sed 's/^/  /'
    echo "  --------------"
    FAIL=$((FAIL + 1))
  fi
}

# make_repo <path> — an empty git repo with commit-friendly defaults.
make_repo() {
  local path="$1"
  git init -q "$path"
  (
    cd "$path"
    git config user.name "fixture"
    git config user.email "fixture@example.invalid"
  )
}

# commit_as <repo> <name> <email> <message>
commit_as() {
  local repo="$1" name="$2" email="$3" message="$4"
  (
    cd "$repo"
    GIT_AUTHOR_NAME="$name" GIT_AUTHOR_EMAIL="$email" \
      GIT_COMMITTER_NAME="$name" GIT_COMMITTER_EMAIL="$email" \
      git commit -q --allow-empty -m "$message"
  )
}

ALLOWED_NAME="Claude"
ALLOWED_EMAIL="noreply@anthropic.com"
# Not a real person's name — this file's whole job is proving nothing
# non-allowlisted lands on this public repo, so it must not itself be
# the commit that introduces a real name (caught in review: an earlier
# draft used the actual repo owner's name here).
LEAKED_NAME="Not Allowlisted"
LEAKED_EMAIL="not-a-real-address@example.invalid"

# --- Case 1: single allowlisted commit passes ---
repo1="$TMP_DIR/repo1"
make_repo "$repo1"
commit_as "$repo1" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before1=$(git -C "$repo1" rev-parse HEAD)
commit_as "$repo1" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "allowlisted change"
after1=$(git -C "$repo1" rev-parse HEAD)
out1=$(cd "$repo1" && "$CHECK" "$before1" "$after1" 2>&1) && exit1=0 || exit1=$?
report "single-allowlisted-commit-passes" 0 "$exit1" "$out1"

# --- Case 1b: multiple new commits, all allowlisted, passes as one
# clean range (not just the single-commit shape of case 1). ---
repo1b="$TMP_DIR/repo1b"
make_repo "$repo1b"
commit_as "$repo1b" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before1b=$(git -C "$repo1b" rev-parse HEAD)
commit_as "$repo1b" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit 1"
commit_as "$repo1b" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit 2"
commit_as "$repo1b" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit 3"
after1b=$(git -C "$repo1b" rev-parse HEAD)
out1b=$(cd "$repo1b" && "$CHECK" "$before1b" "$after1b" 2>&1) && exit1b=0 || exit1b=$?
report "multiple-clean-commits-in-range-passes" 0 "$exit1b" "$out1b"
assert_contains "multiple-clean-commits-reports-correct-count" "$out1b" "All 3 new commit(s)"

# --- Case 2: a non-allowlisted author fails ---
repo2="$TMP_DIR/repo2"
make_repo "$repo2"
commit_as "$repo2" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before2=$(git -C "$repo2" rev-parse HEAD)
commit_as "$repo2" "$LEAKED_NAME" "$LEAKED_EMAIL" "leaked-identity change"
after2=$(git -C "$repo2" rev-parse HEAD)
out2=$(cd "$repo2" && "$CHECK" "$before2" "$after2" 2>&1) && exit2=0 || exit2=$?
report "non-allowlisted-author-fails" 1 "$exit2" "$out2"
assert_contains "non-allowlisted-author-names-the-offending-email" "$out2" "$LEAKED_EMAIL"

# --- Case 3: committer differs from author, and only committer leaks ---
# (the rebase-merge case devops/SKILL.md §5 documents: a clean author,
# a leaked committer — this must still be caught, not waved through
# because the author field alone looked fine.)
repo3="$TMP_DIR/repo3"
make_repo "$repo3"
commit_as "$repo3" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before3=$(git -C "$repo3" rev-parse HEAD)
(
  cd "$repo3"
  GIT_AUTHOR_NAME="$ALLOWED_NAME" GIT_AUTHOR_EMAIL="$ALLOWED_EMAIL" \
    GIT_COMMITTER_NAME="$LEAKED_NAME" GIT_COMMITTER_EMAIL="$LEAKED_EMAIL" \
    git commit -q --allow-empty -m "clean author, leaked committer"
)
after3=$(git -C "$repo3" rev-parse HEAD)
out3=$(cd "$repo3" && "$CHECK" "$before3" "$after3" 2>&1) && exit3=0 || exit3=$?
report "leaked-committer-alone-still-fails" 1 "$exit3" "$out3"

# --- Case 4: multiple new commits, only one bad — still fails, and
# checks every commit in the range (not just the tip). ---
repo4="$TMP_DIR/repo4"
make_repo "$repo4"
commit_as "$repo4" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before4=$(git -C "$repo4" rev-parse HEAD)
commit_as "$repo4" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit 1"
commit_as "$repo4" "$LEAKED_NAME" "$LEAKED_EMAIL" "leaked commit in the middle"
commit_as "$repo4" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit 2 (tip)"
after4=$(git -C "$repo4" rev-parse HEAD)
out4=$(cd "$repo4" && "$CHECK" "$before4" "$after4" 2>&1) && exit4=0 || exit4=$?
report "one-bad-commit-among-several-still-fails" 1 "$exit4" "$out4"

# --- Case 5: before == after (nothing new) is a clean no-op pass ---
repo5="$TMP_DIR/repo5"
make_repo "$repo5"
commit_as "$repo5" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
same5=$(git -C "$repo5" rev-parse HEAD)
out5=$(cd "$repo5" && "$CHECK" "$same5" "$same5" 2>&1) && exit5=0 || exit5=$?
report "before-equals-after-is-a-noop-pass" 0 "$exit5" "$out5"

# --- Case 6: branch creation (before is the all-zero SHA) still checks
# the new tip commit, rather than skipping silently. ---
repo6="$TMP_DIR/repo6"
make_repo "$repo6"
commit_as "$repo6" "$LEAKED_NAME" "$LEAKED_EMAIL" "first commit on a brand-new branch"
after6=$(git -C "$repo6" rev-parse HEAD)
zero_sha="0000000000000000000000000000000000000000"
out6=$(cd "$repo6" && "$CHECK" "$zero_sha" "$after6" 2>&1) && exit6=0 || exit6=$?
report "branch-creation-still-checks-the-tip-commit" 1 "$exit6" "$out6"

# --- Case 6b: documents a known, accepted limitation (see the script's
# own comment) — branch-creation checks ONLY the tip, so a bad commit
# earlier on a brand-new branch is missed if the tip itself is clean.
# This asserts the actual (limited) behavior on purpose, so the gap is
# pinned by a test rather than silently relied upon. ---
repo6b="$TMP_DIR/repo6b"
make_repo "$repo6b"
commit_as "$repo6b" "$LEAKED_NAME" "$LEAKED_EMAIL" "bad commit, not the tip"
commit_as "$repo6b" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean tip"
after6b=$(git -C "$repo6b" rev-parse HEAD)
out6b=$(cd "$repo6b" && "$CHECK" "$zero_sha" "$after6b" 2>&1) && exit6b=0 || exit6b=$?
report "branch-creation-known-gap-misses-non-tip-commit" 0 "$exit6b" "$out6b"

# --- Case 7: argument-count validation — distinct exit code (2) ---
out7=$("$CHECK" "only-one-arg" 2>&1) && exit7=0 || exit7=$?
report "one-arg" 2 "$exit7" "$out7"

out7b=$("$CHECK" 2>&1) && exit7b=0 || exit7b=$?
report "zero-args" 2 "$exit7b" "$out7b"

out7c=$("$CHECK" "one" "two" "three" 2>&1) && exit7c=0 || exit7c=$?
report "three-args" 2 "$exit7c" "$out7c"

# --- Case 8: the multi-parent merge commit shape the intended
# `git merge --no-ff` procedure actually produces — this is the
# realistic shape of what lands on main, not the linear --allow-empty
# chains above. A bad commit anywhere in the merged-in branch, INCLUDING
# under a clean merge commit itself, must still be caught. ---
repo8="$TMP_DIR/repo8"
make_repo "$repo8"
(
  cd "$repo8"
  git checkout -q -b main
)
commit_as "$repo8" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before8=$(git -C "$repo8" rev-parse HEAD)
(
  cd "$repo8"
  git checkout -q -b feature
)
commit_as "$repo8" "$LEAKED_NAME" "$LEAKED_EMAIL" "leaked commit on the feature branch"
(
  cd "$repo8"
  git checkout -q main
  GIT_AUTHOR_NAME="$ALLOWED_NAME" GIT_AUTHOR_EMAIL="$ALLOWED_EMAIL" \
    GIT_COMMITTER_NAME="$ALLOWED_NAME" GIT_COMMITTER_EMAIL="$ALLOWED_EMAIL" \
    git merge -q --no-ff --no-edit -m "Merge feature" feature
)
after8=$(git -C "$repo8" rev-parse HEAD)
out8=$(cd "$repo8" && "$CHECK" "$before8" "$after8" 2>&1) && exit8=0 || exit8=$?
report "leaked-commit-under-a-clean-no-ff-merge-still-fails" 1 "$exit8" "$out8"
feature_sha8=$(git -C "$repo8" rev-parse feature)
assert_contains "leaked-commit-under-merge-names-the-feature-commit-not-just-the-merge-commit" "$out8" "$feature_sha8"

# --- Case 9: a fully clean `git merge --no-ff` (the actual blessed
# procedure — see merge-universal-core-pr.sh in the uc-infra repo) must
# pass: the merge commit AND the merged-in branch commit are both
# allowlisted. ---
repo9="$TMP_DIR/repo9"
make_repo "$repo9"
(
  cd "$repo9"
  git checkout -q -b main
)
commit_as "$repo9" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "seed"
before9=$(git -C "$repo9" rev-parse HEAD)
(
  cd "$repo9"
  git checkout -q -b feature
)
commit_as "$repo9" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "clean commit on the feature branch"
(
  cd "$repo9"
  git checkout -q main
  GIT_AUTHOR_NAME="$ALLOWED_NAME" GIT_AUTHOR_EMAIL="$ALLOWED_EMAIL" \
    GIT_COMMITTER_NAME="$ALLOWED_NAME" GIT_COMMITTER_EMAIL="$ALLOWED_EMAIL" \
    git merge -q --no-ff --no-edit -m "Merge feature" feature
)
after9=$(git -C "$repo9" rev-parse HEAD)
out9=$(cd "$repo9" && "$CHECK" "$before9" "$after9" 2>&1) && exit9=0 || exit9=$?
report "clean-no-ff-merge-passes" 0 "$exit9" "$out9"
assert_contains "clean-no-ff-merge-checks-both-commits" "$out9" "All 2 new commit(s)"

# --- Case 10: BEFORE not present locally (the real force-push shape —
# GitHub's event.before points at a commit this checkout never fetched
# because it's no longer reachable from AFTER). Must fail closed with a
# clear ::error:: and exit 1, not a raw git fatal / exit 128. ---
repo10="$TMP_DIR/repo10"
make_repo "$repo10"
commit_as "$repo10" "$ALLOWED_NAME" "$ALLOWED_EMAIL" "a commit that will become orphaned"
orphan_sha10=$(git -C "$repo10" rev-parse HEAD)
# Simulate "never fetched": a syntactically valid but wholly unknown SHA
# this repo has no object for, standing in for a force-push's old tip
# that a fetch-depth:0-of-AFTER checkout would never have pulled down.
unknown_before10="0123456789abcdef0123456789abcdef01234567"
out10=$(cd "$repo10" && "$CHECK" "$unknown_before10" "$orphan_sha10" 2>&1) && exit10=0 || exit10=$?
report "missing-before-object-fails-closed-not-raw-git-fatal" 1 "$exit10" "$out10"
assert_contains "missing-before-object-has-clear-error-annotation" "$out10" "::error::before-SHA"

echo ""
echo "check-commit-identity_test.sh: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
