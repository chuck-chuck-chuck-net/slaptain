#!/usr/bin/env bash
# Single source of the image tag derivation. Both the root Makefile (GIT_TAG)
# and tests/e2e.sh call this — the rule must never fork between them.
#
# Rule:
#   - HEAD sits on an exact git tag            → that tag        (releases)
#   - clean working tree                       → short hash      (content-addressed)
#   - dirty working tree                       → <hash>-dirty-<state8>
#
# The dirty suffix is a content hash of `git diff HEAD`, not a bare "-dirty":
# a bare suffix aliases EVERY dirty state of one commit to the same tag, so a
# stale image in the registry (or on the nodes) can silently masquerade as the
# current tree. With the state hash, the same dirty tree reproduces the same
# tag (Makefile stamps and e2e.sh agree), and a different edit gets a different
# tag. Untracked files are invisible to `git diff HEAD` and therefore to the
# tag — same blindness the old rule had; `git add -N` a new file if it must
# influence the tag before its first commit.
set -euo pipefail

cd "$(dirname "$0")/.."

if exact=$(git describe --tags --exact-match 2>/dev/null) && [[ -n "$exact" ]]; then
    echo "$exact"
    exit 0
fi

hash=$(git rev-parse --short HEAD)
if git diff --quiet HEAD 2>/dev/null; then
    echo "$hash"
else
    state=$(git diff HEAD | sha256sum | cut -c1-8)
    echo "${hash}-dirty-${state}"
fi
