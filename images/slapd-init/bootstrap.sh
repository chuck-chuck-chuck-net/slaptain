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

# Helper to ensure we have a hash
get_hash() {
    local val="$1"
    local hash_val="$2"
    if [[ -n "$hash_val" ]]; then
        echo "$hash_val"
    elif [[ -n "$val" ]]; then
        slappasswd -s "$val" -h {SSHA}
    else
        echo "ERROR: Neither password nor hash provided for a required field" >&2
        exit 1
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

    ADMIN_PW_HASH=$(get_hash "${LDAP_ADMIN_PW:-}" "${LDAP_ADMIN_PW_HASH:-}")
    ROOT_PW_HASH=$(get_hash "${LDAP_ROOT_PW:-}" "${LDAP_ROOT_PW_HASH:-}")

    TMP_CONF="/tmp/slapd.conf"
    cat <<EOF > "$TMP_CONF"
modulepath /usr/lib/ldap
moduleload back_mdb

include /etc/ldap/schema/core.schema
include /etc/ldap/schema/cosine.schema
include /etc/ldap/schema/inetorgperson.schema
include /etc/ldap/schema/nis.schema

database config
rootdn "cn=admin,cn=config"
rootpw $ROOT_PW_HASH

database mdb
suffix "$LDAP_DOMAIN_DC"
rootdn "cn=admin,$LDAP_DOMAIN_DC"
rootpw $ADMIN_PW_HASH
directory "$DATA_DIR"
EOF

    # Add TLS to slapd.conf if enabled
    if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
        cat <<EOF >> "$TMP_CONF"
TLSCACertificateFile ${LDAP_TLS_CACERT_PATH:-/etc/openldap/tls/ca.crt}
TLSCertificateFile ${LDAP_TLS_CERT_PATH:-/etc/openldap/tls/tls.crt}
TLSCertificateKeyFile ${LDAP_TLS_KEY_PATH:-/etc/openldap/tls/tls.key}
EOF
    fi

    # Convert slapd.conf to slapd.d format
    # Can't use -u to avoid the error; it doesn't do the conversion then
    # we might silence the expected error, but actually I'm no friend of that, it hides other errors as well
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR" || true

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
userPassword: $ADMIN_PW_HASH
EOF

    slapadd -F "$CONFIG_DIR" -b "$LDAP_DOMAIN_DC" -l "$INIT_LDIF"
    rm -f "$TMP_CONF" "$INIT_LDIF"
fi

echo "Bootstrap complete."
ls -R "$CONFIG_DIR"
