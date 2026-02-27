#!/usr/bin/env bash
# Bootstrap script for the slaptain slapd server.
#
# Reads all configuration from environment variables so it can be called
# identically by the automated Job and interactively from the toolkit pod.
#
# Required env vars (all set automatically by the Job / toolkit Deployment):
#   SLAPD_HOST        slapd service hostname (e.g. "slapd")
#   LDAP_ROOT_PW      rootDN (cn=admin,cn=config) password
#   LDAP_ADMIN_PW     admin (cn=admin,<domain>) password
#   LDAP_DOMAIN       domain in DC notation (e.g. "dc=as8,dc=lab,dc=test")
#   LDAPTLS_CACERT    path to the CA cert for LDAPS verification
#
# Usage:
#   bash /config/bootstrap.sh          # normal run
#   bash -x /config/bootstrap.sh       # trace every command
set -euo pipefail

# ── Install tools if not already present ────────────────────────────────────
if ! command -v ldapsearch &>/dev/null; then
    apt-get update && apt-get install -y ldap-utils
fi
if ! python -c 'import ldap3' &>/dev/null 2>&1; then
    pip install pyyaml ldap3
fi

# ── Wait for slapd to accept LDAPS connections ───────────────────────────────
until ldapsearch -x -H "ldaps://${SLAPD_HOST}" -LLL -s base; do
    echo "Waiting for slapd (LDAPS)..."
    sleep 2
done

# ── Step 1: Schema and ACL configuration ─────────────────────────────────────
python /config/ldap-bootstrap.py -d -H "ldaps://${SLAPD_HOST}/" \
    -D "cn=admin,cn=config" -w "${LDAP_ROOT_PW}" \
    /config/ox-schema.json /config/slapd-readpw.json

# ── Step 2: Directory data (OUs and read-only service accounts) ───────────────
python /config/ldap-bootstrap.py -d -H "ldaps://${SLAPD_HOST}/" \
    --domain "${LDAP_DOMAIN}" -w "${LDAP_ADMIN_PW}" \
    --auto-readpw /config/ldap-readpw-users.secret.yaml /config/slapd-ous.json
