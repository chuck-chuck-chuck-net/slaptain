# ADR-014: S3 backup and restore

**Status:** Accepted (impl + e2e green on t3e 2026-06-09)
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
| **Restore model** | `SlapdDatabase.spec.bootstrapFrom.backupRef` — populate a **newly created** database via offline `slapadd`, driven by a cluster-coordinated scale-to-0 → restore Job → scale-up state machine, then peers initial-sync. Costs a deliberate cluster-wide downtime (inherent to offline `slapadd`). One-shot, mutually exclusive with `seed`. A future online `ldapadd` mode covers the small-DB / no-downtime case. Never an in-place overwrite of a populated DB. |
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

Restore is modelled as a **bootstrap source**, never a mutation of a running
database — the CNPG pattern. A new `SlapdDatabase` carries
`spec.bootstrapFrom.backupRef` (a `SlapdBackup` name) or a direct S3 path.

### The OpenLDAP constraint that drives everything

`slapadd` is the matching restore tool for a `slapcat` dump, and it is
**offline-only**: it opens the LMDB environment exclusively, so it must run with
slapd **stopped**. Running it against a live slapd risks corruption. There is no
"online slapadd". This single fact rules out doing the restore inside a live pod
and dictates the whole mechanism below.

### Chosen: offline `slapadd` via a cluster-coordinated scale-to-0 state machine

The cluster is a StatefulSet owned by the SlapdCluster controller, which
SSA-patches `replicas` from `spec.replicas` every reconcile. So restore cannot
be a side actor poking the STS — the controller would immediately revert any
scale change, and the two would fight forever. Instead, **restore is a
first-class cluster phase**: while `status.phase == Restoring`, the SlapdCluster
controller's desired replica count *is* 0.

The sequence reuses the existing empty-DB creation so `slapadd` has its config
target (DB definition + data dir) with no new `cn=config` logic anywhere:

```
Pending      wait until the empty data DB is defined in cn=config
             (the normal SlapdDatabase controller flow does this)
ScalingDown  STS.replicas := 0; wait until pods are gone and the PVC is released
Restoring    node-pinned Job mounting config-/data- of pod-0:
             download LDIF from S3 → wipe target DB files → slapadd → exit;
             operator watches the Job to completion
ScalingUp    on success: status.restoreApplied=true; phase clears;
             STS.replicas := originalReplicas
Completed    peers come up empty → initial syncrepl refresh from pod-0
```

**Downtime is inherent, deliberate, and documented — not gated on "no users".**
`slapadd` is offline-only, so any `slapadd`-based restore *necessarily* means a
slapd downtime. Because the restore takes the whole StatefulSet to 0, it is a
**cluster-wide service interruption** — it affects every database in the cluster
and any LDAP clients, not just the database being restored. We treat this as a
deliberate operation a human chooses and that we document: it is the same
interruption as today's manual `slapcat`/`slapadd` recovery, with less
visibility and more surprise factor, which the docs must offset. It is **not** a
precondition that the cluster be idle or clientless. (For the small-DB /
no-downtime case, see the online `ldapadd` mode below.)

**The real correctness constraint is about the *database*, not the cluster.**
Why peers do "the right thing": pod-0 comes up with the restored DIT and its
restored `contextCSN`; the peers' copies of *that database* come up **empty**,
see pod-0's `contextCSN` greater than their own (absent), and pull a full
initial refresh. This is the normal cold-start path — distinct from the
post-dataloss *re-refresh* of an already-initialized peer that triggers
ITS#9580. It is correct **only because the peers' copy of the restored database
is empty**. `bootstrapFrom` therefore populates a **newly created** database
only — the cluster may already hold other, populated databases and serve clients
— and is **one-shot** (the same lifecycle discipline as the seed, ADR-012) and
**mutually exclusive with `seed`** (a restore replaces seeding). Overwriting a
database that already holds data on the peers is the deferred in-place-restore
case below, which would additionally have to wipe every peer's copy to avoid a
divergent multi-master re-sync.

Robustness properties that keep the machine from getting stuck:

- **Persist `originalReplicas` + phase *before* scaling down**, so a crashed
  operator resumes from status instead of scaling up into the wrong count.
- **Every transition gates on observed state and is re-entrant.**
- **Idempotent Job**: wipe-then-`slapadd`, so a retried Job is clean (a
  half-loaded DB would otherwise fail `slapadd` on duplicate DNs).
  `restoreApplied` flips only on verified success.
- **Failure policy**: on Job failure, set `RestoreFailed` and leave the STS at
  0 with a loud status. The operation is already a deliberate interruption, so
  halting visibly for human inspection beats scaling up a half-restored DIT into
  a multi-master mesh.
- **Optional**: gate the pre-restore scale at `replicas=1` so peers never come
  up until after restore (cleanest sync); restore all `bootstrapFrom` databases
  in one cluster in a **single** scale-down window, not one bounce per DB.

### Rejected: enforce slapadd-vs-slapd exclusion via the PVC access mode

The tempting shortcut is to create the restore Job and let Kubernetes serialize
it against slapd through volume mounting. It does not work: `ReadWriteOnce`
binds to a *node*, not a *pod*, and explicitly permits same-node co-mount — the
exact property the backup Job relies on. So an RWO restore Job could mount
`data-` *while slapd holds it* and `slapadd` into a live env. `ReadWriteOncePod`
would enforce single-pod, but it breaks the backup co-mount, is a breaking
change to existing PVCs, and still wouldn't stop the controller-ownership fight.
The exclusion belongs in the operator's sequencing, not the volume layer.

### Rejected: offline `slapadd` in the init container

An earlier draft put `slapadd` in pod-0's init container, before slapd starts.
It fails on ordering: the init container only builds `cn=config`
**infrastructure**; the data DB definition is created **live by the
SlapdDatabase controller after slapd is up**. At init time the DB is not defined
in `slapd.d`, so `slapadd -b <suffix>` has no target. Pre-defining it in the
init container would duplicate the controller's cn=config logic into bash and
violate "the operator owns `cn=config`" (ADR-002/003).

### Future complementary mode: online `ldapadd` restore (no downtime)

The two restore methods are complementary, not competing, because they trade off
along the axis that matters: **downtime vs. scale.**

- **Offline `slapadd` (scale-to-0)** — the MVP. Fast and complete at the
  millions-of-entries scale, prod-artifact-compatible, but costs a cluster-wide
  interruption.
- **Online `ldapadd`** — loads the backup over the network into a live cluster,
  entries flowing through the accesslog like the seed path (ADR-012,
  BOOTSTRAP.md). **Zero downtime**, but slow and memory-heavy on large DITs.

The online mode is the right tool for a **small** database, or whenever a
cluster-wide interruption is unacceptable — e.g. adding a modest new database to
a busy cluster. It is deferred (the offline mode covers the load-bearing
migration/DR cases first), but explicitly *not rejected*. When added it would
surface as a mode selector on `bootstrapFrom` (e.g. `mode: offline|online`),
reusing the seed controller's live-`ldapadd` machinery for the online path.

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
- **Online `ldapadd` restore mode** — the no-downtime path for small DBs;
  deferred but explicitly planned (see "Future complementary mode" above), not
  rejected.
- **In-place `SlapdRestore` CRD** (overwrite a populated DB) — see above.
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
- **S3 client**: add a Go S3 client (`aws-sdk-go-v2`, not minio-go) and a backup/restore
  subcommand to the operator/`slctl` binary that runs in the uploader container.
- **Restore state machine**: a `Restoring` phase on `SlapdCluster` that the
  cluster controller honors (desired `replicas := 0` while restoring), plus the
  scale-down → restore-Job → scale-up sequencing with persisted
  `originalReplicas`. The restore Job (slapd-init image, node-pinned to pod-0's
  PVCs) downloads from S3, wipes the target DB files, runs `slapadd`, and exits.
  One-shot completion tracked in `SlapdDatabase.status.restoreApplied`. Admission
  guard: `bootstrapFrom` and `seed` are mutually exclusive.
- **RBAC**: grant the operator `batch/v1` `jobs` verbs
  (`create/get/list/watch/delete`) — new for the operator. (Scaling the STS
  needs no new RBAC; the operator already patches StatefulSets.)
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
- Restore populates a **newly created** database; it never overwrites a
  populated one. It runs as a cluster-coordinated **scale-to-0 → restore →
  scale-up** cycle, which is a **deliberate, cluster-wide service interruption**
  (inherent to `slapadd` being offline-only) — documented and human-initiated,
  the same downtime as today's manual recovery. It does **not** require the
  cluster to be idle. `bootstrapFrom` and `seed` cannot both be set on a
  `SlapdDatabase`.
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

## Revision history

- **2026-06-08 (same-day, pre-acceptance):** reworked the restore decision.
  The initial draft proposed offline `slapadd` in pod-0's **init container**;
  that fails because the data DB is not defined in `cn=config` at init time (the
  SlapdDatabase controller defines it live, after slapd starts). The corrected
  decision drives offline `slapadd` from a **cluster-coordinated scale-to-0
  state machine** so slapd is genuinely stopped and the controllers don't fight
  over `replicas`. The discarded init-container approach and the rejected
  PVC-access-mode shortcut are retained above as rejected options.
