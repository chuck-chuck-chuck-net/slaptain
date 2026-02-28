# Cluster Bootstrap

This document explains how a new `SlapdCluster` is bootstrapped: from an empty namespace to a
running, accessible LDAP directory.

---

## Two Phases of Bootstrap

Bootstrap is split between two components: the **init container** (configuration) and the
**operator** (directory data). The split is deliberate and necessary — see
[Why Not slapadd with Replication](#why-not-slapadd-with-replication).

### Phase 1: Init Container — Configuration

The `slapd-init` init container runs before the main slapd process starts. Its sole job is
configuration:

1. Generates a temporary `slapd.conf` from environment variables injected by the operator.
2. Runs `slaptest -f slapd.conf -F /ldap-config` to convert to `slapd.d` (LDIF directory format).
3. The resulting `cn=config` tree is written to the `/ldap-config` PVC (or emptyDir when
   persistence is disabled).

The generated configuration includes:

- Module loading (`back_mdb`, plus `accesslog` + `syncprov` when replication is enabled)
- Schema includes (`core`, `cosine`, `inetorgperson`, `nis`)
- TLS directives (when `spec.ldap.tls.enabled=true`)
- Config database (`cn=config`) with `rootpw` set to the root password hash
- Accesslog database (`cn=accesslog`) — only when replication is enabled
- Main data database (`suffix`, `rootdn`, `rootpw`, `directory`)
- Replication overlays (`accesslog`, `syncprov`) and `syncrepl` entries for each peer pod —
  only when replication is enabled

The init container is **idempotent**: if `$CONFIG_DIR/cn=config` already exists, the entire
generation block is skipped. Data is only wiped when `spec.ldap.forceRebootstrap=true`.

### Phase 2: Operator — Directory Data

Once the main slapd container is running and pod-0 reports Ready, the operator's
`reconcileBootstrap` function connects to pod-0 via a live LDAP connection and adds the minimal
initial entries:

| DN | objectClasses | Purpose |
|---|---|---|
| `<domain>` | `top`, `dcObject`, `organization` | Root / suffix entry |
| `cn=admin,<domain>` | `simpleSecurityObject`, `organizationalRole` | Data admin account |
| `cn=replication,<domain>` | `simpleSecurityObject`, `organizationalRole` | Syncrepl bind account (replication only) |

Going through a live slapd connection — rather than `slapadd` — ensures the `accesslog` overlay
captures every write, which is required for delta-syncrepl. See
[Why Not slapadd with Replication](#why-not-slapadd-with-replication).

---

## Why Not slapadd with Replication?

`slapadd` writes directly to the LMDB database files, **bypassing all overlays**. In a setup
with the `accesslog` overlay, this means `slapadd` writes are invisible to the accesslog
database.

When a replica's syncrepl consumer connects to the provider and asks for changes since CSN `X`,
the provider searches its accesslog for entries with `reqStart >= X`. If the provider's data
database has entries that were never recorded in the accesslog (because they were loaded via
`slapadd`), the provider has no delta entries for those objects. This causes the notorious error:

```
consumer has state info but provider doesn't!
```

The fix: **never load initial data via slapadd when the accesslog overlay is active**. Instead,
use a live LDAP connection so every write goes through the overlay and is recorded in the
accesslog.

**Standalone mode** (single replica, `replication.enabled=false`) is not affected — no accesslog
overlay is configured. `slapadd` is safe there and is used for performance (no slapd process
needed during initialization). The init container calls `slapadd` unconditionally in standalone
mode.

---

## Credential Flow

Bootstrap depends on Kubernetes Secrets managed by the operator's `reconcileSecret` function.

### Auto-generated credentials (default)

When `spec.ldap.credentialsSecretName` is not set, the operator auto-generates credentials on
first reconcile:

**`<name>-credentials`** — plaintext passwords
- Keys: `admin-password`, `root-password`
- Used by: the operator for LDAP bind during bootstrap (and future monitoring/topology ops)
- Create-only: never updated after creation

**`<name>-passwords`** — SSHA-hashed passwords
- Keys: `admin-password-hash`, `root-password-hash`
- Used by: the init container (injected as env vars into `slapd.conf` `rootpw` directives)
- Derived from the same source passwords as `<name>-credentials`, guaranteeing consistency
- Create-only: never updated after creation

### User-provided credentials

Set `spec.ldap.credentialsSecretName: my-secret` to supply your own credentials. The Secret must
contain `admin-password` and `root-password` keys with plaintext values. The operator reads these
and derives SSHA hashes to create `<name>-passwords`. The `<name>-credentials` Secret is not
created in this case.

```bash
kubectl create secret generic my-ldap-credentials \
  --from-literal=admin-password=changeme \
  --from-literal=root-password=changeme
```

### Replication secret

When `spec.replication.enabled=true`, the operator also creates:

**`<name>-replication`** — key `password` (plaintext, randomly generated)
- Used by: the init container (`credentials=` in syncrepl directives)
- Used by: the operator (to add the `cn=replication,<domain>` entry during bootstrap)
- Create-only: never updated after creation

---

## The BootstrapComplete Flag

`status.bootstrapComplete` is a persistent boolean on the `SlapdCluster` status subresource.
Once set to `true`, `reconcileBootstrap` returns immediately without contacting the cluster.

`status.phase` is set to `Bootstrapping` (not `Running`) until both conditions hold:
- All desired replicas are Ready
- `status.bootstrapComplete == true`

**Idempotency within a single bootstrap attempt:** before adding any entries,
`reconcileBootstrap` performs a base-scope LDAP search for the root entry (`<domain>`). If it
exists, bootstrap is considered complete and `status.bootstrapComplete` is set without adding
anything. This means the function is safe to call repeatedly and on operator restarts.

---

## forceRebootstrap

Setting `spec.ldap.forceRebootstrap=true` causes the init container to delete the entire
`/ldap-config`, `/ldap-data`, and (when replication is enabled) `/ldap-accesslog` directories
before regenerating configuration. This is destructive and irreversible.

The pod must restart for this to take effect:

```bash
kubectl rollout restart statefulset/<name> -n <namespace>
```

After the pod restarts:
1. The init container runs again with a clean slate and regenerates `cn=config`.
2. Because `/ldap-data` is empty, `ldapEntryExists` returns `false` on the next operator
   reconcile.
3. However, `status.bootstrapComplete` is a persistent status field and is not automatically
   cleared. To re-run operator-side bootstrap, delete and re-create the `SlapdCluster` CR, or
   manually patch the status:

```bash
kubectl patch slapdcluster/<name> -n <namespace> \
  --subresource=status --type=merge \
  -p '{"status":{"bootstrapComplete":false}}'
```

---

## Standalone vs Replicated — Summary

| Step | Standalone (`replication.enabled=false`) | Replicated (`replication.enabled=true`) |
|---|---|---|
| Init: generate slapd.conf | Yes | Yes (includes accesslog + syncprov + syncrepl) |
| Init: slapadd base entries | **Yes** (direct, fast) | **No** (bypasses accesslog) |
| Operator: wait for pod-0 Ready | — | Yes |
| Operator: LDAP connect to pod-0 | — | Yes (pod IP, port 1024) |
| Operator: add root + admin entries | — | Yes |
| Operator: add `cn=replication` entry | — | Yes |
| Other pods: sync from pod-0 | — | Yes (delta-syncrepl, automatic on startup) |

---

## Connection Strategy

The operator connects to pod-0 **directly via its pod IP** on the container LDAP port (1024),
not through the ClusterIP Service. Reasons:

1. **No cluster-domain discovery needed.** The ClusterIP Service is always
   `<name>.<namespace>.svc.<cluster-domain>`, but resolving this from inside the operator pod
   can fail on clusters with non-standard domain suffixes. The pod IP is always available once
   the pod is scheduled and running.

2. **Direct targeting.** Bootstrap must always reach pod-0 specifically (the StatefulSet seed
   pod). Routing through a ClusterIP Service could in theory route to another pod.

The operator binds as the **data rootdn** (`cn=admin,<domain>`) with the plaintext password from
`<name>-credentials`. OpenLDAP's `rootdn`/`rootpw` mechanism allows authentication at the
configuration level — the bind succeeds even before the LDAP entry for `cn=admin,<domain>`
exists in the database, because it's resolved against `slapd.conf`, not the directory.
