#!/bin/bash
# Single-site e2e test orchestration.
# Deploys the slaptain operator + SlapdCluster + SlapdSchema + SlapdDatabase,
# creates NodePort services for direct access, and runs the e2e suite.
#
# Usage:
#   ./tests/e2e-singlesite.sh setup   [context]
#   ./tests/e2e-singlesite.sh test    [context]
#   ./tests/e2e-singlesite.sh teardown [context]
#   ./tests/e2e-singlesite.sh all     [context]
#
# Context defaults to current kubectl context if not specified.
# Set TEST_RESOURCES=example (default) or TEST_RESOURCES=lab.
set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

NAMESPACE="${NAMESPACE:-slaptain}"
NAMESPACE_TESTING="${NAMESPACE_TESTING:-slaptain-testing}"
NODEPORT_LDAP="${NODEPORT_LDAP:-30389}"
NODEPORT_POD_BASE="${NODEPORT_POD_BASE:-30400}"
NODEPORT_RO_POD_BASE="${NODEPORT_RO_POD_BASE:-30410}"
TEST_RESOURCES="${TEST_RESOURCES:-example}"

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

# ── Helpers ──────────────────────────────────────────────────────────────────

log() { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
die() { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 <setup|test|teardown|all> [context]

Subcommands:
  setup     Deploy operator, SlapdCluster, test resources, NodePort services
  test      Run e2e tests via NodePort (no port-forward)
  teardown  Remove everything created by setup
  all       setup + test + teardown

Environment variables (with defaults):
  NAMESPACE            = $NAMESPACE
  NAMESPACE_TESTING    = $NAMESPACE_TESTING
  NODEPORT_LDAP        = $NODEPORT_LDAP
  NODEPORT_POD_BASE    = $NODEPORT_POD_BASE
  TEST_RESOURCES       = $TEST_RESOURCES  (example or lab)
  HELM_VALUES_SLAPD_CLUSTER = extra -f flags for slapd-cluster chart
EOF
    exit 1
}

# ── Discovery ────────────────────────────────────────────────────────────────

discover_node_ip() {
    NODE_IP=$($KUBECTL get nodes \
        -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
    [[ -z "$NODE_IP" ]] && die "Could not discover node IP"
    log "Node IP: $NODE_IP"
}

# ── CR name resolution from test resources ───────────────────────────────────

resolve_cr_names() {
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"
    [[ -d "$resource_dir" ]] || die "Test resources not found: $resource_dir"

    # Extract CR names from the YAML files (look for "name:" under "metadata:").
    DB_CR_NAME=$(awk '/^kind: SlapdDatabase/{found=1} found && /^  name:/{print $2; exit}' \
        "$resource_dir/database.yaml")
    SCHEMA_CR_NAME=$(awk '/^kind: SlapdSchema/{found=1} found && /^  name:/{print $2; exit}' \
        "$resource_dir/schema.yaml")

    [[ -z "$DB_CR_NAME" ]] && die "Could not extract SlapdDatabase name from $resource_dir/database.yaml"
    [[ -z "$SCHEMA_CR_NAME" ]] && die "Could not extract SlapdSchema name from $resource_dir/schema.yaml"

    DB_CREDENTIALS_SECRET="${DB_CR_NAME}-credentials"
    log "Database CR: $DB_CR_NAME, Schema CR: $SCHEMA_CR_NAME"
    log "Credentials secret: $DB_CREDENTIALS_SECRET"
}

# ── Setup ────────────────────────────────────────────────────────────────────

setup_operator() {
    log "Creating namespaces..."
    $KUBECTL create namespace "$NAMESPACE" --dry-run=client -o yaml | $KUBECTL apply -f -
    $KUBECTL create namespace "$NAMESPACE_TESTING" --dry-run=client -o yaml | $KUBECTL apply -f -

    log "Installing operator (tag: $GIT_TAG)..."
    $HELM upgrade --install slaptain-operator "$PROJECT_ROOT/charts/operator" \
        --namespace "$NAMESPACE" --create-namespace \
        --set "image.repository=ghcr.io/chuck-chuck-chuck-net/slaptain/operator" \
        --set "image.tag=$GIT_TAG" \
        ${HELM_VALUES:-}

    log "Waiting for operator deployment..."
    $KUBECTL -n "$NAMESPACE" rollout status deployment/slaptain-operator --timeout=120s
}

setup_tls() {
    log "Generating TLS certificate (IP SAN: $NODE_IP)..."
    local gencert_args=(-n "$NAMESPACE_TESTING" -t slapd -s slapd -H slapd-headless -i "$NODE_IP")
    if [[ -n "${CTX:-}" ]]; then
        gencert_args=(-c "$CTX" "${gencert_args[@]}")
    fi
    (cd "$SCRIPT_DIR" && ./gencert.sh "${gencert_args[@]}" slapd-tls)
}

setup_cluster() {
    log "Installing SlapdCluster (tag: $GIT_TAG)..."
    $HELM upgrade --install slapd "$PROJECT_ROOT/charts/slapd-cluster" \
        --namespace "$NAMESPACE_TESTING" --create-namespace \
        -f "$PROJECT_ROOT/tests/values.slapd.yaml" \
        --set "images.slapd.tag=$GIT_TAG" \
        --set "images.init.tag=$GIT_TAG" \
        ${HELM_VALUES_SLAPD_CLUSTER:-}

    log "Waiting for StatefulSet slapd..."
    local attempts=0
    while ! $KUBECTL -n "$NAMESPACE_TESTING" get statefulset/slapd &>/dev/null; do
        ((attempts++)) || true
        [[ $attempts -ge 60 ]] && die "StatefulSet slapd did not appear within 60s"
        sleep 1
    done
    $KUBECTL -n "$NAMESPACE_TESTING" rollout status statefulset/slapd --timeout=300s

    log "Waiting for SlapdCluster to reach Running phase..."
    attempts=0
    while true; do
        local phase
        phase=$($KUBECTL -n "$NAMESPACE_TESTING" get slapdclusters.ldap.chuck-chuck-chuck.net slapd \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "$phase" == "Running" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 180 ]] && die "SlapdCluster did not reach Running within 180s (current: $phase)"
        sleep 1
    done
    log "SlapdCluster is Running."
}

setup_test_resources() {
    local resource_dir="$PROJECT_ROOT/tests/resources/$TEST_RESOURCES"
    log "Applying test resources from $resource_dir..."
    $KUBECTL apply -n "$NAMESPACE_TESTING" -f "$resource_dir/"

    log "Waiting for SlapdDatabase $DB_CR_NAME to reach Running..."
    local attempts=0
    while true; do
        local phase
        phase=$($KUBECTL -n "$NAMESPACE_TESTING" get slapddatabases.ldap.chuck-chuck-chuck.net "$DB_CR_NAME" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "$phase" == "Running" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 180 ]] && die "SlapdDatabase did not reach Running within 180s (current: $phase)"
        sleep 1
    done

    log "Waiting for SlapdSchema $SCHEMA_CR_NAME to be applied..."
    attempts=0
    while true; do
        local applied
        applied=$($KUBECTL -n "$NAMESPACE_TESTING" get slapdschemas.ldap.chuck-chuck-chuck.net "$SCHEMA_CR_NAME" \
            -o jsonpath='{.status.applied}' 2>/dev/null || echo "")
        [[ "$applied" == "true" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 120 ]] && die "SlapdSchema was not applied within 120s"
        sleep 1
    done
    log "Test resources ready."
}

setup_nodeport_services() {
    log "Creating NodePort service (ldap:$NODEPORT_LDAP)..."
    $KUBECTL apply -f - <<EOF
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
EOF

    log "Creating per-pod NodePort services..."
    local replicas
    replicas=$($KUBECTL -n "$NAMESPACE_TESTING" get statefulset/slapd \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 3)
    for i in $(seq 0 $((replicas - 1))); do
        local np=$((NODEPORT_POD_BASE + i))
        $KUBECTL apply -f - <<EOF
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
    ro_replicas=$($KUBECTL -n "$NAMESPACE_TESTING" get statefulset/slapd-readonly \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)
    for i in $(seq 0 $((ro_replicas - 1))); do
        local np=$((NODEPORT_RO_POD_BASE + i))
        $KUBECTL apply -f - <<EOF
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
}

# ── Test ─────────────────────────────────────────────────────────────────────

run_tests() {
    log "Running e2e tests via NodePort ($NODE_IP:$NODEPORT_LDAP)..."

    # Scoped kubeconfig for the Go test suite.
    local tmp_kubeconfig
    tmp_kubeconfig=$(mktemp /tmp/e2e-singlesite-kubeconfig.XXXXXX)
    if [[ -n "${CTX:-}" ]]; then
        kubectl config view --context="$CTX" --minify --flatten > "$tmp_kubeconfig"
    else
        kubectl config view --minify --flatten > "$tmp_kubeconfig"
    fi
    trap "rm -f '$tmp_kubeconfig'" EXIT

    (
        cd "$PROJECT_ROOT/tests/e2e"
        KUBECONFIG="$tmp_kubeconfig" \
        NAMESPACE_TESTING="$NAMESPACE_TESTING" \
        LDAP_ADDR="${NODE_IP}:${NODEPORT_LDAP}" \
        E2E_NODE_IP="${NODE_IP}" \
        E2E_POD_NODEPORT_BASE="${NODEPORT_POD_BASE}" \
        E2E_RO_POD_NODEPORT_BASE="${NODEPORT_RO_POD_BASE}" \
        DB_CR_NAME="$DB_CR_NAME" \
        DB_CREDENTIALS_SECRET="$DB_CREDENTIALS_SECRET" \
        SCHEMA_CR_NAME="$SCHEMA_CR_NAME" \
        READPW_OU="${READPW_OU:-ServiceAccounts}" \
        go test -v ./... --ginkgo.v --ginkgo.timeout=10m
    )
}

# ── Teardown ─────────────────────────────────────────────────────────────────

teardown_all() {
    log "Tearing down single-site deployment..."

    # Test resources.
    $KUBECTL delete -n "$NAMESPACE_TESTING" -f "$PROJECT_ROOT/tests/resources/$TEST_RESOURCES/" \
        --ignore-not-found 2>/dev/null || true

    # SlapdCluster.
    $HELM uninstall slapd -n "$NAMESPACE_TESTING" 2>/dev/null || true

    # NodePort services.
    $KUBECTL delete svc slapd-external -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
    for i in 0 1 2 3 4 5 6 7; do
        $KUBECTL delete svc "slapd-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
        $KUBECTL delete svc "slapd-readonly-pod-$i" -n "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
    done

    # PVCs.
    $KUBECTL delete pvc --all -n "$NAMESPACE_TESTING" 2>/dev/null || true

    # Operator.
    $HELM uninstall slaptain-operator -n "$NAMESPACE" 2>/dev/null || true

    # CRDs.
    $KUBECTL delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
    $KUBECTL delete crd slapddatabases.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
    $KUBECTL delete crd slapdschemas.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true

    # CSR.
    $KUBECTL delete csr "slapd-${NAMESPACE_TESTING}-csr" --ignore-not-found 2>/dev/null || true

    # Namespaces.
    $KUBECTL delete namespace "$NAMESPACE_TESTING" --ignore-not-found 2>/dev/null || true
    $KUBECTL delete namespace "$NAMESPACE" --ignore-not-found 2>/dev/null || true

    log "Teardown complete."
}

# ── Main ─────────────────────────────────────────────────────────────────────

[[ $# -lt 1 ]] && usage

subcommand="$1"; shift
CTX="${1:-}"

# Set up kubectl/helm with optional context.
if [[ -n "$CTX" ]]; then
    KUBECTL="kubectl --context $CTX"
    HELM="helm --kube-context $CTX"
else
    KUBECTL="kubectl"
    HELM="helm"
fi

case "$subcommand" in
    setup)
        discover_node_ip
        resolve_cr_names
        setup_operator
        setup_tls
        setup_cluster
        setup_test_resources
        setup_nodeport_services
        log "Setup complete."
        ;;
    test)
        discover_node_ip
        resolve_cr_names
        run_tests
        ;;
    teardown)
        teardown_all
        ;;
    all)
        discover_node_ip
        resolve_cr_names
        setup_operator
        setup_tls
        setup_cluster
        setup_test_resources
        setup_nodeport_services
        run_tests
        teardown_all
        ;;
    *)
        usage
        ;;
esac
