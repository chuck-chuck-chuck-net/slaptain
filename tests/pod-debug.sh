#!/usr/bin/env bash
# pod-debug.sh — debug slapd pods via ephemeral debug containers.
#
# Modes:
#   ./tests/pod-debug.sh [-n namespace] <pod-name>        Collect diagnostic artifacts
#   ./tests/pod-debug.sh [-n namespace] -i <pod-name>     Interactive debug shell with credentials
#
# Output (collect mode): pod-debug-<pod>-<YYYYMMDD-HHMMSS>/ with one file per diagnostic.
set -euo pipefail

NAMESPACE="slaptain-testing"
INTERACTIVE=false
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROFILE="$SCRIPT_DIR/debug-profile.json"
IMAGE="ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:latest"

while getopts "n:i" opt; do
    case $opt in
        n) NAMESPACE="$OPTARG" ;;
        i) INTERACTIVE=true ;;
        *) echo "Usage: $0 [-n namespace] [-i] <pod-name>" >&2; exit 1 ;;
    esac
done
shift $((OPTIND - 1))

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 [-n namespace] [-i] <pod-name>" >&2
    echo "  -i    Interactive shell (with credentials pre-loaded)" >&2
    exit 1
fi

POD="$1"

# Detect the SlapdCluster name from the pod name (strip -N ordinal and -readonly suffix).
CLUSTER_NAME="${POD%-*}"           # slapd-readonly-0 → slapd-readonly
CLUSTER_NAME="${CLUSTER_NAME%-readonly}"  # slapd-readonly → slapd; slapd → slapd

# ── Interactive mode ──────────────────────────────────────────────────────────

if [[ "$INTERACTIVE" == true ]]; then
    echo "Fetching credentials for $NAMESPACE/$CLUSTER_NAME..."

    ROOT_PW=$(kubectl get secret -n "$NAMESPACE" "${CLUSTER_NAME}-config-password" \
        -o jsonpath='{.data.root-password}' 2>/dev/null | base64 -d) || true
    ADMIN_PW=$(kubectl get secret -n "$NAMESPACE" "${CLUSTER_NAME}-passwords" \
        -o jsonpath='{.data.admin-password}' 2>/dev/null | base64 -d) || true

    if [[ -z "$ROOT_PW" && -z "$ADMIN_PW" ]]; then
        echo "WARN: could not fetch credentials (secrets not found)" >&2
    fi

    # Build the base DN from the SlapdCluster spec if available.
    BASE_DN=$(kubectl get slapdcluster -n "$NAMESPACE" "$CLUSTER_NAME" \
        -o jsonpath='{.spec.ldap.domain}' 2>/dev/null) || true

    echo "Launching interactive debug container on $NAMESPACE/$POD..."
    echo ""

    # Embed credentials as env vars in the bash startup. They are visible in the
    # container's process list, which is acceptable for a debug tool on dev clusters.
    # shellcheck disable=SC2086
    kubectl debug -n "$NAMESPACE" "$POD" -it \
        --image="$IMAGE" \
        --image-pull-policy=Never \
        --target=slapd \
        --custom="$PROFILE" \
        -- bash -c "
export ROOT_PW='$(echo "$ROOT_PW" | sed "s/'/'\\\\''/g")'
export ADMIN_PW='$(echo "$ADMIN_PW" | sed "s/'/'\\\\''/g")'
export BASE_DN='$BASE_DN'
echo '──────────────────────────────────────────────'
echo 'Credentials loaded as environment variables:'
echo '  \$ROOT_PW   — cn=admin,cn=config password'
echo '  \$ADMIN_PW  — cn=admin,\$BASE_DN password'
echo '  \$BASE_DN   — $BASE_DN'
echo ''
echo 'Quick commands:'
echo '  ldapsearch -x -H ldap://localhost:1024 -D \"cn=admin,cn=config\" -w \"\$ROOT_PW\" -b \"cn=schema,cn=config\" -s one dn'
echo '  ldapsearch -x -H ldap://localhost:1024 -D \"cn=admin,\$BASE_DN\" -w \"\$ADMIN_PW\" -b \"\$BASE_DN\" -s sub \"(objectClass=*)\"'
echo '  ls /proc/1/root/ldap-config/cn=config/cn=schema/'
echo '  cat /proc/1/task/*/stack'
echo '──────────────────────────────────────────────'
exec bash
"
    exit $?
fi

# ── Collect mode (default) ────────────────────────────────────────────────────

OUTDIR="pod-debug-${POD}-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUTDIR"

echo "Collecting debug artifacts for $NAMESPACE/$POD into $OUTDIR/"

# Run all diagnostics inside a single debug container invocation.
# Each command writes to its own file inside the container, then we tar + base64
# the whole directory back to stdout. This avoids all marker-parsing issues.
ENCODED=$(kubectl debug -n "$NAMESPACE" "$POD" -i \
    --image="$IMAGE" \
    --image-pull-policy=Never \
    --target=slapd \
    --custom="$PROFILE" \
    -- bash -c '
set +e
D=/tmp/pod-debug
mkdir -p "$D"

ps axuwww > "$D/process-info.txt" 2>&1

for t in /proc/1/task/*/; do
    tid=$(basename "$t")
    printf "TID %s: wchan=" "$tid"
    cat "$t/wchan" 2>/dev/null
    echo
done > "$D/thread-states.txt" 2>&1

for t in /proc/1/task/*/; do
    tid=$(basename "$t")
    echo "--- TID $tid ---"
    cat "$t/stack" 2>&1
done > "$D/thread-stacks.txt" 2>&1

ls -la /proc/1/fd/ > "$D/fd-list.txt" 2>&1

cat /proc/1/net/tcp > "$D/tcp-connections.txt" 2>&1

timeout 5 strace -p 1 -f -e trace=futex,write,recvmsg -t > "$D/strace-sample.txt" 2>&1 || true

timeout 5 ldapsearch -x -H ldap://localhost:1024 -b "" -s base namingContexts > "$D/ldap-smoke.txt" 2>&1
echo "exit_code=$?" >> "$D/ldap-smoke.txt"

ls -la /proc/1/root/ldap-config/cn=config/cn=schema/ > "$D/cn-config-schema.txt" 2>&1

ls -R /proc/1/root/ldap-config/ > "$D/cn-config-tree.txt" 2>&1

tar czf - -C /tmp pod-debug 2>/dev/null | base64
')

# Strip any kubectl noise (warnings, prompts) before the base64 payload.
# The base64 output starts with "H4sI" (gzip magic bytes in base64).
PAYLOAD=$(echo "$ENCODED" | sed -n '/^H4sI/,$p')

if [[ -z "$PAYLOAD" ]]; then
    echo "ERROR: no data received from debug container" >&2
    echo "Raw output:" >&2
    echo "$ENCODED" >&2
    exit 1
fi

echo "$PAYLOAD" | base64 -d | tar xzf - -C "$OUTDIR" --strip-components=1

# Report what we got.
ARTIFACTS=0
for f in "$OUTDIR"/*.txt; do
    name=$(basename "$f")
    if [[ -s "$f" ]]; then
        echo "  $name"
        ARTIFACTS=$((ARTIFACTS + 1))
    else
        echo "  $name (empty)"
    fi
done

echo ""
echo "Done. $ARTIFACTS artifacts collected in $OUTDIR/"
