#!/bin/bash
# Backward-compatibility wrapper. The multi-site path now lives in tests/e2e.sh
# as the N>=2 case of the unified script. This wrapper forwards args verbatim.
# New scripts and CI should invoke tests/e2e.sh directly.
set -euo pipefail
exec "$(dirname "$0")/e2e.sh" "$@"
