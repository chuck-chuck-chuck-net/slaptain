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

# Check writability. The accesslog dir may or may not be mounted depending on
# whether this pod is peer-eligible (operator gates the volume on
# NeedsAccesslogVolume()); only check if it actually exists.
touch "$CONFIG_DIR/.writable" && rm "$CONFIG_DIR/.writable" || { echo "ERROR: $CONFIG_DIR is not writable"; exit 1; }
touch "$DATA_DIR/.writable" && rm "$DATA_DIR/.writable" || { echo "ERROR: $DATA_DIR is not writable"; exit 1; }
if [[ -d "$ACCESSLOG_DIR" ]]; then
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

    # ── ServerID directives (multi-master only, ADR-011) ───────────────────────
    # When this cluster runs >1 RW replica, emit one "serverID <id> <url>" per
    # peer pod. Slapd matches its own pod URL against this list to identify
    # itself; this is the standard mechanism for unique CSN attribution across
    # a multi-master mesh.
    if [[ "$REPLICATION_ENABLED" == "true" ]] && [[ "$READONLY_REPLICA" != "true" ]] && \
       [[ -n "${LDAP_REPLICAS:-}" ]] && [[ "$LDAP_REPLICAS" -gt 1 ]]; then
        sid_base="${LDAP_SERVER_ID_BASE:-0}"
        scheme="ldap"; port=1024
        if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
            scheme="ldaps"; port=1025
        fi
        echo "" >> "$TMP_CONF"
        for ((i=0; i<LDAP_REPLICAS; i++)); do
            sid=$((sid_base + i + 1))
            url="${scheme}://${LDAP_CLUSTER_NAME}-${i}.${LDAP_CLUSTER_HEADLESS_SVC}.${LDAP_NAMESPACE}.svc.cluster.local:${port}"
            echo "serverID ${sid} ${url}" >> "$TMP_CONF"
        done
    fi

    if [[ "${LDAP_TLS_ENABLED^^}" == "TRUE" ]]; then
        cat <<EOF >> "$TMP_CONF"

TLSCertificateFile ${LDAP_TLS_CERT_PATH:-/etc/openldap/tls/tls.crt}
TLSCertificateKeyFile ${LDAP_TLS_KEY_PATH:-/etc/openldap/tls/tls.key}
EOF
        # TLSCACertificateFile is optional. Public-CA certs (Let's Encrypt
        # etc.) embed the chain in tls.crt and don't need a separate CA file —
        # slapd falls back to OpenSSL's system trust store. Self-signed or
        # private-PKI setups should mount ca.crt alongside tls.crt/tls.key;
        # only then do we emit the directive. Setting it to a missing file is
        # a hard error in slapd (TLS init def ctx failed: -1).
        cacert_path="${LDAP_TLS_CACERT_PATH:-/etc/openldap/tls/ca.crt}"
        if [[ -f "$cacert_path" ]]; then
            cat <<EOF >> "$TMP_CONF"
TLSCACertificateFile $cacert_path
EOF
        else
            echo "TLS: ca.crt not present at $cacert_path — relying on OpenSSL system trust store"
        fi
    fi

    # ── Config database ────────────────────────────────────────────────────────
    cat <<EOF >> "$TMP_CONF"

database config
rootdn "cn=admin,cn=config"
rootpw $ROOT_PW_HASH
EOF

    # NOTE: Neither the accesslog DB nor data databases are created here.
    # - Data databases are created by the SlapdDatabase controller via
    #   ldapmodify on cn=config (ADR-004).
    # - The accesslog DB is created and torn down by the SlapdDatabase
    #   controller at runtime based on the cluster's replication mode (ADR-010
    #   3e). The accesslog + syncprov modules are still loaded here so the
    #   symbols are available without a slapd restart when the operator later
    #   adds the DB or its overlays.

    # Convert slapd.conf to slapd.d format
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR" || true
    rm -f "$TMP_CONF"
fi

echo "Bootstrap complete."
ls -R "$CONFIG_DIR"
