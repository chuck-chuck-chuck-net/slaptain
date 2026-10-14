#!/bin/bash
set -eux

# Standardized paths
CONFIG_DIR="${CONFIG_DIR:-/config}"
DATA_DIR="${DATA_DIR:-/data}"
ACCESSLOG_DIR="${ACCESSLOG_DIR:-/accesslog}"

# Replication env vars (set by operator)
LDAP_REPLICATION_ENABLED="${LDAP_REPLICATION_ENABLED:-false}"
LDAP_READONLY_REPLICA="${LDAP_READONLY_REPLICA:-false}"

# The per-database LMDB directories to create, as two newline-separated files
# under DATABASE_DIRS_PATH: `data-dirs` (subdirectories of /data) and
# `accesslog-dirs` (subdirectories of /accesslog).
#
# A MOUNTED CONFIGMAP, not an env var, and that is the whole point: an env var
# is part of the pod template, so adding or removing a database re-rendered the
# template and the StatefulSet replaced every pod — for an operation slapd
# performs live (ADR-013 amendment of 2026-09-28). The ConfigMap is referenced
# by name, so the template never moves. Already-running pods get their
# directories from the operator's mkdir Job, not from here.
#
# The two lists are separate because the names are not the same names: a data
# directory is spec.dataDirectory when set and the CR name otherwise, while an
# accesslog directory is always keyed on the CR name (ADR-019 R1).
#
# DATABASE_DIRS is the pre-2026-09-28 env var, kept only as a fallback so a new
# image under an old operator still provisions its directories. It is not
# written by any current operator version.
DATABASE_DIRS_PATH="${DATABASE_DIRS_PATH:-/etc/slaptain/database-dirs}"
DATABASE_DIRS="${DATABASE_DIRS:-}"

# The declared table of startup-only cn=config attributes (ADR-034), rendered by
# the operator into the <cluster>-startup-config ConfigMap and mounted read-only.
# One TAB-separated record per line: <attribute> <database-suffix> <value>.
# Absent file = nothing is managed; see startup_config.py for the semantics.
STARTUP_CONFIG_PATH="${STARTUP_CONFIG_PATH:-/etc/slaptain/startup-config/startup-config}"

# read_dir_list <file> — emit one directory name per line, ignoring blanks.
read_dir_list() {
    [[ -r "$1" ]] || return 0
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line//[[:space:]]/}"
        [[ -n "$line" ]] && echo "$line"
    done < "$1"
}

# AUTH_DIR: the node-local authentication database's LMDB directory (ADR-027).
# Mirrors the AuthDir constant in operator/api/v1alpha1/authdb.go; the two must
# agree, because the operator writes it into olcDbDirectory and back-mdb does
# not create the directory itself.
AUTH_DIR="${DATA_DIR}/_slaptain-auth"

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

# Check writability. Every RW pod mounts the accesslog dir, replicating or not
# (ADR-013 amendment 2026-10-07: gating it on replication state put the mount in
# the pod template and made turning replication on replace pod-0). A read-only
# replica never gets it — it produces no changes — so this stays conditional on
# the directory existing rather than on anything the pod would have to be told.
touch "$CONFIG_DIR/.writable" && rm "$CONFIG_DIR/.writable" || { echo "ERROR: $CONFIG_DIR is not writable"; exit 1; }
touch "$DATA_DIR/.writable" && rm "$DATA_DIR/.writable" || { echo "ERROR: $DATA_DIR is not writable"; exit 1; }
if [[ -d "$ACCESSLOG_DIR" ]]; then
    touch "$ACCESSLOG_DIR/.writable" && rm "$ACCESSLOG_DIR/.writable" || { echo "ERROR: $ACCESSLOG_DIR is not writable"; exit 1; }
fi

# ── Create the node-local auth database's directory (ADR-027) ─────────────────
# UNCONDITIONAL and independent of DATABASE_DIRS, because this database is fixed
# infrastructure rather than a user database: it holds the per-database syncrepl
# bind identities that used to live in the replicated data tree, it exists on
# every pod including read-only replicas, and it must be there before the
# SlapdCluster controller can add the database to cn=config (back-mdb does not
# create olcDbDirectory).
#
# Staying out of DATABASE_DIRS is the point, not an omission. DATABASE_DIRS is a
# pod-template env var, so a change to it rolls the StatefulSet (ADR-013); a
# fixed name written here changes never and therefore rolls nothing.
#
# It also cannot collide with a user database's /data/<dbname>. DATABASE_DIRS
# entries are SlapdDatabase CR names, and a Kubernetes object name is an RFC
# 1123 subdomain — it must start with a lowercase alphanumeric — so no CR can
# ever be named "_slaptain-auth". The leading underscore makes the reservation
# structural rather than a convention.
#
# Ownership needs no chown: this script runs as the slapd UID/GID (1024), the
# same identity that later opens the LMDB environment, so mkdir produces a
# directory slapd owns. Idempotent via mkdir -p — this runs on every pod start,
# not only at first boot.
echo "Ensuring auth database directory: $AUTH_DIR"
mkdir -p "$AUTH_DIR"

# ── Create per-database data and accesslog directories ────────────────────────
# Each SlapdDatabase CR gets its own subdirectory under /data/ and, when this pod
# carries the accesslog volume, under /accesslog/ (ADR-019 R1/R3: one accesslog
# DB per replicated data DB, backed by /accesslog/<dbname> inside the single
# accesslog PVC — not a volume per database). back-mdb does NOT create
# olcDbDirectory, so the directory must exist before the operator adds the DB.
# The accesslog side is gated on the mount existing: read-only replicas never
# get that volume (they produce no changes).
#
# This loop runs at pod START, so it sees whatever the ConfigMap held when the
# pod was created. That is deliberate and sufficient: a database added while
# this pod is running is provisioned by the operator's mkdir Job (ADR-013
# amendment of 2026-09-28), and the next start picks the list up for free.
# Until that amendment it was a rolling restart that kept the list current.
DATA_DIRS=$(read_dir_list "$DATABASE_DIRS_PATH/data-dirs")
LOG_DIRS=$(read_dir_list "$DATABASE_DIRS_PATH/accesslog-dirs")

# Fallback for a new image under a pre-2026-09-28 operator, which sends one
# comma-separated list used for both roots.
if [[ -z "$DATA_DIRS$LOG_DIRS" && -n "$DATABASE_DIRS" ]]; then
    echo "No $DATABASE_DIRS_PATH; falling back to the DATABASE_DIRS env var"
    DATA_DIRS=$(echo "$DATABASE_DIRS" | tr ',' '\n' | tr -d '[:space:]')
    LOG_DIRS="$DATA_DIRS"
fi

for dir in $DATA_DIRS; do
    echo "Ensuring data directory: $DATA_DIR/$dir"
    mkdir -p "$DATA_DIR/$dir"
done
# Gated on the mount existing: read-only replicas never get that volume.
if [[ -d "$ACCESSLOG_DIR" ]]; then
    for dir in $LOG_DIRS; do
        echo "Ensuring accesslog directory: $ACCESSLOG_DIR/$dir"
        mkdir -p "$ACCESSLOG_DIR/$dir"
    done
fi

if [[ ! -d "$CONFIG_DIR/slapd.d/cn=config" ]]; then
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

    # ── ServerID directive (ADR-011, sid-1-per-default; ADR-017, bare form) ────
    # Emit this pod's OWN serverID as a bare integer — unconditionally for every
    # RW pod, even a standalone single-replica cluster ("serverID 1"). cn=config
    # is node-local (ADR-002), so a pod only ever needs its own ID: the integer
    # is the identity stamped into every CSN this pod writes. sid = base +
    # ordinal + 1; the ordinal is the last segment of the StatefulSet pod name
    # ($HOSTNAME, e.g. "slapd-2" → 2). No URL, no peer list — the bare form
    # sheds the FQDN/cluster-domain self-match that made serverID a boot-time
    # crash surface (ADR-015). Carrying the sid from birth keeps CSN history
    # uniform (no sid-0 epoch). This block is only the fresh-bootstrap fast
    # path: the operator reconciles olcServerID to the ordinal-derived value at
    # runtime (ensureServerIDs), because this script never touches an existing
    # config again.
    if [[ "$READONLY_REPLICA" != "true" ]]; then
        sid_base="${LDAP_SERVER_ID_BASE:-0}"
        ordinal="${HOSTNAME##*-}"
        if ! [[ "$ordinal" =~ ^[0-9]+$ ]]; then
            echo "FATAL: cannot derive pod ordinal from HOSTNAME='$HOSTNAME'" >&2
            exit 1
        fi
        sid=$((sid_base + ordinal + 1))
        echo "" >> "$TMP_CONF"
        echo "serverID ${sid}" >> "$TMP_CONF"
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

    # ── back-mdb backend section (ADR-024 R2, bootstrap-time) ──────────────────
    # `backend mdb` configures the BACKEND (olcBackend={0}mdb), not a database:
    # slapd initialises it before any database exists, and idlexp governs the
    # on-disk index layout, so it must be set before data is loaded. This whole
    # block only runs on a fresh /config volume — the operator never converges
    # it, and changing SlapdCluster.spec.backend.idlExponent later only affects
    # pods bootstrapped afterwards. See the field's godoc for the recreate path.
    #
    # Must come after the global directives and before the database sections;
    # slaptest rejects a backend stanza that follows a database stanza.
    if [[ -n "${LDAP_MDB_IDL_EXP:-}" ]]; then
        cat <<EOF >> "$TMP_CONF"

backend mdb
idlexp ${LDAP_MDB_IDL_EXP}
EOF
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

    # Subdir under $CONFIG_DIR: slaptest -F requires an existing, empty target
    # dir, and ext4-backed PVCs (e.g. OpenEBS) always have lost+found at the
    # volume root.
    mkdir -p "$CONFIG_DIR/slapd.d"
    slaptest -f "$TMP_CONF" -F "$CONFIG_DIR/slapd.d"
    rm -f "$TMP_CONF"
fi

# ── Converge the startup-only cn=config attributes (ADR-034) ──────────────────
# Runs on EVERY pod start, fresh config or not. A handful of slapd settings
# cannot be written to a running server — olcSpSessionlogSource is a double free
# that no release fixes — and this is the only moment the configuration is on
# disk with nothing holding it open. slapcat reads it, slapmodify writes it;
# neither runs the configuration parser, so the online path is never taken.
#
# This retires the invariant that the init container never touches an existing
# configuration. It is retired deliberately and narrowly: the blast radius is
# exactly the table compiled into startup_config.py, which is why the table is
# declared rather than inferred. The converger refuses on doubt and verifies by
# reading back, because the error it must never produce is a pod that will not
# start.
#
# It runs AFTER the generation block above, so a configuration written moments
# ago is converged in the same pass if the table already applies to it.
echo "Converging startup-only configuration (table: $STARTUP_CONFIG_PATH)"
STARTUP_CONFIG_PATH="$STARTUP_CONFIG_PATH" \
    python3 /usr/local/bin/startup_config.py "$CONFIG_DIR/slapd.d"

echo "Bootstrap complete."
ls -R "$CONFIG_DIR/slapd.d"
