#!/usr/bin/env bash
# Verify the CHECKED-IN generated files are up to date with their Go sources.
#
# Three sets of files are generated from the API types and their kubebuilder
# markers, and all three are checked in:
#
#   operator/api/v1alpha1/zz_generated.deepcopy.go   controller-gen object
#   operator/config/{crd,rbac}                       controller-gen crd/rbac
#   charts/operator/crds                             the chart's copy of the CRDs
#
# Nothing in CI used to notice when they went stale: edit a marker, forget
# `make operator-manifests operator-generate`, and the tree ships wrong
# manifests while every check stays green. This closes that.
#
# Method: regenerate, then compare the content of those paths before and after.
# Deliberately NOT `git diff --exit-code`: a developer with unrelated
# uncommitted edits under operator/config must get a useful answer rather than
# a false positive. Only a change CAUSED BY regenerating is drift; edits that
# survive regeneration untouched are consistent with the Go sources and are
# none of this check's business.
set -euo pipefail
cd "$(dirname "$0")/.."

PATHS=(
  operator/config
  operator/api/v1alpha1/zz_generated.deepcopy.go
  charts/operator/crds
)

snapshot() { # snapshot -> "<sha256>  <path>" for every file under PATHS, sorted
  find "${PATHS[@]}" -type f -print0 2>/dev/null \
    | sort -z \
    | xargs -0 -r sha256sum
}

# Keep a copy of the pre-regeneration state so the failure message can show the
# actual diff. `git diff` alone is not enough: when the stale file is an
# uncommitted local edit, regenerating restores it to match HEAD and git has
# nothing left to show.
saved="$(mktemp -d)"
trap 'rm -rf "${saved}"' EXIT
for p in "${PATHS[@]}"; do
  [[ -e "${p}" ]] || continue
  mkdir -p "${saved}/$(dirname "${p}")"
  cp -a "${p}" "${saved}/${p}"
done

before="$(snapshot)"
echo "generated-files check: regenerating..."
make operator-manifests operator-generate >/dev/null
after="$(snapshot)"

if [[ "${before}" == "${after}" ]]; then
  echo "OK: the checked-in generated files are up to date."
  exit 0
fi

# A file drifted if its hash changed, or if it appeared/disappeared.
# `|| true`: diff and grep both exit non-zero on the paths this branch exists
# to report, and the script runs under `set -e -o pipefail`.
drifted="$( { diff <(printf '%s\n' "${before}") <(printf '%s\n' "${after}") || true; } \
  | { grep -E '^[<>]' || true; } \
  | awk '{ print $3 }' \
  | sort -u)"

echo
echo "FAIL: the checked-in generated files are STALE."
echo "  Fix: run 'make operator-manifests operator-generate' and commit the result."
echo
echo "  Drifted (regenerating changed these):"
printf '%s\n' "${drifted}" | sed 's/^/    /'
echo
echo "  What regenerating changed (- checked in, + generated):"
while IFS= read -r f; do
  [[ -n "${f}" ]] || continue
  if [[ ! -e "${saved}/${f}" ]]; then
    echo "--- ${f}: NEW file, was not checked in"
    continue
  fi
  if [[ ! -e "${f}" ]]; then
    echo "--- ${f}: REMOVED by regeneration"
    continue
  fi
  diff -u --label "a/${f}" --label "b/${f}" "${saved}/${f}" "${f}" || true
done <<< "${drifted}" | sed 's/^/    /'
exit 1
