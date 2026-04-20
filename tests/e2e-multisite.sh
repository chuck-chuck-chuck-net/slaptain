#!/bin/bash
# Multi-site e2e test orchestration.
# Deploys the slaptain operator + SlapdCluster + SlapdSchema + SlapdDatabase
# across N Kubernetes clusters, configures cross-cluster delta-syncrepl, and
# runs the full e2e suite (including external replication tests).
#
# Usage:
#   ./tests/e2e-multisite.sh setup   ctx1 ctx2 [ctx3 ...]
#   ./tests/e2e-multisite.sh test    ctx1 ctx2 [ctx3 ...]
#   ./tests/e2e-multisite.sh teardown ctx1 ctx2 [ctx3 ...]
#   ./tests/e2e-multisite.sh all     ctx1 ctx2 [ctx3 ...]
#
# Prerequisites: N Kubernetes clusters reachable via kubectl contexts.
# Container images must be available in a registry accessible from all clusters.
set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

NAMESPACE="${NAMESPACE:-slaptain}"
NAMESPACE_TESTING="${NAMESPACE_TESTING:-slaptain-testing}"
NODEPORT_LDAP="${NODEPORT_LDAP:-30389}"
NODEPORT_LDAPS="${NODEPORT_LDAPS:-30636}"
NODEPORT_POD_BASE="${NODEPORT_POD_BASE:-30400}"
NODEPORT_RO_POD_BASE="${NODEPORT_RO_POD_BASE:-30410}"
REGISTRY="${REGISTRY:-ghcr.io/chuck-chuck-chuck-net}"
PROJECT="${PROJECT:-slaptain}"
TEST_RESOURCES="${TEST_RESOURCES:-example}"

# Multus replication network (ADR-007). When MULTUS_NETWORK is set, cross-site
# replication uses a dedicated Multus network instead of NodePort services.
# NodePort services are still created for test runner connectivity.
#
# The script deploys SlapdClusters without externalPeers first, waits for pods
# to get Multus IPs (discovered from pod annotations), then patches the CRs
# to add externalPeers with the discovered podAddresses. No IPs need to be
# known in advance — the operator sets tls_reqcert=allow on syncrepl stanzas
# for IP-based providers, so TLS certs don't need Multus IP SANs.
#
# MULTUS_NETWORK: NAD reference, e.g. "infra/replication-net" or "replication-net"
MULTUS_NETWORK="${MULTUS_NETWORK:-}"

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
Usage: $0 <setup|test|teardown|all> ctx1 ctx2 [ctx3 ...]

Subcommands:
  setup     Deploy operator, SlapdCluster, test resources on all clusters
  test      Run e2e tests (including external replication)
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
                         IPs are discovered from running pods after deployment —
                         no IPs need to be known in advance.
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
configure_multus_external_peers() {
    [[ -z "$MULTUS_NETWORK" ]] && return

    # Helm --set treats commas as value separators — escape for LDAP DNs.
    local helm_suffix="${DB_SUFFIX//,/\\,}"
    local helm_bind_dn="cn=replication\\,${helm_suffix}"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Configuring externalPeers with Multus podAddresses..."

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
            -f "$PROJECT_ROOT/tests/values.slapd.yaml" \
            --set "images.slapd.repository=$REGISTRY/$PROJECT/slapd" \
            --set "images.slapd.tag=$GIT_TAG" \
            --set "images.init.repository=$REGISTRY/$PROJECT/slapd-init" \
            --set "images.init.tag=$GIT_TAG" \
            --set "replication.network.multusNetwork=$MULTUS_NETWORK" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${peer_sets[@]}" \
            ${HELM_VALUES_SLAPD_CLUSTER:-}
    done
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
        hctl "$ctx" upgrade --install slaptain-operator "$PROJECT_ROOT/charts/operator" \
            --namespace "$NAMESPACE" --create-namespace \
            --set "image.repository=$REGISTRY/$PROJECT/operator" \
            --set "image.tag=$GIT_TAG" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            ${HELM_VALUES:-}
    done
}

setup_cross_trust() {
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

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Installing SlapdCluster..."

        # In Multus mode, deploy WITHOUT externalPeers. The pods need to come
        # up first so we can discover their Multus IPs. externalPeers are added
        # later via configure_multus_external_peers.
        local peer_sets=()
        if [[ -z "$MULTUS_NETWORK" ]]; then
            local peer_idx=0
            for other in "${CONTEXTS[@]}"; do
                [[ "$other" == "$ctx" ]] && continue
                peer_sets+=(
                    --set "replication.externalPeers[$peer_idx].name=site-${other}"
                    --set "replication.externalPeers[$peer_idx].uri=ldaps://${NODE_IPS[$other]}:${NODEPORT_LDAPS}"
                    --set "replication.externalPeers[$peer_idx].tlsSecretName=site-${other}-ca"
                    --set "replication.externalPeers[$peer_idx].bindDN=${helm_bind_dn}"
                    --set "replication.externalPeers[$peer_idx].bindPasswordSecretName=${DB_CREDENTIALS_SECRET}"
                )
                ((peer_idx++)) || true
            done
        fi

        local multus_sets=()
        if [[ -n "$MULTUS_NETWORK" ]]; then
            multus_sets=(--set "replication.network.multusNetwork=$MULTUS_NETWORK")
        fi

        hctl "$ctx" upgrade --install slapd "$PROJECT_ROOT/charts/slapd-cluster" \
            --namespace "$NAMESPACE_TESTING" --create-namespace \
            -f "$PROJECT_ROOT/tests/values.slapd.yaml" \
            --set "images.slapd.repository=$REGISTRY/$PROJECT/slapd" \
            --set "images.slapd.tag=$GIT_TAG" \
            --set "images.init.repository=$REGISTRY/$PROJECT/slapd-init" \
            --set "images.init.tag=$GIT_TAG" \
            "${PULL_SECRET_HELM_ARGS[@]}" \
            "${peer_sets[@]}" \
            "${multus_sets[@]}" \
            ${HELM_VALUES_SLAPD_CLUSTER:-}
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

    log "Waiting 30s for cross-cluster replication convergence..."
    sleep 30
}

# ── Test ─────────────────────────────────────────────────────────────────────

run_tests() {
    local ctx0="${CONTEXTS[0]}"
    local ctx1="${CONTEXTS[1]}"

    log "Reading admin password from $ctx0..."
    local admin_pw
    admin_pw=$(kctl "$ctx0" -n "$NAMESPACE_TESTING" get secret "$DB_CREDENTIALS_SECRET" \
        -o jsonpath='{.data.root-password}' | base64 -d)

    local local_ip="${NODE_IPS[$ctx0]}"
    local remote_ip="${NODE_IPS[$ctx1]}"
    log "Test target: local=$ctx0 ($local_ip:$NODEPORT_LDAP), remote=$ctx1 ($remote_ip:$NODEPORT_LDAP)"

    # Create a temp kubeconfig scoped to ctx0 so the Go test suite's k8s client
    # connects to the right cluster without mutating the user's kubeconfig.
    local tmp_kubeconfig
    tmp_kubeconfig=$(mktemp /tmp/e2e-multisite-kubeconfig.XXXXXX)
    kubectl config view --context="$ctx0" --minify --flatten > "$tmp_kubeconfig"
    trap "rm -f '$tmp_kubeconfig'" EXIT

    log "Running e2e tests..."
    (
        cd "$PROJECT_ROOT/tests/e2e"
        KUBECONFIG="$tmp_kubeconfig" \
        NAMESPACE_TESTING="$NAMESPACE_TESTING" \
        LDAP_ADDR="${local_ip}:${NODEPORT_LDAP}" \
        E2E_NODE_IP="${local_ip}" \
        E2E_POD_NODEPORT_BASE="${NODEPORT_POD_BASE}" \
        E2E_RO_POD_NODEPORT_BASE="${NODEPORT_RO_POD_BASE}" \
        E2E_EXTERNAL_REPL=1 \
        E2E_REMOTE_LDAP_ADDR="${remote_ip}:${NODEPORT_LDAP}" \
        E2E_REMOTE_ADMIN_PW="$admin_pw" \
        DB_CR_NAME="$DB_CR_NAME" \
        DB_CREDENTIALS_SECRET="$DB_CREDENTIALS_SECRET" \
        SCHEMA_CR_NAME="$SCHEMA_CR_NAME" \
        READPW_OU="${READPW_OU:-ServiceAccounts}" \
        go test -v ./... --ginkgo.v --ginkgo.timeout=15m
    )
}

# ── Teardown ─────────────────────────────────────────────────────────────────

teardown_all() {
    log "Tearing down multi-site deployment..."

    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Removing resources..."

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

        # Cross-trust secrets.
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            kctl "$ctx" delete secret "site-${other}-ca" -n "$NAMESPACE_TESTING" --ignore-not-found || true
        done

        # Database credentials secret.
        kctl "$ctx" delete secret "$DB_CREDENTIALS_SECRET" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true

        # Operator.
        hctl "$ctx" uninstall slaptain-operator -n "$NAMESPACE" 2>/dev/null || true

        # CSR (cluster-scoped).
        kctl "$ctx" delete csr "slapd-${NAMESPACE_TESTING}-csr" --ignore-not-found 2>/dev/null || true

        # CRDs (cluster-scoped, left behind by helm).
        kctl "$ctx" delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapddatabases.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete crd slapdschemas.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true

        # PVCs.
        kctl "$ctx" delete pvc --all -n "$NAMESPACE_TESTING" 2>/dev/null || true

        # Namespaces.
        kctl "$ctx" delete namespace "$NAMESPACE_TESTING" --ignore-not-found || true
        kctl "$ctx" delete namespace "$NAMESPACE" --ignore-not-found || true
    done

    log "Teardown complete."
}

# ── Main ─────────────────────────────────────────────────────────────────────

[[ $# -lt 3 ]] && usage

subcommand="$1"; shift
CONTEXTS=("$@")

if [[ ${#CONTEXTS[@]} -lt 2 ]]; then
    die "At least 2 kubectl contexts required (got ${#CONTEXTS[@]})"
fi

declare -A NODE_IPS

case "$subcommand" in
    setup)
        discover_node_ips
        resolve_cr_names
        generate_shared_credentials
        setup_foundation
        setup_cross_trust
        setup_slapd_clusters
        wait_for_clusters_ready
        discover_multus_ips
        configure_multus_external_peers
        setup_nodeport_services
        setup_test_resources
        log "Setup complete. Clusters: ${CONTEXTS[*]}"
        [[ -n "$MULTUS_NETWORK" ]] && log "Replication network: $MULTUS_NETWORK (Multus)"
        ;;
    test)
        discover_node_ips
        resolve_cr_names
        run_tests
        ;;
    teardown)
        resolve_cr_names
        teardown_all
        ;;
    all)
        discover_node_ips
        resolve_cr_names
        generate_shared_credentials
        setup_foundation
        setup_cross_trust
        setup_slapd_clusters
        wait_for_clusters_ready
        discover_multus_ips
        configure_multus_external_peers
        setup_nodeport_services
        setup_test_resources
        run_tests
        teardown_all
        ;;
    *)
        usage
        ;;
esac
