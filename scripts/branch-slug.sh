#!/usr/bin/env bash
# The image tag of a branch's dev images (ADR-046): the branch name with every
# character an OCI tag cannot hold replaced by '-'. publish-dev.yml tags with it
# and cleanup-dev.yml deletes by it, so both must call this one rule.
#
# Usage: branch-slug.sh <branch>
#
# The slug stops at 100 characters, which leaves room inside OCI's 128 for the
# longest flavour suffix docker-bake.hcl appends (-ol<version>-slaptain).
set -euo pipefail

branch=${1:?usage: branch-slug.sh <branch>}
slug=$(printf '%s' "$branch" | tr -c 'A-Za-z0-9._-' '-' | sed 's/^[.-]*//' | cut -c1-100)
if [[ -z "$slug" ]]; then
    echo "branch-slug.sh: '$branch' leaves no usable tag" >&2
    exit 1
fi
echo "$slug"
