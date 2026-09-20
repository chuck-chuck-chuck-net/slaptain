#!/bin/bash
# Unified e2e test orchestration. Single entry point for single-site and
# multi-site test runs — the script branches on the number of contexts:
#
#   N=1  single-site:  one cluster, a one-site mesh (index 0 → decade 0), no
#                      external peers, basic LDAP/ACL/schema coverage.
#                      E2E_EXTERNAL_REPL is NOT set.
#   N≥2  multi-site:   N clusters, a full cross-site peer set, cross-site CSN
#                      convergence, includes E2E_EXTERNAL_REPL=1. Needs a
#                      discovery transport (POD_ROUTED or MULTUS_NETWORK).
#
# Every site is deployed identically, from charts/slapd-mesh with ONE generated
# values file (ADR-028). The single per-site input in the whole system is the
# operator's own siteName, passed to charts/operator in setup_foundation; the
# operator derives serverIDBase, external peers, the network mode and the trust
# wiring from the SlapdMesh plus that identity. N=1 falls out of the same code
# path as a one-site mesh with nothing to peer with.
#
# Usage:
#   ./tests/e2e.sh setup    [ctx1 [ctx2 ...]]
#   ./tests/e2e.sh test     [ctx1 [ctx2 ...]]
#   ./tests/e2e.sh teardown [ctx1 [ctx2 ...]]
#   ./tests/e2e.sh all      [ctx1 [ctx2 ...]]
#
# With no context, the current kubectl context is used (single-site).
#
# Prerequisites: kubectl contexts that reach each cluster. Container images
# must be available in a registry reachable from every cluster.
set -euo pipefail

# ── Lab config file (optional) ───────────────────────────────────────────────
# A YAML file describing the lab: site inventory (contexts, API endpoints,
# node-access IPs, VM inventory) plus per-project settings under a `slaptain:`
# key. Unknown keys are ignored — see lab.yaml.sample in the repo root.
#
# Resolution order: $E2E_CONFIG if set, else <repo-root>/lab.yaml if present,
# else no file (everything keeps its env/default behavior). Environment
# variables always win over file values: the file supplies lab facts (sites,
# registry, network mode), never per-run knobs (GIT_TAG, E2E_* gates, seeds).
#
# The file typically contains internal addresses — lab.yaml is gitignored;
# never commit a real one.
_E2E_SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
E2E_CONFIG="${E2E_CONFIG:-}"
if [[ -z "$E2E_CONFIG" && -f "$_E2E_SELF_DIR/../lab.yaml" ]]; then
    E2E_CONFIG="$_E2E_SELF_DIR/../lab.yaml"
fi

LAB_CONTEXTS=()
LAB_SITE_NAMES=()
LAB_SITE_INDICES=()
LAB_SITE_ENDPOINTS=()
if [[ -n "$E2E_CONFIG" ]]; then
    if [[ ! -f "$E2E_CONFIG" ]]; then
        echo "ERROR: E2E_CONFIG=$E2E_CONFIG: no such file" >&2; exit 1
    fi
    if ! command -v yq >/dev/null 2>&1; then
        echo "ERROR: reading $E2E_CONFIG requires yq v4 (https://github.com/mikefarah/yq)" >&2; exit 1
    fi

    # Scalar lookup: empty string when the key is absent.
    _lab_get() { yq -r "$1 // \"\"" "$E2E_CONFIG"; }

    _v="$(_lab_get '.slaptain.registry')";          [[ -n "$_v" && -z "${REGISTRY:-}" ]] && REGISTRY="$_v"
    _v="$(_lab_get '.slaptain.operatorNamespace')"; [[ -n "$_v" && -z "${NAMESPACE:-}" ]] && NAMESPACE="$_v"
    _v="$(_lab_get '.slaptain.testingNamespace')";  [[ -n "$_v" && -z "${NAMESPACE_TESTING:-}" ]] && NAMESPACE_TESTING="$_v"
    _v="$(_lab_get '.slaptain.testResources')";     [[ -n "$_v" && -z "${TEST_RESOURCES:-}" ]] && TEST_RESOURCES="$_v"

    # Replication network mode — only when neither knob is already set, so the
    # POD_ROUTED / MULTUS_NETWORK mutual-exclusion check below still guards
    # explicit env combinations.
    if [[ -z "${POD_ROUTED:-}" && -z "${MULTUS_NETWORK:-}" ]]; then
        case "$(_lab_get '.slaptain.replicationNetwork.mode')" in
            pod-routed) POD_ROUTED=1 ;;
            multus)     MULTUS_NETWORK="$(_lab_get '.slaptain.replicationNetwork.multusNetwork')" ;;
            "")         : ;;
            *)          echo "ERROR: $E2E_CONFIG: unknown slaptain.replicationNetwork.mode" >&2; exit 1 ;;
        esac
    fi

    # Per-site node-access IPs → E2E_NODE_ACCESS_IPS ("ctx=ip ..."). Sites
    # without nodeAccessIP keep the InternalIP default (single-homed nodes).
    if [[ -z "${E2E_NODE_ACCESS_IPS:-}" ]]; then
        E2E_NODE_ACCESS_IPS="$(yq -r '[.sites[] | select(.nodeAccessIP) | (.context // .name) + "=" + .nodeAccessIP] | join(" ")' "$E2E_CONFIG")"
        export E2E_NODE_ACCESS_IPS
    fi

    # Site contexts, in file order — the default context list when the command
    # line names none. `context` is OPTIONAL and defaults to the site name,
    # because a lab whose kube contexts are already named after its sites
    # should not have to say so twice.
    mapfile -t LAB_CONTEXTS < <(yq -r '.sites[] | (.context // .name) // ""' "$E2E_CONFIG" | grep -v '^$' || true)

    # The site IDENTITY and its serverID slot, keyed by context. Both come from
    # the file; neither is positional. See assign_site_names for why.
    mapfile -t LAB_SITE_NAMES   < <(yq -r '.sites[].name // ""' "$E2E_CONFIG")
    mapfile -t LAB_SITE_INDICES < <(yq -r '.sites[] | (.serverIDIndex // -1) | tostring' "$E2E_CONFIG")
    mapfile -t LAB_SITE_ENDPOINTS < <(yq -r '.sites[] | (.endpoint // "")' "$E2E_CONFIG")
fi

# ── Configuration ────────────────────────────────────────────────────────────

NAMESPACE="${NAMESPACE:-slaptain-system}"

NAMESPACE_TESTING="${NAMESPACE_TESTING:-slaptain-testing}"
NODEPORT_LDAP="${NODEPORT_LDAP:-30389}"
NODEPORT_LDAPS="${NODEPORT_LDAPS:-30636}"
NODEPORT_POD_BASE="${NODEPORT_POD_BASE:-30400}"
NODEPORT_RO_POD_BASE="${NODEPORT_RO_POD_BASE:-30410}"
VALUES_FILE="${VALUES_FILE:-$( cd "$(dirname "$0")" && pwd )/values.slapd-persistent.yaml}"

REGISTRY="${REGISTRY:-ghcr.io/chuck-chuck-chuck-net}"
PROJECT="${PROJECT:-slaptain}"
TEST_RESOURCES="${TEST_RESOURCES:-example}"

# Multus replication network (ADR-007). When MULTUS_NETWORK is set, cross-site
# replication rides a dedicated Multus network instead of the primary pod
# network. NodePort services are still created, for test-runner connectivity.
#
# Peers are always discovered dynamically: the operator queries the remote
# cluster's k8s API over the replication network and reads each pod's net1 IP,
# and this script provisions the cross-site RBAC and kubeconfig Secrets with
# scripts/mesh-authorize-peers.sh.
#
# The legacy STATIC_PODADDRESSES mode — discover the IPs here and patch them
# into externalPeers — went with the hand-wired path (MESH-PLAN Phase 7). A
# SlapdMesh has no field that produces a static address list, deliberately, so
# there was nothing left for the mode to configure.
#
# MULTUS_NETWORK: NAD reference, e.g. "infra/replication-net" or "replication-net"
MULTUS_NETWORK="${MULTUS_NETWORK:-}"

# Pod-routed cross-cluster replication (ADR-016). When POD_ROUTED=1, cross-site
# peers are addressed by their primary pod IP (no Multus, no NAD, no operator
# secondary NIC) — for clusters whose pod network is natively routed across sites.
# Uses the same remote-kubeconfig discovery as Multus dynamic mode; the operator's
# network.mode=pod-routed makes discovery read pod.status.podIP. Mutually exclusive
# with MULTUS_NETWORK.
POD_ROUTED="${POD_ROUTED:-}"
if [[ -n "$POD_ROUTED" && -n "$MULTUS_NETWORK" ]]; then
    echo "ERROR: POD_ROUTED and MULTUS_NETWORK are mutually exclusive" >&2
    exit 1
fi

# ── The deployment path (ADR-028, MESH-PLAN Phase 7) ─────────────────────────
#
# There is exactly one. Every run deploys the whole bundle — SlapdMesh,
# SlapdCluster, SlapdDatabases, SlapdSchemas — from charts/slapd-mesh with ONE
# values file applied unchanged at every site, and the operator derives
# serverIDBase, externalPeers, the network mode and the trust wiring from the
# mesh plus its own siteName.
#
# The hand-wired path this replaced — charts/slapd with a per-site
# serverIDBase and an N-1 peer list computed here in shell — is gone, and so is
# the E2E_MESH flag that used to select between the two. A flag permanently set
# to one value is debt with a nicer name, and keeping the loop alive would have
# kept a second, untested way of standing a mesh up inside the very repository
# whose tooling is supposed to be the way. Phase 6 ran both to a full-gate
# three-site comparison before this deletion; docs/MESH-PLAN.md has the numbers.

# discovery_mode: cross-site peers are reached through the remote k8s API
# (a kubeconfig Secret) rather than a static URI. A SlapdMesh describes SITES
# and reaches them through their API servers, so this is the only cross-site
# transport the mesh can express — hence the MULTISITE guard further down.
discovery_mode() {
    [[ -n "$POD_ROUTED" || -n "$MULTUS_NETWORK" ]]
}

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Image tag: git tag or short commit hash (with -dirty suffix for uncommitted changes).
# Tag derivation lives in scripts/image-tag.sh — one rule for the Makefile and
# for us, dirty trees get a content-hashed suffix. See the script.
if [[ -z "${GIT_TAG:-}" ]]; then
    GIT_TAG="$("$PROJECT_ROOT/scripts/image-tag.sh")"
fi

# IMAGE_TAG is how that build is ADDRESSED (docs/VERSIONING.md): a version tag
# loses its `v` (v0.2.1 -> 0.2.1), anything else gains a sha- prefix
# (09ecf10 -> sha-09ecf10). Mirrors the Makefile exactly — pinning
# GIT_TAG=v0.2.1 here must reach the same images `make push` produced, and so
# must an untagged build of the same tree.
if [[ "$GIT_TAG" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    IMAGE_TAG="${GIT_TAG#v}"
else
    IMAGE_TAG="sha-${GIT_TAG}"
fi

# SLAPD_TAG_SUFFIX: appended to the slapd/slapd-init image tags only (the
# operator tag is untouched). Empty = OpenLDAP 2.7.1; "-ol26" runs the suite
# against the legacy OpenLDAP 2.6 pair, which is expected to FAIL the ITS#9580
# assertion in dataloss_recovery_test.go — see ADR-021.
SLAPD_TAG_SUFFIX="${SLAPD_TAG_SUFFIX:-}"
SLAPD_TAG="${IMAGE_TAG}${SLAPD_TAG_SUFFIX}"

# ── Image pull secret ────────────────────────────────────────────────────────
PULL_SECRET_FILE="$SCRIPT_DIR/image-pull-secret.yaml"
PULL_SECRET_HELM_ARGS=()
if [[ -f "$PULL_SECRET_FILE" ]]; then
    PULL_SECRET_NAME=$(awk '/^  name:/{print $2; exit}' "$PULL_SECRET_FILE")
    PULL_SECRET_HELM_ARGS=(--set "imagePullSecrets[0].name=$PULL_SECRET_NAME")
fi

# ── Helpers ──────────────────────────────────────────────────────────────────

log() { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33mWARN:\033[0m %s\n" "$*" >&2; }
die() { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

# ── Fixture ──────────────────────────────────────────────────────────────────
#
# Single fixture: persistent (PVC-backed). Per ADR-013, persistence is
# mandatory in v1alpha1 (the `persistence.enabled` field is gone). The
# historical dual-fixture orchestration (persistent + ephemeral) was retired
# along with the field.

kctl() {
    local ctx="$1"; shift
    kubectl --context "$ctx" "$@"
}

hctl() {
    local ctx="$1"; shift
    helm --kube-context "$ctx" "$@"
}

usage() {
    cat >&2 <<EOF
Usage: $0 <setup|test|teardown|all> [ctx1 [ctx2 ...]]

No context = single-site mode on the current kubectl context; one context =
single-site mode on that context; two or more = multi-site mode with full
external-peer mesh. The script auto-detects N from the argument count.

Subcommands:
  setup     Deploy operator, SlapdCluster, test resources on all clusters
  test      Run e2e tests (external replication tests gated on N>=2)
  teardown  Remove everything created by setup
  config    Print the resolved lab configuration (contexts, registry, IPs) and exit
  all       setup + test + teardown

Environment variables (with defaults):
  NAMESPACE            = $NAMESPACE
  NAMESPACE_TESTING    = $NAMESPACE_TESTING
  NODEPORT_LDAP        = $NODEPORT_LDAP
  NODEPORT_LDAPS       = $NODEPORT_LDAPS
  REGISTRY             = $REGISTRY
  PROJECT              = $PROJECT
  TEST_RESOURCES       = $TEST_RESOURCES  (example or lab)

Node access (NodePort reachability + cert SAN; defaults to the k8s InternalIP):
  E2E_NODE_ACCESS_IP   = single-site override, e.g. <reachable-node-ip>
  E2E_NODE_ACCESS_IPS  = multi-site map, e.g. "<ctx1>=<ip1> <ctx2>=<ip2>"
                         Use when the InternalIP isn't reachable from the runner
                         (dual-homed nodes; the reachable NIC isn't k8s-registered).

Reproducibility and triage:
  E2E_SEED             = Pin Ginkgo's spec-order seed (default: a fresh timestamp,
                         always logged). The suite shares one mutable slapd
                         cluster, so cross-spec interference depends on spec
                         order — replaying the logged seed reproduces it exactly.
  E2E_SCALE            = Set to 1 to run the many-entries fixture class (ADR-024):
                         a generated seed of >1000 entries plus a journal-heavy
                         churn, the only shape in which the breaks-at-scale
                         tunables (replication sizelimit cap, map size, search
                         limits) are observable. Volume knobs: E2E_SCALE_ENTRIES
                         (default 1200), E2E_SCALE_CHURN (default 700).
  E2E_LABEL_FILTER     = Ginkgo label expression selecting which specs run, e.g.
                         "restore-inplace" or "backup && !restore". Lets a single
                         scenario be iterated without paying for the whole suite,
                         and without FAIL_FAST aborting on an unrelated known
                         failure first. Labels are the [bracketed] tags shown
                         after each spec name in the output.
  FAIL_FAST            = Set to 1 to stop at the first failing spec instead of
                         letting the cascade bury its cause. Nothing is torn down
                         on failure ('test' never tears down; 'all' aborts before
                         teardown), so the cluster is left ready for
                         slctl inspect / slctl debug-dump.
  E2E_TEARDOWN_TIMEOUT = Seconds any single blocking teardown step may take
                         (default 180). Teardown never waits forever: a step that
                         overruns is reported with the manual commands to finish
                         it, the remaining contexts are still torn down, and the
                         script exits non-zero.
  E2E_DATAPRESENT_TIMEOUT
                       = Seconds the post-suite DataPresent gate waits on a reason
                         that is never legitimately transient (default 60):
                         GlueSuffix, DataMissingOnPods, DataMissing, NoDataYet,
                         NoReachablePod, an absent condition, anything unknown.
  E2E_DATAPRESENT_RO_TIMEOUT
                       = Seconds the same gate waits on
                         False/DataMissingOnReadOnlyPods (default 600), which
                         ADR-025 calls legitimately transient during a read-only
                         initial sync. The default is a GUESS — RO initial-sync
                         duration is unmeasured on a large DIT (docs/BACKLOG.md,
                         "the big-DIT initial-sync e2e"). It still ends in a
                         failure; it is a longer fuse, not an exemption.

Deployment path (ADR-028 — there is only one; no flag selects it):
  Every run installs charts/slapd-mesh with ONE values file, applied unchanged
  at every site. The operator derives serverIDBase, externalPeers, the network
  mode and the trust wiring from the SlapdMesh plus its own siteName, which
  charts/operator carries as the single per-site fact in the system.

Cross-site replication transport (multi-site only; one is REQUIRED):
  POD_ROUTED           = Set to 1 for pod-routed: peers addressed by primary pod IP
                         via remote-kubeconfig discovery (ADR-016). No Multus/NAD.
                         Requires pod CIDRs routed between sites.
  MULTUS_NETWORK       = NAD reference (e.g. "infra/replication-net"); cross-site
                         over a dedicated Multus network (ADR-007). Mutually
                         exclusive with POD_ROUTED.
  A mesh reaches a site through its API server, so it derives only DISCOVERY
  peers — never a static NodePort URI and never a static address list. A
  multi-site run without one of the two above is refused up front rather than
  producing a cluster with no peers.
EOF
    exit 1
}

# ── Dynamic discovery helpers (ADR-007 amendment) ───────────────────────────

# Set up cross-site kubeconfig Secrets for operator-driven dynamic peer discovery.
# Uses scripts/mesh-authorize-peers.sh to create RBAC + kubeconfig Secrets.
setup_remote_kubeconfigs() {
    [[ "$MULTISITE" -eq 0 ]] && return
    discovery_mode || return
    log "Authorizing peer discovery (RBAC + kubeconfig Secrets)..."
    # The script reads the derived lab file: each site's endpoint (declared, or
    # the InternalIP fallback build_script_lab_file computed) and its name,
    # which it uses for the Secret — "<site>-kubeconfig", exactly what
    # MeshSite.KubeconfigSecretFor() derives. The rename pass that used to
    # follow this call is gone with it.
    "$PROJECT_ROOT/scripts/mesh-authorize-peers.sh" \
        -n "$NAMESPACE_TESTING" --from-lab "$SCRIPT_LAB_FILE"
}

# ── Site identity (ADR-028) ──────────────────────────────────────────────────

# Every operator needs to know which site of the mesh it runs at: it is the one
# per-site fact in the system (ADR-028 §4), and `spec.seed.site` is decided by
# comparing the declared founder against it.
#
# The name is LOGICAL — it is not the kube context name, though it may equal
# one. The fixtures in tests/resources/ are committed to a public repository and
# must name their founder (`seed.site`), so the name has to be neutral (CLAUDE.md
# "References") and stable across labs; a lab's context names are neither.
#
# WHERE IT COMES FROM. lab.yaml's `sites[].name` is authoritative, and
# `serverIDIndex` with it. Without a lab file — contexts named on the command
# line — the historical positional rule applies: the Nth context is `site-N` at
# index N-1. Both paths yield site-1..site-N for the standard lab, which is what
# keeps the committed `seed.site: site-1` working.
#
# Neither is positional WITHIN the file, deliberately. A site name reaches into
# tls_cacert paths, the <site>-ca and <site>-kubeconfig Secret names, seed.site
# and the operator's SITE_NAME; the serverID index is baked into every CSN the
# site's pods have written. Deriving either from list order would make
# reordering YAML a silent rename or a silent renumber (ADR-017, ADR-028).
assign_site_names() {
    local idx=1 ctx
    for ctx in "${CONTEXTS[@]}"; do
        local name="" index=""
        local i
        for i in "${!LAB_CONTEXTS[@]}"; do
            if [[ "${LAB_CONTEXTS[$i]}" == "$ctx" ]]; then
                name="${LAB_SITE_NAMES[$i]:-}"
                index="${LAB_SITE_INDICES[$i]:-}"
                break
            fi
        done
        [[ -z "$name" ]] && name="site-${idx}"
        if [[ -z "$index" || "$index" == "-1" ]]; then
            if [[ -n "${E2E_CONFIG:-}" && -n "${LAB_SITE_NAMES[*]:-}" ]] && \
               printf '%s\n' "${LAB_CONTEXTS[@]}" | grep -qxF "$ctx"; then
                die "$E2E_CONFIG: site '$name' has no serverIDIndex. It is required and never inferred from list position — the index is the site's serverID decade, baked into every CSN its pods have written (ADR-017, ADR-028)."
            fi
            index=$((idx - 1))
        fi
        SITE_NAMES[$ctx]="$name"
        SITE_INDICES[$ctx]="$index"
        ((idx++)) || true
    done
}

# ── Peer Secret naming ───────────────────────────────────────────────────────
#
# A cross-site peer is referenced by two Secrets whose names must agree between
# the thing that CREATES them (this script) and the thing that READS them (the
# operator):
#
#   peer_ca_secret          the Secret holding that site's CA under ca.crt.
#   peer_kubeconfig_secret  the Secret holding a kubeconfig for that site's API,
#                           which ADR-007 dynamic discovery binds with.
#
# The operator derives both from the MESH SITE name — MeshSite.CASecretNameFor()
# and KubeconfigSecretFor() default to "<site>-ca" and "<site>-kubeconfig"
# (ADR-028 §4, "peer names are mesh site names") — so these two functions exist
# to say that convention once, in the harness, rather than in five call sites.
#
# There is no peer_name function any more: the peer's NAME is the site's name,
# the operator picks it, and nothing here gets to have an opinion. The pre-Phase-7
# harness named peers after kube CONTEXTS and had to keep both schemes straight;
# that whole axis is gone with the hand-wired path.
peer_ca_secret() { # ctx
    echo "${SITE_NAMES[$1]}-ca"
}

peer_kubeconfig_secret() { # ctx
    echo "${SITE_NAMES[$1]}-kubeconfig"
}

# ── Discovery ────────────────────────────────────────────────────────────────

discover_node_ips() {
    log "Discovering node IPs..."
    for ctx in "${CONTEXTS[@]}"; do
        local ip
        ip=$(kctl "$ctx" get nodes \
            -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
        [[ -z "$ip" ]] && die "Could not discover node IP for context $ctx"
        NODE_IPS[$ctx]="$ip"

        # NODE_ACCESS_IPS[ctx] is the address used to reach the node's NodePorts
        # (from the test runner) and to SAN the TLS cert. It defaults to the k8s
        # InternalIP but can differ: on dual-homed clusters the InternalIP may sit
        # on a network with no north-south access to the runner (e.g. a routed
        # replication network chosen as the primary node network); the reachable
        # address is then a secondary NIC that k8s does not register, so it can't
        # be auto-discovered. Supply it explicitly:
        #   single-site:  E2E_NODE_ACCESS_IP=<reachable-node-ip>
        #   multi-site:   E2E_NODE_ACCESS_IPS="<ctx1>=<ip1> <ctx2>=<ip2>"
        # An override is used verbatim (not validated against the node object).
        # NODE_IPS keeps the InternalIP because the cross-site API-server
        # addresses built from it (setup_remote_kubeconfigs) must ride the
        # cross-site-routed replication network, not the site-local internal one.
        local access_ip="$ip"
        if [[ -n "${E2E_NODE_ACCESS_IPS:-}" ]]; then
            local pair
            for pair in $E2E_NODE_ACCESS_IPS; do
                if [[ "$pair" == "$ctx="* ]]; then
                    access_ip="${pair#*=}"
                fi
            done
        fi
        if [[ -n "${E2E_NODE_ACCESS_IP:-}" && "${#CONTEXTS[@]}" -eq 1 ]]; then
            access_ip="$E2E_NODE_ACCESS_IP"
        fi
        NODE_ACCESS_IPS[$ctx]="$access_ip"

        if [[ "$access_ip" == "$ip" ]]; then
            log "  $ctx → $ip"
        else
            log "  $ctx → $ip (NodePort access via $access_ip)"
        fi
    done
}

# ── CR name resolution from test resources ───────────────────────────────────

resolve_cr_names() {
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"
    [[ -d "$resource_dir" ]] || die "Test resources not found: $resource_dir"

    DB_CR_NAME=$(awk '/^kind: SlapdDatabase/{found=1} found && /^  name:/{print $2; exit}' \
        "$resource_dir/database.yaml")
    SCHEMA_CR_NAME=$(awk '/^kind: SlapdSchema/{found=1} found && /^  name:/{print $2; exit}' \
        "$resource_dir/schema.yaml")
    DB_SUFFIX=$(awk '/^  suffix:/{gsub(/"/, "", $2); print $2; exit}' \
        "$resource_dir/database.yaml")

    [[ -z "$DB_CR_NAME" ]] && die "Could not extract SlapdDatabase name from $resource_dir/database.yaml"
    [[ -z "$SCHEMA_CR_NAME" ]] && die "Could not extract SlapdSchema name from $resource_dir/schema.yaml"
    [[ -z "$DB_SUFFIX" ]] && die "Could not extract suffix from $resource_dir/database.yaml"

    DB_CREDENTIALS_SECRET="${DB_CR_NAME}-credentials"
    log "Database CR: $DB_CR_NAME (suffix: $DB_SUFFIX), Schema CR: $SCHEMA_CR_NAME"
    log "Credentials secret: $DB_CREDENTIALS_SECRET"

    # Optional second SlapdDatabase (database2.yaml). The `example` fixture
    # declares one so the standard suite covers the two-replicated-database
    # shape ADR-019 is about; other resource sets need not. Empty when absent,
    # and every consumer below is guarded on that.
    DB2_CR_NAME=""
    DB2_SUFFIX=""
    DB2_CREDENTIALS_SECRET=""
    if [[ -f "$resource_dir/database2.yaml" ]]; then
        DB2_CR_NAME=$(awk '/^kind: SlapdDatabase/{found=1} found && /^  name:/{print $2; exit}' \
            "$resource_dir/database2.yaml")
        DB2_SUFFIX=$(awk '/^  suffix:/{gsub(/"/, "", $2); print $2; exit}' \
            "$resource_dir/database2.yaml")
        [[ -z "$DB2_CR_NAME" ]] && die "Could not extract SlapdDatabase name from $resource_dir/database2.yaml"
        [[ -z "$DB2_SUFFIX" ]] && die "Could not extract suffix from $resource_dir/database2.yaml"
        DB2_CREDENTIALS_SECRET="${DB2_CR_NAME}-credentials"
        log "Second database CR: $DB2_CR_NAME (suffix: $DB2_SUFFIX), credentials secret: $DB2_CREDENTIALS_SECRET"
    fi
}

# ── The lab file the bootstrap scripts read ───────────────────────────────────
#
# scripts/mesh-*.sh all take their sites from a lab file, which is the whole
# point: one format, one place, and the tool a user runs is the tool this suite
# runs. But the suite knows two things the user's file does not necessarily
# state — the node InternalIPs it discovered, and the endpoint derivation for a
# run whose contexts came from the command line — so it hands the scripts a
# DERIVED file rather than the user's own.
#
# Derived, never edited in place: the user's lab.yaml is an input to this suite
# and the suite has no business writing to it.
#
# certIPs carries the node InternalIP as an extra SAN. The pre-delegation code
# SANed it alongside the access IP, and dropping it here would have quietly
# changed what every fixture certificate covers — the kind of difference that
# only shows up when two paths are forced together, which is the reason for
# forcing them together.
SCRIPT_LAB_FILE=""
build_script_lab_file() {
    SCRIPT_LAB_FILE=$(mktemp /tmp/e2e-lab.XXXXXX.yaml)
    {
        echo "# Generated by tests/e2e.sh for scripts/mesh-*.sh — not the user's lab.yaml."
        echo "sites:"
        local ctx
        for ctx in "${CONTEXTS[@]}"; do
            local endpoint=""
            if [[ -n "${E2E_CONFIG:-}" ]]; then
                endpoint=$(yq -r "(.sites[] | select((.context // .name) == \"$ctx\") | .endpoint) // \"\"" "$E2E_CONFIG")
            fi
            if [[ -z "$endpoint" ]]; then
                # No declared endpoint (command-line contexts, or a lab file
                # that omits it): fall back to the node's InternalIP with the
                # port from its kubeconfig entry. Pins peer discovery to one
                # node, which is exactly why sites[].endpoint exists — so this
                # is the fallback, not the rule.
                local api_server api_port="6443"
                api_server=$(kctl "$ctx" config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)
                [[ "$api_server" =~ :([0-9]+)$ ]] && api_port="${BASH_REMATCH[1]}"
                endpoint="https://${NODE_IPS[$ctx]}:${api_port}"
            fi
            echo "  - name: ${SITE_NAMES[$ctx]}"
            echo "    serverIDIndex: ${SITE_INDICES[$ctx]}"
            echo "    context: ${ctx}"
            echo "    endpoint: ${endpoint}"
            echo "    nodeAccessIP: ${NODE_ACCESS_IPS[$ctx]}"
            if [[ "${NODE_ACCESS_IPS[$ctx]}" != "${NODE_IPS[$ctx]}" ]]; then
                echo "    certIPs: [${NODE_IPS[$ctx]}]"
            fi
        done
    } > "$SCRIPT_LAB_FILE"
    log "Lab file for the bootstrap scripts: $SCRIPT_LAB_FILE"
}

# Credentials and TLS trust, both delegated to the scripts a user runs.
#
# They were inline loops here until the scripts existed, and keeping copies
# would have guaranteed drift: the suite's copy is exercised every cycle and the
# user's is exercised at demo time. Delegating makes the documented path the
# tested one.
setup_shared_credentials() {
    local dbs=(--database "$DB_CR_NAME")
    [[ -n "$DB2_CR_NAME" ]] && dbs+=(--database "$DB2_CR_NAME")
    "$PROJECT_ROOT/scripts/mesh-share-credentials.sh" \
        --from-lab "$SCRIPT_LAB_FILE" -n "$NAMESPACE_TESTING" "${dbs[@]}"
}

setup_tls_trust() {
    "$PROJECT_ROOT/scripts/mesh-establish-trust.sh" \
        --from-lab "$SCRIPT_LAB_FILE" -n "$NAMESPACE_TESTING" --cluster slapd
}

setup_foundation() {
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Creating namespaces..."
        kctl "$ctx" create namespace "$NAMESPACE" --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -
        kctl "$ctx" create namespace "$NAMESPACE_TESTING" --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -

        if [[ -f "$PULL_SECRET_FILE" ]]; then
            log "[$ctx] Applying image pull secret to $NAMESPACE and $NAMESPACE_TESTING..."
            kctl "$ctx" apply -n "$NAMESPACE" -f "$PULL_SECRET_FILE"
            kctl "$ctx" apply -n "$NAMESPACE_TESTING" -f "$PULL_SECRET_FILE"
        fi

        log "[$ctx] Installing operator (tag: $IMAGE_TAG, site: ${SITE_NAMES[$ctx]})..."
        local operator_multus_sets=()
        if [[ -n "$MULTUS_NETWORK" ]]; then
            operator_multus_sets=(--set "multus.network=$MULTUS_NETWORK")
        fi
        hctl "$ctx" upgrade --install slaptain "$PROJECT_ROOT/charts/operator" \
            --namespace "$NAMESPACE" --create-namespace \
            --set "image.repository=$REGISTRY/$PROJECT/operator" \
            --set "image.tag=$IMAGE_TAG" \
            --set "siteName=${SITE_NAMES[$ctx]}" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${operator_multus_sets[@]}"
    done
}

# ── The bundle values (ADR-028) ──────────────────────────────────────────────
#
# One values file, built ONCE, applied unchanged at every site. That is not a
# tidiness choice: "the same bytes everywhere" is the property ADR-028 §3 rests
# the whole parity design on, and a harness that templated a file per site would
# be asserting the property by construction while quietly not having it. Building
# it once and reusing the path makes the claim checkable — `sha256sum` it, and
# every `helm upgrade` below names the same file.
MESH_VALUES_FILE=""

# The chart values for one run of the fixture. Three parts:
#
#   mesh     — sites (name + serverIDIndex) and the fabric. serverIDIndex is
#              (position - 1) so site-1/2/3 → decades 0/100/200, which is EXACTLY
#              what the pre-ADR-028 harness produced with site_idx*100.
#              Reproducing it is mandatory, not tidy: olcServerID is baked into
#              every CSN a pod has written, so a derivation that renumbers a live
#              site splits its history across two sids (MESH-PLAN hazard 2,
#              ADR-017).
#   cluster  — tests/values.slapd-persistent.yaml, lifted under `cluster:` plus
#              the images. Read from that file rather than retyped here so the
#              fixture has one definition, shared with every other consumer of
#              it (charts/slapd still takes it directly).
#   databases / schemas
#            — derived from tests/resources/<set>/*.yaml, transformed into the
#              chart's {name, spec} passthrough shape. Those files stay the
#              single source of truth for the fixture CRs. clusterRef is dropped:
#              the chart pins it (slapd-mesh.childSpec), and leaving it in would
#              be a second name to keep in sync.
build_mesh_values() {
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"
    local crs=()
    local f
    for f in "$resource_dir"/*.yaml; do
        [[ -e "$f" ]] && crs+=("$f")
    done
    [[ ${#crs[@]} -eq 0 ]] && die "No test resources found in $resource_dir"

    MESH_VALUES_FILE=$(mktemp /tmp/e2e-mesh-values.XXXXXX.yaml)

    {
        echo "# Generated by tests/e2e.sh — ONE file, applied unchanged at every site."
        # The mesh block comes from scripts/mesh-derive-topology.sh whenever a lab file
        # describes the sites, so the tool the docs tell users to run is the one
        # the suite exercises on every cycle. A second implementation here would
        # be free to drift, and would drift.
        if [[ -n "${E2E_CONFIG:-}" ]]; then
            "$PROJECT_ROOT/scripts/mesh-derive-topology.sh" -f "$E2E_CONFIG" | grep -v '^#'
        else
        echo "mesh:"
        echo "  name: slapd-mesh"
        echo "  sites:"
        local ctx
        for ctx in "${CONTEXTS[@]}"; do
            echo "    - name: ${SITE_NAMES[$ctx]}"
            echo "      serverIDIndex: ${SITE_INDICES[$ctx]}"
        done
        if [[ -n "$POD_ROUTED" ]]; then
            echo "  network:"
            echo "    mode: pod-routed"
        elif [[ -n "$MULTUS_NETWORK" ]]; then
            echo "  network:"
            echo "    mode: multus"
            echo "    multusNetwork: \"$MULTUS_NETWORK\""
        fi
        fi   # end: generator vs command-line-contexts fallback

        local pull_secrets="[]"
        [[ -n "${PULL_SECRET_NAME:-}" ]] && pull_secrets="[{\"name\": \"$PULL_SECRET_NAME\"}]"
        MESH_SLAPD_REPO="$REGISTRY/$PROJECT/slapd" \
        MESH_SLAPD_TAG="$SLAPD_TAG" \
        MESH_INIT_REPO="$REGISTRY/$PROJECT/slapd-init" \
        MESH_INIT_TAG="$SLAPD_TAG" \
        MESH_PULL_SECRETS="$pull_secrets" \
        yq -N '{"cluster": (. * {
                    "name": "slapd",
                    "images": {
                        "slapd": {"repository": strenv(MESH_SLAPD_REPO), "tag": strenv(MESH_SLAPD_TAG)},
                        "init":  {"repository": strenv(MESH_INIT_REPO),  "tag": strenv(MESH_INIT_TAG)}
                    },
                    "imagePullSecrets": (strenv(MESH_PULL_SECRETS) | from_json)
                })}' "$VALUES_FILE"

        yq -N ea '[select(.kind == "SlapdDatabase")
                   | {"name": .metadata.name, "spec": (.spec | del(.clusterRef))}]
                  | {"databases": .}' "${crs[@]}"
        yq -N ea '[select(.kind == "SlapdSchema")
                   | {"name": .metadata.name, "spec": (.spec | del(.clusterRef))}]
                  | {"schemas": .}' "${crs[@]}"
    } > "$MESH_VALUES_FILE"

    log "Mesh values: $MESH_VALUES_FILE (sha256 $(sha256sum "$MESH_VALUES_FILE" | cut -c1-16)…)"
}

# Install the bundle. Same chart, same values file, same release name at every
# site; the only per-site input in the whole system is the operator's siteName,
# which setup_foundation already passed to charts/operator.
#
# What a pre-ADR-028 harness computed at this point — the serverIDBase decade
# and the N-1 external peers with their CA and kubeconfig Secret names — is
# absent on purpose. The operator derives all of it from the mesh plus its own
# identity, and a cluster that both references a mesh and hand-writes one of those fields
# is refused outright with MeshResolved=False (ADR-028 §4). So the absence here
# is the feature under test, not an omission.
setup_mesh_bundle() {
    build_mesh_values
    local ctx
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Installing the mesh bundle (site ${SITE_NAMES[$ctx]}, identical values)..."
        hctl "$ctx" upgrade --install slapd "$PROJECT_ROOT/charts/slapd-mesh" \
            --namespace "$NAMESPACE_TESTING" --create-namespace \
            -f "$MESH_VALUES_FILE"
    done
}

# The mesh's MeshResolved condition is the gate everything else depends on: a
# cluster that cannot resolve its mesh reconciles NOTHING (deliberately — an
# empty peer set would tear every cross-site stanza off every pod), so a failure
# here would otherwise surface much later as an unexplained missing-stanza
# symptom. Check it explicitly and say what is wrong.
wait_mesh_resolved() {
    local ctx attempts status reason
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Waiting for MeshResolved=True..."
        attempts=0
        while true; do
            status=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapdclusters.ldap.chuck-chuck-chuck.net slapd \
                -o jsonpath='{.status.conditions[?(@.type=="MeshResolved")].status}' 2>/dev/null || echo "")
            [[ "$status" == "True" ]] && break
            ((attempts++)) || true
            if [[ $attempts -ge 120 ]]; then
                reason=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapdclusters.ldap.chuck-chuck-chuck.net slapd \
                    -o jsonpath='{.status.conditions[?(@.type=="MeshResolved")].message}' 2>/dev/null || echo "")
                die "[$ctx] MeshResolved did not become True within 120s (status=${status:-<absent>}): ${reason:-<no message>}"
            fi
            sleep 1
        done
        log "[$ctx] MeshResolved=True (site ${SITE_NAMES[$ctx]})."
    done
}

setup_nodeport_services() {
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Creating NodePort service (ldap:$NODEPORT_LDAP, ldaps:$NODEPORT_LDAPS)..."
        kctl "$ctx" apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: slapd-external
  namespace: $NAMESPACE_TESTING
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: slapd
    app.kubernetes.io/instance: slapd
  ports:
    - name: ldap
      port: 389
      targetPort: 1024
      nodePort: $NODEPORT_LDAP
    - name: ldaps
      port: 636
      targetPort: 1025
      nodePort: $NODEPORT_LDAPS
EOF

        # Per-pod NodePort services for direct pod access.
        log "[$ctx] Creating per-pod NodePort services..."
        local replicas
        replicas=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get statefulset/slapd \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 3)
        for i in $(seq 0 $((replicas - 1))); do
            local np=$((NODEPORT_POD_BASE + i))
            kctl "$ctx" apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: slapd-pod-$i
  namespace: $NAMESPACE_TESTING
spec:
  type: NodePort
  selector:
    statefulset.kubernetes.io/pod-name: slapd-$i
  ports:
    - name: ldap
      port: 389
      targetPort: 1024
      nodePort: $np
EOF
        done

        # Per-pod NodePort for read-only replicas.
        local ro_replicas
        ro_replicas=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get statefulset/slapd-readonly \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)
        for i in $(seq 0 $((ro_replicas - 1))); do
            local np=$((NODEPORT_RO_POD_BASE + i))
            kctl "$ctx" apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: slapd-readonly-pod-$i
  namespace: $NAMESPACE_TESTING
spec:
  type: NodePort
  selector:
    statefulset.kubernetes.io/pod-name: slapd-readonly-$i
  ports:
    - name: ldap
      port: 389
      targetPort: 1024
      nodePort: $np
EOF
        done
    done
}

wait_for_clusters_ready() {
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Waiting for StatefulSet slapd to appear..."
        local attempts=0
        while ! kctl "$ctx" -n "$NAMESPACE_TESTING" get statefulset/slapd &>/dev/null; do
            ((attempts++)) || true
            if [[ $attempts -ge 60 ]]; then
                die "[$ctx] StatefulSet slapd did not appear within 60s"
            fi
            sleep 1
        done
        log "[$ctx] Waiting for StatefulSet slapd to be ready..."
        kctl "$ctx" -n "$NAMESPACE_TESTING" rollout status statefulset/slapd --timeout=300s

        log "[$ctx] Waiting for SlapdCluster to reach Running phase..."
        attempts=0
        while true; do
            local phase
            phase=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapdclusters.ldap.chuck-chuck-chuck.net slapd \
                -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [[ "$phase" == "Running" ]]; then
                break
            fi
            ((attempts++)) || true
            if [[ $attempts -ge 180 ]]; then
                die "[$ctx] SlapdCluster did not reach Running phase within 180s (current: $phase)"
            fi
            sleep 1
        done
        log "[$ctx] SlapdCluster is Running."
    done
}

apply_test_resources() {
    # The fixture's PLAIN manifests only — in practice the readpw password
    # Secret the ACL specs bind with.
    #
    # The SlapdDatabase and SlapdSchema objects are not applied here: they come
    # out of charts/slapd-mesh, in the same release as the SlapdCluster (ADR-028
    # §5, the packaging unit is a chart). The CR files under
    # tests/resources/<set>/ remain the single source of truth for them —
    # build_mesh_values reads THOSE files and transforms them into chart values,
    # so there is no second copy to drift.
    #
    # The SAME bytes go to EVERY site (ADR-028 §3). Founder-only seeding
    # (ADR-025) is still in force, but it is a property of the spec rather than
    # of this script: each fixture's `spec.seed.site` names the founder, every
    # operator compares that name against its own site identity (--set
    # siteName=… in setup_foundation), and the sites that do not match withhold
    # the seed and receive the DIT by replication. That replaced
    # `strip_seed_block`, which deleted the seed mapping from every context
    # after the first — a per-site EDIT of an object that must be identical
    # everywhere, and precisely the deployment procedure ADR-025 says must not
    # be relied upon. The operator's evidence belt (a suffix with a foreign
    # creator withholds the seed) is unchanged and still the belt.
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"
    local files=()
    local f
    for f in "$resource_dir"/*.yaml; do
        [[ -e "$f" ]] || continue
        # Skip anything the chart renders; keep plain manifests.
        if yq -N ea 'select(.kind == "SlapdDatabase" or .kind == "SlapdSchema") | .kind' "$f" \
             | grep -q .; then
            continue
        fi
        files+=(-f "$f")
    done
    [[ ${#files[@]} -eq 0 ]] && { log "No non-CR test resources to apply."; return 0; }

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Applying test resources from $resource_dir (site ${SITE_NAMES[$ctx]})..."
        kctl "$ctx" apply -n "$NAMESPACE_TESTING" "${files[@]}"
    done
}

wait_test_resources_ready() {
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Waiting for SlapdDatabase $DB_CR_NAME to reach Running..."
        local attempts=0
        while true; do
            local phase
            phase=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net "$DB_CR_NAME" \
                -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            [[ "$phase" == "Running" ]] && break
            ((attempts++)) || true
            [[ $attempts -ge 180 ]] && die "[$ctx] SlapdDatabase did not reach Running within 180s (current: $phase)"
            sleep 1
        done

        if [[ -n "$DB2_CR_NAME" ]]; then
            log "[$ctx] Waiting for SlapdDatabase $DB2_CR_NAME to reach Running..."
            attempts=0
            while true; do
                local phase2
                phase2=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net "$DB2_CR_NAME" \
                    -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
                [[ "$phase2" == "Running" ]] && break
                ((attempts++)) || true
                [[ $attempts -ge 180 ]] && die "[$ctx] SlapdDatabase $DB2_CR_NAME did not reach Running within 180s (current: $phase2)"
                sleep 1
            done
        fi

        log "[$ctx] Waiting for SlapdSchema $SCHEMA_CR_NAME to be applied..."
        attempts=0
        while true; do
            local applied
            applied=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapdschemas.ldap.chuck-chuck-chuck.net "$SCHEMA_CR_NAME" \
                -o jsonpath='{.status.applied}' 2>/dev/null || echo "")
            [[ "$applied" == "true" ]] && break
            ((attempts++)) || true
            [[ $attempts -ge 120 ]] && die "[$ctx] SlapdSchema was not applied within 120s"
            sleep 1
        done
        log "[$ctx] Test resources ready."
    done

    if [[ "$MULTISITE" -eq 1 ]]; then
        log "Waiting 30s for cross-cluster replication convergence..."
        sleep 30
    fi
}

# ── Test ─────────────────────────────────────────────────────────────────────

run_tests() {
    local ctx0="${CONTEXTS[0]}"
    local local_ip="${NODE_ACCESS_IPS[$ctx0]}"

    log "Reading admin password from $ctx0..."
    local admin_pw
    admin_pw=$(kctl "$ctx0" -n "$NAMESPACE_TESTING" get secret "$DB_CREDENTIALS_SECRET" \
        -o jsonpath='{.data.root-password}' | base64 -d)

    # Create a temp kubeconfig scoped to ctx0 so the Go test suite's k8s client
    # connects to the right cluster without mutating the user's kubeconfig.
    local tmp_kubeconfig
    tmp_kubeconfig=$(mktemp /tmp/e2e-kubeconfig.XXXXXX)
    kubectl config view --context="$ctx0" --minify --flatten > "$tmp_kubeconfig"
    trap "rm -f '$tmp_kubeconfig'" EXIT

    # Build the test-env block. Multi-site adds external-replication env vars
    # pointing at the second context. The Go suite gates external-replication
    # specs on E2E_EXTERNAL_REPL=1.
    # E2E_NODE_IP is the Go suite's contract (helpers_test.go, restore_inplace_test.go)
    # for the node address to dial per-pod NodePorts — the resolved access IP, not
    # the E2E_NODE_ACCESS_IP override the deployer may have set above.
    local test_env=(
        "KUBECONFIG=$tmp_kubeconfig"
        "NAMESPACE_TESTING=$NAMESPACE_TESTING"
        "LDAP_ADDR=${local_ip}:${NODEPORT_LDAP}"
        "E2E_NODE_IP=${local_ip}"
        "E2E_POD_NODEPORT_BASE=${NODEPORT_POD_BASE}"
        "E2E_RO_POD_NODEPORT_BASE=${NODEPORT_RO_POD_BASE}"
        "DB_CR_NAME=$DB_CR_NAME"
        "DB_CREDENTIALS_SECRET=$DB_CREDENTIALS_SECRET"
        "SCHEMA_CR_NAME=$SCHEMA_CR_NAME"
        "READPW_OU=${READPW_OU:-ServiceAccounts}"
    )

    # Second replicated database (ADR-019 fixture). Absent for resource sets
    # that declare only one; the accesslog specs skip themselves then.
    if [[ -n "$DB2_CR_NAME" ]]; then
        test_env+=(
            "DB2_CR_NAME=$DB2_CR_NAME"
            "DB2_SUFFIX=$DB2_SUFFIX"
            "DB2_CREDENTIALS_SECRET=$DB2_CREDENTIALS_SECRET"
        )
    fi

    # Backup e2e (ADR-014): deploy the versitygw S3 server and enable the gated
    # backup specs. Opt-in via E2E_BACKUP=1 in the environment.
    if [[ "${E2E_BACKUP:-}" == "1" ]]; then
        log "[$ctx0] E2E_BACKUP=1 — deploying versitygw S3 server"
        kubectl --context="$ctx0" -n "$NAMESPACE_TESTING" apply -f "$PROJECT_ROOT/tests/resources/versitygw.yaml"
        test_env+=("E2E_BACKUP=1")
    fi

    # Scale-up e2e: standalone → HA transition (replicas 1→2 + replication
    # flip) on a second, self-contained cluster. Needs no extra infrastructure.
    # Opt-in via E2E_SCALEUP=1 in the environment.
    if [[ "${E2E_SCALEUP:-}" == "1" ]]; then
        log "[$ctx0] E2E_SCALEUP=1 — enabling scale-up transition specs"
        test_env+=("E2E_SCALEUP=1")
    fi

    # Many-entries fixture class (ADR-024): a generated seed of >1000 entries
    # plus a journal-heavy churn, which is the only shape in which the
    # breaks-at-scale tunables are observable at all — a single-digit-entry
    # LDIF cannot see a 500-entry cap. Writes thousands of entries into the
    # shared fixture database (under its own OU, removed afterwards) and takes
    # minutes, so it is opt-in. E2E_SCALE_ENTRIES / E2E_SCALE_CHURN tune the
    # volume without a repository diff.
    if [[ "${E2E_SCALE:-}" == "1" ]]; then
        log "[$ctx0] E2E_SCALE=1 — enabling the many-entries scale specs"
        test_env+=("E2E_SCALE=1")
        if [[ -n "${E2E_SCALE_ENTRIES:-}" ]]; then
            test_env+=("E2E_SCALE_ENTRIES=$E2E_SCALE_ENTRIES")
        fi
        if [[ -n "${E2E_SCALE_CHURN:-}" ]]; then
            test_env+=("E2E_SCALE_CHURN=$E2E_SCALE_CHURN")
        fi
    fi

    if [[ "$MULTISITE" -eq 1 ]]; then
        local ctx1="${CONTEXTS[1]}"
        local remote_ip="${NODE_ACCESS_IPS[$ctx1]}"
        log "Test target: local=$ctx0 ($local_ip:$NODEPORT_LDAP), remote=$ctx1 ($remote_ip:$NODEPORT_LDAP)"

        # The cn=config admin password is per-cluster: each SlapdCluster
        # auto-generates its own <name>-config-password, and unlike the database
        # credentials (pre-created identically on every site by
        # scripts/mesh-share-credentials.sh) it is NOT shared. So the suite's rootPW —
        # read from the LOCAL cluster — cannot bind cn=admin,cn=config on a
        # remote site, and any diagnostic that tried got
        # `LDAP Result Code 49 "Invalid Credentials"`. Export the remote site's
        # own config password so cross-site cn=config reads actually work.
        local remote_root_pw
        remote_root_pw=$(kctl "$ctx1" -n "$NAMESPACE_TESTING" get secret slapd-config-password \
            -o jsonpath='{.data.root-password}' 2>/dev/null | base64 -d || true)
        if [[ -z "$remote_root_pw" ]]; then
            warn "[$ctx1] could not read slapd-config-password; cross-site cn=config diagnostics will be skipped"
        fi

        test_env+=(
            "E2E_EXTERNAL_REPL=1"
            "E2E_REMOTE_LDAP_ADDR=${remote_ip}:${NODEPORT_LDAP}"
            "E2E_REMOTE_ADMIN_PW=$admin_pw"
            "E2E_REMOTE_ROOT_PW=$remote_root_pw"
        )
    else
        log "Test target: single-site $ctx0 ($local_ip:$NODEPORT_LDAP)"
    fi

    # Reproducibility + triage knobs. The suite mutates one shared `slapd`
    # cluster across ~15 specs, three of them destructive to it (case-2 deletes
    # its PVCs, resilience deletes its pods, restore-replay does an in-place
    # restore on it). Cross-spec interference is therefore a function of spec
    # order, which Ginkgo reshuffles every run.
    #
    #   E2E_SEED  — pin Ginkgo's spec-order seed. Defaulted here rather than left
    #               to Ginkgo so that every run's seed is chosen and logged by
    #               *us*: reproducing an interference failure then never depends
    #               on still having Ginkgo's stdout.
    #   FAIL_FAST — stop at the first failing spec. Without it one early failure
    #               cascades and later specs bury its cause; `./e2e.sh test`
    #               never tears down, and `all` aborts before teardown (set -e),
    #               so the failed state is left standing either way.
    local seed="${E2E_SEED:-$(date +%s)}"
    # Suite ceilings, not budgets. Raised from 25m/30m when the pod-replacing
    # specs (resilience ×3, dataloss ×1) gained a cross-site recovery wait: on a
    # multi-site pod-routed run each of them may sit for up to
    # crossSiteRecoveryBudget (8m, sized off measurements of 140-268s) while the
    # peer sites rediscover the new pod IPs. Four such waits plus the gated
    # backup/restore/scaleup scenarios can outrun 25m
    # without anything actually being wrong.
    local ginkgo_flags=(--ginkgo.v --ginkgo.timeout=60m "--ginkgo.seed=$seed")
    if [[ -n "${E2E_LABEL_FILTER:-}" ]]; then
        ginkgo_flags+=("--ginkgo.label-filter=$E2E_LABEL_FILTER")
        log "Label filter: $E2E_LABEL_FILTER (only matching specs run)"
    fi
    if [[ "${FAIL_FAST:-}" == "1" ]]; then
        ginkgo_flags+=(--ginkgo.fail-fast)
        log "FAIL_FAST=1 — stopping at the first failing spec (state left standing)"
    fi
    log "Ginkgo seed: $seed  — replay this exact spec order with E2E_SEED=$seed"

    log "Running e2e tests..."
    (
        cd "$PROJECT_ROOT/tests/e2e"
        env "${test_env[@]}" go test -v ./... \
            -timeout 65m \
            "${ginkgo_flags[@]}"
    )

    check_data_present_every_site
}

# check_data_present_every_site asserts that EVERY site's every SlapdDatabase
# reports DataPresent=True.
#
# It lives here rather than in the Go suite because only this script knows about
# the other sites: the suite reaches remote sites over LDAP only, and has no
# client for their Kubernetes API. And the non-founder sites are exactly the
# interesting ones — founder-only seeding (ADR-025 decision 1) means sites 2..N
# deploy seed-stripped, which is the shape whose DataPresent used to sit at
# Unknown/NotSeeded forever, with the per-pod suffix-visibility detector
# (decision 5) therefore never running. Measured on a healthy three-site mesh on
# 2026-09-14: 4 of 6 databases had the detector disabled that way.
#
# Budgets are per REASON, not one deadline for all of them — see
# datapresent_budget below for why, and E2E_DATAPRESENT_TIMEOUT /
# E2E_DATAPRESENT_RO_TIMEOUT to override either.

# Seconds a DataPresent reason that is NEVER legitimately transient after a
# completed suite gets before this gate fails: GlueSuffix, DataMissingOnPods,
# DataMissing, NoDataYet, NoReachablePod, an absent condition, and anything
# unrecognised.
#
# The gate only ever runs after a green suite (set -e aborts otherwise), so
# every spec has already asserted its own convergence; what is left to wait for
# is one fresh evaluation of the condition, not a cluster settling. The loop
# forces that evaluation (see the poke below), which lands in seconds, so 60s is
# an order of magnitude of headroom over the mechanism it waits on and still
# three times faster than the 180s a genuine GlueSuffix — the silent-corruption
# class this whole gate exists for — used to be granted before being reported.
DATAPRESENT_TIMEOUT="${E2E_DATAPRESENT_TIMEOUT:-60}"

# Seconds False/DataMissingOnReadOnlyPods gets — deliberately generous, and
# **a guess**, said plainly.
#
# How long a legitimate read-only initial sync takes is UNMEASURED on anything
# larger than the fixture: on fixture-sized data it is seconds, on a
# production-sized DIT nobody has timed it (docs/BACKLOG.md, "PRIORITY RAISED:
# the big-DIT initial-sync e2e" — that lane is what would calibrate this
# number, and until it runs, any arithmetic here would be arithmetic about
# nothing). 600s is picked to be comfortably larger than every RO re-sync
# observed on a lab fixture and small enough that a broken replica still ends
# the run rather than hanging it. Raise it via E2E_DATAPRESENT_RO_TIMEOUT if a
# fixture ever grows a DIT worth the name; replace it with a measurement when
# the big-DIT lane lands.
DATAPRESENT_RO_TIMEOUT="${E2E_DATAPRESENT_RO_TIMEOUT:-600}"

# How often the loop pokes a pending database into re-evaluating (seconds).
# A healthy SlapdDatabase reconciles on a 5-minute floor (databaseResyncInterval)
# and DataPresent is phase-neutral observability, so a False that has already
# healed can sit in status for up to five minutes with nothing wrong anywhere.
# Without the poke every budget below would be measuring condition staleness
# instead of cluster state — and the strict one could not be short at all.
DATAPRESENT_POKE_INTERVAL=30

# datapresent_budget maps a DataPresent reason to the seconds it gets.
#
# The reason strings are the operator's, verbatim (aggregateDataPresent in
# operator/internal/controller/data_present.go). Anything not named there falls
# to the strict budget on purpose: an unrecognised reason is not a licence to
# wait.
#
# Two budgets, because ADR-025's read-only amendment split the reasons for
# exactly this: "an alert rule can page immediately on DataMissingOnPods /
# GlueSuffix and give DataMissingOnReadOnlyPods a fuse longer than an initial
# sync". This gate is that alert rule. Both fuses still END in a failure — an
# RO pod carrying the same glue with the same entryUUID as its glued provider
# is ADR-025 evidence item 5, so "never fail on the RO reason" would restore
# the blind spot the all-pods rule was written to close.
datapresent_budget() { # reason → seconds
    case "$1" in
        DataMissingOnReadOnlyPods) echo "$DATAPRESENT_RO_TIMEOUT" ;;
        *)                         echo "$DATAPRESENT_TIMEOUT" ;;
    esac
}

check_data_present_every_site() {
    local start=$SECONDS last_poke=-1
    local ctx db status reason pending overdue budget elapsed poke

    log "Verifying DataPresent on every site's databases (ADR-025 D5) — \
${DATAPRESENT_TIMEOUT}s budget, ${DATAPRESENT_RO_TIMEOUT}s for DataMissingOnReadOnlyPods..."
    while true; do
        pending=""
        overdue=""
        elapsed=$((SECONDS - start))
        poke=0
        if (( last_poke < 0 || elapsed - last_poke >= DATAPRESENT_POKE_INTERVAL )); then
            poke=1
            last_poke=$elapsed
        fi

        for ctx in "${CONTEXTS[@]}"; do
            for db in $(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net \
                -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
                status=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net "$db" \
                    -o jsonpath='{.status.conditions[?(@.type=="DataPresent")].status}' 2>/dev/null || true)
                if [[ "$status" == "True" ]]; then continue; fi

                reason=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net "$db" \
                    -o jsonpath='{.status.conditions[?(@.type=="DataPresent")].reason}' 2>/dev/null || true)
                budget=$(datapresent_budget "${reason:-<none>}")
                pending+="  [$ctx] $db: ${status:-<absent>}/${reason:-<none>} (budget ${budget}s, waited ${elapsed}s)"$'\n'

                # Per-database budgets, judged per database: the strictest
                # applicable one governs, because the first database to outlive
                # its OWN budget ends the run. One RO-pending database can
                # therefore never extend the leash of a GlueSuffix one.
                if (( elapsed >= budget )); then
                    overdue+="  [$ctx] $db: ${status:-<absent>}/${reason:-<none>} — exceeded its ${budget}s budget"$'\n'
                fi

                # Force a fresh evaluation rather than waiting out the 5-minute
                # resync floor. The SlapdDatabase watch carries no predicate, so
                # a metadata-only write enqueues a reconcile immediately;
                # reconciles are idempotent (ADR-001) and DataPresent drives no
                # action (ADR-012), so the only effect is a current reading.
                # Best-effort: if the write fails we simply measure staleness.
                if (( poke )); then
                    kctl "$ctx" -n "$NAMESPACE_TESTING" annotate --overwrite \
                        slapddatabases.ldap.chuck-chuck-chuck.net "$db" \
                        "e2e.ldap.chuck-chuck-chuck.net/datapresent-poke=$(date +%s)" >/dev/null 2>&1 || true
                fi
            done
        done

        [[ -z "$pending" ]] && break

        if [[ -n "$overdue" ]]; then
            printf '%s' "$pending" >&2
            printf 'Over budget:\n%s' "$overdue" >&2
            die "DataPresent is not True on every site's databases (see above). \
GlueSuffix, DataMissingOnPods, DataMissing, NoDataYet, NoReachablePod and an absent condition \
share the ${DATAPRESENT_TIMEOUT}s budget: none of them is legitimately transient once the suite has \
finished, so the only thing being waited for is one reconcile, and a glue suffix is silent \
corruption that should be reported fast (ADR-025). DataMissingOnReadOnlyPods gets \
${DATAPRESENT_RO_TIMEOUT}s instead because ADR-025's read-only amendment calls it legitimately \
transient during an RO initial sync — that budget is a guess, not a measurement, and \
E2E_DATAPRESENT_RO_TIMEOUT raises it. Exceeding even that means the replica is broken or its \
syncrepl stanzas are not converging (check with: slctl inspect -n $NAMESPACE_TESTING slapd)."
        fi
        sleep 5
    done
    log "DataPresent=True on every site's databases."
}

# ── Teardown ─────────────────────────────────────────────────────────────────

# Bound, in seconds, on any single blocking teardown step. Teardown must never
# outlive the thing it is tearing down: the context loop is serial, so one site
# that never finishes used to mean the remaining sites were never torn down at
# all. 180s is roughly 3x the slowest healthy step measured on a lab (a 4-pod
# cluster's StatefulSet deletion plus PVC release) — long enough that a busy
# cluster is not declared stuck, short enough that a human notices the run ended.
TEARDOWN_TIMEOUT="${E2E_TEARDOWN_TIMEOUT:-180}"

# Contexts whose teardown did not complete. Collected rather than fatal, so the
# remaining contexts are still torn down; reported and exited non-zero at the end.
TEARDOWN_STUCK=()
declare -A TEARDOWN_STUCK_CTX

teardown_stuck() { # ctx message
    TEARDOWN_STUCK+=("[$1] $2")
    TEARDOWN_STUCK_CTX["$1"]=1
    warn "[$1] $2"
}

# Owners before storage (ADR-018). Any pod object that names a PVC holds a
# deletion lease on it — the pod's phase is irrelevant — so `kubectl delete pvc
# --all` against a namespace that still runs a slapd pod marks the PVCs
# Terminating and then blocks forever, which blocks the namespace, which blocks
# every remaining context.
#
# Teardown only ever knew about the fixtures it created itself. A SlapdCluster
# that a *spec* stood up and deliberately kept — E2E_KEEP_ON_FAILURE, see
# tests/e2e/restore_test.go — is exactly such a pod, and is what hung teardown
# on a three-site lab on 2026-09-15. So this deletes by --all, not by name.
#
# Order inside the step is not free either: a SlapdDatabase finalizer reconciles
# cn=config on the live pods (ADR-005), so every database goes first, while its
# cluster's pods are still running, and the clusters follow.
delete_slapd_owners() { # ctx
    local ctx="$1" kind names extra known n

    for kind in slapddatabase slapdcluster; do
        names=$(kctl "$ctx" get "$kind" -n "$NAMESPACE_TESTING" \
            -o jsonpath='{range .items[*]}{.metadata.name} {end}' 2>/dev/null || true)
        names="${names% }"
        [[ -z "$names" ]] && continue

        # Everything this script itself created. Anything else is debris from a
        # spec, and is evidence somebody may have wanted — say so before it dies.
        case "$kind" in
            slapddatabase) known=" $DB_CR_NAME $DB2_CR_NAME " ;;
            slapdcluster)  known=" slapd " ;;
        esac
        extra=""
        for n in $names; do
            [[ "$known" == *" $n "* ]] || extra+=" $n"
        done
        if [[ -n "$extra" ]]; then
            warn "[$ctx] $kind not created by this script:$extra — deleting. If an earlier suite run kept it on purpose (E2E_KEEP_ON_FAILURE), its evidence goes now."
        fi

        log "[$ctx] Deleting $kind: $names"
        kctl "$ctx" delete "$kind" --all -n "$NAMESPACE_TESTING" --ignore-not-found \
            --timeout="${TEARDOWN_TIMEOUT}s" \
            || teardown_stuck "$ctx" "$kind deletion did not finish within ${TEARDOWN_TIMEOUT}s"
    done
}

# What is still standing, and how to finish the job by hand.
report_stuck_namespace() { # ctx
    local ctx="$1"
    warn "[$ctx] $NAMESPACE_TESTING did not drain. Still present:"
    kctl "$ctx" get slapdcluster,slapddatabase,statefulset,pod,pvc \
        -n "$NAMESPACE_TESTING" >&2 2>/dev/null || true
    cat >&2 <<EOF

Finish it by hand — owners before storage, because a pod object is a PVC
deletion lease (ADR-018) and deleting the PVCs first only makes them Terminating:

  kubectl --context $ctx -n $NAMESPACE_TESTING delete slapddatabase --all
  kubectl --context $ctx -n $NAMESPACE_TESTING delete slapdcluster --all
  kubectl --context $ctx -n $NAMESPACE_TESTING delete pvc --all
  kubectl --context $ctx delete namespace $NAMESPACE_TESTING

A SlapdDatabase that will not go is holding its cleanup finalizer because a pod
was unreachable — that is deliberate (ADR-005). Look at why first, then force it:

  kubectl --context $ctx -n $NAMESPACE_TESTING patch slapddatabase <name> \\
      --type=merge -p '{"metadata":{"finalizers":null}}'

The operator and the CRDs are left installed on this context so the finalizer
can still be released; re-run the teardown once the namespace is gone.
EOF
}

teardown_all() {
    log "Tearing down deployment..."

    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Removing resources from $NAMESPACE_TESTING..."

        # A namespace left Terminating by an interrupted run — the Ctrl-C after
        # the hang this ordering fixes — still accepts deletes, and needs them:
        # nothing else will remove the pod whose lease holds its PVCs. So the
        # owner deletes below run either way; only the namespace delete is
        # skipped, since one is already in flight.
        local ns_terminating=0
        if [[ "$(kctl "$ctx" get namespace "$NAMESPACE_TESTING" \
                 -o jsonpath='{.status.phase}' 2>/dev/null || true)" == "Terminating" ]]; then
            ns_terminating=1
            warn "[$ctx] $NAMESPACE_TESTING is already Terminating from an earlier run — clearing what holds it, not deleting it again."
        fi

        # Test resources (SlapdDatabase, SlapdSchema, readpw secret).
        if [[ -d "$resource_dir" ]]; then
            kctl "$ctx" delete -n "$NAMESPACE_TESTING" -f "$resource_dir/" \
                --ignore-not-found --timeout="${TEARDOWN_TIMEOUT}s" 2>/dev/null || true
        fi

        # SlapdCluster.
        hctl "$ctx" uninstall slapd -n "$NAMESPACE_TESTING" 2>/dev/null || true

        # Everything the fixtures above did not cover: spec-created SlapdDatabases
        # and SlapdClusters. Owners before storage — see delete_slapd_owners.
        delete_slapd_owners "$ctx"

        # NodePort services (main + per-pod).
        kctl "$ctx" delete svc slapd-external -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        for i in 0 1 2 3 4 5 6 7; do
            kctl "$ctx" delete svc "slapd-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
            kctl "$ctx" delete svc "slapd-readonly-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        done

        # Cross-trust and kubeconfig secrets, under the site-derived names this
        # run creates them with AND the context-derived names the pre-Phase-7
        # hand-wired path used. The second set is not dead code: a namespace that
        # a pre-Phase-7 run left behind still holds them, and a stale CA Secret
        # under the old scheme is exactly the debris that makes the next run's
        # failure hard to read. It costs one no-op delete per pair and can be
        # dropped once no lab has a pre-Phase-7 namespace left.
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            kctl "$ctx" delete secret "$(peer_ca_secret "$other")" "site-${other}-ca" \
                -n "$NAMESPACE_TESTING" --ignore-not-found || true
            kctl "$ctx" delete secret "$(peer_kubeconfig_secret "$other")" "${other}-kubeconfig" \
                -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        done

        # Remote reader RBAC (created by mesh-authorize-peers.sh).
        kctl "$ctx" delete rolebinding slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete role slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete secret slaptain-remote-reader-token -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete sa slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true

        # Database credentials secrets.
        kctl "$ctx" delete secret "$DB_CREDENTIALS_SECRET" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        if [[ -n "$DB2_CREDENTIALS_SECRET" ]]; then
            kctl "$ctx" delete secret "$DB2_CREDENTIALS_SECRET" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        fi

        # CSR (cluster-scoped) — name includes the namespace.
        kctl "$ctx" delete csr "slapd-${NAMESPACE_TESTING}-csr" --ignore-not-found 2>/dev/null || true

        # PVCs. Safe to wait on now: every owner above is gone, so no pod object
        # holds a lease on them.
        kctl "$ctx" delete pvc --all -n "$NAMESPACE_TESTING" --timeout="${TEARDOWN_TIMEOUT}s" 2>/dev/null \
            || teardown_stuck "$ctx" "PVC deletion did not finish within ${TEARDOWN_TIMEOUT}s"

        # Testing namespace.
        if [[ "$ns_terminating" -eq 0 ]]; then
            kctl "$ctx" delete namespace "$NAMESPACE_TESTING" --ignore-not-found \
                --timeout="${TEARDOWN_TIMEOUT}s" \
                || teardown_stuck "$ctx" "namespace $NAMESPACE_TESTING did not go within ${TEARDOWN_TIMEOUT}s"
        elif kctl "$ctx" get namespace "$NAMESPACE_TESTING" >/dev/null 2>&1; then
            teardown_stuck "$ctx" "namespace $NAMESPACE_TESTING was already Terminating and still is"
        fi

        [[ -n "${TEARDOWN_STUCK_CTX[$ctx]:-}" ]] && report_stuck_namespace "$ctx"
    done

    # Cluster-scoped resources: operator, CRDs, operator namespace.
    for ctx in "${CONTEXTS[@]}"; do
        # Removing the operator or the CRDs under a namespace that has not
        # drained is strictly destructive: the operator is the only thing that
        # can still release a SlapdDatabase finalizer, and deleting a CRD out
        # from under finalized CRs wedges them permanently.
        if [[ -n "${TEARDOWN_STUCK_CTX[$ctx]:-}" ]]; then
            warn "[$ctx] Leaving the operator and the CRDs installed — $NAMESPACE_TESTING is not clean."
            continue
        fi

        log "[$ctx] Removing cluster-scoped resources..."

        hctl "$ctx" uninstall slaptain -n "$NAMESPACE" 2>/dev/null || true

        kctl "$ctx" delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapddatabases.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapdschemas.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        # SlapdMesh (ADR-028). Helm only installs a chart's crds/ on the FIRST
        # install, so a CRD left behind here is the one the next run would be
        # stuck with — and this is the kind whose schema is still moving.
        kctl "$ctx" delete crd slapdmeshes.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true

        kctl "$ctx" delete namespace "$NAMESPACE" --ignore-not-found \
            --timeout="${TEARDOWN_TIMEOUT}s" \
            || teardown_stuck "$ctx" "namespace $NAMESPACE did not go within ${TEARDOWN_TIMEOUT}s"
    done

    if [[ ${#TEARDOWN_STUCK[@]} -gt 0 ]]; then
        warn "Teardown INCOMPLETE:"
        for line in "${TEARDOWN_STUCK[@]}"; do warn "  $line"; done
        return 1
    fi

    log "Teardown complete."
}

# ── Main ─────────────────────────────────────────────────────────────────────

[[ $# -lt 1 ]] && usage

subcommand="$1"; shift
CONTEXTS=("$@")

# No context given → the lab file's site list (all sites, multi-site when
# N≥2), else single-site on the current kubectl context.
if [[ ${#CONTEXTS[@]} -lt 1 ]]; then
    if [[ ${#LAB_CONTEXTS[@]} -ge 1 ]]; then
        CONTEXTS=("${LAB_CONTEXTS[@]}")
        log "No context given — using lab config sites: ${CONTEXTS[*]}"
    else
        current_ctx=$(kubectl config current-context 2>/dev/null || true)
        [[ -z "$current_ctx" ]] && die "No context given and no current kubectl context is set"
        CONTEXTS=("$current_ctx")
        log "No context given — using current context: $current_ctx"
    fi
fi

# MULTISITE=1 when running the cross-cluster path. Used to gate external-peer
# wiring, cross-trust CA distribution, the convergence sleep, and the
# E2E_EXTERNAL_REPL test-suite flag. N=1 collapses every per-context loop to
# a single iteration with empty peer arrays.
MULTISITE=0
if [[ ${#CONTEXTS[@]} -ge 2 ]]; then
    MULTISITE=1
fi

# A SlapdMesh describes SITES, and a site is reached through its API server, so
# every peer the operator derives uses ADR-007 dynamic discovery. There is no
# mesh field that produces a static NodePort `uri`, deliberately
# (externalPeersForSite: "static addressing is the pre-discovery path and stays
# hand-configured"). A multi-site run therefore needs a discovery transport —
# say so here rather than let it surface as a cluster with zero peers.
#
# This is the one capability the pre-Phase-7 hand-wired path had and this one
# does not: cross-site replication over NodePort URIs, and over static Multus
# podAddresses, are no longer exercised by the suite. Both were hand-configured
# shapes with no mesh expression; the operator still supports them.
#
# Checked in do_setup rather than here, so `config` stays a pure dump and
# `teardown` can still clean up after a run that was started differently.
require_discovery_transport() {
    [[ "$MULTISITE" -eq 1 ]] && ! discovery_mode || return 0
    echo "ERROR: a ${#CONTEXTS[@]}-site run needs a discovery transport." >&2
    echo "       Set POD_ROUTED=1 (ADR-016) or MULTUS_NETWORK=<nad> (ADR-007)." >&2
    echo "       The mesh derives peers via the remote k8s API; NodePort URIs and" >&2
    echo "       static podAddresses are hand-configured shapes with no mesh form." >&2
    exit 1
}

declare -A NODE_IPS          # node k8s InternalIP — the cross-site API-server address
declare -A NODE_ACCESS_IPS   # address used to reach node NodePorts + cert SAN (override: E2E_NODE_ACCESS_IP[S])
declare -A SITE_NAMES        # logical per-site identity handed to the operator (ADR-028 §4)
declare -A SITE_INDICES      # that site's serverID slot; decade = index * 100 (ADR-017)

assign_site_names

# Fail fast when the images this run would deploy were never pushed — an
# ImagePullBackOff twenty minutes into setup is the worst way to learn that.
# Anonymous HEAD against the OCI distribution API; only a definite 404 dies.
# Registries that demand auth even for manifest HEADs (401/403) and unreachable
# ones get a warning — the probe must never false-fail a working private setup.
# Probe the registry for an image, following the Docker registry v2 auth dance.
#
# A bare GET is not enough: ghcr.io (and Docker Hub, and any registry with
# token auth) answers 401 with a WWW-Authenticate challenge even for images
# that are PUBLIC, and expects the client to exchange it for a bearer token.
# Without that the probe could never return 200 there — it fell into the
# "cannot verify, continuing" branch on every call, which is the worst outcome:
# the check that exists to fail fast instead waved the run through, and the
# missing image surfaced minutes later as ImagePullBackOff.
#
# The challenge is parsed rather than hardcoded, so this works against ghcr,
# Docker Hub, Harbor and a plain open registry alike. Credentials are never
# sent: an anonymous token is all a public image needs, and a private one
# legitimately stays unverifiable (still a WARN, not a failure).
registry_probe() { # url -> http code on stdout
    local url="$1" hdrs code challenge realm service scope token
    hdrs=$(curl -s -o /dev/null -D - --max-time 10 \
        -H "$REGISTRY_ACCEPT" "$url" 2>/dev/null) || { echo 000; return; }
    code=$(printf '%s' "$hdrs" | awk 'NR==1{print $2}')
    [[ "$code" != "401" ]] && { echo "${code:-000}"; return; }

    challenge=$(printf '%s' "$hdrs" | grep -i '^www-authenticate:' | head -1)
    realm=$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"$challenge")
    service=$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"$challenge")
    scope=$(sed -n 's/.*scope="\([^"]*\)".*/\1/p' <<<"$challenge")
    [[ -z "$realm" ]] && { echo 401; return; }

    token=$(curl -s --max-time 10 --get \
        ${service:+--data-urlencode "service=$service"} \
        ${scope:+--data-urlencode "scope=$scope"} \
        "$realm" 2>/dev/null | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    [[ -z "$token" ]] && { echo 401; return; }

    curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -H "Authorization: Bearer $token" -H "$REGISTRY_ACCEPT" "$url" 2>/dev/null || echo 000
}

REGISTRY_ACCEPT="Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

require_image_in_registry() { # image-name tag
    local host path url code
    case "$REGISTRY" in
        */*) host="${REGISTRY%%/*}"; path="${REGISTRY#*/}/$PROJECT/$1" ;;
        *)   host="$REGISTRY";       path="$PROJECT/$1" ;;
    esac
    url="https://$host/v2/$path/manifests/$2"
    code=$(registry_probe "$url")
    case "$code" in
        200) : ;;
        404) die "$REGISTRY/$PROJECT/$1:$2 is not in the registry. Build and push first (make push REGISTRY=$REGISTRY), or pin GIT_TAG=<a pushed tag>. Untagged builds are addressed sha-<hash>, and a dirty tree derives sha-<hash>-dirty-<state8> (scripts/image-tag.sh + docs/VERSIONING.md) — those exist only after you push them." ;;
        401|403) log "WARN: $REGISTRY/$PROJECT/$1:$2 needs credentials to verify (HTTP $code) — continuing. A private image is fine if the cluster can pull it; a typo in the registry path looks the same from here." ;;
        *)   log "WARN: cannot verify $1:$2 in the registry (HTTP $code) — continuing" ;;
    esac
}

do_setup() {
    require_discovery_transport
    require_image_in_registry slapd "$SLAPD_TAG"
    require_image_in_registry operator "$IMAGE_TAG"
    build_script_lab_file
    setup_foundation
    # The bootstrap proper, in the order docs/MULTI-SITE.md documents, each step
    # delegated to the script a user would run.
    setup_shared_credentials
    setup_tls_trust
    setup_remote_kubeconfigs
    # The fixture's non-CR manifests (the readpw Secret). The SlapdDatabases and
    # SlapdSchemas themselves come out of charts/slapd-mesh below, in the same
    # release as the SlapdCluster — so the operator's first StatefulSet reconcile
    # already sees the full database list and bakes the right DATABASE_DIRS into
    # the initial pod template, which is what the pre-chart ordering here was
    # for (BUG-ANALYSIS-database-dirs-rolling-restart.md, option A).
    apply_test_resources
    setup_mesh_bundle
    # A cluster whose meshRef does not resolve reconciles NOTHING, so the
    # StatefulSet below would simply never appear. Check the condition that
    # explains why before waiting on the symptom.
    wait_mesh_resolved
    wait_for_clusters_ready
    setup_nodeport_services
    wait_test_resources_ready
}

# `config` is a pure dump — resolved values only, no cluster access. Handled
# before discover_node_ips so it works with unreachable contexts too.
if [[ "$subcommand" == "config" ]]; then
    echo "lab config file:      ${E2E_CONFIG:-<none>}"
    echo "contexts:             ${CONTEXTS[*]} (multisite=$MULTISITE)"
    site_map=""
    for ctx in "${CONTEXTS[@]}"; do site_map+="${ctx}=${SITE_NAMES[$ctx]} "; done
    echo "site identities:      ${site_map% } (founder: site-1, per spec.seed.site)"
    echo "deployment path:      charts/slapd-mesh, one values file per run (ADR-028)"
    peer_map=""
    for ctx in "${CONTEXTS[@]}"; do
        peer_map+="${SITE_NAMES[$ctx]}[$(peer_ca_secret "$ctx"),$(peer_kubeconfig_secret "$ctx")] "
    done
    echo "peer[ca,kubeconfig]:  ${peer_map% }"
    echo "registry/project:     $REGISTRY / $PROJECT"
    echo "image tag:            $IMAGE_TAG${SLAPD_TAG_SUFFIX:+ (slapd pair: $IMAGE_TAG$SLAPD_TAG_SUFFIX)}"
    echo "operator namespace:   $NAMESPACE"
    echo "testing namespace:    $NAMESPACE_TESTING"
    echo "test resources:       $TEST_RESOURCES"
    if [[ -n "$POD_ROUTED" ]]; then
        echo "replication network:  pod-routed (ADR-016)"
    elif [[ -n "$MULTUS_NETWORK" ]]; then
        echo "replication network:  multus ($MULTUS_NETWORK, ADR-007)"
    elif [[ "$MULTISITE" -eq 1 ]]; then
        echo "replication network:  <none - a multi-site run is REFUSED without POD_ROUTED or MULTUS_NETWORK>"
    else
        echo "replication network:  <none needed: single site>"
    fi
    echo "node access IPs:      ${E2E_NODE_ACCESS_IPS:-${E2E_NODE_ACCESS_IP:-<InternalIP default>}}"
    exit 0
fi

discover_node_ips
resolve_cr_names

case "$subcommand" in
    setup)
        do_setup
        log ""
        log "Setup complete."
        log "  Contexts: ${CONTEXTS[*]}"
        log "  Deployment: charts/slapd-mesh, one values file ($MESH_VALUES_FILE) at every site"
        if [[ -n "$POD_ROUTED" ]]; then
            log "  Cross-site transport: pod-routed (primary pod IPs, discovery; ADR-016)"
        elif [[ -n "$MULTUS_NETWORK" ]]; then
            log "  Replication network: $MULTUS_NETWORK (Multus, dynamic discovery; ADR-007)"
        fi
        ;;
    test)
        run_tests
        ;;
    teardown)
        teardown_all
        ;;
    all)
        do_setup
        run_tests
        teardown_all
        ;;
    *)
        usage
        ;;
esac
