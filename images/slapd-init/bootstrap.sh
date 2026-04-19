#!/bin/bash
set -eux

# Standardized paths
CONFIG_DIR="${CONFIG_DIR:-/config}"
DATA_DIR="${DATA_DIR:-/data}"
ACCESSLOG_DIR="${ACCESSLOG_DIR:-/accesslog}"

# Replication env vars (set by operator)
LDAP_REPLICATION_ENABLED="${LDAP_REPLICATION_ENABLED:-false}"
LDAP_READONLY_REPLICA="${LDAP_READONLY_REPLICA:-false}"

# DATABASE_DIRS: comma-separated list of SlapdDatabase CR names.
# The init container creates /data/<name>/ for each.
DATABASE_DIRS="${DATABASE_DIRS:-}"

echo "Bootstrapping OpenLDAP (cn=config infrastructure only)"
echo "User: $(id)"
echo "Config Dir: $CONFIG_DIR"
echo "Data Dir: $DATA_DIR"

REPLICATION_ENABLED=false
READONLY_REPLICA=false
if [[ "${LDAP_READONLY_REPLICA^^}" == "TRUE" ]]; then
    READONLY_REPLICA=true
    REPLICATION_ENABLED=true
elif [[ "${LDAP_REPLICATION_ENABLED^^}" == "TRUE" ]]; then
    REPLICATION_ENABLED=true
fi

echo "Replication: $REPLICATION_ENABLED (readonly=${READONLY_REPLICA})"

# Check writability
touch "$CONFIG_DIR/.writable" && rm "$CONFIG_DIR/.writable" || { echo "ERROR: $CONFIG_DIR is not writable"; exit 1; }
touch "$DATA_DIR/.writable" && rm "$DATA_DIR/.writable" || { echo "ERROR: $DATA_DIR is not writable"; exit 1; }
if [[ "$REPLICATION_ENABLED" == "true" ]] && [[ "$READONLY_REPLICA" != "true" ]]; then
    touch "$ACCESSLOG_DIR/.writable" && rm "$ACCESSLOG_DIR/.writable" || { echo "ERROR: $ACCESSLOG_DIR is not writable"; exit 1; }
fi

# ── Create per-database data directories ──────────────────────────────────────
# Each SlapdDatabase CR gets its own subdirectory under /data/.
# The SlapdCluster controller passes the list of database CR names.
if [[ -n "$DATABASE_DIRS" ]]; then
    IFS=',' read -ra DIRS <<< "$DATABASE_DIRS"
    for dir in "${DIRS[@]}"; do
        dir=$(echo "$dir" | xargs)  # trim whitespace
        if [[ -n "$dir" ]]; then
            echo "Ensuring data directory: $DATA_DIR/$dir"
            mkdir -p "$DATA_DIR/$dir"
        fi
    done
fi

FORCE_REBOOTSTRAP="${FORCE_REBOOTSTRAP:-false}"
if [[ "${FORCE_REBOOTSTRAP^^}" == "TRUE" ]]; then
    echo "FORCE_REBOOTSTRAP is TRUE. Cleaning up existing data..."
    rm -rf "$CONFIG_DIR"/* "$DATA_DIR"/*
    if [[ "$REPLICATION_ENABLED" == "true" ]] && [[ "$READONLY_REPLICA" != "true" ]]; then
        rm -rf "$ACCESSLOG_DIR"/*
    fi
fi

if [[ ! -d "$CONFIG_DIR/cn=config" ]]; then
    echo "Generating base configuration..."

    ROOT_PW_HASH=$(slappasswd -s "$LDAP_ROOT_PW" -h {SSHA})

    TMP_CONF="/tmp/slapd.conf"

    # ── Global directives ──────────────────────────────────────────────────────
    cat <<EOF > "$TMP_CONF"
modulepath /usr/lib/ldap
moduleload back_mdb
EOF

    if [[ "$REPLICATION_ENABLED" == "true" ]] && [[ "$READONLY_REPLICA" != "true" ]]; then
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

    # ── Accesslog database (RW replication only, not for read-only replicas) ──
    # Sets up the accesslog infrastructure. The SlapdDatabase controller adds
    # the accesslog overlay to each data database that opts into delta-sync
    # and configures per-database ACLs on the accesslog.
    if [[ "$REPLICATION_ENABLED" == "true" ]] && [[ "$READONLY_REPLICA" != "true" ]]; then
        cat <<EOF >> "$TMP_CONF"

database mdb
suffix cn=accesslog
rootdn "cn=admin,cn=config"
directory "$ACCESSLOG_DIR"
index default eq
index reqEnd,reqResult,reqStart eq

overlay syncprov
syncprov-nopresent TRUE
syncprov-reloadhint TRUE
EOF
    fi

    # NOTE: No data database is created here. Data databases are created
    # dynamically by the SlapdDatabase controller via ldapmodify on cn=config.
    # See ADR-004 for the multi-resource architecture.

    # Convert slapd.conf to slapd.d format
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR" || true
    rm -f "$TMP_CONF"
fi

echo "Bootstrap complete."
ls -R "$CONFIG_DIR"
