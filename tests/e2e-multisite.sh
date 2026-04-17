#!/bin/bash
# Multi-site e2e test orchestration.
# Deploys the slaptain operator + SlapdCluster + slapd-test across N Kubernetes
# clusters, configures cross-cluster delta-syncrepl, and runs the full e2e suite.
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
LDAP_DOMAIN="${LDAP_DOMAIN:-dc=chuck-chuck-chuck,dc=net}"
NODEPORT_LDAP="${NODEPORT_LDAP:-30389}"
NODEPORT_LDAPS="${NODEPORT_LDAPS:-30636}"
NODEPORT_POD_BASE="${NODEPORT_POD_BASE:-30400}"
NODEPORT_RO_POD_BASE="${NODEPORT_RO_POD_BASE:-30410}"
REGISTRY="${REGISTRY:-ghcr.io/chuck-chuck-chuck-net}"
PROJECT="${PROJECT:-slaptain}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

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
  setup     Deploy operator, SlapdCluster, slapd-test on all clusters
  test      Run e2e tests (including external replication)
  teardown  Remove everything created by setup
  all       setup + test + teardown

Environment variables (with defaults):
  NAMESPACE            = $NAMESPACE
  NAMESPACE_TESTING    = $NAMESPACE_TESTING
  LDAP_DOMAIN          = $LDAP_DOMAIN
  NODEPORT_LDAP        = $NODEPORT_LDAP
  NODEPORT_LDAPS       = $NODEPORT_LDAPS
  REGISTRY             = $REGISTRY
  PROJECT              = $PROJECT
  HELM_VALUES          = operator chart values (use absolute paths)
  HELM_VALUES_SLAPD_CLUSTER = slapd-cluster chart values (use absolute paths)
  HELM_VALUES_SLAPD_TESTING = slapd-test chart values (use absolute paths)
EOF
    exit 1
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

# ── Setup phases ─────────────────────────────────────────────────────────────

generate_passwords() {
    log "Generating shared credentials..."
    ADMIN_PW=$(openssl rand -base64 18)
    ROOT_PW=$(openssl rand -base64 18)
    REPL_PW=$(openssl rand -base64 24)
}

setup_foundation() {
    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Creating namespaces..."
        kctl "$ctx" create namespace "$NAMESPACE" --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -
        kctl "$ctx" create namespace "$NAMESPACE_TESTING" --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -

        log "[$ctx] Creating shared credentials secret..."
        kctl "$ctx" create secret generic slapd-credentials \
            -n "$NAMESPACE_TESTING" \
            --from-literal=admin-password="$ADMIN_PW" \
            --from-literal=root-password="$ROOT_PW" \
            --from-literal=replication-password="$REPL_PW" \
            --dry-run=client -o yaml \
            | kctl "$ctx" apply -f -

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

        log "[$ctx] Installing operator..."
        hctl "$ctx" upgrade --install slaptain-operator "$PROJECT_ROOT/charts/operator" \
            --namespace "$NAMESPACE" --create-namespace \
            --set "image.repository=$REGISTRY/$PROJECT/operator" \
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
    local helm_domain="${LDAP_DOMAIN//,/\\,}"
    local helm_bind_dn="cn=replication\\,${helm_domain}"

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Installing SlapdCluster..."

        # Build --set args for externalPeers (all OTHER contexts).
        local peer_sets=()
        local peer_idx=0
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            peer_sets+=(
                --set "replication.externalPeers[$peer_idx].name=site-${other}"
                --set "replication.externalPeers[$peer_idx].uri=ldaps://${NODE_IPS[$other]}:${NODEPORT_LDAPS}"
                --set "replication.externalPeers[$peer_idx].tlsSecretName=site-${other}-ca"
                --set "replication.externalPeers[$peer_idx].bindDN=${helm_bind_dn}"
                --set "replication.externalPeers[$peer_idx].bindPasswordSecretName=slapd-credentials"
            )
            ((peer_idx++)) || true
        done

        hctl "$ctx" upgrade --install slapd "$PROJECT_ROOT/charts/slapd-cluster" \
            --namespace "$NAMESPACE_TESTING" --create-namespace \
            --set "credentials.existingSecret=slapd-credentials" \
            --set "replication.enabled=true" \
            --set "ldap.domain=${helm_domain}" \
            "${peer_sets[@]}" \
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

        # Per-pod NodePort services for direct pod access (replaces kubectl port-forward).
        # Uses statefulset.kubernetes.io/pod-name label to target individual pods.
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

setup_slapd_test() {
    local ctx="${CONTEXTS[0]}"
    log "[$ctx] Installing slapd-test (first site only)..."
    hctl "$ctx" upgrade --install slapd-test "$PROJECT_ROOT/charts/slapd-test" \
        --namespace "$NAMESPACE_TESTING" --create-namespace \
        ${HELM_VALUES_SLAPD_TESTING:-}
}

wait_for_ready() {
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
    done

    local ctx="${CONTEXTS[0]}"
    log "[$ctx] Waiting for bootstrap job to complete..."
    kctl "$ctx" -n "$NAMESPACE_TESTING" wait job/slapd-test \
        --for=condition=complete --timeout=300s

    log "Waiting 30s for cross-cluster replication convergence..."
    sleep 30
}

# ── Test ─────────────────────────────────────────────────────────────────────

run_tests() {
    local ctx0="${CONTEXTS[0]}"
    local ctx1="${CONTEXTS[1]}"

    log "Reading admin password from $ctx0..."
    local admin_pw
    admin_pw=$(kctl "$ctx0" -n "$NAMESPACE_TESTING" get secret slapd-credentials \
        -o jsonpath='{.data.admin-password}' | base64 -d)

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
        go test -v ./... --ginkgo.v --ginkgo.timeout=15m
    )
}

# ── Teardown ─────────────────────────────────────────────────────────────────

teardown_all() {
    log "Tearing down multi-site deployment..."

    for ctx in "${CONTEXTS[@]}"; do
        log "[$ctx] Removing resources..."

        # slapd-test (first context only)
        if [[ "$ctx" == "${CONTEXTS[0]}" ]]; then
            hctl "$ctx" uninstall slapd-test -n "$NAMESPACE_TESTING" 2>/dev/null || true
        fi

        # SlapdCluster
        hctl "$ctx" uninstall slapd -n "$NAMESPACE_TESTING" 2>/dev/null || true

        # NodePort services (main + per-pod)
        kctl "$ctx" delete svc -n "$NAMESPACE_TESTING" -l '!app.kubernetes.io/managed-by' \
            --field-selector metadata.name!=kubernetes --ignore-not-found 2>/dev/null || true
        kctl "$ctx" delete svc slapd-external -n "$NAMESPACE_TESTING" --ignore-not-found || true
        for i in 0 1 2 3 4 5 6 7; do
            kctl "$ctx" delete svc "slapd-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
            kctl "$ctx" delete svc "slapd-readonly-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        done

        # Cross-trust secrets
        for other in "${CONTEXTS[@]}"; do
            [[ "$other" == "$ctx" ]] && continue
            kctl "$ctx" delete secret "site-${other}-ca" -n "$NAMESPACE_TESTING" --ignore-not-found || true
        done

        # Operator
        hctl "$ctx" uninstall slaptain-operator -n "$NAMESPACE" 2>/dev/null || true

        # Credentials secret
        kctl "$ctx" delete secret slapd-credentials -n "$NAMESPACE_TESTING" --ignore-not-found || true

        # CSR (cluster-scoped)
        kctl "$ctx" delete csr "slapd-${NAMESPACE_TESTING}-csr" --ignore-not-found || true

        # CRD (cluster-scoped, left behind by helm)
        kctl "$ctx" delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found || true

        # Namespaces
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
        generate_passwords
        setup_foundation
        setup_cross_trust
        setup_slapd_clusters
        setup_nodeport_services
        setup_slapd_test
        wait_for_ready
        log "Setup complete. Clusters: ${CONTEXTS[*]}"
        ;;
    test)
        discover_node_ips
        run_tests
        ;;
    teardown)
        teardown_all
        ;;
    all)
        discover_node_ips
        generate_passwords
        setup_foundation
        setup_cross_trust
        setup_slapd_clusters
        setup_nodeport_services
        setup_slapd_test
        wait_for_ready
        run_tests
        teardown_all
        ;;
    *)
        usage
        ;;
esac
