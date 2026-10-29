#!/usr/bin/env bash
# Single source of the image tag derivation. Both the root Makefile (GIT_TAG,
# EXPORTER_GIT_TAG) and tests/e2e.sh call this — the rule must never fork
# between them.
#
# Usage: image-tag.sh [main|exporter] [--image]     (default: main)
#
# Two version lines live in this repository (docs/VERSIONING.md):
#   main      git tags vX.Y.Z           — operator, slapd, slapd-init, toolkit, charts
#   exporter  git tags exporter/vX.Y.Z  — the slapd-exporter module and image
#             (the tag form Go requires for a module in a subdirectory)
# Each stream matches ONLY its own tag pattern, so a commit carrying both
# v0.4.0 and exporter/v0.2.0 resolves unambiguously in either stream.
#
# Rule (per stream):
#   - HEAD sits on an exact tag of the stream  → that tag        (releases)
#   - clean working tree                       → short hash      (content-addressed)
#   - dirty working tree                       → <hash>-dirty-<state8>
#
# The printed value is what HEAD IS; the callers turn it into how the image is
# ADDRESSED (strip `v` / `exporter/v` from a version tag, prefix sha- otherwise).
# --image prints that addressed form instead, for callers with no Makefile to
# do it (docker-bake.hcl in CI): v0.2.1 -> 0.2.1, 09ecf10 -> sha-09ecf10.
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

stream=main image=
for arg in "$@"; do
    case "$arg" in
        main|exporter) stream=$arg ;;
        --image)       image=1 ;;
        *)             echo "usage: $0 [main|exporter] [--image]" >&2; exit 2 ;;
    esac
done

case "$stream" in
    main)     match='v*';          prefix='v' ;;
    exporter) match='exporter/v*'; prefix='exporter/v' ;;
esac

if exact=$(git describe --tags --exact-match --match "$match" 2>/dev/null) && [[ -n "$exact" ]]; then
    tag=$exact
else
    hash=$(git rev-parse --short HEAD)
    if git diff --quiet HEAD 2>/dev/null; then
        tag=$hash
    else
        state=$(git diff HEAD | sha256sum | cut -c1-8)
        tag="${hash}-dirty-${state}"
    fi
fi

# Same version-tag test as the Makefile's IS_VERSION_TAG: a prefix match, so an
# rc (v0.2.1-rc1) is addressed like any other tagged build.
if [[ -z "$image" ]]; then
    echo "$tag"
elif [[ "$tag" =~ ^${prefix}[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    echo "${tag#"$prefix"}"
else
    echo "sha-${tag}"
fi
