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

**Extended by ADR-018.** The PVC analysis above covers *mount concurrency* and
is correct: a co-located Job may co-mount RWO volumes that slapd holds. It does
not cover *PVC lifecycle*. A pod object that names a PVC blocks that PVC's
deletion for as long as the object exists — finished pods included — so every
co-located Job is also a lease on PVC lifecycle. ADR-018 records the mechanism
and the reaping rules that follow.

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

## Amendment (2026-06-09): in-place restore (`SlapdRestore`) + slapadd-all-pods machine

**Status:** Accepted. Reverses the MVP's "in-place restore — deferred/rejected"
decision and reworks the restore data path. The original sections above are
preserved for history; this amendment supersedes two specific points, called out
inline below.

### Context

The MVP shipped `bootstrapFrom` (restore into a *fresh* database) and deferred
in-place restore (rollback into a *populated* cluster — "someone broke a database,
roll back to yesterday"). Reviewing it on a live cluster surfaced three things:

1. **delete+recreate is a footgun at N≥2.** Deleting a `SlapdDatabase` and
   recreating it with `bootstrapFrom` *appears* to roll back — and does on a
   single replica, because the restore Job wipes pod-0's data before `slapadd`.
   But on a multi-replica cluster the Job only touches pod-0; peers keep the old
   data, and on scale-up multi-master syncrepl resurrects it / diverges. That is
   the exact reason in-place was deferred, and it is the dangerous kind of
   "works in test (N=1), corrupts in prod (N>1)."
2. **Destroy-last.** A restore must not wipe data before it is confident the
   restore can succeed; finding out the backup is unreachable/corrupt *after*
   wiping is unacceptable.
3. **Relying on syncrepl to repopulate peers is the wrong data path.** The
   original "slapadd pod-0, let peers initial-sync" approach transfers the whole
   dataset N−1 times over a per-entry online refresh (slow at millions of
   entries) and — critically — drives peers through the syncrepl **refresh**
   path, which is exactly what triggers upstream **ITS#9580** (refresh fills the
   accesslog with out-of-order CSNs). Restore should not depend on the buggiest
   path in the stack.

### Decisions

**1. Add a `SlapdRestore` CRD for explicit in-place restore.** A restore is an
event/command, not desired state, so it is its own object rather than a field on
`SlapdDatabase` (a spec field for a one-shot imperative action forces clunky
trigger-token patterns, and mutating the DB CR to roll it back is the opposite of
the intent). Precedent: mariadb-operator `Restore`, Medusa `MedusaRestoreJob`.

```
SlapdRestore (sr)                     # imperative, immutable once terminal
  spec.databaseRef                    # an existing (possibly populated) SlapdDatabase
  spec.source.backupRef               # a SlapdBackup, or:
  spec.source.s3 { storage, key }     # a direct object (e.g. a legacy slapcat dump)
  status: phase (Pending|Preflight|Restoring|Completed|Failed) | startedAt | completedAt | message
```

Non-destructive to the CR (metadata preserved), auditable (one object per
restore), guardable (refuse a second concurrent restore). Per the
cluster-owns-the-StatefulSet decision (Architecture A above), the **SlapdCluster
controller** watches `SlapdRestore` — like it already watches `SlapdDatabase` —
and drives the machine; the `SlapdRestore` is just the request.

**2. Rework the restore machine to `slapadd` into *every* pod** (supersedes the
"slapadd pod-0 + peers initial-sync" step of the original Restore section). All
RW *and* RO pods load the same artifact directly and offline, then come up
already-converged — replication moves no bulk data.

- **Efficient:** parallel offline LMDB bulk loads from a local artifact + N cheap
  S3 downloads, instead of N−1 full online syncrepl refreshes.
- **Robust:** no pod takes the syncrepl refresh path, so the restore cannot
  trigger ITS#9580. (See the ITS#9580 write-up; restore deliberately bypasses
  refresh.)
- **Correct:** `slapadd` preserves `entryCSN`/`contextCSN` from the slapcat LDIF,
  so loading the *same* artifact into every pod yields an identical starting
  state → multi-master sees no diffs → instant convergence. Requires `serverID`
  hygiene (ADR-011) so loaded source-CSNs and the restore cluster's new writes
  don't collide.

**3. Preflight before any destruction; peers as the safety net** (this is the
"destroy-last" guarantee).

```
Preflight (cluster still UP — nothing wiped, no downtime):
  download artifact + validate (S3 reachable, creds OK, object present, gunzip
  clean, non-empty LDIF whose first DN == target suffix; optional trial slapadd
  into a scratch dir). Fail here → abort with neither downtime nor data loss.
  Replication-password check: the backup Job stamps the {SSHA} of the source's
  cn=replication userPassword onto the object's `replication-pw-hash` metadata
  (a targeted `slapcat -a '(cn=replication)'`, not a body scan). When the target
  DB replicates, preflight SSHA-verifies the cluster's replication-password
  against that metadata. Default-deny: a mismatch OR a non-{SSHA} scheme fails —
  the restored DB keeps the backup's password (we never rewrite it; that would
  be a hidden rotation), so the target's credentials Secret must carry the
  source's replication-password. A foreign/legacy dump has no such metadata and
  is not checked. `spec.bootstrapFrom.skipReplicationPasswordCheck: true` bypasses
  the check for a deliberate mismatch or unverifiable scheme — the operator logs
  the bypass and the user owns repairing cn=replication if syncrepl then fails.
ScaleDown:  STS (+ RO STS) → 0; wait for pods gone / PVCs released.
Restore:    pod-0 first — wipe the target DB's data dir, then slapadd; confirm.
            Then, in parallel, every other RW + RO pod: wipe target DB dir +
            slapadd the same artifact. The per-pod wipe is the step immediately
            before that pod's own load — never "wipe all, then start loading."
ScaleUp:    STS → original counts. Cluster is already converged.
Abort/fallback:
  - preflight failure: no scale-down, no data touched.
  - pod-0 load failure (despite preflight): peers still hold the old data →
    scale back up on it and mark Failed; no total loss.
  - a peer's direct load failure: that pod alone falls back to syncrepl refresh
    on scale-up (the only residual, single-pod ITS#9580 exposure), or the
    machine aborts and retries.
```

The wipe is scoped to the *target database's* `/data/<dir>` on each pod, so other
databases on the cluster are untouched.

### Consequences

- **`bootstrapFrom` becomes peer-state-agnostic** (free, from decision 2): every
  pod gets a direct load, so a `bootstrapFrom` restore is correct whether the
  cluster is fresh *or* a delete-and-recreated rollback. The N≥2 footgun is gone;
  delete+recreate "is gonna be fine."
- Two triggers, one machine: `SlapdDatabase.spec.bootstrapFrom` (create-time,
  declarative) and `SlapdRestore` (imperative, on a live DB) both feed the same
  preflight → slapadd-all-pods machine.
- Restore is deterministic and observable (discrete per-pod Jobs with clear
  success/failure) rather than waiting on opaque syncrepl convergence.
- New RBAC: `SlapdRestore` + status. The machine still creates only `batch/v1`
  Jobs.
- Supersedes: the original "Restore → Chosen" step "(slapadd pod-0) … peers
  perform their normal initial syncrepl refresh from pod-0"; and the original
  "Rejected (deferred): in-place `SlapdRestore` CRD".
- Still out of scope: PITR, online `ldapadd` mode, GCS/Azure, IRSA.

### Implementation order

1. Rework the machine to preflight + slapadd-all-pods (this alone makes
   `bootstrapFrom` safe at N≥2 and is the foundation).
2. Add the `SlapdRestore` CRD + controller wiring on top (same machine).
3. e2e on a multi-replica cluster: rollback via `SlapdRestore`, and
   delete+recreate `bootstrapFrom`, both asserting no divergence and no refresh.

## Amendment (2026-08-24): restore semantics under cross-cluster replication; rollback is mesh-wide

**Status:** Accepted. Does not reverse the 2026-06-09 amendment — the machine it
built is unchanged and still correct. What changes is what `SlapdRestore` is
allowed to *promise*, and the addition of a mesh-wide rollback procedure that
the operator does not (and for now will not) orchestrate.

### Context

The 2026-06-09 amendment made in-place restore safe at N≥2 *within one cluster*
by loading every pod directly instead of letting syncrepl repopulate peers, and
a later fix (`fix(restore): wipe the accesslog LMDB on restore under
replication`) removed the local accesslog as a source of stale deltas. Both hold.

A multi-site e2e run then failed the accesslog-replay spec: an in-place restore
reported `Completed`, and the restored entry never appeared. The investigation
produced this chain:

1. The artifact was correct — the entry was present in the backup LDIF.
2. The restore loaded it — after `slapadd` the entry existed on the pods.
3. Every local pod then recorded a *delete* of it, staggered in StatefulSet
   scale-up order (~1.5–2 s apart), i.e. as each pod rejoined the mesh.
4. The remote sites still held the original `add` and `delete` in *their*
   accesslogs, which no local wipe ever touched.
5. The restored entry carried its original `entryCSN` from the artifact, some
   seconds *older* than the peers' delete.

**The mechanism, stated generally:** offline `slapadd` replays *historical* CSNs
— that is precisely the property that makes a restored replica able to resume
delta-syncrepl rather than force a full refresh. The restored `contextCSN` is
therefore the backup's high-water mark, so peers ship every delta recorded since,
and per-entry resolution takes the newest CSN. A restored entry is by
construction older than any post-backup change that survives anywhere else in the
mesh. **A single-site restore cannot win that comparison, ever.** This is not a
race, a missed wipe, or a bug in the restore machine; it is the data model.

It also cuts both ways: an entry *deleted* after the backup fails to come back
(demonstrated), and an entry *added* after the backup is not removed (same
mechanism; not separately demonstrated).

This was invisible until now because a single-site deployment has no peers
holding post-backup deltas — which is also why the earlier single-site runs of
this spec were green.

### The three recovery operations

Conflating these is what produced the wrong expectation. They are distinct, and
only the third is a rollback:

| Operation | Tool | Semantics |
|---|---|---|
| Replace one pod / node | delete pod + its PVCs; syncrepl refills (ADR-012 case 2) | Local loss, healed from peers. `bootstrapFrom` is the *wrong* tool: it seeds stale data the mesh then heals anyway. |
| Seed a site or database | `SlapdDatabase.spec.bootstrapFrom` | Loads an artifact into a *fresh* database. On a cluster with live peers it converges to the **mesh's** state — a fast seed that avoids a large initial sync, not a rollback. |
| Roll back in time | mesh-wide destroy, then `bootstrapFrom` everywhere | The only true rollback. Works because no survivor is left holding a newer delta. |

### Decisions

1. **`SlapdRestore` is retained, with its promise restated per topology.** On a
   cluster with no external peers it is a point-in-time rollback (verified green
   on a standalone cluster — `replicas: 1`, replication disabled — 2026-08-24).
   On a mesh member it is a **local re-seed**: the data is loaded locally and
   peers then replay their newer changes as each pod rejoins, so the cluster
   converges back to the mesh's current state. On a *healthy* mesh member that is
   close to a no-op; its value is re-seeding a damaged site from a local artifact
   so syncrepl need only carry the delta, instead of paying a full WAN sync.
2. **Neither refuse nor warn — document.** An earlier draft of this amendment
   had the operator emit a warning when the target cluster has
   `spec.replication.externalPeers`. Dropped. The mesh-member behaviour is the
   *defined contract* — a local re-seed that the mesh then repairs — not a
   degraded outcome, and warning on correct usage is noise that teaches people to
   ignore warnings. Restoring a replica in Postgres, Galera or MongoDB raises no
   warning that it will subsequently follow the cluster, for exactly this reason.
   The distinction is carried by `docs/BACKUP.md`, which is where someone
   reaching for a rollback is looking. Refusing was rejected separately (below).
3. **Rollback of a replicated deployment is a mesh-wide operation, performed by
   a human runbook.** Quiesce or destroy *every* site first, then `bootstrapFrom`
   the same artifact. The sequencing is the load-bearing part: no site may still
   be serving post-backup data while another returns with restored data, or the
   survivor re-propagates its deltas into it. The per-site wipes are already
   handled (the restore Job clears `/data` and `/accesslog` on every RW pod), so
   what the runbook must guarantee is the *global* ordering, not the local
   cleaning. Documented in `docs/BACKUP.md`.
4. **Cross-site orchestration is explicitly not decided here.** See below.

### Options considered

**Online diff-apply (`ldapadd`).** Compute the difference between the live DIT
and the artifact and apply it as ordinary LDAP writes, which carry *fresh* CSNs
and therefore win. Rejected: guaranteeing the consistency of that diff is a large
burden of its own, and it is not what a competent human operator would do. This
operator's job is to automate the sane manual procedure, not to invent a new one.

**Restore one site, force the other sites to re-initialise.** Rejected: it makes
the restored site a single point of failure during recovery, extends downtime by
a full resync per site, and exercises rarely-used paths at precisely the moment
one can least afford to discover defects in them.

**Strip or bump CSNs before `slapadd`.** Rejected, and more firmly than first
proposed. CSN stability is a *feature* of a `slapcat`/`slapadd` restore: with
`entryUUID` and `entryCSN` preserved, a restored replica resumes delta-syncrepl
instead of forcing a full refresh. Moreover the value that actually decides
whether a peer's delta is applied is `contextCSN`, so stripping `entryCSN` would
not even fix the symptom; bumping `contextCSN` past the mesh maximum would make
the restored site silently *ignore* every peer change below it. That is not half
a rollback, it is a rollback plus silent divergence.

**Refuse in-place restore when `externalPeers` is configured.** Rejected. It
would remove the feature from the topology this project exists to serve, and the
behaviour it guards against is unremarkable for a replicated database: restoring
one member of a replicated set does not roll the set back — the set heals the
member. Postgres streaming replication, Galera SST/IST, MongoDB replica-set
initial sync and Cassandra repair all behave this way, and all document the
whole-cluster rollback as a separate fenced procedure. The defect was in our
documentation promising otherwise, not in the behaviour.

**Remove or park `SlapdRestore` entirely.** Considered seriously, and rejected on
two grounds: it is verified working on a standalone cluster, and as an
open-source project most users will not have a multi-cluster deployment at all,
so the feature serves the majority case even though it does not serve this
project's own primary one.

### Not decided: hub-and-spoke orchestration

Automating operation 3 means one actor driving *other* clusters: quiescing them,
running restores there, and sequencing the result. Today the operator reaches
across clusters only to **read** — discovering peer pod addresses through a
remote kubeconfig (ADR-007 amendment, ADR-016). Driving another site is
categorically different: it introduces a control plane, and with it hub failure,
partition behaviour, and arbitration.

That is in direct tension with this project's second architectural requirement,
that each site be autonomous. It is therefore a fundamentals question and gets
its own ADR if it is ever pursued — not a paragraph here. Until then, the
mesh-wide rollback runbook is human-operated, which is a legitimate answer and
is how comparable systems document the same operation.

### Consequences

- `docs/BACKUP.md` is restructured around the three operations above, including
  the rollback runbook and its sequencing requirement. The section presenting
  in-place restore as a rollback is replaced.
- No operator code change follows from this amendment (decision 2): the
  behaviour is already correct, only its documented meaning changes.
- The accesslog-replay e2e spec asserts rollback semantics, which are no longer
  promised on a mesh member, so on the shared multi-site fixture it fails
  deterministically. Relocating it onto its own cluster with no external peers —
  where it correctly guards the accesslog wipe, and where it would also stop
  being the most destructive spec on the shared fixture — is **deferred to the
  backlog**, blocked on an e2e framework refactor: today the suite's primary
  fixture is provisioned by external shell scripting, and each spec needing a
  different topology hand-rolls its own cluster. See `docs/BACKLOG.md`.
- Verified status, to be kept honest as it changes: rollback via `SlapdRestore`
  is green on a standalone cluster (`replicas: 1`, replication disabled,
  2026-08-24, automated). The replicated *single-site* path (N≥2, no external
  peers) — the topology where the accesslog wipe actually matters — was verified
  manually and is not under automated coverage; it is a backlog item pending the
  e2e framework refactor.
- `docs/BACKLOG.md` carries hub-and-spoke cross-site orchestration as an
  explicitly undecided item.

### Related

- ADR-012 — seed is one-shot; case 2 is operation 1 in the table above.
- ADR-007 (amendment) / ADR-016 — the operator's existing *read-only* reach into
  remote clusters, and the baseline that hub-and-spoke would change.
- ADR-018 — co-located Job PVC leases; the lease bug was silently blocking
  operation 1.
