#!/usr/bin/env bash
# The per-database credential Secrets a mesh needs, IDENTICAL at every site.
#
# THIS IS THE ONE PREREQUISITE THAT FAILS AS SOMETHING ELSE. Each SlapdDatabase
# reads <database>-credentials for two values: root-password (the database's
# rootDN) and replication-password (what consumers bind with). The replication
# password must be the SAME at every site, because the legacy
# cn=replication,<suffix> identity is an entry inside the REPLICATED tree — so
# exactly one password can match mesh-wide (ADR-008). Let each site's operator
# generate its own and cross-site replication fails with err=49: an
# authentication error, which sends you looking at TLS, stanzas and firewalls
# for as long as it takes to remember this file.
#
# It is also why charts/slapd-mesh generates no Secret. A chart calling
# randAlphaNum would hand every site a different password and produce exactly
# that failure, looking for all the world like a chart doing its job.
#
# Which databases? From the chart values you deploy — the `databases[].name`
# list in your directory values file. That is a deliberate cross-layer read:
# the databases are a property of the DEPLOYMENT, while the sites are a property
# of the LAB, and this script is the one place the two have to meet.
#
# Re-runnable and non-destructive: a Secret that already exists is left alone
# and its values are COPIED to the sites that lack it, so a site added later
# joins with the passwords the mesh already uses. Nothing is ever rotated here —
# rotation is a deliberate act (ADR-027) and belongs nowhere near bootstrap.
#
# DELIBERATELY NOT HERE: certificates. See scripts/mesh-trust.sh for why the
# opposite rules (identical everywhere vs different per site) keep them apart.
#
# Usage:
#   ./scripts/mesh-credentials.sh -f values.directory.yaml [-n NAMESPACE]
set -euo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/.." && pwd)"

LAB_FILE="${E2E_CONFIG:-}"
DIRECTORY_FILE=""
NAMESPACE="slaptain"
DRY_RUN=""
DATABASES=()

log() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
Usage: $0 [-f DIRECTORY_VALUES] [-n NAMESPACE] [options]
       $0 --database NAME [--database NAME ...] [-n NAMESPACE]

Creates one credential Secret per database, with IDENTICAL values at every site
of the mesh. Sites come from a lab file (\$E2E_CONFIG, else <repo-root>/lab.yaml).

The replication-password MUST match mesh-wide (ADR-008) — this script exists so
that is true by construction rather than by remembering.

Options:
  -f, --file FILE      Directory values file; databases[].name is read from it
      --database NAME  Name a database explicitly (repeatable); skips -f
  -n, --namespace NS   Namespace on all clusters (default: slaptain)
      --from-lab FILE  Lab file to read (default: \$E2E_CONFIG or lab.yaml)
      --dry-run        Say what would happen; change nothing
  -h, --help           Show this help

Existing Secrets are never overwritten: their values are copied to any site
that lacks them, so adding a site later is safe.
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f|--file)      DIRECTORY_FILE="$2"; shift 2 ;;
        --database)     DATABASES+=("$2"); shift 2 ;;
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --from-lab)     LAB_FILE="$2"; shift 2 ;;
        --dry-run)      DRY_RUN=1; shift ;;
        -h|--help)      usage ;;
        *)              die "Unknown argument: $1" ;;
    esac
done

[[ -z "$LAB_FILE" ]] && LAB_FILE="$REPO_ROOT/lab.yaml"
[[ -f "$LAB_FILE" ]] || die "no lab file at $LAB_FILE (pass --from-lab)"
command -v yq >/dev/null 2>&1 || die "yq v4 is required"

if [[ ${#DATABASES[@]} -eq 0 ]]; then
    [[ -z "$DIRECTORY_FILE" ]] && die "name the databases: -f <directory values file>, or --database NAME"
    [[ -f "$DIRECTORY_FILE" ]] || die "no such file: $DIRECTORY_FILE"
    mapfile -t DATABASES < <(yq -r '.databases[].name // ""' "$DIRECTORY_FILE" | grep -v '^$' || true)
    [[ ${#DATABASES[@]} -eq 0 ]] && die "$DIRECTORY_FILE lists no databases[].name"
fi

count=$(yq -r '.sites | length' "$LAB_FILE")
[[ "$count" == "null" || "$count" -eq 0 ]] && die "$LAB_FILE declares no sites"
SITES=() CONTEXTS=()
for i in $(seq 0 $((count - 1))); do
    SITES+=("$(yq -r ".sites[$i].name" "$LAB_FILE")")
    CONTEXTS+=("$(yq -r ".sites[$i].context // .sites[$i].name" "$LAB_FILE")")
done

log "Databases: ${DATABASES[*]}"
log "Sites: ${SITES[*]}"

for db in "${DATABASES[@]}"; do
    secret="${db}-credentials"
    root_pw="" repl_pw="" found_at=""

    # Adopt whatever the mesh already uses, from wherever it already exists.
    if [[ -z "$DRY_RUN" ]]; then
        for i in "${!SITES[@]}"; do
            ctx="${CONTEXTS[$i]}"
            existing=$(kubectl --context "$ctx" -n "$NAMESPACE" get secret "$secret" \
                -o json 2>/dev/null || true)
            [[ -z "$existing" ]] && continue
            root_pw=$(printf '%s' "$existing" | yq -r '.data["root-password"] // ""' | base64 -d 2>/dev/null || true)
            repl_pw=$(printf '%s' "$existing" | yq -r '.data["replication-password"] // ""' | base64 -d 2>/dev/null || true)
            if [[ -n "$root_pw" && -n "$repl_pw" ]]; then
                found_at="${SITES[$i]}"
                break
            fi
            log "[$db] $secret at ${SITES[$i]} is missing a key — ignoring it as a source"
        done
    fi

    if [[ -n "$found_at" ]]; then
        log "[$db] adopting the passwords already in use (from $found_at)"
    else
        log "[$db] generating new passwords"
        root_pw=$(openssl rand -base64 18)
        repl_pw=$(openssl rand -base64 24)
    fi

    for i in "${!SITES[@]}"; do
        site="${SITES[$i]}" ctx="${CONTEXTS[$i]}"
        if [[ -n "$DRY_RUN" ]]; then
            printf '  would: ensure Secret %s at %s (keys root-password, replication-password)\n' \
                "$secret" "$site" >&2
            continue
        fi
        kubectl --context "$ctx" create namespace "$NAMESPACE" \
            --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
        if kubectl --context "$ctx" -n "$NAMESPACE" get secret "$secret" >/dev/null 2>&1; then
            log "  [$site] $secret exists — left alone"
            continue
        fi
        log "  [$site] creating $secret"
        env ROOT_PW="$root_pw" REPL_PW="$repl_pw" bash -c \
            "kubectl --context '$ctx' -n '$NAMESPACE' create secret generic '$secret' \
                --from-literal=root-password=\"\$ROOT_PW\" \
                --from-literal=replication-password=\"\$REPL_PW\" \
                --dry-run=client -o yaml | kubectl --context '$ctx' apply -f - >/dev/null"
    done
done

log ""
if [[ -n "$DRY_RUN" ]]; then
    log "Dry run: nothing was changed."
else
    log "Credentials in place for ${#DATABASES[@]} database(s) across ${#SITES[@]} site(s)."
    log "The replication-password is now identical mesh-wide, which is the point (ADR-008)."
fi
