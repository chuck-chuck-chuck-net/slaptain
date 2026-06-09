# Backup / Restore Implementation Plan (ADR-014)

Engineering breakdown for the S3 backup/restore feature decided in
[`docs/adrs/adr-014-s3-backup-restore.md`](adrs/adr-014-s3-backup-restore.md).
Read the ADR first for the *why*; this file is the *how* and the *order*.

Phases are ordered so each one leaves the tree green and shippable. Backup
(Phases 1–4) lands before restore (Phase 5).

## Resuming this work

If you are picking this up cold:

1. Read ADR-014 (decisions, rejected alternatives, the restore state machine).
2. Read this file for the phase breakdown.
3. Phases are sequential by dependency: types (1) → S3 transfer (2) →
   on-demand backup (3) → scheduled backup (4) → restore (5) → e2e + docs (6).
   Phase 0 is one-time tooling/deps setup.
4. Each phase notes its outcome ("shippable" = compiles, CRDs install, no
   regression). Don't start a phase before its predecessor is green.

## Conventions this plan follows (verified against the codebase)

- Types in `operator/api/v1alpha1/<kind>_types.go`: kubebuilder markers,
  tristate `*bool` for round-trip-safe optional booleans, CEL `XValidation`
  for cross-field rules, `SchemeBuilder.Register` in `init()`, helper methods
  on the type. Status uses `[]metav1.Condition` (listType=map, listMapKey=type).
- Controllers in `operator/internal/controller/<kind>_controller.go`: a
  `<kind>FieldManager` const, `+kubebuilder:rbac` markers above `Reconcile`,
  registered in `cmd/main.go`, SSA via
  `r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(...))`.
- RBAC: markers generate `config/rbac/role.yaml`; `make operator-manifests`
  syncs CRDs to `charts/operator/crds/`. Always run
  `make operator-generate operator-manifests` after type changes and commit the
  generated CRD + chart copy together.
- The operator binary already ships a `slctl` cobra tree under
  `operator/cmd/slctl/cmd/` — the home for the S3 transfer subcommands.
- The operator dials slapd with go-ldap using Secret-sourced credentials
  (no exec, no sidecar). Backups talk to LMDB via `slapcat`; restore via
  `slapadd`; neither needs a network LDAP path (the future online mode would).

## Phase 0 — Dependencies & scaffolding

- Add `github.com/aws/aws-sdk-go-v2` (`service/s3` + `config` + `credentials`;
  Apache-2.0, official, reaches MinIO/Ceph via `BaseEndpoint` + `UsePathStyle`)
  and `github.com/robfig/cron/v3` (schedule parsing; the lib k8s CronJob uses).
  Deliberately **not** minio-go — see the avoid-MinIO preference.
- Scaffold the kinds: `kubebuilder create api --group ldap --version v1alpha1
  --kind SlapdBackup` and `--kind SlapdScheduledBackup` (no
  `--skip-go-version-check` needed for `create api` per CLAUDE.md), then
  hand-edit.
- **Outcome:** compiles, empty reconcilers. Shippable.

## Phase 1 — API types (no behavior)

- `slapdbackup_types.go`: `SlapdBackup` (shortName `sb`),
  `SlapdBackupSpec{ DatabaseRef, Storage S3StorageSpec, Compression, PodAffinity *bool }`,
  `SlapdBackupStatus{ Phase, StartedAt, CompletedAt, Path, SizeBytes, JobName, Conditions }`.
  Printcolumns: Database, Phase, Completed, Age.
- `slapdscheduledbackup_types.go`: `SlapdScheduledBackup` (shortName `ssb`),
  spec `{ DatabaseRef, Schedule, Suspend, Immediate, Storage, Retention{MaxCount,MaxAge}, BackupTemplate }`,
  status `{ LastScheduleTime, NextScheduleTime, LastBackupRef }`.
- Shared `S3StorageSpec{ Bucket, Prefix, Endpoint, Region,
  AccessKeyIDSecretKeyRef, SecretAccessKeySecretKeyRef corev1.SecretKeySelector,
  InsecureTLS }`.
- Extend `SlapdDatabaseSpec` with `BootstrapFrom *BootstrapSource{ BackupRef *ObjectRef; S3 *S3StorageSpec+key }`
  and `SlapdDatabaseStatus.RestoreApplied bool` + a `Restored` condition.
  CEL `XValidation`: `bootstrapFrom` and `seed` mutually exclusive.
- `make operator-generate operator-manifests`.
- **Outcome:** CRDs install and validate; no runtime behavior. Shippable.

## Phase 2 — S3 transfer subcommand (the Job's Go workload)

- Add `slctl backup-upload --file … --s3-bucket/-endpoint/-region/-prefix …`
  (creds from env) → aws-sdk-go-v2 `PutObject` (multipart via the s3 manager);
  and `slctl restore-download --key … --out …` → `GetObject`.
- Thin and unit-testable against a MinIO testcontainer.
- **Outcome:** the binary can move artifacts to/from S3 independently of any
  controller.

## Phase 3 — `SlapdBackup` controller + Job builder (core backup path)

- `backup_job.go::buildBackupJob`: two-container `batch/v1` Job —
  - **initContainer** (slapd-init image):
    `slapcat -F /config -b <suffix> | gzip > /staging/dump.ldif.gz`
  - **container** (operator image): `slctl backup-upload …`, S3 creds via
    `env.valueFrom.secretKeyRef`
  - `emptyDir` staging volume; mounts `config-<c>-0` + `data-<c>-0`; **required
    PodAffinity** to pod-0 (`statefulset.kubernetes.io/pod-name` +
    `app.kubernetes.io/instance`, topologyKey `kubernetes.io/hostname`), gated by
    `spec.podAffinity` (default true). Pattern lifted from mariadb-operator
    `PhysicalBackup`.
- `slapdbackup_controller.go`: resolve
  `databaseRef → SlapdDatabase → clusterRef → SlapdCluster`; create the Job
  (`SetControllerReference`); observe Job → set `status.phase`/timestamps;
  derive `path` deterministically (`<prefix>/<cluster>/<db>/<ts>.ldif.gz`);
  immutable once `Completed`.
- New RBAC: `+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete`
  (**first Job usage in the operator**). Register in `main.go`; regen manifests.
- **Outcome:** `kubectl apply` a `SlapdBackup` → gzipped slapcat LDIF lands in
  S3. End-to-end backup works.

## Phase 4 — `SlapdScheduledBackup` controller

- `robfig/cron` next-time eval; on fire create an owned `SlapdBackup`; update
  `last/nextScheduleTime`; honor `suspend`/`immediate`; `RequeueAfter` next tick.
- Retention: list owned `SlapdBackup`s, delete beyond `maxCount` / older than
  `maxAge`, **and** delete their S3 objects.
- **Outcome:** scheduled backups + retention.

## Phase 5 — Restore via `bootstrapFrom` (cluster-coordinated offline `slapadd`)

Restore is a **SlapdCluster phase**, not a side actor — otherwise the cluster
controller reverts the scale-down and the two fight over `replicas`. See ADR-014
"Restore" for the full rationale (and why init-container slapadd / PVC-access-mode
serialization were rejected).

**5.1 — API + admission guards** (extends Phase 1 types)
- `SlapdDatabaseStatus.RestoreApplied bool` + a `Restored` condition (from
  Phase 1).
- Add a `Restoring` value to the `SlapdCluster` phase enum; persist
  `status.restore{ originalReplicas, phase, databases[] }`.
- CEL `XValidation`: `bootstrapFrom` and `seed` mutually exclusive (from Phase 1).

**5.2 — SlapdCluster controller honors the restore phase**
- Early reconcile step: detect databases in this cluster with `bootstrapFrom`
  set and `restoreApplied=false`, on a **freshly created database** (its peer
  copies are empty). The cluster may already serve other, populated databases
  and LDAP clients — the restore is a deliberate, documented, cluster-wide
  interruption, not gated on the cluster being idle.
- Replica reconcile becomes:
  `desiredReplicas := (phase==Restoring) ? 0 : spec.replicas`. This single change
  prevents the controller fight.
- State machine (each transition gated on observed state, re-entrant):
  - `Pending` → wait until the SlapdDatabase controller has defined the empty DB
    in `cn=config` (gives `slapadd` its target).
  - record `originalReplicas` **before** scaling → `ScalingDown` (STS→0, wait
    pods gone + PVC released).
  - `Restoring` → create the restore Job, watch to completion.
  - on success → `RestoreApplied=true`, `ScalingUp` (STS→`originalReplicas`),
    clear phase.
  - on Job failure → `RestoreFailed`, leave STS at 0, loud status/event (the
    interruption is already deliberate; halt visibly rather than scale up a
    half-restored DIT into the mesh).

**5.3 — Restore Job builder** (`restore_job.go`)
- slapd-init image; **node affinity to pod-0's PVC node**; mounts
  `config-<c>-0` + `data-<c>-0`.
- Steps: `slctl restore-download` (Phase 2) → **wipe target DB files**
  (idempotent retry) → `slapadd -F /config -b <suffix> -l dump.ldif` → exit.
- Loops all `bootstrapFrom` DBs in the cluster within the one scale-down window.
- No new RBAC beyond Phase 3's `jobs` verbs (the operator already patches
  StatefulSets).

**5.4 — (Future) online `ldapadd` restore mode** — deferred, not in MVP.
The zero-downtime path for small DBs / busy clusters: a `bootstrapFrom.mode:
offline|online` selector whose `online` branch reuses the seed controller's
live-`ldapadd` machinery to load the backup over the network without scaling the
cluster down. Slot in here when picked up.

- **Outcome:** apply a cluster whose new DB has `bootstrapFrom` → cluster-wide
  scale-to-0 → DIT restored on pod-0 → peers initial-sync to nominal. Disaster
  recovery and migration cutover both work.

## Phase 6 — e2e + docs

- e2e (gated `E2E_BACKUP=1`): deploy cluster+DB with data → MinIO in-cluster →
  `SlapdBackup` → assert object present → new cluster+DB with `bootstrapFrom` →
  assert DIT restored. Bonus: assert the artifact round-trips through stock
  `slapadd` (prod-parity check).
- `docs/BACKUP.md` (user-facing); CLAUDE.md updates (new CRDs, Job usage, batch
  RBAC); optional `slctl backup`/`restore` convenience wrappers.

## Open choices (deferrable to implementation time)

- **Pre-restore scale gate**: bring the cluster to `replicas=1` before scaling
  to 0 (so peers never come up until after restore — cleanest sync) vs. full-N
  then 0. Lean toward gating at 1.
- **Job → status reporting**: how the backup Job reports `sizeBytes`/`path`
  back to `SlapdBackup.status` — controller derives the key + a post-completion
  HEAD, vs. the Job writing a pod terminationMessage.

## Phase 7 — In-place restore: preflight + slapadd-all-pods + `SlapdRestore` (ADR-014 amendment)

Reverses the deferred in-place restore and reworks the data path. See the
ADR-014 **Amendment (2026-06-09)**. Order: machine rework first (also makes
`bootstrapFrom` peer-state-agnostic), then the CRD, then multi-replica e2e.

**7.1 — Preflight (destroy-last).** Before any scale-down, the SlapdCluster
controller validates each restore source **inline** (no Job, cluster stays up):
read the creds Secret → stream the S3 object → gunzip → confirm a non-empty LDIF
whose suffix entry (`dn: <suffix>`) is present, reading only the start. New
restore sub-phase `Preflight` (does NOT hold the StatefulSet down). Failure →
`PreflightFailed`, no scale-down, no data touched, no downtime.
- `internal/backup`: add `Preflight(ctx, cfg, key, suffix, replPassword)`.
- shared `s3ConfigFromStorage(ctx, client, ns, S3StorageSpec)` (reads creds
  Secret → static-cred `S3Config`); refactor the scheduled controller to use it.
- **Done (2026-06-09):** replication-password verification. The backup Job
  stamps the `{SSHA}` of the source's `cn=replication` userPassword onto the S3
  object's `replication-pw-hash` metadata; preflight SSHA-verifies the target
  cluster's `replication-password` against it, default-deny (mismatch or
  non-`{SSHA}` scheme fails). `spec.bootstrapFrom.skipReplicationPasswordCheck`
  bypasses it. Foreign/legacy dumps carry no metadata → not checked.

**7.2 — slapadd into every pod** (supersedes "slapadd pod-0 + peers
initial-sync"). During the scale-to-0 window, run a wipe+`slapadd` Job for
**each** RW and RO pod against that pod's own PVCs, so the cluster comes up
already-converged with no syncrepl refresh (→ no ITS#9580 exposure).
- `restore_job.go`: parameterize `buildRestoreJob` by pod identity — config/data
  PVC names, and the accesslog mount gated per pod (RW on a replicated cluster
  mounts `accesslog-<c>-<i>`; RO pods never, since their cn=config has no
  accesslog DB).
- `slapdcluster_restore.go`: fan `runRestoreJobs` across RW ordinals `0..N-1`
  and RO ordinals `0..M-1` (counts from `status.restore.originalReplicas` /
  `originalReadReplicas`). Wipe is per-pod, immediately before that pod's own
  load. Refinement (later): pod-0 first, confirm, then peers (peers as safety
  net); for now all-parallel with all-must-succeed-before-scale-up.
- Fallback: a pod whose direct load fails falls back to syncrepl refresh on
  scale-up (single-pod ITS#9580 exposure), or the machine aborts and retries.

**7.3 — `SlapdRestore` CRD** (imperative in-place restore on a live, populated
DB). `spec.databaseRef` + `spec.source.{backupRef|s3}`; immutable once terminal;
status `phase`. The SlapdCluster controller watches it (Architecture A) and
drives the same preflight → slapadd-all-pods machine against the named DB.
Guard: refuse a second concurrent restore.

**7.4 — e2e** (gated, multi-replica): rollback via `SlapdRestore`, and
delete+recreate `bootstrapFrom`, both on N≥2 — assert restored DIT on every pod,
no divergence, and (ideally) that no syncrepl refresh occurred.
- **Done (2026-06-09):** `bootstrapFrom` into a replicated cluster
  (`restore_replication_test.go`, gated `E2E_BACKUP=1`). Asserts (1) preflight
  default-deny on a mismatched `replication-password` without scaling down, and
  (2) once the password matches, the slapadd-all-pods machine lands the full DIT
  on **every** RW pod directly — verified per-pod and decoupled from syncrepl
  convergence (the target reuses the source TLS cert, whose SANs don't cover it,
  so cross-pod replication intentionally doesn't converge; preflight runs before
  scale-down and slapadd loads each pod independently, so neither assertion needs
  it). The `SlapdRestore` rollback variant waits on 7.3.

**Outcome:** `bootstrapFrom` is correct at any replica count (delete+recreate
rollback "is gonna be fine"), and there is an explicit, non-destructive,
auditable in-place restore path. Restore never traverses the syncrepl refresh
path.
