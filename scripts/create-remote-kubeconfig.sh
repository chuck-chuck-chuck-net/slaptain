#!/bin/bash
# Create the cross-site kubeconfig Secrets that peer discovery needs.
#
# Every site's operator has to read the OTHER sites' pod IPs to write its
# syncrepl stanzas, and it does that by querying their Kubernetes APIs. This
# script provisions what that requires: on each cluster a ServiceAccount with a
# Role granting `get`/`list` on pods and nothing else, plus — on every site, for
# every other site — a Secret holding a kubeconfig for that peer's API. N sites
# means N×(N−1) such Secrets, because every site needs its own credential for
# every other one. Works for both transports: pod-routed (ADR-016, the default)
# reads the primary pod IP, multus (ADR-007) reads net1.
#
# ── TWO WAYS TO SAY WHICH SITES ───────────────────────────────────────────────
#
# FROM A LAB FILE (the default). With no positional arguments the script reads
# lab.yaml — $E2E_CONFIG, else <repo-root>/lab.yaml, else whatever --from-lab
# names — and takes each site's:
#
#     name       the site identity. Secrets are named "<name>-kubeconfig",
#                which is exactly what the operator derives from the SlapdMesh
#                (MeshSite.KubeconfigSecretFor), so nothing has to be pinned.
#     context    the kubectl context to reach that cluster as an admin.
#                Optional; defaults to name.
#     endpoint   the API address to put IN the kubeconfig — the one reachable
#                FROM PODS at the other sites. On dual-homed nodes this is NOT
#                the address in your kubeconfig: that one is the admin network,
#                and a kubeconfig carrying it would work from your laptop and
#                fail from a pod. Required in this mode, for that reason.
#
#   ./scripts/create-remote-kubeconfig.sh -n slaptain-testing
#   ./scripts/create-remote-kubeconfig.sh --from-lab other-lab.yaml
#
# FROM THE COMMAND LINE (overrides the file). Positional CONTEXT=API_URL pairs,
# where API_URL is again the pod-reachable address:
#
#   ./scripts/create-remote-kubeconfig.sh siteA=https://192.0.2.10:6443 \
#                                         siteB=https://192.0.2.20:6443
#
# One difference, and it is the reason to prefer the file: with no lab file
# there is no site name to use, so the Secrets are named after the CONTEXT
# ("siteB-kubeconfig" for context siteB). If your contexts are not named after
# your sites, point each mesh site's kubeconfigSecret.name at what came out.
#
# ── LOOK BEFORE YOU RUN ──────────────────────────────────────────────────────
#
# `--dry-run` prints every object this script would create, to stdout, grouped
# by the cluster it would be applied to, with the progress log on stderr — so it
# pipes and redirects like `helm template`. Each kubeconfig Secret is followed
# by its payload decoded into comments, because base64 hides the one thing worth
# reading. It talks to no cluster, which also means it cannot fetch the
# ServiceAccount tokens: those are replaced by an obvious placeholder. The
# output shows the SHAPE faithfully and the credentials not at all. Applying it
# will not produce a working mesh, and is not the point.
#
#   ./scripts/create-remote-kubeconfig.sh --dry-run -n slaptain-testing
#
# ── WHAT IT CREATES, PER SITE ────────────────────────────────────────────────
#
#   Namespace, ServiceAccount, Role + RoleBinding (pods: get,list)
#   Secret <sa>-token                  long-lived token, filled by the API server
#   Secret <peer>-kubeconfig  × (N−1)  one per other site
#
# A mesh-driven SlapdCluster needs no further wiring: the operator derives the
# Secret name per site. On a mesh-less cluster, reference it explicitly:
#
#   externalPeers:
#     - name: site-b
#       discovery:
#         kubeconfigSecret:
#           name: site-b-kubeconfig
#
# Note the kubeconfigs use insecure-skip-tls-verify: the peer API's certificate
# rarely carries the replication-network address as a SAN. The credential is the
# token; this affects how the API server is authenticated, not how the client is.
#
# tests/e2e.sh runs this script, so the documented path is the exercised one.
set -euo pipefail

NAMESPACE="slaptain"
SA_NAME="slaptain-remote-reader"
DRY_RUN=""
LAB_FILE=""

# Shaped like a token, unmistakably not one. A dry run never reads a real
# credential, so nothing here can leak one — and nobody can mistake the output
# for something that would authenticate.
DUMMY_TOKEN="DRY-RUN.NOT-A-REAL-SERVICEACCOUNT-TOKEN.%s"

# Progress goes to stderr, so `--dry-run > bootstrap.yaml` yields manifests
# alone, exactly as `helm template` does.
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 [options]                          # sites from lab.yaml (default)
       $0 [options] ctx1=API1 ctx2=API2 ...  # sites named explicitly

Provisions the RBAC and kubeconfig Secrets that cross-site peer discovery needs:
N sites get N×(N-1) Secrets, one per ordered pair.

With no positional arguments the sites come from a lab file — \$E2E_CONFIG, else
<repo-root>/lab.yaml — using each site's name (which names the Secret, matching
what the operator derives from the SlapdMesh), its context, and its endpoint:
the API address reachable FROM PODS at the other sites, which on dual-homed
nodes is not the one in your kubeconfig.

Arguments:
  CONTEXT=API_URL   kubectl context and that cluster's pod-reachable API address
                    (e.g. siteA=https://192.0.2.10:6443). Overrides the lab
                    file. Secrets are then named after the CONTEXT, since no
                    site name is available.

Options:
  -n, --namespace NS    Namespace on all clusters (default: slaptain)
      --from-lab [FILE] Read sites from FILE (default: \$E2E_CONFIG or lab.yaml)
      --dry-run         Print the manifests instead of applying them; contacts
                        no cluster, so tokens are placeholders
  -h, --help            Show this help

Examples:
  $0 -n slaptain-testing                     # the lab, as described in lab.yaml
  $0 --dry-run -n slaptain-testing           # ...but just show me
  $0 siteA=https://192.0.2.10:6443 siteB=https://192.0.2.20:6443
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --dry-run)      DRY_RUN=1; shift ;;
        --from-lab)     LAB_FILE="${2:-}"; [[ -n "$LAB_FILE" && "$LAB_FILE" != -* ]] && shift 2 || { LAB_FILE="auto"; shift; } ;;
        -h|--help)      usage ;;
        -*)             die "Unknown option: $1" ;;
        *)              break ;;
    esac
done

# ── Where the sites come from ─────────────────────────────────────────────
#
# Default: the lab file, because the information is already there and in one
# place — each site's kube context, its API address ON THE REPLICATION NETWORK
# (which on dual-homed nodes is NOT the address in your kubeconfig), and its
# logical name. Positional CONTEXT=API pairs still work and win.
#
# One deliberate difference between the two modes: with a lab file the Secrets
# are named after the SITE (site-2-kubeconfig), which is what the operator
# derives from the mesh (MeshSite.KubeconfigSecretFor). With positional pairs
# there is no site name to use, so they are named after the context, and you
# must point kubeconfigSecret.name at whatever came out.
declare -A SITE_APIS
declare -A SITE_LABELS
CONTEXTS=()

if [[ $# -eq 0 && -z "$LAB_FILE" ]]; then
    _self="$(cd "$(dirname "$0")/.." && pwd)"
    [[ -n "${E2E_CONFIG:-}" ]] && LAB_FILE="$E2E_CONFIG"
    [[ -z "$LAB_FILE" && -f "$_self/lab.yaml" ]] && LAB_FILE="$_self/lab.yaml"
fi
if [[ "$LAB_FILE" == "auto" ]]; then
    _self="$(cd "$(dirname "$0")/.." && pwd)"
    LAB_FILE="${E2E_CONFIG:-$_self/lab.yaml}"
fi

if [[ -n "$LAB_FILE" && $# -eq 0 ]]; then
    [[ -f "$LAB_FILE" ]] || die "no lab file at $LAB_FILE"
    command -v yq >/dev/null 2>&1 || die "reading $LAB_FILE requires yq v4"
    log "Reading sites from $LAB_FILE"
    count=$(yq -r '.sites | length' "$LAB_FILE")
    [[ "$count" == "null" || "$count" -eq 0 ]] && die "$LAB_FILE declares no sites"
    for i in $(seq 0 $((count - 1))); do
        name=$(yq -r ".sites[$i].name // \"\"" "$LAB_FILE")
        ctx=$(yq -r ".sites[$i].context // .sites[$i].name // \"\"" "$LAB_FILE")
        api=$(yq -r ".sites[$i].endpoint // \"\"" "$LAB_FILE")
        [[ -z "$name" ]] && die "$LAB_FILE: sites[$i] has no name"
        [[ -z "$api" ]] && die "$LAB_FILE: site '$name' has no endpoint. It is the API address reachable FROM PODS at the other sites, which on dual-homed nodes is not the one in your kubeconfig — that is the whole reason this field exists."
        CONTEXTS+=("$ctx")
        SITE_APIS[$ctx]="$api"
        SITE_LABELS[$ctx]="$name"
    done
    [[ ${#CONTEXTS[@]} -lt 2 ]] && die "$LAB_FILE declares ${#CONTEXTS[@]} site(s); cross-site kubeconfigs need at least 2"
    set -- # consume: the loop below must not re-parse
fi

[[ ${#CONTEXTS[@]} -eq 0 && $# -lt 2 ]] && { echo "At least 2 context=api pairs required (or a lab file)." >&2; usage; }

# Parse context=api pairs.
for arg in "$@"; do
    if [[ "$arg" != *=* ]]; then
        die "Invalid argument '$arg'. Expected CONTEXT=API_URL (e.g., siteA=https://192.168.99.1:6443)"
    fi
    ctx="${arg%%=*}"
    api="${arg#*=}"
    SITE_APIS[$ctx]="$api"
    SITE_LABELS[$ctx]="$ctx"   # no site name available in positional mode
    CONTEXTS+=("$ctx")
done

# emit reads a manifest on stdin and either applies it to the named context or
# prints it. Both modes consume the SAME text: a dry run that rendered its own
# copy of the manifests would drift from what the real run applies, which is the
# one thing that would make it worthless.
emit() { # context description
    local ctx="$1" what="$2"
    if [[ -n "$DRY_RUN" ]]; then
        printf -- '---\n# %s\n# apply with: kubectl --context %s apply -f -\n' "$what" "$ctx"
        cat
        printf '\n'
    else
        kubectl --context "$ctx" apply -f - >/dev/null
    fi
}

# render runs kubectl's CLIENT-side dry run, which never contacts a server. In
# --dry-run we drop --context too, so the script works on a machine that has
# never heard of these clusters.
render() { # context args...
    local ctx="$1"; shift
    if [[ -n "$DRY_RUN" ]]; then
        kubectl "$@" --dry-run=client -o yaml
    else
        kubectl --context "$ctx" "$@" --dry-run=client -o yaml
    fi
}

# ── For each site: create RBAC and extract token ───────────────────────────

declare -A SITE_TOKENS

for ctx in "${CONTEXTS[@]}"; do
    log "[$ctx] Setting up RBAC in namespace $NAMESPACE..."

    render "$ctx" create namespace "$NAMESPACE" \
        | emit "$ctx" "[$ctx] namespace $NAMESPACE"

    render "$ctx" -n "$NAMESPACE" create serviceaccount "$SA_NAME" \
        | emit "$ctx" "[$ctx] ServiceAccount the peers authenticate as"

    emit "$ctx" "[$ctx] RBAC — read pods in $NAMESPACE, and nothing else" <<EOF
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
    emit "$ctx" "[$ctx] long-lived token for that ServiceAccount (k8s fills .data.token)" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${token_secret}
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${SA_NAME}
type: kubernetes.io/service-account-token
EOF

    if [[ -n "$DRY_RUN" ]]; then
        # The only step with no offline equivalent: the token is minted by the
        # API server into the Secret above. Everything downstream of here uses
        # a placeholder, which is why the output cannot authenticate.
        # shellcheck disable=SC2059
        SITE_TOKENS[$ctx]=$(printf "$DUMMY_TOKEN" "$ctx")
        log "[$ctx] Token: placeholder (dry run reads no credential)"
        continue
    fi

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
        secret_name="${SITE_LABELS[$remote_ctx]}-kubeconfig"

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
        render "$local_ctx" -n "$NAMESPACE" create secret generic "$secret_name" \
            --from-literal="kubeconfig=${kubeconfig}" \
            | emit "$local_ctx" "[$local_ctx] Secret $secret_name — how $local_ctx reaches $remote_ctx's API"

        # The Secret's payload is the point of this whole script, and base64
        # hides it. Echo it back as comments: still valid YAML, and the reader
        # sees what the operator will actually load.
        if [[ -n "$DRY_RUN" ]]; then
            printf '# ...whose kubeconfig payload decodes to:\n'
            printf '%s\n' "$kubeconfig" | sed 's/^/#     /'
            printf '\n'
        fi
    done
done

log ""
if [[ -n "$DRY_RUN" ]]; then
    log "Dry run: nothing was created, and no cluster was contacted."
    log "Tokens above are placeholders — the real ones are minted by each API server."
else
    log "Done. Kubeconfig Secrets created on ${#CONTEXTS[@]} sites."
fi
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
