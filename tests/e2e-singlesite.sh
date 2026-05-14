#!/bin/bash
# Backward-compatibility wrapper. The single-site path now lives in tests/e2e.sh
# as the N=1 case of the unified script. The wrapper:
#   * preserves the old invocation shape (./tests/e2e-singlesite.sh <cmd> [ctx])
#   * defaults to the current kubectl context if none is given (the old
#     behaviour the unified script does not replicate to keep its contract
#     explicit).
# New scripts and CI should invoke tests/e2e.sh directly.
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <setup|test|teardown|all> [context]" >&2
    exit 1
fi

cmd="$1"; shift
ctx="${1:-$(kubectl config current-context)}"
exec "$(dirname "$0")/e2e.sh" "$cmd" "$ctx"
