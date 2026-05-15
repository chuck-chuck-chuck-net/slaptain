#!/bin/bash
# Unified e2e test orchestration. Single entry point for single-site and
# multi-site test runs — the script branches on the number of contexts:
#
#   N=1  single-site:  one cluster, no external peers, basic LDAP/ACL/schema
#                      coverage. E2E_EXTERNAL_REPL is NOT set.
#   N≥2  multi-site:   N clusters, full external-peer mesh, cross-site CSN
#                      convergence, includes E2E_EXTERNAL_REPL=1.
#
# The shape of every per-context setup phase is identical regardless of N;
# only run_tests and a couple of cross-cluster-only steps differ. The N=1
# case naturally falls out of the multi-site loop with empty peer arrays.
#
# Usage:
#   ./tests/e2e.sh setup    ctx1 [ctx2 ...]
#   ./tests/e2e.sh test     ctx1 [ctx2 ...]
#   ./tests/e2e.sh teardown ctx1 [ctx2 ...]
#   ./tests/e2e.sh all      ctx1 [ctx2 ...]
#
# The legacy tests/e2e-singlesite.sh and tests/e2e-multisite.sh are now
# thin wrappers around this script — see their source.
#
# Prerequisites: kubectl contexts that reach each cluster. Container images
# must be available in a registry reachable from every cluster.
set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

NAMESPACE="${NAMESPACE:-slaptain}"

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
# replication uses a dedicated Multus network instead of NodePort services.
# NodePort services are still created for test runner connectivity.
#
# Two Multus peer discovery modes (ADR-007 amendment):
#
#   Dynamic (default):  The operator discovers remote pod Multus IPs by querying
#                       the remote cluster's k8s API over the replication network.
#                       ExternalPeers use discovery.kubeconfigSecret. The script
#                       provisions cross-site RBAC and kubeconfig Secrets via
#                       scripts/create-remote-kubeconfig.sh.
#
#   Static:             The script discovers Multus IPs from pod annotations and
#                       patches the CRs with static podAddresses. Set
#                       STATIC_PODADDRESSES=1 to use this legacy mode.
#
# MULTUS_NETWORK: NAD reference, e.g. "infra/replication-net" or "replication-net"
MULTUS_NETWORK="${MULTUS_NETWORK:-}"
STATIC_PODADDRESSES="${STATIC_PODADDRESSES:-}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Image tag: git tag or short commit hash (with -dirty suffix for uncommitted changes).
if [[ -z "${GIT_TAG:-}" ]]; then
    if exact=$(git -C "$PROJECT_ROOT" describe --tags --exact-match 2>/dev/null) && [[ -n "$exact" ]]; then
        GIT_TAG="$exact"
    else
        hash=$(git -C "$PROJECT_ROOT" rev-parse --short HEAD)
        if ! git -C "$PROJECT_ROOT" diff --quiet HEAD 2>/dev/null; then
            GIT_TAG="${hash}-dirty"
        else
            GIT_TAG="$hash"
        fi
    fi
fi

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
Usage: $0 <setup|test|teardown|all> ctx1 [ctx2 ...]

One context = single-site mode; two or more = multi-site mode with full
external-peer mesh. The script auto-detects N from the argument count.

Subcommands:
  setup     Deploy operator, SlapdCluster, test resources on all clusters
  test      Run e2e tests (external replication tests gated on N>=2)
  teardown  Remove everything created by setup
  all       setup + test + teardown

Environment variables (with defaults):
  NAMESPACE            = $NAMESPACE
  NAMESPACE_TESTING    = $NAMESPACE_TESTING
  NODEPORT_LDAP        = $NODEPORT_LDAP
  NODEPORT_LDAPS       = $NODEPORT_LDAPS
  REGISTRY             = $REGISTRY
  PROJECT              = $PROJECT
  TEST_RESOURCES       = $TEST_RESOURCES  (example or lab)
  HELM_VALUES          = operator chart values (use absolute paths)
  HELM_VALUES_SLAPD_CLUSTER = slapd-cluster chart values (use absolute paths)

Multus replication network (ADR-007):
  MULTUS_NETWORK       = NAD reference (e.g. "infra/replication-net")
                         When set, cross-site replication uses Multus pod-to-pod.
  STATIC_PODADDRESSES  = Set to 1 for legacy static podAddresses mode.
                         Default (unset): dynamic discovery via remote kubeconfig.
EOF
    exit 1
}

# ── Multus helpers ───────────────────────────────────────────────────────────

# Associative array: context → comma-separated Multus pod IPs.
# Populated by discover_multus_ips after pods are running.
declare -A MULTUS_IPS

# Discover Multus IPs from running pods on all clusters.
# Reads the k8s.v1.cni.cncf.io/network-status annotation from each slapd pod
# and extracts the non-default interface IP. Populates MULTUS_IPS.
discover_multus_ips() {
    [[ -z "$MULTUS_NETWORK" ]] && return

    log "Discovering Multus pod IPs from running pods..."
    for ctx in "${CONTEXTS[@]}"; do
        local ips=()
        local replicas
        replicas=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get statefulset/slapd \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 1)

        for i in $(seq 0 $((replicas - 1))); do
            local pod="slapd-$i"
            local ip
            ip=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get pod "$pod" \
                -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' 2>/dev/null \
                | python3 -c "
import json, sys
data = json.load(sys.stdin)
for net in data:
    if not net.get('default', False) and net.get('ips'):
        print(net['ips'][0])
        break
" 2>/dev/null || echo "")
            [[ -z "$ip" ]] && die "[$ctx] $pod: no Multus IP found in network-status annotation"
            ips+=("$ip")
            log "  [$ctx] $pod -> $ip"
        done

        MULTUS_IPS[$ctx]=$(IFS=','; echo "${ips[*]}")
    done
}

# Patch SlapdCluster CRs on each cluster to add externalPeers with discovered
# Multus podAddresses. Called after discover_multus_ips.
# Used in legacy STATIC_PODADDRESSES mode only.
configure_multus_external_peers_static() {
    [[ -z "$MULTUS_NETWORK" ]] && return

    # Helm --set treats commas as value separators — escape for LDAP DNs.
    local helm_suffix="${DB_SUFFIX//,/\\,}"
    local helm_bind_dn="cn=replication\\,${helm_suffix}"

    local site_idx=0
    for ctx in "${CONTEXTS[@]}"; do
        local server_id_base=$((site_idx * 100))
        log "[$ctx] Configuring externalPeers with static Multus podAddresses (serverIDBase=${server_id_base})..."

        local peer_sets=()
        local peer_idx=0
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            IFS=',' read -ra other_ips <<< "${MULTUS_IPS[$other]}"
            peer_sets+=(
                --set "replication.externalPeers[$peer_idx].name=site-${other}"
                --set "replication.externalPeers[$peer_idx].port=1025"
                --set "replication.externalPeers[$peer_idx].tlsSecretName=site-${other}-ca"
                --set "replication.externalPeers[$peer_idx].bindDN=${helm_bind_dn}"
                --set "replication.externalPeers[$peer_idx].bindPasswordSecretName=${DB_CREDENTIALS_SECRET}"
            )
            for addr_idx in "${!other_ips[@]}"; do
                peer_sets+=(
                    --set "replication.externalPeers[$peer_idx].podAddresses[$addr_idx]=${other_ips[$addr_idx]}"
                )
            done
            ((peer_idx++)) || true
        done

        # Helm upgrade with the same values + externalPeers added.
        hctl "$ctx" upgrade slapd "$PROJECT_ROOT/charts/slapd-cluster" \
            --namespace "$NAMESPACE_TESTING" \
            -f "$VALUES_FILE" \
            --set "images.slapd.repository=$REGISTRY/$PROJECT/slapd" \
            --set "images.slapd.tag=$GIT_TAG" \
            --set "images.init.repository=$REGISTRY/$PROJECT/slapd-init" \
            --set "images.init.tag=$GIT_TAG" \
            --set "replication.network.multusNetwork=$MULTUS_NETWORK" \
            --set "replication.serverIDBase=${server_id_base}" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${peer_sets[@]}" \
            ${HELM_VALUES_SLAPD_CLUSTER:-}
        ((site_idx++)) || true
    done
}

# ── Dynamic discovery helpers (ADR-007 amendment) ───────────────────────────

# Set up cross-site kubeconfig Secrets for operator-driven dynamic peer discovery.
# Uses scripts/create-remote-kubeconfig.sh to create RBAC + kubeconfig Secrets.
setup_remote_kubeconfigs() {
    [[ "$MULTISITE" -eq 0 ]] && return
    [[ -z "$MULTUS_NETWORK" ]] && return
    [[ -n "$STATIC_PODADDRESSES" ]] && return

    log "Setting up cross-site kubeconfig Secrets for dynamic discovery..."

    # Build context=API pairs. The API server address uses the node's replication
    # network IP — discovered from the Multus network-status on the first running
    # pod, or falling back to the node's InternalIP (which works when the API
    # server binds 0.0.0.0 and the replication network is routable).
    local pairs=()
    for ctx in "${CONTEXTS[@]}"; do
        # Use node IP as the API server address on the replication network.
        # The k8s API is reachable at the
        # node's replication-network IP because it binds 0.0.0.0.
        local api_ip="${NODE_IPS[$ctx]}"
        local api_server api_port
        api_server=$(kctl "$ctx" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
        # Extract port from https://host:port — default 6443 if no port specified.
        if [[ "$api_server" =~ :([0-9]+)$ ]]; then
            api_port="${BASH_REMATCH[1]}"
        else
            api_port="6443"
        fi
        pairs+=("${ctx}=https://${api_ip}:${api_port}")
    done

    "$PROJECT_ROOT/scripts/create-remote-kubeconfig.sh" \
        -n "$NAMESPACE_TESTING" \
        "${pairs[@]}"
}

# Unified entry point: configure Multus external peers post-deploy.
# Only needed for static podAddresses mode — dynamic discovery peers are
# configured in the initial helm install (setup_slapd_clusters).
configure_multus_external_peers() {
    [[ "$MULTISITE" -eq 0 ]] && return
    [[ -z "$MULTUS_NETWORK" ]] && return
    [[ -z "$STATIC_PODADDRESSES" ]] && return

    discover_multus_ips
    configure_multus_external_peers_static
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
        log "  $ctx → ${NODE_IPS[$ctx]}"
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
}

# ── Setup phases ─────────────────────────────────────────────────────────────

generate_shared_credentials() {
    log "Generating shared database credentials..."
    SHARED_ROOT_PW=$(openssl rand -base64 18)
    SHARED_REPL_PW=$(openssl rand -base64 24)
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

        log "[$ctx] Pre-creating shared database credentials ($DB_CREDENTIALS_SECRET)..."
        kctl "$ctx" create secret generic "$DB_CREDENTIALS_SECRET" \
            -n "$NAMESPACE_TESTING" \
            --from-literal=root-password="$SHARED_ROOT_PW" \
            --from-literal=replication-password="$SHARED_REPL_PW" \
            --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -

        # TLS certificate: node IP for NodePort test access. Multus IPs are NOT
        # needed as SANs — the operator sets tls_reqcert=allow on syncrepl stanzas
        # for IP-based providers, so CA verification suffices (ADR-007).
        log "[$ctx] Generating TLS certificate (IP SAN: ${NODE_IPS[$ctx]})..."
        (
            cd "$SCRIPT_DIR"
            ./gencert.sh \
                -c "$ctx" \
                -n "$NAMESPACE_TESTING" \
                -t slapd \
                -s slapd \
                -H slapd-headless \
                -i "${NODE_IPS[$ctx]}" \
                slapd-tls
        )

        log "[$ctx] Installing operator (tag: $GIT_TAG)..."
        local operator_multus_sets=()
        if [[ -n "$MULTUS_NETWORK" && -z "$STATIC_PODADDRESSES" ]]; then
            operator_multus_sets=(--set "multus.network=$MULTUS_NETWORK")
        fi
        hctl "$ctx" upgrade --install slaptain-operator "$PROJECT_ROOT/charts/operator" \
            --namespace "$NAMESPACE" --create-namespace \
            --set "image.repository=$REGISTRY/$PROJECT/operator" \
            --set "image.tag=$GIT_TAG" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${operator_multus_sets[@]}" \
            ${HELM_VALUES:-}
    done
}

setup_cross_trust() {
    [[ "$MULTISITE" -eq 0 ]] && return
    log "Setting up cross-cluster TLS trust..."

    # Extract CA cert from each cluster.
    declare -A CA_CERTS
    for ctx in "${CONTEXTS[@]}"; do
        CA_CERTS[$ctx]=$(kctl "$ctx" -n "$NAMESPACE_TESTING" get secret slapd-tls \
            -o jsonpath='{.data.ca\.crt}' | base64 -d)
        [[ -z "${CA_CERTS[$ctx]}" ]] && die "Could not extract CA cert from $ctx"
    done

    # Create cross-trust secrets: on each cluster, install every OTHER cluster's CA.
    for dst in "${CONTEXTS[@]}"; do
        for src in "${CONTEXTS[@]}"; do
            [[ "$src" == "$dst" ]] && continue
            log "  site-${src}-ca → $dst"
            kctl "$dst" -n "$NAMESPACE_TESTING" create secret generic "site-${src}-ca" \
                --from-literal=ca.crt="${CA_CERTS[$src]}" \
                --dry-run=client -o yaml \
                | kctl "$dst" apply -f -
        done
    done
}

setup_slapd_clusters() {
    # Helm --set treats commas as value separators. Escape them with \, for
    # values that contain literal commas (LDAP DNs like dc=example,dc=org).
    local helm_suffix="${DB_SUFFIX//,/\\,}"
    local helm_bind_dn="cn=replication\\,${helm_suffix}"

    local site_idx=0
    for ctx in "${CONTEXTS[@]}"; do
        # Per-site serverIDBase keeps slapd's multimaster CSN tracking
        # collision-free across sites. With base=site_idx*100 each cluster
        # gets its own decade (siteA: 1-99, siteB: 101-199, ...) — matches
        # the convention codified in ADR-011.
        # Required for cross-cluster syncrepl to converge.
        local server_id_base=$((site_idx * 100))
        log "[$ctx] Installing SlapdCluster (serverIDBase=${server_id_base})..."

        local peer_sets=()
        local peer_idx=0
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue

            if [[ -n "$MULTUS_NETWORK" && -z "$STATIC_PODADDRESSES" ]]; then
                # Dynamic discovery: configure externalPeers with kubeconfigSecret
                # in the initial install. Cross-trust secrets and kubeconfig secrets
                # are already created, so the pod template gets the CA volumes right
                # away — no second Helm upgrade needed.
                peer_sets+=(
                    --set "replication.externalPeers[$peer_idx].name=site-${other}"
                    --set "replication.externalPeers[$peer_idx].port=1025"
                    --set "replication.externalPeers[$peer_idx].tlsSecretName=site-${other}-ca"
                    --set "replication.externalPeers[$peer_idx].bindDN=${helm_bind_dn}"
                    --set "replication.externalPeers[$peer_idx].bindPasswordSecretName=${DB_CREDENTIALS_SECRET}"
                    --set "replication.externalPeers[$peer_idx].discovery.kubeconfigSecret.name=${other}-kubeconfig"
                )
            elif [[ -z "$MULTUS_NETWORK" ]]; then
                # NodePort mode: configure externalPeers with URIs.
                peer_sets+=(
                    --set "replication.externalPeers[$peer_idx].name=site-${other}"
                    --set "replication.externalPeers[$peer_idx].uri=ldaps://${NODE_IPS[$other]}:${NODEPORT_LDAPS}"
                    --set "replication.externalPeers[$peer_idx].tlsSecretName=site-${other}-ca"
                    --set "replication.externalPeers[$peer_idx].bindDN=${helm_bind_dn}"
                    --set "replication.externalPeers[$peer_idx].bindPasswordSecretName=${DB_CREDENTIALS_SECRET}"
                )
            fi
            # Static podAddresses mode: no peers yet — added after pods are
            # running via configure_multus_external_peers.
            ((peer_idx++)) || true
        done

        local multus_sets=()
        if [[ -n "$MULTUS_NETWORK" ]]; then
            multus_sets=(--set "replication.network.multusNetwork=$MULTUS_NETWORK")
        fi

        hctl "$ctx" upgrade --install slapd "$PROJECT_ROOT/charts/slapd-cluster" \
            --namespace "$NAMESPACE_TESTING" --create-namespace \
            -f "$VALUES_FILE" \
            --set "images.slapd.repository=$REGISTRY/$PROJECT/slapd" \
            --set "images.slapd.tag=$GIT_TAG" \
            --set "images.init.repository=$REGISTRY/$PROJECT/slapd-init" \
            --set "images.init.tag=$GIT_TAG" \
            --set "replication.serverIDBase=${server_id_base}" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${peer_sets[@]}" \
            "${multus_sets[@]}" \
            ${HELM_VALUES_SLAPD_CLUSTER:-}
        ((site_idx++)) || true
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

setup_test_resources() {
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Applying test resources from $resource_dir..."
        kctl "$ctx" apply -n "$NAMESPACE_TESTING" -f "$resource_dir/"

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
    local local_ip="${NODE_IPS[$ctx0]}"

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

    if [[ "$MULTISITE" -eq 1 ]]; then
        local ctx1="${CONTEXTS[1]}"
        local remote_ip="${NODE_IPS[$ctx1]}"
        log "Test target: local=$ctx0 ($local_ip:$NODEPORT_LDAP), remote=$ctx1 ($remote_ip:$NODEPORT_LDAP)"
        test_env+=(
            "E2E_EXTERNAL_REPL=1"
            "E2E_REMOTE_LDAP_ADDR=${remote_ip}:${NODEPORT_LDAP}"
            "E2E_REMOTE_ADMIN_PW=$admin_pw"
        )
    else
        log "Test target: single-site $ctx0 ($local_ip:$NODEPORT_LDAP)"
    fi

    log "Running e2e tests..."
    (
        cd "$PROJECT_ROOT/tests/e2e"
        env "${test_env[@]}" go test -v ./... \
            --ginkgo.v \
            --ginkgo.timeout=15m
    )
}

# ── Teardown ─────────────────────────────────────────────────────────────────

teardown_all() {
    log "Tearing down deployment..."

    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Removing resources from $NAMESPACE_TESTING..."

        # Test resources (SlapdDatabase, SlapdSchema, readpw secret).
        if [[ -d "$resource_dir" ]]; then
            kctl "$ctx" delete -n "$NAMESPACE_TESTING" -f "$resource_dir/" \
                --ignore-not-found 2>/dev/null || true
        fi

        # SlapdCluster.
        hctl "$ctx" uninstall slapd -n "$NAMESPACE_TESTING" 2>/dev/null || true

        # NodePort services (main + per-pod).
        kctl "$ctx" delete svc slapd-external -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        for i in 0 1 2 3 4 5 6 7; do
            kctl "$ctx" delete svc "slapd-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
            kctl "$ctx" delete svc "slapd-readonly-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        done

        # Cross-trust and kubeconfig secrets.
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            kctl "$ctx" delete secret "site-${other}-ca" -n "$NAMESPACE_TESTING" --ignore-not-found || true
            kctl "$ctx" delete secret "${other}-kubeconfig" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        done

        # Remote reader RBAC (created by create-remote-kubeconfig.sh).
        kctl "$ctx" delete rolebinding slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete role slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete secret slaptain-remote-reader-token -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete sa slaptain-remote-reader -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true

        # Database credentials secret.
        kctl "$ctx" delete secret "$DB_CREDENTIALS_SECRET" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true

        # CSR (cluster-scoped) — name includes the namespace.
        kctl "$ctx" delete csr "slapd-${NAMESPACE_TESTING}-csr" --ignore-not-found 2>/dev/null || true

        # PVCs.
        kctl "$ctx" delete pvc --all -n "$NAMESPACE_TESTING" 2>/dev/null || true

        # Testing namespace.
        kctl "$ctx" delete namespace "$NAMESPACE_TESTING" --ignore-not-found || true
    done

    # Cluster-scoped resources: operator, CRDs, operator namespace.
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Removing cluster-scoped resources..."

        hctl "$ctx" uninstall slaptain-operator -n "$NAMESPACE" 2>/dev/null || true

        kctl "$ctx" delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapddatabases.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapdschemas.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true

        kctl "$ctx" delete namespace "$NAMESPACE" --ignore-not-found || true
    done

    log "Teardown complete."
}

# ── Main ─────────────────────────────────────────────────────────────────────

[[ $# -lt 2 ]] && usage

subcommand="$1"; shift
CONTEXTS=("$@")

if [[ ${#CONTEXTS[@]} -lt 1 ]]; then
    die "At least 1 kubectl context required"
fi

# MULTISITE=1 when running the cross-cluster path. Used to gate external-peer
# wiring, cross-trust CA distribution, the convergence sleep, and the
# E2E_EXTERNAL_REPL test-suite flag. N=1 collapses every per-context loop to
# a single iteration with empty peer arrays.
MULTISITE=0
if [[ ${#CONTEXTS[@]} -ge 2 ]]; then
    MULTISITE=1
fi

declare -A NODE_IPS

do_setup() {
    setup_foundation
    setup_cross_trust
    setup_remote_kubeconfigs
    setup_slapd_clusters
    wait_for_clusters_ready
    configure_multus_external_peers
    setup_nodeport_services
    setup_test_resources
}

discover_node_ips
resolve_cr_names
generate_shared_credentials  # one shared password set across all contexts

case "$subcommand" in
    setup)
        do_setup
        log ""
        log "Setup complete."
        log "  Contexts: ${CONTEXTS[*]}"
        if [[ -n "$MULTUS_NETWORK" ]]; then
            if [[ -n "$STATIC_PODADDRESSES" ]]; then
                log "  Replication network: $MULTUS_NETWORK (Multus, static podAddresses)"
            else
                log "  Replication network: $MULTUS_NETWORK (Multus, dynamic discovery)"
            fi
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
