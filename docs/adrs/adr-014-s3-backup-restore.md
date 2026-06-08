# ADR-014: S3 backup and restore

**Status:** Proposed
**Date:** 2026-06-08

## Context

A slaptain consumer asked for a backup (and restore) capability targeting S3.
This is also a production-readiness item for the prod migration (see
`docs/MIGRATION-PLAN.md`): the legacy on-VM OpenLDAP it replaces is backed up
daily, and slaptain must not regress that guarantee.

Three facts shape the design.

### 1. What the legacy setup does today

The conventional legacy OpenLDAP backup is a scheduled cron job
Its LDAP path is, in effect:

```
slapcat | gzip > dump.ldif.gz   # then upload to an S3-compatible store
```

So the legacy setup takes a **logical `slapcat` LDIF**, gzips it, and ships it to an
S3-compatible store. It runs `slapcat` against the **live** slapd (no stop).
There is **no restore automation** — recovery is a manual `slapadd`.

The artifact format matters: if slaptain emits the *same* gzipped-slapcat-LDIF
to the *same* kind of store, legacy backups and slaptain backups become mutually
restorable. That directly de-risks the cutover.

### 2. slaptain's backup surface is small

Almost everything in a slaptain cluster is already declarative and
reconstructable by the operator from CRs:

- **`cn=config`** (schemas, ACLs, replication topology, overlays, TLS) is
  node-local (ADR-002) and rebuilt on every reconcile from `SlapdCluster`,
  `SlapdSchema`, and `SlapdDatabase` CRs (ADR-004). It does **not** need backing
  up.
- **N-way multi-master** (Phase 2) means every RW pod converges to the same
  DIT, so a backup taken from **any one pod** is representative.

That leaves exactly one irreplaceable artifact: the **data DIT** under each
`SlapdDatabase` suffix. Everything else comes back from the CRs.

### 3. The mechanical obstacle: getting `slapcat` to the data

`slapcat` reads the LMDB environment directly. Two slaptain facts collide with
that:

- The **runtime image is distroless** (`gcr.io/distroless/base-debian13`) — it
  ships `/usr/sbin/slapd` and nothing else. No `slapcat`, no shell.
- The **data PVC is `ReadWriteOnce`** and is held by the live slapd pod.

The slapd-init image (`debian:trixie-slim`) does carry the OpenLDAP tools,
including `slapcat`/`slapadd`. The PVC question is subtler than it first looks
(see "RWO is per-node, not per-pod" below).

## What this ADR decides

| Dimension | Decision |
|---|---|
| **Backup method** | Logical `slapcat -F /config -b <suffix>` → gzip → S3. Prod-parity artifact. |
| **Backup scope** | The data DIT only, per `SlapdDatabase`. Never `cn=config`. |
| **Executor** | A one-shot Kubernetes **Job** (slapd-init image) co-located with the target pod via *required* PodAffinity, mounting that pod's `config-` and `data-` PVCs. An init container produces the dump into an `emptyDir` staging volume; a second container (operator image) uploads to S3. |
| **Restore model** | `SlapdDatabase.spec.bootstrapFrom.backupRef` — restore into a **fresh** database via offline `slapadd` on pod-0 during bootstrap, then peers initial-sync. Never an in-place mutation of a running DB. |
| **API** | `SlapdBackup` (on-demand) + `SlapdScheduledBackup` (cron + retention) + a shared `storage.s3` struct + the `bootstrapFrom` field above. |

The decisions are grounded in two references: the conventional slapcat/gzip/S3 backup approach (method +
artifact format) and mariadb-operator's `PhysicalBackup` (executor mechanics).

## Backup method

### Chosen: logical `slapcat` LDIF

`slapcat` produces a complete, `slapadd`-restorable LDIF including operational
attributes (`entryUUID`, `entryCSN`, `structuralObjectClass`, …). It streams,
so it scales to the millions-of-entries prod target. It is what the legacy setup already
does, giving artifact parity.

`slapcat` on a live back-mdb database is **safe**: LMDB is multi-process MVCC,
`slapcat` opens a read transaction and sees a consistent point-in-time snapshot
without blocking slapd's writers. Prod relies on exactly this (`slapcat`,
no stop). The one nuance — the dump reflects the instant the read txn opened,
not a frozen whole-run snapshot — is the same semantics the legacy setup already accepts.

### Rejected: network `ldapsearch` dump

An operator that already speaks go-ldap could dump the subtree over the wire.
Rejected because:

- It cannot faithfully reproduce a `slapcat` LDIF (operational-attribute and
  ordering fidelity), so the artifact is **not** prod-compatible and not a
  clean `slapadd` source.
- It is slow and memory-heavy at millions of entries — precisely where the prod
  target lives.

### Rejected (deferred): physical file copy / VolumeSnapshot

A raw copy of the LMDB files, or a CSI VolumeSnapshot of the data PVC, would be
fast and complete. Deferred because:

- Snapshots live in the storage backend, not S3 — reaching S3 needs a mover
  (Velero/VolSync). That is a *different feature* from the "to S3" request and
  carries a CSI-capability dependency.
- A raw LMDB copy is not portable across slapd versions/page sizes and is not
  the prod artifact.

VolumeSnapshot remains a sensible **opt-in alternative method** later (it is
exactly how mariadb-operator structures the choice), but it is not the baseline.

## Executor

The backup must run `slapcat` with filesystem access to the target pod's
`/config` and `/data`. We create a one-shot **Job** to do it.

### RWO is per-node, not per-pod

`ReadWriteOnce` binds a volume to a single **node**; multiple pods on that node
may co-mount it. The single-pod restriction is the separate, opt-in
`ReadWriteOncePod` mode, which slaptain does not use. So a backup Job that
lands on the **same node** as the target slapd pod can mount its `data-` and
`config-` PVCs while slapd holds them. This removes the main objection to a
Job-based executor.

### Chosen: co-located one-shot Job (mariadb-operator pattern)

mariadb-operator's `PhysicalBackup` does exactly this, and the pattern is
battle-tested. We copy it:

- **Co-location** via `PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution`,
  `topologyKey: kubernetes.io/hostname`, matching the target pod's
  `app.kubernetes.io/instance` and `statefulset.kubernetes.io/pod-name` labels
  (both already present on slaptain pods; the latter is set automatically by the
  StatefulSet controller). `Required` (not `nodeName`) so the Job follows the
  pod if it reschedules. mariadb-operator's own comment on this term reads
  *"Required for ReadWriteOnce storage"* — our exact case.
- **Escape hatch**: a `podAffinity *bool` (default `true`) on the backup spec,
  mirroring mariadb-operator, for operators who would rather not co-schedule.
- **Two-step Job**: an **init container** (slapd-init image) runs
  `slapcat -F /config -b <suffix>`, gzips, and writes to an `emptyDir`
  **staging** volume; the **main container** (operator image) uploads the
  staged file to S3 and the Job exits. Staging defaults to `emptyDir` (on the
  node); it is cleaned up with the Job.
- **Target pod**: pod-0 by default (deterministic, matches the seed target of
  ADR-012). Backing up any ready RW pod is equally valid under multi-master;
  selecting a healthy pod when pod-0 is down is a future refinement, not MVP.
- **Mounts**: `config-<c>-<i>` and `data-<c>-<i>`, RW (matching the precedent —
  `slapcat` is read-only at the data level but LMDB registers a reader slot in
  the lock file, so a read-only filesystem mount is avoided).

### Rejected: backup sidecar in every slapd pod

A permanent sidecar sharing `/data` would avoid co-location entirely. Rejected
for MVP: an always-on idle container per pod is ongoing overhead, and triggering
it from the operator without `pods/exec` needs an awkward signalling channel.
(This mirrors the reasoning in ADR-013 against management sidecars.) It stays a
fallback if the cluster forbids co-scheduling Jobs onto data nodes.

### Rejected: backup inline in the reconcile loop

Backups are long-running and must not block reconciliation. All three reference
operators run backups as Jobs/Pods, not inline. We follow suit.

### Note: first Job usage in the operator

Today the operator only creates StatefulSets, Services, and Secrets. This is the
first time it creates `batch/v1` Jobs, which requires new RBAC
(`jobs: create/get/list/watch/delete`).

## Restore

### Chosen: `bootstrapFrom` into a fresh database, offline `slapadd` on pod-0

Restore is modelled as a **bootstrap source**, never a mutation of a running
database — the CNPG pattern. A new `SlapdDatabase` carries
`spec.bootstrapFrom.backupRef` (a `SlapdBackup` name) or a direct S3 path. On
first bootstrap:

1. pod-0's **init container** downloads the LDIF from S3 and runs
   `slapadd -F /config -b <suffix>` into the empty `/data` **before slapd
   starts**. This is fast (bulk load) and matches prod's manual recovery.
2. slapd starts; pods 1..N-1 perform their normal **initial syncrepl refresh**
   from pod-0, exactly as in a fresh-cluster cold start.

`slapadd` bypasses the accesslog overlay, but that is correct here: it runs on
an empty DB before replication is live, and the standard initial full sync
(via the data DB's `syncprov`) populates peers. This is the same path a fresh
cluster takes; it is distinct from the post-dataloss *re-refresh* of an
already-initialized peer that triggers ITS#9580.

`bootstrapFrom` is **one-shot**, consumed exactly once and tracked in status —
the same lifecycle discipline as the seed (ADR-012). Re-applying does not
re-restore.

### Rejected: live `ldapadd` restore (the seed mechanism)

The seed deliberately loads via live `ldapadd` so entries flow through the
accesslog (ADR-012, BOOTSTRAP.md). For restore that is the wrong trade: it is
slow and memory-heavy at scale, and the accesslog-flow benefit is moot on an
empty pre-replication DB. Offline `slapadd` is the correct, prod-matching tool
for bulk restore.

### Rejected (deferred): in-place `SlapdRestore` CRD

A standalone CRD that overwrites a running database's DIT (the
mariadb-operator/Medusa model) is more flexible but destructive against live
data and needs careful guarding. Out of MVP scope; revisit if a real
"roll back this live cluster" need appears.

## API shape

At ADR altitude (full Go types live in `operator/api/v1alpha1/`):

```
SlapdBackup (sb)                       # on-demand; immutable once Completed
  spec.databaseRef.name
  spec.storage.s3 { bucket, prefix, endpoint, region,
                    accessKeyIdSecretKeyRef{name,key},
                    secretAccessKeySecretKeyRef{name,key}, insecureTLS }
  spec.compression: gzip               # only value in MVP
  spec.podAffinity: true               # co-location escape hatch
  status: phase | startedAt | completedAt | path | sizeBytes | error

SlapdScheduledBackup (ssb)             # emits SlapdBackup objects
  spec.databaseRef.name
  spec.schedule: "0 2 * * *"
  spec.suspend / spec.immediate
  spec.storage.s3 { ... }              # same shared struct
  spec.retention { maxCount, maxAge }  # keep-N and/or keep-duration
  status: lastScheduleTime | nextScheduleTime | lastBackupRef

SlapdDatabase.spec.bootstrapFrom       # restore = create-from-backup
  backupRef.name                       # a SlapdBackup, or:
  s3 { ... path ... }                  # direct S3 source
```

The `storage.s3` struct is shared across backup, scheduled backup, and
`bootstrapFrom`. Credentials are always Secret references, never inline — the
universal pattern across CNPG/mariadb/Medusa and consistent with slaptain's
existing Secret conventions.

## Non-goals (MVP)

Deferred to later phases, each a candidate for its own follow-up:

- **PITR.** OpenLDAP has no WAL/binlog stream. The accesslog could approximate
  time-targeted recovery later, but it is out of scope now.
- **VolumeSnapshot / physical method** (see above).
- **GCS / Azure Blob providers.** S3 and S3-compatible (MinIO) only.
- **IRSA / workload-identity credentials.** Static key-based creds only; an
  `inheritFromIAMRole`-style flag is a natural later addition.
- **In-place `SlapdRestore` CRD** (see above).
- **`cn=config` backup** — reconstructed from CRs by design.

## Consequences

### Required implementation work

- **CRD types** (`operator/api/v1alpha1/`): `SlapdBackup`,
  `SlapdScheduledBackup`, the shared `S3StorageSpec`, and the `bootstrapFrom`
  field on `SlapdDatabaseSpec`. Regenerate deepcopy + CRD YAML and sync to
  `charts/operator/crds/` (`make operator-generate operator-manifests`).
- **Controllers**: a `SlapdBackup` controller (build + create the Job, observe
  it, surface status) and a `SlapdScheduledBackup` controller (cron evaluation,
  emit `SlapdBackup` objects, enforce retention by deleting old objects/objects'
  S3 artifacts).
- **Job builder**: the co-located two-container Job (PodAffinity, staging
  emptyDir, slapcat init container, operator-image uploader).
- **S3 client**: add a Go S3 client (e.g. minio-go) and a backup/restore
  subcommand to the operator/`slctl` binary that runs in the uploader container.
- **Restore path**: extend the slapd-init container to handle
  `bootstrapFrom` — download from S3 and `slapadd` on pod-0 before slapd starts;
  track one-shot completion in `SlapdDatabase.status`.
- **RBAC**: grant the operator `batch/v1` `jobs` verbs
  (`create/get/list/watch/delete`) — new for the operator.
- **e2e**: a backup→object-store→restore-into-fresh-cluster round-trip test,
  ideally asserting artifact compatibility with a prod-style `slapcat` dump.
- **Docs**: a backup/restore section (likely `docs/BACKUP.md`), CLAUDE.md
  updates (new CRDs, new Job usage, new RBAC), and a `slctl` backup/restore
  helper if warranted.

### Behavioural notes for users

- A backup Job schedules onto the **same node** as the target slapd pod; on a
  cordoned/full node it may stay Pending until co-location is possible (tunable
  via `podAffinity: false`, at the cost of needing a node that can mount the
  PVC).
- Restore creates a **new** database; it never overwrites a running one.
- Backups capture the data DIT only. Schema, ACLs, and topology are restored by
  re-applying the corresponding CRs, not from the backup.

## Related

- ADR-002 — `cn=config` is node-local and operator-managed (why we don't back it
  up).
- ADR-004 — multi-resource CRD architecture (where `SlapdBackup` /
  `SlapdScheduledBackup` fit, and why schema/ACL/topology are CR-reconstructable).
- ADR-012 — seed is one-shot; the `bootstrapFrom` restore reuses the same
  one-shot lifecycle discipline and pod-0 target.
- ADR-013 — persistent storage is required (a precondition for any
  backup/restore story) and the precedent for declining a permanent sidecar.
- `docs/MIGRATION-PLAN.md` — backup parity with legacy prod is a migration
  readiness item.
