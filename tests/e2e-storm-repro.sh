#!/usr/bin/env bash
# ITS#9580 storm reproduction (ADR-021): observe the dataloss no-storm
# assertion RED on OpenLDAP 2.6, deliberately.
#
# The storm needs three ingredients (ADR-008 amendment of 2026-09-11, ADR-021):
#   dormancy — a serverID that wrote once and then never again, so its CSN
#              rides in every cookie but ages;
#   ammunition — accesslogs that can no longer vouch for that old CSN
#              (here: an aggressive accesslogPurge in the storm-repro fixture);
#   trigger — a reconnect presenting such a cookie (here: the dataloss spec's
#              PVC-loss recovery).
# A fresh cluster has none of these, which is why the plain e2e never goes red
# on 2.6 (measured — see ADR-021). This script manufactures the first two and
# then iterates the trigger.
#
# Usage:
#   GIT_TAG=<pushed-tag> ./tests/e2e-storm-repro.sh [ctx1 ctx2 ... ctxN]
#
# Contexts default to the lab config's sites (lab.yaml — see lab.yaml.sample);
# the LAST context is the idle site. Requires at least two sites, images pushed
# for both the plain and the -ol26 pair at GIT_TAG, and yq v4.
#
# Env knobs:
#   MAX_ITERS      trigger iterations before giving up          (default 10)
#   PURGE_WAIT_S   dormancy horizon: purge maxage + slack       (default 420)
#   SKIP_SETUP=1   reuse the already-deployed storm-repro mesh
#
# The mesh is deployed with SLAPD_TAG_SUFFIX=-ol26 (OpenLDAP 2.6) and
# TEST_RESOURCES=storm-repro (accesslogPurge "0+00:05 0+00:01"). On success the
# run ends with the resilience-labelled e2e FAILING on the no-storm spec — that
# failure is the observed red the assertion never had (Test Discipline). It is
# left to the operator of this script to record the evidence (ADR-021).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

log() { printf "\033[1;35m[storm-repro]\033[0m %s\n" "$*"; }
die() { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

[[ -n "${GIT_TAG:-}" ]] || die "GIT_TAG must name a pushed image tag (both plain and -ol26 pairs)"
command -v yq >/dev/null || die "requires yq v4"
command -v ldapadd >/dev/null || die "requires ldap-utils (ldapadd)"

E2E_CONFIG="${E2E_CONFIG:-$PROJECT_ROOT/lab.yaml}"

CONTEXTS=("$@")
if [[ ${#CONTEXTS[@]} -lt 1 ]]; then
    [[ -f "$E2E_CONFIG" ]] || die "no contexts given and no lab config at $E2E_CONFIG"
    mapfile -t CONTEXTS < <(yq -r '.sites[].context' "$E2E_CONFIG")
fi
[[ ${#CONTEXTS[@]} -ge 2 ]] || die "need at least two sites (one stays idle); got: ${CONTEXTS[*]:-none}"

PRIMARY_CTX="${CONTEXTS[0]}"
IDLE_CTX="${CONTEXTS[-1]}"
MAX_ITERS="${MAX_ITERS:-10}"
PURGE_WAIT_S="${PURGE_WAIT_S:-420}"   # fixture maxage is 5 min; leave slack for a purge sweep

# Node-access address of a site: lab config nodeAccessIP, else the node
# InternalIP (single-homed labs).
site_addr() {
    local ctx="$1" ip=""
    if [[ -f "$E2E_CONFIG" ]]; then
        ip="$(yq -r ".sites[] | select(.context == \"$ctx\") | .nodeAccessIP // \"\"" "$E2E_CONFIG")"
    fi
    if [[ -z "$ip" ]]; then
        ip="$(kubectl --context "$ctx" get nodes \
            -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
    fi
    echo "$ip"
}

e2e() { # subcommand — always the -ol26 storm-repro mesh
    SLAPD_TAG_SUFFIX=-ol26 TEST_RESOURCES=storm-repro GIT_TAG="$GIT_TAG" \
        "$SCRIPT_DIR/e2e.sh" "$@"
}

# ── Phase 1: the mesh ────────────────────────────────────────────────────────
if [[ -z "${SKIP_SETUP:-}" ]]; then
    log "deploying the -ol26 (OpenLDAP 2.6) mesh with the storm-repro fixture on: ${CONTEXTS[*]}"
    e2e teardown "${CONTEXTS[@]}"
    e2e setup "${CONTEXTS[@]}"
else
    log "SKIP_SETUP=1 — reusing the deployed mesh"
fi

ADMIN_PW="$(kubectl --context "$PRIMARY_CTX" -n "${NAMESPACE_TESTING:-slaptain-testing}" \
    get secret example-db-credentials -o jsonpath='{.data.root-password}' | base64 -d)"
PRIMARY_ADDR="$(site_addr "$PRIMARY_CTX"):30389"
IDLE_ADDR="$(site_addr "$IDLE_CTX"):30389"

# Dormancy: one write originating on the idle site. The entry itself is
# irrelevant; what matters is that some serverID of the idle site stamps a CSN
# into the mesh's contextCSN and then never writes again. The entry stays
# (deleting it would stamp a second, newer CSN). Re-armed before EVERY trigger
# iteration: iteration 1 of the first run showed that one trigger consumes the
# dormancy — after its refreshes settle, no journal still carries the dormant
# SID's minCSN entry, and later reconnects pass again.
arm_dormancy() {
    local marker="dormant-marker-$(date +%s)"
    log "arming dormancy: marker via the idle site ($IDLE_CTX @ $IDLE_ADDR)"
    ldapadd -x -H "ldap://$IDLE_ADDR" -D "cn=admin,dc=example,dc=org" -w "$ADMIN_PW" <<EOF
dn: uid=$marker,ou=People,dc=example,dc=org
objectClass: inetOrgPerson
uid: $marker
cn: Dormant Marker
sn: Marker
EOF
}

# Churn: add/delete cycles through a site's LDAP endpoint. Keeps the journals
# full of newer entries so the purge sweeps drop everything as old as the
# marker — and, during a trigger, keeps writes flowing while the recovering
# mesh is mid-refresh (the original storm ran under live traffic; quiet
# recoveries settled with only isolated stale answers).
churn() { # addr seconds uid-suffix
    local addr="$1" secs="$2" uid="churn-$3" end i=0
    end=$(( $(date +%s) + secs ))
    while (( $(date +%s) < end )); do
        i=$((i+1))
        ldapmodify -x -H "ldap://$addr" -D "cn=admin,dc=example,dc=org" -w "$ADMIN_PW" \
            >/dev/null 2>&1 <<EOF || true
dn: uid=$uid,ou=People,dc=example,dc=org
changetype: add
objectClass: inetOrgPerson
uid: $uid
cn: churn
sn: churn
EOF
        ldapdelete -x -H "ldap://$addr" -D "cn=admin,dc=example,dc=org" -w "$ADMIN_PW" \
            "uid=$uid,ou=People,dc=example,dc=org" >/dev/null 2>&1 || true
        sleep 3
    done
    return 0
}

SECOND_ADDR="$PRIMARY_ADDR"
if [[ ${#CONTEXTS[@]} -ge 3 ]]; then
    SECOND_ADDR="$(site_addr "${CONTEXTS[1]}"):30389"   # a non-idle remote site
fi

# ── Trigger loop: re-arm dormancy, age it past the purge horizon, then run the
# dataloss spec under live churn until the no-storm assertion goes red ────────
# E2E_LABEL_FILTER=resilience runs the dataloss container (the PVC-loss
# recovery + the no-storm assertion). E2E_RESILIENCE stays unset, so the
# warm-restart specs are not even registered.
for (( iter=1; iter<=MAX_ITERS; iter++ )); do
    arm_dormancy
    log "aging dormancy for ${PURGE_WAIT_S}s (churn on primary; idle site silent)"
    churn "$PRIMARY_ADDR" "$PURGE_WAIT_S" "age-$iter"
    log "trigger iteration $iter/$MAX_ITERS (live churn on primary + second site during recovery)"
    out="$PROJECT_ROOT/storm-repro-iter-$iter.log"
    churn "$PRIMARY_ADDR" 900 "live-$iter-a" & CH1=$!
    churn "$SECOND_ADDR" 900 "live-$iter-b" & CH2=$!
    rc=0
    E2E_LABEL_FILTER=resilience e2e test "${CONTEXTS[@]}" >"$out" 2>&1 || rc=$?
    kill "$CH1" "$CH2" 2>/dev/null || true
    wait "$CH1" "$CH2" 2>/dev/null || true
    if [[ $rc -eq 0 ]]; then
        count="$(grep -o '"total"=[0-9]*' "$out" | tail -1 || true)"
        log "iteration $iter green (stale ${count:-n/a}) — no storm yet"
    else
        if grep -q "no 'sync cookie is stale' storm" "$out" && grep -q "FAIL!" "$out"; then
            log "RED OBSERVED on iteration $iter — the no-storm assertion failed on 2.6"
            grep -E '"total"=[0-9]*|\[FAILED\]' "$out" | tail -5
            log "evidence: $out — record it in ADR-021 and tear the mesh down when done:"
            log "  SLAPD_TAG_SUFFIX=-ol26 TEST_RESOURCES=storm-repro GIT_TAG=$GIT_TAG $SCRIPT_DIR/e2e.sh teardown ${CONTEXTS[*]}"
            exit 0
        fi
        log "iteration $iter failed on something other than the storm spec — inspect $out"
        exit 2
    fi
done

log "no storm in $MAX_ITERS iterations — negative result; record it (ADR-021)"
exit 3
