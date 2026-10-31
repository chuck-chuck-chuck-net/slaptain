#!/usr/bin/env bash
# Single source of the image tag derivation. Both the root Makefile (GIT_TAG,
# EXPORTER_GIT_TAG) and tests/e2e.sh call this — the rule must never fork
# between them.
#
# Usage: image-tag.sh [--image] [main|exporter]     (default: main)
#
# --image prints how the build is ADDRESSED in a registry rather than what it
# IS: a version tag loses its `v` / `exporter/v` prefix, anything else gains a
# `sha-` one. That rule used to live in the Makefile, which was fine while
# `make` was the only thing that built images. It is not any more -- the
# pipelines drive buildx directly -- and a second copy of it in a workflow is
# exactly the fork the paragraph below forbids.
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

# GNU coreutils spells it sha256sum, macOS spells it `shasum -a 256`. This is
# on the DIRTY-TREE path, which is the everyday developer path rather than an
# edge case, so assuming the GNU one breaks tag derivation for a whole class
# of machine on the command they run most.
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum
    else
        shasum -a 256
    fi
}

addressed=0
if [[ "${1:-}" == "--image" ]]; then
    addressed=1
    shift
fi

case "${1:-main}" in
    main)     match='v*'; strip='v' ;;
    exporter) match='exporter/v*'; strip='exporter/v' ;;
    *)        echo "usage: $0 [--image] [main|exporter]" >&2; exit 2 ;;
esac

# What HEAD is -> how it is addressed. A version tag is addressed by its bare
# semver, so the chart version, the appVersion and the image tag are one
# string and `image.tag | default .Chart.AppVersion` is correct by
# construction. Everything else gets the sha- prefix the sibling project's
# docker/metadata-action publishes, which also makes a registry listing
# self-describing: `0.2.0` is a release, `sha-09ecf10` is a build of a commit.
#
# The version test is a PREFIX match so a pre-release counts: v0.2.1-rc1 is
# addressed :0.2.1-rc1, not sha-<hash>. Whether an rc is the headline release
# is a different question with a different predicate -- see docs/VERSIONING.md
# trap 2, where reusing one for both is how an rc ends up published under a
# nonsense tag.
address() {
    local what="$1"
    if [[ $addressed -eq 0 ]]; then
        echo "$what"
    elif [[ "$what" =~ ^"$strip"[0-9]+\.[0-9]+\.[0-9]+ ]]; then
        echo "${what#"$strip"}"
    else
        echo "sha-$what"
    fi
}

if exact=$(git describe --tags --exact-match --match "$match" 2>/dev/null) && [[ -n "$exact" ]]; then
    address "$exact"
    exit 0
fi

hash=$(git rev-parse --short HEAD)
if git diff --quiet HEAD 2>/dev/null; then
    address "$hash"
else
    state=$(git diff HEAD | sha256 | cut -c1-8)
    address "${hash}-dirty-${state}"
fi
