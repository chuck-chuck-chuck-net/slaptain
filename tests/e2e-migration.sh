#!/bin/bash
# Migration e2e orchestration (ADR-010 3f).
#
# Stands up two SlapdClusters in one Kubernetes cluster:
#
#   fake-prod  — peer mode, deltaSync=false (mimics a legacy non-slaptain
#                provider that only speaks plain syncrepl). Single pod. Seeded
#                with a small DIT including operational attributes.
#   slaptain   — consumer-only mode. Single pod. externalPeers points at
#                fake-prod via in-cluster service DNS with syncMode=plain.
#
# The Ginkgo spec (tests/e2e/migration_test.go) then exercises:
#
#   1. Reads from slaptain return fake-prod's entries with preserved
#      operational attributes (entryUUID etc.).
#   2. Writes to slaptain are rejected (olcReadOnly=TRUE → unwillingToPerform).
#   3. Patching slaptain.spec.replication.mode: peer triggers in-place
#      promotion (no pod restart).
#   4. After promotion, writes to slaptain succeed.
#   5. EntryUUIDs are unchanged across the transition (no re-sync happened).
#
# Usage:
#   ./tests/e2e-migration.sh setup     [context]
#   ./tests/e2e-migration.sh test      [context]
#   ./tests/e2e-migration.sh teardown  [context]
#   ./tests/e2e-migration.sh all       [context]
set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

NAMESPACE_OPERATOR="${NAMESPACE_OPERATOR:-slaptain}"
NAMESPACE_FAKEPROD="${NAMESPACE_FAKEPROD:-fakeprod}"
NAMESPACE_SLAPTAIN="${NAMESPACE_SLAPTAIN:-slaptain-target}"
REGISTRY="${REGISTRY:-ghcr.io/chuck-chuck-chuck-net}"
PROJECT="${PROJECT:-slaptain}"
NODEPORT_SLAPTAIN="${NODEPORT_SLAPTAIN:-30389}"
SUFFIX="${SUFFIX:-dc=example,dc=org}"
SHARED_REPL_PW="${SHARED_REPL_PW:-mig-repl-$(openssl rand -hex 6 2>/dev/null || echo deadbeef)}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Image tag: git tag or short commit hash (with -dirty suffix).
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

PULL_SECRET_FILE="$SCRIPT_DIR/image-pull-secret.yaml"
PULL_SECRET_HELM_ARGS=()
if [[ -f "$PULL_SECRET_FILE" ]]; then
    PULL_SECRET_NAME=$(awk '/^  name:/{print $2; exit}' "$PULL_SECRET_FILE")
    PULL_SECRET_HELM_ARGS=(--set "imagePullSecrets[0].name=$PULL_SECRET_NAME")
fi

log() { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
die() { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 <setup|test|teardown|all> [context]
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

# ── Setup ────────────────────────────────────────────────────────────────────

setup_namespaces() {
    log "Creating namespaces..."
    for ns in "$NAMESPACE_OPERATOR" "$NAMESPACE_FAKEPROD" "$NAMESPACE_SLAPTAIN"; do
        $KUBECTL create namespace "$ns" --dry-run=client -o yaml | $KUBECTL apply -f -
        if [[ -f "$PULL_SECRET_FILE" ]]; then
            $KUBECTL apply -n "$ns" -f "$PULL_SECRET_FILE"
        fi
    done
}

setup_operator() {
    log "Installing operator..."
    $HELM upgrade --install slaptain-operator "$PROJECT_ROOT/charts/operator" \
        --namespace "$NAMESPACE_OPERATOR" --create-namespace \
        --set "image.repository=$REGISTRY/$PROJECT/operator" \
        --set "image.tag=$GIT_TAG" \
        "${PULL_SECRET_HELM_ARGS[@]}"
    $KUBECTL -n "$NAMESPACE_OPERATOR" rollout status deployment/slaptain-operator --timeout=120s
}

# Pre-create the shared replication password Secret in both namespaces. The
# bind DN must exist on fake-prod (created by its SlapdDatabase seed entry)
# and the same plaintext must be referenced by slaptain's externalPeer.
setup_shared_repl_secret() {
    log "Pre-creating shared replication credentials Secret..."
    for ns in "$NAMESPACE_FAKEPROD" "$NAMESPACE_SLAPTAIN"; do
        $KUBECTL create secret generic mig-replication-pw \
            -n "$ns" \
            --from-literal=replication-password="$SHARED_REPL_PW" \
            --from-literal=password="$SHARED_REPL_PW" \
            --dry-run=client -o yaml \
            | $KUBECTL apply -f -
    done
}

apply_fake_prod() {
    log "Deploying fake-prod SlapdCluster + SlapdDatabase..."
    cat <<EOF | $KUBECTL apply -f -
---
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: fakeprod
  namespace: $NAMESPACE_FAKEPROD
spec:
  images:
    slapd:
      repository: $REGISTRY/$PROJECT/slapd
    init:
      repository: $REGISTRY/$PROJECT/slapd-init
  replicas: 1
  logLevel: 256
  replication:
    enabled: true
    mode: peer
  ldap:
    tls:
      enabled: false
  service:
    type: ClusterIP
$(if [[ -n "${PULL_SECRET_NAME:-}" ]]; then
    echo "  imagePullSecrets:"
    echo "  - name: $PULL_SECRET_NAME"
fi)
---
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdDatabase
metadata:
  name: fakeprod-db
  namespace: $NAMESPACE_FAKEPROD
spec:
  clusterRef: fakeprod
  suffix: "$SUFFIX"
  replication:
    deltaSync: false   # fake-prod mimics a legacy plain-syncrepl provider
    ridBase: 100
  acls:
    - 'to attrs=userPassword by self write by anonymous auth by * read'
    - 'to * by * read'
  seed:
    entries:
      - |
        dn: $SUFFIX
        objectClass: top
        objectClass: dcObject
        objectClass: organization
        o: example
        dc: example
      - |
        dn: ou=People,$SUFFIX
        objectClass: organizationalUnit
        ou: People
      - |
        dn: uid=alice,ou=People,$SUFFIX
        objectClass: inetOrgPerson
        cn: Alice Source
        sn: Source
        uid: alice
        userPassword: alice-source-pw
EOF
}

apply_slaptain_consumer() {
    log "Deploying slaptain (consumer-only) SlapdCluster + SlapdDatabase..."
    # The externalPeer uses an in-cluster service DNS pointing at fake-prod's
    # ClusterIP Service (default port 389) and plain syncrepl.
    cat <<EOF | $KUBECTL apply -f -
---
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slaptain
  namespace: $NAMESPACE_SLAPTAIN
spec:
  images:
    slapd:
      repository: $REGISTRY/$PROJECT/slapd
    init:
      repository: $REGISTRY/$PROJECT/slapd-init
  replicas: 1
  logLevel: 256
  replication:
    enabled: true
    mode: consumer-only
    externalPeers:
      - name: fake-prod
        uri: "ldap://fakeprod.${NAMESPACE_FAKEPROD}.svc.cluster.local:389"
        bindDN: "cn=replication,$SUFFIX"
        bindPasswordSecretName: mig-replication-pw
        syncMode: plain
  ldap:
    tls:
      enabled: false
  service:
    type: ClusterIP
$(if [[ -n "${PULL_SECRET_NAME:-}" ]]; then
    echo "  imagePullSecrets:"
    echo "  - name: $PULL_SECRET_NAME"
fi)
---
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdDatabase
metadata:
  name: slaptain-db
  namespace: $NAMESPACE_SLAPTAIN
spec:
  clusterRef: slaptain
  suffix: "$SUFFIX"
  replication:
    deltaSync: false   # consumer pulls from a plain-syncrepl source
    ridBase: 200
EOF
}

wait_for_running() {
    local ns="$1" name="$2"
    log "[$ns] Waiting for SlapdCluster $name to reach Running..."
    local attempts=0
    while true; do
        local phase
        phase=$($KUBECTL -n "$ns" get slapdclusters.ldap.chuck-chuck-chuck.net "$name" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "$phase" == "Running" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 180 ]] && die "[$ns] $name not Running within 180s (current: $phase)"
        sleep 1
    done
}

wait_for_db_running() {
    local ns="$1" name="$2"
    log "[$ns] Waiting for SlapdDatabase $name to reach Running..."
    local attempts=0
    while true; do
        local phase
        phase=$($KUBECTL -n "$ns" get slapddatabases.ldap.chuck-chuck-chuck.net "$name" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "$phase" == "Running" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 180 ]] && die "[$ns] SlapdDatabase $name not Running within 180s (current: $phase)"
        sleep 1
    done
}

# Wait for the replication-bind DN to exist on fake-prod. It's seeded by the
# SlapdDatabase controller's ensureReplicationUser; we can't bind from
# slaptain (and so can't sync) until it's there.
wait_for_repl_user_on_fakeprod() {
    log "[$NAMESPACE_FAKEPROD] Waiting for cn=replication,$SUFFIX to be created..."
    local attempts=0
    while true; do
        # Probe via kubectl exec into the slapd pod — distroless image, no
        # shell, so use a debug container or the slapd-toolkit. Simpler: just
        # wait for SlapdDatabase to be Running, which implies the bind user
        # was added (it's step 9 in the SlapdDatabase reconcile).
        local phase
        phase=$($KUBECTL -n "$NAMESPACE_FAKEPROD" get slapddatabases.ldap.chuck-chuck-chuck.net fakeprod-db \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "$phase" == "Running" ]] && break
        ((attempts++)) || true
        [[ $attempts -ge 120 ]] && die "[$NAMESPACE_FAKEPROD] cn=replication not ready within 120s"
        sleep 1
    done
}

setup_nodeport_for_slaptain() {
    log "Creating NodePort service for slaptain (for in-test LDAP access)..."
    $KUBECTL apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: slaptain-external
  namespace: $NAMESPACE_SLAPTAIN
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: slapd
    app.kubernetes.io/instance: slaptain
  ports:
    - name: ldap
      port: 389
      targetPort: 1024
      nodePort: $NODEPORT_SLAPTAIN
EOF
}

# ── Test ─────────────────────────────────────────────────────────────────────

run_tests() {
    log "Running migration e2e tests..."

    local admin_pw repl_pw
    admin_pw=$($KUBECTL -n "$NAMESPACE_SLAPTAIN" get secret slaptain-db-credentials \
        -o jsonpath='{.data.root-password}' | base64 -d)
    repl_pw=$($KUBECTL -n "$NAMESPACE_SLAPTAIN" get secret mig-replication-pw \
        -o jsonpath='{.data.replication-password}' | base64 -d)

    local tmp_kubeconfig
    tmp_kubeconfig=$(mktemp /tmp/e2e-migration-kubeconfig.XXXXXX)
    if [[ -n "${CTX:-}" ]]; then
        kubectl config view --context="$CTX" --minify --flatten > "$tmp_kubeconfig"
    else
        kubectl config view --minify --flatten > "$tmp_kubeconfig"
    fi
    trap "rm -f '$tmp_kubeconfig'" EXIT

    (
        cd "$PROJECT_ROOT/tests/e2e"
        KUBECONFIG="$tmp_kubeconfig" \
        E2E_MIGRATION=1 \
        E2E_MIGRATION_LDAP_ADDR="${NODE_IP}:${NODEPORT_SLAPTAIN}" \
        E2E_MIGRATION_ADMIN_PW="$admin_pw" \
        E2E_MIGRATION_NS_SLAPTAIN="$NAMESPACE_SLAPTAIN" \
        E2E_MIGRATION_NS_FAKEPROD="$NAMESPACE_FAKEPROD" \
        E2E_MIGRATION_SUFFIX="$SUFFIX" \
        go test -v ./... --ginkgo.v --ginkgo.timeout=10m --ginkgo.label-filter=migration
    )
}

# ── Teardown ─────────────────────────────────────────────────────────────────

teardown_all() {
    log "Tearing down migration deployment..."
    for ns in "$NAMESPACE_SLAPTAIN" "$NAMESPACE_FAKEPROD"; do
        $KUBECTL delete slapddatabases.ldap.chuck-chuck-chuck.net --all -n "$ns" --ignore-not-found || true
        $KUBECTL delete slapdclusters.ldap.chuck-chuck-chuck.net --all -n "$ns" --ignore-not-found || true
        $KUBECTL delete pvc --all -n "$ns" --ignore-not-found || true
    done
    $HELM uninstall slaptain-operator -n "$NAMESPACE_OPERATOR" 2>/dev/null || true
    $KUBECTL delete crd slapdclusters.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
    $KUBECTL delete crd slapddatabases.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
    $KUBECTL delete crd slapdschemas.ldap.chuck-chuck-chuck.net --ignore-not-found 2>/dev/null || true
    for ns in "$NAMESPACE_SLAPTAIN" "$NAMESPACE_FAKEPROD" "$NAMESPACE_OPERATOR"; do
        $KUBECTL delete namespace "$ns" --ignore-not-found || true
    done
    log "Teardown complete."
}

# ── Main ─────────────────────────────────────────────────────────────────────

[[ $# -lt 1 ]] && usage
subcommand="$1"; shift
CTX="${1:-}"

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
        setup_namespaces
        setup_operator
        setup_shared_repl_secret
        apply_fake_prod
        wait_for_running "$NAMESPACE_FAKEPROD" fakeprod
        wait_for_db_running "$NAMESPACE_FAKEPROD" fakeprod-db
        wait_for_repl_user_on_fakeprod
        apply_slaptain_consumer
        wait_for_running "$NAMESPACE_SLAPTAIN" slaptain
        wait_for_db_running "$NAMESPACE_SLAPTAIN" slaptain-db
        setup_nodeport_for_slaptain
        log "Setup complete."
        ;;
    test)
        discover_node_ip
        run_tests
        ;;
    teardown)
        teardown_all
        ;;
    all)
        discover_node_ip
        setup_namespaces
        setup_operator
        setup_shared_repl_secret
        apply_fake_prod
        wait_for_running "$NAMESPACE_FAKEPROD" fakeprod
        wait_for_db_running "$NAMESPACE_FAKEPROD" fakeprod-db
        wait_for_repl_user_on_fakeprod
        apply_slaptain_consumer
        wait_for_running "$NAMESPACE_SLAPTAIN" slaptain
        wait_for_db_running "$NAMESPACE_SLAPTAIN" slaptain-db
        setup_nodeport_for_slaptain
        run_tests
        teardown_all
        ;;
    *)
        usage
        ;;
esac
