#!/bin/bash
set -eux

# Standardized paths to avoid parent directory permission issues
CONFIG_DIR="${CONFIG_DIR:-/ldap-config}"
DATA_DIR="${DATA_DIR:-/ldap-data}"
ACCESSLOG_DIR="${ACCESSLOG_DIR:-/ldap-accesslog}"

# Replication env vars (set by operator in Phase 2)
LDAP_REPLICATION_ENABLED="${LDAP_REPLICATION_ENABLED:-false}"
LDAP_REPLICAS="${LDAP_REPLICAS:-1}"
LDAP_CLUSTER_NAME="${LDAP_CLUSTER_NAME:-}"
LDAP_CLUSTER_HEADLESS_SVC="${LDAP_CLUSTER_HEADLESS_SVC:-}"
LDAP_NAMESPACE="${LDAP_NAMESPACE:-}"
LDAP_REPLICATION_PASSWORD="${LDAP_REPLICATION_PASSWORD:-}"

# Pod ordinal from StatefulSet hostname (<name>-<ordinal>)
ORDINAL="${HOSTNAME##*-}"

REPLICATION_ENABLED=false
if [[ "${LDAP_REPLICATION_ENABLED^^}" == "TRUE" ]] && [[ "${LDAP_REPLICAS}" -gt 1 ]]; then
    REPLICATION_ENABLED=true
fi

echo "Bootstrapping OpenLDAP"
echo "User: $(id)"
echo "Config Dir: $CONFIG_DIR"
echo "Data Dir: $DATA_DIR"
echo "Replication: $REPLICATION_ENABLED (replicas=${LDAP_REPLICAS}, ordinal=${ORDINAL})"

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
if [[ "$REPLICATION_ENABLED" == "true" ]]; then
    touch "$ACCESSLOG_DIR/.writable" && rm "$ACCESSLOG_DIR/.writable" || { echo "ERROR: $ACCESSLOG_DIR is not writable"; exit 1; }
fi

FORCE_REBOOTSTRAP="${FORCE_REBOOTSTRAP:-false}"
if [[ "${FORCE_REBOOTSTRAP^^}" == "TRUE" ]]; then
    echo "FORCE_REBOOTSTRAP is TRUE. Cleaning up existing data..."
    rm -rf "$CONFIG_DIR"/* "$DATA_DIR"/*
    if [[ "$REPLICATION_ENABLED" == "true" ]]; then
        rm -rf "$ACCESSLOG_DIR"/*
    fi
fi

if [[ ! -d "$CONFIG_DIR/cn=config" ]]; then
    echo "Generating base configuration..."

    ADMIN_PW_HASH=$(slappasswd -s "$LDAP_ADMIN_PW" -h {SSHA})
    ROOT_PW_HASH=$(slappasswd -s "$LDAP_ROOT_PW" -h {SSHA})

    TMP_CONF="/tmp/slapd.conf"

    # ── Global directives ──────────────────────────────────────────────────────
    cat <<EOF > "$TMP_CONF"
modulepath /usr/lib/ldap
moduleload back_mdb
EOF

    if [[ "$REPLICATION_ENABLED" == "true" ]]; then
        cat <<EOF >> "$TMP_CONF"
moduleload accesslog
moduleload syncprov
EOF
    fi

    cat <<EOF >> "$TMP_CONF"

include /etc/ldap/schema/core.schema
include /etc/ldap/schema/cosine.schema
include /etc/ldap/schema/inetorgperson.schema
include /etc/ldap/schema/nis.schema
EOF

    if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
        cat <<EOF >> "$TMP_CONF"

TLSCACertificateFile ${LDAP_TLS_CACERT_PATH:-/etc/openldap/tls/ca.crt}
TLSCertificateFile ${LDAP_TLS_CERT_PATH:-/etc/openldap/tls/tls.crt}
TLSCertificateKeyFile ${LDAP_TLS_KEY_PATH:-/etc/openldap/tls/tls.key}
EOF
    fi

    # ── Config database ────────────────────────────────────────────────────────
    cat <<EOF >> "$TMP_CONF"

database config
rootdn "cn=admin,cn=config"
rootpw $ROOT_PW_HASH
EOF

    # ── Accesslog database (replication only) ──────────────────────────────────
    if [[ "$REPLICATION_ENABLED" == "true" ]]; then
        cat <<EOF >> "$TMP_CONF"

database mdb
suffix cn=accesslog
rootdn "cn=admin,cn=config"
directory "$ACCESSLOG_DIR"
index default eq
index reqEnd,reqResult,reqStart eq

access to *
  by dn.exact="cn=replication,$LDAP_DOMAIN_DC" read
  by * none

overlay syncprov
syncprov-nopresent TRUE
syncprov-reloadhint TRUE
EOF
    fi

    # ── Main data database ─────────────────────────────────────────────────────
    cat <<EOF >> "$TMP_CONF"

database mdb
suffix "$LDAP_DOMAIN_DC"
rootdn "cn=admin,$LDAP_DOMAIN_DC"
rootpw $ADMIN_PW_HASH
directory "$DATA_DIR"
EOF

    if [[ "$REPLICATION_ENABLED" == "true" ]]; then
        # Peer URL scheme depends on TLS
        if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
            PEER_SCHEME="ldaps"
            PEER_PORT=1025
            SYNCREPL_TLS_OPT="  tls_cacert=${LDAP_TLS_CACERT_PATH:-/etc/openldap/tls/ca.crt}"
        else
            PEER_SCHEME="ldap"
            PEER_PORT=1024
            SYNCREPL_TLS_OPT=""
        fi

        cat <<EOF >> "$TMP_CONF"

overlay accesslog
logdb cn=accesslog
logops writes
logsuccess TRUE
logpurge 07+00:00 01+00:00

overlay syncprov
syncprov-checkpoint 100 10
syncprov-sessionlog 100

access to attrs=userPassword
  by self write
  by anonymous auth
  by * none

access to *
  by dn.exact="cn=replication,$LDAP_DOMAIN_DC" read
  by * read
EOF

        # One syncrepl block per peer (skip self); mirrormode must follow syncrepl
        for (( i=0; i<LDAP_REPLICAS; i++ )); do
            if [[ "$i" == "$ORDINAL" ]]; then
                continue
            fi
            PEER_HOST="${LDAP_CLUSTER_NAME}-${i}.${LDAP_CLUSTER_HEADLESS_SVC}.${LDAP_NAMESPACE}.svc.cluster.local"
            RID=$(printf "%03d" $(( i + 1 )))
            PROVIDER_URI="${PEER_SCHEME}://${PEER_HOST}:${PEER_PORT}"
            cat <<EOF >> "$TMP_CONF"

syncrepl rid=$RID
  provider=$PROVIDER_URI
  type=refreshAndPersist
  interval=00:00:05:00
  network-timeout=5
  timeout=3
  searchbase="$LDAP_DOMAIN_DC"
  filter="(objectClass=*)"
  scope=sub
  schemachecking=off
  bindmethod=simple
  binddn="cn=replication,$LDAP_DOMAIN_DC"
  credentials=$LDAP_REPLICATION_PASSWORD
  logbase="cn=accesslog"
  logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"
  syncdata=accesslog
${SYNCREPL_TLS_OPT}
  retry="5 +"
EOF
        done

        # mirrormode must be declared after all syncrepl directives
        cat <<'EOF' >> "$TMP_CONF"

mirrormode on
EOF
    fi

    # Convert slapd.conf to slapd.d format
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR" || true

    # ── Initial LDIF for data database ────────────────────────────────────────
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

    if [[ "$REPLICATION_ENABLED" == "true" ]]; then
        REPL_PW_HASH=$(slappasswd -s "$LDAP_REPLICATION_PASSWORD" -h {SSHA})
        cat <<EOF >> "$INIT_LDIF"

dn: cn=replication,$LDAP_DOMAIN_DC
objectClass: simpleSecurityObject
objectClass: organizationalRole
cn: replication
description: Syncrepl bind account
userPassword: $REPL_PW_HASH
EOF
    fi

    # Standalone (no replication): seed data directly via slapadd.
    # Replicated: skip slapadd entirely — the operator adds the base entries via a
    # live LDAP connection after pod-0 is ready, so the accesslog overlay captures
    # every write.  slapadd bypasses overlays; an unseeded accesslog causes
    # "consumer has state info but provider doesn't!" on the first delta-sync
    # reconnect.  Starting from an empty database on all pods avoids this entirely.
    if [[ "$REPLICATION_ENABLED" != "true" ]]; then
        slapadd -F "$CONFIG_DIR" -b "$LDAP_DOMAIN_DC" -l "$INIT_LDIF"
    else
        echo "Replication enabled: skipping slapadd; operator will bootstrap via LDAP."
    fi
    rm -f "$TMP_CONF" "$INIT_LDIF"
fi

echo "Bootstrap complete."
ls -R "$CONFIG_DIR"
