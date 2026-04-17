#!/usr/bin/env bash
# Bootstrap script for the slaptain slapd server.
#
# Reads all configuration from environment variables so it can be called
# identically by the automated Job and interactively from the toolkit pod.
#
# Required env vars (all set automatically by the Job / toolkit Deployment):
#   SLAPD_HOST        slapd service hostname (e.g. "slapd")
#   LDAP_ADMIN_PW     admin (cn=admin,<domain>) password
#   LDAP_DOMAIN       domain in DC notation (e.g. "dc=chuck-chuck-chuck,dc=net")
#   READPW_OU         name of the read-only service account OU (e.g. "Readpw")
#   LDAPTLS_CACERT    path to the CA cert for LDAPS verification
#
# Optional env vars:
#   LDAP_ROOT_PW      rootDN (cn=admin,cn=config) password — only required when
#                     bootstrap.customSchemaJson is set
#
# Usage:
#   bash /config/bootstrap.sh          # normal run
#   bash -x /config/bootstrap.sh       # trace every command
set -euo pipefail

# ── Wait for slapd to accept LDAPS connections ───────────────────────────────
until ldapsearch -x -H "ldaps://${SLAPD_HOST}" -LLL -s base; do
    echo "Waiting for slapd (LDAPS)..."
    sleep 2
done

# ── TEMPORARY: Wait for root entry (operator bootstrap race) ─────────────────
# The operator's reconcileBootstrap creates the root entry after the CR reaches
# Running phase. The slapd-test Job may start before this completes. This loop
# is a workaround — the proper fix is for the CR status to reflect bootstrap
# completion more accurately. See backlog.
echo "Waiting for root entry ${LDAP_DOMAIN}..."
until ldapsearch -x -H "ldaps://${SLAPD_HOST}" \
    -D "cn=admin,${LDAP_DOMAIN}" -w "${LDAP_ADMIN_PW}" \
    -b "${LDAP_DOMAIN}" -s base "(objectClass=*)" dn -LLL 2>/dev/null | grep -q "^dn:"; do
    sleep 2
done
echo "Root entry exists."

# ── Step 1: Custom schema (cn=config, optional) ───────────────────────────────
# ACL management is handled by the operator (spec.ldap.acls on the SlapdCluster
# CR), not here.  This step only loads application-specific schema extensions.
# custom-schema.json is only present when bootstrap.customSchemaJson is set.
# Use --bindpw=VALUE (long-form with =) instead of -w VALUE so that passwords
# starting with '-' are never misinterpreted as flags by Python's argparse.
if [ -f /config/custom-schema.json ]; then
    python /config/ldap-bootstrap.py -d -H "ldaps://${SLAPD_HOST}/" \
        -D "cn=admin,cn=config" --bindpw="${LDAP_ROOT_PW}" \
        /config/custom-schema.json
fi

# ── Step 2: Directory data (OUs and read-only service accounts) ───────────────
python /config/ldap-bootstrap.py -d -H "ldaps://${SLAPD_HOST}/" \
    --domain "${LDAP_DOMAIN}" --bindpw="${LDAP_ADMIN_PW}" \
    --readpw-ou "${READPW_OU}" \
    --auto-readpw /config/ldap-readpw-users.secret.yaml /config/slapd-ous.json
