#!/bin/bash
# Decode slapd hex timestamps (69aafab1.06e09e7d) to ISO 8601.
# Usage:
#   kubectl logs slapd-0 | ./decode-slapd-ts.sh
#   ./decode-slapd-ts.sh < logfile.txt
while IFS= read -r line; do
  if [[ "$line" =~ ^([0-9a-f]{8})\.[0-9a-f]+(.*) ]]; then
    ts=$(date -u -d "@$((16#${BASH_REMATCH[1]}))" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || ts="${BASH_REMATCH[1]}"
    printf '%s%s\n' "$ts" "${BASH_REMATCH[2]}"
  else
    printf '%s\n' "$line"
  fi
done
