#!/bin/bash
# Repeatedly run `tests/e2e.sh all <contexts>` until it fails or --max-iter
# iterations have completed. On failure, stops and leaves the cluster intact
# for diagnostics. Per-iteration logs go to /tmp/e2e-repro/iter-NNN.log.
#
# Useful for chasing non-deterministic failures — e.g. the dataloss+resilience
# syncrepl divergence parked in
# docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md.
#
# Usage:
#   tests/e2e-repro.sh [--max-iter N] ctx1 [ctx2 ...]
#
# Examples:
#   tests/e2e-repro.sh t3e bento              # run until failure
#   tests/e2e-repro.sh --max-iter 20 t3e      # at most 20 iterations
set -euo pipefail

usage() {
    # Print the leading comment block (skipping the shebang), stripped of '# '.
    awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0" >&2
    exit 1
}

MAX_ITER=0   # 0 = unlimited
CONTEXTS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --max-iter)
            [[ $# -ge 2 ]] || { echo "ERROR: --max-iter requires a value" >&2; exit 2; }
            MAX_ITER="$2"
            shift 2
            ;;
        --max-iter=*)
            MAX_ITER="${1#*=}"
            shift
            ;;
        -h|--help)
            usage
            ;;
        --)
            shift
            CONTEXTS+=("$@")
            break
            ;;
        -*)
            echo "ERROR: unknown flag: $1" >&2
            usage
            ;;
        *)
            CONTEXTS+=("$1")
            shift
            ;;
    esac
done

if [[ ${#CONTEXTS[@]} -lt 1 ]]; then
    echo "ERROR: at least one kubectl context required" >&2
    usage
fi

if ! [[ "$MAX_ITER" =~ ^[0-9]+$ ]]; then
    echo "ERROR: --max-iter must be a non-negative integer (got: $MAX_ITER)" >&2
    exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
E2E="$SCRIPT_DIR/e2e.sh"
[[ -x "$E2E" ]] || { echo "ERROR: $E2E not found or not executable" >&2; exit 2; }

LOG_DIR="${LOG_DIR:-/tmp/e2e-repro}"
mkdir -p "$LOG_DIR"

echo "==> Repro loop: contexts=${CONTEXTS[*]} max-iter=${MAX_ITER:-unlimited} logs=$LOG_DIR"

trap 'echo "==> interrupted at iter ${iter:-?}"; exit 130' INT TERM

iter=0
while true; do
    iter=$((iter + 1))
    if [[ $MAX_ITER -gt 0 && $iter -gt $MAX_ITER ]]; then
        echo "==> Reached --max-iter $MAX_ITER without failure"
        exit 0
    fi

    log="$LOG_DIR/iter-$(printf '%03d' "$iter").log"
    echo "==> iter $iter: starting at $(date -Is) (log: $log)"

    # Disable `set -e` for the test call so we can branch on its exit code.
    set +e
    "$E2E" all "${CONTEXTS[@]}" > "$log" 2>&1
    rc=$?
    set -e

    if [[ $rc -eq 0 ]]; then
        echo "==> iter $iter: PASSED"
    else
        echo "==> iter $iter: FAILED (exit $rc) — cluster preserved on: ${CONTEXTS[*]}"
        echo "==> log: $log"
        echo "==> tail:"
        tail -30 "$log" | sed 's/^/    /'
        exit "$rc"
    fi
done
