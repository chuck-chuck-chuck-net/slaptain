# Backup and Restore

slaptain backs up each database's **data tree (DIT)** to S3 and restores it into
a freshly created database, or into an existing one. The design rationale and rejected alternatives are
in [ADR-014](adrs/adr-014-s3-backup-restore.md); this document is the user-facing
how-to.

## At a glance

| You want to… | Use | Notes |
|---|---|---|
| Back up a database now | `SlapdBackup` | one-shot; `slapcat`→gzip→S3 |
| Back up on a schedule | `SlapdScheduledBackup` | cron + retention (`maxCount`/`maxAge`) |
| Restore into a **new** database | `SlapdDatabase.spec.bootstrapFrom` | one-shot, mutually exclusive with `seed` |
| Roll back an **existing** database | `SlapdRestore` | imperative, in-place, repeatable — **a true rollback only without external peers**, see below |
| Replace a lost pod | *no backup needed* | delete the pod + its PVCs; syncrepl refills it from peers |
| Roll back a **replicated deployment** | mesh-wide runbook | every site, in the right order — the operator does not orchestrate it |

All four use the same S3 `storage` block (bucket / endpoint / region /
`credentialsSecretName`) and the same gzipped-`slapcat`-LDIF artifact format,
which is interchangeable with stock `slapcat`/`slapadd`. Restores are
*destroy-last* (validated before any data is touched) and load every pod
directly, so a multi-replica restore needs no syncrepl refresh. Requires the
operator running with `OPERATOR_IMAGE` set (it is, in the shipped Helm chart).

## Which operation do you actually need?

Three different situations are easy to confuse, and reaching for the wrong one is
the most common way to be surprised. Only the third is a rollback.

**1. A pod lost its data** (disk failure, deleted PVCs). Delete the pod and its
PVCs and let the StatefulSet recreate them; syncrepl refills the pod from its
peers. No backup is involved. `bootstrapFrom` is the *wrong* tool here — it would
seed stale data that the mesh then heals anyway. See ADR-012.

**2. You need a database populated from an artifact** — a new site, a new
database, a migration, or re-seeding a damaged site quickly. That is
`bootstrapFrom` (or `SlapdRestore` on an existing database). On a cluster with
live external peers the result converges to **the mesh's current state**, not the
artifact's: the load is local and fast, and syncrepl then carries only the delta,
which is exactly what you want when the alternative is a full initial sync across
a WAN. It is not a rollback.

**3. You need the directory as it was at a point in time.** That is a rollback,
and on a replicated deployment it is a **mesh-wide** operation — see
[Rolling back a replicated deployment](#rolling-back-a-replicated-deployment)
below. Restoring a single site does not roll the deployment back.

If your deployment is a single cluster with no external peers, 2 and 3 are the
same thing and `SlapdRestore` does what you expect.

## What is and isn't backed up

Only the **data DIT** under each `SlapdDatabase` suffix is backed up. Everything
else — schemas, ACLs, replication topology, overlays, TLS — lives in `cn=config`
and is reconstructed by the operator from the `SlapdCluster` / `SlapdSchema` /
`SlapdDatabase` CRs. So a restore is: recreate the CRs, then load the DIT from a
backup.

The artifact is a **gzipped `slapcat` LDIF** — the same format the legacy on-VM
backup produces, so artifacts are mutually restorable with stock
`slapcat`/`slapadd`.

## S3 credentials

All S3 access uses a Kubernetes Secret referenced by name, with two plaintext
keys:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-s3-creds
type: Opaque
stringData:
  access-key-id: AKIA...
  secret-access-key: ...
```

The MVP supports static key credentials against AWS S3 and any S3-compatible
store (Ceph RGW, versitygw, …) via a custom endpoint. (IRSA / workload identity
is a planned addition.)

## On-demand backup — `SlapdBackup`

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdBackup
metadata:
  name: nightly-2026-06-08
spec:
  databaseRef: my-database        # the SlapdDatabase to back up (same namespace)
  storage:
    bucket: my-ldap-backups
    prefix: prod                  # optional; object key is <prefix>/<cluster>/<db>/<ts>.ldif.gz
    endpoint: ""                  # empty for AWS S3; set for S3-compatible stores
                                  # (must resolve from BOTH the backup/restore
                                  # Jobs' namespace AND the operator's namespace —
                                  # the operator validates restores and prunes
                                  # retention inline; for an in-cluster S3 service
                                  # use an FQDN like svc.namespace.svc)
    region: eu-central-1
    credentialsSecretName: my-s3-creds
  # compression: gzip             # only value supported
  # podAffinity: true             # see "Scheduling" below
```

The operator creates a co-located Job that runs `slapcat | gzip` against pod-0's
data PVC and uploads to S3. Watch it:

```bash
kubectl get slapdbackup nightly-2026-06-08 -o wide
# PHASE: Pending → Running → Completed   (status.path holds the S3 object key)
```

A completed `SlapdBackup` is an immutable record. Failed backups stay `Failed`
for inspection (check the `<name>-backup` Job's logs).

### What an artifact actually is — and what the record tells you

An artifact is **one pod's view of the DIT at one moment**: the Job always
`slapcat`s pod-0. On a converged cluster that is the whole truth. On a
replicating cluster that is momentarily behind, it is not — a write ACKed
through the ClusterIP Service may have landed on another pod and not yet reached
pod-0, in which case it is legitimately absent from the artifact. This is
inherent to backing up one replica of a replicating set, not a fault, and it is
most likely on a freshly created or freshly restarted cluster, where consumer
sessions can still be inside their retry window. It happened to us on
2026-09-12; see the ADR-014 amendment of that date.

A backup therefore always runs — it is never gated, delayed or refused on
replication health — and records the circumstances it ran under:

```bash
kubectl get slapdbackup nightly-2026-06-08 \
  -o jsonpath='{.status.sourcePod}{"\n"}{.status.sourceContextCSN}{"\n"}'
kubectl get slapdbackup nightly-2026-06-08 -o yaml | grep -A5 SourceConverged
```

| Field | What it tells you |
|---|---|
| `status.sourcePod` | Which pod the bytes came from. |
| `status.sourceContextCSN` | That pod's `contextCSN` vector when the Job was created — the artifact's place in the replication timeline. The LDIF embeds the same vector; this is the copy you can query without downloading the object. |
| condition `SourceConverged` | What the `SlapdCluster` said about replica convergence at the time (it mirrors the cluster's own `ReplicationConverged` condition, with its message and timestamp). `NotReplicated` means there was nothing to be current with. |

`SourceConverged=False` does not mean the artifact is bad — it means the source
was known to be behind some peer, and a write accepted elsewhere in the seconds
before the backup may not be in it. Treat it as the first thing to check when a
restored tree is missing something you are sure was written. The verdict is only
as fresh as the cluster's CSN monitoring tick (60 s), and an idle database reads
converged even across a broken link (ADR-008 amendment) — it is a recorded
observation, not a guarantee.

### Scheduling

The backup Job is pinned (required PodAffinity) to the node running pod-0 so it
can mount that pod's `ReadWriteOnce` data PVC. On a cordoned/full node the Job
may stay `Pending` until co-location is possible. Set `spec.podAffinity: false`
to drop the pin (a node able to mount the PVC must then be available).

## Scheduled backups — `SlapdScheduledBackup`

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdScheduledBackup
metadata:
  name: nightly
spec:
  databaseRef: my-database
  schedule: "0 2 * * *"           # standard 5-field cron
  # suspend: false
  # immediate: false              # also fire once on creation
  storage:
    bucket: my-ldap-backups
    region: eu-central-1
    credentialsSecretName: my-s3-creds
  retention:
    maxCount: 14                  # keep the 14 most recent successful backups
    maxAge: "720h"                # and/or delete anything older than 30 days
```

Each tick creates an owned `SlapdBackup`. Retention prunes **completed** backups
beyond `maxCount` or older than `maxAge`, deleting the S3 object first, then the
`SlapdBackup` object. Running/pending/failed backups are never pruned.

## Restore — `SlapdDatabase.spec.bootstrapFrom`

Restore is modelled as a **bootstrap source**: a *newly created* database loads
its DIT from a backup instead of being seeded. It is one-shot and mutually
exclusive with `spec.seed`.

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdDatabase
metadata:
  name: my-database
spec:
  clusterRef: my-cluster
  suffix: dc=example,dc=org
  replication: { enabled: true, ridBase: 100 }
  bootstrapFrom:
    backupRef: nightly-2026-06-08     # a SlapdBackup in this namespace
    # — or restore directly from an object, e.g. a legacy slapcat dump:
    # s3:
    #   storage: { bucket: my-ldap-backups, region: eu-central-1, credentialsSecretName: my-s3-creds }
    #   key: prod/legacy/2026-06-01.ldif.gz
```

### What restore does (and the downtime it costs)

`slapadd` is offline-only, so a restore is a **deliberate, cluster-wide service
interruption** — the same downtime as a manual `slapcat`/`slapadd` recovery,
just driven by the operator. The cluster, while restoring, reports
`status.phase: Restoring` and `status.restore`:

1. The empty database is created normally (in `cn=config`).
2. The operator scales the StatefulSet(s) to **0** and waits for the data PVC to
   be released.
3. A per-database restore Job downloads the LDIF, wipes the (empty) target DB
   files, and runs offline `slapadd` into pod-0's data PVC.
4. On success the operator marks the database restored and scales back up. The
   artifact is loaded into **every** pod directly, so all replicas come up
   identical and no syncrepl refresh is needed.
5. If a restore Job fails, the cluster is **held at 0 replicas** with a loud
   status for inspection rather than scaling up a half-restored directory.

`bootstrapFrom` populates a *new* database only — it never overwrites a
populated one. To roll back an **existing** database to a backup, use a
`SlapdRestore` (below). A no-downtime online (`ldapadd`-based) restore mode for
small databases is planned (see ADR-014).

### Replication-password verification

A restored DB **keeps the backup's `cn=replication` password** — the operator
never rewrites it (that would be a hidden password rotation, which slaptain does
not support). So for a replicated cluster, the target's `<db>-credentials`
Secret must carry the *source's* `replication-password`, or intra-cluster
syncrepl will silently fail to authenticate after the restore.

To catch this before it costs you downtime, every slaptain backup stamps the
`{SSHA}` hash of its `cn=replication` password onto the S3 object's
`replication-pw-hash` metadata. Preflight verifies the cluster's
`replication-password` against it and **default-denies**: a mismatch *or* a
non-`{SSHA}` hash scheme fails the restore before anything is wiped.

- A **foreign/legacy dump** (uploaded outside slaptain) has no such metadata and
  is never checked — provide matching credentials and you're fine.
- To restore despite a failing check (a deliberate mismatch you'll repair by
  hand, or an unverifiable hash scheme), set
  `spec.bootstrapFrom.skipReplicationPasswordCheck: true`. The operator logs the
  bypass; if the passwords don't actually match, repairing `cn=replication` is
  then on you.

For a **cross-cluster** restore into a fresh replicated cluster, pre-create the
target's `<db>-credentials` Secret with the *source's* `replication-password`
(copy it from the source's `<db>-credentials`) before the database is created —
otherwise the operator generates a fresh random one that won't match the backup,
and preflight will (correctly) default-deny.

## In-place restore — `SlapdRestore`

A `SlapdRestore` loads a backup into an **existing, populated** database. It is
imperative and immutable — a command, not desired state — so to restore again you
create another `SlapdRestore`.

**What it means depends on your topology, and this is the part to get right:**

- **No external peers** (a single cluster, replicated or not): it is a true
  rollback. "Someone broke the directory; restore yesterday's backup" does
  exactly what you expect.
- **A member of a cross-cluster mesh** (`spec.replication.externalPeers` is set):
  it is a **local re-seed, not a rollback**. The data is loaded locally, and as
  each pod rejoins the mesh its peers replay every change made since the backup,
  so the cluster converges back to the mesh's current state. On a healthy mesh
  member that is close to a no-op. Its value is re-seeding a *damaged* site from
  a local artifact so syncrepl need only carry the delta instead of a full sync
  across the WAN. The operator emits a warning event and records this in
  `status.message` when the target has external peers.

The reason is not a defect and not fixable by trying harder: `slapadd` restores
each entry with its **original CSN** — the property that lets a restored replica
resume delta-syncrepl instead of forcing a full refresh — so restored entries are
by construction *older* than any post-backup change that still exists elsewhere in
the mesh, and syncrepl resolves per entry in favour of the newest. Restoring one
member of a replicated set does not roll the set back; the set heals the member.
This is how replicated databases generally behave. To actually go back in time,
see the mesh-wide runbook below. Full reasoning: ADR-014, amendment 2026-08-24.

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdRestore
metadata:
  name: rollback-2026-06-09
spec:
  databaseRef: my-database          # an existing SlapdDatabase in this namespace
  source:
    backupRef: nightly-2026-06-08   # a SlapdBackup, or:
    # s3:                           # a direct object (e.g. a legacy dump)
    #   storage: { bucket: my-ldap-backups, region: eu-central-1, credentialsSecretName: my-s3-creds }
    #   key: prod/legacy/2026-06-08.ldif.gz
  # skipReplicationPasswordCheck: true   # same escape hatch as bootstrapFrom
```

The SlapdCluster controller picks it up and drives the **same machine** as
`bootstrapFrom`: preflight (cluster stays up) → scale the cluster to 0 → offline
`slapadd` of the artifact into **every** pod (so all replicas come up identical,
no syncrepl refresh) → scale back up. Watch `status.phase`
(`Pending`→`Preflight`→`Restoring`→`Completed`, or `Failed`).

- It is the **same cluster-wide downtime** as a `bootstrapFrom` restore —
  `slapadd` is offline.
- Only **one restore runs per cluster at a time**; additional `SlapdRestore`s (or
  a `bootstrapFrom`) queue behind the active one.
- The same replication-password verification applies. For an in-cluster rollback
  the backup's `cn=replication` password matches the cluster's own, so preflight
  passes; for a backup from another cluster, the cross-cluster note above applies.
- On `Failed` (a `slapadd` Job failed) the cluster is **held at 0 replicas** for
  inspection rather than scaling up a half-restored directory.

## Rolling back a replicated deployment

A point-in-time rollback of a cross-cluster deployment is a **mesh-wide**
operation, performed by hand. The operator deliberately does not orchestrate it:
driving other clusters would require a control plane, which is in tension with
this project's requirement that each site be autonomous. See the ADR-014
amendment for that reasoning; if it is ever built it gets its own ADR.

**The invariant that makes it work, and the only thing you must not get wrong:**

> No site that still holds post-backup data may be reachable by a site that has
> already been restored.

Break that and the surviving site replays its newer changes into the restored one
and undoes the rollback — quietly, because every CR still reports success. The
per-site cleaning is already handled for you (each restore wipes `/data` and
`/accesslog` on every RW pod); what the runbook has to guarantee is the *global
ordering*.

1. **Stop every site first.** Delete the `SlapdCluster` on every site, and its
   PVCs. `cn=config` is node-local and fully reconstructed by the operator from
   the CRs (ADR-002/ADR-004), so nothing of value is lost — the directory data
   comes from the artifact.
2. **Verify no slapd pod is running anywhere**, across all sites, before
   continuing. This is the step that enforces the invariant.
3. **Keep `<db>-credentials`.** That Secret carries the `replication-password`
   and deliberately has no owner reference, so it survives CR deletion. Preflight
   verifies it against the hash stamped on the backup object, so if you *do*
   delete it, pre-create it with the source's password before recreating the
   database or the restore will (correctly) default-deny. (`<name>-config-password`
   is owned and will simply be regenerated — that one is fine to lose.)
4. **Recreate `SlapdCluster` + `SlapdDatabase` with `bootstrapFrom`** pointing at
   the same artifact, on every site.
5. **Verify convergence**: `slctl inspect -n <ns> <cluster>` on each site, and
   check that `contextCSN` matches across sites.

For a large directory, loading the artifact locally at every site is usually
faster than restoring one site and letting the others take a full initial sync
across the WAN. Both end in the same state.

A note on confidence: the individual operations here (backup, `bootstrapFrom`,
per-site restore) are covered by e2e. The **mesh-wide sequencing is not** — it is
derived from the replication mechanics described above and from operational
practice. Treat step 2 as the checkpoint that matters.

## Testing locally

The e2e backup tests use **versitygw** (`ghcr.io/versity/versitygw`, Apache-2.0)
as a lean in-cluster S3 target — not MinIO. Deploy it and run the gated specs:

```bash
kubectl apply -n slaptain-testing -f tests/resources/versitygw.yaml
E2E_BACKUP=1 ./tests/e2e.sh all <kube-context>
# or, against an already-deployed cluster:
kubectl apply -n slaptain-testing -f tests/resources/versitygw.yaml
E2E_BACKUP=1 make e2e-run
```

`tests/resources/versitygw.yaml` is ephemeral (emptyDir) and pre-creates the
`slaptain-backups` bucket. Point `SlapdBackup.spec.storage` at
`http://versitygw:7480` with the `versitygw-creds` Secret.
