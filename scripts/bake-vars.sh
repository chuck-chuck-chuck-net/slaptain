#!/usr/bin/env bash
# Print the variables docker-bake.hcl needs, derived from the checkout the same
# way the Makefile derives them. Plain KEY=VALUE lines for $GITHUB_ENV, or
# quoted export lines with --shell, because OL26SRC_PATCHES contains spaces.
#
#   CI:     scripts/bake-vars.sh >> "$GITHUB_ENV"
#   local:  eval "$(scripts/bake-vars.sh --shell)" && docker buildx bake --print
set -euo pipefail

cd "$(dirname "$0")/.."

emit() {
    if [[ "${1:-}" == --shell ]]; then
        while IFS='=' read -r key value; do
            printf 'export %s=%q\n' "$key" "$value"
        done
    else
        cat
    fi
}

{
echo "TAG=$(scripts/image-tag.sh main --image)"
echo "EXPORTER_TAG=$(scripts/image-tag.sh exporter --image)"
# The 2.7 OpenLDAP version comes from the packaging fork's changelog (ADR-040
# decision 1): "openldap (2.7.1-0+slaptain3) unstable; ..." -> 2.7.1
echo "OPENLDAP_27_VERSION=$(head -1 images/openldap/debian/changelog | tr -d '()' | awk '{print $2}' | cut -d- -f1)"
# Stamped onto the -ol26src images, derived from the directory so the label
# cannot drift from the patches that were applied.
echo "OL26SRC_PATCHES=$(find images/openldap/local-patches -name '*.patch' -printf '%f\n' | sed 's/\.patch$//' | sort | paste -sd' ')"
} | emit "$@"
