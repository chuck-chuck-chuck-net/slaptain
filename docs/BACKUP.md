# Backup and Restore

slaptain backs up each database's **data tree (DIT)** to S3 and restores it into
a freshly created database. The design rationale and rejected alternatives are
in [ADR-014](adrs/adr-014-s3-backup-restore.md); this document is the user-facing
how-to.

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
4. On success the operator marks the database restored and scales back up; the
   other replicas come up empty and initial-sync from pod-0.
5. If a restore Job fails, the cluster is **held at 0 replicas** with a loud
   status for inspection rather than scaling up a half-restored directory.

Restore populates a *new* database only — it never overwrites a populated one.
A no-downtime online (`ldapadd`-based) restore mode for small databases is
planned (see ADR-014).

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
