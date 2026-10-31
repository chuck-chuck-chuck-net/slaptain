#!/usr/bin/env bash
# The one way images get built, here and in every pipeline.
#
# docker-bake.hcl describes the graph; this derives the handful of values HCL
# cannot work out for itself and invokes buildx. It exists so that a pipeline
# needs nothing but buildx and a shell -- no make, which in CI was never doing
# the one job make is for (a fresh checkout has no stamps, so nothing is
# incremental) and only ever carried configuration that belongs here.
#
# Usage:
#   scripts/bake.sh [--push|--import|--cacheonly] [TARGET|GROUP ...]
#
#   --cacheonly  (default) build and publish nothing: a syntax-and-gates check
#   --push       build and push to $REGISTRY
#   --import     build to docker-archive tarballs under .build/, for the
#                node-import path on clusters with no reachable registry
#
# Anything after the mode is handed to `buildx bake` unchanged, so
# `scripts/bake.sh --push images` and `scripts/bake.sh slapd --print` both work.
set -euo pipefail

cd "$(dirname "$0")/.."

mode=cacheonly
case "${1:-}" in
    --push)      mode=push;      shift ;;
    --import)    mode=import;    shift ;;
    --cacheonly) mode=cacheonly; shift ;;
esac

# ── values HCL cannot derive ────────────────────────────────────────────────

# Both version lines, addressed the way a registry sees them. The rule lives
# in image-tag.sh and must never be re-implemented anywhere else.
export IMAGE_TAG="${IMAGE_TAG:-$(scripts/image-tag.sh --image)}"
export EXPORTER_IMAGE_TAG="${EXPORTER_IMAGE_TAG:-$(scripts/image-tag.sh --image exporter)}"

# The OpenLDAP version, taken from the packaging fork's changelog rather than
# written down twice (ADR-040 decision 1), so the tag and the package cannot
# disagree. Sanitised for OCI: a tag may not contain every character a Debian
# version may.
if [[ -z "${OPENLDAP_27_VERSION:-}" ]]; then
    OPENLDAP_27_VERSION=$(head -1 images/openldap-deb/debian/changelog \
        | tr -d '()' | awk '{print $2}' | cut -d- -f1)
fi
export OPENLDAP_27_VERSION

# The -ol26src patch list, derived from the directory so the image's label
# cannot claim a patch that was not applied.
if [[ -z "${OL26SRC_PATCHES:-}" ]]; then
    OL26SRC_PATCHES=$(cd images/openldap-deb/local-patches 2>/dev/null \
        && ls -1 ./*.patch 2>/dev/null | sed 's|^\./||; s|\.patch$||' | tr '\n' ' ' | sed 's/ $//')
fi
export OL26SRC_PATCHES

export REGISTRY="${REGISTRY:-ghcr.io/chuck-chuck-chuck-net}"
export PROJECT="${PROJECT:-slaptain}"

# buildx is a standalone binary on some hosts and a Docker CLI plugin on
# others (a GitHub runner, Arch's docker-buildx package). BUILDX="docker
# buildx" covers the second; the array is so the two words survive.
read -r -a BUILDX_CMD <<< "${BUILDX:-buildx}"

# ── invoke ──────────────────────────────────────────────────────────────────

case "$mode" in
    cacheonly)
        exec "${BUILDX_CMD[@]}" bake "$@"
        ;;
    push)
        OUTPUT="type=registry" exec "${BUILDX_CMD[@]}" bake "$@"
        ;;
    import)
        # One tarball per image, so each can be streamed to a node CRI
        # separately. The per-target destination is why this is a loop rather
        # than one --set: a single dest would have every image overwrite the
        # last. The target list comes from bake itself, never from a second
        # copy of it here.
        # python3 is used only to read bake's own resolved target list. It is
        # a laptop-side dependency: --import exists for clusters with no
        # reachable registry, which is not a situation CI is ever in. Fail
        # here rather than let the mapfile come back empty and look like
        # "no targets matched".
        if ! command -v python3 >/dev/null; then
            echo "bake.sh: --import needs python3 to resolve the target list" >&2
            exit 1
        fi
        mkdir -p .build
        # A read loop rather than mapfile: mapfile is bash 4, and macOS still
        # ships bash 3.2 as /bin/bash. The shebang saves anyone with a newer
        # bash on PATH and nobody else, which is not a distinction worth
        # making for four lines.
        targets=()
        while IFS= read -r t; do
            [[ -n $t ]] && targets+=("$t")
        done < <("${BUILDX_CMD[@]}" bake --print "$@" 2>/dev/null \
            | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["target"]))')
        if [[ ${#targets[@]} -eq 0 ]]; then
            echo "bake.sh: no targets resolved for: $*" >&2
            exit 1
        fi
        for t in "${targets[@]}"; do
            echo "==> $t -> .build/${t}.tar"
            "${BUILDX_CMD[@]}" bake --set "${t}.output=type=docker,dest=.build/${t}.tar" "$@" "$t"
        done
        ;;
esac
