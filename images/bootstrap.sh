#!/bin/bash
set -eux

# Standardized paths to avoid parent directory permission issues
CONFIG_DIR="${CONFIG_DIR:-/ldap-config}"
DATA_DIR="${DATA_DIR:-/ldap-data}"

echo "Bootstrapping OpenLDAP"
echo "User: $(id)"
echo "Config Dir: $CONFIG_DIR"
echo "Data Dir: $DATA_DIR"

fix_crc() {
    local target="$1"
    if [[ -f "$target" ]]; then
        crc=$(python3 -c "import zlib; import sys; print('%08x' % (zlib.crc32(sys.stdin.buffer.read()) & 0xFFFFFFFF))" < <(tail -n +3 "$target"))
        sed -i -e "/^# CRC32 .*/s/# CRC32 .*/# CRC32 $crc/" "$target"
    fi
}

# Check writability
touch "$CONFIG_DIR/.writable" && rm "$CONFIG_DIR/.writable" || { echo "ERROR: $CONFIG_DIR is not writable"; exit 1; }
touch "$DATA_DIR/.writable" && rm "$DATA_DIR/.writable" || { echo "ERROR: $DATA_DIR is not writable"; exit 1; }

FORCE_REBOOTSTRAP="${FORCE_REBOOTSTRAP:-false}"
if [[ "${FORCE_REBOOTSTRAP^^}" == "TRUE" ]]; then
    echo "FORCE_REBOOTSTRAP is TRUE. Cleaning up existing data..."
    rm -rf "$CONFIG_DIR"/* "$DATA_DIR"/*
fi

if [[ ! -d "$CONFIG_DIR/cn=config" ]]; then
    echo "Generating base configuration..."

    TMP_CONF="/tmp/slapd.conf"
    cat <<EOF > "$TMP_CONF"
modulepath /usr/lib/openldap
moduleload back_mdb

include /etc/openldap/schema/core.schema
include /etc/openldap/schema/cosine.schema
include /etc/openldap/schema/inetorgperson.schema
include /etc/openldap/schema/nis.schema

database config
rootdn "cn=admin,cn=config"
rootpw $LDAP_ROOT_PW_HASH

database mdb
suffix "$LDAP_DOMAIN_DC"
rootdn "cn=admin,$LDAP_DOMAIN_DC"
rootpw $LDAP_ADMIN_PW_HASH
directory "$DATA_DIR"
EOF

    # Convert slapd.conf to slapd.d format
    # Can't use -u to avoid the error; it doesn't do the conversion then
    # we might silence the expected error, but actually I'm no friend of that, it hides other errors as well
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR" || true

    # Handle TLS if enabled
    if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
        CONFIG_FILE="$CONFIG_DIR/cn=config/olcDatabase={0}config.ldif"
        echo "olcTLSCACertificateFile: ${LDAP_TLS_CACERT_PATH:-/etc/openldap/tls/ca.crt}" >> "$CONFIG_FILE"
        echo "olcTLSCertificateFile: ${LDAP_TLS_CERT_PATH:-/etc/openldap/tls/tls.crt}" >> "$CONFIG_FILE"
        echo "olcTLSCertificateKeyFile: ${LDAP_TLS_KEY_PATH:-/etc/openldap/tls/tls.key}" >> "$CONFIG_FILE"
        fix_crc "$CONFIG_FILE"
    fi

    # Create initial domain objects
    t1=${LDAP_DOMAIN_DC%%,*}
    dc=${t1#dc=}
    INIT_LDIF="/tmp/init.ldif"
    cat <<EOF > "$INIT_LDIF"
dn: $LDAP_DOMAIN_DC
objectClass: top
objectClass: dcObject
objectClass: organization
o: $dc
dc: $dc

dn: cn=admin,$LDAP_DOMAIN_DC
objectClass: simpleSecurityObject
objectClass: organizationalRole
cn: admin
description: LDAP administrator
userPassword: $LDAP_ADMIN_PW_HASH
EOF

    slapadd -F "$CONFIG_DIR" -b "$LDAP_DOMAIN_DC" -l "$INIT_LDIF"
    rm -f "$TMP_CONF" "$INIT_LDIF"
fi

echo "Bootstrap complete."
ls -R "$CONFIG_DIR"
