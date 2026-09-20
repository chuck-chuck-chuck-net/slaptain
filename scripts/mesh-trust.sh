#!/usr/bin/env bash
# TLS trust for a mesh: a server certificate per site, then every site's CA
# distributed to all the others.
#
# These are one script because they are one ordered operation. The CA
# distribution consumes what the certificate issuance produces, and it cannot
# run until EVERY site has a certificate — do it per-site and the first site
# has nothing to hand out yet. Running them separately is the easiest thing to
# get wrong by hand, so the ordering is enforced here rather than documented.
#
# What each site gets:
#   Secret slapd-tls     its own server cert + key (+ ca.crt of the issuer)
#   Secret <site>-ca     every OTHER site's CA, under ca.crt — the name the
#                        operator derives from the mesh (MeshSite.CASecretNameFor)
#                        and mounts at /etc/openldap/tls/peers/<site>/ca.crt for
#                        each cross-site syncrepl stanza's tls_cacert (ADR-007).
#
# DELIBERATELY NOT HERE: database credentials. They look like neighbours —
# both are Secrets, both are prerequisites — but the rules are opposite. A
# replication password must be IDENTICAL at every site (ADR-008), while a
# certificate must DIFFER per site because it carries that site's names. One
# script with both behaviours under one name would be a trap. See
# scripts/mesh-credentials.sh.
#
# Issuance uses tests/gencert.sh, which asks the cluster's own CA to sign via
# the CSR API — free trust inside each cluster, four sharp edges, all of them in
# docs/TLS.md §3. If you issue certificates some other way (cert-manager, your
# PKI), skip issuance with --distribute-only and this script will only spread
# the CAs it finds.
#
# Usage:
#   ./scripts/mesh-trust.sh [-n NAMESPACE] [--cluster NAME] [options]
set -euo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/.." && pwd)"

LAB_FILE="${E2E_CONFIG:-}"
NAMESPACE="slaptain"
CLUSTER="slapd"
TLS_SECRET="slapd-tls"
DRY_RUN=""
DISTRIBUTE_ONLY=""

log() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 [options]

Issues one server certificate per site, then distributes every site's CA to all
the others. Sites come from a lab file (\$E2E_CONFIG, else <repo-root>/lab.yaml).

Options:
  -n, --namespace NS     Namespace holding the SlapdCluster (default: slaptain)
      --cluster NAME     SlapdCluster name; drives the cert's SANs and the
                         headless Service name (default: slapd)
      --from-lab FILE    Lab file to read (default: \$E2E_CONFIG or lab.yaml)
      --distribute-only  Skip issuance; only spread the CAs already present
      --dry-run          Say what would happen; change nothing
  -h, --help             Show this help

The certificate's SANs cover the ClusterIP Service, its FQDNs, the per-pod
wildcard *.<cluster>-headless.<ns>.svc.<domain>, and the site's nodeAccessIP
when lab.yaml declares one (NodePort access). Pod IPs are NOT SANs: IP-addressed
cross-site peers use tls_reqcert=allow, because a certificate cannot carry an
address assigned after it was issued (ADR-007).
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--namespace)    NAMESPACE="$2"; shift 2 ;;
        --cluster)         CLUSTER="$2"; shift 2 ;;
        --from-lab)        LAB_FILE="$2"; shift 2 ;;
        --distribute-only) DISTRIBUTE_ONLY=1; shift ;;
        --dry-run)         DRY_RUN=1; shift ;;
        -h|--help)         usage ;;
        *)                 die "Unknown argument: $1" ;;
    esac
done

[[ -z "$LAB_FILE" ]] && LAB_FILE="$REPO_ROOT/lab.yaml"
[[ -f "$LAB_FILE" ]] || die "no lab file at $LAB_FILE (pass --from-lab)"
command -v yq >/dev/null 2>&1 || die "yq v4 is required"

count=$(yq -r '.sites | length' "$LAB_FILE")
[[ "$count" == "null" || "$count" -eq 0 ]] && die "$LAB_FILE declares no sites"

SITES=() CONTEXTS=() NODEIPS=()
for i in $(seq 0 $((count - 1))); do
    name=$(yq -r ".sites[$i].name // \"\"" "$LAB_FILE")
    ctx=$(yq -r ".sites[$i].context // .sites[$i].name // \"\"" "$LAB_FILE")
    nip=$(yq -r ".sites[$i].nodeAccessIP // \"\"" "$LAB_FILE")
    [[ -z "$name" ]] && die "$LAB_FILE: sites[$i] has no name"
    SITES+=("$name"); CONTEXTS+=("$ctx"); NODEIPS+=("$nip")
done

run() { # describe... -- command...
    local desc=() ; while [[ "$1" != "--" ]]; do desc+=("$1"); shift; done; shift
    if [[ -n "$DRY_RUN" ]]; then
        printf '  would: %s\n' "${desc[*]}" >&2
    else
        "$@"
    fi
}

# ── 1. A certificate per site ────────────────────────────────────────────────
if [[ -z "$DISTRIBUTE_ONLY" ]]; then
    for i in "${!SITES[@]}"; do
        site="${SITES[$i]}" ctx="${CONTEXTS[$i]}" nip="${NODEIPS[$i]}"
        if [[ -z "$DRY_RUN" ]] && kubectl --context "$ctx" -n "$NAMESPACE" \
             get secret "$TLS_SECRET" >/dev/null 2>&1; then
            log "[$site] $TLS_SECRET exists — leaving it alone"
            continue
        fi
        log "[$site] Issuing $TLS_SECRET${nip:+ (IP SAN $nip)}..."
        # gencert.sh creates the namespace's Secret itself and is a no-op when
        # the Secret already exists, so this stays re-runnable.
        run "gencert.sh -c $ctx -n $NAMESPACE -t $CLUSTER -s $CLUSTER -H ${CLUSTER}-headless ${nip:+-i $nip} $TLS_SECRET" -- \
            bash -c "cd '$REPO_ROOT/tests' && ./gencert.sh -c '$ctx' -n '$NAMESPACE' -t '$CLUSTER' -s '$CLUSTER' -H '${CLUSTER}-headless' ${nip:+-i '$nip'} '$TLS_SECRET'"
    done
fi

# ── 2. Every site's CA, to every other site ──────────────────────────────────
#
# Read them ALL first. A per-site loop that issued and distributed in one pass
# would hand out whatever existed at the time, which for the first site is
# nothing.
log "Collecting CAs..."
declare -A CA
for i in "${!SITES[@]}"; do
    site="${SITES[$i]}" ctx="${CONTEXTS[$i]}"
    if [[ -n "$DRY_RUN" ]]; then
        CA[$site]="<dry-run: ${site}'s ca.crt>"
        continue
    fi
    CA[$site]=$(kubectl --context "$ctx" -n "$NAMESPACE" get secret "$TLS_SECRET" \
        -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 -d || true)
    [[ -z "${CA[$site]}" ]] && die "[$site] $TLS_SECRET has no ca.crt. A certificate from a public CA carries its chain in tls.crt and needs no bundle — but then cross-site peers have nothing to verify against either, so a mesh needs one. See docs/TLS.md."
done

for i in "${!SITES[@]}"; do
    dst="${SITES[$i]}" dst_ctx="${CONTEXTS[$i]}"
    for j in "${!SITES[@]}"; do
        src="${SITES[$j]}"
        [[ "$src" == "$dst" ]] && continue
        log "  ${src}-ca → $dst"
        # env, not a trailing assignment: `bash -c '...' VAR=x` sets $0, not a
        # variable, and the CA would be written EMPTY — a Secret that exists,
        # looks right, and verifies nothing.
        run "kubectl --context $dst_ctx -n $NAMESPACE apply secret ${src}-ca (ca.crt from $src)" -- \
            env CA_PAYLOAD="${CA[$src]}" bash -c "kubectl --context '$dst_ctx' -n '$NAMESPACE' \
                create secret generic '${src}-ca' --from-literal=ca.crt=\"\$CA_PAYLOAD\" \
                --dry-run=client -o yaml | kubectl --context '$dst_ctx' apply -f - >/dev/null"
    done
done

log ""
if [[ -n "$DRY_RUN" ]]; then
    log "Dry run: nothing was changed."
else
    log "Trust established: ${#SITES[@]} certificates, $(( ${#SITES[@]} * (${#SITES[@]} - 1) )) peer-CA Secrets."
fi
