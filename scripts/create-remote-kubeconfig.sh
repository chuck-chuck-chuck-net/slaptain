#!/bin/bash
# Create kubeconfig Secrets for cross-site SlapdCluster peer discovery (ADR-007).
#
# For each site pair, creates RBAC on the remote cluster and deploys a kubeconfig
# Secret on the local cluster. The operator uses this kubeconfig to query the
# remote k8s API over the replication network and discover pod Multus IPs.
#
# Usage:
#   ./scripts/create-remote-kubeconfig.sh [options] ctx1=API1 ctx2=API2 [ctx3=API3 ...]
#
# Options:
#   -n, --namespace NS    Namespace on all clusters (default: slaptain)
#   -h, --help            Show this help
#
# Each argument is CONTEXT=API_URL where API_URL is the k8s API server address
# on the replication network (e.g., https://192.168.99.1:6443).
#
# Example (two sites):
#   ./scripts/create-remote-kubeconfig.sh siteA=https://192.168.99.1:6443 siteB=https://192.168.99.2:6443
#
# This creates:
#   - RBAC on both clusters (ServiceAccount + Role + RoleBinding)
#   - Secret "siteB-kubeconfig" on siteA (kubeconfig for siteB's API)
#   - Secret "siteA-kubeconfig" on siteB (kubeconfig for siteA's API)
#
# Then reference in SlapdCluster CR:
#   externalPeers:
#     - name: site-b
#       discovery:
#         kubeconfigSecret:
#           name: siteB-kubeconfig
set -euo pipefail

NAMESPACE="slaptain"
SA_NAME="slaptain-remote-reader"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 [options] ctx1=API1 ctx2=API2 [ctx3=API3 ...]

Creates cross-site kubeconfig Secrets for SlapdCluster peer discovery.
Each site gets a Secret per remote site, used by ExternalPeer.Discovery.

Arguments:
  CONTEXT=API_URL   kubectl context and its k8s API address on the
                    replication network (e.g., siteA=https://192.168.99.1:6443)

Options:
  -n, --namespace NS    Namespace on all clusters (default: slaptain)
  -h, --help            Show this help

Examples:
  $0 siteA=https://192.168.99.1:6443 siteB=https://192.168.99.2:6443
  $0 -n slaptain-testing s1=https://192.168.99.1:6443 s2=https://192.168.99.2:6443
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        -h|--help)      usage ;;
        -*)             die "Unknown option: $1" ;;
        *)              break ;;
    esac
done

[ $# -lt 2 ] && { echo "At least 2 context=api pairs required." >&2; usage; }

# Parse context=api pairs.
declare -A SITE_APIS
CONTEXTS=()
for arg in "$@"; do
    if [[ "$arg" != *=* ]]; then
        die "Invalid argument '$arg'. Expected CONTEXT=API_URL (e.g., siteA=https://192.168.99.1:6443)"
    fi
    ctx="${arg%%=*}"
    api="${arg#*=}"
    SITE_APIS[$ctx]="$api"
    CONTEXTS+=("$ctx")
done

# ── For each site: create RBAC and extract token ───────────────────────────

declare -A SITE_TOKENS

for ctx in "${CONTEXTS[@]}"; do
    log "[$ctx] Setting up RBAC in namespace $NAMESPACE..."

    kubectl --context "$ctx" create namespace "$NAMESPACE" --dry-run=client -o yaml \
        | kubectl --context "$ctx" apply -f - 2>/dev/null

    kubectl --context "$ctx" -n "$NAMESPACE" create serviceaccount "$SA_NAME" \
        --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - 2>/dev/null

    kubectl --context "$ctx" -n "$NAMESPACE" apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${SA_NAME}
subjects:
  - kind: ServiceAccount
    name: ${SA_NAME}
    namespace: ${NAMESPACE}
EOF

    # Long-lived token Secret.
    token_secret="${SA_NAME}-token"
    kubectl --context "$ctx" -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${token_secret}
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${SA_NAME}
type: kubernetes.io/service-account-token
EOF

    log "[$ctx] Waiting for token..."
    token=""
    for i in $(seq 1 30); do
        token=$(kubectl --context "$ctx" -n "$NAMESPACE" get secret "$token_secret" \
            -o jsonpath='{.data.token}' 2>/dev/null | base64 -d) || true
        [ -n "$token" ] && break
        sleep 1
    done
    [ -z "$token" ] && die "[$ctx] Token not populated after 30s"
    SITE_TOKENS[$ctx]="$token"
    log "[$ctx] Token ready"
done

# ── For each site pair: create a kubeconfig Secret on the local cluster ────

for local_ctx in "${CONTEXTS[@]}"; do
    for remote_ctx in "${CONTEXTS[@]}"; do
        [ "$local_ctx" = "$remote_ctx" ] && continue

        api="${SITE_APIS[$remote_ctx]}"
        token="${SITE_TOKENS[$remote_ctx]}"
        secret_name="${remote_ctx}-kubeconfig"

        kubeconfig="apiVersion: v1
kind: Config
clusters:
  - name: ${remote_ctx}
    cluster:
      server: ${api}
      insecure-skip-tls-verify: true
users:
  - name: ${SA_NAME}
    user:
      token: ${token}
contexts:
  - name: ${remote_ctx}
    context:
      cluster: ${remote_ctx}
      user: ${SA_NAME}
      namespace: ${NAMESPACE}
current-context: ${remote_ctx}"

        log "[$local_ctx] Creating Secret $secret_name (kubeconfig for $remote_ctx)..."
        kubectl --context "$local_ctx" -n "$NAMESPACE" create secret generic "$secret_name" \
            --from-literal="kubeconfig=${kubeconfig}" \
            --dry-run=client -o yaml \
            | kubectl --context "$local_ctx" apply -f -
    done
done

log ""
log "Done. Kubeconfig Secrets created on ${#CONTEXTS[@]} sites."
log ""
log "SlapdCluster CR example:"
log "  externalPeers:"
for ctx in "${CONTEXTS[@]}"; do
    log "    - name: ${ctx}"
    log "      discovery:"
    log "        kubeconfigSecret:"
    log "          name: ${ctx}-kubeconfig"
done
log ""
log "Sites:"
for ctx in "${CONTEXTS[@]}"; do
    log "  $ctx → API: ${SITE_APIS[$ctx]}"
done
