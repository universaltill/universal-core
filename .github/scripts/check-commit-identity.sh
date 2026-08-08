#!/usr/bin/env bash
#
# check-commit-identity.sh <before-sha> <after-sha>
#
# Fails if any commit newly reachable from <after-sha> (but not from
# <before-sha>) has an author or committer email outside the allowlist
# below. Intended to run on every push to `main`, wired as this repo's
# safety net for uc-infra#186/#83: a real personal email address leaked
# into this (PUBLIC) repo's history multiple times because a cycle
# merged a PR via the GitHub API instead of the required local-merge
# procedure (uc-infra/.claude/skills/devops/SKILL.md §5). Doc-only
# guidance ("merge locally") is necessary but was shown, repeatedly, not
# to be sufficient by itself — nothing stopped a cycle from skipping it.
# This script is the thing that catches a violation the moment it lands,
# instead of a future cycle's manual `git log` audit finding it days
# later (which is how both prior leaks were actually discovered).
#
# Deliberately independent of GitHub Actions context beyond the two SHAs
# it's given: it only calls `git rev-list`/`git show` against whatever
# repo it's run inside, so it can be exercised directly against a
# throwaway fixture repo (see check-commit-identity_test.sh) without a
# real push event.
#
# Honest gaps, named rather than left implicit (same discipline as this
# repo's other CI script, check-coverage-floor.sh, and ci.yml's own
# "Honest gap"/"Known, accepted gap" comments):
#   - This is post-hoc DETECTION, not prevention. By the time this check
#     runs, the offending commit is already public. A red check run here
#     means "go fix the allowlist/history problem," never "this push was
#     blocked" — this pipeline has no tool access to configure branch
#     protection that would make it a real gate.
#   - A push that edits this script or commit-identity-guard.yml in the
#     same push as a non-allowlisted commit can weaken or remove the
#     check it's being caught by, the same class of gap
#     check-coverage-floor.sh's tamper-check step exists to close for
#     the coverage floor specifically — nothing equivalent exists here.
#   - The allowlist is deliberately narrow (this pipeline's one pinned
#     identity). Any externally-authored commit reaching `main` —
#     including a legitimate outside contributor's PR, which
#     cla.yml already anticipates — trips this the same as a real leak
#     would. That's intentional, not a bug: widening the allowlist is a
#     deliberate Reviewer-level decision (who else is allowed to author
#     a commit on this public repo's main), never something to do
#     quietly just to turn a red check green.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: check-commit-identity.sh <before-sha> <after-sha>" >&2
  exit 2
fi

BEFORE="$1"
AFTER="$2"

# Emails allowed to author AND commit on this repo's main. This is the
# pipeline's pinned placeholder git identity (both repos' local git
# config pin the same value — see devops/SKILL.md §5's "check git config
# user.email... every time" lesson), not an independent decision made
# here; keep the two in sync. Deliberately a single entry today — see
# the "Honest gaps" note above on what widening this means.
ALLOWLIST=(
  "noreply@anthropic.com"
)

ZERO_SHA="0000000000000000000000000000000000000000"

if [ "$BEFORE" = "$ZERO_SHA" ]; then
  # Branch/ref creation. Only the tip commit is checked, not the whole
  # branch's history — reachable in practice only if `main` were ever
  # deleted and recreated, which this pipeline never does. A full
  # `git rev-list "$AFTER"` is deliberately NOT used here: this repo's
  # pre-existing `main` history already contains the non-allowlisted
  # identities uc-infra#83/#186 are about (36 commits with a real gmail
  # author, 22 with a real GitHub-account committer) — scanning all of
  # history would make this check permanently red on a repo whose
  # current tip is otherwise clean. Tip-only is a deliberate, narrower
  # promise: "nothing new is leaking," not "this branch's history is
  # clean" (it isn't, and fixing that is the separate, Farshid-owned
  # history-rewrite question uc-infra#83 already declined).
  REVLIST=$(git rev-list -1 "$AFTER")
else
  # Refuse rather than guess if BEFORE isn't actually present locally
  # (e.g. a force-push/history-rewrite whose old tip is now orphaned and
  # was never fetched — fetch-depth: 0 only fetches what's reachable
  # from AFTER, not an arbitrary abandoned BEFORE). Left unchecked,
  # `git rev-list BEFORE..AFTER` would fail with a raw, unannotated git
  # fatal error and a plain exit 128 — this fails the same way every
  # other refusal in this script does (an ::error:: annotation, exit 1)
  # instead of a third, undocumented exit code.
  if ! git cat-file -e "${BEFORE}^{commit}" 2>/dev/null; then
    echo "::error::before-SHA $BEFORE is not present locally (force-push or history rewrite?) — cannot determine which commits are new, refusing to guess. If this was a legitimate force-push, this needs a human to confirm main's actual new tip is clean." >&2
    exit 1
  fi
  REVLIST=$(git rev-list "${BEFORE}..${AFTER}")
fi

if [ -z "$REVLIST" ]; then
  echo "No new commits to check (before=$BEFORE after=$AFTER)."
  exit 0
fi

is_allowlisted() {
  local email="$1"
  local allowed
  for allowed in "${ALLOWLIST[@]}"; do
    if [ "$email" = "$allowed" ]; then
      return 0
    fi
  done
  return 1
}

violations=0
checked=0
while IFS= read -r sha; do
  [ -z "$sha" ] && continue
  checked=$((checked + 1))
  # Raw %ae/%ce, not the mailmap-resolved %aE/%cE: this repo has no
  # .mailmap today, but if one is ever added, %aE/%cE would resolve a
  # leaked address through it and silently pass — the whole point here
  # is to see the actual, unmapped commit metadata that ends up public.
  author_email=$(git show -s --format='%ae' "$sha")
  committer_email=$(git show -s --format='%ce' "$sha")
  ok=1
  is_allowlisted "$author_email" || ok=0
  is_allowlisted "$committer_email" || ok=0
  if [ "$ok" -ne 1 ]; then
    echo "::error::Commit $sha has a non-allowlisted author/committer identity (author: $author_email, committer: $committer_email). Allowed: ${ALLOWLIST[*]}. This repo is public — see uc-infra#186 and uc-infra#83. This commit is already public; fixing it going forward means merging with uc-infra/scripts/merge-universal-core-pr.sh (never mcp__github__merge_pull_request or any other GitHub-API-driven merge) — deciding what, if anything, to do about a commit that already landed is a separate, human call." >&2
    violations=$((violations + 1))
  fi
done <<<"$REVLIST"

if [ "$violations" -gt 0 ]; then
  echo "::error::${violations} of ${checked} new commit(s) failed the identity check." >&2
  exit 1
fi

echo "All ${checked} new commit(s) have an allowlisted author and committer."
