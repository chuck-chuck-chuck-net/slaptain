#!/usr/bin/env bash
# Derive the slapd-mesh chart's `mesh:` block from lab.yaml.
#
# Derives, rather than creates: this is a pure function of the lab file. It
# contacts no cluster, changes nothing, and writes to stdout.
#
# This fills the gap between two well-defined things: lab.yaml describes a
# SUBSTRATE (which clusters exist, how to reach them, which hypervisor holds
# their VMs) and charts/slapd-mesh deploys a DIRECTORY. The mesh topology —
# which sites, which serverID slot, which network, which trust Secrets — is the
# part of the deployment that is fully determined by the substrate, so it is
# generated rather than written twice.
#
# What it does NOT emit, and never will: `cluster`, `databases`, `schemas`.
# Those are choices, not facts about the lab. Keep them in a second values file
# and hand both to helm:
#
#   ./scripts/mesh-derive-topology.sh > values.topology.yaml
#   helm upgrade --install ldap charts/slapd-mesh \
#       -f values.topology.yaml -f values.directory.yaml
#
# The output is mesh-scoped and therefore applied UNCHANGED at every site. It
# contains no kube context, no node IP and no credential: those live in
# lab.yaml and stop here (ADR-028 §3).
#
# Usage:
#   ./scripts/mesh-derive-topology.sh [-f lab.yaml] [--mesh-name NAME]
set -euo pipefail

LAB="${E2E_CONFIG:-}"
MESH_NAME=""

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 [-f LAB_FILE] [--mesh-name NAME]

Generates the slapd-mesh chart's mesh: block from a lab description.

  -f, --file FILE     lab description (default: \$E2E_CONFIG, else <repo>/lab.yaml)
      --mesh-name N   SlapdMesh object name (default: slaptain.meshName, else slapd-mesh)
  -h, --help          this text
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f|--file)    LAB="$2"; shift 2 ;;
        --mesh-name)  MESH_NAME="$2"; shift 2 ;;
        -h|--help)    usage ;;
        *)            die "Unknown argument: $1" ;;
    esac
done

if [[ -z "$LAB" ]]; then
    LAB="$(cd "$(dirname "$0")/.." && pwd)/lab.yaml"
fi
[[ -f "$LAB" ]] || die "no lab file at $LAB (pass -f, or set E2E_CONFIG)"
command -v yq >/dev/null 2>&1 || die "yq v4 is required (https://github.com/mikefarah/yq)"

get() { yq -r "$1 // \"\"" "$LAB"; }

[[ -z "$MESH_NAME" ]] && MESH_NAME="$(get '.slaptain.meshName')"
[[ -z "$MESH_NAME" ]] && MESH_NAME="slapd-mesh"

count=$(yq -r '.sites | length' "$LAB")
[[ "$count" == "null" || "$count" -eq 0 ]] && die "$LAB declares no sites"

# Validate before emitting: a mesh values file that renders is not the same as
# one that is right, and these three mistakes are all silent at render time.
declare -A seen_name seen_index
for i in $(seq 0 $((count - 1))); do
    name=$(yq -r ".sites[$i].name // \"\"" "$LAB")
    index=$(yq -r ".sites[$i].serverIDIndex // \"\"" "$LAB")
    [[ -z "$name" ]] && die "$LAB: sites[$i] has no name. The name IS the site identity — it becomes the operator's SITE_NAME, the <site>-ca and <site>-kubeconfig Secret names, and what seed.site is matched against."
    [[ -z "$index" ]] && die "$LAB: site '$name' has no serverIDIndex. Required and never inferred from position: the index is the site's serverID decade (index × 100), and olcServerID is baked into every CSN its pods have written."
    [[ -n "${seen_name[$name]:-}" ]] && die "$LAB: two sites are named '$name'. Two sites claiming one identity collide their serverID decades."
    [[ -n "${seen_index[$index]:-}" ]] && die "$LAB: serverIDIndex $index is claimed by '${seen_index[$index]}' and '$name'. One decade cannot hold two sites — their writes read as already-seen."
    seen_name[$name]=1
    seen_index[$index]="$name"
done

mode=$(get '.slaptain.replicationNetwork.mode')
multus=$(get '.slaptain.replicationNetwork.multusNetwork')

printf '# GENERATED — do not edit. Regenerate with:\n'
printf '#   %s -f %s\n' "$(basename "$0")" "$LAB"
printf '# source: %s (sha256 %s)\n' "$LAB" "$(sha256sum "$LAB" | cut -c1-16)"
printf '#\n'
printf '# Mesh-scoped: apply this UNCHANGED at every site. It deliberately carries no\n'
printf '# kube context, node IP or credential — those stay in the lab file.\n'
printf 'mesh:\n'
printf '  name: %s\n' "$MESH_NAME"
printf '  sites:\n'
for i in $(seq 0 $((count - 1))); do
    name=$(yq -r ".sites[$i].name" "$LAB")
    index=$(yq -r ".sites[$i].serverIDIndex" "$LAB")
    endpoint=$(yq -r ".sites[$i].endpoint // \"\"" "$LAB")
    printf '    - name: %s\n' "$name"
    printf '      serverIDIndex: %s\n' "$index"
    # The endpoint is read by the bootstrap tooling and `slctl mesh verify`,
    # never by the operator — it dials the address inside the kubeconfig
    # Secret. Emitted so the two can be compared.
    if [[ -n "$endpoint" ]]; then
        printf '      endpoint: %s\n' "$endpoint"
    fi
done
if [[ -n "$mode" ]]; then
    printf '  network:\n'
    printf '    mode: %s\n' "$mode"
    if [[ -n "$multus" ]]; then
        printf '    multusNetwork: %s\n' "$multus"
    fi
fi
